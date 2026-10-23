package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// receiptMargin is how long past an emergency's expire time its receipt is
// kept before it is dropped: Pushover stops the emergency itself at its
// expire time, so a cancel after that has nothing to stop.
const receiptMargin = 5 * time.Minute

// noReceipt is the receipt recorded for an emergency the channel accepted
// without a usable receipt: the phone rings, but there is nothing to cancel it
// with, and the record says so until the incident resolves or it expires.
const noReceipt = "-"

// receiptLife is how long each channel's emergency lasts, by channel name.
var receiptLife = map[string]time.Duration{"pushover": pushoverExpire}

// receipter is a channel whose page is an emergency that repeats on the
// phone until it is cancelled: Pushover. Sending a page returns a receipt,
// kept in the reporter's state until the incident resolves, when the
// emergency is cancelled. PagerDuty has its resolve; ntfy's messages have
// nothing to close.
type receipter interface {
	// takeReceipt returns the receipt of the last send, and forgets it;
	// empty when it opened no emergency.
	takeReceipt() string
	// cancel stops the emergency with the receipt.
	cancel(receipt string) error
	// expiry is how long an emergency lasts before the channel stops it.
	expiry() time.Duration
}

// receiptChannel is the pseudo-channel under which the state file records
// one receipt of channel, with the time it was sent (Unix seconds): the key
// is then the alert's, so cancelReceipts finds it by alert. A receipt is
// dropped only by its cancel, or by pruneReceipts once its emergency is over,
// never by forgetAlert. A receipt is a credential for its emergency's status
// and is never logged.
func receiptChannel(channel, receipt string, at time.Time) string {
	return "receipt:" + channel + ":" + receipt + ":" + strconv.FormatInt(at.Unix(), 10)
}

// keepReceipt records, in seen, the receipt of the emergency the send of
// alert key just opened on s, if it opened one.
func keepReceipt(s sender, key string, seen map[string]bool, at time.Time) {
	r, ok := s.(receipter)
	if !ok {
		return
	}
	if receipt := r.takeReceipt(); receipt != "" {
		seen[channelKey(receiptChannel(s.name(), receipt, at), key)] = true
	}
}

// cancelReceipts cancels every emergency recorded for the alert keyed key on
// every channel that can cancel one, and forgets each that is cancelled. It
// returns the pseudo-channel names of the receipts whose cancel failed, which
// the caller keeps (as it keeps a failed resolve) so the next firing tries
// again, and the joined error. A receipt older than its emergency's life
// and the margin is dropped without a cancel (receiptOver): the channel has
// stopped it. A receipt recorded as noReceipt has nothing to cancel and is
// dropped. The error names the channel, never a receipt.
func cancelReceipts(senders []sender, key string, seen map[string]bool, now time.Time) ([]string, error) {
	var kept []string
	var failures []error
	for _, s := range senders {
		r, ok := s.(receipter)
		if !ok {
			continue
		}
		prefix := "receipt:" + s.name() + ":"
		for k := range seen {
			ch, alert, found := strings.Cut(k, "|")
			rest, mine := strings.CutPrefix(ch, prefix)
			if !found || !mine || alert != key {
				continue
			}
			receipt, stamp, _ := strings.Cut(rest, ":")
			if receipt == noReceipt || receiptOver(stamp, r.expiry(), now) {
				delete(seen, k)
				continue
			}
			if err := r.cancel(receipt); err != nil {
				kept = append(kept, ch)
				failures = append(failures, fmt.Errorf("%s: cancel the page of %s: %w", s.name(), key, err))
				continue
			}
			delete(seen, k)
		}
	}
	return kept, errors.Join(failures...)
}

// receiptOver reports whether a receipt sent at stamp (Unix seconds) is past
// its emergency's life and the margin at now. The reporter has no clock but the
// host's wall clock across runs, so this is wall-clock time. Pushover stops
// the emergency at its own expire time, so dropping a receipt after
// expire+margin only gives up a cancel that would do nothing. A receipt stamped
// after now (the clock went back since it was sent) is never over, and is
// kept. The residue: if the clock was behind when it was sent and has since
// been put right, the receipt can read older than it is and be dropped up to
// that many minutes early; the emergency then runs to its expire or to the
// owner's acknowledgement. Nothing then guarantees a clock page: the clock
// check pages only a drift that persists over its consecutive checks, and a
// clock put right between two checks passes the next one unremarked.
func receiptOver(stamp string, life time.Duration, now time.Time) bool {
	unix, err := strconv.ParseInt(stamp, 10, 64)
	return err == nil && now.After(time.Unix(unix, 0).Add(life+receiptMargin))
}

// pruneReceipts drops from seen every receipt past its emergency's life and
// the margin (receiptOver), whatever the alert it was sent for or the state of
// its channel: a receipt is self-expiring. It runs at the start of every run,
// so the receipts of alerts that are never resolved (a repeating journal
// alert) do not accumulate in the state file. When it drops the last record of
// an alert, so that nothing but its bookkeeping is left (the incident
// recovered while its channel was set aside, or the channel is no longer
// configured), it does what a successful cancel would: the alert's remaining
// bookkeeping (its recovery marker, runs and reminders) goes too, so nothing
// is left behind. It reports whether it changed anything, for the caller to
// save.
func pruneReceipts(seen map[string]bool, sent map[string]time.Time, repeats map[string]int, now time.Time) bool {
	alerts := map[string]bool{}
	for k := range seen {
		ch, alert, found := strings.Cut(k, "|")
		rest, isReceipt := strings.CutPrefix(ch, "receipt:")
		if !found || !isReceipt {
			continue
		}
		channel, rest, _ := strings.Cut(rest, ":")
		_, stamp, _ := strings.Cut(rest, ":")
		if life, ok := receiptLife[channel]; ok && receiptOver(stamp, life, now) {
			delete(seen, k)
			alerts[alert] = true
		}
	}
	for alert := range alerts {
		if !deliveredAnywhere(seen, alert) {
			forgetAlert(alert, nil, seen, sent, repeats)
		}
	}
	return len(alerts) > 0
}

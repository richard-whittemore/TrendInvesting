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

// queueReceiptCancellations snapshots the emergencies a recovery or quiet route
// must cancel (ADR 0019, "Alerting"; alerts-and-dashboard.md, "Paging invariant"). Each marker owns
// one receipt and alert key; a later incident's receipts cannot join its retry.
// An unavailable channel retains the same snapshot until it can cancel.
func queueReceiptCancellations(seen map[string]bool, key, channel string) {
	for k := range seen {
		ch, alert, _ := strings.Cut(k, "|")
		if alert == key && strings.HasPrefix(ch, "receipt:"+channel) {
			seen["cancel:"+k] = true
		}
	}
}

// cancelReceipts requests cancellation of the alert's current emergencies,
// including unavailable channels, then attempts available senders (ADR 0019,
// "Alerting"). Retry paths use retryReceiptCancellations without a new snapshot.
func cancelReceipts(senders []sender, key string, seen map[string]bool, now time.Time) ([]string, error) {
	queueReceiptCancellations(seen, key, "")
	return retryReceiptCancellations(senders, key, seen, now)
}

// retryReceiptCancellations attempts only previously requested receipts
// (ADR 0019, "Alerting"; alerts-and-dashboard.md, "Paging invariant"). Errors name the channel and
// alert, never a receipt. Expired or missing receipts need no remote cancel.
func retryReceiptCancellations(senders []sender, key string, seen map[string]bool, now time.Time) ([]string, error) {
	discardUnscopedCancellations(seen)
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
			pending := "cancel:" + k
			if !found || !mine || alert != key || !seen[pending] {
				continue
			}
			receipt, stamp, _ := strings.Cut(rest, ":")
			if receipt != noReceipt && !receiptOver(stamp, r.expiry(), now) {
				if err := r.cancel(receipt); err != nil {
					kept = append(kept, ch)
					failures = append(failures, fmt.Errorf("%s: cancel the page of %s: %w", s.name(), key, err))
					continue
				}
			}
			delete(seen, k)
			delete(seen, pending)
		}
	}
	return kept, errors.Join(failures...)
}

// discardUnscopedCancellations removes legacy requests whose receipt ownership
// is unknowable (ADR 0019, "Alerting"; alerts-and-dashboard.md, "Paging invariant"). Never guess that
// a newer incident's emergency belongs to an old request. Keep the receipts for
// a later observed recovery or normal expiry, which still bounds their lifetime.
func discardUnscopedCancellations(seen map[string]bool) bool {
	changed := false
	for k := range seen {
		ch, _, _ := strings.Cut(k, "|")
		target, pending := strings.CutPrefix(ch, "cancel:")
		if pending && !strings.HasPrefix(target, "receipt:") {
			delete(seen, k)
			changed = true
		}
	}
	return changed
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
	changed := discardUnscopedCancellations(seen)
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
	for k := range seen {
		ch, alert, _ := strings.Cut(k, "|")
		if receipt, pending := strings.CutPrefix(ch, "cancel:"); pending && !seen[channelKey(receipt, alert)] {
			delete(seen, k)
		}
	}
	for alert := range alerts {
		if !deliveredAnywhere(seen, alert) {
			forgetAlert(alert, nil, seen, sent, repeats)
		}
	}
	return changed || len(alerts) > 0
}

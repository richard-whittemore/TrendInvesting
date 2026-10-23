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
// is then the alert's, so forgetting the alert (forgetAlert) forgets its
// receipts too. A receipt is a credential for its emergency's status and is
// never logged.
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
// and the margin is dropped without a cancel: the channel has stopped it. The error names the channel, never a receipt.
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
			if unix, err := strconv.ParseInt(stamp, 10, 64); err == nil && now.After(time.Unix(unix, 0).Add(r.expiry()+receiptMargin)) {
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

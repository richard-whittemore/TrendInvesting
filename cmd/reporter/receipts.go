package main

import (
	"errors"
	"fmt"
	"strings"
)

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
}

// receiptChannel is the pseudo-channel under which the state file records
// one receipt of channel: the key is then the alert's, so forgetting the
// alert (forgetAlert) forgets its receipts too. A receipt is a credential
// for its emergency's status and is never logged.
func receiptChannel(channel, receipt string) string {
	return "receipt:" + channel + ":" + receipt
}

// keepReceipt records, in seen, the receipt of the emergency the send of
// alert key just opened on s, if it opened one.
func keepReceipt(s sender, key string, seen map[string]bool) {
	r, ok := s.(receipter)
	if !ok {
		return
	}
	if receipt := r.takeReceipt(); receipt != "" {
		seen[channelKey(receiptChannel(s.name(), receipt), key)] = true
	}
}

// cancelReceipts cancels every emergency recorded for the alert keyed key on
// every channel that can cancel one, and forgets each that is cancelled. It
// returns the pseudo-channel names of the receipts whose cancel failed, which
// the caller keeps (as it keeps a failed resolve) so the next firing tries
// again, and the joined error. The error names the channel, never a receipt.
func cancelReceipts(senders []sender, key string, seen map[string]bool) ([]string, error) {
	var kept []string
	var failures []error
	for _, s := range senders {
		r, ok := s.(receipter)
		if !ok {
			continue
		}
		prefix := receiptChannel(s.name(), "")
		for k := range seen {
			ch, alert, found := strings.Cut(k, "|")
			receipt, mine := strings.CutPrefix(ch, prefix)
			if !found || !mine || alert != key {
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

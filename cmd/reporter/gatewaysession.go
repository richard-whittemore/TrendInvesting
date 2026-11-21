package main

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// gatewaySessionNotice retains a one-shot page until delivery and resolution
// finish (docs/manual/server.md, "When another login takes the Gateway's session").
type gatewaySessionNotice struct {
	At        time.Time `json:"at"`
	Resolving bool      `json:"resolving,omitempty"`
}

func (g gatewayTarget) sessionNotices(h *hostGateway, events []time.Time, scanned bool, host string, senders []sender, setAside []channelFault, seen map[string]bool, sent map[string]time.Time, repeats map[string]int, now time.Time, record func() error) error {
	if scanned {
		if h.SessionNotifiedThrough == nil {
			cutoff := now.Add(-15 * time.Minute)
			// Include the boundary itself; only older lines are suppressed (server.md,
			// "When another login takes the Gateway's session").
			cutoff = cutoff.Add(-time.Nanosecond)
			h.SessionNotifiedThrough = &cutoff
		}
		sort.Slice(events, func(i, j int) bool { return events[i].Before(events[j]) })
		for _, at := range events {
			if !at.After(*h.SessionNotifiedThrough) {
				continue
			}
			h.SessionNotices = append(h.SessionNotices, gatewaySessionNotice{At: at})
			cursor := at
			h.SessionNotifiedThrough = &cursor
		}
	}
	if err := record(); err != nil {
		return err
	}
	var failures []error
	for i := 0; i < len(h.SessionNotices); {
		done, err := g.sendSessionNotice(&h.SessionNotices[i], host, senders, setAside, seen, sent, repeats, now, record)
		failures = append(failures, err)
		if !done {
			i++
			continue
		}
		h.SessionNotices = append(h.SessionNotices[:i], h.SessionNotices[i+1:]...)
		if err := record(); err != nil {
			return errors.Join(append(failures, err)...)
		}
	}
	return errors.Join(failures...)
}

func (g gatewayTarget) sendSessionNotice(n *gatewaySessionNotice, host string, senders []sender, setAside []channelFault, seen map[string]bool, sent map[string]time.Time, repeats map[string]int, now time.Time, record func() error) (bool, error) {
	key := "gateway-session:" + g.scope(host) + ":" + n.At.UTC().Format(time.RFC3339Nano)
	deliveryPending := !n.Resolving && len(setAside) > 0
	if !n.Resolving {
		a := report.Alert{
			Key: key, Kind: "gateway-session", Severity: report.Urgent, AccountID: host, At: n.At,
			Title: fmt.Sprintf("%s: %s session used by another login", host, g.name()),
			Body:  fmt.Sprintf("Another login, for example the IBKR app or website, used this Gateway's username (%s, event at %s). IBC 3.24.2 documents primary as keeping or recovering the existing session against another TWS or Gateway session. The same is expected for the IBKR app or website, but is not yet confirmed by a real event. Stop using this Gateway's username anywhere else. If the Gateway's logged-out page follows, the setting did not hold, and that page's remedy applies. See docs/manual/server.md, \"When another login takes the Gateway's session\".", g.unit(), n.At.UTC().Format(time.RFC3339Nano)),
		}
		if err := deliver(senders, a, seen, sent, repeats, now, 0, record); err != nil {
			return false, err
		}
		n.Resolving = true
		if !deliveryPending {
			if err := record(); err != nil {
				return false, err
			}
		}
	}
	failed, err := resolveAll(senders, key, seen, now)
	keep := map[string]bool{}
	for _, name := range failed {
		keep[name] = true
	}
	for _, f := range setAside {
		keep[f.channel] = true
	}
	if deliveryPending {
		// Retain successful delivery receipts until set-aside channels return,
		// while resolving their peers now (server.md, "When another login takes the Gateway's session").
		for _, s := range senders {
			keep[s.name()] = true
		}
		n.Resolving = false
	}
	forgetAlert(key, keep, seen, sent, repeats)
	return !deliveryPending && !deliveredAnywhere(seen, key), errors.Join(err, record())
}

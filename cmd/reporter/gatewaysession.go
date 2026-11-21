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

func (g gatewayTarget) sessionNotices(h *hostGateway, events []time.Time, host string, senders []sender, setAside []channelFault, seen map[string]bool, sent map[string]time.Time, repeats map[string]int, now time.Time, record func() error) error {
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
	if !n.Resolving {
		a := report.Alert{
			Key: key, Kind: "gateway-session", Severity: report.Urgent, AccountID: host, At: n.At,
			Title: fmt.Sprintf("%s: %s session used by another login", host, g.name()),
			Body:  fmt.Sprintf("Another login, for example the IBKR app or website, used this Gateway's username (%s, event at %s). The Gateway is set to take the session back by itself, so no restart is normally needed. Stop using this Gateway's username anywhere else. If the Gateway's logged-out page follows, follow that page instead. See docs/manual/server.md, \"When another login takes the Gateway's session\".", g.unit(), n.At.UTC().Format(time.RFC3339Nano)),
		}
		if err := deliver(senders, a, seen, sent, repeats, now, 0, record); err != nil {
			return false, err
		}
		if len(setAside) > 0 {
			return false, fmt.Errorf("reporter: Gateway session notice has channels set aside")
		}
		n.Resolving = true
		if err := record(); err != nil {
			return false, err
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
	forgetAlert(key, keep, seen, sent, repeats)
	return !deliveredAnywhere(seen, key), errors.Join(err, record())
}

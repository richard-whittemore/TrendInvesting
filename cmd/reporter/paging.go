package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// pagingPolicy implements alerts-and-dashboard.md's Paging invariant. State
// lives in the caller's locked durable record, shared by hooks and timer runs.
type pagingPolicy struct {
	bypass   bool
	after    time.Duration
	schedule report.IBKRSchedule
}

// loadPagingPolicy reads non-secret owner settings for both entry points.
// Invalid configuration fails the firing but leaves delivery immediate, so
// a configuration fault cannot hide capital-safety alerts (ADR 0019).
func loadPagingPolicy(getenv func(string) string) (*pagingPolicy, error) {
	fallback := &pagingPolicy{bypass: true}
	path := getenv("TI_REPORTER_CONFIG")
	if path == "" {
		path = "/etc/trend-investing/reporter.env"
	}
	if _, err := os.Stat(path); err == nil {
		var e error
		getenv, e = withSecretsFile(getenv, path)
		if e != nil {
			return fallback, fmt.Errorf("reporter: %s: %w", path, e)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fallback, fmt.Errorf("reporter: %s: %w", path, err)
	}
	after := getenv("TI_ESCALATE_AFTER")
	if after == "" {
		after = "30m"
	}
	delay, err := time.ParseDuration(after)
	if err != nil || delay < 0 {
		return fallback, errors.New("reporter: TI_ESCALATE_AFTER must be a non-negative duration (default 30m, 0 is immediate)")
	}
	path = getenv("TI_IBKR_MAINTENANCE_FILE")
	if path == "" {
		path = "/etc/trend-investing/ibkr-maintenance.json"
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &pagingPolicy{after: delay}, nil
	}
	if err != nil {
		return fallback, fmt.Errorf("reporter: IBKR schedule: %w", err)
	}
	schedule, err := report.ParseIBKRSchedule(raw)
	if err != nil {
		return fallback, fmt.Errorf("reporter: IBKR schedule: %w", err)
	}
	return &pagingPolicy{after: delay, schedule: schedule}, nil
}

func policyOf(policies []*pagingPolicy) *pagingPolicy {
	if len(policies) > 0 {
		return policies[0]
	}
	return nil
}

// route returns the eligible channels and reminder period. The delay begins
// at observation, or at the end of the last quiet window, never at delivery.
// Only caller-observed open incidents reach here; recovery forgets their clock.
func (p *pagingPolicy) route(senders []sender, a report.Alert, sent map[string]time.Time, now time.Time, every time.Duration) ([]sender, report.Alert, time.Duration, error) {
	if p == nil || p.bypass || a.Kind == kindSummary {
		return senders, a, every, nil
	}
	near, err := report.SessionNear(now)
	if err != nil {
		near = true
	}
	if a.Kind == report.KindUnitFailed && sent[channelKey("observed", a.Key)].IsZero() {
		sent[channelKey("observed", a.Key)] = now
	}
	if a.Class(near) == report.CapitalSafety {
		return senders, a, every, err
	}
	if p.after > every {
		every = p.after
	}
	observed := channelKey("observed", a.Key)
	if sent[observed].IsZero() {
		sent[observed] = now
	}
	key := channelKey("escalation", a.Key)
	if sent[key].IsZero() {
		sent[key] = sent[observed]
	}
	if end := p.schedule.ClosedSince(sent[key], now); end.After(sent[key]) {
		sent[key] = end
	}
	until := p.schedule.WindowEnd(now)
	if a.Kind == report.KindGatewayResetWait {
		loc, e := loadNewYork()
		if e != nil {
			return senders, a, every, e
		}
		if end := resetNightEnd(now.In(loc)); end.After(until) {
			until = end
		}
	}
	// A reset wait superseded by another start state (or a removed schedule)
	// ends its hold now; an old future deadline cannot extend it.
	if until.IsZero() && sent[key].After(now) {
		sent[key] = now
	}
	if a.Kind == report.KindIBKRConnectivity && until.IsZero() {
		a.Body += " If the Gateway remains logged in and connectivity is still lost after maintenance, outside weekday 05:30–09:30 ET, restart it by name: sudo systemctl restart ti-gateway."
	}
	quiet := !near && !until.IsZero()
	if quiet {
		sent[key] = until
		a.Body = "scheduled IBKR interruption. " + a.Body
		if a.PublicBody != "" {
			a.PublicBody = "scheduled IBKR interruption. " + a.PublicBody
		}
	}
	escalated := near || !quiet && now.Sub(sent[key]) >= p.after
	var allowed []sender
	for _, s := range senders {
		if (s.name() == "pushover" || s.name() == "pagerduty") && !escalated {
			continue
		}
		allowed = append(allowed, s)
	}
	// The window's single notice is not reminded until it closes.
	if quiet {
		a.Repeat = false
	}
	return allowed, a, every, err
}

// routeAndCancel stops outstanding emergencies when routing enters a quiet
// window (alerts-and-dashboard.md, "Paging invariant"). Callers persist seen
// even when cancellation fails; pending cancels retry at every firing, including
// after the window closes, until successful or the receipt expires (ADR 0019).
func (p *pagingPolicy) routeAndCancel(senders []sender, a report.Alert, seen map[string]bool, sent map[string]time.Time, now time.Time, every time.Duration) ([]sender, report.Alert, time.Duration, error) {
	allowed, routed, interval, err := p.route(senders, a, sent, now, every)
	for _, s := range senders {
		eligible := false
		for _, next := range allowed {
			if next.name() == s.name() {
				eligible = true
			}
		}
		if !eligible || seen[channelKey("cancel:"+s.name(), a.Key)] {
			_, cancelErr := cancelReceipts([]sender{s}, a.Key, seen, now)
			err = errors.Join(err, cancelErr)
		}
	}
	return allowed, routed, interval, err
}

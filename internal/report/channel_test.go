package report_test

import (
	"errors"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// TestAChannelTheReporterCannotUseIsAnUrgentAlertOnTheOthers: a channel
// whose secret is empty or malformed is set aside and the others still
// deliver (ADR 0019, "Alerting": an alert is a safety requirement), so the
// alert names the channel and why, and goes out on the channels that work.
// The key is the account, the channel and the problem, so one problem is one
// incident and a different one is another.
func TestAChannelTheReporterCannotUseIsAnUrgentAlertOnTheOthers(t *testing.T) {
	at := time.Date(2024, time.January, 2, 21, 0, 0, 0, time.UTC)
	a := report.ChannelRefusedAlert("joint", "pagerduty", errors.New("TI_PAGERDUTY_ROUTING_KEY is empty"), at)
	if a.Kind != report.KindChannelRefused || a.Severity != report.Urgent || a.AccountID != "joint" || !a.At.Equal(at) || a.Repeat {
		t.Errorf("alert = %+v", a)
	}
	if !contains(a.Title, "pagerduty") || !contains(a.Body, "TI_PAGERDUTY_ROUTING_KEY is empty") || !contains(a.Body, "other channels") {
		t.Errorf("alert = %+v, want the channel, the reason and that the other channels still deliver", a)
	}
	if b := report.ChannelRefusedAlert("joint", "ntfy", errors.New("TI_PAGERDUTY_ROUTING_KEY is empty"), at); b.Key == a.Key {
		t.Error("two channels share a key")
	}
	if b := report.ChannelRefusedAlert("joint", "pagerduty", errors.New("other"), at); b.Key == a.Key {
		t.Error("two problems share a key")
	}
	if b := report.ChannelRefusedAlert("joint", "pagerduty", errors.New("TI_PAGERDUTY_ROUTING_KEY is empty"), at.Add(time.Hour)); b.Key != a.Key {
		t.Error("the same problem on a later run is a new key")
	}
}

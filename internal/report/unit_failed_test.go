package report_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// TestAFailedUnitPagesUrgentlyWithTheUnitTheReasonAndTheManualRow: a trading
// unit that stops pages once, naming the unit, the last line it wrote as the
// reason, and the manual row the owner reads (ADR 0019, "Alerting").
func TestAFailedUnitPagesUrgentlyWithTheUnitTheReasonAndTheManualRow(t *testing.T) {
	at := time.Date(2026, 10, 22, 15, 4, 0, 0, time.UTC)
	for unit, row := range map[string]string{
		"ti-adapter-boost.service":  `docs/manual/ibkr-adapter.md, "What can go wrong", the row "The adapter's unit failed"`,
		"ti-engine@boost.service":   `docs/manual/engine.md, "While it runs", the row "The engine's unit failed"`,
		"ti-reporter@boost.service": `docs/manual/alerts-and-dashboard.md, "The reporter itself", the row "A trading unit failed"`,
	} {
		a := report.UnitFailedAlert("lightsail", unit, "reconcile incomplete: open orders unread", at)
		if a.Severity != report.Urgent || a.Kind != report.KindUnitFailed || a.Repeat {
			t.Errorf("%s: severity %v kind %v repeat %v, want urgent, unit-failed, once", unit, a.Severity, a.Kind, a.Repeat)
		}
		if a.Key != report.UnitFailedKey(unit) || !strings.Contains(a.Key, unit) {
			t.Errorf("%s: key %q does not name the unit", unit, a.Key)
		}
		if a.Title != "lightsail: "+unit+" failed" {
			t.Errorf("%s: title %q", unit, a.Title)
		}
		for _, want := range []string{unit, "reconcile incomplete: open orders unread", row} {
			if !strings.Contains(a.Body, want) {
				t.Errorf("%s: body lacks %q:\n%s", unit, want, a.Body)
			}
		}
	}
}

// TestTheReasonCarriesNeitherASecretNorAnAccountNorAnAddress: the journal's
// last line is the owner's to read on the server, not on the channels, which
// are given no secret, account number or address.
func TestTheReasonCarriesNeitherASecretNorAnAccountNorAnAddress(t *testing.T) {
	line := "  account DU1234567 refused; posted to https://hc-ping.com/abcd-1234 with key s3cr3tK3y and topic  my-topic\n"
	got := report.UnitFailedReason(line, []string{"s3cr3tK3y", "my-topic", "ab"})
	for _, leak := range []string{"DU1234567", "hc-ping.com", "abcd-1234", "s3cr3tK3y", "my-topic"} {
		if strings.Contains(got, leak) {
			t.Errorf("reason %q leaks %q", got, leak)
		}
	}
	if !strings.Contains(got, "refused") || strings.Contains(got, "\n") {
		t.Errorf("reason %q lost its words or is not one line", got)
	}
	if long := report.UnitFailedReason(strings.Repeat("x ", 500), nil); len([]rune(long)) > 320 {
		t.Errorf("a long reason is not cut: %d runes", len([]rune(long)))
	}
	if report.UnitFailedReason("  \n", nil) == "" {
		t.Error("an empty journal gives an empty reason; it must say there was no line")
	}
}

// TestAFailedUnitJoinsTheStandingWhetherOrNotAnythingElseIsInForce: the page a
// channel is given when it begins names every failed trading unit with its
// manual row, and exists when that is all there is.
func TestAFailedUnitJoinsTheStandingWhetherOrNotAnythingElseIsInForce(t *testing.T) {
	failed := []string{"ti-adapter-boost.service", "ti-engine@boost.service"}

	untouched := report.Alert{Key: "state-at-start:joint", AccountID: "joint"}
	if a, in := report.WithFailedUnits(untouched, false, nil); in || a.Title != "" {
		t.Errorf("no failed unit changed the standing: %+v in force %v", a, in)
	}

	a, in := report.WithFailedUnits(untouched, false, failed)
	if !in || a.Severity != report.Urgent || a.Title != "joint: state at reporter start: Normal, a trading unit is failed" {
		t.Errorf("with nothing else in force: %+v in force %v", a, in)
	}
	for _, text := range []string{a.Body, a.PublicBody} {
		for _, want := range append(failed, "docs/manual/engine.md", "docs/manual/ibkr-adapter.md") {
			if !strings.Contains(text, want) {
				t.Errorf("the text lacks %q: %s", want, text)
			}
		}
	}

	halted := report.Alert{Key: "state-at-start:joint", AccountID: "joint", Severity: report.Urgent, Title: "joint: state at reporter start: Halted", Body: "private.", PublicBody: "public."}
	b, in := report.WithFailedUnits(halted, true, failed[:1])
	if !in || b.Title != halted.Title || !strings.HasPrefix(b.Body, "private. Failed trading unit") || !strings.HasPrefix(b.PublicBody, "public. Failed trading unit") || !strings.Contains(b.Body, failed[0]) {
		t.Errorf("with the engine Halted: %+v", b)
	}
}

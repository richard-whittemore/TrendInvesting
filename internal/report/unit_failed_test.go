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
		"ti-reporter@boost.service": `docs/manual/alerts-and-dashboard.md, "The reporter itself", the row "<host>: <unit> failed"`,
	} {
		const run = "0123456789abcdef0123456789abcdef"
		a := report.UnitFailedAlert("lightsail", unit, run, "reconcile incomplete: open orders unread", at)
		if a.Severity != report.Urgent || a.Kind != report.KindUnitFailed || a.Repeat {
			t.Errorf("%s: severity %v kind %v repeat %v, want urgent, unit-failed, once", unit, a.Severity, a.Kind, a.Repeat)
		}
		if a.Key != report.UnitFailedKey(unit, run) || !strings.HasPrefix(a.Key, report.UnitFailedKeyPrefix(unit)) || !strings.Contains(a.Key, run) {
			t.Errorf("%s: key %q does not name the unit and its failed run", unit, a.Key)
		}
		if other := report.UnitFailedKey(unit, "fedcba9876543210fedcba9876543210"); other == a.Key {
			t.Errorf("%s: two failed runs share the key %q", unit, a.Key)
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

// TestAPublicRelayIsSentTheFailedUnitPageWithoutTheJournalLine: a public
// ntfy relay is sent the page's public text, which names the unit and the
// manual row but not the journal line, which stays on the private channels.
func TestAPublicRelayIsSentTheFailedUnitPageWithoutTheJournalLine(t *testing.T) {
	a := report.UnitFailedAlert("lightsail", "ti-engine@boost.service", "0123456789abcdef0123456789abcdef", "reconcile incomplete: open orders unread", time.Date(2026, 10, 22, 15, 4, 0, 0, time.UTC))
	if a.PublicBody == "" || strings.Contains(a.PublicBody, "reconcile incomplete") {
		t.Errorf("public text %q: want one without the journal line", a.PublicBody)
	}
	for _, want := range []string{"ti-engine@boost.service", "docs/manual/engine.md", "journalctl -u ti-engine@boost.service"} {
		if !strings.Contains(a.PublicBody, want) {
			t.Errorf("public text lacks %q:\n%s", want, a.PublicBody)
		}
	}
}

// TestTheReminderNamesWhenTheFailureBeganAndHowManyRunsFailed: a failure
// episode that goes on is reminded under the episode's own key, saying since
// when and how many runs failed, with no journal line (ADR 0019, "Alerting").
func TestTheReminderNamesWhenTheFailureBeganAndHowManyRunsFailed(t *testing.T) {
	since := time.Date(2026, 10, 22, 15, 4, 0, 0, time.UTC)
	const run = "0123456789abcdef0123456789abcdef"
	a := report.UnitStillFailedAlert("lightsail", "ti-reporter@boost.service", run, since, 13, since.Add(time.Hour))
	if a.Key != report.UnitFailedKey("ti-reporter@boost.service", run) || a.Severity != report.Urgent || a.Kind != report.KindUnitFailed {
		t.Errorf("key %q severity %v kind %v: want the episode's key, urgent, unit-failed", a.Key, a.Severity, a.Kind)
	}
	if a.Title != "lightsail: ti-reporter@boost.service still failing" {
		t.Errorf("title %q", a.Title)
	}
	for _, want := range []string{"since 2026-10-22T15:04:00Z", "13 failed runs", "docs/manual/alerts-and-dashboard.md"} {
		if !strings.Contains(a.Body, want) || !strings.Contains(a.PublicBody, want) {
			t.Errorf("reminder lacks %q:\n%s", want, a.Body)
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

// TestAReminderWithoutAKnownCountSaysNoCount: a reminder for an episode no
// later run was named for (one run, or unknown) says the unit is still failing
// without a count, and a count of two or more is said as runs (ADR 0019,
// "Alerting").
func TestAReminderWithoutAKnownCountSaysNoCount(t *testing.T) {
	since := time.Date(2026, 10, 22, 15, 4, 0, 0, time.UTC)
	const run = "0123456789abcdef0123456789abcdef"
	const unit = "ti-engine@boost.service"
	for _, runs := range []int{0, 1} {
		a := report.UnitStillFailedAlert("lightsail", unit, run, since, runs, since.Add(time.Hour))
		for _, body := range []string{a.Body, a.PublicBody} {
			if !strings.Contains(body, "still failing since 2026-10-22T15:04:00Z") || strings.Contains(body, "failed run") {
				t.Errorf("runs %d: the reminder must say still failing since the start, with no count:\n%s", runs, body)
			}
		}
	}
	a := report.UnitStillFailedAlert("lightsail", unit, run, since, 2, since.Add(time.Hour))
	if !strings.Contains(a.Body, "2 failed runs") {
		t.Errorf("runs 2: the reminder lacks %q:\n%s", "2 failed runs", a.Body)
	}
}

// TestTheDataJobsPageNeverSaysSystemdDoesNotRestartIt: the data job is run
// again by its timer, so its page and a state-at-start line say that, while the
// engine's and the adapter's still say a start is the owner's (ADR 0019,
// "Alerting").
func TestTheDataJobsPageNeverSaysSystemdDoesNotRestartIt(t *testing.T) {
	at := time.Date(2026, 10, 22, 15, 4, 0, 0, time.UTC)
	data := report.UnitFailedAlert("lightsail", "ti-data.service", "0123456789abcdef0123456789abcdef", "refused", at)
	for _, text := range []string{data.Body, data.PublicBody} {
		if strings.Contains(text, "does not restart") || strings.Contains(text, "every start is yours") {
			t.Errorf("the data job's page says it is not restarted:\n%s", text)
		}
		if !strings.Contains(text, "timer") {
			t.Errorf("the data job's page does not say its timer runs it again:\n%s", text)
		}
	}
	engine := report.UnitFailedAlert("lightsail", "ti-engine@boost.service", "0123456789abcdef0123456789abcdef", "stopped", at)
	if !strings.Contains(engine.Body, "systemd does not restart the engine or the adapter") {
		t.Errorf("the engine's page lost its restart sentence:\n%s", engine.Body)
	}
	a, _ := report.WithFailedUnits(report.Alert{AccountID: "boost"}, false, []string{"ti-data.service", "ti-engine@boost.service"})
	if strings.Contains(a.Body, "which systemd does not restart: ti-data") || !strings.Contains(a.Body, "timer") {
		t.Errorf("the state-at-start line words the data job as not restarted:\n%s", a.Body)
	}
}

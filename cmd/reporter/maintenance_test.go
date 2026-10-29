package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// The maintenance-window seam: while a window is open the reporter still
// fires and still pings the outside heartbeat, records a failed unit, a
// logged-out Gateway and a reconciliation that moved no state without paging
// them, and pages whatever is still wrong at the first firing after the
// window closes; a capital-safety alert is never held (internal/maintenance;
// internal/report, maintenance.go; ADR 0019, "Alerting").

const markerName = "maintenance.json"

// openWindow writes a window into dir (beside the state files, where the
// reporter and the hook look for it) that opened a minute before from and
// ends at until.
func openWindow(t *testing.T, dir string, from, until time.Time) {
	t.Helper()
	opened := from.Add(-time.Minute).UTC()
	body := `{"opened":"` + opened.Format(time.RFC3339) + `","until":"` + until.UTC().Format(time.RFC3339) +
		`","opened_unix":` + strconv.FormatInt(opened.Unix(), 10) + `,"until_unix":` + strconv.FormatInt(until.Unix(), 10) + `}`
	if err := os.WriteFile(filepath.Join(dir, markerName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func windowFile(dir string) string { return filepath.Join(dir, markerName) }

func recordText(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTheHookRecordsAFailureInsteadOfPagingWhileAWindowIsOpen(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	openWindow(t, dir, at, at.Add(10*time.Minute))
	const unit = "ti-engine@boost.service"
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), at, unit); err != nil {
		t.Fatalf("the hook failed in a window: %v", err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if got := r.to(host); len(got) != 0 {
			t.Errorf("%s was paged inside a window: %v", host, got)
		}
	}
	rec := recordText(t, dir, "unit-failures.json")
	if !strings.Contains(rec, "held:") || !strings.Contains(rec, "unit-failed:"+unit+"/"+firstRun) {
		t.Errorf("the failure is not recorded with the window noted:\n%s", rec)
	}
	if strings.Contains(rec, "ntfy|unit-failed") || strings.Contains(rec, "pagerduty|unit-failed") {
		t.Errorf("the record says a channel took a page that was never sent:\n%s", rec)
	}
}

func TestTheHookPagesWhenNoWindowIsOpenOrTheWindowHasExpired(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dir string, at time.Time){
		"no marker": func(*testing.T, string, time.Time) {},
		"expired": func(t *testing.T, dir string, at time.Time) {
			openWindow(t, dir, at.Add(-20*time.Minute), at.Add(-time.Second))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := &relay{sent: map[string][]string{}}
			systemd := newStandInSystemd(t)
			dir := t.TempDir()
			at := t0.Add(time.Hour)
			setup(t, dir, at)
			const unit = "ti-engine@boost.service"
			if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), at, unit); err != nil {
				t.Fatal(err)
			}
			for _, host := range []string{ntfyHost, pdHost} {
				if n := pagesFor(r, host, unit); n != 1 {
					t.Errorf("%s took %d pages, want 1", host, n)
				}
			}
		})
	}
}

func TestAFailedUnitHeldInAWindowPagesOnceAtTheFiringAfterItCloses(t *testing.T) {
	const unit = "ti-adapter-boost.service"
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.fail(unit)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(12*time.Minute))
	// The hook and two firings inside the window.
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), start, unit); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := firing(t, r, dir, systemd, start.Add(time.Duration(i)*5*time.Minute), unit); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, unit); n != 0 {
			t.Fatalf("%s was paged %d times for the unit inside the window: %v", host, n, r.to(host))
		}
	}
	if !strings.Contains(recordText(t, dir, "unit-failures.json"), "held:") {
		t.Error("the failure inside the window is not recorded")
	}
	// The window ends at +12m; the firing at +15m finds the unit still failed.
	for i := 3; i <= 5; i++ {
		if err := firing(t, r, dir, systemd, start.Add(time.Duration(i)*5*time.Minute), unit); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, unit); n != 1 {
			t.Errorf("%s took %d pages for the unit after the window closed, want exactly 1", host, n)
		}
	}
	if strings.Contains(recordText(t, dir, "unit-failures.json"), "held:") {
		t.Error("a held record outlived the window that held it")
	}
}

func TestAUnitThatRecoveredInsideTheWindowResolvesQuietly(t *testing.T) {
	const unit = "ti-adapter-boost.service"
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(12*time.Minute))
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), start, unit); err != nil {
		t.Fatal(err)
	}
	// systemd shows it active again by the next firing.
	for i := 1; i <= 5; i++ {
		if err := firing(t, r, dir, systemd, start.Add(time.Duration(i)*5*time.Minute), unit); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, unit); n != 0 {
			t.Errorf("%s was paged %d times for a unit that recovered inside the window", host, n)
		}
	}
	if strings.Contains(recordText(t, dir, "unit-failures.json"), "held:") {
		t.Error("the recovered unit's held record was not cleared")
	}
}

func TestAReminderDueInsideAWindowWaitsForItsClose(t *testing.T) {
	const unit = "ti-engine@boost.service"
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), start, unit); err != nil {
		t.Fatal(err)
	}
	systemd.fail(unit)
	// The hour's reminder falls due at +60m, inside a window from +58m to +70m.
	openWindow(t, dir, start.Add(58*time.Minute), start.Add(70*time.Minute))
	for _, m := range []int{60, 65} {
		if err := firing(t, r, dir, systemd, start.Add(time.Duration(m)*time.Minute), unit); err != nil {
			t.Fatal(err)
		}
	}
	reminders := func(host string) int { return count(r.to(host), unit+" still failing") }
	if n := reminders(ntfyHost) + reminders(pdHost); n != 0 {
		t.Fatalf("%d reminders inside the window", n)
	}
	if err := firing(t, r, dir, systemd, start.Add(75*time.Minute), unit); err != nil {
		t.Fatal(err)
	}
	if reminders(ntfyHost) != 1 || reminders(pdHost) != 1 {
		t.Errorf("the reminder was not sent once the window closed: ntfy %d, pagerduty %d", reminders(ntfyHost), reminders(pdHost))
	}
}

func TestAWindowedFiringStillPingsTheHeartbeatThoughAHeldUnitIsFailed(t *testing.T) {
	const unit = "ti-adapter-boost.service"
	e, server := newStandInEndpoint(t)
	systemd := newStandInSystemd(t)
	systemd.fail(unit)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	fire := func() {
		t.Helper()
		environment := map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath}
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-units", unit, "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
		if err := run(args, env(environment), func() time.Time { return at }, &bytes.Buffer{}, server.Client()); err != nil {
			t.Fatal(err)
		}
	}
	fire()
	if e.count("ping") != 0 {
		t.Fatalf("pinged with a trading unit failed and no window: %v", e.events)
	}
	openWindow(t, dir, at, at.Add(10*time.Minute))
	fire()
	if e.count("ping") != 1 {
		t.Errorf("pings = %d inside a window, want 1 (events %v)", e.count("ping"), e.events)
	}
	// Once the window has closed the failed unit withholds the ping again.
	at = at.Add(20 * time.Minute)
	fire()
	if e.count("ping") != 1 {
		t.Errorf("pings = %d after the window closed with the unit still failed, want still 1 (events %v)", e.count("ping"), e.events)
	}
}

// A window is open for a firing only if it is open when the firing starts and
// still open when it delivers: one that closes mid-firing holds nothing from
// then on, and the failed unit pages and withholds the ping.
func TestAWindowThatClosesDuringAFiringHoldsNothingFromThenOn(t *testing.T) {
	const unit = "ti-adapter-boost.service"
	e, server := newStandInEndpoint(t)
	systemd := newStandInSystemd(t)
	systemd.fail(unit)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(10*time.Minute))
	calls := 0
	clock := func() time.Time {
		calls++
		if calls == 1 {
			return start // the firing starts inside the window
		}
		return start.Add(20 * time.Minute) // and it has closed by the time anything is delivered
	}
	environment := map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath}
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-units", unit, "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
	if err := run(args, env(environment), clock, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ping") != 0 {
		t.Errorf("pinged with a failed unit after the window closed mid-firing: %v", e.events)
	}
	if strings.Contains(recordText(t, dir, "unit-failures.json"), "held:") {
		t.Error("the unit was held by a window that had closed")
	}
}

// No reader removes a marker: an expired one already reads closed, and a
// start that replaces it a moment later must never be undone by a reader that
// read the old one.
func TestNoReaderRemovesAnExpiredMarker(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	openWindow(t, dir, at.Add(-time.Hour), at.Add(-30*time.Minute))
	if err := firing(t, r, dir, systemd, at, "ti-adapter-boost.service"); err != nil {
		t.Fatal(err)
	}
	if err := notifyAt(t, r, dir, systemd, monitored("ti-engine@boost.service", firstRun), at, "ti-engine@boost.service"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(windowFile(dir)); err != nil {
		t.Errorf("a reader removed the expired marker: %v", err)
	}
}

// A window records an outage and pages nothing; it does not move the outage's
// start, so the normal 10 minutes still count from it. An outage already older
// than that when the window closes pages at the first firing after.
func TestAnOldGatewayOutageHeldByAWindowPagesAtTheFiringAfterItCloses(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	openWindow(t, dir, tuesday(14, 0), tuesday(14, 25))
	for _, at := range []time.Time{tuesday(14, 0), tuesday(14, 10), tuesday(14, 15), tuesday(14, 20)} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", at); err != nil {
			t.Fatal(err)
		}
	}
	if n := gatewayPages(r); n != 0 {
		t.Fatalf("%d gateway pages inside the window", n)
	}
	if !strings.Contains(recordText(t, dir, "host-gateway.json"), "held_in_maintenance_until") {
		t.Errorf("the held outage is not recorded with the window noted:\n%s", recordText(t, dir, "host-gateway.json"))
	}
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 26)); err != nil {
		t.Fatal(err)
	}
	if n := gatewayPages(r); n != 2 {
		t.Errorf("%d gateway pages at the first firing after the window, want 1 per channel", n)
	}
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 31)); err != nil {
		t.Fatal(err)
	}
	if n := gatewayPages(r); n != 2 {
		t.Errorf("the page repeated at once: %d", n)
	}
}

func TestAShortGatewayOutageHeldByAWindowDoesNotPageWhenItCloses(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	// First seen at 14:20, six minutes before the window closes at 14:25 and
	// 14:26's firing: the Gateway may simply still be logging in.
	openWindow(t, dir, tuesday(14, 0), tuesday(14, 25))
	for _, at := range []time.Time{tuesday(14, 20), tuesday(14, 22)} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", at); err != nil {
			t.Fatal(err)
		}
	}
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 26)); err != nil {
		t.Fatal(err)
	}
	if n := gatewayPages(r); n != 0 {
		t.Errorf("%d gateway pages for a six-minute outage when the window closed", n)
	}
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 31)); err != nil {
		t.Fatal(err)
	}
	if n := gatewayPages(r); n != 2 {
		t.Errorf("%d gateway pages once the outage is 10 minutes old, want 1 per channel", n)
	}
}

func TestAGatewayThatLoggedInAgainInsideTheWindowPagesNothing(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	openWindow(t, dir, tuesday(14, 0), tuesday(14, 25))
	for _, at := range []time.Time{tuesday(14, 0), tuesday(14, 12)} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", at); err != nil {
			t.Fatal(err)
		}
	}
	g.login()
	for _, at := range []time.Time{tuesday(14, 15), tuesday(14, 30), tuesday(14, 45)} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", at); err != nil {
			t.Fatal(err)
		}
	}
	if n := gatewayPages(r); n != 0 {
		t.Errorf("%d gateway pages for an outage that ended inside the window", n)
	}
}

// pendingCashEntries is a run whose reconciliation found only the cash
// different on a day the Flex reader has not read: it alerts and moves no
// state.
func pendingCashEntries(t *testing.T) []journal.Entry {
	entries := haltedEntries(t)[:3]
	obs := event.ReconciliationPayload{
		AccountID: "joint", ReconciliationID: "close:r", Trigger: event.ReconciliationTriggerMorning, AsOf: t0,
		Unreadable: []string{}, Positions: []event.AccountPosition{}, TotalCash: 1500, Currency: "USD",
	}
	result := event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "close:r", InputID: "reconciliation", AnchorID: "snap",
		Classification: event.ReconciliationPendingCashRead, Action: event.ReconciliationActionAlert, CashCompared: true, Currency: "USD",
		Components: []event.ReconciliationComponent{{Component: event.ReconciliationComponentCash, Book: 100000, Expected: 100000, Observed: 150000, Residual: 50000, Explanations: []string{}}},
		Detail:     "pending-cash-read: total cash: the gate's detail",
	}
	r := envelope(t, 5, event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, "result", obs.AsOf, result)
	r.CausationID = "reconciliation"
	return append(entries,
		journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 4, event.ReconciliationEventType, event.ReconciliationSchemaVersion, "reconciliation", obs.AsOf, obs)},
		journal.Entry{Kind: journal.KindDecision, Envelope: r})
}

func journalFiring(t *testing.T, r *relay, dir string, entries []journal.Entry, at time.Time) {
	t.Helper()
	path := writeRun(t, dir, entries)
	if err := os.Chtimes(path, t0, t0); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty"}
	if err := run(args, twoChannels(), func() time.Time { return at }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
}

// No install causes a reconciliation, and a cash decrease is a money signal:
// a window holds no reconciliation of any kind.
func TestAPendingCashReconciliationIsNeverHeldByAWindow(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	openWindow(t, dir, at, at.Add(10*time.Minute))
	journalFiring(t, r, dir, pendingCashEntries(t), at)
	for _, host := range []string{ntfyHost, pdHost} {
		if n := count(r.to(host), "reconciliation pending-cash-read"); n != 1 {
			t.Errorf("%s took %d pending-cash pages inside the window, want 1", host, n)
		}
	}
}

// Whatever a window holds, an order refused and a Halted account page as they
// always do (internal/report, NeverHeld).
func TestCapitalSafetyAlertsInsideAWindowStillPage(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	openWindow(t, dir, at, at.Add(10*time.Minute))
	journalFiring(t, r, dir, haltedEntries(t), at)
	for _, host := range []string{ntfyHost, pdHost} {
		got := strings.Join(r.to(host), "\n")
		if !strings.Contains(got, "engine halted") || !strings.Contains(got, "order refused") {
			t.Errorf("%s: the refusal and the halt must page inside a window:\n%s", host, got)
		}
	}
}

func TestAnUnexplainedReconciliationThatDegradesTheAccountIsNeverHeld(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	openWindow(t, dir, at, at.Add(10*time.Minute))
	journalFiring(t, r, dir, mismatchEntries(t), at)
	for _, host := range []string{ntfyHost, pdHost} {
		if n := count(r.to(host), "reconciliation unexplained"); n != 1 {
			t.Errorf("%s took %d pages for a reconciliation that Degrades the account inside a window, want 1: %v", host, n, r.to(host))
		}
	}
}

func TestTheDailySummaryIsUnchangedByAWindow(t *testing.T) {
	counts := map[bool]int{}
	for _, windowed := range []bool{false, true} {
		r := &relay{sent: map[string][]string{}}
		dir := t.TempDir()
		at := t0.Add(time.Hour)
		if windowed {
			openWindow(t, dir, at, at.Add(10*time.Minute))
		}
		path := writeJournal(t, dir)
		args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-summary"}
		if err := run(args, twoChannels(), func() time.Time { return at }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
			t.Fatal(err)
		}
		counts[windowed] = count(r.to(ntfyHost), "daily summary")
	}
	if counts[false] == 0 || counts[true] != counts[false] {
		t.Errorf("daily summaries delivered: %d without a window, %d inside one; want the same, and not none", counts[false], counts[true])
	}
}

// A held failure that recovers inside the window is marked handled, so a
// stale redelivery of the same failed run after the window never pages it.
func TestAHeldFailureThatRecoveredIsNotPagedByAStaleRedelivery(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(12*time.Minute))
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, firstRun), start, dataJob); err != nil {
		t.Fatal(err)
	}
	// The data job runs well again inside the window.
	if err := firing(t, r, dir, systemd, start.Add(5*time.Minute), "ti-engine@boost.service", "-resolve-units", dataJob); err != nil {
		t.Fatal(err)
	}
	// After the window, systemd hands the hook that same failed run again.
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, firstRun), start.Add(30*time.Minute), dataJob); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, dataJob); n != 0 {
			t.Errorf("%s was paged %d times for a failed run that recovered inside the window", host, n)
		}
	}
}

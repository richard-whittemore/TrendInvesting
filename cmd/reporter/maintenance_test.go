package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The maintenance-window seam: while a window is open the reporter still
// fires and still pings the outside heartbeat, records a failed unit, a
// logged-out Gateway without paging them, and pages whatever is still wrong at the first firing after the
// window closes; a capital-safety alert is never held (internal/maintenance;
// internal/report, maintenance.go; ADR 0019, "Alerting").

const markerName = "maintenance.json"

// openWindow writes a window into dir that opened a minute before from and
// ends at until, in the one layout ti-maintenance writes, and points the
// reporter and the hook at it (on the server the marker is in a folder only
// root can write; the tests' folder stands in for it).
func openWindow(t *testing.T, dir string, from, until time.Time) {
	t.Helper()
	opened := from.Add(-time.Minute).UTC()
	until = until.UTC()
	body := `{"id":"0123456789abcdef","opened":"` + opened.Format("2006-01-02T15:04:05Z") + `","until":"` + until.Format("2006-01-02T15:04:05Z") +
		`","opened_unix":` + strconv.FormatInt(opened.Unix(), 10) + `,"until_unix":` + strconv.FormatInt(until.Unix(), 10) + "}\n"
	if err := os.WriteFile(filepath.Join(dir, markerName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pointAtMarker(t, dir)
}

// pointAtMarker makes dir's marker the one the reporter reads, for the test.
func pointAtMarker(t *testing.T, dir string) {
	t.Helper()
	was := maintenanceFileDefault
	maintenanceFileDefault = filepath.Join(dir, markerName)
	t.Cleanup(func() { maintenanceFileDefault = was })
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
// then on, so the failed unit pages and withholds the ping.
func TestAWindowThatClosesDuringAFiringHoldsNothingFromThenOn(t *testing.T) {
	const unit = "ti-adapter-boost.service"
	r := &relay{sent: map[string][]string{}}
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
	environment := map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": "https://" + ntfyHost, "TI_HEARTBEAT_URL": "https://hc.test/ping/x"}
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-units", unit, "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
	if err := run(args, env(environment), clock, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.to("hc.test")); n != 0 {
		t.Errorf("pinged with a failed unit after the window closed mid-firing: %d pings", n)
	}
	if n := pagesFor(r, ntfyHost, unit); n != 1 {
		t.Errorf("the failed unit was paged %d times after the window closed mid-firing, want 1", n)
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

// Each hold decision asks the window at that moment, not once for the step:
// the window ends while the first unit is judged, and the second is paged.
func TestTheWindowIsAskedAtEachUnitsDecision(t *testing.T) {
	const a, b = "ti-adapter-boost.service", "ti-engine@boost.service"
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "unit-failures.json")
	at := t0.Add(time.Hour)
	senders, setAside, err := buildSenders("ntfy,pagerduty", twoChannels(), &http.Client{Transport: r}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	window := func() time.Time {
		asked++
		if asked == 1 {
			return at.Add(10 * time.Minute) // open for the first unit
		}
		return time.Time{} // closed by the second
	}
	samples := map[string]unitSample{a: {failed: true, invocation: firstRun}, b: {failed: true, invocation: firstRun}}
	if _, err := unitStep(path, []string{a, b}, nil, samples, at, systemd.journalctl(), nil, senders, setAside, at, time.Hour, window, nil); err != nil {
		t.Fatal(err)
	}
	if asked < 2 {
		t.Fatalf("the window was asked %d times for two decisions", asked)
	}
	if n := pagesFor(r, ntfyHost, a); n != 0 {
		t.Errorf("%s was paged %d times: the window was open at its decision", a, n)
	}
	if n := pagesFor(r, ntfyHost, b); n != 1 {
		t.Errorf("%s was paged %d times: the window had closed at its decision, want 1", b, n)
	}
}

// A window that ends while the Gateway's journal is being read holds nothing:
// the stand-in journalctl removes the marker as it runs.
func TestAWindowThatEndsDuringTheGatewayCheckHoldsNothing(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	openWindow(t, dir, tuesday(14, 0), tuesday(14, 25))
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.dir, "rmwin"), []byte(windowFile(dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 15)); err != nil {
		t.Fatal(err)
	}
	if n := gatewayPages(r); n != 2 {
		t.Errorf("%d gateway pages when the window ended during the check, want 1 per channel", n)
	}
}

// heldDuring is the one decision: only the install's own noise is held, and
// no capital-safety kind ever is.
func TestOnlyAFailedUnitAndALoggedOutGatewayAreHeldAndNoNeverHeldKindIs(t *testing.T) {
	open := func() time.Time { return t0 }
	for _, k := range report.NeverHeld {
		if heldDuring(report.Alert{Kind: k}, open) {
			t.Errorf("%s is held by a window", k)
		}
	}
	for _, k := range []report.Kind{report.KindUnitFailed, report.KindGatewayLoggedOut} {
		if !heldDuring(report.Alert{Kind: k}, open) {
			t.Errorf("%s is not held by an open window", k)
		}
		if heldDuring(report.Alert{Kind: k}, func() time.Time { return time.Time{} }) {
			t.Errorf("%s is held by a window that is closed", k)
		}
	}
}

// What a window held is told once, when it closes: one routine message naming
// each held unit as recovered or still failed (and paged).
func TestTheCloseOfAWindowTellsWhatItHeldOnce(t *testing.T) {
	const recovered, failed = "ti-adapter-boost.service", "ti-engine@boost.service"
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.fail(failed)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(10*time.Minute))
	for _, u := range []string{recovered, failed} {
		if err := notifyAt(t, r, dir, systemd, monitored(u, firstRun), start, u); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := firing(t, r, dir, systemd, start.Add(time.Duration(11+5*i)*time.Minute), recovered+","+failed); err != nil {
			t.Fatal(err)
		}
	}
	// The routine summary goes to the channel the daily summary uses (the
	// owner's own ntfy), never to an urgent channel.
	if n := count(r.to(pdHost), "maintenance window closed"); n != 0 {
		t.Errorf("PagerDuty took %d close summaries, want 0", n)
	}
	got := r.to(ntfyHost)
	if n := count(got, "maintenance window closed"); n != 1 {
		t.Fatalf("ntfy took %d close summaries, want 1:\n%v", n, got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"held: " + recovered + " recovered", failed + " still failed (paged)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the close summary lacks %q:\n%s", want, joined)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, failed); n != 1 {
			t.Errorf("%s: %s paged %d times, want 1", host, failed, n)
		}
		if n := pagesFor(r, host, recovered); n != 0 {
			t.Errorf("%s: %s paged %d times, but it recovered", host, recovered, n)
		}
	}
}

// A held failure of a unit this firing does not sample belongs to the instance
// that does: it is left untouched, never dropped and never marked handled.
func TestAHeldFailureOfAnUnsampledUnitIsLeftUntouched(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(10*time.Minute))
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, firstRun), start, dataJob); err != nil {
		t.Fatal(err)
	}
	before := recordText(t, dir, "unit-failures.json")
	if err := firing(t, r, dir, systemd, start.Add(20*time.Minute), "ti-engine@boost.service"); err != nil {
		t.Fatal(err)
	}
	after := recordText(t, dir, "unit-failures.json")
	if !strings.Contains(after, "held:") || !strings.Contains(after, "unit-failed:"+dataJob+"/"+firstRun) {
		t.Errorf("the unsampled unit's held record was dropped:\n%s", after)
	}
	if strings.Contains(after, "handled|") {
		t.Errorf("the unsampled unit's run was marked handled:\n%s", after)
	}
	_ = before
	if n := count(r.to(ntfyHost), "maintenance window closed"); n != 0 {
		t.Errorf("a close summary was sent for a unit this firing does not sample")
	}
}

// A healthy sample older than a held failure says nothing about it: the
// record is kept, and the run is not marked handled.
func TestAHealthySampleOlderThanAHeldFailureDoesNotRecoverIt(t *testing.T) {
	const unit = "ti-adapter-boost.service"
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "unit-failures.json")
	at := t0.Add(time.Hour)
	openWindow(t, dir, at, at.Add(10*time.Minute))
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), at, unit); err != nil {
		t.Fatal(err)
	}
	senders, setAside, err := buildSenders("ntfy,pagerduty", twoChannels(), &http.Client{Transport: r}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	sampled := map[string]unitSample{unit: {state: "active"}}
	// Sampled a minute BEFORE the hook recorded the failure, judged after the window.
	if _, err := unitStep(path, []string{unit}, nil, sampled, at.Add(-time.Minute), systemd.journalctl(), nil, senders, setAside, at.Add(20*time.Minute), time.Hour, nil, nil); err != nil {
		t.Fatal(err)
	}
	rec := recordText(t, path[:len(path)-len("unit-failures.json")], "unit-failures.json")
	if !strings.Contains(rec, "held:") || strings.Contains(rec, "handled|") {
		t.Errorf("a stale healthy sample recovered a newer held failure:\n%s", rec)
	}
}

// A marker that is there and cannot be read as a window is said once, never
// paged, and never removed.
func TestAMarkerThatIsNotAWindowIsSaidOnceAndNeverPaged(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	if err := os.WriteFile(windowFile(dir), []byte("{\"id\": \"typo\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pointAtMarker(t, dir)
	var out bytes.Buffer
	for i := 0; i < 3; i++ {
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-units", "ti-adapter-boost.service", "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
		if err := run(args, twoChannels(), func() time.Time { return at.Add(time.Duration(i) * 5 * time.Minute) }, &out, &http.Client{Transport: r}); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(out.String(), "maintenance marker"); n != 1 {
		t.Errorf("the bad marker was mentioned %d times in three firings, want once:\n%s", n, out.String())
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := count(r.to(host), "maintenance marker"); n != 0 {
			t.Errorf("%s was paged about the marker", host)
		}
	}
	if _, err := os.Stat(windowFile(dir)); err != nil {
		t.Errorf("the marker was removed: %v", err)
	}
}

// mixedTransport sends the GitHub stand-in's requests to it and everything
// else to the recording relay.
type mixedTransport struct {
	ghHost string
	relay  *relay
}

func (m mixedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == m.ghHost {
		return http.DefaultTransport.RoundTrip(req)
	}
	return m.relay.RoundTrip(req)
}

// The close summary is a routine message: it opens no issue in the alerts
// repository (nothing would ever close it) and keeps no record behind it.
func TestTheCloseSummaryOpensNoIssueAndLeavesNoRecord(t *testing.T) {
	const recovered = "ti-adapter-boost.service"
	r := &relay{sent: map[string][]string{}}
	gh := &fakeGitHub{}
	ghURL := fakeGitHubServer(t, gh)
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(10*time.Minute))
	if err := notifyAt(t, r, dir, systemd, monitored(recovered, firstRun), start, recovered); err != nil {
		t.Fatal(err)
	}
	environment := twoChannels()
	getenv := func(k string) string {
		if v := issuesEnv(ghURL)[k]; v != "" {
			return v
		}
		return environment(k)
	}
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty,github-issues", "-units", recovered, "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
	client := &http.Client{Transport: mixedTransport{ghHost: strings.TrimPrefix(ghURL, "http://"), relay: r}}
	for i := 0; i < 2; i++ {
		if err := run(args, getenv, func() time.Time { return start.Add(time.Duration(15+5*i) * time.Minute) }, &bytes.Buffer{}, client); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(r.to(ntfyHost), "maintenance window closed"); n != 1 {
		t.Fatalf("ntfy took %d close summaries, want 1", n)
	}
	for _, issue := range gh.snapshot() {
		if strings.Contains(issue.title, "maintenance window closed") {
			t.Errorf("the close summary opened an issue: %q", issue.title)
		}
	}
	if strings.Contains(recordText(t, dir, "unit-failures.json"), "maintenance-closed") || strings.Contains(recordText(t, dir, "unit-failures.json"), "closing:") {
		t.Errorf("the delivered close summary left records behind:\n%s", recordText(t, dir, "unit-failures.json"))
	}
}

// The summary says "paged" only for a page that was delivered.
func TestTheCloseSummaryDoesNotSayPagedForAPageThatFailed(t *testing.T) {
	const failed = "ti-engine@boost.service"
	base := &relay{sent: map[string][]string{}}
	flaky := &flakyRelay{relay: base, failHost: pdHost}
	systemd := newStandInSystemd(t)
	systemd.fail(failed)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(10*time.Minute))
	if err := notifyAt(t, flaky, dir, systemd, monitored(failed, firstRun), start, failed); err != nil {
		t.Fatal(err)
	}
	// PagerDuty refuses everything at the firing after the close: the page fails.
	_ = firing(t, flaky, dir, systemd, start.Add(15*time.Minute), failed)
	joined := strings.Join(base.to(ntfyHost), "\n")
	if !strings.Contains(joined, "maintenance window closed") || strings.Contains(joined, "(paged)") || !strings.Contains(joined, "page failed, retrying") {
		t.Errorf("the summary of a page that failed must say so, not that it was paged:\n%s", joined)
	}
}

// A summary that could not be delivered is not lost: its records stay until a
// firing delivers it.
func TestACloseSummaryThatCouldNotBeSentIsSentAtALaterFiring(t *testing.T) {
	const recovered = "ti-adapter-boost.service"
	base := &relay{sent: map[string][]string{}}
	flaky := &flakyRelay{relay: base, failHost: ntfyHost, failBody: "maintenance window closed"}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(10*time.Minute))
	if err := notifyAt(t, flaky, dir, systemd, monitored(recovered, firstRun), start, recovered); err != nil {
		t.Fatal(err)
	}
	if err := firing(t, flaky, dir, systemd, start.Add(15*time.Minute), recovered); err == nil {
		t.Fatal("a firing whose close summary was refused must fail")
	}
	flaky.setFailing(false)
	if err := firing(t, flaky, dir, systemd, start.Add(20*time.Minute), recovered); err != nil {
		t.Fatal(err)
	}
	if err := firing(t, flaky, dir, systemd, start.Add(25*time.Minute), recovered); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(base.to(ntfyHost), "\n")
	if n := strings.Count(joined, "held: "+recovered+" recovered"); n != 1 {
		t.Errorf("the close summary reached ntfy %d times after the retry, want once:\n%s", n, joined)
	}
}

// A held failure that recovered is handled for the hook too, trading units
// included: a late redelivery of the same failed run does not page.
func TestAStaleHookRedeliveryOfARecoveredHeldTradingUnitDoesNotPage(t *testing.T) {
	const unit = "ti-adapter-boost.service"
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(12*time.Minute))
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), start, unit); err != nil {
		t.Fatal(err)
	}
	if err := firing(t, r, dir, systemd, start.Add(5*time.Minute), unit); err != nil {
		t.Fatal(err)
	}
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), start.Add(30*time.Minute), unit); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, unit); n != 0 {
			t.Errorf("%s was paged %d times by a stale redelivery of a recovered run", host, n)
		}
	}
}

// The reporter's own unit: a clean firing marks its held failures handled
// instead of forgetting them, so a delayed hook for the same run is silent.
func TestCleaningTheReportersOwnUnitMarksItsHeldRunsHandled(t *testing.T) {
	const unit = "ti-reporter@boost.service"
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "unit-failures.json")
	start := t0.Add(time.Hour)
	openWindow(t, dir, start, start.Add(10*time.Minute))
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), start, unit); err != nil {
		t.Fatal(err)
	}
	if err := clearUnit(path, unit, "", nil, nil, start.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rec := recordText(t, dir, "unit-failures.json")
	if strings.Contains(rec, "held:") || !strings.Contains(rec, "handled|"+unit+"/"+firstRun) {
		t.Errorf("clearUnit must mark the held run handled and forget the hold:\n%s", rec)
	}
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), start.Add(30*time.Minute), unit); err != nil {
		t.Fatal(err)
	}
	if n := pagesFor(r, ntfyHost, unit); n != 0 {
		t.Errorf("a delayed hook paged %d times for a run the reporter had seen well", n)
	}
}

// Every capital-safety kind, handed to the reporter as an alert of the run
// inside an open window, pages on each channel: the table is the NeverHeld
// list itself, so a kind added to it is tested at once.
func TestEveryNeverHeldKindPagesThroughARealFiringInsideAWindow(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	openWindow(t, dir, at, at.Add(10*time.Minute))
	was := journalReader
	t.Cleanup(func() { journalReader = was })
	journalReader = func(path string, now time.Time, ack time.Duration, seen map[string]bool) (report.History, []event.Envelope, string, []report.Alert, error) {
		history, envelopes, account, alerts, err := was(path, now, ack, seen)
		for _, k := range report.NeverHeld {
			alerts = append(alerts, report.Alert{Key: "never-held:" + string(k), Kind: k, Severity: report.Urgent, AccountID: account, At: at, Title: account + ": never-held " + string(k), Body: "a " + string(k) + " alert"})
		}
		return history, envelopes, account, alerts, err
	}
	journalFiring(t, r, dir, haltedEntries(t), at)
	for _, k := range report.NeverHeld {
		for _, host := range []string{ntfyHost, pdHost} {
			if n := count(r.to(host), "never-held "+string(k)); n != 1 {
				t.Errorf("%s took %d pages for a %s alert inside a window, want 1", host, n, k)
			}
		}
	}
}

// A window holds the Gateway's outage; the close tells it too.
func TestTheCloseSummaryNamesAHeldGatewayOutage(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	openWindow(t, dir, tuesday(14, 20), tuesday(14, 25))
	for _, at := range []time.Time{tuesday(14, 20), tuesday(14, 22)} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", at); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []time.Time{tuesday(14, 26), tuesday(14, 31)} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", at); err != nil {
			t.Fatal(err)
		}
	}
	joined := strings.Join(r.to("ntfy.private.test"), "\n")
	if n := strings.Count(joined, "maintenance window closed; held: IB Gateway still logged out"); n != 1 {
		t.Errorf("the close summary named the held Gateway outage %d times, want once:\n%s", n, joined)
	}
}

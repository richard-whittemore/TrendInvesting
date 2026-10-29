package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// An IRA's data job, ti-data@<instance>.service, is the data job for the
// hook and the reporter, as Boost's ti-data.service is: paged once by its
// OnFailure hook with the data job's manual row, never reminded, and only
// resolved by every reporter (ADR 0019, "Alerting").
const iraDataJob = "ti-data@trad-ira.service"

func TestTheHookPagesAFailedIRADataJobOnceWithTheDataJobsManualRow(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		if err := notify(t, r, dir, systemd, twoChannels(), iraDataJob); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, iraDataJob); n != 1 {
			t.Errorf("%s took %d pages for the IRA data job, want 1", host, n)
		}
		if got := strings.Join(r.to(host), "\n"); !strings.Contains(got, "The data job fails") {
			t.Errorf("%s: the page does not give the data job's manual row:\n%s", host, got)
		}
	}
}

func TestEveryDataJobIsOnlyResolvedAndNeverReminded(t *testing.T) {
	if err := checkResolveUnits([]string{"ti-data.service", iraDataJob}, []string{"ti-engine@boost.service"}); err != nil {
		t.Fatal(err)
	}
	if remindsUnit(iraDataJob) {
		t.Error("the IRA data job is reminded hourly; its timer runs it again")
	}
	for _, bad := range []string{"ti-data@.service", "ti-data@Trad.service", "ti-data@-x.service", "ti-data@trad-ira.timer"} {
		if tradingUnit.MatchString(bad) {
			t.Errorf("%q is accepted as a unit name", bad)
		}
	}
}

// An IRA's adapter runs as an instance of the adapter template,
// ti-adapter@<instance>.service (Boost's keeps its own unit,
// ti-adapter-boost.service): its OnFailure hook names it so, and the
// reporter samples it, so its name is a trading unit's (lessons L8).
func TestAnIRAAdaptersTemplateInstanceIsATradingUnit(t *testing.T) {
	for _, good := range []string{"ti-adapter@trad-ira.service", "ti-adapter@roth-ira.service", "ti-adapter-boost.service"} {
		if !tradingUnit.MatchString(good) {
			t.Errorf("%q is refused as a trading unit's name", good)
		}
	}
	for _, bad := range []string{"ti-adapter@.service", "ti-adapter@Trad.service", "ti-adapter@-x.service", "ti-adapter@trad-ira.timer"} {
		if tradingUnit.MatchString(bad) {
			t.Errorf("%q is accepted as a unit name", bad)
		}
	}
}

// An IRA's Flex reader, ti-flex@<instance>.service, is a timer-run job like
// the data job: paged once by its OnFailure hook with the Flex reader's manual
// row, never reminded, only resolved by the reporter (lessons L8).
const iraFlexReader = "ti-flex@trad-ira.service"

func TestTheHookPagesAFailedIRAFlexReaderOnceAndTheReporterOnlyResolvesIt(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		if err := notify(t, r, dir, systemd, twoChannels(), iraFlexReader); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, iraFlexReader); n != 1 {
			t.Errorf("%s took %d pages for the IRA Flex reader, want 1", host, n)
		}
		if got := strings.Join(r.to(host), "\n"); !strings.Contains(got, "docs/manual/reconciliation.md") {
			t.Errorf("%s: the page does not give the Flex reader's manual row:\n%s", host, got)
		}
	}
	if err := checkResolveUnits([]string{iraFlexReader}, []string{"ti-engine@trad-ira.service"}); err != nil {
		t.Fatal(err)
	}
	if remindsUnit(iraFlexReader) {
		t.Error("the Flex reader is reminded hourly; its timer runs it again")
	}
	for _, bad := range []string{"ti-flex@.service", "ti-flex@Trad.service", "ti-flex@-x.service", "ti-flex@trad-ira.timer"} {
		if tradingUnit.MatchString(bad) {
			t.Errorf("%q is accepted as a unit name", bad)
		}
	}
}

// TestAFailedIRAUnitIsSeenByTheReporterSample: the reporter's unit list names
// every account's engine, adapter, data job and Flex reader, so a failed IRA
// unit is paged by a firing of any instance's reporter when the hook missed
// it, and the failed run holds back the heartbeat (lessons L8).
func TestAFailedIRAUnitIsSeenByTheReporterSample(t *testing.T) {
	units := directive(t, reporterUnitPath, "[Service]", "ExecStart")
	if len(units) != 1 {
		t.Fatalf("ExecStart = %v", units)
	}
	words := strings.Fields(units[0])
	flagValue := func(name string) []string {
		for i, w := range words {
			if w == name && i+1 < len(words) {
				return strings.Split(words[i+1], ",")
			}
		}
		return nil
	}
	watched := append(flagValue("-units"), flagValue("-resolve-units")...)
	for _, account := range []string{"boost", "trad-ira", "roth-ira"} {
		for _, unit := range []string{"ti-engine@" + account + ".service", "ti-data@" + account + ".service", "ti-flex@" + account + ".service"} {
			if account == "boost" && strings.HasPrefix(unit, "ti-data@") {
				unit = "ti-data.service"
			}
			if !slices.Contains(watched, unit) {
				t.Errorf("the reporter's unit list does not cover %s", unit)
			}
		}
	}
	for _, unit := range []string{"ti-adapter-boost.service", "ti-adapter@trad-ira.service", "ti-adapter@roth-ira.service"} {
		if !slices.Contains(flagValue("-units"), unit) {
			t.Errorf("the reporter's -units does not cover %s", unit)
		}
	}

	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.fail("ti-adapter@trad-ira.service")
	dir := t.TempDir()
	if err := firing(t, r, dir, systemd, t0.Add(time.Hour), strings.Join(flagValue("-units"), ",")); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, "ti-adapter@trad-ira.service"); n != 1 {
			t.Errorf("%s took %d pages for the failed IRA adapter, want 1", host, n)
		}
	}
}

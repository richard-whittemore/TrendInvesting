package main

import (
	"strings"
	"testing"
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

package parity_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
)

func TestTheReportListsEveryCheckWithItsThresholdAndResult(t *testing.T) {
	r := parity.Report{
		Checks: []parity.Check{
			{Run: "FU", Name: parity.CheckWeights, Threshold: "abs <= 1e-12", Compared: 51, MaxDiff: 2.5e-13},
			{Run: "FU", Name: parity.CheckScores, Threshold: "rel <= 1e-09", Compared: 84, Failures: 2, MaxDiff: 3e-8,
				First: "2009-06 N100: Go 0.25, QuantConnect 0.2500000075"},
			{Run: "FU", Name: parity.CheckDifferences, Threshold: "none off the list", MayBeEmpty: true},
		},
		Differences: []parity.Difference{
			{Run: "FU", Where: "2009-06 N130", Kind: parity.KindGap, Size: "QuantConnect score 0.1"},
			{Run: "FU", Where: "2009-06 N100", Kind: parity.KindNoSector, Size: "1 mark"},
		},
	}
	want := strings.Join([]string{
		"| Run | Check | Threshold | Compared | Failures | Max difference | Result |",
		"| --- | --- | --- | --- | --- | --- | --- |",
		"| FU | weights | abs <= 1e-12 | 51 | 0 | 2.5e-13 | PASS |",
		"| FU | scores | rel <= 1e-09 | 84 | 2 | 3e-08 | FAIL |",
		"| FU | differences | none off the list | 0 | 0 | 0 | PASS |",
		"",
		"**Overall: FAIL**",
		"",
		"First failure per failing check:",
		"",
		"- FU scores: 2009-06 N100: Go 0.25, QuantConnect 0.2500000075",
		"",
		"| Run | Where | Live departure | On ADR 0026 point 8's list | Size |",
		"| --- | --- | --- | --- | --- |",
		"| FU | 2009-06 N130 | gap | yes | QuantConnect score 0.1 |",
		"| FU | 2009-06 N100 | no-sector | no | 1 mark |",
		"",
	}, "\n")
	if got := r.Markdown(); got != want {
		t.Errorf("report:\n%s\nwant:\n%s", got, want)
	}
}

func TestAPassingReportSaysSoAndHasNoDifferenceTable(t *testing.T) {
	r := parity.Report{Checks: []parity.Check{{Run: "B", Name: parity.CheckBoostRSI, Threshold: "exact", Compared: 3}}}
	want := strings.Join([]string{
		"| Run | Check | Threshold | Compared | Failures | Max difference | Result |",
		"| --- | --- | --- | --- | --- | --- | --- |",
		"| B | boost-rsi | exact | 3 | 0 | 0 | PASS |",
		"",
		"**Overall: PASS**",
		"",
		"No differences.",
		"",
	}, "\n")
	if got := r.Markdown(); got != want {
		t.Errorf("report:\n%s\nwant:\n%s", got, want)
	}
}

func TestACheckThatComparedNothingFails(t *testing.T) {
	if (parity.Check{Name: parity.CheckScores}).Pass() {
		t.Error("a check with nothing compared proves nothing and must fail")
	}
	if !(parity.Check{Name: parity.CheckDifferences, MayBeEmpty: true}).Pass() {
		t.Error("no differences at all is a pass")
	}
}

func TestMergeKeepsEveryRunsChecksAndDifferencesInOrder(t *testing.T) {
	a := compare(months())
	b := compareBoost(syntheticBoost(400, 210))
	m := parity.Merge(a, b)
	if len(m.Checks) != len(a.Checks)+len(b.Checks) || m.Checks[0].Run != "SYN" || m.Checks[len(m.Checks)-1].Run != "BSYN" {
		t.Errorf("merged checks %+v", m.Checks)
	}
	if !m.Pass() {
		t.Error("two passing reports merge to a pass")
	}
	bad := months()
	bad[1].Ranked[0].Sector = ""
	if parity.Merge(compare(bad), b).Pass() {
		t.Error("one failing report fails the merge")
	}
}

func TestThresholdsAreTheSpecifications(t *testing.T) {
	// The parity gate's pass/fail thresholds (ADR 0026 point 7, item 2;
	// docs/research/parity-2026-10.md, "Thresholds").
	if parity.WeightTolerance != 1e-12 || parity.RelativeTolerance != 1e-9 {
		t.Errorf("weights %g, scores and volatility %g", parity.WeightTolerance, parity.RelativeTolerance)
	}
}

func TestAFailingCheckThatComparedNothingSaysSo(t *testing.T) {
	r := parity.Report{Checks: []parity.Check{{Run: "B", Name: parity.CheckBoostRSI, Threshold: "exact"}}}
	if got := r.Markdown(); !strings.Contains(got, "- B boost-rsi: nothing was compared\n") {
		t.Errorf("report:\n%s", got)
	}
}

package report_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// iraInstance is the Traditional IRA deployment instance (docs/manual/server.md).
const iraInstance = "trad-ira"

// monthEnd is the decision session closing month m of 2025 (synthetic dates).
func monthEnd(m int) time.Time {
	return time.Date(2025, time.Month(m), 28, 21, 0, 0, 0, time.UTC)
}

// newIRAStream is a momentum account's stream: the configuration of the
// Traditional IRA, as the engine journals it first.
func newIRAStream(t *testing.T) *stream {
	s := &stream{t: t}
	s.add(event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", monthEnd(1).Add(-time.Hour),
		event.TargetWeightConfigurationPayload{
			StrategyID: event.TargetWeightStrategyMomentum, AccountID: iraInstance, Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR,
			Momentum: event.MomentumConfiguration{
				Rule: event.MomentumRuleTrend, ListingType: "CS", UniverseSize: 500, MinPrice: 5,
				ScoreLookback: 252, ScoreSkip: 21, VolatilityReturns: 252, TopN: 25, SectorCap: 3,
				InverseVolatility: true, Invested: 0.98, TrendInstrument: "SPY", TrendCloses: 200,
				RiskOffInstrument: "IEF", RiskOffFraction: 0.5, RebalanceBand: 0.25,
				UniverseSource: "massive", SectorSources: []string{"ishares-iwv-gics", "nasdaq-screener"},
				UniverseInput: event.UniverseInputCompact, MaxUnlabelledShare: 0.20,
			},
			Glide: event.GlideConfiguration{
				Anchors:            []event.GlideAnchor{{AgeYears: 38, Share: 0}, {AgeYears: 53, Share: 0.308}, {AgeYears: 65, Share: 0.5}, {AgeYears: 72, Share: 0.7}},
				RetirementAgeYears: 65, BondFunds: []event.BondFund{{InstrumentID: "IEF", Fraction: 0.5}, {InstrumentID: "SHY", Fraction: 0.5}}, Birth: "1980-01",
			},
			Execution: event.ExecutionConfiguration{CashRule: event.CashRuleSettled, ShareQuantity: event.ShareQuantityMinimumLot, MaxFeesPerOrder: 1, MaxSnapshotAge: 30},
			Limits:    event.MarginLimits{MaxExposure: 1, MaxOrdersPerDecision: 60, MaxOrderFraction: 0.5, PriceBand: 0.05},
		})
	return s
}

func iraDecision(m int) event.TargetsDecidedPayload {
	end := monthEnd(m)
	return event.TargetsDecidedPayload{
		AccountID: iraInstance, StrategyID: event.TargetWeightStrategyMomentum,
		DecisionTime: end, SessionPeriodEnd: end, Outcome: event.TargetsOutcomeDecided,
		Targets: []event.TargetWeight{
			{InstrumentID: "AAA", Weight: 0.49, Shares: []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: 0.49}}},
			{InstrumentID: "IEF", Weight: 0.49, Shares: []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: 0.49}}},
		},
		Glide: event.GlideFigures{RebalanceSession: end.AddDate(0, 0, 1).Format(time.DateOnly), AgeMonths: 30 * 12},
		Momentum: event.MomentumFigures{
			CloseSession: end.Format(time.DateOnly), TrendInstrument: "SPY", TrendCloses: 300, TrendClose: 99, TrendMean: 100, RiskOff: true,
			UniverseRows: 3, UniverseListingType: 2, UniverseAtOrAboveFloor: 2, UniverseMembers: 2,
			Names: []event.MomentumName{
				{InstrumentID: "AAA", UniverseRank: 1, DollarVolume: 5e8, Sector: "Tech", SectorSource: "ishares-iwv-gics",
					Status: event.MomentumStatusPicked, Closes: 300, Score: 0.3, Volatility: 0.02, ScoreRank: 1, Weight: 0.49, BacktestWouldPick: true},
				{InstrumentID: "BBB", UniverseRank: 2, DollarVolume: 4e8, Status: event.MomentumStatusShortHistory, Closes: 100},
			},
		},
	}
}

// decidedMonth journals a decided month-end rebalance.
func (s *stream) decidedMonth(m int) {
	s.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, fmt.Sprintf("targets:%d", m), monthEnd(m), iraDecision(m))
}

// withheldMonth journals the month-end decision withheld for reason.
func (s *stream) withheldMonth(m int, reason string) {
	p := iraDecision(m)
	p.Outcome, p.Targets, p.WithheldReason, p.Detail = event.TargetsOutcomeWithheld, nil, reason, "detail for "+reason
	s.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, fmt.Sprintf("targets:%d", m), monthEnd(m), p)
}

func pagesOf(alerts []report.Alert) []report.Alert {
	var out []report.Alert
	for _, a := range alerts {
		if a.Severity == report.Urgent {
			out = append(out, a)
		}
	}
	return out
}

// TestAnIRAsHandSalePageNamesItsOwnUnitsAndNoBoost: the steps of a sale by
// hand stop the adapter, then the engine, of the account that sells; an IRA
// instance's page names ti-adapter@trad-ira and ti-engine@trad-ira, never
// Boost's units (docs/live-trading/lessons-2026-10.md L8).
func TestAnIRAsHandSalePageNamesItsOwnUnitsAndNoBoost(t *testing.T) {
	s := newIRAStream(t)
	s.saleWithheld(0, "sale-1", event.SaleWithheldNotPositive)
	got := ofKind(alertsOf(t, s), report.KindSaleByHand)
	if len(got) != 1 {
		t.Fatalf("kinds = %v, want one sale-by-hand page", kinds(alertsOf(t, s)))
	}
	text := got[0].Title + "\n" + got[0].Body + "\n" + got[0].PublicBody
	for _, want := range []string{"sudo systemctl stop ti-adapter@" + iraInstance, "sudo systemctl stop ti-engine@" + iraInstance, "journalctl -u ti-adapter@" + iraInstance} {
		if !strings.Contains(text, want) {
			t.Errorf("the page does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(strings.ToLower(text), "boost") {
		t.Errorf("an IRA's page names Boost:\n%s", text)
	}
}

// TestABoostHandSalePageStillNamesBoostsUnits: the guard changes nothing for
// Boost, whose adapter is ti-adapter-boost.
func TestABoostHandSalePageStillNamesBoostsUnits(t *testing.T) {
	s := newStream(t)
	s.saleWithheld(0, "sale-1", event.SaleWithheldNotPositive)
	got := ofKind(alertsOf(t, s), report.KindSaleByHand)
	if len(got) != 1 || !strings.Contains(got[0].Body, "sudo systemctl stop ti-adapter-boost, then sudo systemctl stop ti-engine@boost") {
		t.Fatalf("Boost's page: %+v", got)
	}
}

// TestAWithheldIRARebalancePagesOnceNamingTheReason: a month-end decision the
// momentum engine withholds skips a whole month, so whatever the reason it is
// an urgent page that names the reason and repeats until acknowledged; one
// page per withheld month, however many records the month's withhold leaves;
// a decided rebalance pages nothing (lessons L11).
func TestAWithheldIRARebalancePagesOnceNamingTheReason(t *testing.T) {
	reasons := []string{
		event.WithheldMissingBars, event.WithheldStaleAccountSnapshot, event.WithheldMissingUniverse, event.WithheldInsufficientTrendHistory,
		event.WithheldNoEligibleNames, event.WithheldRebalanceInProgress, event.WithheldAllUnaffordable, event.WithheldHistoriesMissing, event.WithheldLabelsMissing,
	}
	for _, reason := range reasons {
		s := newIRAStream(t)
		s.withheldMonth(2, reason)
		s.withheldMonth(2, reason)
		pages := pagesOf(alertsOf(t, s))
		if len(pages) != 1 {
			t.Errorf("%s: %d pages, want one", reason, len(pages))
			continue
		}
		p := pages[0]
		if !strings.Contains(p.Title, reason) || !strings.Contains(p.Body, reason) || !p.Repeat || !strings.Contains(p.Body, p.Key) {
			t.Errorf("%s: the page does not name its reason or repeat until acknowledged: %+v", reason, p)
		}
		if strings.Contains(strings.ToLower(p.Title+p.Body), "boost") {
			t.Errorf("%s: an IRA's page names Boost: %s", reason, p.Body)
		}
	}

	s := newIRAStream(t)
	s.decidedMonth(2)
	if pages := pagesOf(alertsOf(t, s)); len(pages) != 0 {
		t.Errorf("a decided rebalance paged: %+v", pages)
	}

	// A later month withheld for the same reason is a second skipped month.
	s = newIRAStream(t)
	s.withheldMonth(2, event.WithheldNoEligibleNames)
	s.withheldMonth(3, event.WithheldNoEligibleNames)
	pages := pagesOf(alertsOf(t, s))
	if len(pages) != 2 || pages[0].Key == pages[1].Key {
		t.Errorf("two withheld months paged %d times, want two pages with their own keys", len(pages))
	}
}

func TestMissedJanuaryAndFebruaryWithholdsHaveDistinctDecisionKeys(t *testing.T) {
	s := newIRAStream(t)
	for i, session := range []string{"2026-01-30", "2026-02-27"} {
		p := iraDecision(1)
		found := time.Date(2026, 2, 2, 21, 0, 0, 0, time.UTC)
		if i == 1 {
			found = found.AddDate(0, 0, 25)
		}
		p.DecisionTime, p.SessionPeriodEnd, p.Momentum.CloseSession = found, found, session
		p.Glide.RebalanceSession = found.AddDate(0, 0, 1).Format(time.DateOnly)
		p.Outcome, p.Targets, p.WithheldReason, p.Detail = event.TargetsOutcomeWithheld, nil, event.WithheldMissingBars, "missing bars"
		s.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, session, found, p)
	}
	pages := pagesOf(alertsOf(t, s))
	if len(pages) != 2 {
		t.Fatalf("got %d pages, want two decision sessions", len(pages))
	}
	for i, session := range []string{"2026-01-30", "2026-02-27"} {
		if !strings.Contains(pages[i].Key, session) || !strings.Contains(pages[i].Body, session) {
			t.Errorf("decision session %s absent: %+v", session, pages[i])
		}
	}
}

func TestIRAHandSaleUnitsDoNotUseBrokerAccount(t *testing.T) {
	s := newIRAStream(t)
	s.out[0].Payload = []byte(strings.ReplaceAll(string(s.out[0].Payload), iraInstance, "DU0000001"))
	s.out[0].PayloadHash = event.HashPayload(s.out[0].Payload)
	if _, err := report.Alerts(s.out, ""); err == nil || !strings.Contains(err.Error(), "engine instance") {
		t.Fatalf("missing deployment instance was not refused: %v", err)
	}
	s.saleWithheld(0, "sale-1", event.SaleWithheldNotPositive)
	got := ofKind(alertsOf(t, s), report.KindSaleByHand)
	if len(got) != 1 {
		t.Fatalf("pages: %+v", got)
	}
	for _, unit := range []string{"ti-adapter@trad-ira", "ti-engine@trad-ira"} {
		if !strings.Contains(got[0].Body, unit) {
			t.Errorf("missing %s: %s", unit, got[0].Body)
		}
	}
	if strings.Contains(got[0].Body, "ti-adapter@DU") || strings.Contains(got[0].Body, "ti-engine@DU") {
		t.Errorf("broker account used as a unit: %s", got[0].Body)
	}
}

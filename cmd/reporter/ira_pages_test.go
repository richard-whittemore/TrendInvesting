package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// iraEntries is a synthetic Traditional IRA run: its configuration, a month-end
// decision withheld for labels-missing, and a sell released at one session
// and still unfilled when the next closes.
func iraEntries(t *testing.T) []journal.Entry {
	t.Helper()
	cfg := event.TargetWeightConfigurationPayload{
		StrategyID: event.TargetWeightStrategyMomentum, AccountID: "trad-ira", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR,
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
	}
	decided := event.TargetsDecidedPayload{
		AccountID: "trad-ira", StrategyID: event.TargetWeightStrategyMomentum, DecisionTime: t0, SessionPeriodEnd: t0,
		Outcome: event.TargetsOutcomeWithheld, WithheldReason: event.WithheldLabelsMissing, Detail: "too many names unlabelled",
		Glide: event.GlideFigures{RebalanceSession: "2024-01-03", AgeMonths: 360},
		Momentum: event.MomentumFigures{
			CloseSession: "2024-01-02", TrendInstrument: "SPY", TrendCloses: 300, TrendClose: 99, TrendMean: 100, RiskOff: true,
			UniverseRows: 3, UniverseListingType: 2, UniverseAtOrAboveFloor: 2, UniverseMembers: 1,
			Names: []event.MomentumName{{InstrumentID: "AAA", UniverseRank: 1, DollarVolume: 5e8, Sector: "Tech", SectorSource: "ishares-iwv-gics",
				Status: event.MomentumStatusPicked, Closes: 300, Score: 0.3, Volatility: 0.02, ScoreRank: 1, Weight: 0.49, BacktestWouldPick: true}},
		},
	}
	sell := event.OrderReleasedPayload{AccountID: "trad-ira", PlanID: "plan:s", Tag: "targets", ClientOrderID: "tw-sell", InstrumentID: "AAA",
		Side: event.OrderSideSell, Quantity: 10, LimitPrice: 101, Mode: event.TradingModePaper, Send: true,
		Figures: event.RiskFigures{NetLiquidation: 4000, ReferencePrice: 100, ExposureBefore: 0.5, ExposureAfter: 0.98, OrderFraction: 0.48, OrdersInDecision: 1}}
	next := t0.Add(24 * time.Hour)
	return []journal.Entry{
		{Kind: journal.KindInput, Envelope: envelope(t, 1, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "cfg", t0.Add(-time.Hour), cfg)},
		{Kind: journal.KindInput, Envelope: envelope(t, 2, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s0", t0, event.SessionClosedPayload{PeriodEnd: t0, InstrumentIDs: []string{"SPY"}})},
		{Kind: journal.KindDecision, Envelope: envelope(t, 3, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets", t0, decided)},
		{Kind: journal.KindDecision, Envelope: envelope(t, 4, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release", t0, sell)},
		{Kind: journal.KindInput, Envelope: envelope(t, 5, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s1", next, event.SessionClosedPayload{PeriodEnd: next, InstrumentIDs: []string{"SPY"}})},
	}
}

// TestTheReporterPagesAWithheldRebalanceAndAnIncompleteSellOfAnIRA: the two
// IRA pages reach the reporter's channels from the engine's journal, each
// naming the IRA's own account and no Boost unit.
func TestTheReporterPagesAWithheldRebalanceAndAnIncompleteSellOfAnIRA(t *testing.T) {
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("h", "v", t0.Add(-time.Hour), t0.Add(24*time.Hour)), iraEntries(t)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "trad-ira.journal")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeEnv, client := acceptingNtfy(t)
	var out bytes.Buffer
	args := []string{"-journal", path, "-state", filepath.Join(dir, "state.json"), "-channel", "stdout,ntfy"}
	if err := run(args, fakeEnv, func() time.Time { return t0.Add(48 * time.Hour) }, &out, client); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"trad-ira: rebalance withheld (labels-missing)", "trad-ira: sell of AAA incomplete"} {
		if !strings.Contains(got, want) {
			t.Errorf("the firing does not page %q:\n%s", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "boost") {
		t.Errorf("an IRA's pages name Boost:\n%s", got)
	}
}

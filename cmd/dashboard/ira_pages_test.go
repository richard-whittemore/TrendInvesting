package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

func iraEnvelope(t *testing.T, seq uint64, typ string, version uint32, id string, at time.Time, p any) event.Envelope {
	return input(t, seq, at, typ, version, id, p).Envelope
}
func iraEntries(t *testing.T, brokerAccount string) []journal.Entry {
	t.Helper()
	cfg := event.TargetWeightConfigurationPayload{
		StrategyID: event.TargetWeightStrategyMomentum, AccountID: brokerAccount, Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR,
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
		AccountID: brokerAccount, StrategyID: event.TargetWeightStrategyMomentum, DecisionTime: sessions, SessionPeriodEnd: sessions,
		Outcome: event.TargetsOutcomeWithheld, WithheldReason: event.WithheldLabelsMissing, Detail: "too many names unlabelled",
		Glide: event.GlideFigures{RebalanceSession: "2024-01-03", AgeMonths: 360},
		Momentum: event.MomentumFigures{
			CloseSession: "2024-01-02", TrendInstrument: "SPY", TrendCloses: 300, TrendClose: 99, TrendMean: 100, RiskOff: true,
			UniverseRows: 3, UniverseListingType: 2, UniverseAtOrAboveFloor: 2, UniverseMembers: 1,
			Names: []event.MomentumName{{InstrumentID: "AAA", UniverseRank: 1, DollarVolume: 5e8, Sector: "Tech", SectorSource: "ishares-iwv-gics",
				Status: event.MomentumStatusPicked, Closes: 300, Score: 0.3, Volatility: 0.02, ScoreRank: 1, Weight: 0.49, BacktestWouldPick: true}},
		},
	}
	sell := event.OrderReleasedPayload{AccountID: brokerAccount, PlanID: "plan:s", Tag: "targets", ClientOrderID: "tw-sell", InstrumentID: "AAA",
		Side: event.OrderSideSell, Quantity: 10, LimitPrice: 101, Mode: event.TradingModePaper, Send: true,
		Figures: event.RiskFigures{NetLiquidation: 4000, ReferencePrice: 100, ExposureBefore: 0.5, ExposureAfter: 0.98, OrderFraction: 0.48, OrdersInDecision: 1}}
	next := sessions.Add(24 * time.Hour)
	return []journal.Entry{
		{Kind: journal.KindInput, Envelope: iraEnvelope(t, 1, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "cfg", sessions.Add(-time.Hour), cfg)},
		{Kind: journal.KindInput, Envelope: iraEnvelope(t, 2, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s0", sessions, event.SessionClosedPayload{PeriodEnd: sessions, InstrumentIDs: []string{"SPY"}})},
		{Kind: journal.KindDecision, Envelope: iraEnvelope(t, 3, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets", sessions, decided)},
		{Kind: journal.KindDecision, Envelope: iraEnvelope(t, 4, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release", sessions, sell)},
		{Kind: journal.KindInput, Envelope: iraEnvelope(t, 5, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s1", next, event.SessionClosedPayload{PeriodEnd: next, InstrumentIDs: []string{"SPY"}})},
	}
}

func TestDashboardShowsIncompleteSellInHTMLAndJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("h", "v", sessions.Add(-time.Hour), sessions.Add(24*time.Hour)), iraEntries(t, "trad-ira")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ira.journal")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHandlerFor([]string{path}, nil, nil, "SPY")
	for _, url := range []string{"/", "/api/summary.json"} {
		rec := get(h, http.MethodGet, url)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sell of AAA incomplete") {
			t.Errorf("%s: status %d, incomplete sell absent: %s", url, rec.Code, rec.Body.String())
		}
	}
}

// Alert commands use deployment folders, never a copied journal's basename
// or broker account ID (ADR 0019, "Alerting"; docs/live-trading/lessons-2026-10.md L8).
func TestDashboardIRAUnitIdentity(t *testing.T) {
	var buf memoryFile
	log, err := journal.CreateLog(&buf, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	entries := iraEntries(t, "DU0000001")
	for _, group := range [][]journal.Entry{entries[:1], entries[1:4], entries[4:]} {
		if err := log.Append(group); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	t.Chdir(root)
	for _, tc := range []struct {
		name, path, enginesRoot string
		directory, known        bool
	}{
		{name: "standalone relative", path: "ira.log"},
		{name: "standalone absolute", path: filepath.Join(root, "ira.log")},
		{name: "copied journal", path: filepath.Join(root, "trad-ira", "run.log")},
		{name: "rehearsal directory", path: filepath.Join(root, "trad-ira"), directory: true},
		{name: "relative rehearsal directory", path: "trad-ira/", directory: true},
		{name: "configured directory", path: filepath.Join(root, "engines", "trad-ira"), enginesRoot: filepath.Join(root, "engines"), directory: true, known: true},
		{name: "relative configured directory", path: "engines/trad-ira/", enginesRoot: "engines", directory: true, known: true},
		{name: "dot configured directory", path: "engines/trad-ira/.", enginesRoot: "engines", directory: true, known: true},
		{name: "configured journal", path: filepath.Join(root, "engines", "trad-ira", "run.log"), enginesRoot: filepath.Join(root, "engines"), known: true},
		{name: "configured root is not instance", path: filepath.Join(root, "engines", "run.log"), enginesRoot: filepath.Join(root, "engines")},
		{name: "provisioned journal", path: "/var/lib/trend-investing/engines/trad-ira/run.log", known: true},
		{name: "nested non-instance folder", path: "/var/lib/trend-investing/engines/archive/trad-ira/run.log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h http.Handler
			if tc.directory {
				if err := os.MkdirAll(tc.path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(tc.path, "run.log"), buf.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
				h = newHandlerWith(source{dirs: []string{tc.path}, enginesRoot: tc.enginesRoot}, "SPY", os.ReadFile)
			} else {
				h = newHandlerWith(source{journals: []string{tc.path}, enginesRoot: tc.enginesRoot}, "SPY", func(string) ([]byte, error) { return buf.Bytes(), nil })
			}
			for _, url := range []string{"/", "/api/summary.json"} {
				rec := get(h, http.MethodGet, url)
				text := rec.Body.String()
				if rec.Code != http.StatusOK || !strings.Contains(text, "sell of AAA incomplete") {
					t.Fatalf("%s: %d: incomplete sell absent", url, rec.Code)
				}
				unknown := "instance unknown: run the reporter for the account to get the exact units"
				if tc.known {
					for _, unit := range []string{"ti-adapter@trad-ira", "ti-engine@trad-ira"} {
						if !strings.Contains(text, unit) {
							t.Errorf("%s: missing %s", url, unit)
						}
					}
					if strings.Contains(text, unknown) {
						t.Errorf("%s: provisioned instance marked unknown", url)
					}
				} else {
					if !strings.Contains(text, unknown) {
						t.Errorf("%s: missing unknown-instance instruction", url)
					}
					for _, forbidden := range []string{"systemctl stop", "ti-adapter@", "ti-engine@"} {
						if strings.Contains(text, forbidden) {
							t.Errorf("%s: unknown instance contains %q", url, forbidden)
						}
					}
				}
				if strings.Contains(text, "ti-adapter@DU") || strings.Contains(text, "ti-engine@DU") {
					t.Errorf("%s: broker account used as a unit", url)
				}
			}
		})
	}
}

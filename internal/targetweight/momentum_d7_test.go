package targetweight_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// dearAAA makes AAA's raw close 200 times its adjusted one, about $14,000 a
// share against a target of a few thousand dollars in the $10,000 IRA.
var dearAAA = map[string]float64{"AAA": 200}

func inputOf(t *testing.T, r momentumResult, id string) event.Envelope {
	t.Helper()
	for _, in := range r.inputs {
		if in.ID == id {
			return in
		}
	}
	t.Fatalf("no input %s", id)
	return event.Envelope{}
}

// TestMinimumLotSubstitutesAnUnaffordablePick runs D7-L, the IRAs' share
// quantity rule (owner decision 2026-10-16): January's first pick, AAA, costs
// more by the share than its target, so it is recorded unaffordable with its
// one share and target value, and BBB, which the sector cap had kept behind
// AAA in Tech, takes its place (ADR 0026, point 2, "D7-L"; plan.md lines
// 380-383). Only the picks held are bought.
func TestMinimumLotSubstitutesAnUnaffordablePick(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{rawScale: dearAAA})
	jan := targets(t, r.decisions)[0]
	if got := targetIDs(jan); !equalStrings(got, []string{"BBB", "CCC"}) {
		t.Fatalf("January targets %v", got)
	}
	var bar event.CompletedBarPayload
	decode(t, inputOf(t, r, "bar:AAA:2025-01-31"), &bar)
	var snap event.AccountCashSnapshotPayload
	decode(t, inputOf(t, r, "snapshot:2025-01-31"), &snap)
	if jan.Momentum.MinimumLot != (event.MinimumLotFigures{NetLiquidation: snap.NetLiquidation, MomentumScale: 1}) {
		t.Errorf("minimum lot figures %+v", jan.Momentum.MinimumLot)
	}
	for _, n := range jan.Momentum.Names {
		switch n.InstrumentID {
		case "AAA":
			if n.Status != event.MomentumStatusUnaffordable || n.OneShare != bar.Raw.Close || !(n.TargetValue > 0 && n.TargetValue < n.OneShare) || n.Weight != 0 || !n.BacktestWouldPick {
				t.Errorf("AAA recorded %+v", n)
			}
		case "BBB":
			if n.Status != event.MomentumStatusPicked || n.OneShare != 0 {
				t.Errorf("BBB recorded %+v", n)
			}
		}
	}
	for _, o := range planned(t, r.decisions) {
		if o.InstrumentID == "AAA" {
			t.Errorf("AAA planned: %+v", o)
		}
	}
	// Without the dear share the same run picks AAA, as before.
	plain := targets(t, simulateMomentum(t, momentumOptions{}).decisions)[0]
	if got := targetIDs(plain); !equalStrings(got, []string{"AAA", "CCC"}) {
		t.Errorf("January targets without the dear share %v", got)
	}
}

// TestMinimumLotValuesTheMomentumSleeveAfterTheGlide checks the target value
// uses the momentum sleeve's share of the account, 1 - b, the weight the
// planner sizes from.
func TestMinimumLotValuesTheMomentumSleeveAfterTheGlide(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{mutate: withBirth(glideBirth)})
	for _, d := range targets(t, r.decisions) {
		if d.Glide.BondShare == 0 || d.Momentum.MinimumLot.MomentumScale != 1-d.Glide.BondShare {
			t.Errorf("scale %v with b %v", d.Momentum.MinimumLot.MomentumScale, d.Glide.BondShare)
		}
	}
}

// TestATradeRoundedToNoShareIsRecorded makes IEF cost about $3,900 a share,
// so February's buy of it, scaled to the settled cash, rounds to none: the
// journal records the skip with the share fraction it was scaled to, never
// dropping it silently.
func TestATradeRoundedToNoShareIsRecorded(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{rawScale: map[string]float64{"IEF": 40}})
	skips := ofType(r.decisions, event.OrderSkippedEventType)
	if len(skips) != 1 {
		t.Fatalf("%d skips", len(skips))
	}
	var p event.OrderSkippedPayload
	decode(t, skips[0], &p)
	if p.InstrumentID != "IEF" || p.Reason != event.OrderSkippedScaledBelowOneShare || !(p.Shares > 0 && p.Shares < 1) || p.Tag != febDecisionID {
		t.Errorf("skip %+v", p)
	}
	for _, o := range planned(t, r.decisions) {
		if o.InstrumentID == "IEF" {
			t.Errorf("IEF planned: %+v", o)
		}
	}
}

const febDecisionID = "targets-decided:ira:2025-02-28T21:00:00Z"

// TestMinimumLotRunReplaysByteIdentically reruns and replays a run with a
// substitution and a skip.
func TestMinimumLotRunReplaysByteIdentically(t *testing.T) {
	opts := momentumOptions{rawScale: map[string]float64{"AAA": 200, "IEF": 40}}
	write := func(r momentumResult) []byte {
		header, err := r.recorder.Header(r.hash, strategyVersion)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := journal.Write(&buf, header, r.entries); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	first := write(simulateMomentum(t, opts))
	if !bytes.Equal(first, write(simulateMomentum(t, opts))) {
		t.Fatal("rerunning must write a byte-identical journal")
	}
	header, records, err := journal.Read(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.CheckIdentity(header, records); err != nil {
		t.Fatal(err)
	}
	inputs, decisions, err := journal.Split(records)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := targetweight.New(strategyVersion, momentumConfiguration(), targetweight.Options{DecideFrom: closeOf("2025-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := replay.New(handler)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if d := replay.Equivalent(decisions, replayed); d != nil {
		t.Fatalf("replay diverged: %+v", d)
	}
}

// TestMinimumLotDoesNotApplyWithNoMomentumSleeve puts the whole book in the
// bond sleeve (b = 1 at 53 on this schedule): no pick has a target value, so
// no substitution is made or recorded, and a zero target not held is not
// traded or recorded as skipped.
func TestMinimumLotDoesNotApplyWithNoMomentumSleeve(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{mutate: func(c *event.TargetWeightConfigurationPayload) {
		c.Glide.Birth = glideBirth
		c.Glide.Anchors = []event.GlideAnchor{{AgeYears: 38, Share: 0}, {AgeYears: 53, Share: 1}, {AgeYears: 65, Share: 1}}
	}})
	for _, d := range targets(t, r.decisions) {
		if d.Glide.BondShare != 1 || d.Momentum.MinimumLot != (event.MinimumLotFigures{}) {
			t.Errorf("b %v, minimum lot %+v", d.Glide.BondShare, d.Momentum.MinimumLot)
		}
	}
	if n := len(ofType(r.decisions, event.OrderSkippedEventType)); n != 0 {
		t.Errorf("%d skips", n)
	}
}

// TestMinimumLotMovesAFullyRiskOffBookToTheRiskOffInstrument sets
// RiskOffFraction 1 with b = 0: February is risk-off, so every pick has a
// zero target. D7-L skips only a pick with a positive target value whose one
// share exceeds it, so no name is unaffordable, the decision is made, the
// book moves to the risk-off instrument and every held stock is sold.
func TestMinimumLotMovesAFullyRiskOffBookToTheRiskOffInstrument(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{mutate: func(c *event.TargetWeightConfigurationPayload) {
		c.Momentum.RiskOffFraction = 1
	}})
	ds := targets(t, r.decisions)
	if len(ds) != 2 {
		t.Fatalf("%d decisions", len(ds))
	}
	feb := ds[1]
	if feb.Outcome != event.TargetsOutcomeDecided || !feb.Momentum.RiskOff || feb.Glide.BondShare != 0 {
		t.Fatalf("February: %s %s risk-off %v b %v", feb.Outcome, feb.WithheldReason, feb.Momentum.RiskOff, feb.Glide.BondShare)
	}
	for _, n := range feb.Momentum.Names {
		if n.Status == event.MomentumStatusUnaffordable {
			t.Errorf("%s unaffordable at a zero target: %+v", n.InstrumentID, n)
		}
	}
	for _, w := range feb.Targets {
		if w.InstrumentID != "IEF" && w.Weight != 0 {
			t.Errorf("%s target %v in a fully risk-off book", w.InstrumentID, w.Weight)
		}
	}
	if ief, ok := targetOf(feb, "IEF"); !ok || ief.Weight != 0.98 {
		t.Errorf("IEF target %+v", ief)
	}
	var phases []string
	for _, o := range planned(t, r.decisions) {
		phases = append(phases, o.Side+" "+o.InstrumentID)
	}
	if want := []string{"buy AAA", "buy CCC", "sell AAA", "sell CCC", "buy IEF"}; !equalStrings(phases, want) {
		t.Errorf("orders %v, want %v", phases, want)
	}
}

// TestMinimumLotIsNotAppliedFromALateSnapshot takes January's snapshot an
// hour after the close: the decision is withheld as stale, and the lot, which
// values picks at that close, is not applied either, so the withheld record
// names no unaffordable pick and no minimum-lot figures.
func TestMinimumLotIsNotAppliedFromALateSnapshot(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{rawScale: dearAAA, lateSnapshot: map[string]bool{"2025-01-31": true}, stopAfter: "2025-01-31"})
	jan := targets(t, r.decisions)[0]
	if jan.Outcome != event.TargetsOutcomeWithheld || jan.WithheldReason != event.WithheldStaleAccountSnapshot {
		t.Fatalf("January: %s %s", jan.Outcome, jan.WithheldReason)
	}
	if jan.Momentum.MinimumLot != (event.MinimumLotFigures{}) {
		t.Errorf("minimum lot figures %+v", jan.Momentum.MinimumLot)
	}
	for _, n := range jan.Momentum.Names {
		if n.Status == event.MomentumStatusUnaffordable {
			t.Errorf("%s unaffordable from a late snapshot: %+v", n.InstrumentID, n)
		}
	}
}

// TestMinimumLotFiguresOnlyWhenPicksWereValued withholds January before any
// pick is valued: on the trend check (no SPY bar), on a missing universe, and
// as no-eligible-names with no candidate (every member short of a 30-session
// lookback): none of these decisions records minimum-lot figures.
func TestMinimumLotFiguresOnlyWhenPicksWereValued(t *testing.T) {
	for name, opts := range map[string]momentumOptions{
		"trend check":      {noBar: map[string]string{"2025-01-31": "SPY"}, stopAfter: "2025-01-31"},
		"missing universe": {noUniverse: true, stopAfter: "2025-01-31"},
		"no candidate":     {mutate: func(c *event.TargetWeightConfigurationPayload) { c.Momentum.ScoreLookback = 30 }, stopAfter: "2025-01-31"},
	} {
		t.Run(name, func(t *testing.T) {
			jan := targets(t, simulateMomentum(t, opts).decisions)[0]
			if jan.Outcome != event.TargetsOutcomeWithheld {
				t.Fatalf("January %s", jan.Outcome)
			}
			if jan.Momentum.MinimumLot != (event.MinimumLotFigures{}) {
				t.Errorf("%s: minimum lot figures %+v", jan.WithheldReason, jan.Momentum.MinimumLot)
			}
		})
	}
}

// TestMinimumLotMovesAnUnaffordablePartialRiskOffBookToTheRiskOffInstrument
// makes January risk-off at RiskOffFraction 0.5 with every candidate's one
// share above its positive target and the IRA holding HHH, an unlabelled
// stock: the decision is made with every candidate unaffordable and the
// whole book in the risk-off instrument, so HHH is sold and IEF bought,
// rather than the decision being withheld and the old book kept.
func TestMinimumLotMovesAnUnaffordablePartialRiskOffBookToTheRiskOffInstrument(t *testing.T) {
	dear := map[string]float64{"AAA": 200, "BBB": 200, "CCC": 200, "FFF": 200, "GGG": 200}
	r := simulateMomentum(t, momentumOptions{riskOffJanuary: true, rawScale: dear, held: map[string]int64{"HHH": 10}, stopAfter: "2025-02-03"})
	jan := targets(t, r.decisions)[0]
	if jan.Outcome != event.TargetsOutcomeDecided || !jan.Momentum.RiskOff {
		t.Fatalf("January: %s %s risk-off %v", jan.Outcome, jan.WithheldReason, jan.Momentum.RiskOff)
	}
	unaffordable := 0
	for _, n := range jan.Momentum.Names {
		switch n.Status {
		case event.MomentumStatusUnaffordable:
			unaffordable++
		case event.MomentumStatusPicked:
			t.Errorf("%s picked at a share above its target", n.InstrumentID)
		}
	}
	if unaffordable == 0 {
		t.Error("no name recorded unaffordable")
	}
	if jan.Momentum.MinimumLot == (event.MinimumLotFigures{}) {
		t.Error("no minimum lot figures on a decision whose picks were valued")
	}
	for _, w := range jan.Targets {
		if w.InstrumentID != "IEF" && w.Weight != 0 {
			t.Errorf("%s target %v in an unaffordable book", w.InstrumentID, w.Weight)
		}
	}
	if ief, ok := targetOf(jan, "IEF"); !ok || ief.Weight != 0.98 {
		t.Errorf("IEF target %+v", ief)
	}
	var phases []string
	for _, o := range planned(t, r.decisions) {
		phases = append(phases, o.Side+" "+o.InstrumentID)
	}
	if want := []string{"sell HHH", "buy IEF"}; !equalStrings(phases, want) {
		t.Errorf("orders %v, want %v", phases, want)
	}
}

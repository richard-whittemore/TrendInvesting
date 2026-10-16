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

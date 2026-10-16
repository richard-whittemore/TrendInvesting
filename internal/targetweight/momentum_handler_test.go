package targetweight_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

func targetIDs(p event.TargetsDecidedPayload) []string {
	var out []string
	for _, t := range p.Targets {
		out = append(out, t.InstrumentID)
	}
	return out
}

func statuses(p event.TargetsDecidedPayload) map[string]string {
	out := map[string]string{}
	for _, n := range p.Momentum.Names {
		out[n.InstrumentID] = n.Status
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMomentumRunDecidesOnTheLastSessionOfEachMonth(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{})
	decisions := targets(t, r.decisions)
	if len(decisions) != 2 {
		t.Fatalf("%d decisions, want one at each month's last session", len(decisions))
	}
	jan, feb := decisions[0], decisions[1]
	if jan.Momentum.CloseSession != "2025-01-31" || feb.Momentum.CloseSession != "2025-02-28" {
		t.Fatalf("decided on %s and %s", jan.Momentum.CloseSession, feb.Momentum.CloseSession)
	}
	if jan.Outcome != event.TargetsOutcomeDecided || !equalStrings(targetIDs(jan), []string{"AAA", "CCC"}) || jan.Momentum.RiskOff {
		t.Errorf("January: %s %v risk-off %v", jan.Outcome, targetIDs(jan), jan.Momentum.RiskOff)
	}
	// Every ranked name is recorded with what the rule did with it: the
	// unlabelled HHH is left out though the backtest would have picked it,
	// BBB is capped behind AAA in Tech, FFF ranks after the book is full;
	// DDD (under $5) and EEE (a fund) never join the universe.
	want := map[string]string{"HHH": "no-sector", "AAA": "picked", "BBB": "sector-capped", "CCC": "picked", "FFF": "beyond-top-n"}
	got := statuses(jan)
	for id, status := range want {
		if got[id] != status {
			t.Errorf("January %s: %s, want %s", id, got[id], status)
		}
	}
	if len(got) != 5 || jan.Momentum.UniverseRows != 9 || jan.Momentum.UniverseListingType != 6 || jan.Momentum.UniverseAtOrAboveFloor != 5 {
		t.Errorf("January universe: %d names, counts %+v", len(got), jan.Momentum)
	}
	if !jan.Momentum.Names[0].BacktestWouldPick {
		t.Error("the backtest, which left an unlabelled name uncapped, would have picked HHH")
	}
	sum := 0.0
	for _, w := range jan.Targets {
		sum += w.Weight
	}
	if sum < 0.98-1e-12 || sum > 0.98+1e-12 {
		t.Errorf("January weights sum to %v, want 0.98", sum)
	}
	// February: SPY is below its mean, so half the book goes to IEF; GGG has
	// too few closes; FFF falls out of the top 5 by dollar volume.
	if feb.Outcome != event.TargetsOutcomeDecided || !feb.Momentum.RiskOff || !equalStrings(targetIDs(feb), []string{"BBB", "CCC", "IEF"}) {
		t.Errorf("February: %s %v risk-off %v", feb.Outcome, targetIDs(feb), feb.Momentum.RiskOff)
	}
	if feb.Targets[2].Weight != 0.49 || !(feb.Momentum.TrendClose < feb.Momentum.TrendMean) {
		t.Errorf("February risk-off: IEF %v, close %v, mean %v", feb.Targets[2].Weight, feb.Momentum.TrendClose, feb.Momentum.TrendMean)
	}
	if got := statuses(feb); got["GGG"] != "short-history" || got["FFF"] != "" || got["AAA"] != "beyond-top-n" {
		t.Errorf("February statuses: %v", got)
	}
}

func TestMomentumBuysAtOnceWithoutSellsThenSellsFirst(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{})
	orders := planned(t, r.decisions)
	var phases []string
	for _, o := range orders {
		phases = append(phases, o.Side+" "+o.InstrumentID)
	}
	// January holds only cash: its buys are planned at the decision. February
	// sells AAA (no longer picked) and CCC (twice its new target) at the
	// decision, and buys BBB and IEF only once both sells have filled.
	want := []string{"buy AAA", "buy CCC", "sell AAA", "sell CCC", "buy BBB", "buy IEF"}
	if !equalStrings(phases, want) {
		t.Fatalf("orders %v, want %v", phases, want)
	}
	feb := closeOf("2025-02-28")
	for i, e := range ofType(r.decisions, event.OrderPlannedEventType) {
		switch {
		case i < 4 && e.EventTime.After(feb):
			t.Errorf("order %d planned at %s, want at a decision", i, e.EventTime)
		case i >= 4 && !e.EventTime.After(closeOf("2025-03-03").Add(-7*60*60*1e9)):
			t.Errorf("buy %d planned at %s, want after the sells filled on 2025-03-03", i, e.EventTime)
		}
	}
	for _, o := range orders[4:] {
		if o.Tag != orders[2].Tag {
			t.Errorf("the buy phase serves the same decision: tag %s, want %s", o.Tag, orders[2].Tag)
		}
	}
}

// TestMomentumBuysAreBudgetedUnderTheCashRule checks the D5 rule: under
// settled-cash the sells' proceeds, unsettled the day they fill, do not fund
// the buys; under total-cash they do.
func TestMomentumBuysAreBudgetedUnderTheCashRule(t *testing.T) {
	costs := func(r momentumResult) (total float64, first event.OrderReleasedPayload) {
		buys := released(t, r.decisions)[4:]
		for _, b := range buys {
			total += b.Figures.BuyCost
		}
		return total, buys[0]
	}
	settled := simulateMomentum(t, momentumOptions{})
	settledCost, first := costs(settled)
	if !first.Figures.CashChecked || settledCost > first.Figures.CashAvailable {
		t.Errorf("settled-cash buys cost %v against %v available", settledCost, first.Figures.CashAvailable)
	}
	total := simulateMomentum(t, momentumOptions{mutate: func(c *event.TargetWeightConfigurationPayload) { c.Execution.CashRule = event.CashRuleTotal }})
	totalCost, firstTotal := costs(total)
	if !(firstTotal.Figures.CashAvailable > first.Figures.CashAvailable+5000) || totalCost > firstTotal.Figures.CashAvailable {
		t.Errorf("total-cash buys cost %v against %v available (settled-cash had %v)", totalCost, firstTotal.Figures.CashAvailable, first.Figures.CashAvailable)
	}
	// Each buy is checked against what the earlier ones left.
	buys := released(t, total.decisions)[4:]
	if buys[1].Figures.CashAvailable != buys[0].Figures.CashAvailable-buys[0].Figures.BuyCost {
		t.Errorf("second buy checked against %v, want %v - %v", buys[1].Figures.CashAvailable, buys[0].Figures.CashAvailable, buys[0].Figures.BuyCost)
	}
	// The planned buys were scaled to the budget: at full size they would
	// not fit under settled-cash.
	plannedBuys := planned(t, settled.decisions)[4:]
	full := 0.0
	for _, o := range planned(t, total.decisions)[4:] {
		full += orderplan.BuyCost(o, 1)
	}
	if len(plannedBuys) == 0 || !(full > first.Figures.CashAvailable) {
		t.Errorf("settled-cash plan %d buys; total-cash buys cost %v against %v", len(plannedBuys), full, first.Figures.CashAvailable)
	}
}

func TestMomentumBuyPhaseWaitsForAPartlyFilledSellToBeCancelled(t *testing.T) {
	// AAA's sell fills 10 of its shares and the rest is cancelled: the buys
	// follow the cancellation, and the shortfall is never borrowed.
	r := simulateMomentum(t, momentumOptions{partial: map[string]int64{"AAA": 10}})
	var cancel event.Envelope
	for _, e := range r.venue {
		if e.Type == event.OrderLifecycleEventType {
			var p event.LimitOrderLifecyclePayload
			decode(t, e, &p)
			if p.Status == event.OrderLifecycleStatusCanceled {
				cancel = e
			}
		}
	}
	buys := ofType(r.decisions, event.OrderPlannedEventType)[4:]
	if cancel.ID == "" || len(buys) == 0 {
		t.Fatalf("cancel %q, %d buys", cancel.ID, len(buys))
	}
	for _, b := range buys {
		if b.EventTime.Before(cancel.EventTime) {
			t.Errorf("buy planned at %s, before the cancel at %s", b.EventTime, cancel.EventTime)
		}
	}
}

func TestMomentumJournalIsByteIdenticalAndReplays(t *testing.T) {
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
	first := write(simulateMomentum(t, momentumOptions{}))
	if !bytes.Equal(first, write(simulateMomentum(t, momentumOptions{}))) {
		t.Fatal("rerunning the same inputs must write a byte-identical journal")
	}
	if _, err := journal.Verify(bytes.NewReader(first)); err != nil {
		t.Fatal(err)
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

func withheldReasons(t *testing.T, r momentumResult) []string {
	t.Helper()
	var out []string
	for _, d := range targets(t, r.decisions) {
		out = append(out, d.Outcome+":"+d.WithheldReason)
	}
	return out
}

func TestMomentumFailsClosed(t *testing.T) {
	for name, c := range map[string]struct {
		opts momentumOptions
		want []string
	}{
		"stale snapshot": {momentumOptions{noSnapshot: map[string]bool{"2025-01-31": true}},
			[]string{"withheld:stale-account-snapshot", "decided:"}},
		// Orders are valued at the close (ADR 0026, point 2).
		"snapshot taken after the close": {momentumOptions{lateSnapshot: map[string]bool{"2025-01-31": true}},
			[]string{"withheld:stale-account-snapshot", "decided:"}},
		// IEF is targeted risk-off in February but has no final bar.
		"risk-off instrument without its final bar": {momentumOptions{noBar: map[string]string{"2025-02-28": "IEF"}},
			[]string{"decided:", "withheld:missing-bars"}},
		"a held name without its final bar": {momentumOptions{noBar: map[string]string{"2025-02-28": "AAA"}},
			[]string{"decided:", "withheld:missing-bars"}},
		"SPY with a gap in its trend window": {momentumOptions{noBar: map[string]string{"2025-01-30": "SPY"}},
			[]string{"withheld:missing-bars", "decided:"}},
		"no universe snapshot": {momentumOptions{noUniverse: true},
			[]string{"withheld:missing-universe", "withheld:missing-universe"}},
		"orders of the last rebalance still working": {momentumOptions{silentVenue: true},
			[]string{"decided:", "withheld:rebalance-in-progress"}},
		"too few SPY closes": {momentumOptions{mutate: func(c *event.TargetWeightConfigurationPayload) { c.Momentum.TrendCloses = 30 }},
			[]string{"withheld:insufficient-trend-history", "decided:"}},
		"no name with enough history": {momentumOptions{mutate: func(c *event.TargetWeightConfigurationPayload) { c.Momentum.ScoreLookback = 25 }},
			[]string{"withheld:no-eligible-names", "decided:"}},
		"decisions before the first session to decide are quiet": {momentumOptions{decideFrom: "2025-02-03"},
			[]string{"decided:"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := simulateMomentum(t, c.opts)
			if got := withheldReasons(t, r); !equalStrings(got, c.want) {
				t.Fatalf("decisions %v, want %v", got, c.want)
			}
			for _, d := range targets(t, r.decisions) {
				if d.Outcome == event.TargetsOutcomeWithheld && d.Detail == "" {
					t.Errorf("withheld without its detail: %+v", d)
				}
			}
		})
	}
}

func TestAWithheldRebalancePlansNothing(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{noBar: map[string]string{"2025-02-28": "IEF"}})
	for _, o := range planned(t, r.decisions) {
		if o.Tag != "targets-decided:ira:2025-01-31T21:00:00Z" {
			t.Errorf("an order for a withheld decision: %+v", o)
		}
	}
	feb := targets(t, r.decisions)[1]
	if len(feb.Momentum.Names) != 5 || !feb.Momentum.RiskOff {
		t.Errorf("a withheld decision still records the ranking: %+v", feb.Momentum)
	}
}

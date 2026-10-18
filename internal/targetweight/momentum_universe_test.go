package targetweight_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// compactBar reports whether the compact stream carries a daily bar for id:
// the trend, risk-off and bond instruments, and every name held, as the
// market-data step writes them (its required and optional instruments).
func compactBar(id string, held map[string]int64) bool {
	return id == "SPY" || id == "IEF" || id == "SHY" || held[id] != 0
}

// closeHistories is the compact stream's history for the universe snapshot
// snap of calendar session i: one market.close.history per member the rule
// ranks, with its adjusted closes over the run's window, leaving out each
// session it did not trade, never closed or had no bar, as the full stream
// does.
func closeHistories(t *testing.T, cfg event.TargetWeightConfigurationPayload, cal []string, i int, snap event.UniverseSnapshotPayload, opts momentumOptions) []event.CloseHistoryPayload {
	t.Helper()
	m := cfg.Momentum
	rule := momentum.Config{
		ListingType: m.ListingType, UniverseSize: m.UniverseSize, MinPrice: m.MinPrice,
		ScoreLookback: m.ScoreLookback, ScoreSkip: m.ScoreSkip, VolatilityReturns: m.VolatilityReturns,
		TopN: m.TopN, SectorCap: m.SectorCap, Invested: m.Invested,
		TrendInstrument: m.TrendInstrument, TrendCloses: m.TrendCloses,
		RiskOffInstrument: m.RiskOffInstrument, RiskOffFraction: m.RiskOffFraction,
		MaxUnlabelledShare: m.MaxUnlabelledShare,
	}
	rows := make([]momentum.Row, len(snap.Rows))
	for k, r := range snap.Rows {
		rows[k] = momentum.Row{InstrumentID: r.InstrumentID, Type: r.Type, Close: r.Close, Volume: r.Volume, Sector: r.Sector, SectorSource: r.SectorSource}
	}
	members, _, err := momentum.RankUniverse(rule, rows)
	if err != nil {
		t.Fatal(err)
	}
	listed := slices.DeleteFunc(slices.Clone(cal[:i+1]), func(s string) bool {
		return opts.unlisted[s] || slices.Contains(opts.retract, s)
	})
	window := listed[max(0, len(listed)-max(m.ScoreLookback+1, m.TrendCloses)):]
	byID := map[string]name{}
	for _, n := range names() {
		byID[n.id] = n
	}
	var out []event.CloseHistoryPayload
	for _, member := range members {
		n := byID[member.InstrumentID]
		h := event.CloseHistoryPayload{InstrumentID: n.id, Session: cal[i], AdjustedBase: "2024-12-31", Closes: []event.SessionClose{}}
		for k, s := range cal[:i+1] {
			traded := k >= n.from && (n.to == 0 || k < n.to)
			if traded && slices.Contains(window, s) && !opts.neverClosed[s] && opts.noBar[s] != n.id {
				h.Closes = append(h.Closes, event.SessionClose{Session: s, Close: n.price(cal, k, opts.riskOffJanuary)})
			}
		}
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b event.CloseHistoryPayload) int { return compareStrings(a.InstrumentID, b.InstrumentID) })
	return out
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// requireSameDecisions fails unless the two runs emitted the same records,
// byte for byte, in the same order.
func requireSameDecisions(t *testing.T, want, got []event.Envelope) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%d records, want %d", len(got), len(want))
	}
	for i := range want {
		if want[i].ID != got[i].ID || want[i].Type != got[i].Type || !bytes.Equal(want[i].Payload, got[i].Payload) || !want[i].EventTime.Equal(got[i].EventTime) {
			t.Fatalf("record %d: %s %s\n%s\nwant %s %s\n%s", i, got[i].Type, got[i].ID, got[i].Payload, want[i].Type, want[i].ID, want[i].Payload)
		}
	}
}

// TestTheCompactStreamDecidesAsTheFullBarsDo is the replay-equivalence
// acceptance test of the compact universe input: the run fed one universe
// snapshot and a close history for each ranked member decides, plans and
// releases exactly what the run fed a bar for every listed stock every
// session does, through both rebalances and their fills.
func TestTheCompactStreamDecidesAsTheFullBarsDo(t *testing.T) {
	full := simulateMomentum(t, momentumOptions{})
	compact := simulateMomentum(t, momentumOptions{compact: true})
	if len(targets(t, full.decisions)) != 2 || len(planned(t, full.decisions)) == 0 {
		t.Fatalf("the full run made %d decisions and %d orders; the fixture should rebalance twice", len(targets(t, full.decisions)), len(planned(t, full.decisions)))
	}
	requireSameDecisions(t, full.decisions, compact.decisions)
}

// TestTheCompactStreamCarriesNoBarPerUniverseMember counts what the compact
// run's journal ingested: per session, bars only for the trend, risk-off and
// bond instruments and the names held, never one per listed stock; and close
// histories only at the two decision sessions, one per ranked member.
func TestTheCompactStreamCarriesNoBarPerUniverseMember(t *testing.T) {
	full := simulateMomentum(t, momentumOptions{})
	compact := simulateMomentum(t, momentumOptions{compact: true})
	count := func(r momentumResult, eventType string) map[string]int {
		out := map[string]int{}
		for _, in := range r.inputs {
			if in.Type == eventType {
				out[in.EventTime.UTC().Format("2006-01-02")]++
			}
		}
		return out
	}
	fullBars, bars, histories := count(full, event.CompletedBarEventType), count(compact, event.CompletedBarEventType), count(compact, event.CloseHistoryEventType)
	for session, n := range bars {
		// SPY, IEF and SHY, and at most the two names and IEF the book holds.
		if n > 5 || n >= fullBars[session] {
			t.Errorf("%s: %d bars in the compact stream, %d in the full one", session, n, fullBars[session])
		}
	}
	if len(histories) != 2 || histories["2025-01-31"] != 5 || histories["2025-02-28"] != 5 {
		t.Errorf("close histories by session %v, want the universe's 5 members at each month's last session only", histories)
	}
	if len(compact.inputs) >= len(full.inputs) {
		t.Errorf("the compact run ingested %d inputs, the full run %d", len(compact.inputs), len(full.inputs))
	}
}

// historyFixture is a momentum run configured for the compact universe
// input, with a calendar of 2025-01-30, 01-31 and 02-03, whose 01-31 is a
// decision session, and helpers for the inputs of the compact universe
// stream.
type historyFixture struct {
	t *testing.T
	h *targetweight.Handler
	s *stream
}

func newHistoryFixture(t *testing.T) historyFixture {
	t.Helper()
	return newHistoryFixtureReading(t, event.UniverseInputCompact)
}

// newHistoryFixtureReading is newHistoryFixture for a run whose
// configuration declares the universe input shape input.
func newHistoryFixtureReading(t *testing.T, input string) historyFixture {
	t.Helper()
	h, s := momentumHandlerReading(t, input)
	mustApply(t, h, calendarInput(t, s, "calendar", "2025-01-30", "2025-01-31", "2025-02-03"))
	return historyFixture{t: t, h: h, s: s}
}

// universe is the snapshot of session: AAA and BBB rank; ETF is not common stock.
func (f historyFixture) universe(session string) event.Envelope {
	return f.s.envelope(f.t, event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, "universe:"+session, closeOf(session),
		event.UniverseSnapshotPayload{Session: session, Source: "fixture-universe", Rows: []event.UniverseRow{
			{InstrumentID: "AAA", Type: "CS", Close: 20, Volume: 1e6, Sector: "Tech", SectorSource: "gics"},
			{InstrumentID: "BBB", Type: "CS", Close: 10, Volume: 1e6, Sector: "Health", SectorSource: "gics"},
			{InstrumentID: "ETF", Type: "ETF", Close: 50, Volume: 1e6},
		}})
}

func (f historyFixture) history(id, session, base string, closes ...event.SessionClose) event.Envelope {
	return f.s.envelope(f.t, event.CloseHistoryEventType, event.CloseHistorySchemaVersion, "history:"+id+":"+session, closeOf(session),
		event.CloseHistoryPayload{InstrumentID: id, Session: session, AdjustedBase: base, Closes: append([]event.SessionClose{}, closes...)})
}

func (f historyFixture) bar(id, session string, price float64) event.Envelope {
	return f.s.envelope(f.t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "bar:"+id+":"+session, closeOf(session),
		adjustedBar(id, price, 1e6, closeOf(session)))
}

func (f historyFixture) close(session string, ids ...string) event.Envelope {
	return f.s.envelope(f.t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "session:"+session, closeOf(session),
		event.SessionClosedPayload{PeriodEnd: closeOf(session), InstrumentIDs: ids})
}

func close31(price float64) event.SessionClose {
	return event.SessionClose{Session: "2025-01-31", Close: price}
}

// TestCloseHistoryRules checks how the run reads the compact universe input:
// histories follow their session's universe snapshot, carry closes on the
// calendar only, come once per member, only for a decision session, name
// exactly the members the run itself ranks, and agree with every close the run
// already holds for those instruments, on the same base.
func TestCloseHistoryRules(t *testing.T) {
	const base = "2024-12-31"
	t.Run("a history follows its session's universe snapshot", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustRefuse(t, f.h, f.history("AAA", "2025-01-31", base, close31(20)), "not the session of the universe snapshot held")
		mustApply(t, f.h, f.universe("2025-01-30"))
		mustRefuse(t, f.h, f.history("AAA", "2025-01-31", base, close31(20)), "not the session of the universe snapshot held")
	})
	t.Run("an invalid history is refused", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustRefuse(t, f.h, f.history("", "2025-01-31", base), "invalid")
	})
	t.Run("a close off the calendar is refused", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustRefuse(t, f.h, f.history("AAA", "2025-01-31", base, event.SessionClose{Session: "2025-01-29", Close: 19}), "not on the calendar")
	})
	t.Run("one history per member", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustApply(t, f.h, f.history("AAA", "2025-01-31", base, close31(20)))
		mustRefuse(t, f.h, f.history("AAA", "2025-01-31", base, close31(20)), "second close history")
	})
	t.Run("histories name exactly the members the run ranks", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustApply(t, f.h, f.history("AAA", "2025-01-31", base, close31(20)))
		mustApply(t, f.h, f.bar("SPY", "2025-01-31", 500))
		mustRefuse(t, f.h, f.close("2025-01-31", "SPY"), "but the universe ranks [AAA BBB]")
	})
	t.Run("histories arrive only for a decision session", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.universe("2025-01-30"))
		mustApply(t, f.h, f.history("AAA", "2025-01-30", base))
		mustApply(t, f.h, f.bar("SPY", "2025-01-30", 500))
		mustRefuse(t, f.h, f.close("2025-01-30", "SPY"), "not a decision session")
	})
	t.Run("histories cannot close an earlier session", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustApply(t, f.h, f.history("AAA", "2025-01-31", base))
		mustApply(t, f.h, f.bar("SPY", "2025-01-30", 500))
		mustRefuse(t, f.h, f.close("2025-01-30", "SPY"), "not a decision session")
	})
	t.Run("a close the run holds must agree", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.bar("AAA", "2025-01-31", 20))
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustApply(t, f.h, f.history("AAA", "2025-01-31", base, close31(21)))
		mustApply(t, f.h, f.history("BBB", "2025-01-31", base, close31(10)))
		mustRefuse(t, f.h, f.close("2025-01-31", "AAA"), "but the run holds 20")
	})
	t.Run("on the same base", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.bar("AAA", "2025-01-31", 20))
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustApply(t, f.h, f.history("AAA", "2025-01-31", "2025-01-30", close31(20)))
		mustApply(t, f.h, f.history("BBB", "2025-01-31", base, close31(10)))
		mustRefuse(t, f.h, f.close("2025-01-31", "AAA"), "levels on two bases")
	})
	t.Run("a member's agreeing bar and history decide together", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.bar("AAA", "2025-01-31", 20))
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustApply(t, f.h, f.history("AAA", "2025-01-31", base, close31(20)))
		mustApply(t, f.h, f.history("BBB", "2025-01-31", base, close31(10)))
		if out := mustApply(t, f.h, f.close("2025-01-31", "AAA")); len(targets(t, out)) != 1 {
			t.Fatalf("%d decisions at the decision session", len(targets(t, out)))
		}
	})
}

// TestMomentumHistoryRequestNamesTheMembersTheRunRanks pins the seam the
// market-data step asks through: for a decision session, the members the run
// will rank from the snapshot, in rank order, and the window of closes each
// history must cover; for any other session, none.
func TestMomentumHistoryRequestNamesTheMembersTheRunRanks(t *testing.T) {
	cfg := momentumConfiguration()
	cfg.Momentum.UniverseInput = event.UniverseInputCompact
	f := historyFixture{t: t, s: &stream{}}
	var snap event.UniverseSnapshotPayload
	decode(t, f.universe("2025-01-31"), &snap)

	got, err := targetweight.MomentumHistoryRequest(cfg, snap, "2025-02-03")
	if err != nil {
		t.Fatal(err)
	}
	want := targetweight.HistoryRequest{Session: "2025-01-31", DecisionSession: true, Window: 7, Members: []string{"AAA", "BBB"}}
	if !equalJSON(t, got, want) {
		t.Errorf("request %+v, want %+v", got, want)
	}
	if got, err := targetweight.MomentumHistoryRequest(cfg, snap, "2025-01-31"); err == nil {
		t.Errorf("a next session that does not follow the snapshot's was accepted: %+v", got)
	}
	snap.Session = "2025-01-30"
	if got, err := targetweight.MomentumHistoryRequest(cfg, snap, "2025-01-31"); err != nil || got.DecisionSession || len(got.Members) != 0 {
		t.Errorf("mid-month request %+v, %v; want no decision and no members", got, err)
	}
	other := snap
	other.Source = "elsewhere"
	if _, err := targetweight.MomentumHistoryRequest(cfg, other, "2025-01-31"); err == nil || !strings.Contains(err.Error(), "this run reads") {
		t.Errorf("a snapshot from another source: %v", err)
	}
	if _, err := targetweight.MomentumHistoryRequest(cfg, event.UniverseSnapshotPayload{}, "2025-01-31"); err == nil {
		t.Error("an invalid snapshot was accepted")
	}
	if _, err := targetweight.MomentumHistoryRequest(testConfiguration(), snap, "2025-01-31"); err == nil {
		t.Error("a Boost configuration was accepted")
	}
	bars := cfg
	bars.Momentum.UniverseInput = event.UniverseInputBars
	if _, err := targetweight.MomentumHistoryRequest(bars, snap, "2025-01-31"); err == nil || !strings.Contains(err.Error(), "reads bars") {
		t.Errorf("a configuration reading bars: %v", err)
	}
}

// TestADecisionSessionWithoutItsHistoriesIsWithheld: in a run configured
// for the compact input, a decision session whose ranked members sent no
// close history is withheld as histories-missing, never decided from the
// bars of the names held alone.
func TestADecisionSessionWithoutItsHistoriesIsWithheld(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{compact: true, noHistories: map[string]bool{"2025-02-28": true}})
	got := targets(t, r.decisions)
	if len(got) != 2 || got[0].Outcome != event.TargetsOutcomeDecided {
		t.Fatalf("%d decisions, the first %s; want January decided and February's", len(got), got[0].Outcome)
	}
	feb := got[1]
	if feb.Outcome != event.TargetsOutcomeWithheld || feb.WithheldReason != event.WithheldHistoriesMissing || feb.Momentum.CloseSession != "2025-02-28" {
		t.Fatalf("February is %s (%s) at %s: %s", feb.Outcome, feb.WithheldReason, feb.Momentum.CloseSession, feb.Detail)
	}
	if !strings.Contains(feb.Detail, "2025-02-28") || len(feb.Momentum.Names) != 0 {
		t.Errorf("detail %q, names %v", feb.Detail, feb.Momentum.Names)
	}
}

// TestCloseHistoryArrivalRules: a history is refused as it arrives when its
// session has already closed, and a later universe snapshot is refused while
// the held one's histories wait for their session to close.
func TestCloseHistoryArrivalRules(t *testing.T) {
	const base = "2024-12-31"
	t.Run("a history for a session already closed", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustApply(t, f.h, f.history("AAA", "2025-01-31", base, close31(20)))
		mustApply(t, f.h, f.history("BBB", "2025-01-31", base, close31(10)))
		mustApply(t, f.h, f.bar("SPY", "2025-01-31", 500))
		mustApply(t, f.h, f.close("2025-01-31", "SPY"))
		mustRefuse(t, f.h, f.history("AAA", "2025-01-31", base, close31(20)), "already closed")
	})
	t.Run("a snapshot while histories wait", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.universe("2025-01-30"))
		mustApply(t, f.h, f.history("AAA", "2025-01-30", base))
		mustRefuse(t, f.h, f.universe("2025-01-31"), "still wait")
	})
	t.Run("a ranked member's bar and its universe row agree on the raw close", func(t *testing.T) {
		f := newHistoryFixture(t)
		mustApply(t, f.h, f.bar("AAA", "2025-01-31", 21)) // the universe row says 20
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustApply(t, f.h, f.history("AAA", "2025-01-31", base, close31(21)))
		mustApply(t, f.h, f.history("BBB", "2025-01-31", base, close31(10)))
		mustRefuse(t, f.h, f.close("2025-01-31", "AAA"), "universe row close 20 for AAA disagrees with its bar's raw close 21")
	})
}

// TestTheFirstCompactDecisionSessionWithoutHistoriesIsWithheld: a run
// configured for the compact input applies its rules from the first decision
// session, before any history has been read, so a first month whose ranked
// members sent no close history is withheld as histories-missing, never
// decided from the bars of the names held alone.
func TestTheFirstCompactDecisionSessionWithoutHistoriesIsWithheld(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{compact: true, noHistories: map[string]bool{"2025-01-31": true}})
	got := targets(t, r.decisions)
	if len(got) == 0 {
		t.Fatal("no decision")
	}
	jan := got[0]
	if jan.Outcome != event.TargetsOutcomeWithheld || jan.WithheldReason != event.WithheldHistoriesMissing || jan.Momentum.CloseSession != "2025-01-31" {
		t.Fatalf("January is %s (%s) at %s: %s", jan.Outcome, jan.WithheldReason, jan.Momentum.CloseSession, jan.Detail)
	}
	if len(planned(t, r.decisions)) != 0 && got[len(got)-1].Momentum.CloseSession == "2025-01-31" {
		t.Errorf("January's withheld decision planned orders")
	}
}

// TestAUniverseOnlyCompactSessionIsWithheld: a compact run fed a decision
// session's universe snapshot and bars for SPY alone, no history at all,
// withholds that session as histories-missing.
func TestAUniverseOnlyCompactSessionIsWithheld(t *testing.T) {
	f := newHistoryFixture(t)
	mustApply(t, f.h, f.universe("2025-01-31"))
	mustApply(t, f.h, f.bar("SPY", "2025-01-31", 500))
	got := targets(t, mustApply(t, f.h, f.close("2025-01-31", "SPY")))
	if len(got) != 1 || got[0].WithheldReason != event.WithheldHistoriesMissing || !strings.Contains(got[0].Detail, "2 members ranked") {
		t.Fatalf("decisions %+v", got)
	}
}

// TestABarsRunReadsNoCloseHistory: a run configured for bars refuses a
// close history, which only the compact input carries.
func TestABarsRunReadsNoCloseHistory(t *testing.T) {
	f := newHistoryFixtureReading(t, event.UniverseInputBars)
	mustApply(t, f.h, f.universe("2025-01-31"))
	mustRefuse(t, f.h, f.history("AAA", "2025-01-31", "2024-12-31", close31(20)), "reads bars")
}

// TestABarsDecisionSessionWithNoRankedMemberBarIsWithheld: a run configured
// for bars whose decision session has its universe snapshot but no bar for
// any member ranked from it (a stream written without the members' bars) is
// withheld as missing-bars, never decided from the bars of the names held
// alone.
func TestABarsDecisionSessionWithNoRankedMemberBarIsWithheld(t *testing.T) {
	f := newHistoryFixtureReading(t, event.UniverseInputBars)
	mustApply(t, f.h, f.universe("2025-01-31"))
	mustApply(t, f.h, f.bar("SPY", "2025-01-31", 500))
	got := targets(t, mustApply(t, f.h, f.close("2025-01-31", "SPY")))
	if len(got) != 1 || got[0].WithheldReason != event.WithheldMissingBars || !strings.Contains(got[0].Detail, "2 of the 2 members ranked") {
		t.Fatalf("decisions %+v", got)
	}
}

// TestABarsDecisionSessionWithOnlyTheHeldNamesBarsIsWithheld: a run
// configured for bars fed a stream carrying bars only for SPY, IEF, SHY and
// the names held, which rank, withholds each decision as missing-bars,
// naming how many ranked members have no bar: every member ranked from the
// snapshot needs its bar that session (ADR 0026, point 5, "fail closed").
func TestABarsDecisionSessionWithOnlyTheHeldNamesBarsIsWithheld(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{heldBarsOnly: true, held: map[string]int64{"AAA": 10, "BBB": 10}})
	got := targets(t, r.decisions)
	if len(got) != 2 {
		t.Fatalf("%d decisions, want January's and February's", len(got))
	}
	for _, d := range got {
		if d.Outcome != event.TargetsOutcomeWithheld || d.WithheldReason != event.WithheldMissingBars || !strings.Contains(d.Detail, "of the 5 members ranked") {
			t.Errorf("%s is %s (%s): %s", d.Momentum.CloseSession, d.Outcome, d.WithheldReason, d.Detail)
		}
	}
	if len(planned(t, r.decisions)) != 0 {
		t.Errorf("a withheld decision planned orders")
	}
}

// TestACompactDecisionSessionRankingNoMemberNeedsNoHistory: a compact
// decision session whose snapshot ranks no member needs no history, so it
// is decided by the rule, not withheld as histories-missing.
func TestACompactDecisionSessionRankingNoMemberNeedsNoHistory(t *testing.T) {
	f := newHistoryFixture(t)
	mustApply(t, f.h, f.s.envelope(t, event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, "universe:2025-01-31", closeOf("2025-01-31"),
		event.UniverseSnapshotPayload{Session: "2025-01-31", Source: "fixture-universe", Rows: []event.UniverseRow{
			{InstrumentID: "ETF", Type: "ETF", Close: 50, Volume: 1e6},
		}}))
	mustApply(t, f.h, f.bar("SPY", "2025-01-31", 500))
	got := targets(t, mustApply(t, f.h, f.close("2025-01-31", "SPY")))
	if len(got) != 1 || got[0].WithheldReason == event.WithheldHistoriesMissing {
		t.Fatalf("decisions %+v", got)
	}
}

// TestABarsRunDecidesWithoutAMemberLeftOutOfTheDecisionSession: a member with
// no bar on the decision session is left out of that session's snapshot too,
// as the market-data step writes it (ADR 0026 point 5), so the next stock by
// dollar volume is ranked in its place, and the run configured for bars
// decides, from the bars of every member it ranks, exactly as the compact
// run does. A universe of 4 leaves FFF, the next by dollar volume, outside
// it on clean data. HHH, unlabelled, is one member in four, above the
// default unlabelled-share ceiling, so the ceiling is raised to isolate the
// missing bar.
func TestABarsRunDecidesWithoutAMemberLeftOutOfTheDecisionSession(t *testing.T) {
	four := func(c *event.TargetWeightConfigurationPayload) {
		c.Momentum.UniverseSize, c.Momentum.MaxUnlabelledShare = 4, 0.3
	}
	spoiled := map[string]string{"2025-01-31": "AAA"}
	full := simulateMomentum(t, momentumOptions{mutate: four, noBar: spoiled})
	compact := simulateMomentum(t, momentumOptions{mutate: four, noBar: spoiled, compact: true})
	clean := simulateMomentum(t, momentumOptions{mutate: four})
	decided := targets(t, full.decisions)
	if got := withheldReasons(t, full); len(got) == 0 || got[0] != "decided:" {
		t.Fatalf("decisions %v, want January's decided", got)
	}
	ranked := func(d event.TargetsDecidedPayload) []string {
		var out []string
		for _, n := range d.Momentum.Names {
			out = append(out, n.InstrumentID)
		}
		slices.Sort(out)
		return out
	}
	was, now := ranked(targets(t, clean.decisions)[0]), ranked(decided[0])
	if want := []string{"BBB", "CCC", "FFF", "HHH"}; !slices.Contains(was, "AAA") || !slices.Equal(now, want) {
		t.Fatalf("January ranked %v, want the clean run's %v with FFF, the next by dollar volume, in AAA's place", now, was)
	}
	requireSameDecisions(t, full.decisions, compact.decisions)
}

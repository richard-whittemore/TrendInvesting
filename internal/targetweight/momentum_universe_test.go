package targetweight_test

import (
	"bytes"
	"slices"
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

// historyFixture is a momentum run with a calendar of 2025-01-30, 01-31 and
// 02-03, whose 01-31 is a decision session, and helpers for the inputs of the
// compact universe stream.
type historyFixture struct {
	t *testing.T
	h *targetweight.Handler
	s *stream
}

func newHistoryFixture(t *testing.T) historyFixture {
	t.Helper()
	h, s := momentumHandler(t)
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

func close31(price float64) event.SessionClose { return event.SessionClose{Session: "2025-01-31", Close: price} }

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

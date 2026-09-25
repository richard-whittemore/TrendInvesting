package strategy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file holds ADR 0021's tests: a Session (CONTEXT.md: "Session") ends
// with market.session.closed, and only then are its Adds and entries
// decided, across the whole universe, in ADR 0010's order.
//
// The fixture is four instruments sharing Sessions:
//
//   - "EXIT" and "ADD" follow breakoutBars from day(1) and break out at
//     day(56). Each is filled there, so both hold a one-Unit Campaign.
//   - "ENTRY" and "BREAK" follow the same highs one day later, from day(2),
//     so their breakouts are at day(57).
//
// On day(57), EXIT's bar breaches its Exit Channel (low 90 below the warm
// 100), ADD's bar reaches its next Add rung, and ENTRY's and BREAK's bars
// break out. One Session therefore holds an exit, an Add and two
// simultaneous entries, which ADR 0010 orders exits, then Adds, then ranked
// entries, whatever order the bars arrived in.

const (
	sessionExit  = "EXIT"
	sessionAdd   = "ADD"
	sessionEntry = "ENTRY"
	sessionBreak = "BREAK"
)

// sessionDay57 is every instrument with a bar in day(57)'s Session.
var sessionDay57 = []string{sessionEntry, sessionAdd, sessionExit, sessionBreak}

func sessionClosedEnvelope(t *testing.T, sequence uint64, periodEnd time.Time, ids []string) event.Envelope {
	t.Helper()
	payload := mustMarshal(t, event.SessionClosedPayload{PeriodEnd: periodEnd, InstrumentIDs: ids})
	return event.Envelope{
		ID:                fmt.Sprintf("session-%d", sequence),
		Type:              event.SessionClosedEventType,
		SchemaVersion:     event.SessionClosedSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         periodEnd,
		RecordedAt:        periodEnd,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// laggedBreakoutBars is breakoutBars one day later: every bar's PeriodEnd
// moves by exactly one calendar day, so its breakout — whichever index that
// lands on, once #34's history preamble is inserted — stays exactly one day
// behind lead's own: day(57) today.
func laggedBreakoutBars(instrumentID string) []event.CompletedBarPayload {
	bars := breakoutBars(instrumentID)
	for i := range bars {
		bars[i].PeriodEnd = bars[i].PeriodEnd.AddDate(0, 0, 1)
	}
	return bars
}

// staggered appends lead's series with lagged's one day behind it, sharing
// every Session they both have a bar in: lead[0] alone, then, for each of
// lead's later bars, paired with whichever of lagged's bars falls on the
// identical PeriodEnd, if any. lagged's own bars that share no lead Session —
// #34's history preamble, whose hourly-offset timestamps
// (breakoutBars' own doc comment) never coincide with lead's — are sent on
// their own, in order, once lead's own series is exhausted. lagged's very
// last bar is left for the caller's own Session.
//
// Matching by PeriodEnd, rather than by lead[i] paired with lagged[i-1],
// is what keeps this correct once #34's preamble makes consecutive elements
// of either series no longer exactly one calendar day apart.
func staggered(s *stream, lead, lagged []event.CompletedBarPayload) *stream {
	byPeriodEnd := make(map[time.Time]event.CompletedBarPayload, len(lagged))
	for _, b := range lagged {
		byPeriodEnd[b.PeriodEnd] = b
	}
	consumed := make(map[time.Time]bool, len(lagged))

	s.bar(lead[0])
	for i := 1; i < len(lead); i++ {
		if partner, ok := byPeriodEnd[lead[i].PeriodEnd]; ok {
			s.session(lead[i], partner)
			consumed[partner.PeriodEnd] = true
			continue
		}
		s.bar(lead[i])
	}
	for _, b := range lagged[:len(lagged)-1] {
		if !consumed[b.PeriodEnd] {
			s.bar(b)
		}
	}
	return s
}

// openingFillAs is openingFill with its own fill ID: a fill ID names one
// execution across the whole run, so two instruments cannot share one.
func openingFillAs(instrumentID, fillID string) event.FillPayload {
	fill := openingFill(instrumentID)
	fill.FillID = fillID
	return fill
}

// sessionHistory is every Session up to and including day(56), with both
// breakouts filled.
//
// EXIT and ADD (day(1)-based) and ENTRY and BREAK (one day later) share the
// same 55-bar ramp shape, and #34's own breakoutHistoryPreamble bars once it
// ends, but each side's preamble sits at hourly offsets private to its own
// timeline (breakoutBars' own doc comment) — EXIT/ADD's between day(55) and
// day(56), ENTRY/BREAK's one day later, between day(56) and day(57). So
// unlike the shared 55-bar ramp, where every Session still holds a bar from
// both timelines, each side's preamble Sessions hold only that side's own
// bars — exactly as day(1)'s own opening Session above already holds only
// EXIT and ADD, before ENTRY and BREAK have a bar at all.
func sessionHistory(t *testing.T) *stream {
	t.Helper()
	s := newStream(t, validConfigurationPayload())
	exit, add := breakoutBars(sessionExit), breakoutBars(sessionAdd)
	entry, brk := laggedBreakoutBars(sessionEntry), laggedBreakoutBars(sessionBreak)
	rampLen := len(exit) - breakoutHistoryPreamble - 1 // the shared, one-day-apart ramp: 55

	s.session(exit[0], add[0])
	for i := 1; i < rampLen; i++ {
		// EXIT first, so the Session's own ranking, not arrival order, is
		// what puts ADD's entry ahead of EXIT's at day(56).
		s.session(exit[i], entry[i-1], add[i], brk[i-1])
	}
	// EXIT and ADD's own #34 history preamble: ENTRY and BREAK have no bar
	// yet in any of these Sessions.
	for i := rampLen; i < rampLen+breakoutHistoryPreamble; i++ {
		s.session(exit[i], add[i])
	}
	// day(56): EXIT and ADD break out; ENTRY and BREAK are still one bar from
	// their own — their last ramp bar, unaffected by either side's preamble.
	s.session(exit[len(exit)-1], entry[rampLen-1], add[len(add)-1], brk[rampLen-1])
	// ENTRY and BREAK's own #34 history preamble, one day later than EXIT and
	// ADD's: EXIT and ADD have already broken out and have no bar here.
	for i := rampLen; i < rampLen+breakoutHistoryPreamble; i++ {
		s.session(entry[i], brk[i])
	}
	s.fill(openingFillAs(sessionAdd, "sim-fill-add-1"))
	s.fill(openingFillAs(sessionExit, "sim-fill-exit-1"))
	return s
}

// day57Bars is the Session holding an exit, an Add and an entry.
func day57Bars(t *testing.T) map[string]event.CompletedBarPayload {
	t.Helper()
	rung, err := sizing.NextAddLevel(campaignFillPrice, breakoutFixtureN(t, validConfigurationPayload()), sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	entry, brk := laggedBreakoutBars(sessionEntry), laggedBreakoutBars(sessionBreak)
	return map[string]event.CompletedBarPayload{
		sessionExit:  postEntryBar(sessionExit, day(57), 90),
		sessionAdd:   addOpportunityBar(sessionAdd, day(57), rung+1),
		sessionEntry: entry[len(entry)-1],
		sessionBreak: brk[len(brk)-1],
	}
}

func day57Stream(t *testing.T, order []string) *stream {
	t.Helper()
	bars := day57Bars(t)
	s := sessionHistory(t)
	ordered := make([]event.CompletedBarPayload, 0, len(order))
	for _, id := range order {
		ordered = append(ordered, bars[id])
	}
	return s.session(ordered...)
}

// permutations returns every ordering of ids.
func permutations(ids []string) [][]string {
	if len(ids) <= 1 {
		return [][]string{slices.Clone(ids)}
	}
	var out [][]string
	for i := range ids {
		rest := slices.Concat(ids[:i], ids[i+1:])
		for _, p := range permutations(rest) {
			out = append(out, append([]string{ids[i]}, p...))
		}
	}
	return out
}

func indexOf(t *testing.T, emitted []event.Envelope, id string) int {
	t.Helper()
	for i, e := range emitted {
		if e.ID == id {
			return i
		}
	}
	t.Fatalf("no emission with ID %q", id)
	return -1
}

// TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments is ADR 0010's
// daily order across instruments. Falsified by proposing at bar time: with
// ENTRY's bar first, its trade proposal then precedes EXIT's exit proposal.
func TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments(t *testing.T) {
	t.Parallel()

	for _, order := range permutations(sessionDay57) {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			t.Parallel()
			emitted := day57Stream(t, order).mustRun()

			exit := indexOf(t, emitted, exitProposalID(sessionExit, day(57)))
			add := indexOf(t, emitted, addProposalID(sessionAdd, day(57), 2))
			first := indexOf(t, emitted, testDecisionID("proposal", sessionBreak, day(57)))
			second := indexOf(t, emitted, testDecisionID("proposal", sessionEntry, day(57)))
			if exit >= add || add >= first || first >= second {
				t.Fatalf("emission indexes exit %d, add %d, entries %d and %d; want exit < add < BREAK's entry < ENTRY's", exit, add, first, second)
			}

			// The proposals are the Session's decisions, not their bars'.
			last := emitted[len(emitted)-1].CausationID
			for _, i := range []int{add, first, second} {
				if emitted[i].CausationID != last {
					t.Errorf("%s caused by %q, want the session close %q", emitted[i].ID, emitted[i].CausationID, last)
				}
			}
		})
	}
}

// withoutStreamPosition strips what an envelope owes to its position in the
// input stream: Sequence and the input that caused it. ADR 0021 claims
// order-independence for everything else, because a hash chain necessarily
// follows the order its inputs arrived in.
func withoutStreamPosition(t *testing.T, e event.Envelope) []byte {
	t.Helper()
	e.Sequence, e.CausationID, e.CorrelationID = 0, "", ""
	encoded, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return encoded
}

// TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived is the
// shuffled-input test. Every permutation of day(57)'s bars yields the same
// session-close emissions byte for byte, and the same decisions per
// instrument once stream position is stripped. Falsified by sizing the
// Session's two Signals in the order their bars arrived: BREAK's and
// ENTRY's entries then swap with the permutation.
func TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived(t *testing.T) {
	t.Parallel()

	var wantClose []byte
	var wantPerInstrument map[string][][]byte
	for _, order := range permutations(sessionDay57) {
		emitted := day57Stream(t, order).mustRun()
		closeID := emitted[len(emitted)-1].CausationID

		var closeBytes []byte
		perInstrument := make(map[string][][]byte)
		for _, e := range emitted {
			if e.CausationID == closeID {
				encoded, err := json.Marshal(e)
				if err != nil {
					t.Fatalf("Marshal() error = %v", err)
				}
				closeBytes = append(closeBytes, encoded...)
			}
			id := instrumentOf(t, e)
			perInstrument[id] = append(perInstrument[id], withoutStreamPosition(t, e))
		}
		if len(closeBytes) == 0 {
			t.Fatalf("order %v: the session close emitted nothing", order)
		}
		if wantClose == nil {
			wantClose, wantPerInstrument = closeBytes, perInstrument
			continue
		}
		if !bytes.Equal(closeBytes, wantClose) {
			t.Errorf("order %v: session-close emissions differ:\n got  %s\n want %s", order, closeBytes, wantClose)
		}
		for id, want := range wantPerInstrument {
			if got := perInstrument[id]; !slices.EqualFunc(got, want, bytes.Equal) {
				t.Errorf("order %v: %s's decisions differ", order, id)
			}
		}
	}
}

// TestSimultaneousSignalsAreTakenInRankedOrder pins ADR 0010's ranking (as
// amended by the owner's decision of 2026-09-25): Strength descending, then
// 20-day median dollar volume descending, then symbol ascending. Every
// synthetic bar this package's fixtures build shares one split-adjusted
// close (syntheticBar) and one raw volume (priceView), so EXIT and ADD's
// Signals here tie on both Strength and dollar volume, and the order falls
// through to the last tie-break: ADD (alphabetically first) is taken before
// EXIT, at day(56), even though EXIT's bar arrives first in the stream.
// Falsified by taking Signals in arrival order.
// TestUnclassifiedGroupCapBindsOnStrengthNotSymbol (unit_caps_test.go) is
// this test's counterpart with genuinely different Strength values, proving
// the first two keys — not just the symbol fallback — actually govern.
func TestSimultaneousSignalsAreTakenInRankedOrder(t *testing.T) {
	t.Parallel()

	emitted := sessionHistory(t).mustRun()
	add := indexOf(t, emitted, testDecisionID("proposal", sessionAdd, day(56)))
	exit := indexOf(t, emitted, testDecisionID("proposal", sessionExit, day(56)))
	if add >= exit {
		t.Fatalf("ADD's entry at %d, EXIT's at %d; want ADD first", add, exit)
	}
	if emitted[add].CausationID != emitted[exit].CausationID {
		t.Fatalf("the two entries have different causes %q and %q; want the one session close", emitted[add].CausationID, emitted[exit].CausationID)
	}
}

// TestNoEntryOrAddIsProposedBeforeTheSessionCloses: a bar evaluates, signals
// and exits, but proposes neither an entry nor an Add. Falsified by sizing
// the Signal at bar time.
func TestNoEntryOrAddIsProposedBeforeTheSessionCloses(t *testing.T) {
	t.Parallel()

	bars := day57Bars(t)
	s := sessionHistory(t)
	for _, id := range sessionDay57 {
		s.barOnly(bars[id])
	}
	emitted := s.mustRun()
	for _, e := range emitted {
		if e.EventTime.Equal(day(57)) && (e.Type == event.TradeProposalEventType || e.Type == event.AddProposalEventType || e.Type == event.ProposalDeclinedEventType) {
			t.Errorf("%s (%s) emitted before the Session closed", e.ID, e.Type)
		}
	}
	if countFor(t, emitted, event.SignalEventType, sessionEntry) != 1 {
		t.Error("ENTRY's Signal was not emitted at its bar")
	}
	if countFor(t, emitted, event.ExitProposalEventType, sessionExit) != 1 {
		t.Error("EXIT's exit was not proposed at its bar")
	}
}

// TestAnInstrumentIsEvaluatedOncePerSession: a second bar for one instrument
// in one Session fails closed, whether or not it is delisted. Falsified by
// letting the second bar re-evaluate the Setup.
func TestAnInstrumentIsEvaluatedOncePerSession(t *testing.T) {
	t.Parallel()

	bars := day57Bars(t)
	t.Run("live", func(t *testing.T) {
		t.Parallel()
		sessionHistory(t).barOnly(bars[sessionEntry]).barOnly(bars[sessionEntry]).
			wantRunError("rejecting a duplicate or out-of-order bar")
	})
	t.Run("delisted", func(t *testing.T) {
		t.Parallel()
		// sessionHistory leaves ENTRY's own last completed bar at the end of
		// its #34 history preamble (one Session before day(57)'s breakout,
		// entry[len(entry)-1], which day57Bars itself reserves) — not simply
		// day(56) now that the preamble sits between them.
		entry := laggedBreakoutBars(sessionEntry)
		entryLastBar := entry[len(entry)-2].PeriodEnd
		sessionHistory(t).corporateAction(delistingAction(sessionEntry, entryLastBar.Add(time.Minute))).
			barOnly(bars[sessionEntry]).barOnly(bars[sessionEntry]).
			wantRunError(`instrument "ENTRY" already has a bar in the Session ending`)
	})
}

// TestASessionCloseMustNameExactlyTheBarsReceived: the named set is checked
// against what arrived, and every disagreement fails closed. Each case is
// falsified by trusting the producer's statement.
func TestASessionCloseMustNameExactlyTheBarsReceived(t *testing.T) {
	t.Parallel()

	bars := day57Bars(t)
	tests := []struct {
		name  string
		build func(*stream)
		want  string
	}{
		{
			name: "a bar missing",
			build: func(s *stream) {
				s.barOnly(bars[sessionAdd]).barOnly(bars[sessionExit]).closeSession(day(57), sessionAdd, sessionEntry, sessionExit)
			},
			want: `named but not received: ["ENTRY"]`,
		},
		{
			name: "an extra bar",
			build: func(s *stream) {
				s.barOnly(bars[sessionAdd]).barOnly(bars[sessionEntry]).barOnly(bars[sessionExit]).closeSession(day(57), sessionAdd, sessionExit)
			},
			want: `received but not named: ["ENTRY"]`,
		},
		{
			name: "the wrong period end",
			build: func(s *stream) {
				s.barOnly(bars[sessionAdd]).closeSession(day(58), sessionAdd)
			},
			want: "does not match the open Session",
		},
		{
			name:  "no Session open",
			build: func(s *stream) { s.closeSession(day(57), sessionAdd) },
			want:  "no Session is open",
		},
		{
			name: "a bar for another period end while a Session is open",
			build: func(s *stream) {
				s.barOnly(bars[sessionAdd]).barOnly(postEntryBar(sessionExit, day(58), 150))
			},
			want: "while the Session ending",
		},
		{
			name: "a bar for a Session already closed",
			build: func(s *stream) {
				s.bar(bars[sessionAdd]).barOnly(bars[sessionExit])
			},
			want: "is not after the last closed Session",
		},
		{
			name: "the stream ending inside a Session",
			build: func(s *stream) {
				s.barOnly(bars[sessionAdd]).endOfStream(day(57))
			},
			want: "Session ending 2026-02-28T00:00:00Z is still open",
		},
		{
			name: "a corporate action for an instrument the open Session has a bar for",
			build: func(s *stream) {
				s.barOnly(bars[sessionEntry]).corporateAction(delistingAction(sessionEntry, day(57)))
			},
			want: "before the Session closes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := sessionHistory(t)
			tt.build(s)
			s.wantRunError(tt.want)
		})
	}
}

// TestASessionCloseThatCannotBeReadFailsClosed: a close before any
// configuration, at a schema this build does not know, or whose payload does
// not decode or validate, is refused rather than read (ADR 0015).
func TestASessionCloseThatCannotBeReadFailsClosed(t *testing.T) {
	t.Parallel()

	bars := day57Bars(t)
	valid := sessionClosedEnvelope(t, 1, day(1), []string{"AAPL"})
	tests := []struct {
		name   string
		mutate func(*event.Envelope)
		want   string
	}{
		{"unknown schema", func(e *event.Envelope) { e.SchemaVersion = event.SessionClosedSchemaVersion + 1 }, "session closed payload schema version"},
		{"undecodable", func(e *event.Envelope) { e.Payload = json.RawMessage(`{"period_end":"not a time"}`) }, "decode session closed payload"},
		{"invalid", func(e *event.Envelope) {
			e.Payload = mustMarshal(t, event.SessionClosedPayload{PeriodEnd: day(57), InstrumentIDs: []string{sessionExit, sessionAdd}})
		}, "strictly ascending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := sessionHistory(t).barOnly(bars[sessionAdd]).barOnly(bars[sessionExit])
			closed := sessionClosedEnvelope(t, s.seq+1, day(57), []string{sessionAdd, sessionExit})
			tt.mutate(&closed)
			closed.PayloadHash = event.HashPayload(closed.Payload)
			s.seq++
			s.envelopes = append(s.envelopes, closed)
			s.wantRunError(tt.want)
		})
	}

	reducer, err := strategy.NewReducer(testStrategyVersion, validConfigurationPayload())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reducer.Apply(context.Background(), valid); err == nil || !strings.Contains(err.Error(), "before a configuration event") {
		t.Fatalf("Apply(session close before configuration) error = %v, want a refusal", err)
	}
}

// TestASessionsDecisionsAreMadeFromThePreviousCloseSnapshot: a Session's
// Adds and entries are sized against the cash stated at its previous close
// (ADR 0010), whichever order the day's bars arrive in. A snapshot stated at
// the Session's own close, delivered before it closes, is refused rather
// than funding it. Falsified by sizing from whatever snapshot is newest.
func TestASessionsDecisionsAreMadeFromThePreviousCloseSnapshot(t *testing.T) {
	t.Parallel()

	const cash = 10_000.0
	bars := day57Bars(t)
	snapshot := func(asOf time.Time) event.AccountSnapshotPayload {
		payload := defaultAccountSnapshot(validConfigurationPayload())
		payload.AsOf, payload.AvailableCash = asOf, cash
		return payload
	}

	for _, order := range permutations(sessionDay57) {
		s := sessionHistory(t).snapshot(snapshot(day(56)))
		for _, id := range order {
			s.barOnly(bars[id])
		}
		emitted := s.closeSession(day(57), sessionAdd, sessionBreak, sessionEntry, sessionExit).mustRun()
		for _, id := range []string{sessionAdd, sessionEntry, sessionBreak} {
			declined := decodeProposalDeclined(t, onlyDeclineFor(t, emitted, id, day(57)))
			if declined.Reason != event.DeclineReasonInsufficientCash || declined.AvailableCash != cash {
				t.Errorf("order %v: %s declined %q with available cash %v, want insufficient-cash against %v", order, id, declined.Reason, declined.AvailableCash, cash)
			}
		}
	}

	s := sessionHistory(t).snapshot(snapshot(day(56)))
	for _, id := range sessionDay57 {
		s.barOnly(bars[id])
	}
	s.snapshot(snapshot(day(57))).closeSession(day(57), sessionAdd, sessionBreak, sessionEntry, sessionExit).
		wantRunError("is not cash known at the previous close")
}

func onlyDeclineFor(t *testing.T, emitted []event.Envelope, instrumentID string, periodEnd time.Time) event.Envelope {
	t.Helper()
	var found []event.Envelope
	for _, e := range emitted {
		if e.Type == event.ProposalDeclinedEventType && e.EventTime.Equal(periodEnd) && instrumentOf(t, e) == instrumentID {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d declines for %s at %s, want 1", len(found), instrumentID, periodEnd)
	}
	return found[0]
}

// TestAStopFillBeforeTheSessionClosesCancelsTheAdd: a Campaign closed
// between its bar and the Session's close has no Add to decide. Falsified by
// deciding the Add from what the bar recorded without re-reading the
// Campaign.
func TestAStopFillBeforeTheSessionClosesCancelsTheAdd(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	bars := day57Bars(t)
	stop := closingStopFill(sessionAdd, testDecisionID("campaign", sessionAdd, day(56)), breakoutFixtureN(t, cfg), day(57))
	stop.UnitIDs = []string{"sim-fill-add-1"}
	stop.FillID = "sim-fill-add-stop"
	emitted := sessionHistory(t).barOnly(bars[sessionAdd]).fill(stop).closeSession(day(57), sessionAdd).mustRun()
	if n := countFor(t, emitted, event.AddProposalEventType, sessionAdd); n != 0 {
		t.Fatalf("%d Add proposals for a Campaign stopped before its Session closed, want 0", n)
	}
}

// TestReplayingASessionFixtureTwiceYieldsByteIdenticalEmissions: the whole
// multi-instrument stream replays byte for byte.
func TestReplayingASessionFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	order := []string{sessionExit, sessionEntry, sessionBreak, sessionAdd}
	first, err := json.Marshal(day57Stream(t, order).mustRun())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	second, err := json.Marshal(day57Stream(t, order).mustRun())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two replays of one stream differ")
	}
}

// withSessionCloses returns envelopes with a market.session.closed after
// each run of consecutive bars sharing one period end, naming them, and
// every Sequence renumbered contiguously from the first. It is how a fixture
// written as a plain bar stream states the Sessions ADR 0021 requires: a run
// is closed before any input that is not one of its bars.
func withSessionCloses(t *testing.T, envelopes []event.Envelope) []event.Envelope {
	t.Helper()
	if len(envelopes) == 0 {
		return envelopes
	}
	var out []event.Envelope
	var open []string
	var openEnd time.Time
	flush := func() {
		if len(open) == 0 {
			return
		}
		slices.Sort(open)
		out = append(out, sessionClosedEnvelope(t, 0, openEnd, open))
		open = nil
	}
	for _, e := range envelopes {
		var bar event.CompletedBarPayload
		isBar := e.Type == event.CompletedBarEventType && json.Unmarshal(e.Payload, &bar) == nil
		if !isBar || (len(open) > 0 && !bar.PeriodEnd.Equal(openEnd)) {
			flush()
		}
		out = append(out, e)
		if isBar {
			open, openEnd = append(open, bar.InstrumentID), bar.PeriodEnd
		}
	}
	flush()
	first := envelopes[0].Sequence
	for i := range out {
		out[i].Sequence = first + uint64(i)
		if out[i].Type == event.SessionClosedEventType {
			out[i].ID = fmt.Sprintf("session-%d", out[i].Sequence)
		}
	}
	return out
}

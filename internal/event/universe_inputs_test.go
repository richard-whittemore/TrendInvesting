package event_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validCloseHistory() event.CloseHistoryPayload {
	return event.CloseHistoryPayload{
		InstrumentID: "AAA", Session: "2025-03-04", AdjustedBase: "2025-03-03",
		Closes: []event.SessionClose{{Session: "2025-03-03", Close: 50}, {Session: "2025-03-04", Close: 51.5}},
	}
}

func validSessionRetracted() event.SessionRetractedPayload {
	return event.SessionRetractedPayload{Source: "operator", Session: "2025-03-05", Reason: "the exchange closed for a national day of mourning"}
}

func TestUniverseInputFixturesAreValid(t *testing.T) {
	for _, p := range []validator{validCloseHistory(), validSessionRetracted()} {
		if err := p.Validate(); err != nil {
			t.Errorf("%T: %v", p, err)
		}
	}
}

func TestCloseHistoryValidate(t *testing.T) {
	v := validCloseHistory()
	type P = event.CloseHistoryPayload
	closes := func(mutate func([]event.SessionClose)) func(*P) {
		return func(p *P) {
			c := append([]event.SessionClose(nil), p.Closes...)
			mutate(c)
			p.Closes = c
		}
	}
	expectInvalid(t, "instrument", v, func(p *P) { p.InstrumentID = "" }, "instrument")
	expectInvalid(t, "session", v, func(p *P) { p.Session = "2025-3-4" }, "session")
	expectInvalid(t, "base", v, func(p *P) { p.AdjustedBase = "" }, "base")
	expectInvalid(t, "base after session", v, func(p *P) { p.AdjustedBase = "2025-03-05" }, "after")
	expectInvalid(t, "close date", v, closes(func(c []event.SessionClose) { c[0].Session = "03/03" }), "date")
	expectInvalid(t, "order", v, closes(func(c []event.SessionClose) { c[1].Session = "2025-03-03" }), "ascending")
	expectInvalid(t, "after the session", v, closes(func(c []event.SessionClose) { c[1].Session = "2025-03-05" }), "look-ahead")
	expectInvalid(t, "before the base", v, func(p *P) { p.AdjustedBase = "2025-03-04" }, "before the adjusted base")
	expectInvalid(t, "zero close", v, closes(func(c []event.SessionClose) { c[0].Close = 0 }), "positive")
	expectInvalid(t, "infinite close", v, closes(func(c []event.SessionClose) { c[0].Close = math.Inf(1) }), "positive")
	empty := v
	empty.Closes = nil
	if err := empty.Validate(); err != nil {
		t.Errorf("a ranked member with no usable close is valid; the rule records it short of history: %v", err)
	}
}

func TestSessionRetractedValidate(t *testing.T) {
	v := validSessionRetracted()
	type P = event.SessionRetractedPayload
	expectInvalid(t, "source", v, func(p *P) { p.Source = "" }, "source")
	expectInvalid(t, "session", v, func(p *P) { p.Session = "2025-02-30" }, "session")
	expectInvalid(t, "reason", v, func(p *P) { p.Reason = "" }, "reason")
}

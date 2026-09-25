package strategy

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestFillChainedAddWindowAndHold pins ADR 0011's additional Session and
// ADR 0020's standing hold using actual entry and Add fill replies.
func TestFillChainedAddWindowAndHold(t *testing.T) {
	for _, kind := range []string{"entry", "add"} {
		t.Run(kind, func(t *testing.T) {
			r, fill := transitionFixture(t, kind)
			out := windowApply(t, r, fill)
			pending := r.instruments["AAPL"].pendingAddProposal
			if pending == nil {
				t.Fatal("fill reply did not chain an Add")
			}
			id := pending.proposalID
			windowAssertWire(t, out, 2)
			hold := r.holdTotal()
			// Irregular observations count Sessions, not elapsed calendar days.
			first := windowBar(t, 5)
			out = windowApply(t, r, first)
			if r.instruments["AAPL"].pendingAddProposal == nil || !slices.Contains(holdIDs(r), id) {
				t.Fatal("first subsequent bar expired the chained Add or released its hold")
			}
			for _, e := range out {
				if e.Type == event.ProposalExpiredEventType {
					t.Fatal("first bar emitted an expiry")
				}
			}
			out = windowApply(t, r, windowClose(t, 5))
			for _, e := range out {
				if e.Type == event.AddProposalEventType {
					t.Fatal("intervening close replaced the standing Add")
				}
			}
			if r.holdTotal() != hold {
				t.Fatal("intervening close changed the standing hold")
			}
			out = windowApply(t, r, windowBar(t, 9))
			if r.instruments["AAPL"].pendingAddProposal != nil || slices.Contains(holdIDs(r), id) {
				t.Fatal("second subsequent bar retained the chained Add or its hold")
			}
			found := false
			for _, e := range out {
				if e.Type == event.ProposalExpiredEventType {
					var p event.ProposalExpiredPayload
					if err := json.Unmarshal(e.Payload, &p); err != nil {
						t.Fatal(err)
					}
					if p.ProposalID == id && p.ExpiredAt.Equal(day(9)) {
						found = true
					}
				}
			}
			if !found {
				t.Fatal("second subsequent bar did not record the chain's expiry")
			}
		})
	}
}

// TestOrdinaryAddKeepsOneSession pins the unchanged session-close window
// and release of its hold at the first subsequent bar (ADR 0011, ADR 0020).
func TestOrdinaryAddKeepsOneSession(t *testing.T) {
	r, _ := transitionFixture(t, "add")
	r.instruments["AAPL"].pendingAddProposal = nil
	r.holds = nil
	out, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
		s, _ := tx.instrument("AAPL")
		return tx.evaluateAdd(s, event.Envelope{Type: event.SessionClosedEventType})
	})
	if err != nil {
		t.Fatal(err)
	}
	windowAssertWire(t, out, 1)
	id := r.instruments["AAPL"].pendingAddProposal.proposalID
	windowApply(t, r, windowBar(t, 5))
	if r.instruments["AAPL"].pendingAddProposal != nil || slices.Contains(holdIDs(r), id) {
		t.Fatal("ordinary Add or its hold survived the next bar")
	}
}

func windowAssertWire(t *testing.T, out []event.Envelope, want float64) {
	t.Helper()
	for _, e := range out {
		if e.Type == event.AddProposalEventType {
			var p map[string]any
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if e.SchemaVersion != event.AddProposalSchemaVersion || p["valid_for_sessions"] != want {
				t.Errorf("window schema=%d payload=%v, want current schema window %v", e.SchemaVersion, p["valid_for_sessions"], want)
			}
			return
		}
	}
	t.Fatal("no Add proposal")
}

func windowApply(t *testing.T, r *Reducer, input event.Envelope) []event.Envelope {
	t.Helper()
	out, err := r.transact(func(tx *transition) ([]event.Envelope, error) { return tx.apply(input) })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func windowBar(t *testing.T, at int) event.Envelope {
	t.Helper()
	e := invariantTestBarEnvelopeAt(t, 1, "AAPL", day(at), 110)
	var p event.CompletedBarPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	p.Raw.Low, p.SplitAdjusted.Low = 100, 100
	e.Payload, _ = json.Marshal(p)
	return e
}

func windowClose(t *testing.T, at int) event.Envelope {
	t.Helper()
	p, err := json.Marshal(event.SessionClosedPayload{PeriodEnd: day(at), InstrumentIDs: []string{"AAPL"}})
	if err != nil {
		t.Fatal(err)
	}
	return event.Envelope{Type: event.SessionClosedEventType, SchemaVersion: event.SessionClosedSchemaVersion, EventTime: day(at), RecordedAt: day(at), Payload: p}
}

// TestExtendedAddResolvesBeforeExpiry checks the existing fill, cancellation
// and stream-end paths while the extra-session hold stands (ADR 0011/0020).
func TestExtendedAddResolvesBeforeExpiry(t *testing.T) {
	for _, resolution := range []string{"fill", "partial-stop", "delisting", "end"} {
		t.Run(resolution, func(t *testing.T) {
			r, opening := transitionFixture(t, "add")
			windowApply(t, r, opening)
			windowApply(t, r, windowBar(t, 5))
			windowApply(t, r, windowClose(t, 5))
			pending := r.instruments["AAPL"].pendingAddProposal
			if pending == nil {
				t.Fatal("chain did not survive the next bar")
			}
			id := pending.proposalID
			fixture := resolution
			if resolution == "fill" {
				fixture = "add"
			}
			_, input := transitionFixture(t, fixture)
			input.EventTime, input.RecordedAt = day(9), day(9)
			if resolution == "partial-stop" {
				var p event.FillPayload
				if err := json.Unmarshal(input.Payload, &p); err != nil {
					t.Fatal(err)
				}
				p.FillID, p.FilledAt = "partial-stop-in-extra-session", day(9)
				input.Payload, _ = json.Marshal(p)
			}
			if resolution == "delisting" {
				p := event.CorporateActionPayload{InstrumentID: "AAPL", Kind: event.CorporateActionKindDelisting, EffectiveAt: day(9)}
				input.Payload, _ = json.Marshal(p)
			}
			if resolution == "fill" {
				c := r.instruments["AAPL"].campaign
				p := event.FillPayload{InstrumentID: "AAPL", Kind: event.FillKindAdd, CampaignID: c.campaignID, ProposalID: id, FillID: "extra-session-fill", Direction: event.DirectionLong, Quantity: pending.quantity, Price: pending.level, Level: pending.level, FilledAt: day(9)}
				input = event.Envelope{Type: event.FillEventType, SchemaVersion: event.FillSchemaVersion, EventTime: day(9), RecordedAt: day(9)}
				input.Payload, _ = json.Marshal(p)
			}
			before := len(r.fillDebits)
			windowApply(t, r, input)
			if slices.Contains(holdIDs(r), id) {
				t.Fatal("resolved chain still holds cash and cap headroom")
			}
			if resolution == "fill" {
				if len(r.fillDebits) != before+1 {
					t.Fatal("fill did not replace the hold with one debit")
				}
				windowApply(t, r, windowBar(t, 9))
			}
		})
	}
}

// TestAChainedAddEndsAtAnInterveningExit pins a limit of the extra Session
// (ADR 0011, as amended 2026-09-24): a bar that would both Add and exit
// results in the exit only (ADR 0010), so the chain expires with the bar that
// proposes the exit and its hold is released then (ADR 0020).
func TestAChainedAddEndsAtAnInterveningExit(t *testing.T) {
	r, fill := transitionFixture(t, "add")
	windowApply(t, r, fill)
	pending := r.instruments["AAPL"].pendingAddProposal
	if pending == nil || !pending.survivesNextBar {
		t.Fatal("fill reply did not chain a two-Session Add")
	}
	id := pending.proposalID
	// The fixture's Exit Channel low is 99; a low of 98 breaches it.
	bar := windowBar(t, 5)
	var p event.CompletedBarPayload
	if err := json.Unmarshal(bar.Payload, &p); err != nil {
		t.Fatal(err)
	}
	p.Raw.Low, p.SplitAdjusted.Low = 98, 98
	bar.Payload, _ = json.Marshal(p)
	out := windowApply(t, r, bar)
	if r.instruments["AAPL"].pendingAddProposal != nil || slices.Contains(holdIDs(r), id) {
		t.Fatal("the chained Add or its hold outlived the exit bar")
	}
	expired, exit := false, false
	for _, e := range out {
		switch e.Type {
		case event.ExitProposalEventType:
			exit = true
		case event.ProposalExpiredEventType:
			var p event.ProposalExpiredPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			expired = expired || p.ProposalID == id && p.ExpiredAt.Equal(day(5)) &&
				p.Reason == event.ExpiryReasonSupersededByNextBar
		}
	}
	if !exit || !expired {
		t.Fatalf("exit proposed = %v, chain expired with the bar = %v; want both", exit, expired)
	}
}

// TestAFullStopOutExpiresThePendingAdd: a stop fill that closes the whole
// Campaign cancels its pending Add in the same transition, exactly as a
// partial stop-out does, whether the Add is ordinary or fill-chained. It is
// not left for a later bar to expire: a daily adapter reports the next
// Session's fills before that bar, so the Add's order could otherwise fill
// into a Campaign that no longer exists (ADR 0011 and ADR 0020, as amended
// 2026-09-24). The expiry reuses the stop-superseded reason and its hold is
// released at once.
func TestAFullStopOutExpiresThePendingAdd(t *testing.T) {
	for _, kind := range []string{"ordinary", "fill-chained"} {
		t.Run(kind, func(t *testing.T) {
			var r *Reducer
			var stop event.Envelope
			if kind == "ordinary" {
				r, stop = transitionFixture(t, "full-stop")
			} else {
				var fill event.Envelope
				r, fill = transitionFixture(t, "add")
				windowApply(t, r, fill)
				if p := r.instruments["AAPL"].pendingAddProposal; p == nil || !p.survivesNextBar {
					t.Fatal("fill reply did not chain a two-Session Add")
				}
				c := r.instruments["AAPL"].campaign
				ids := make([]string, 0, len(c.units))
				var quantity int64
				for _, u := range c.units {
					ids = append(ids, u.openingFillID)
					quantity += u.quantity
				}
				payload := event.FillPayload{InstrumentID: "AAPL", Kind: event.FillKindStop, CampaignID: c.campaignID, FillID: "closing-stop", UnitIDs: ids, Direction: event.DirectionLong, Quantity: quantity, Price: 98, Level: 98, FilledAt: day(4)}
				stop = event.Envelope{Type: event.FillEventType, SchemaVersion: event.FillSchemaVersion, EventTime: day(4), RecordedAt: day(4)}
				stop.Payload, _ = json.Marshal(payload)
			}
			id := r.instruments["AAPL"].pendingAddProposal.proposalID
			out := windowApply(t, r, stop)
			if r.instruments["AAPL"].campaign != nil {
				t.Fatal("the stop did not close the Campaign")
			}
			if r.instruments["AAPL"].pendingAddProposal != nil || slices.Contains(holdIDs(r), id) {
				t.Fatal("the pending Add or its hold outlived the full stop-out")
			}
			var types []string
			expired := false
			for _, e := range out {
				types = append(types, e.Type)
				if e.Type == event.ProposalExpiredEventType {
					var p event.ProposalExpiredPayload
					if err := json.Unmarshal(e.Payload, &p); err != nil {
						t.Fatal(err)
					}
					expired = p.ProposalID == id && p.Kind == event.ProposalKindAdd &&
						p.Reason == event.ExpiryReasonSupersededByStop &&
						p.Rule == event.RuleAddProposalSupersededByStop
				}
			}
			want := []string{event.CampaignUnitsStoppedEventType, event.ProposalExpiredEventType, event.CampaignExitedEventType}
			if !expired || !slices.Equal(types, want) {
				t.Fatalf("emissions %v (Add expired by the stop = %v), want %v", types, expired, want)
			}
			// The next bar has nothing left to expire.
			for _, e := range windowApply(t, r, windowBar(t, 5)) {
				if e.Type == event.ProposalExpiredEventType {
					t.Fatal("the next bar expired the Add a second time")
				}
			}
		})
	}
}

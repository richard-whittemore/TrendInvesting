package targetweight_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// ADR 0026, amendment of 2026-10-29: replay preserves the adapter's
// exact reason; lack of evidence is never classified as a final refusal.
func TestMomentumWithheldPickPreservesAdapterEvidence(t *testing.T) {
	for _, tc := range []struct{ failure, detail string }{
		{event.MarkFailureTransient, "IBKR's answer to its contract request did not end within 5s, asked twice"},
		{event.MarkFailureFinal, "IBKR answered its contract request with error 200: No security definition"},
	} {
		t.Run(tc.failure, func(t *testing.T) {
			r := simulateMomentum(t, momentumOptions{noMark: map[string]string{"2025-01-31": "AAA"}, stopAfter: "2025-01-31",
				beforeClose: func(s *stream, session string) []event.Envelope {
					if session != "2025-01-31" {
						return nil
					}
					at := closeOf(session)
					return []event.Envelope{s.envelope(t, event.MarkUnavailableEventType, event.MarkUnavailableSchemaVersion, "unavailable", at,
						event.MarkUnavailablePayload{InstrumentID: "AAA", AsOf: at, Source: event.AdapterIBKR, Failure: tc.failure, Detail: tc.detail})}
				}})
			p := targets(t, r.decisions)[0]
			if len(p.WithheldPicks) != 1 || p.WithheldPicks[0].Detail != tc.detail || p.WithheldPicks[0].Failure != tc.failure {
				t.Fatalf("evidence: %+v", p.WithheldPicks)
			}
		})
	}
}

func TestMomentumUnavailableMarkObservationsAdvance(t *testing.T) {
	h, s := momentumHandler(t)
	at := closeOf("2025-01-02")
	failure := func(id string, when time.Time) event.Envelope {
		return s.envelope(t, event.MarkUnavailableEventType, event.MarkUnavailableSchemaVersion, id, when,
			event.MarkUnavailablePayload{InstrumentID: "AAA", AsOf: when, Source: event.AdapterIBKR, Failure: event.MarkFailureTransient, Detail: "timeout"})
	}
	mark := func(id string, when time.Time) event.Envelope {
		return s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, id, when,
			event.MarkRecordedPayload{InstrumentID: "AAA", AsOf: when, Source: event.AdapterIBKR, Price: 100})
	}
	mustApply(t, h, failure("failed", at))
	mustRefuse(t, h, failure("repeated", at), "does not follow")
	mustRefuse(t, h, mark("old-mark", at), "does not follow")
	mustApply(t, h, mark("resolved", at.Add(time.Minute)))
	mustRefuse(t, h, failure("old-failure", at.Add(time.Minute)), "does not follow")
	bad := s.envelope(t, event.MarkUnavailableEventType, event.MarkUnavailableSchemaVersion, "invalid", at, event.MarkUnavailablePayload{})
	mustRefuse(t, h, bad, "invalid")
}

func TestRulesVersionDeclaresPickWithholding(t *testing.T) {
	if targetweight.RulesVersion == "1.28.0" {
		t.Fatal("ADR 0016: withholding an IRA pick changes decisions and requires a rules bump")
	}
}

// A decided month's withholding survives the sell/buy boundary even if
// a later observation resolves the name (ADR 0026, 2026-10-29).
func TestMomentumWithheldPickStaysCashAfterSellsAndALaterMark(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{noMark: map[string]string{"2025-02-28": "BBB"}, stopAfter: "2025-03-03"})
	h, s := momentumHandler(t)
	var decisions []event.Envelope
	inserted := false
	for _, in := range r.inputs {
		if in.Type == event.TargetWeightConfigurationEventType {
			continue
		}
		decisions = append(decisions, mustApply(t, h, s.envelope(t, in.Type, in.SchemaVersion, in.ID, in.EventTime, in.Payload))...)
		if in.ID == "session:2025-02-28" {
			at := closeOf("2025-02-28").Add(time.Hour)
			decisions = append(decisions, mustApply(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "late-BBB", at,
				event.MarkRecordedPayload{InstrumentID: "BBB", AsOf: at, Source: event.AdapterIBKR, Price: 100}))...)
			inserted = true
		}
	}
	if !inserted {
		t.Fatal("fixture never crossed the two-phase decision")
	}
	feb := targets(t, decisions)[1]
	if len(feb.WithheldPicks) != 1 || feb.WithheldPicks[0].InstrumentID != "BBB" {
		t.Fatalf("February: %+v", feb.WithheldPicks)
	}
	refusals := ofType(decisions, event.OrderRefusedEventType)
	if len(refusals) != 1 {
		t.Fatalf("refusals: %d", len(refusals))
	}
	var p event.OrderRefusedPayload
	decode(t, refusals[0], &p)
	if p.InstrumentID != "BBB" || p.Reason != event.RefusedMissingMark {
		t.Fatalf("late mark restored withheld buy: %+v", p)
	}
	if len(ofType(decisions, event.EngineStateEventType)) != 0 {
		t.Fatal("withholding degraded")
	}
	for _, buy := range released(t, decisions) {
		if buy.Side == event.OrderSideBuy && buy.InstrumentID == "IEF" {
			return
		}
	}
	t.Fatal("other buy after the sell phase was not released")
}

// ADR 0026, amendment of 2026-10-29: a buy needs an available mark
// when the sell phase ends, even if its mark was valid at the decision.
func TestMomentumLaterUnavailableMarkWithholdsDelayedBuy(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{stopAfter: "2025-03-03"})
	h, s := momentumHandler(t)
	var decisions, buyPhase []event.Envelope
	inserted := false
	for _, in := range r.inputs {
		if in.Type == event.TargetWeightConfigurationEventType {
			continue
		}
		out := mustApply(t, h, s.envelope(t, in.Type, in.SchemaVersion, in.ID, in.EventTime, in.Payload))
		decisions = append(decisions, out...)
		if in.ID == "session:2025-02-28" {
			at := closeOf("2025-03-03").Add(-390 * time.Minute)
			mustApply(t, h, s.envelope(t, event.MarkUnavailableEventType, event.MarkUnavailableSchemaVersion, "later-unavailable-BBB", at,
				event.MarkUnavailablePayload{InstrumentID: "BBB", AsOf: at, Source: event.AdapterIBKR, Failure: event.MarkFailureTransient, Detail: "mark lookup timed out"}))
			inserted = true
			continue
		}
		if inserted && len(ofType(out, event.OrderPlannedEventType)) != 0 {
			buyPhase = out
			break
		}
	}
	if !inserted || len(buyPhase) == 0 {
		t.Fatal("fixture never crossed the sell/buy boundary after the unavailable observation")
	}
	feb := targets(t, decisions)[1]
	if len(feb.WithheldPicks) != 0 {
		t.Fatalf("mark was already missing at the decision: %+v", feb.WithheldPicks)
	}
	refusals := ofType(buyPhase, event.OrderRefusedEventType)
	if len(refusals) != 1 {
		t.Fatalf("delayed buy refusals: %d, want one for BBB's superseded mark; released: %+v", len(refusals), released(t, buyPhase))
	}
	var p event.OrderRefusedPayload
	decode(t, refusals[0], &p)
	if p.InstrumentID != "BBB" || p.Reason != event.RefusedMissingMark || p.Figures.MarkSet {
		t.Fatalf("superseded mark did not withhold BBB: %+v", p)
	}
	buys := released(t, buyPhase)
	if len(buys) != 1 || buys[0].Side != event.OrderSideBuy || buys[0].InstrumentID != "IEF" {
		t.Fatalf("other buy after the sell phase: %+v", buys)
	}
	if len(ofType(decisions, event.EngineStateEventType)) != 0 {
		t.Fatal("withholding degraded")
	}
}

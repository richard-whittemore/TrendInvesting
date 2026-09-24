package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// The planned universe size and a realistic accepted-fill history for a long
// backtest: the per-input transaction cost must not scale with either
// (docs/development.md: reducer transactions).
const (
	benchUniverseInstruments = 1_000
	benchAcceptedFills       = 3_000
	benchWarmUpBars          = 80
)

func benchBarEnvelope(tb testing.TB, instrumentID string, periodEnd time.Time) event.Envelope {
	tb.Helper()
	return benchBarEnvelopeAt(tb, instrumentID, periodEnd, 101)
}

// benchBarEnvelopeAt is benchBarEnvelope with the given high. Every warm-up
// bar's high is 101, so any higher one is a breakout.
func benchBarEnvelopeAt(tb testing.TB, instrumentID string, periodEnd time.Time, high float64) event.Envelope {
	tb.Helper()
	// The whole bar moves with its high, so a breakout's True Range, and
	// with it N and the Protective Stop, stays bounded however many
	// Sessions the benchmark runs.
	low, closing := high-2, high-1
	if high <= 101 {
		low, closing = 99, 100
	}
	bar := event.CompletedBarPayload{
		InstrumentID:  instrumentID,
		PeriodEnd:     periodEnd,
		SplitAdjusted: event.PriceView{View: event.ViewSplitAdjusted, Open: closing, High: high, Low: low, Close: closing, Volume: 1_000_000},
		Raw:           event.PriceView{View: event.ViewRaw, Open: closing, High: high, Low: low, Close: closing, Volume: 1_000_000},
	}
	return benchEnvelope(tb, "bench-bar-"+instrumentID+"-"+periodEnd.Format(time.RFC3339), event.CompletedBarEventType, event.CompletedBarSchemaVersion, periodEnd, bar)
}

// benchSessionClosedEnvelope ends the Session at periodEnd, naming ids.
func benchSessionClosedEnvelope(tb testing.TB, periodEnd time.Time, ids []string) event.Envelope {
	tb.Helper()
	return benchEnvelope(tb, "bench-session-"+periodEnd.Format(time.RFC3339), event.SessionClosedEventType, event.SessionClosedSchemaVersion, periodEnd,
		event.SessionClosedPayload{PeriodEnd: periodEnd, InstrumentIDs: ids})
}

func benchEnvelope(tb testing.TB, id, eventType string, schemaVersion uint32, at time.Time, v any) event.Envelope {
	tb.Helper()
	payload, err := json.Marshal(v)
	if err != nil {
		tb.Fatal(err)
	}
	return event.Envelope{
		ID:                id,
		Type:              eventType,
		SchemaVersion:     schemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        at,
		Source:            "fixture",
		StrategyVersion:   invariantTestStrategyVersion,
		ConfigurationHash: event.ConfigurationHash(invariantTestConfigurationPayload()),
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

func benchConfiguredReducer(tb testing.TB) *Reducer {
	tb.Helper()
	config := invariantTestConfigurationPayload()
	r, err := NewReducer(invariantTestStrategyVersion, config)
	if err != nil {
		tb.Fatal(err)
	}
	payload, err := json.Marshal(config)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := r.Apply(context.Background(), event.Envelope{
		ID: "bench-config", Type: event.ConfigurationEventType, SchemaVersion: event.ConfigurationSchemaVersion,
		EnvelopeVersion: event.CurrentEnvelopeVersion, EventTime: day(0), RecordedAt: day(0), Source: "fixture",
		StrategyVersion: invariantTestStrategyVersion, ConfigurationHash: event.ConfigurationHash(config),
		PayloadHash: event.HashPayload(payload), Payload: payload,
	}); err != nil {
		tb.Fatal(err)
	}
	return r
}

// benchWideUniverse builds a reducer holding benchUniverseInstruments
// instruments, each past indicator warm-up, plus benchAcceptedFills accepted
// fills. Each instrument is warmed on its own single-instrument reducer so
// the setup cost does not depend on the transaction implementation under
// measurement.
func benchWideUniverse(tb testing.TB) *Reducer {
	tb.Helper()
	return benchUniverse(tb, benchUniverseInstruments)
}

// benchUniverse is benchWideUniverse with instruments instruments.
func benchUniverse(tb testing.TB, instruments int) *Reducer {
	tb.Helper()
	r := benchConfiguredReducer(tb)
	for i := range instruments {
		id := fmt.Sprintf("I%04d", i)
		warm := benchConfiguredReducer(tb)
		for d := range benchWarmUpBars {
			if _, err := warm.Apply(context.Background(), benchBarEnvelope(tb, id, day(d+1))); err != nil {
				tb.Fatal(err)
			}
			if _, err := warm.Apply(context.Background(), benchSessionClosedEnvelope(tb, day(d+1), []string{id})); err != nil {
				tb.Fatal(err)
			}
		}
		r.instruments[id] = warm.instruments[id]
	}
	// Every instrument's last Session is closed; the next may open after it.
	r.hasClosedSession, r.lastClosedSession = true, day(benchWarmUpBars)
	// ADR 0010's cash basis, so a Signal in the benchmark is sized rather
	// than refused for want of a snapshot.
	r.hasAvailableCash, r.availableCash, r.availableCashAsOf = true, 1e12, day(0)
	for i := range benchAcceptedFills {
		id := fmt.Sprintf("fill-%05d", i)
		r.acceptedFills[id] = acceptedFillState{
			instrumentID: fmt.Sprintf("I%04d", i%instruments), kind: event.FillKindStop,
			unitIDs: []string{id + "-unit"}, quantity: 1, price: 100, direction: event.DirectionLong, filledAt: day(1),
		}
	}
	return r
}

// BenchmarkApplyBarWideUniverse measures one Apply of one instrument's
// completed bar against a whole-universe reducer. Each bar is its own
// Session (ADR 0021), whose close is applied outside the timer.
func BenchmarkApplyBarWideUniverse(b *testing.B) {
	r := benchWideUniverse(b)
	inputs := make([]event.Envelope, b.N)
	closes := make([]event.Envelope, b.N)
	for i := range inputs {
		inputs[i] = benchBarEnvelope(b, "I0500", day(benchWarmUpBars+1+i))
		closes[i] = benchSessionClosedEnvelope(b, day(benchWarmUpBars+1+i), []string{"I0500"})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, err := r.Apply(context.Background(), inputs[i]); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if _, err := r.Apply(context.Background(), closes[i]); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

// BenchmarkApplySessionCloseWideUniverse measures one Apply of the
// market.session.closed ending a Session in which every one of the
// universe's instruments delivered a bar: the pass reads all of them and
// copies only those it proposes for (docs/development.md: reducer
// transactions). The Session's bars are applied outside the timer.
func BenchmarkApplySessionCloseWideUniverse(b *testing.B) {
	for _, signals := range []int{0, 10, 100} {
		b.Run(fmt.Sprintf("signals=%d", signals), func(b *testing.B) {
			r := benchWideUniverse(b)
			ids := make([]string, benchUniverseInstruments)
			for i := range ids {
				ids[i] = fmt.Sprintf("I%04d", i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for n := range b.N {
				b.StopTimer()
				periodEnd := day(benchWarmUpBars + 1 + n)
				for i, id := range ids {
					// A rising high breaks out every Session.
					high := 101.0
					if i < signals {
						high = 200 + float64(n)
					}
					if _, err := r.Apply(context.Background(), benchBarEnvelopeAt(b, id, periodEnd, high)); err != nil {
						b.Fatal(err)
					}
				}
				closed := benchSessionClosedEnvelope(b, periodEnd, ids)
				b.StartTimer()
				proposed, err := r.Apply(context.Background(), closed)
				if err != nil {
					b.Fatal(err)
				}
				if len(proposed) != signals {
					b.Fatalf("the session close emitted %d decisions, want one per Signal (%d)", len(proposed), signals)
				}
				for _, d := range proposed {
					if d.Type != event.TradeProposalEventType {
						b.Fatalf("the session close emitted %s, want only trade proposals: the benchmark must measure the proposal path", d.Type)
					}
				}
			}
		})
	}
}

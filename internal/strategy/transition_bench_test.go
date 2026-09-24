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
	bar := event.CompletedBarPayload{
		InstrumentID:  instrumentID,
		PeriodEnd:     periodEnd,
		SplitAdjusted: event.PriceView{View: event.ViewSplitAdjusted, Open: 100, High: 101, Low: 99, Close: 100, Volume: 1_000_000},
		Raw:           event.PriceView{View: event.ViewRaw, Open: 100, High: 101, Low: 99, Close: 100, Volume: 1_000_000},
	}
	payload, err := json.Marshal(bar)
	if err != nil {
		tb.Fatal(err)
	}
	return event.Envelope{
		ID:                "bench-bar-" + instrumentID + "-" + periodEnd.Format(time.RFC3339),
		Type:              event.CompletedBarEventType,
		SchemaVersion:     event.CompletedBarSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         periodEnd,
		RecordedAt:        periodEnd,
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
	r := benchConfiguredReducer(tb)
	for i := range benchUniverseInstruments {
		id := fmt.Sprintf("I%04d", i)
		warm := benchConfiguredReducer(tb)
		for d := range benchWarmUpBars {
			if _, err := warm.Apply(context.Background(), benchBarEnvelope(tb, id, day(d+1))); err != nil {
				tb.Fatal(err)
			}
		}
		r.instruments[id] = warm.instruments[id]
	}
	for i := range benchAcceptedFills {
		id := fmt.Sprintf("fill-%05d", i)
		r.acceptedFills[id] = acceptedFillState{
			instrumentID: fmt.Sprintf("I%04d", i%benchUniverseInstruments), kind: event.FillKindStop,
			unitIDs: []string{id + "-unit"}, quantity: 1, price: 100, direction: event.DirectionLong, filledAt: day(1),
		}
	}
	return r
}

// BenchmarkApplyBarWideUniverse measures one Apply of one instrument's
// completed bar against a whole-universe reducer.
func BenchmarkApplyBarWideUniverse(b *testing.B) {
	r := benchWideUniverse(b)
	inputs := make([]event.Envelope, b.N)
	for i := range inputs {
		inputs[i] = benchBarEnvelope(b, "I0500", day(benchWarmUpBars+1+i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, err := r.Apply(context.Background(), inputs[i]); err != nil {
			b.Fatal(err)
		}
	}
}

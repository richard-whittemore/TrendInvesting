// Package spike is the measurement harness for the LEAN-to-Go transport
// spike (issue #26). It is not part of the trading path: it exists to produce
// the numbers recorded in docs/adr/0014-lean-go-transport.md, and to give the
// Python side a Go peer that behaves like a real decision engine.
//
// The bar payload it generates is mirrored byte-for-byte in
// adapter/lean/spike/, so both sides measure the same message.
package spike

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// Clock supplies wall-clock readings. It is a parameter rather than a call to
// time.Now because the determinism rules in .golangci.yml forbid ambient time
// outside cmd/ and adapter/, and a measurement harness is the one place where
// reading a clock is the point.
type Clock func() time.Time

// StrategyVersion labels envelopes produced by the harness so a journal cannot
// confuse spike traffic with a real run.
const StrategyVersion = "transport-spike"

// Bar is one instrument's completed daily bar.
type Bar struct {
	Symbol string  `json:"symbol"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume int64   `json:"volume"`
}

// BarSlice is the payload of one completed-bar envelope: the whole universe
// for one trading day, which is how a daily-bar strategy actually sees data.
type BarSlice struct {
	AsOf string `json:"as_of"`
	Bars []Bar  `json:"bars"`
}

// Proposal is one trade the engine wants placed.
type Proposal struct {
	Symbol    string  `json:"symbol"`
	Direction string  `json:"direction"`
	Quantity  int64   `json:"quantity"`
	StopPrice float64 `json:"stop_price"`
}

// Decision is the payload of a decision envelope.
type Decision struct {
	Considered int        `json:"considered"`
	Proposals  []Proposal `json:"proposals"`
}

// maxProposals bounds the reply so the measurement reflects the real shape of
// the exchange: a large request and a small reply.
const maxProposals = 10

// NewBarEnvelope builds a completed-bar envelope covering universe symbols.
//
// Prices are generated arithmetically rather than randomly: the harness must
// produce the same bytes on every run so that a latency difference between two
// runs is a property of the transport and not of the data.
func NewBarEnvelope(sequence uint64, universe int, now Clock) event.Envelope {
	bars := make([]Bar, 0, universe)
	for i := range universe {
		// Two coprime strides over a fixed range give every symbol a distinct
		// price that still changes from bar to bar. The arithmetic is done in
		// float64 so no width conversion can wrap.
		spread := math.Mod(float64(sequence%1000)*7919+float64(i)*104729, 18000)
		base := 20 + spread/100
		bars = append(bars, Bar{
			Symbol: fmt.Sprintf("SPK%04d", i),
			Open:   round2(base),
			High:   round2(base * 1.012),
			Low:    round2(base * 0.991),
			Close:  round2(base * 1.004),
			Volume: int64(100000 + (i*13)%900000),
		})
	}
	payload, err := json.Marshal(BarSlice{
		AsOf: "2026-09-09",
		Bars: bars,
	})
	if err != nil {
		// BarSlice contains only strings and numbers, so this cannot fail;
		// panicking keeps the harness's signature honest instead of returning
		// an error that no caller could act on.
		panic("spike: encode bar slice: " + err.Error())
	}
	observed := now()
	return event.Envelope{
		ID:                "bar-" + strconv.FormatUint(sequence, 10),
		Type:              "market.bar.completed",
		SchemaVersion:     1,
		EventTime:         observed,
		RecordedAt:        observed,
		Sequence:          sequence,
		CorrelationID:     "spike-run",
		Source:            "lean-adapter",
		StrategyVersion:   StrategyVersion,
		ConfigurationHash: "spike",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// Decider is the reference decision engine for the spike.
//
// It does the work a real engine's boundary would do — decode the whole
// universe, look at every bar, emit a small set of proposals — so the
// measurement is not a strawman that only moves bytes.
func Decider(now Clock) transport.Decider {
	return func(_ context.Context, bar event.Envelope) (event.Envelope, error) {
		var slice BarSlice
		if err := json.Unmarshal(bar.Payload, &slice); err != nil {
			return event.Envelope{}, fmt.Errorf("spike: decode bar payload: %w", err)
		}
		proposals := make([]Proposal, 0, maxProposals)
		for _, candidate := range slice.Bars {
			if candidate.Close <= candidate.Open || len(proposals) == maxProposals {
				continue
			}
			proposals = append(proposals, Proposal{
				Symbol:    candidate.Symbol,
				Direction: "long",
				Quantity:  100,
				StopPrice: round2(candidate.Close * 0.98),
			})
		}
		payload, err := json.Marshal(Decision{
			Considered: len(slice.Bars),
			Proposals:  proposals,
		})
		if err != nil {
			return event.Envelope{}, fmt.Errorf("spike: encode decision: %w", err)
		}
		decided := now()
		return event.Envelope{
			ID:                "decision-" + bar.ID,
			Type:              "decision.proposed",
			SchemaVersion:     1,
			EventTime:         bar.EventTime,
			RecordedAt:        decided,
			Sequence:          bar.Sequence,
			CorrelationID:     bar.CorrelationID,
			CausationID:       bar.ID,
			Source:            "decision-engine",
			StrategyVersion:   StrategyVersion,
			ConfigurationHash: bar.ConfigurationHash,
			PayloadHash:       event.HashPayload(payload),
			Payload:           payload,
		}, nil
	}
}

// Stats summarises a run of round trips.
type Stats struct {
	Count        int
	RequestBytes int
	Min          time.Duration
	Median       time.Duration
	P95          time.Duration
	P99          time.Duration
	Max          time.Duration
	Mean         time.Duration
}

// Summarise reduces raw samples to the percentiles the ADR reports. It sorts a
// copy: a caller's sample order is its own business.
func Summarise(samples []time.Duration) Stats {
	if len(samples) == 0 {
		return Stats{}
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, sample := range sorted {
		total += sample
	}
	return Stats{
		Count:  len(sorted),
		Min:    sorted[0],
		Median: percentile(sorted, 50),
		P95:    percentile(sorted, 95),
		P99:    percentile(sorted, 99),
		Max:    sorted[len(sorted)-1],
		Mean:   total / time.Duration(len(sorted)),
	}
}

// percentile uses nearest-rank on an ascending slice, which needs no
// interpolation and never invents a value that was not measured.
func percentile(sorted []time.Duration, p int) time.Duration {
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// String renders one row of the ADR's latency table.
func (s Stats) String() string {
	var row strings.Builder
	fmt.Fprintf(&row, "n=%d req=%dB", s.Count, s.RequestBytes)
	for _, column := range []struct {
		label string
		value time.Duration
	}{
		{"min", s.Min}, {"median", s.Median}, {"p95", s.P95},
		{"p99", s.P99}, {"max", s.Max}, {"mean", s.Mean},
	} {
		fmt.Fprintf(&row, " %s=%s", column.label, column.value.Round(time.Microsecond))
	}
	return row.String()
}

// Run performs rounds round trips over client and summarises them.
//
// warmup exchanges are performed and discarded first, so that first-touch page
// faults and connection setup do not land in the reported percentiles.
func Run(ctx context.Context, client *transport.Client, rounds, universe, warmup int, now Clock) (Stats, error) {
	sequence := uint64(1)
	for range warmup {
		if _, err := client.Decide(ctx, NewBarEnvelope(sequence, universe, now)); err != nil {
			return Stats{}, fmt.Errorf("spike: warmup round trip: %w", err)
		}
		sequence++
	}

	samples := make([]time.Duration, 0, rounds)
	requestBytes := 0
	for range rounds {
		bar := NewBarEnvelope(sequence, universe, now)
		if requestBytes == 0 {
			encoded, err := json.Marshal(bar)
			if err != nil {
				return Stats{}, fmt.Errorf("spike: measure request size: %w", err)
			}
			requestBytes = len(encoded) + 1
		}
		started := now()
		decision, err := client.Decide(ctx, bar)
		elapsed := now().Sub(started)
		if err != nil {
			return Stats{}, fmt.Errorf("spike: round trip %d: %w", sequence, err)
		}
		if decision.CausationID != bar.ID {
			return Stats{}, fmt.Errorf("spike: round trip %d answered by %q", sequence, decision.CausationID)
		}
		samples = append(samples, elapsed)
		sequence++
	}

	stats := Summarise(samples)
	stats.RequestBytes = requestBytes
	return stats, nil
}

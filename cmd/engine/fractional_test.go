package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// TestFractionalEngineMonth replays the existing synthetic momentum month
// under ADR 0026 D7-F, including the engine's synthetic terminal reports.
func TestFractionalEngineMonth(t *testing.T) {
	cfg := boostConfiguration(t, momentumConfigPath)
	cfg.Execution.ShareQuantity = "fractional"
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fractional.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	asOf, _ := time.Parse(time.RFC3339, momentumAsOf)
	comp, err := compose(path, "test-build", asOf)
	if err != nil {
		t.Fatal(err)
	}
	inputs := []event.Envelope{comp.configEnvelope}
	sequence := configurationSequence + 1
	for _, in := range readMonth(t, momentumMonthPath) {
		if in.Type == event.AccountSnapshotEventType {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(in.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			payload["quantity_scale"] = json.RawMessage(`10000`)
			in.Payload, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			in.SchemaVersion = 6
		}
		inputs = append(inputs, event.Envelope{ID: in.ID, Type: in.Type, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: in.SchemaVersion,
			EventTime: in.EventTime, RecordedAt: in.EventTime, Sequence: sequence, Source: "test-adapter",
			StrategyVersion: comp.strategyVersion, ConfigurationHash: comp.configurationHash, PayloadHash: event.HashPayload(in.Payload), Payload: in.Payload})
		sequence++
	}
	run := func(inputs []event.Envelope) []event.Envelope {
		c, err := compose(path, "test-build", asOf)
		if err != nil {
			t.Fatal(err)
		}
		engine, err := replay.New(c.handler)
		if err != nil {
			t.Fatal(err)
		}
		out, err := engine.Run(context.Background(), inputs)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first, second := run(inputs), run(inputs)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if !bytes.Equal(a, b) {
		t.Fatal("fractional month replay differs byte for byte")
	}
	legacy := append([]event.Envelope(nil), inputs...)
	for i := range legacy {
		if legacy[i].Type != event.AccountSnapshotEventType {
			continue
		}
		var p map[string]json.RawMessage
		if err := json.Unmarshal(legacy[i].Payload, &p); err != nil {
			t.Fatal(err)
		}
		delete(p, "quantity_scale")
		legacy[i].Payload, err = json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		legacy[i].PayloadHash = event.HashPayload(legacy[i].Payload)
		legacy[i].SchemaVersion = event.AccountSnapshotCashVersion
	}
	legacyBytes, err := json.Marshal(run(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, legacyBytes) {
		t.Fatal("upcast legacy snapshots change fractional decisions")
	}

	var golden bytes.Buffer
	releases, cancels := 0, 0
	for _, e := range first {
		if e.Type != event.OrderReleasedEventType && e.Type != event.OrderLifecycleEventType {
			continue
		}
		var p struct {
			InstrumentID  string `json:"instrument_id"`
			Quantity      int64  `json:"quantity"`
			QuantityScale int64  `json:"quantity_scale"`
			Synthetic     bool   `json:"synthetic"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.QuantityScale != 10000 || p.Quantity <= 0 {
			t.Fatalf("missing fixed-point quantity: %s", e.Payload)
		}
		if e.Type == event.OrderReleasedEventType {
			releases++
		} else {
			cancels++
			if !p.Synthetic {
				t.Fatal("expected synthetic cancel")
			}
		}
		line, _ := json.Marshal(struct {
			Type       string `json:"type"`
			Version    uint32 `json:"version"`
			Instrument string `json:"instrument"`
			Ticks      int64  `json:"ticks"`
		}{e.Type, e.SchemaVersion, p.InstrumentID, p.Quantity})
		golden.Write(line)
		golden.WriteByte('\n')
	}
	if releases == 0 || cancels != releases {
		t.Fatalf("released %d, canceled %d", releases, cancels)
	}
	goldenPath := "testdata/momentum-fractional.golden.jsonl"
	if os.Getenv("TI_WRITE_FRACTIONAL_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, golden.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, golden.Bytes()) {
		t.Fatalf("fractional quantity golden differs:\n%s", golden.Bytes())
	}
}

// TestFractionalEngineKeepsAnExpensivePick uses the same synthetic prices,
// with AAA's raw price scaled up, to distinguish D7-F from D7-L (ADR 0026).
func TestFractionalEngineKeepsAnExpensivePick(t *testing.T) {
	cfg := boostConfiguration(t, momentumConfigPath)
	cfg.Execution.ShareQuantity = event.ShareQuantityFractional
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fractional.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	asOf, _ := time.Parse(time.RFC3339, momentumAsOf)
	comp, err := compose(path, "test-build", asOf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := comp.handler.Apply(context.Background(), comp.configEnvelope); err != nil {
		t.Fatal(err)
	}
	sequence := configurationSequence + 1
	var outputs []event.Envelope
	for _, in := range momentumMonth(t) {
		switch in.Type {
		case event.CompletedBarEventType:
			var p event.CompletedBarAdjustedPayload
			if err := json.Unmarshal(in.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.InstrumentID == "AAA" {
				p.Raw.Open *= 200
				p.Raw.High *= 200
				p.Raw.Low *= 200
				p.Raw.Close *= 200
			}
			in.Payload, err = json.Marshal(p)
		case event.MarkRecordedEventType:
			var p event.MarkRecordedPayload
			if err := json.Unmarshal(in.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.InstrumentID == "AAA" {
				p.Price *= 200
			}
			in.Payload, err = json.Marshal(p)
		case event.UniverseSnapshotEventType:
			var p event.UniverseSnapshotPayload
			if err := json.Unmarshal(in.Payload, &p); err != nil {
				t.Fatal(err)
			}
			for i := range p.Rows {
				if p.Rows[i].InstrumentID == "AAA" {
					p.Rows[i].Close *= 200
				}
			}
			in.Payload, err = json.Marshal(p)
		}
		if err != nil {
			t.Fatal(err)
		}
		e := event.Envelope{ID: in.ID, Type: in.Type, SchemaVersion: in.SchemaVersion, EnvelopeVersion: event.CurrentEnvelopeVersion,
			EventTime: in.EventTime, RecordedAt: in.EventTime, Sequence: sequence, Source: "test-adapter", StrategyVersion: comp.strategyVersion, ConfigurationHash: comp.configurationHash, Payload: in.Payload, PayloadHash: event.HashPayload(in.Payload)}
		out, err := comp.handler.Apply(context.Background(), e)
		if err != nil {
			t.Fatalf("%s: %v", in.ID, err)
		}
		outputs = append(outputs, out...)
		sequence += 1 + uint64(len(out))
	}
	found := false
	for _, e := range outputs {
		switch e.Type {
		case event.TargetsDecidedEventType:
			var p event.TargetsDecidedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Momentum.MinimumLot != (event.MinimumLotFigures{}) {
				t.Fatal("D7-F applied minimum-lot substitution")
			}
			for _, n := range p.Momentum.Names {
				if n.InstrumentID == "AAA" && n.Status != event.MomentumStatusPicked {
					t.Fatalf("expensive pick: %+v", n)
				}
			}
		case event.OrderReleasedEventType:
			var p event.OrderReleasedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.InstrumentID == "AAA" {
				found = true
				if p.Quantity <= 0 || p.Quantity >= 10000 || p.QuantityScale != 10000 {
					t.Fatalf("want a fractional share of AAA: %+v", p)
				}
			}
		}
	}
	if !found {
		t.Fatal("D7-F did not release expensive AAA")
	}
}

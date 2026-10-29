package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// missingMarkMonth expands the committed synthetic month to 25 equally
// weighted picks, omitting the first pick's final mark (ADR 0026, amendment
// of 2026-10-29). Prices remain synthetic fixture data, not a strategy golden.
func missingMarkMonth(t *testing.T) []fixtureInput {
	t.Helper()
	var out []fixtureInput
	names := make([]string, 25)
	for i := range names {
		names[i] = fmt.Sprintf("A%02d", i)
	}
	for _, in := range readMonth(t, momentumMonthPath) {
		out = append(out, in)
		add := func(id string, p any) {
			cp := in
			cp.ID += ":" + id
			cp.Payload, _ = json.Marshal(p)
			out = append(out, cp)
		}
		switch in.Type {
		case event.CompletedBarEventType:
			var p event.CompletedBarAdjustedPayload
			_ = json.Unmarshal(in.Payload, &p)
			if p.InstrumentID == "AAA" {
				for _, id := range names {
					p.InstrumentID = id
					add(id, p)
				}
			}
		case event.MarkRecordedEventType:
			var p event.MarkRecordedPayload
			_ = json.Unmarshal(in.Payload, &p)
			if p.InstrumentID == "AAA" {
				for _, id := range names {
					if id == names[0] && p.AsOf.Format("2006-01-02") == "2025-01-31" {
						failure := in
						failure.Type, failure.SchemaVersion, failure.ID = event.MarkUnavailableEventType, event.MarkUnavailableSchemaVersion, "unavailable:"+id
						failure.Payload, _ = json.Marshal(event.MarkUnavailablePayload{InstrumentID: id, AsOf: p.AsOf, Source: event.AdapterIBKR,
							Failure: event.MarkFailureTransient, Detail: "IBKR's answer to its contract request did not end within 5s, asked twice"})
						out = append(out, failure)
						continue
					}
					p.InstrumentID = id
					add(id, p)
				}
			}
		case event.UniverseSnapshotEventType:
			var p event.UniverseSnapshotPayload
			_ = json.Unmarshal(in.Payload, &p)
			template := p.Rows[0]
			p.Rows = nil
			for _, id := range names {
				row := template
				row.InstrumentID, row.Sector = id, id
				p.Rows = append(p.Rows, row)
			}
			out[len(out)-1].Payload, _ = json.Marshal(p)
		case event.SessionClosedEventType:
			var p event.SessionClosedPayload
			_ = json.Unmarshal(in.Payload, &p)
			p.InstrumentIDs = append(p.InstrumentIDs, names...)
			slices.Sort(p.InstrumentIDs)
			out[len(out)-1].Payload, _ = json.Marshal(p)
		}
	}
	return out
}

func TestMomentumMonthOneMissingMarkReleases24Buys(t *testing.T) {
	cfg := boostConfiguration(t, momentumConfigPath)
	cfg.Momentum.UniverseSize, cfg.Momentum.TopN = 25, 25
	cfg.Limits.MaxOrdersPerDecision = 30
	at, _ := time.Parse(time.RFC3339, momentumAsOf)
	version := event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, "test-build")
	h, err := targetweight.New(version, cfg, targetweight.Options{DecideFrom: at})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cfg)
	fixture := append([]fixtureInput{{Type: event.TargetWeightConfigurationEventType, SchemaVersion: event.TargetWeightConfigurationSchemaVersion, ID: "configuration", EventTime: at, Payload: raw}}, missingMarkMonth(t)...)
	var inputs []event.Envelope
	for i, in := range fixture {
		inputs = append(inputs, event.Envelope{EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: in.SchemaVersion,
			ID: in.ID, Type: in.Type, EventTime: in.EventTime, RecordedAt: in.EventTime, Sequence: uint64(i + 1),
			Source: "fixture", StrategyVersion: version, ConfigurationHash: h.ConfigurationHash(), Payload: in.Payload, PayloadHash: event.HashPayload(in.Payload)})
	}
	engine, err := replay.New(h)
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	buys, refused, states, withheld := 0, 0, 0, 0
	for _, in := range decisions {
		switch in.Type {
		case event.OrderReleasedEventType:
			var p event.OrderReleasedPayload
			_ = json.Unmarshal(in.Payload, &p)
			if p.Side != event.OrderSideBuy || p.InstrumentID == "A00" {
				t.Fatalf("unexpected release: %+v", p)
			}
			buys++
		case event.OrderRefusedEventType:
			var p event.OrderRefusedPayload
			_ = json.Unmarshal(in.Payload, &p)
			if p.Reason != event.RefusedMissingMark || p.InstrumentID != "A00" {
				t.Fatalf("unexpected refusal: %+v", p)
			}
			refused++
		case event.EngineStateEventType:
			states++
		case event.TargetsDecidedEventType:
			var p event.TargetsDecidedPayload
			_ = json.Unmarshal(in.Payload, &p)
			withheld += len(p.WithheldPicks)
			if in.SchemaVersion != event.TargetsDecidedSchemaVersion {
				t.Fatalf("decision schema: %d", in.SchemaVersion)
			}
			if len(p.WithheldPicks) == 1 && (p.WithheldPicks[0].InstrumentID != "A00" || p.WithheldPicks[0].Reason != "missing-mark" ||
				p.WithheldPicks[0].Failure != event.MarkFailureTransient || p.WithheldPicks[0].Detail != "IBKR's answer to its contract request did not end within 5s, asked twice") {
				t.Fatalf("wrong pick: %+v", p)
			}
		}
	}
	if buys != 24 || refused != 1 || withheld != 1 || states != 0 {
		t.Fatalf("buys=%d refused=%d withheld=%d states=%d; want 24/1/1/0", buys, refused, withheld, states)
	}
}

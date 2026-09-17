package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

func TestDecisionTextPreservesAdjacentFloatsAndRecordedProvenance(t *testing.T) {
	_, records := readJournalFile(t, goldenJournal)
	for _, r := range records {
		if r.Envelope.Type != event.SignalEventType {
			continue
		}
		for _, high := range []float64{118.2875, math.Nextafter(118.2875, math.Inf(1))} {
			var p event.SignalPayload
			if err := json.Unmarshal(r.Envelope.Payload, &p); err != nil {
				t.Fatal(err)
			}
			p.EntryChannelHigh = 100
			p.BreakoutHigh = high
			p.Rule = "recorded.custom.rule"
			p.ADR = "9876"
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			e := r.Envelope
			e.Payload = raw
			e.PayloadHash = event.HashPayload(raw)
			line, _, err := decisionLine(e)
			if err != nil {
				t.Fatal(err)
			}
			want := strconv.FormatFloat(high, 'g', -1, 64)
			if !strings.Contains(line, "high "+want+" exceeded") || !strings.Contains(line, "rule recorded.custom.rule; ADR 9876") {
				t.Fatal(line)
			}
			part := strings.Split(strings.Split(line, "high ")[1], " ")[0]
			got, err := strconv.ParseFloat(part, 64)
			if err != nil || math.Float64bits(got) != math.Float64bits(high) {
				t.Fatalf("%s did not round-trip", line)
			}
		}
		return
	}
	t.Fatal("missing Signal")
}

func TestDecisionTextDeclineReasons(t *testing.T) {
	// Cash figures transcribed from strategy.TestUnaffordableUnitIsDeclinedWithBothCashFigures.
	for _, reason := range []string{event.DeclineReasonInsufficientCash, event.DeclineReasonUnitCostNotRepresentable, event.DeclineReasonQuantityBelowOneUnit} {
		p := event.ProposalDeclinedPayload{
			InstrumentID: "AAPL", PeriodEnd: time.Date(2026, 1, 22, 0, 0, 0, 0, time.UTC),
			Kind: event.ProposalDeclinedKindEntry, SignalID: "signal:AAPL", Reason: reason, Detail: "recorded decline detail",
		}
		if reason == event.DeclineReasonInsufficientCash {
			p.RequiredCash = 20615
			p.AvailableCash = 20614.99
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		_, records := readJournalFile(t, goldenJournal)
		e := records[0].Envelope
		e.Type = event.ProposalDeclinedEventType
		e.SchemaVersion = event.ProposalDeclinedSchemaVersion
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		e.Payload = raw
		e.PayloadHash = event.HashPayload(raw)
		sentence, err := decisionSentence(e)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sentence, "declined entry proposal because "+reason) || !strings.Contains(sentence, p.Detail) {
			t.Fatal(sentence)
		}
		if reason == event.DeclineReasonInsufficientCash && !strings.Contains(sentence, "required cash 20615 exceeded available cash 20614.99") {
			t.Fatal(sentence)
		}
		if reason != event.DeclineReasonInsufficientCash && strings.Contains(sentence, "cash") {
			t.Fatal(sentence)
		}
	}
}

func TestDecisionTextRefusesUnknownOrInvalidEvidence(t *testing.T) {
	_, records := readJournalFile(t, goldenJournal)
	var base event.Envelope
	for _, r := range records {
		if r.Envelope.Type == event.SignalEventType {
			base = r.Envelope
			break
		}
	}
	for _, alter := range []func(*event.Envelope){
		func(e *event.Envelope) { e.Type = "future.decision" },
		func(e *event.Envelope) { e.SchemaVersion++ },
		func(e *event.Envelope) {
			e.Payload = []byte(`{"n":"not a number"}`)
			e.PayloadHash = event.HashPayload(e.Payload)
		},
		func(e *event.Envelope) { e.Payload = []byte(`{}`); e.PayloadHash = event.HashPayload(e.Payload) },
		func(e *event.Envelope) { e.PayloadHash = "incorrect" },
	} {
		e := base
		alter(&e)
		if _, _, err := decisionLine(e); err == nil {
			t.Fatalf("accepted %+v", e)
		}
	}
}

func TestDecisionLogValidatesBeforeFilteringAndWriting(t *testing.T) {
	path := rewriteJournal(t, signalJournal(t), func(_ *journal.Header, entries []journal.Entry) []journal.Entry {
		for i := range entries {
			if entries[i].Kind == journal.KindDecision {
				entries[i].Envelope.SchemaVersion++
			}
		}
		return entries
	})
	var out bytes.Buffer
	err := run(context.Background(), []string{"-decisions", path, "-instrument", "MSFT"}, &out)
	if err == nil || !strings.Contains(err.Error(), "schema") || out.Len() != 0 {
		t.Fatalf("%v %s", err, &out)
	}
}

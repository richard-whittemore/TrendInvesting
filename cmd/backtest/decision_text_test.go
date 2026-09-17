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

func TestDecisionLogFullFixtureAndMissingProvenance(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-decisions", goldenJournal}, &out); err != nil {
		t.Fatal(err)
	}
	_, records := readJournalFile(t, goldenJournal)
	count := 0
	for _, r := range records {
		if r.Kind == journal.KindDecision {
			count++
		}
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != count {
		t.Fatalf("%d lines for %d decisions", len(lines), count)
	}
	for _, line := range lines {
		if !strings.Contains(line, "rule ") || !strings.Contains(line, "ADR ") {
			t.Fatal(line)
		}
	}
	if !strings.Contains(out.String(), "rule not recorded; ADR not recorded") {
		t.Fatal("missing provenance was concealed")
	}
	if !strings.Contains(out.String(), "because input-stream-ended; no fill was recorded for this proposal") {
		t.Fatal("expiry reason missing")
	}
}

func TestDecisionLogMixedGolden(t *testing.T) {
	path := rewriteJournal(t, goldenJournal, func(_ *journal.Header, entries []journal.Entry) []journal.Entry {
		var selected []journal.Entry
		for _, e := range entries {
			if e.Kind == journal.KindInput || e.Envelope.Type == event.SignalEventType || e.Envelope.Type == event.ProposalExpiredEventType {
				selected = append(selected, e)
			}
			if e.Envelope.Type == event.SignalEventType {
				p := event.ProposalDeclinedPayload{InstrumentID: "MSFT", PeriodEnd: e.Envelope.EventTime, Kind: event.ProposalDeclinedKindEntry, SignalID: "signal:MSFT", Reason: event.DeclineReasonInsufficientCash, Detail: "one Unit cannot be funded", RequiredCash: 20615, AvailableCash: 20614.99}
				raw, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				e.Envelope.Type = event.ProposalDeclinedEventType
				e.Envelope.SchemaVersion = event.ProposalDeclinedSchemaVersion
				e.Envelope.ID = "decline:MSFT"
				e.Envelope.Sequence = 23
				e.Envelope.Payload = raw
				e.Envelope.PayloadHash = event.HashPayload(raw)
				selected = append(selected, e)
			}
		}
		return selected
	})
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-decisions", path}, &out); err != nil {
		t.Fatal(err)
	}
	const want = `2026-01-22T00:00:00Z [decision 22] AAPL: Signal long because high 129.01 exceeded Entry Channel 127.01; N was 1 (rule entry.channel.breakout; ADR 0002).
2026-01-22T00:00:00Z [decision 23] MSFT: declined entry proposal because insufficient-cash: one Unit cannot be funded; Signal "signal:MSFT"; required cash 20615 exceeded available cash 20614.99 (rule not recorded; ADR not recorded).
2026-02-02T00:00:00Z [decision 58] AAPL: expired exit proposal "exit-proposal:AAPL:2026-02-02T00:00:00.000000000Z" for 20000 shares at 128.8 because input-stream-ended; no fill was recorded for this proposal (rule exit-proposal.expires.with-its-bar; ADR 0011).
`
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", &out, want)
	}
	out.Reset()
	if err := run(context.Background(), []string{"-decisions", path, "-date", "2026-01-22", "-instrument", "MSFT"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != strings.Split(want, "\n")[1]+"\n" {
		t.Fatal(&out)
	}
}

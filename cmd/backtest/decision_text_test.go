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
	"unicode"

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
}

// TestDecisionLogExplainsAProposalTheRunEndedHolding renders the end-of-stream
// expiry of a proposal no fill ever answered.
func TestDecisionLogExplainsAProposalTheRunEndedHolding(t *testing.T) {
	_, path := runBacktestOnBars(t, writeBars(t, barsEndingWithAnOutstandingExit(t)))
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-decisions", path, "-date", "2026-01-23"}, &out); err != nil {
		t.Fatal(err)
	}
	const want = `AAPL: expired exit proposal "exit-proposal:AAPL:2026-01-23T00:00:00.000000000Z" for 20000 shares at 125.21 because input-stream-ended; no fill was recorded for this proposal (rule exit-proposal.expires.with-its-bar; ADR 0011).`
	if !strings.Contains(out.String(), want) {
		t.Fatalf("got:\n%s\nwant a line ending:\n%s", &out, want)
	}
}

func TestDecisionLogMixedGolden(t *testing.T) {
	path := rewriteJournal(t, goldenJournal, func(_ *journal.Header, entries []journal.Entry) []journal.Entry {
		var selected []journal.Entry
		for _, e := range entries {
			if e.Kind == journal.KindInput || e.Envelope.Type == event.SignalEventType {
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

func TestDecisionAccountAndHaltSentences(t *testing.T) {
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	// Account figures follow the event payload fixtures; the first Drawdown Step
	// is Faith's inclusive boundary example (The Turtle Rules p.17).
	tests := []struct {
		kind    string
		version uint32
		payload any
		want    string
	}{
		{event.DrawdownStepAppliedEventType, event.DrawdownStepAppliedSchemaVersion, event.DrawdownStepAppliedPayload{AsOf: at, Equity: 900000, Threshold: 900000, NotionalBefore: 1000000, NotionalAfter: 800000, StepNumber: 1, Rule: event.RuleNotionalAccountDrawdownStep, ADR: event.ADRNotionalAccountDrawdownStep}, "applied Drawdown Step 1, reducing Notional Account from 1e+06 to 800000 because actual equity 900000 was at or below threshold 900000"},
		{event.NotionalAccountRecoveredEventType, event.NotionalAccountRecoveredSchemaVersion, event.NotionalAccountRecoveredPayload{AsOf: at, Equity: 1000000, StartingFigure: 1000000, NotionalBefore: 640000, StepsCleared: 2, Rule: event.RuleNotionalAccountRecovery, ADR: event.ADRNotionalAccountRecovery}, "restored Notional Account from 640000 to 1e+06 and cleared 2 Drawdown Steps because actual equity 1e+06 regained the yearly starting figure"},
		{event.NotionalAccountRebasedEventType, event.NotionalAccountRebasedSchemaVersion, event.NotionalAccountRebasedPayload{AsOf: at, Equity: 950000, PreviousStartingFigure: 1000000, NewStartingFigure: 950000, Rule: event.RuleNotionalAccountRebase, ADR: event.ADRNotionalAccountRebase}, "rebased Notional Account to actual equity 950000 for the yearly re-basing; yearly starting figure changed from 1e+06 to 950000"},
		{event.NotionalAccountCashAdjustedEventType, event.NotionalAccountCashAdjustedSchemaVersion, event.NotionalAccountCashAdjustedPayload{AsOf: at, Amount: 200000, EquityBefore: 890000, EquityAfter: 1090000, StartingFigureBefore: 1000000, StartingFigureAfter: 1224719.1011235956, NotionalBefore: 800000, NotionalAfter: 979775.2808988765, Rule: event.RuleNotionalAccountCashAdjustment, ADR: event.ADRNotionalAccountCashAdjustment}, "adjusted Notional Account from 800000 to 979775.2808988765 because cash movement 200000 changed actual equity from 890000 to 1.09e+06"},
		{event.EngineStateEventType, event.EngineStateSchemaVersion, event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonCampaignWithoutProtectiveStop, Detail: "recorded detail\nsecond line"}, `engine became halted because campaign-without-protective-stop: "recorded detail\nsecond line"`},
	}
	_, records := readJournalFile(t, goldenJournal)
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			e := records[0].Envelope
			e.Type = tc.kind
			e.SchemaVersion = tc.version
			e.Payload = raw
			e.PayloadHash = event.HashPayload(raw)
			got, err := decisionSentence(e)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %s\nwant %s", got, tc.want)
			}
			line, instrument, err := decisionLine(e)
			if err != nil || instrument != "" || !strings.Contains(line, "account: "+tc.want) {
				t.Fatalf("%s %s %v", line, instrument, err)
			}
		})
	}
}

func TestDecisionTextExitConfirmationFollowsTheExitReason(t *testing.T) {
	_, records := readJournalFile(t, goldenJournal)
	var base event.Envelope
	for _, r := range records {
		if r.Envelope.Type == event.CampaignExitedEventType {
			base = r.Envelope
			break
		}
	}
	if base.Type == "" {
		t.Fatal("missing Campaign exit")
	}
	var exited event.CampaignExitedPayload
	if err := json.Unmarshal(base.Payload, &exited); err != nil {
		t.Fatal(err)
	}
	const campaign = `exited Campaign "campaign:AAPL:2026-01-22T00:00:00.000000000Z" because `
	const closed = `; 4 Units and 20000 shares closed at `
	const result = ` 128.75; realised result 17299.999999999898`
	for _, tc := range []struct {
		reason string
		rule   string
		adr    string
		cause  string
		want   string
	}{
		{event.ExitReasonStop, event.RuleCampaignExitedByStop, event.ADRCampaignExitRecordsTheFill, exited.FillID,
			campaign + `stop, confirmed by fill "fill:AAPL:2026-02-02T00:00:00.000000000Z:1"` + closed + `average price` + result},
		{event.ExitReasonExitChannel, event.RuleCampaignExitedByExitChannel, event.ADRCampaignExitRecordsTheFill, exited.FillID,
			campaign + `exit-channel, confirmed by fill "fill:AAPL:2026-02-02T00:00:00.000000000Z:1"` + closed + `average price` + result},
		// ADR 0009 forces a Delisting Exit directly, so its FillID names the
		// corporate-action envelope and there is no execution to reconcile.
		{event.ExitReasonDelisting, event.RuleCampaignExitedByDelisting, event.ADRDelistingForcesExit, "corp-action-42",
			campaign + `delisting, forced by corporate action "corp-action-42"; no fill was recorded for this exit` + closed + `last available price` + result},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			p := exited
			p.Reason, p.Rule, p.ADR, p.FillID = tc.reason, tc.rule, tc.adr, tc.cause
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			e := base
			e.Payload = raw
			e.PayloadHash = event.HashPayload(raw)
			got, err := decisionSentence(e)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
			if tc.reason == event.ExitReasonDelisting && strings.Contains(got, "confirmed by fill") {
				t.Fatalf("a Delisting Exit has no fill to confirm it: %s", got)
			}
		})
	}
}

// Envelope and chain validation do not restrict the Unicode a journal's text
// fields may carry, so a decision line must not be able to lie about its own
// shape: a separator that splits it in two, or an override that reverses the
// order a reader sees it in.
func TestDecisionTextEscapesRunesThatReshapeTheLine(t *testing.T) {
	_, records := readJournalFile(t, goldenJournal)
	for _, tc := range []struct {
		name   string
		detail string
		want   string
	}{
		{"line separator", "halted\u2028 and cleared", "\"halted\\u2028 and cleared\""},
		{"paragraph separator", "halted\u2029 and cleared", "\"halted\\u2029 and cleared\""},
		{"right-to-left override", "halted\u202e and cleared", "\"halted\\u202e and cleared\""},
		{"left-to-right mark", "halted\u200e and cleared", "\"halted\\u200e and cleared\""},
		{"newline", "halted\n and cleared", `"halted\n and cleared"`},
		// Ordinary text stays readable, whatever alphabet it is written in.
		{"printable non-ascii", "clôture à 1 234,50 € — 日経 ±2N", "clôture à 1 234,50 € — 日経 ±2N"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonCampaignWithoutProtectiveStop, Detail: tc.detail}
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			e := records[0].Envelope
			e.Type = event.EngineStateEventType
			e.SchemaVersion = event.EngineStateSchemaVersion
			e.Payload = raw
			e.PayloadHash = event.HashPayload(raw)
			line, _, err := decisionLine(e)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(line, "campaign-without-protective-stop: "+tc.want) {
				t.Fatalf("got %s\nwant detail %s", line, tc.want)
			}
			if strings.ContainsFunc(line, func(r rune) bool { return !unicode.IsGraphic(r) }) {
				t.Fatalf("a non-graphic rune survived into %q", line)
			}
		})
	}
}

// An Exit Order names the Unit, its own quantity, the level it rests at, and
// which of the two restated levels governs it — so the log line alone says
// why the order sits where it does.
func TestDecisionTextExitOrderSentences(t *testing.T) {
	at := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	const campaign = "campaign:AAPL:2026-01-22T00:00:00.000000000Z"
	for _, tc := range []struct {
		name    string
		payload event.ExitOrderSetPayload
		want    string
	}{
		{
			name:    "protective stop governs",
			payload: event.ExitOrderSetPayload{CampaignID: campaign, InstrumentID: "AAPL", UnitIndex: 2, Level: 124.5, Quantity: 5000, Source: event.ExitOrderSourceProtectiveStop, ProtectiveStop: 124.5, AsOf: at, Rule: event.RuleExitOrderHigherOfStopAndExitChannel, ADR: event.ADRExitOrderRestsAtTheLevel},
			want:    `set Exit Order for Unit 2 of Campaign "` + campaign + `" to 5000 shares at 124.5 because protective-stop governs; Protective Stop 124.5, no Exit-Channel exit proposed`,
		},
		{
			name:    "exit channel governs",
			payload: event.ExitOrderSetPayload{CampaignID: campaign, InstrumentID: "AAPL", UnitIndex: 1, Level: 128.8, Quantity: 5000, Source: event.ExitOrderSourceExitChannel, ProtectiveStop: 124.5, ExitChannelLevel: 128.8, AsOf: at, Rule: event.RuleExitOrderHigherOfStopAndExitChannel, ADR: event.ADRExitOrderRestsAtTheLevel},
			want:    `set Exit Order for Unit 1 of Campaign "` + campaign + `" to 5000 shares at 128.8 because exit-channel governs; Protective Stop 124.5, Exit Channel level 128.8`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			_, records := readJournalFile(t, goldenJournal)
			e := records[0].Envelope
			e.Type = event.ExitOrderSetEventType
			e.SchemaVersion = event.ExitOrderSetSchemaVersion
			e.Payload = raw
			e.PayloadHash = event.HashPayload(raw)
			got, err := decisionSentence(e)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

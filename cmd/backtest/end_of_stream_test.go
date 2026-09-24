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
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

const barsMultiInstrumentOutOfOrderFixture = "testdata/bars_multi_instrument_out_of_order.json"

// TestRunCompletedIsStampedWithTheLatestPeriodEndAcrossEveryInstrument
// covers #174: event.RunCompletedEventType's timestamp must be at or after
// every bar the run delivered (its own doc comment: "at the instant the
// stream ended"), so it has to be the LATEST PeriodEnd across every
// instrument the fixture holds bars for — never just the last array
// element, since readBars' own contract promises only "the order the run
// delivers them" (cmd/backtest/backtest.go's doc comment on readBars), not
// global chronology.
//
// The fixture lists AAPL's 32 bars (ending 2026-02-02) before MSFT's two
// bars (ending 2025-12-02): a well-formed multi-instrument fixture whose
// last array element is not the chronologically-latest bar. Stamping
// replay.run.completed with the last element's PeriodEnd would understate
// the true end of the stream by two months, which
// internal/strategy/end_of_stream.go's applyRunCompleted rejects outright:
// it errors if the declared completion instant precedes any instrument's
// own last bar (AAPL's, in this fixture). A run that used the correct
// maximum never trips that check.
func TestRunCompletedIsStampedWithTheLatestPeriodEndAcrossEveryInstrument(t *testing.T) {
	out := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := options{
		configPath: configurationFixture,
		barsPath:   barsMultiInstrumentOutOfOrderFixture,
		outPath:    out,
		build:      testBuild,
	}

	var log bytes.Buffer
	if err := backtest(context.Background(), opts, &log); err != nil {
		t.Fatalf("backtest(%+v) error = %v\n%s", opts, err, log.String())
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}

	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}

	// AAPL's last bar (2026-02-02) is the true latest PeriodEnd across both
	// instruments; MSFT's last bar (2025-12-02), the fixture's last array
	// element, is not.
	wantCompletedAt := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)

	var sawRunCompleted bool
	for _, record := range records {
		if record.Kind != journal.KindInput || record.Envelope.Type != event.RunCompletedEventType {
			continue
		}
		sawRunCompleted = true
		if !record.Envelope.EventTime.Equal(wantCompletedAt) {
			t.Fatalf("%s stamped at %s, want the latest period end across every instrument (%s)",
				event.RunCompletedEventType, record.Envelope.EventTime.Format(time.RFC3339), wantCompletedAt.Format(time.RFC3339))
		}
	}
	if !sawRunCompleted {
		t.Fatalf("the journal records no %s event", event.RunCompletedEventType)
	}

	// AAPL's outstanding breakout proposal (same fixture as the golden
	// journal's) must still reach its end-of-stream terminal event: the fix
	// must not merely avoid the error, it must let the run actually
	// complete and resolve what it was holding (#174's acceptance
	// criteria).
	var expired []event.ProposalExpiredPayload
	for _, record := range records {
		if record.Kind != journal.KindDecision || record.Envelope.Type != event.ProposalExpiredEventType {
			continue
		}
		var payload event.ProposalExpiredPayload
		if err := json.Unmarshal(record.Envelope.Payload, &payload); err != nil {
			t.Fatalf("decode proposal expired payload: %v", err)
		}
		expired = append(expired, payload)
	}
	if len(expired) != 1 {
		t.Fatalf("got %d %s decisions, want 1 (AAPL's outstanding breakout proposal)", len(expired), event.ProposalExpiredEventType)
	}
	if expired[0].InstrumentID != "AAPL" {
		t.Fatalf("the expired proposal names instrument %q, want %q", expired[0].InstrumentID, "AAPL")
	}
	if expired[0].Reason != event.ExpiryReasonInputStreamEnded {
		t.Fatalf("expiry reason = %q, want %q", expired[0].Reason, event.ExpiryReasonInputStreamEnded)
	}
	if !expired[0].ExpiredAt.Equal(wantCompletedAt) {
		t.Fatalf("expiry ExpiredAt = %s, want %s", expired[0].ExpiredAt.Format(time.RFC3339), wantCompletedAt.Format(time.RFC3339))
	}
}

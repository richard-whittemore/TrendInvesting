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
)

// With enough cash, the golden fixture's Campaign resolves every proposal it
// raises: its entry and all three Adds fill on the breakout bar, and the exit
// its last bar proposes rests above every Unit's stop, so each Unit's Exit
// Order moves up to it and it fills in that same bar. A run that ends while a
// proposal is still outstanding needs a fixture of its own, and these
// helpers derive it from the golden bars rather than inventing a new series.

// fourUnitCash is opening cash that funds the fixture's whole Add Ladder.
// Each of its Units costs about 64 % of the 1,000,000 starting equity, so
// the default run, whose cash is that equity, takes Unit 1 and declines
// Unit 2 for insufficient cash (ADR 0020). A test whose subject needs all
// four Units states this figure rather than relying on cash the account
// does not hold; four Units cost about 2,558,000 with commissions.
const fourUnitCash = 3_000_000.0

// withFourUnitCash returns opts with fourUnitCash as its opening cash.
func withFourUnitCash(opts options) options {
	cash := fourUnitCash
	opts.availableCash = &cash
	return opts
}

// outstandingExitBar is the bar after the golden fixture's breakout.
var outstandingExitBar = time.Date(2026, 1, 23, 0, 0, 0, 0, time.UTC)

// barsEndingWithAnOutstandingExit is the golden bar fixture cut the day
// after its breakout, with that day's low lowered to 125.0. The bar breaks
// the 125.21 Exit Channel, so the reducer proposes an exit there; but the
// four Units' stops (126.56-126.71) are above it, so every Unit's Exit
// Order stays at its own stop (ADR 0005, as amended), the stops close the
// Campaign, and the exit proposal is left outstanding when the input stream
// ends.
func barsEndingWithAnOutstandingExit(t *testing.T) []event.CompletedBarPayload {
	t.Helper()
	all, err := readBars(barsFixture)
	if err != nil {
		t.Fatal(err)
	}
	var bars []event.CompletedBarPayload
	for _, bar := range all {
		if bar.PeriodEnd.After(outstandingExitBar) {
			break
		}
		if bar.PeriodEnd.Equal(outstandingExitBar) {
			for _, view := range []*event.PriceView{&bar.SplitAdjusted, &bar.Raw} {
				view.Low, view.Close = 125.0, 125.5
			}
		}
		bars = append(bars, bar)
	}
	if len(bars) == 0 || !bars[len(bars)-1].PeriodEnd.Equal(outstandingExitBar) {
		t.Fatalf("the golden bar fixture no longer contains its %s bar", outstandingExitBar.Format(time.DateOnly))
	}
	return bars
}

// writeBars writes bars as a bar fixture and returns its path.
func writeBars(t *testing.T, bars []event.CompletedBarPayload) string {
	t.Helper()
	raw, err := json.Marshal(bars)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bars.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runBacktestOnBars runs the fixture configuration over the bars at
// barsPath, with fourUnitCash, and returns the journal written, with its
// path.
func runBacktestOnBars(t *testing.T, barsPath string) (written []byte, path string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := withFourUnitCash(options{configPath: configurationFixture, barsPath: barsPath, outPath: out, build: testBuild})
	var log bytes.Buffer
	if err := backtest(context.Background(), opts, &log); err != nil {
		t.Fatalf("backtest(%+v) error = %v\n%s", opts, err, log.String())
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the journal the command wrote: %v", err)
	}
	return written, out
}

// TestAnExitBelowEveryStopLeavesTheStopsToCloseTheCampaign is the fixture's
// own premise, pinned: the stops sell every Unit at its own level, and the
// exit proposal is what the run ends holding.
func TestAnExitBelowEveryStopLeavesTheStopsToCloseTheCampaign(t *testing.T) {
	_, path := runBacktestOnBars(t, writeBars(t, barsEndingWithAnOutstandingExit(t)))
	_, records := readJournalFile(t, path)
	var stops, exits int
	for _, record := range records {
		if record.Envelope.Type != event.FillEventType || !record.Envelope.EventTime.Equal(outstandingExitBar) {
			continue
		}
		var fill event.FillPayload
		if err := json.Unmarshal(record.Envelope.Payload, &fill); err != nil {
			t.Fatal(err)
		}
		switch fill.Kind {
		case event.FillKindStop:
			stops++
		case event.FillKindExit:
			exits++
		}
	}
	if stops != 4 || exits != 0 {
		t.Fatalf("the bar filled %d stop(s) and %d exit(s), want the four Units' own stops and no exit", stops, exits)
	}
}

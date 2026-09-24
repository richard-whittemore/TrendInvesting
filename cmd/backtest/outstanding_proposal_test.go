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

// When its account funds them, the golden fixture's Campaign resolves every
// proposal it raises: its entry and all three Adds fill on the breakout bar,
// and the exit its last bar proposes rests above every Unit's stop, so each
// Unit's Exit Order moves up to it and it fills in that same bar. A run that
// ends while a proposal is still outstanding needs a fixture of its own, and
// these helpers derive it from the golden bars rather than inventing a new
// series.

// fourUnitCash is an account that funds the golden bars' whole Add Ladder:
// four Units cost about 2,558,000 with commissions, and each costs about
// 64 % of the 1,000,000 Notional Account the configuration sizes from, so a
// 1,000,000 account takes Unit 1 and declines Unit 2 (ADR 0020). This is a
// 3,000,000 account, all cash, trading a 1,000,000 Notional Account. That
// holds only while no snapshot crosses a re-basing date, which re-bases the
// Notional Account to the account's equity (ADR 0007); the golden bars all
// fall in January 2026. The fused-multiply-add guards need these bars at
// these prices (fusion_test.go), which is why they are run this way rather
// than lowered as fourUnitBarsFixture is.
const fourUnitCash = 3_000_000.0

// fourUnitBarsFixture is the golden bars with every price 90.00 lower, and
// nothing else changed. Lowering every price by one amount leaves every
// range, channel, N, rung and stop distance, and so every decision, as it
// was, while a Unit costs about 19 % of the Notional Account rather than 64
// %: an account holding the 1,000,000 starting equity in cash funds all four
// Units. TestTheFourUnitBarsAreTheGoldenBarsLowered pins the derivation.
const fourUnitBarsFixture = "testdata/bars_four_units.json"

// fourUnitShift is how far fourUnitBarsFixture lowers the golden bars.
const fourUnitShift = 90.0

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
	return barsCutWithAnOutstandingExit(t, barsFixture, 0)
}

// barsCutWithAnOutstandingExit is barsEndingWithAnOutstandingExit over the
// golden bars lowered by shift, at path.
func barsCutWithAnOutstandingExit(t *testing.T, path string, shift float64) []event.CompletedBarPayload {
	t.Helper()
	all, err := readBars(path)
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
				view.Low, view.Close = 125.0-shift, 125.5-shift
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

// TestTheFourUnitBarsAreTheGoldenBarsLowered pins fourUnitBarsFixture's
// derivation: the golden bars, every price fourUnitShift lower, to the cent,
// and every other field unchanged.
func TestTheFourUnitBarsAreTheGoldenBarsLowered(t *testing.T) {
	golden, err := readBars(barsFixture)
	if err != nil {
		t.Fatal(err)
	}
	lowered, err := readBars(fourUnitBarsFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(lowered) != len(golden) {
		t.Fatalf("%d lowered bars for %d golden ones", len(lowered), len(golden))
	}
	for i, bar := range golden {
		want := bar
		for _, view := range []*event.PriceView{&want.SplitAdjusted, &want.Raw} {
			view.Open, view.High, view.Low, view.Close = view.Open-fourUnitShift, view.High-fourUnitShift, view.Low-fourUnitShift, view.Close-fourUnitShift
		}
		got := lowered[i]
		for _, pair := range [][2]event.PriceView{{got.SplitAdjusted, want.SplitAdjusted}, {got.Raw, want.Raw}} {
			g, w := pair[0], pair[1]
			for _, diff := range []float64{g.Open - w.Open, g.High - w.High, g.Low - w.Low, g.Close - w.Close} {
				if diff > 0.001 || diff < -0.001 {
					t.Fatalf("bar %d: %+v, want the golden bar lowered by %v: %+v", i+1, got, fourUnitShift, want)
				}
			}
			g.Open, g.High, g.Low, g.Close = w.Open, w.High, w.Low, w.Close
			if g != w {
				t.Fatalf("bar %d: %+v differs from the golden bar in more than its prices", i+1, got)
			}
		}
		if got.InstrumentID != bar.InstrumentID || !got.PeriodEnd.Equal(bar.PeriodEnd) {
			t.Fatalf("bar %d is %s at %s, want %s at %s", i+1, got.InstrumentID, got.PeriodEnd, bar.InstrumentID, bar.PeriodEnd)
		}
	}
}

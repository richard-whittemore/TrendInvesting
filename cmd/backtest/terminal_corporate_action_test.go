package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the command-seam tests for a cash-crediting corporate
// action whose EffectiveAt falls at or after the instrument's own last bar
// in the fixture (ADR 0024's review finding): fills.StateLastClose has
// already taken the run's one and only closing account.snapshot by the time
// deliverRemainingActions delivers such an action, so a dividend or a
// split's cash in lieu delivered there would credit the simulated account's
// ledger with no statement ever recording it. The fix refuses the whole run
// before delivering any such action (backtest.go's refuseIfCashCrediting) —
// the conservative choice, over inventing a second, artificial closing
// statement.

// barsThroughEntry is the golden bar fixture cut at outstandingExitBar
// (outstanding_proposal_test.go), UNMODIFIED: unlike
// barsEndingWithAnOutstandingExit, this does not lower that day's low, so no
// stop or exit fires and the Campaign the golden fixture opens is still open
// when the cut fixture ends. Its last bar is outstandingExitBar itself, so an
// action effective strictly after it is a REMAINING action (deliverRemainingActions),
// never one deliverActionsDueFor catches mid-run.
func barsThroughEntry(t *testing.T) []event.CompletedBarPayload {
	t.Helper()
	all, err := readBars(barsFixture)
	if err != nil {
		t.Fatal(err)
	}
	var bars []event.CompletedBarPayload
	for _, bar := range all {
		bars = append(bars, bar)
		if bar.PeriodEnd.Equal(outstandingExitBar) {
			return bars
		}
	}
	t.Fatalf("the golden bar fixture no longer contains its %s bar", outstandingExitBar.Format(time.DateOnly))
	return nil
}

// runTerminalBacktest runs the golden bars cut at outstandingExitBar, with
// actionsPath as the corporate-actions fixture, and returns the error
// backtest() produced, if any.
func runTerminalBacktest(t *testing.T, actionsPath string) error {
	t.Helper()
	opts := options{
		configPath:           configurationFixture,
		barsPath:             writeBars(t, barsThroughEntry(t)),
		corporateActionsPath: actionsPath,
		outPath:              filepath.Join(t.TempDir(), "journal.jsonl"),
		build:                testBuild,
	}
	var log bytes.Buffer
	return backtest(context.Background(), opts, &log)
}

// TestATerminalDividendIsRefused is the review finding's pinned regression:
// a dividend effective after the fixture's last bar for its instrument would
// otherwise credit the simulated account's cash with no account.snapshot
// ever stating it, since fills.StateLastClose has already taken the run's
// only closing statement by the time deliverRemainingActions runs. The run
// refuses instead of silently losing the credit from the journal's own
// evidence.
func TestATerminalDividendIsRefused(t *testing.T) {
	t.Parallel()

	dividendAt := outstandingExitBar.Add(12 * time.Hour)
	err := runTerminalBacktest(t, writeActions(t, []event.CorporateActionPayload{
		{
			InstrumentID: "AAPL",
			Kind:         event.CorporateActionKindDividend,
			EffectiveAt:  dividendAt,
			CashAmount:   500.0,
			Currency:     "USD",
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "could never reach any statement") {
		t.Fatalf("backtest() error = %v, want a refusal naming the terminal dividend's unreachable statement (ADR 0024)", err)
	}
}

// TestATerminalCashInLieuIsRefused is ADR 0024's review finding, checked
// against ADR 0023's own cash in lieu: the identical gap exists there too,
// since a split's cash reaches the simulated account's ledger the same way a
// dividend's does (internal/fills.observeCashInLieu), and the same fix
// (refuseIfCashCrediting) closes it for both at once. This split states no
// shortfall (RawSharesLost 0), so it changes no Unit's quantity — the
// corporate action's own doc comment: "a split with no shortfall but some
// cash... emits only the [decision], with no reductions" — leaving only the
// terminal-statement question this test asks.
func TestATerminalCashInLieuIsRefused(t *testing.T) {
	t.Parallel()

	splitAt := outstandingExitBar.Add(12 * time.Hour)
	err := runTerminalBacktest(t, writeActions(t, []event.CorporateActionPayload{
		{
			InstrumentID:            "AAPL",
			Kind:                    event.CorporateActionKindSplit,
			EffectiveAt:             splitAt,
			NewShares:               2,
			OldShares:               1,
			EngineSharesPerRawShare: 1,
			RawSharesLost:           0,
			CashInLieu:              91.0,
			Currency:                "USD",
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "could never reach any statement") {
		t.Fatalf("backtest() error = %v, want a refusal naming the terminal split's unreachable statement (ADR 0023, ADR 0024)", err)
	}
}

// TestATerminalSplitWithNoCashInLieuIsNotRefused: refuseIfCashCrediting
// checks CashInLieu, not the split kind alone. A split stating no shortfall
// and no cash (nothing to credit) is delivered normally even as a remaining
// action, since it changes no Unit and touches no ledger.
func TestATerminalSplitWithNoCashInLieuIsNotRefused(t *testing.T) {
	t.Parallel()

	splitAt := outstandingExitBar.Add(12 * time.Hour)
	err := runTerminalBacktest(t, writeActions(t, []event.CorporateActionPayload{
		{
			InstrumentID:            "AAPL",
			Kind:                    event.CorporateActionKindSplit,
			EffectiveAt:             splitAt,
			NewShares:               2,
			OldShares:               1,
			EngineSharesPerRawShare: 1,
			Currency:                "USD",
		},
	}))
	if err != nil {
		t.Fatalf("backtest() error = %v, want none: a split crediting no cash has nothing a terminal statement could miss", err)
	}
}

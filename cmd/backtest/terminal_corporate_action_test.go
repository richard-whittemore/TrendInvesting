package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
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

// runTerminalBacktestRecords is runTerminalBacktest for a run expected to
// succeed, returning the journal's records rather than only its error.
func runTerminalBacktestRecords(t *testing.T, actionsPath string) []journal.Record {
	t.Helper()
	return runJournal(t, options{
		configPath:           configurationFixture,
		barsPath:             writeBars(t, barsThroughEntry(t)),
		corporateActionsPath: actionsPath,
	})
}

// finalAccountSnapshot returns the last account.snapshot record in records.
func finalAccountSnapshot(t *testing.T, records []journal.Record) event.AccountSnapshotPayload {
	t.Helper()
	var last event.AccountSnapshotPayload
	found := false
	for _, r := range records {
		if r.Envelope.Type != event.AccountSnapshotEventType {
			continue
		}
		found = true
		decodeRecord(t, r, &last)
	}
	if !found {
		t.Fatal("no account.snapshot record in the journal")
	}
	return last
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

// TestATerminalSymbolChangeLeavesTheClosingSnapshotCorrect answers a review
// finding: a symbol change effective at or after its instrument's own last
// bar is, like a terminal dividend or cash in lieu, delivered by
// deliverRemainingActions AFTER fills.StateLastClose has already computed
// and frozen the run's one closing account.snapshot. Unlike a dividend or
// cash in lieu, this is NOT the same gap: event.AccountSnapshotPayload states
// only the account's aggregate Equity and AvailableCash — actual account
// figures, never the Notional Account (CONTEXT.md: "Notional Account" is a
// separate, sizing-only figure this payload does not carry at all) — with no
// per-instrument field a rename could leave stale, and a pure identity
// relabelling moves no cash and changes no holding's quantity or value
// (internal/fills.observeSymbolChanged only moves map keys: Simulator.books,
// account.holdings, account.closes).
// So the aggregate figures the closing statement already carries are exactly
// the same whether the rename is delivered before or after it. This test
// proves that, rather than asserting it: the final statement of a run ending
// with a terminal symbol change is byte-for-byte the SAME as one with no
// corporate action at all, over the identical bars.
//
// Refusing a terminal symbol change, the way a terminal cash credit is
// refused, would therefore reject a legitimate, common scenario (a company's
// last recorded bar under an old ticker, renamed before any new one arrives)
// for no correctness reason — the closing statement was never wrong.
func TestATerminalSymbolChangeLeavesTheClosingSnapshotCorrect(t *testing.T) {
	t.Parallel()

	renameAt := outstandingExitBar.Add(12 * time.Hour)

	baseline := finalAccountSnapshot(t, runTerminalBacktestRecords(t, corporateActionsEmptyFixture))
	withRename := runTerminalBacktestRecords(t, writeActions(t, []event.CorporateActionPayload{
		{
			InstrumentID:    "AAPL",
			Kind:            event.CorporateActionKindSymbolChange,
			EffectiveAt:     renameAt,
			NewInstrumentID: "AAPL2",
		},
	}))

	if got, want := finalAccountSnapshot(t, withRename), baseline; got != want {
		t.Fatalf("final account snapshot with a terminal symbol change = %+v, want the SAME as the baseline's %+v: a pure rename moves no cash and changes no holding's value", got, want)
	}

	found := false
	for _, r := range withRename {
		if r.Envelope.Type != event.InstrumentSymbolChangedEventType {
			continue
		}
		var changed event.InstrumentSymbolChangedPayload
		decodeRecord(t, r, &changed)
		if changed.InstrumentID == "AAPL" && changed.NewInstrumentID == "AAPL2" {
			found = true
		}
	}
	if !found {
		t.Fatal("no strategy.instrument.symbol-changed decision for AAPL -> AAPL2: the rename was not actually applied, so the matching statement above proves nothing")
	}
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// testConfigPath is the fixture every test in this package runs against,
// mirroring cmd/backtest's own testdata/configuration.json (a distinct
// strategy_id keeps a journal produced here from ever being mistaken for one
// cmd/backtest produced, should the two ever land beside each other).
const testConfigPath = "testdata/configuration.json"

// testConfiguration reads and validates testConfigPath, failing the test
// immediately if the fixture itself is broken.
func testConfiguration(t *testing.T) event.ConfigurationPayload {
	t.Helper()
	cfg, err := readConfiguration(testConfigPath)
	if err != nil {
		t.Fatalf("read the fixture configuration: %v", err)
	}
	return cfg
}

// flatBar returns a completed bar for instrument on
// 2026-01-02+dayOffset days, priced identically flat in both required views
// (split-adjusted and raw). Every bar this package's tests drive is built
// from this one function so that no bar's high ever exceeds another's:
// internal/strategy.Reducer's breakout rule is a strict ">" over the
// preceding EntryChannelLength highs, and a series that never changes can
// never satisfy it. That keeps every test in this package independent of
// internal/fills entirely (never composed here — see decider.go's decision
// 1 comment) and of whether an account.snapshot was ever supplied
// (cashAtPreviousClose is read only when a Signal is sized, and a flat
// series never raises one).
func flatBar(instrument string, dayOffset int) event.CompletedBarPayload {
	periodEnd := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).AddDate(0, 0, dayOffset)
	view := event.PriceView{
		View: event.ViewSplitAdjusted, Open: 100, High: 100, Low: 100, Close: 100, Volume: 1_000_000,
	}
	raw := view
	raw.View = event.ViewRaw
	return event.CompletedBarPayload{InstrumentID: instrument, PeriodEnd: periodEnd, SplitAdjusted: view, Raw: raw}
}

// barEnvelope wraps bar as a completed-bar input envelope, in the shape a
// LEAN adapter (or, here, a test client) sends over the wire: EventTime and
// RecordedAt both equal the bar's own PeriodEnd, mirroring cmd/backtest's own
// fixture convention (backtest.go's inputEnvelope) so that a comparison
// against a journal cmd/backtest produced from the identical bar (see
// compare_test.go) is not confounded by two different RecordedAt policies —
// internal/strategy.Reducer stamps every decision's own RecordedAt from the
// input's (reducer.go's stamp), so this choice is what lets that field match
// too.
func barEnvelope(t *testing.T, bar event.CompletedBarPayload, sequence uint64, strategyVersion, configurationHash string) event.Envelope {
	t.Helper()
	encoded, err := json.Marshal(bar)
	if err != nil {
		t.Fatalf("encode bar payload: %v", err)
	}
	return event.Envelope{
		ID:                "bar:" + bar.InstrumentID + ":" + bar.PeriodEnd.UTC().Format(time.RFC3339Nano),
		Type:              event.CompletedBarEventType,
		SchemaVersion:     event.CompletedBarSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         bar.PeriodEnd,
		RecordedAt:        bar.PeriodEnd,
		Sequence:          sequence,
		Source:            "test-client",
		StrategyVersion:   strategyVersion,
		ConfigurationHash: configurationHash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}
}

// shortSocketDir returns a fresh, private directory suitable for a
// Unix-domain socket: short enough that a socket inside it never
// approaches transport.MaxSocketPathBytes (t.TempDir()'s own path is
// already close to that limit once a subtest's own name is appended to it,
// per-macOS's /var/folders/... TMPDIR — ADR 0014's own consequence 2), and
// created with mode 0700 directly (os.MkdirTemp's own default), which is
// what transport.Listen requires of a socket's parent directory (ADR 0014's
// access amendment) — unlike t.TempDir(), whose directories are 0755.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "eng-")
	if err != nil {
		t.Fatalf("create a short-path socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// writeBarsFixture marshals bars as the JSON array cmd/backtest's own -bars
// flag reads (backtest.go's readBars), so compare_test.go can hand the exact
// same bytes to both cmd/backtest and this command's test client.
func writeBarsFixture(t *testing.T, dir string, bars []event.CompletedBarPayload) string {
	t.Helper()
	encoded, err := json.Marshal(bars)
	if err != nil {
		t.Fatalf("encode bars fixture: %v", err)
	}
	path := filepath.Join(dir, "bars.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write bars fixture: %v", err)
	}
	return path
}

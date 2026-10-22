package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// pairedJournal is a synthetic run with two closed sessions: SPY 100 then
// 101 (no dividend), and the account 4000 then 4010.
func pairedJournal(t *testing.T, dir string) string {
	t.Helper()
	entries := haltedEntries(t)[:1]
	for n, c := range []float64{100, 101} {
		at := t0.AddDate(0, 0, n)
		view := func(label string) event.PriceView {
			return event.PriceView{View: label, Open: c, High: c, Low: c, Close: c, Volume: 1}
		}
		seq := uint64(len(entries))
		entries = append(entries,
			journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, seq+1, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "bar"+string(rune('0'+n)), at,
				event.CompletedBarAdjustedPayload{CompletedBarPayload: event.CompletedBarPayload{InstrumentID: "SPY", PeriodEnd: at, SplitAdjusted: view(event.ViewSplitAdjusted), Raw: view(event.ViewRaw)},
					Adjusted: view(event.ViewAdjusted), AdjustedBase: "2024-01-02"})},
			journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, seq+2, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "snap"+string(rune('0'+n)), at,
				event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: at, Currency: "USD", NetLiquidation: 4000 + float64(10*n)})},
			journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, seq+3, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "session"+string(rune('0'+n)), at,
				event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}})})
	}
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("h", "v", t0.Add(-time.Hour), t0.AddDate(0, 0, 1)), entries); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "joint.journal")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheDailySummaryGivesTheVerdictWithTheFlexLedgersTransfersMirrored(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	journalPath := pairedJournal(t, dir)
	stamp := t0.AddDate(0, 0, 1).Add(-2 * time.Hour).Format(time.RFC3339)
	line, err := json.Marshal(map[string]any{"id": "ibkr-flex:t1", "type": event.CashMovementEventType, "schema_version": event.CashMovementTransferVersion,
		"event_time": stamp, "source": "ibkr-flex",
		"payload": map[string]any{"as_of": stamp, "amount": 10, "equity_before": 4000, "currency": "USD", "transaction_id": "t1"}})
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "cash-movements.jsonl")
	if err := os.WriteFile(ledger, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return t0.AddDate(0, 0, 1).Add(time.Hour) }
	var out bytes.Buffer
	args := []string{"-journal", journalPath, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy", "-summary", "-cash-ledger", ledger}
	if err := run(args, fakeEnv, now, &out, fakeClient); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Behind SPY by 1.0 point since 2024-01-02; dollar gap -40.00 (account 4010.00, SPY shadow 4050.00)",
		"your deposits and withdrawals mirrored as recorded by the Flex reader (1 in this period: 10.00 in, 0.00 out)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, out.String())
		}
	}
	// Without a ledger the summary says the comparison assumes no transfer.
	out.Reset()
	args = []string{"-journal", journalPath, "-state", filepath.Join(dir, "s2.json"), "-channel", "stdout,ntfy", "-summary"}
	if err := run(args, fakeEnv, now, &out, fakeClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "deposits and withdrawals not yet recorded: the comparison assumes none") {
		t.Errorf("summary without a ledger:\n%s", out.String())
	}
}

// The summary says how far the Flex reader has recorded, and takes a ledger
// only from the account's own flex/<instance> folder: the instance of the
// engine directory the journal is in.
func TestTheDailySummaryTakesOnlyTheAccountsOwnLedgerAndSaysHowFarItReaches(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	root := t.TempDir()
	engines, own, other := filepath.Join(root, "engines", "trad-ira"), filepath.Join(root, "flex", "trad-ira"), filepath.Join(root, "flex", "roth-ira")
	for _, d := range []string{engines, own, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	journalPath := pairedJournal(t, engines)
	stamp := t0.AddDate(0, 0, 1).Add(-2 * time.Hour).Format(time.RFC3339)
	line, err := json.Marshal(map[string]any{"id": "ibkr-flex:t1", "type": event.CashMovementEventType, "schema_version": event.CashMovementTransferVersion,
		"event_time": stamp, "source": "ibkr-flex",
		"payload": map[string]any{"as_of": stamp, "amount": 10, "equity_before": 4000, "currency": "USD", "transaction_id": "t1"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{own, other} {
		ledger := filepath.Join(d, "cash-movements.jsonl")
		if err := os.WriteFile(ledger, append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		// The reader's last reading covered the day before the summary's.
		if err := os.WriteFile(ledger+".status.json", []byte(`{"covered_to":"20240102","state":"read"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := func() time.Time { return t0.AddDate(0, 0, 1).Add(time.Hour) }
	for name, c := range map[string]struct{ dir, want string }{
		"own":   {own, "transfers recorded through 2024-01-02; deposits or withdrawals from the last two trading sessions may not be recorded yet; a transfer appears in the summary two trading sessions after it posts (a Friday transfer on Tuesday's)"},
		"other": {other, "ledger not accepted: it belongs to roth-ira, not trad-ira"},
	} {
		var out bytes.Buffer
		args := []string{"-journal", journalPath, "-state", filepath.Join(root, name+".json"), "-channel", "stdout,ntfy", "-summary", "-cash-ledger", filepath.Join(c.dir, "cash-movements.jsonl")}
		if err := run(args, fakeEnv, now, &out, fakeClient); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("%s: summary lacks %q:\n%s", name, c.want, out.String())
		}
	}
}

// The daily summary's costs line totals the month's commissions, fees and
// interest from the costs ledger beside the cash ledger, and says when that
// ledger is not there yet.
func TestTheDailySummaryGivesThisMonthsCostsFromTheLedgerBesideTheCashLedger(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	journalPath := pairedJournal(t, dir)
	stamp := t0.AddDate(0, 0, 1).Add(-2 * time.Hour).Format(time.RFC3339)
	var lines strings.Builder
	for _, c := range []struct {
		id, kind string
		amount   float64
	}{{"c1", event.CostCommission, -1}, {"c2", event.CostFee, -10}, {"c3", event.CostInterestPaid, -3.21}, {"c4", event.CostInterestReceived, 0.42}} {
		line, err := json.Marshal(map[string]any{"id": "ibkr-flex:" + c.id, "type": event.CostEventType, "schema_version": event.CostSchemaVersion,
			"event_time": stamp, "source": "ibkr-flex",
			"payload": map[string]any{"kind": c.kind, "amount": c.amount, "currency": "USD", "as_of": stamp, "transaction_id": c.id}})
		if err != nil {
			t.Fatal(err)
		}
		lines.Write(line)
		lines.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "costs.jsonl"), []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return t0.AddDate(0, 0, 1).Add(time.Hour) }
	var out bytes.Buffer
	args := []string{"-journal", journalPath, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy", "-summary", "-cash-ledger", filepath.Join(dir, "cash-movements.jsonl")}
	if err := run(args, fakeEnv, now, &out, fakeClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Costs this month: commissions 1.00, fees 10.00, interest 2.79\n") {
		t.Errorf("summary lacks the costs line:\n%s", out.String())
	}
	out.Reset()
	args = []string{"-journal", journalPath, "-state", filepath.Join(dir, "s2.json"), "-channel", "stdout,ntfy", "-summary"}
	if err := run(args, fakeEnv, now, &out, fakeClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Costs this month: commissions 0.00, fees 0.00, interest 0.00 (fees and interest not yet recorded by the Flex reader)") {
		t.Errorf("summary without a ledger:\n%s", out.String())
	}
}

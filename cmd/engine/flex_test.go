package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// The Flex ledger seam (ADR 0019, amended 2026-10-21): before each adapter
// input, the engine delivers every ledger record and the reader's latest
// status not yet journalled in this run, as inputs at the next sequences,
// each checked as the reader writes it.

const flexHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// ledgerRecord is a ledger line as the Flex reader writes it: a synthetic
// deposit, IBKR's transaction id, on a made-up day.
func ledgerRecord(id string) string {
	return `{"event_time": "2026-06-02T04:00:00Z", "id": "ibkr-flex:` + id + `", "payload": {"amount": 1000.0, "as_of": "2026-06-02T04:00:00Z", "currency": "USD", "equity_before": 20000.0, "transaction_id": "` + id + `"}, "schema_version": 2, "source": "ibkr-flex", "type": "account.cash-movement"}` + "\n"
}

const readStatus = `{"checked_at": "2026-06-03T01:30:00Z", "covered_to": "20260601", "state": "read", "written": 1}` + "\n"

// instanceDirs lays out a server's state for instance: its engine's state
// directory, holding the durable log at logPath, and its Flex directory,
// holding the ledger at ledgerPath.
func instanceDirs(t *testing.T, instance string) (logPath, ledgerPath string) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{filepath.Join(root, "engines", instance), filepath.Join(root, "flex", instance)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "engines", instance, "run-1.log"), filepath.Join(root, "flex", instance, "cash-movements.jsonl")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// flexWiring is a decider over an accepting handler with ledgerPath's
// ledger delivered before each adapter input, its output in out.
func flexWiring(t *testing.T, logPath, ledgerPath string, recovered *journal.Recovery, out *bytes.Buffer) (func(event.Envelope) (event.Envelope, error), *journal.Recorder) {
	t.Helper()
	accepting := replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil })
	engine, recorder, err := durableWiring(t, logPath, flexHash, accepting, recovered)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := openFlexLedger(options{logPath: logPath, cashLedger: ledgerPath, mode: modePaper}, composition{accountID: "joint", strategyVersion: "test", configurationHash: flexHash}, recovered, out)
	if err != nil {
		t.Fatal(err)
	}
	ledger.recorder = recorder
	if recovered == nil {
		// The run's own first input, as the configuration is: the
		// engine's cursor then names the next sequence.
		if _, err := engine.Apply(context.Background(), adapterInput(t, "configuration", 1, flexHash)); err != nil {
			t.Fatal(err)
		}
	}
	decide := newDecider(&wireGuard{ledger: ledger}, engine, recorder, "test", flexHash, nil)
	return func(in event.Envelope) (event.Envelope, error) { return decide(context.Background(), in) }, recorder
}

func inputIDs(recorder *journal.Recorder) []string {
	var ids []string
	for _, e := range recorder.Entries() {
		if e.Kind == journal.KindInput {
			ids = append(ids, e.Envelope.ID)
		}
	}
	return ids
}

func TestTheLedgerAndTheStatusAreDeliveredBeforeTheAdapterInput(t *testing.T) {
	logPath, ledgerPath := instanceDirs(t, "trad-ira")
	writeFile(t, ledgerPath, ledgerRecord("T1"))
	writeFile(t, ledgerPath+".status.json", readStatus)
	decide, recorder := flexWiring(t, logPath, ledgerPath, nil, &bytes.Buffer{})

	reply, err := decide(adapterInput(t, "adapter:1", 2, flexHash))
	if err != nil || reply.Sequence != 4 {
		t.Fatalf("reply at %d, %v; want the adapter's input at 4, after the record and the status", reply.Sequence, err)
	}
	ids := inputIDs(recorder)
	if len(ids) != 4 || ids[1] != "ibkr-flex:T1" || !strings.HasPrefix(ids[2], "ibkr-flex-status:") || ids[3] != "adapter:1" {
		t.Fatalf("inputs %v", ids)
	}
	movement := recorder.Entries()[1].Envelope
	if movement.Type != event.CashMovementEventType || movement.SchemaVersion != event.CashMovementTransferVersion || movement.Source != event.FlexSource {
		t.Errorf("the record's envelope: %+v", movement)
	}
	var p event.CashMovementPayload
	if err := json.Unmarshal(movement.Payload, &p); err != nil || p.Amount != 1000 || p.TransactionID != "T1" {
		t.Errorf("payload %+v, %v", p, err)
	}
	var status event.FlexStatusPayload
	if err := json.Unmarshal(recorder.Entries()[2].Envelope.Payload, &status); err != nil || status.AccountID != "joint" || status.CoveredTo != "20260601" {
		t.Errorf("status %+v, %v", status, err)
	}

	// Nothing new: the next input is recorded where it was sent.
	if reply, err := decide(adapterInput(t, "adapter:2", 5, flexHash)); err != nil || reply.Sequence != 5 {
		t.Fatalf("reply at %d, %v", reply.Sequence, err)
	}
	// The reader writes another transfer, and a new status: both go next.
	appendFile(t, ledgerPath, ledgerRecord("T2"))
	writeFile(t, ledgerPath+".status.json", strings.Replace(readStatus, "01:30", "01:31", 1))
	if reply, err := decide(adapterInput(t, "adapter:3", 6, flexHash)); err != nil || reply.Sequence != 8 {
		t.Fatalf("reply at %d, %v", reply.Sequence, err)
	}
	if ids := inputIDs(recorder); len(ids) != 8 || ids[5] != "ibkr-flex:T2" {
		t.Errorf("inputs %v", ids)
	}
}

func TestAnAdapterInputMovedByFlexInputsIsAnsweredAgain(t *testing.T) {
	logPath, ledgerPath := instanceDirs(t, "trad-ira")
	writeFile(t, ledgerPath, ledgerRecord("T1"))
	decide, recorder := flexWiring(t, logPath, ledgerPath, nil, &bytes.Buffer{})
	first, err := decide(adapterInput(t, "adapter:1", 2, flexHash))
	if err != nil || first.Sequence != 3 {
		t.Fatalf("reply at %d, %v", first.Sequence, err)
	}
	again, err := decide(adapterInput(t, "adapter:1", 2, flexHash))
	if err != nil || !bytes.Equal(event.CanonicalEnvelopeBytes(first), event.CanonicalEnvelopeBytes(again)) {
		t.Fatalf("sent again: %v", err)
	}
	if len(recorder.Entries()) != 3 {
		t.Errorf("nothing is recorded twice: %v", inputIDs(recorder))
	}
	// Resumed, the log's Flex inputs moved the adapter's input, so it is
	// answered at the sequence it was sent with.
	recovery := recoverFile(t, logPath)
	nextLog := filepath.Join(filepath.Dir(logPath), "run-2.log")
	resumed, recorder := flexWiring(t, nextLog, ledgerPath, &recovery, &bytes.Buffer{})
	if _, err := resumed(adapterInput(t, "adapter:1", 2, flexHash)); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
	if ids := inputIDs(recorder); len(ids) != 3 {
		t.Errorf("a resumed run delivers nothing it holds: %v", ids)
	}
}

func TestALedgerLineNotAsTheReaderWritesItDeliversNothingFromTheLedger(t *testing.T) {
	logPath, ledgerPath := instanceDirs(t, "trad-ira")
	writeFile(t, ledgerPath, ledgerRecord("T1")+"not a record\n")
	writeFile(t, ledgerPath+".status.json", readStatus)
	out := &bytes.Buffer{}
	decide, recorder := flexWiring(t, logPath, ledgerPath, nil, out)
	if _, err := decide(adapterInput(t, "adapter:1", 2, flexHash)); err != nil {
		t.Fatal(err)
	}
	if _, err := decide(adapterInput(t, "adapter:2", 4, flexHash)); err != nil {
		t.Fatal(err)
	}
	for _, id := range inputIDs(recorder) {
		if strings.HasPrefix(id, "ibkr-flex:") {
			t.Errorf("a record of a refused ledger was delivered: %s", id)
		}
	}
	if got := strings.Count(out.String(), "line 2"); got != 1 {
		t.Errorf("the refusal is said once, naming the line: %q", out.String())
	}
}

func TestTheLedgerMustBeTheInstances(t *testing.T) {
	logPath, _ := instanceDirs(t, "trad-ira")
	_, other := instanceDirs(t, "roth-ira")
	comp := composition{accountID: "ira", strategyVersion: "test", configurationHash: flexHash}
	if _, err := openFlexLedger(options{logPath: logPath, cashLedger: other, mode: modePaper}, comp, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "roth-ira") {
		t.Errorf("another instance's ledger: %v", err)
	}
	if _, err := openFlexLedger(options{logPath: logPath, cashLedger: filepath.Join(t.TempDir(), "missing", "cash-movements.jsonl"), mode: modePaper}, comp, nil, &bytes.Buffer{}); err == nil {
		t.Errorf("a ledger whose folder is missing is refused")
	}
	if _, err := openFlexLedger(options{logPath: logPath, cashLedger: other, mode: modePaper}, composition{strategyVersion: "test"}, nil, &bytes.Buffer{}); err == nil {
		t.Errorf("a Turtle run reads no ledger")
	}
	mine := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(logPath))), "flex", "trad-ira", "cash-movements.jsonl")
	if _, err := openFlexLedger(options{logPath: logPath, cashLedger: mine, mode: modePaper}, comp, nil, &bytes.Buffer{}); err != nil {
		t.Errorf("the instance's own ledger, not yet written: %v", err)
	}
}

func TestTheCashLedgerFlagIsChecked(t *testing.T) {
	err := run(context.Background(), options{socketPath: "s", configPath: "c", outPath: "o", asOf: "2026-01-01T00:00:00Z", build: "b", mode: modeBacktest, cashLedger: "l"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "-cash-ledger") {
		t.Errorf("a backtest takes no ledger: %v", err)
	}
}

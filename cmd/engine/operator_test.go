package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/operator"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// writeKillSwitch records the operator's kill switch for account at at into
// inbox, as the operator tool does, and returns the record.
func writeKillSwitch(t *testing.T, inbox, account string, at time.Time, engaged bool) operator.Record {
	t.Helper()
	r, err := operator.KillSwitch(event.KillSwitchPayload{AccountID: account, AsOf: at, Engaged: engaged, Operator: "owner", Detail: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, r.FileName()), r.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

// operatorInputs returns the operator inputs in entries, by envelope ID.
func operatorInputs(entries []journal.Entry) []event.Envelope {
	var out []event.Envelope
	for _, e := range entries {
		if e.Kind == journal.KindInput && e.Envelope.Source == operator.Source {
			out = append(out, e.Envelope)
		}
	}
	return out
}

// refusalReasons returns the reasons of the risk.order.refused decisions in
// entries.
func refusalReasons(t *testing.T, entries []journal.Entry) []string {
	t.Helper()
	var out []string
	for _, e := range entries {
		if e.Envelope.Type != event.OrderRefusedEventType {
			continue
		}
		var p event.OrderRefusedPayload
		if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Reason)
	}
	return out
}

// inboxOptions is a dry-run Boost invocation in dir reading the operator
// inbox dir/operator.
func inboxOptions(t *testing.T, dir string) options {
	t.Helper()
	opts := boostOptions(t, dir)
	opts.operatorDir = filepath.Join(dir, "operator")
	if err := os.Mkdir(opts.operatorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return opts
}

// TestTheOperatorsKillSwitchStopsAndReleasesOrders: the operator's kill
// switch, recorded into the engine's inbox by the operator tool, is
// delivered as an operator.kill-switch input before the adapter's next
// input, at the next position of the run's one input sequence, and journalled
// before any decision it affects (ADR 0026, point 3; ADR 0027). Engaged,
// Boost's first buy is refused (kill-switch) and nothing is released; Boost
// orders only when its target changes (orderplan.PlanBoost), so the refused
// buy is not planned again. Released before the session closes, the buy is
// released. The adapter continues the sequence the decisions carry.
func TestTheOperatorsKillSwitchStopsAndReleasesOrders(t *testing.T) {
	for _, c := range []struct {
		name     string
		release  bool
		released int
		refused  []string
	}{
		{"engaged", false, 0, []string{event.RefusedKillSwitch}},
		{"released before the close", true, 1, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := inboxOptions(t, dir)
			stop := startEngine(t, opts)
			want := []string{writeKillSwitch(t, opts.operatorDir, "joint", boostSession(0).Add(-2*time.Hour), true).ID}
			feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
			feed.beforeClose = func(i int) {
				if i == 0 && c.release {
					want = append(want, writeKillSwitch(t, opts.operatorDir, "joint", boostSession(0).Add(-time.Hour), false).ID)
				}
			}
			feed.sessions(0, 3)
			_ = feed.client.Close()
			if err := stop(); err != nil {
				t.Fatalf("stop the engine: %v", err)
			}
			if len(feed.released) != c.released {
				t.Errorf("released %+v, want %d", feed.released, c.released)
			}
			entries := recoverFile(t, opts.logPath).Entries
			var got []string
			for _, e := range operatorInputs(entries) {
				got = append(got, e.ID)
			}
			if !slices.Equal(got, want) {
				t.Errorf("the log holds operator inputs %v, want %v", got, want)
			}
			var previous uint64
			for _, e := range entries {
				if e.Kind != journal.KindInput {
					continue
				}
				if previous != 0 && e.Envelope.Sequence != previous+1 {
					t.Fatalf("input %s at sequence %d follows %d", e.Envelope.ID, e.Envelope.Sequence, previous)
				}
				previous = e.Envelope.Sequence
			}
			if got := refusalReasons(t, entries); !slices.Equal(got, c.refused) {
				t.Errorf("refusals %v, want %v", got, c.refused)
			}
		})
	}
}

// TestAnAlertAcknowledgementIsDeliveredAndJournalled: an acknowledgement the
// operator tool records in the inbox is delivered as an
// operator.alert.acknowledged input before the adapter's next input and
// journalled, with no refusal note beside it: the engine decides nothing
// from it (ADR 0019, amended 2026-09-24, "Alerting").
func TestAnAlertAcknowledgementIsDeliveredAndJournalled(t *testing.T) {
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	r, err := operator.AlertAcknowledged(event.AlertAcknowledgedPayload{
		AccountID: "joint", AlertKey: "engine-state:halted:risk-limit-breached:||:2026-10-17T14:00:00Z@joint",
		AcknowledgedBy: "owner", AcknowledgedAt: boostSession(1).Add(-time.Hour), Detail: "seen"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opts.operatorDir, r.FileName()), r.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	feed.sessions(1, 2)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	inputs := operatorInputs(recoverFile(t, opts.logPath).Entries)
	if len(inputs) != 1 || inputs[0].ID != r.ID || inputs[0].Type != event.AlertAcknowledgedEventType {
		t.Errorf("the log holds operator inputs %+v, want the acknowledgement", inputs)
	}
	if note, err := os.ReadFile(filepath.Join(opts.operatorDir, r.FileName()+refusedSuffix)); err == nil {
		t.Errorf("the acknowledgement was refused: %q", note)
	}
	if _, err := os.Stat(filepath.Join(opts.operatorDir, r.FileName()+deliveredSuffix)); err != nil {
		t.Errorf("no delivered marker: %v", err)
	}
}

// TestAnOperatorInputIsDeliveredOnce: an input recorded in an earlier log of
// the directory, this run's resumed log included, is never delivered again,
// though its record stays in the inbox. A resume reports the sequence the
// adapter's next input carries.
func TestAnOperatorInputIsDeliveredOnce(t *testing.T) {
	dir := t.TempDir()
	first := inboxOptions(t, dir)
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	engaged := writeKillSwitch(t, first.operatorDir, "joint", boostSession(1).Add(-time.Hour), true)
	feed.sessions(1, 2)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	next := feed.sequence

	resumed := first
	resumed.resumePath, resumed.logPath, resumed.outPath = first.logPath, filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	resumed.socketPath = filepath.Join(shortSocketDir(t), "engine.sock")
	out := &strings.Builder{}
	stop = startEngineInvocation(t, func(ctx context.Context, w io.Writer) error { return run(ctx, resumed, io.MultiWriter(w, out)) })
	feed = newBoostFeed(t, resumed.socketPath, boostConfiguration(t, boostConfigPath), "test-build", next)
	feed.sessions(2, 4)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if inputs := operatorInputs(recoverFile(t, resumed.logPath).Entries); len(inputs) != 1 || inputs[0].ID != engaged.ID {
		t.Errorf("the resumed log holds operator inputs %+v, want the engagement once", inputs)
	}
	if want := fmt.Sprintf("the adapter's next input carries sequence %d", next); !strings.Contains(out.String(), want) {
		t.Errorf("the resume reported %q, want it to say %q", out.String(), want)
	}
}

// TestAnOperatorRecordTheEngineRefusesIsNoted: a record the engine cannot
// read is never delivered, and one the run refuses (another account's) is
// journalled as delivered with no effect; each is noted beside it in the
// inbox with why, never retried, and the adapter's input is decided as
// usual.
func TestAnOperatorRecordTheEngineRefusesIsNoted(t *testing.T) {
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	damaged := filepath.Join(opts.operatorDir, "20240103T190000Z-operator.kill-switch.json")
	if err := os.WriteFile(damaged, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := writeKillSwitch(t, opts.operatorDir, "ira", boostSession(1).Add(-time.Hour), true)
	feed.sessions(1, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{damaged: "decode the operator record", filepath.Join(opts.operatorDir, other.FileName()): "kill switch for account"} {
		note, err := os.ReadFile(path + refusedSuffix)
		if err != nil || !strings.Contains(string(note), want) {
			t.Errorf("%s: note %q (%v), want it to say %q", path, note, err, want)
		}
	}
	inputs := operatorInputs(recoverFile(t, opts.logPath).Entries)
	if len(inputs) != 1 || inputs[0].ID != other.ID {
		t.Errorf("the log holds operator inputs %+v, want only the other account's, refused", inputs)
	}
	if len(feed.released) == 0 {
		t.Error("the adapter's inputs were not decided")
	}
}

// TestTheOperatorInboxIsForARunThatCanReachABroker: a backtest takes no
// operator input, and the inbox must be a directory that exists.
func TestTheOperatorInboxIsForARunThatCanReachABroker(t *testing.T) {
	dir := t.TempDir()
	backtest := boostOptions(t, dir)
	backtest.mode, backtest.logPath, backtest.operatorDir = modeBacktest, "", dir
	refuse(t, "a backtest", backtest, "-operator")
	missing := boostOptions(t, dir)
	missing.operatorDir = filepath.Join(dir, "absent")
	refuse(t, "no such directory", missing, "absent")
	file := boostOptions(t, dir)
	file.operatorDir = filepath.Join(dir, "file")
	if err := os.WriteFile(file.operatorDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	refuse(t, "a file", file, "not a directory")
}

// failingSink fails every write of the durable log once fail is set, as a
// disk fault does partway through a run.
type failingSink struct {
	journal.SyncWriter
	fail *atomic.Bool
}

func (s failingSink) Write(p []byte) (int, error) {
	if s.fail.Load() {
		return 0, errors.New("injected write fault")
	}
	return s.SyncWriter.Write(p)
}

// TestALogFailureWhileDeliveringAnOperatorRecordLeavesItForTheNextStart: a
// record whose input could not be made durable was not delivered: no note
// is written beside it, the run stops with the error (ADR 0027 §3: a failed
// write ends the log), and a resume from the log delivers it, so a kill
// switch is never dropped across a restart.
func TestALogFailureWhileDeliveringAnOperatorRecordLeavesItForTheNextStart(t *testing.T) {
	var fail atomic.Bool
	original := durableLogSink
	durableLogSink = func(file *os.File) journal.SyncWriter { return failingSink{SyncWriter: file, fail: &fail} }
	t.Cleanup(func() { durableLogSink = original })

	dir := t.TempDir()
	first := inboxOptions(t, dir)
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	engaged := writeKillSwitch(t, first.operatorDir, "joint", boostSession(1).Add(-time.Hour), true)
	fail.Store(true)
	at := boostSession(1)
	if err := feed.trySend(event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "mark:failed", at,
		event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: boostClose(1), Source: event.AdapterIBKR}); err == nil {
		t.Fatal("an input decided after its operator record could not be made durable")
	}
	_ = feed.client.Close()
	err := stop()
	if err == nil || !strings.Contains(err.Error(), engaged.ID) || !strings.Contains(err.Error(), "not durable") {
		t.Fatalf("the run ended with %v, want it stopped by the operator record %s that is not durable", err, engaged.ID)
	}
	if _, err := os.Stat(filepath.Join(first.operatorDir, engaged.FileName()+refusedSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refusal note was written beside a record the log failed under: %v", err)
	}

	fail.Store(false)
	resumed := first
	resumed.resumePath, resumed.logPath, resumed.outPath = first.logPath, filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	resumed.socketPath = filepath.Join(shortSocketDir(t), "engine.sock")
	stop = startEngine(t, resumed)
	feed = newBoostFeed(t, resumed.socketPath, boostConfiguration(t, boostConfigPath), "test-build", feed.sequence)
	feed.sessions(1, 2)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if inputs := operatorInputs(recoverFile(t, resumed.logPath).Entries); len(inputs) != 1 || inputs[0].ID != engaged.ID {
		t.Errorf("the resumed log holds operator inputs %+v, want the engagement", inputs)
	}
}

// TestADeliveredRecordIsMarkedSoAMovedLogCannotRedeliverIt: once its input
// is flushed to the durable log, a record is marked delivered beside it, so
// a run whose -log directory no longer holds that log (quarantined, or a
// new directory) does not deliver it again.
func TestADeliveredRecordIsMarkedSoAMovedLogCannotRedeliverIt(t *testing.T) {
	dir := t.TempDir()
	first := inboxOptions(t, dir)
	stop := startEngine(t, first)
	engaged := writeKillSwitch(t, first.operatorDir, "joint", boostSession(0).Add(-2*time.Hour), true)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(first.operatorDir, engaged.FileName()+deliveredSuffix)); err != nil {
		t.Errorf("the delivered record is not marked: %v", err)
	}

	elsewhere := boostOptions(t, t.TempDir())
	elsewhere.operatorDir = first.operatorDir
	stop = startEngine(t, elsewhere)
	feed = newBoostFeed(t, elsewhere.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if inputs := operatorInputs(recoverFile(t, elsewhere.logPath).Entries); len(inputs) != 0 {
		t.Errorf("a run logging elsewhere delivered %+v again", inputs)
	}
}

// TestAKillSwitchOlderThanTheOneInForceIsRefused: the owner's instructions
// are ordered by the time they state, so a kill-switch record stating a time
// earlier than the newest kill switch already applied for its account (one
// written with -at in the past) is refused by the gate, journalled as a
// refused input like any other refusal, and noted, and the switch stays as
// the later instruction left it.
func TestAKillSwitchOlderThanTheOneInForceIsRefused(t *testing.T) {
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	engaged := writeKillSwitch(t, opts.operatorDir, "joint", boostSession(0).Add(-2*time.Hour), true)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	stale := writeKillSwitch(t, opts.operatorDir, "joint", boostSession(0).Add(-3*time.Hour), false)
	feed.sessions(1, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	entries := recoverFile(t, opts.logPath).Entries
	if inputs := operatorInputs(entries); len(inputs) != 2 || inputs[0].ID != engaged.ID || inputs[1].ID != stale.ID {
		t.Errorf("the log holds operator inputs %+v, want the engagement and the refused release", inputs)
	}
	note, err := os.ReadFile(filepath.Join(opts.operatorDir, stale.FileName()+refusedSuffix))
	if err != nil || !strings.Contains(string(note), engaged.ID) {
		t.Errorf("note %q (%v), want it to name the later instruction %s", note, err, engaged.ID)
	}
	if len(feed.released) != 0 {
		t.Errorf("released %+v with the switch engaged", feed.released)
	}
}

// TestAKillSwitchStatingAFutureTimeIsRefused: a kill switch stating a time
// later than now, beyond the clock skew allowed, is a mistyped -at; it would
// order every truthful instruction before it, so it is never delivered and
// is noted, as a record the engine cannot read is.
func TestAKillSwitchStatingAFutureTimeIsRefused(t *testing.T) {
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	future := writeKillSwitch(t, opts.operatorDir, "joint", time.Now().Add(time.Hour).Truncate(time.Second), false)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if inputs := operatorInputs(recoverFile(t, opts.logPath).Entries); len(inputs) != 0 {
		t.Errorf("the log holds operator inputs %+v, want none", inputs)
	}
	note, err := os.ReadFile(filepath.Join(opts.operatorDir, future.FileName()+refusedSuffix))
	if err != nil || !strings.Contains(string(note), "later than now") {
		t.Errorf("note %q (%v), want it to say the time is later than now", note, err)
	}
}

// TestAKillSwitchWithinTheClockSkewIsDelivered: a kill switch stating a
// time ahead of the engine's clock by less than operatorClockSkew is from a
// clock that runs slightly ahead, not a mistyped -at, so it is delivered and
// journalled, never refused (ADR 0019, amendment of 2026-10-17, "operator
// instructions count in the order of the times they state").
func TestAKillSwitchWithinTheClockSkewIsDelivered(t *testing.T) {
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	ahead := writeKillSwitch(t, opts.operatorDir, "joint", time.Now().Add(operatorClockSkew-time.Minute).Truncate(time.Second), true)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if inputs := operatorInputs(recoverFile(t, opts.logPath).Entries); len(inputs) != 1 || inputs[0].ID != ahead.ID {
		t.Errorf("the log holds operator inputs %+v, want %s", inputs, ahead.ID)
	}
	if _, err := os.Stat(filepath.Join(opts.operatorDir, ahead.FileName()+deliveredSuffix)); err != nil {
		t.Errorf("no delivered marker: %v", err)
	}
	if note, err := os.ReadFile(filepath.Join(opts.operatorDir, ahead.FileName()+refusedSuffix)); err == nil {
		t.Errorf("refused within the clock skew: %q", note)
	}
}

// TestAClearanceStatingAFutureTimeIsRefused: the future-time refusal holds
// for every operator record, not the kill switch alone: a clearance stating
// a time later than now, beyond the clock skew allowed, is never delivered,
// and is noted (ADR 0019, amendment of 2026-10-17, "operator instructions
// count in the order of the times they state").
func TestAClearanceStatingAFutureTimeIsRefused(t *testing.T) {
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	future, err := operator.RebalanceCleared(event.RebalanceClearedPayload{
		AccountID: "joint", AsOf: time.Now().Add(time.Hour).Truncate(time.Second), DecisionID: "targets:test",
		ClientOrderIDs: []string{"order-1"}, Operator: "owner", Detail: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opts.operatorDir, future.FileName()), future.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if inputs := operatorInputs(recoverFile(t, opts.logPath).Entries); len(inputs) != 0 {
		t.Errorf("the log holds operator inputs %+v, want none", inputs)
	}
	note, err := os.ReadFile(filepath.Join(opts.operatorDir, future.FileName()+refusedSuffix))
	if err != nil || !strings.Contains(string(note), "later than now") {
		t.Errorf("note %q (%v), want it to say the time is later than now", note, err)
	}
}

// TestARefusedRecordIsSettledUnderItsFileName: a record kept under a name
// other than the one the operator tool gives it is refused once, its note
// beside the file itself, whether it is refused before delivery or
// journalled as refused.
func TestARefusedRecordIsSettledUnderItsFileName(t *testing.T) {
	for _, c := range []struct {
		name string
		at   func() time.Time
	}{
		{"stated in the future", func() time.Time { return time.Now().Add(time.Hour).Truncate(time.Second) }},
		{"backdated", func() time.Time { return boostSession(0).Add(-3 * time.Hour) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := inboxOptions(t, dir)
			out := &strings.Builder{}
			stop := startEngineInvocation(t, func(ctx context.Context, w io.Writer) error { return run(ctx, opts, io.MultiWriter(w, out)) })
			writeKillSwitch(t, opts.operatorDir, "joint", boostSession(0).Add(-2*time.Hour), true)
			feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
			feed.sessions(0, 1)
			r, err := operator.KillSwitch(event.KillSwitchPayload{AccountID: "joint", AsOf: c.at(), Engaged: false, Operator: "owner", Detail: "a copy"})
			if err != nil {
				t.Fatal(err)
			}
			copied := filepath.Join(opts.operatorDir, "kill-switch-copy.json")
			if err := os.WriteFile(copied, r.Encode(), 0o600); err != nil {
				t.Fatal(err)
			}
			feed.sessions(1, 3)
			_ = feed.client.Close()
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(out.String(), "refused operator record"); got != 1 {
				t.Errorf("the record was refused %d times, want once:\n%s", got, out.String())
			}
			if _, err := os.Stat(copied + refusedSuffix); err != nil {
				t.Errorf("no note beside the record itself: %v", err)
			}
			for _, suffix := range []string{refusedSuffix, deliveredSuffix} {
				if _, err := os.Stat(filepath.Join(opts.operatorDir, r.FileName()+suffix)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("a %s file beside a name that is no file: %v", suffix, err)
				}
			}
		})
	}
}

// TestADeliveredRecordIsNotReadAgain: the inbox reads a record once; one
// already delivered is not read again before each later input.
func TestADeliveredRecordIsNotReadAgain(t *testing.T) {
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	engaged := writeKillSwitch(t, opts.operatorDir, "joint", boostSession(0).Add(-2*time.Hour), true)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	if err := os.WriteFile(filepath.Join(opts.operatorDir, engaged.FileName()), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	feed.sessions(1, 2)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(opts.operatorDir, engaged.FileName()+refusedSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a delivered record was read again: %v", err)
	}
}

// TestARefusedInputAfterOperatorInputsStatesItsSequence: when operator
// inputs were recorded before the adapter's input and that input is
// refused, the refusal states, in its Sequence, where the input was
// recorded, and the adapter continues from it (the wire contract,
// engine.go).
func TestARefusedInputAfterOperatorInputsStatesItsSequence(t *testing.T) {
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	writeKillSwitch(t, opts.operatorDir, "joint", boostSession(1).Add(-2*time.Hour), true)
	at := boostSession(1).Add(-time.Hour)
	err := feed.trySend(event.KillSwitchEventType, event.KillSwitchSchemaVersion, "adapter:other-account", at,
		event.KillSwitchPayload{AccountID: "ira", AsOf: at, Engaged: true, Operator: "owner", Detail: "another account"})
	var protocolErr *transport.ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Sequence != feed.sequence+1 {
		t.Fatalf("err = %#v, want a refusal stating sequence %d", err, feed.sequence+1)
	}
	feed.sequence = protocolErr.Sequence + 1
	feed.sessions(1, 2)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// TestAResumeWithAnApprovalStatesOneNextSequence: a resume of a start cut
// short that records its approval states one sequence for the adapter's
// first input, the one after the approval.
func TestAResumeWithAnApprovalStatesOneNextSequence(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	firstLog := recoverFile(t, first.logPath)
	latch, _ := lastLatch(t, firstLog.Entries)
	cut := configurationOnlyLog(t, dir, "run-2.log", laterAsOf, time.Now().Add(time.Hour))

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumed.resumePath, resumed.asOf = cut, laterAsOf
	resumed.approvalPath = writeApproval(t, dir, "run-1.log", firstLog.FinalRecordHash, latch)
	out := &strings.Builder{}
	stop = startEngineInvocation(t, func(ctx context.Context, w io.Writer) error { return run(ctx, resumed, io.MultiWriter(w, out)) })
	if err := stop(); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	if got := strings.Count(out.String(), "carries sequence"); got != 1 || !strings.Contains(out.String(), fmt.Sprintf("carries sequence %d", approvalSequence+1)) {
		t.Errorf("the resume printed %q, want one next sequence, %d", out.String(), approvalSequence+1)
	}
}

// recordsAfterTheFirstSession is how many records the durable log of a
// Boost run with an empty inbox holds once the adapter has sent its first
// session.
func recordsAfterTheFirstSession(t *testing.T) int {
	t.Helper()
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	n := len(recoverFile(t, opts.logPath).Entries)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	return n
}

// withRecordLimit bounds the durable log of every run the test starts at
// limit records.
func withRecordLimit(t *testing.T, limit int) {
	t.Helper()
	original := durableRecordLimit
	durableRecordLimit = limit
	t.Cleanup(func() { durableRecordLimit = original })
}

// TestARecordTheLogRefusesIsLeftForTheNextStart: a record the recorder
// refuses of its own, here at its record bound (ADR 0017), was never
// appended to the log, so it is not delivered: no marker or note is written
// beside it, the run stops with the error, and the next start delivers it,
// so a kill switch is never dropped across the restart a full log forces.
func TestARecordTheLogRefusesIsLeftForTheNextStart(t *testing.T) {
	withRecordLimit(t, recordsAfterTheFirstSession(t))
	dir := t.TempDir()
	first := inboxOptions(t, dir)
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	engaged := writeKillSwitch(t, first.operatorDir, "joint", boostSession(1).Add(-time.Hour), true)
	at := boostSession(1)
	if err := feed.trySend(event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "mark:full", at,
		event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: boostClose(1), Source: event.AdapterIBKR}); err == nil {
		t.Fatal("an input decided after its operator record was refused by the log")
	}
	_ = feed.client.Close()
	if err := stop(); err == nil || !strings.Contains(err.Error(), engaged.ID) {
		t.Fatalf("the run ended with %v, want it stopped by the operator record %s", err, engaged.ID)
	}
	for _, suffix := range []string{deliveredSuffix, refusedSuffix} {
		if _, err := os.Stat(filepath.Join(first.operatorDir, engaged.FileName()+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a %s file was written beside a record the log never took: %v", suffix, err)
		}
	}

	withRecordLimit(t, journal.DefaultMaxRecords)
	next := boostOptions(t, t.TempDir())
	next.operatorDir = first.operatorDir
	stop = startEngine(t, next)
	feed = newBoostFeed(t, next.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 1)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if inputs := operatorInputs(recoverFile(t, next.logPath).Entries); len(inputs) != 1 || inputs[0].ID != engaged.ID {
		t.Errorf("the next start's log holds operator inputs %+v, want the engagement", inputs)
	}
}

// TestAFailedDeliveryStatesTheLastSequenceRecorded: when a delivery fails
// after an earlier record was journalled, the refusal states, in its
// Sequence, the last sequence recorded, that of the journalled record, and
// says the adapter's input was not recorded, never that it was applied. The
// run stops on it, so the wire may not deliver the refusal; the decider is
// driven directly.
func TestAFailedDeliveryStatesTheLastSequenceRecorded(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	accepting := replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil })
	recorder := journal.NewBoundedRecorder(accepting, 2)
	engine, err := replay.New(recorder)
	if err != nil {
		t.Fatal(err)
	}
	record := func(at time.Time) operator.Record {
		r, err := operator.KillSwitch(event.KillSwitchPayload{AccountID: "joint", AsOf: at, Engaged: true, Operator: "owner", Detail: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	at := time.Date(2024, time.January, 2, 15, 0, 0, 0, time.UTC)
	if _, err := engine.Apply(context.Background(), record(at).Envelope(1, "test", hash)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, r := range []operator.Record{record(at.Add(time.Hour)), record(at.Add(2 * time.Hour))} {
		if err := os.WriteFile(filepath.Join(dir, r.FileName()), r.Encode(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inbox := &operatorInbox{
		dir: dir, delivered: map[string]bool{}, settled: map[string]bool{}, decoded: map[string]operator.Record{},
		recorder: recorder, out: io.Discard, strategyVersion: "test", configurationHash: hash,
	}
	adapter := record(at.Add(3*time.Hour)).Envelope(2, "test", hash)
	adapter.ID, adapter.Source = "adapter:unrecorded", "test-adapter"
	_, err = newDecider(&wireGuard{}, engine, recorder, "test", hash, inbox)(context.Background(), adapter)
	var sequenced *transport.SequencedError
	if !errors.As(err, &sequenced) || sequenced.Sequence != 2 {
		t.Fatalf("err = %#v, want a refusal stating sequence 2, the journalled record's", err)
	}
	if msg := err.Error(); strings.Contains(msg, "applied at") || !strings.Contains(msg, "adapter:unrecorded was not recorded") {
		t.Errorf("the refusal says %q, want it to say the input was not recorded", msg)
	}
}

// TestAnInputMovedByAnOperatorInputIsAnsweredAgain: an operator input
// delivered before the adapter's input moves that input one sequence later
// than it was sent at. Sent again as it was first sent, it is the adapter
// asking for its committed reply, and gets it byte for byte; no operator
// record is delivered before it, and nothing is recorded (ADR 0027,
// amendment "re-delivery on request").
func TestAnInputMovedByAnOperatorInputIsAnsweredAgain(t *testing.T) {
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	first := ""
	feed.beforeClose = func(int) {
		first = writeKillSwitch(t, opts.operatorDir, "joint", boostSession(0).Add(-time.Hour), true).ID
	}
	feed.sessions(0, 1)
	if feed.lastReply.Sequence != feed.last.Sequence+1 {
		t.Fatalf("the close was sent at %d and recorded at %d, want it moved by the operator input", feed.last.Sequence, feed.lastReply.Sequence)
	}
	writeKillSwitch(t, opts.operatorDir, "joint", boostSession(0).Add(-time.Minute), false)
	again, err := feed.client.Decide(context.Background(), feed.last)
	if err != nil {
		t.Fatalf("ask again for the committed reply to %s: %v", feed.last.ID, err)
	}
	if want, got := wireBytes(t, feed.lastReply), wireBytes(t, again); !bytes.Equal(want, got) {
		t.Fatalf("the reply served again differs from the one recorded:\n first %s\n again %s", want, got)
	}
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the engine: %v", err)
	}
	if inputs := operatorInputs(recoverFile(t, opts.logPath).Entries); len(inputs) != 1 || inputs[0].ID != first {
		t.Errorf("the log holds operator inputs %+v, want only %s: an input sent again delivers nothing", inputs, first)
	}
}

// directInbox is an operator inbox in a fresh directory over recorder, for
// tests that drive the decider directly.
func directInbox(t *testing.T, recorder *journal.Recorder, hash string) *operatorInbox {
	t.Helper()
	return &operatorInbox{
		dir: t.TempDir(), delivered: map[string]bool{}, settled: map[string]bool{}, decoded: map[string]operator.Record{},
		recorder: recorder, out: io.Discard, strategyVersion: "test", configurationHash: hash,
	}
}

// adapterInput is an adapter's input under id at sequence, for tests that
// drive the decider directly.
func adapterInput(t *testing.T, id string, sequence uint64, hash string) event.Envelope {
	t.Helper()
	r, err := operator.KillSwitch(event.KillSwitchPayload{AccountID: "joint", AsOf: time.Date(2024, time.January, 2, 15, 0, 0, 0, time.UTC), Engaged: true, Operator: "owner", Detail: id})
	if err != nil {
		t.Fatal(err)
	}
	in := r.Envelope(sequence, "test", hash)
	in.ID, in.Source = id, "test-adapter"
	return in
}

// TestAnInputSentAgainIsAnsweredOnlyAtTheSequenceItWasSentWith: an input
// sent again is answered from its committed group only at the sequence it
// was recorded at or the one it was sent with, which the recorder holds
// beside the group. At any other sequence it
// is another input under a committed ID and is refused, with an inbox
// configured or not, moved or not (ADR 0027, amendment "re-delivery on
// request").
func TestAnInputSentAgainIsAnsweredOnlyAtTheSequenceItWasSentWith(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	guard, engine, recorder := newTestWiring(t, replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	inbox := directInbox(t, recorder, hash)
	decide := newDecider(guard, engine, recorder, "test", hash, inbox)
	for n := uint64(1); n <= 3; n++ {
		if _, err := decide(context.Background(), adapterInput(t, fmt.Sprintf("adapter:%d", n), n, hash)); err != nil {
			t.Fatal(err)
		}
	}
	refusedAt := func(id string, sequence uint64) {
		t.Helper()
		if _, err := decide(context.Background(), adapterInput(t, id, sequence, hash)); err == nil || !strings.Contains(err.Error(), "committed with different content") {
			t.Errorf("%s sent again at %d: err = %v, want it refused", id, sequence, err)
		}
	}
	answeredAt := func(id string, sequence uint64) {
		t.Helper()
		if _, err := decide(context.Background(), adapterInput(t, id, sequence, hash)); err != nil {
			t.Errorf("%s sent again at %d: %v, want its committed reply", id, sequence, err)
		}
	}
	answeredAt("adapter:3", 3)
	refusedAt("adapter:3", 1)

	r, err := operator.KillSwitch(event.KillSwitchPayload{AccountID: "joint", AsOf: time.Date(2024, time.January, 2, 16, 0, 0, 0, time.UTC), Engaged: true, Operator: "owner", Detail: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox.dir, r.FileName()), r.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	reply, err := decide(context.Background(), adapterInput(t, "adapter:4", 4, hash))
	if err != nil || reply.Sequence != 5 {
		t.Fatalf("adapter:4 sent at 4: reply at %d, %v; want it recorded at 5, after the operator input", reply.Sequence, err)
	}
	answeredAt("adapter:4", 4)
	answeredAt("adapter:4", 5)
	refusedAt("adapter:4", 3)
	refusedAt("adapter:4", 6)
	if got := len(recorder.Entries()); got != 5 {
		t.Errorf("the recorder holds %d records, want 5: an input sent again records nothing", got)
	}
}

// TestARecorderRefusalAfterOperatorInputsStopsTheRun: when operator inputs
// were recorded before the adapter's input and the recorder then refuses
// that input of its own (here its emission ceiling, ADR 0017), the sequence
// the input was given is spent but not in the log, so the run stops on it,
// once the refusal is written: the refusal states the last operator input's
// sequence, every later input is refused with the same cause, and the log,
// missing no sequence, resumes.
func TestARecorderRefusalAfterOperatorInputsStopsTheRun(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	burst := make([]event.Envelope, journal.MaxEmissionsPerInput+1)
	for i := range burst {
		burst[i] = stubEnvelope(fmt.Sprintf("burst-%d", i))
	}
	handler := replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.ID == "adapter:2" {
			return burst, nil
		}
		return nil, nil
	})
	path := filepath.Join(t.TempDir(), "run-1.log")
	engine, recorder, err := durableWiring(t, path, hash, handler, nil)
	if err != nil {
		t.Fatal(err)
	}
	inbox := directInbox(t, recorder, hash)
	var stopped error
	guard := &wireGuard{stop: func(cause error) { stopped = cause }}
	decide := newDecider(guard, engine, recorder, "test", hash, inbox)
	if _, err := decide(context.Background(), adapterInput(t, "adapter:1", 1, hash)); err != nil {
		t.Fatal(err)
	}
	writeOperatorRecord(t, inbox.dir, time.Date(2024, time.January, 2, 16, 0, 0, 0, time.UTC))
	_, err = decide(context.Background(), adapterInput(t, "adapter:2", 2, hash))
	var limit *journal.EmissionLimitError
	var sequenced *transport.SequencedError
	if !errors.As(err, &limit) || !errors.As(err, &sequenced) || sequenced.Sequence != 2 {
		t.Fatalf("err = %#v, want the emission ceiling's refusal stating sequence 2, the operator input's", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "adapter:2 was not recorded") || !strings.Contains(msg, "the run stops") {
		t.Errorf("the refusal says %q, want it to say the input was not recorded and the run stops", msg)
	}
	if stopped != nil {
		t.Errorf("the run was stopped with %v before the refusal was written", stopped)
	}
	guard.afterReply()
	if !errors.As(stopped, &limit) || !errors.As(guard.failure(), &limit) {
		t.Errorf("the run was stopped with %v (failure %v), want the emission ceiling's refusal", stopped, guard.failure())
	}
	if _, err := decide(context.Background(), adapterInput(t, "adapter:3", sequenced.Sequence+1, hash)); !errors.As(err, &limit) {
		t.Errorf("the next input: err = %v, want it refused with the cause the run stopped on", err)
	}
	recovery := recoverFile(t, path)
	if _, _, err := durableWiring(t, filepath.Join(t.TempDir(), "run-2.log"), hash, handler, &recovery); err != nil {
		t.Errorf("resume the stopped run's log: %v", err)
	}
}

// burstHandler decides more than the recorder takes from one input
// (journal.MaxEmissionsPerInput, ADR 0017) for the input named id, and
// nothing for any other.
func burstHandler(id string) replay.Handler {
	burst := make([]event.Envelope, journal.MaxEmissionsPerInput+1)
	for i := range burst {
		burst[i] = stubEnvelope(fmt.Sprintf("burst-%d", i))
	}
	return replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.ID == id {
			return burst, nil
		}
		return nil, nil
	})
}

// TestARefusalThatStopsTheRunReachesTheAdapterFirst: a refusal that stops
// the run is written to the adapter, with the sequence it states, before
// serving ends, and the next input is refused with the cause the run
// stopped on.
func TestARefusalThatStopsTheRunReachesTheAdapterFirst(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	engine, recorder, err := durableWiring(t, filepath.Join(t.TempDir(), "run-1.log"), hash, burstHandler("adapter:2"), nil)
	if err != nil {
		t.Fatal(err)
	}
	inbox := directInbox(t, recorder, hash)
	serveCtx, stopServing := context.WithCancelCause(t.Context())
	defer stopServing(nil)
	guard := &wireGuard{stop: stopServing}
	decide := newDecider(guard, engine, recorder, "test", hash, inbox)
	path := filepath.Join(shortSocketDir(t), "engine.sock")
	server, err := transport.Listen(path, decide, transport.ServerConfig{MaxConnections: 1, AfterReply: guard.afterReply})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.ServeContext(serveCtx) }()
	client, err := transport.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	exchange, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if _, err := client.Decide(exchange, adapterInput(t, "adapter:1", 1, hash)); err != nil {
		t.Fatal(err)
	}
	writeOperatorRecord(t, inbox.dir, time.Date(2024, time.January, 2, 16, 0, 0, 0, time.UTC))
	_, err = client.Decide(exchange, adapterInput(t, "adapter:2", 2, hash))
	var protocolErr *transport.ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != transport.CodeDeciderFailed || protocolErr.Sequence != 2 {
		t.Fatalf("err = %#v, want the refusal stating sequence 2, the operator input's", err)
	}
	select {
	case <-served:
	case <-exchange.Done():
		t.Fatal("serving did not end after the refusal that stops the run")
	}
	var limit *journal.EmissionLimitError
	if !errors.As(context.Cause(serveCtx), &limit) {
		t.Errorf("serving ended with %v, want the emission ceiling's refusal", context.Cause(serveCtx))
	}
	if _, err := decide(context.Background(), adapterInput(t, "adapter:3", 3, hash)); !errors.As(err, &limit) {
		t.Errorf("the next input: err = %v, want it refused with the cause the run stopped on", err)
	}
}

// TestARefusalThatStopsTheRunStopsItWhenItsReplyCannotBeWritten: an
// adapter gone before the refusal that stops the run is written still has
// the run stop, with the cause, instead of leaving it poisoned and serving.
func TestARefusalThatStopsTheRunStopsItWhenItsReplyCannotBeWritten(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	engine, recorder, err := durableWiring(t, filepath.Join(t.TempDir(), "run-1.log"), hash, burstHandler("adapter:1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServing := context.WithCancelCause(t.Context())
	defer stopServing(nil)
	guard := &wireGuard{stop: stopServing, stopGrace: time.Hour}
	decide := newDecider(guard, engine, recorder, "test", hash, nil)
	reached, closed := make(chan struct{}), make(chan struct{})
	gated := func(ctx context.Context, in event.Envelope) (event.Envelope, error) {
		close(reached)
		<-closed
		return decide(ctx, in)
	}
	path := filepath.Join(shortSocketDir(t), "engine.sock")
	server, err := transport.Listen(path, gated, transport.ServerConfig{MaxConnections: 1, AfterReply: guard.afterReply})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.ServeContext(serveCtx) }()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(adapterInput(t, "adapter:1", 1, hash))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		t.Fatal(err)
	}
	<-reached
	_ = conn.Close()
	close(closed)

	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("serving did not end after a refusal that stops the run could not be written")
	}
	var limit *journal.EmissionLimitError
	if !errors.As(context.Cause(serveCtx), &limit) {
		t.Errorf("serving ended with %v, want the emission ceiling's refusal", context.Cause(serveCtx))
	}
}

// TestACauseRecordedAfterItsReplyStillStopsTheRun: a decision transport
// abandoned at its deadline that then records a cause the run stops on has
// no reply left to follow, and an adapter that stays connected and idle
// sends none; the run still stops, with the cause, within the guard's
// grace.
func TestACauseRecordedAfterItsReplyStillStopsTheRun(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	engine, recorder, err := durableWiring(t, filepath.Join(t.TempDir(), "run-1.log"), hash, burstHandler("adapter:1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServing := context.WithCancelCause(t.Context())
	defer stopServing(nil)
	guard := &wireGuard{stop: stopServing, stopGrace: 50 * time.Millisecond}
	decide := newDecider(guard, engine, recorder, "test", hash, nil)
	finished := make(chan struct{})
	late := func(ctx context.Context, in event.Envelope) (event.Envelope, error) {
		<-ctx.Done()
		// Stands in for a decision that finishes after transport gave up on
		// it, once its timeout reply has been written.
		time.Sleep(50 * time.Millisecond)
		defer close(finished)
		return decide(context.Background(), in)
	}
	path := filepath.Join(shortSocketDir(t), "engine.sock")
	server, err := transport.Listen(path, late, transport.ServerConfig{MaxConnections: 1, DecisionTimeout: 50 * time.Millisecond, AfterReply: guard.afterReply})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.ServeContext(serveCtx) }()
	client, err := transport.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	exchange, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var protocolErr *transport.ProtocolError
	if _, err := client.Decide(exchange, adapterInput(t, "adapter:1", 1, hash)); !errors.As(err, &protocolErr) || protocolErr.Code != transport.CodeTimeout {
		t.Fatalf("err = %#v, want the decision's timeout", err)
	}
	<-finished
	select {
	case <-served:
	case <-exchange.Done():
		t.Fatal("serving did not end after a cause recorded once its reply was written")
	}
	var limit *journal.EmissionLimitError
	if !errors.As(context.Cause(serveCtx), &limit) {
		t.Errorf("serving ended with %v, want the emission ceiling's refusal", context.Cause(serveCtx))
	}
}

// TestAnEmissionCeilingRefusalWithoutOperatorInputsStopsTheRun: the
// recorder refusing an input no operator input moved (its emission
// ceiling, ADR 0017) still spends the input's sequence, so the run stops
// on it whether an inbox is configured or not: the next input is refused
// with the same cause, and the log, missing no sequence, resumes.
func TestAnEmissionCeilingRefusalWithoutOperatorInputsStopsTheRun(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for _, withInbox := range []bool{false, true} {
		t.Run(fmt.Sprintf("inbox=%v", withInbox), func(t *testing.T) {
			handler := burstHandler("adapter:2")
			path := filepath.Join(t.TempDir(), "run-1.log")
			engine, recorder, err := durableWiring(t, path, hash, handler, nil)
			if err != nil {
				t.Fatal(err)
			}
			var inbox *operatorInbox
			if withInbox {
				inbox = directInbox(t, recorder, hash)
			}
			guard := &wireGuard{}
			decide := newDecider(guard, engine, recorder, "test", hash, inbox)
			if _, err := decide(context.Background(), adapterInput(t, "adapter:1", 1, hash)); err != nil {
				t.Fatal(err)
			}
			_, err = decide(context.Background(), adapterInput(t, "adapter:2", 2, hash))
			var limit *journal.EmissionLimitError
			if !errors.As(err, &limit) || !strings.Contains(err.Error(), "the run stops") {
				t.Fatalf("err = %v, want the emission ceiling's refusal, saying the run stops", err)
			}
			if !errors.As(guard.failure(), &limit) {
				t.Errorf("the run's failure is %v, want the emission ceiling's refusal", guard.failure())
			}
			if _, err := decide(context.Background(), adapterInput(t, "adapter:3", 3, hash)); !errors.As(err, &limit) {
				t.Errorf("the next input: err = %v, want it refused with the cause the run stopped on", err)
			}
			recovery := recoverFile(t, path)
			if _, _, err := durableWiring(t, filepath.Join(t.TempDir(), "run-2.log"), hash, handler, &recovery); err != nil {
				t.Errorf("resume the stopped run's log: %v", err)
			}
		})
	}
}

// TestARecoveredInputSentAgainWhileAnotherIsDecidedDoesNotRace: checking a
// re-sent input recovered from the log against the operator inputs before
// it reads the recorder, which a concurrent decision writes; under the race
// detector the two must never overlap.
func TestARecoveredInputSentAgainWhileAnotherIsDecidedDoesNotRace(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	accepting := replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil })
	first := filepath.Join(t.TempDir(), "run-1.log")
	engine, _, err := durableWiring(t, first, hash, accepting, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []event.Envelope{
		adapterInput(t, "adapter:1", 1, hash),
		operatorRecord(t, time.Date(2024, time.January, 2, 16, 0, 0, 0, time.UTC)).Envelope(2, "test", hash),
		adapterInput(t, "adapter:2", 3, hash),
	} {
		if _, err := engine.Apply(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	recovery := recoverFile(t, first)
	engine, recorder, err := durableWiring(t, filepath.Join(t.TempDir(), "run-2.log"), hash, accepting, &recovery)
	if err != nil {
		t.Fatal(err)
	}
	decide := newDecider(&wireGuard{}, engine, recorder, "test", hash, nil)
	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range rounds {
			if _, err := decide(context.Background(), adapterInput(t, "adapter:2", 2, hash)); err != nil {
				t.Errorf("adapter:2 sent again at 2: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for n := range uint64(rounds) {
			if _, err := decide(context.Background(), adapterInput(t, fmt.Sprintf("adapter:%d", n+4), n+4, hash)); err != nil {
				t.Errorf("adapter:%d: %v", n+4, err)
				return
			}
		}
	}()
	wg.Wait()
}

// TestAResumedLogEndingInOperatorInputsAnswersTheNextInput: a log whose
// last inputs are operator inputs (its run stopped after delivering them)
// resumes, and the adapter's next input, moved by an operator input this
// exchange delivers, is answered with its committed reply. Sent again, it
// is answered at the sequence it was sent with or the one it was recorded
// at, and at no sequence the operator inputs of the earlier run stand at;
// after a further restart it is still answered at the sequence it was sent
// with (ADR 0027, amendment "re-delivery on request").
func TestAResumedLogEndingInOperatorInputsAnswersTheNextInput(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	accepting := replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil })
	first := filepath.Join(t.TempDir(), "run-1.log")
	engine, _, err := durableWiring(t, first, hash, accepting, nil)
	if err != nil {
		t.Fatal(err)
	}
	logOperatorTail(t, engine, hash)

	recovery := recoverFile(t, first)
	second := filepath.Join(t.TempDir(), "run-2.log")
	engine, recorder, err := durableWiring(t, second, hash, accepting, &recovery)
	if err != nil {
		t.Fatal(err)
	}
	inbox := directInbox(t, recorder, hash)
	writeOperatorRecord(t, inbox.dir, time.Date(2024, time.January, 2, 18, 0, 0, 0, time.UTC))
	decide := newDecider(&wireGuard{}, engine, recorder, "test", hash, inbox)
	reply, err := decide(context.Background(), adapterInput(t, "adapter:4", 4, hash))
	if err != nil || reply.Sequence != 5 {
		t.Fatalf("adapter:4 sent at 4: reply at %d, %v; want its committed reply, recorded at 5 after the operator input", reply.Sequence, err)
	}
	answeredAt(t, decide, "adapter:4", 4, hash)
	answeredAt(t, decide, "adapter:4", 5, hash)
	refusedAt(t, decide, "adapter:4", 2, hash)
	refusedAt(t, decide, "adapter:4", 3, hash)
	refusedAt(t, decide, "adapter:4", 6, hash)

	recovery = recoverFile(t, second)
	engine, recorder, err = durableWiring(t, filepath.Join(t.TempDir(), "run-3.log"), hash, accepting, &recovery)
	if err != nil {
		t.Fatal(err)
	}
	decide = newDecider(&wireGuard{}, engine, recorder, "test", hash, nil)
	answeredAt(t, decide, "adapter:4", 4, hash)
	answeredAt(t, decide, "adapter:4", 5, hash)
	refusedAt(t, decide, "adapter:4", 1, hash)
	refusedAt(t, decide, "adapter:4", 6, hash)
}

// TestAnInputSentAgainIsCheckedAgainstTheSequenceItWasSentWith: operator
// inputs a resumed log ends in did not move the adapter's next input, sent
// at the sequence after them, so sent again it is answered only at that
// sequence: a re-send at its sequence less their count is another input
// under a committed ID, refused, and nothing is recorded (ADR 0027,
// amendment "re-delivery on request").
func TestAnInputSentAgainIsCheckedAgainstTheSequenceItWasSentWith(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	accepting := replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil })
	first := filepath.Join(t.TempDir(), "run-1.log")
	engine, _, err := durableWiring(t, first, hash, accepting, nil)
	if err != nil {
		t.Fatal(err)
	}
	logOperatorTail(t, engine, hash)

	recovery := recoverFile(t, first)
	engine, recorder, err := durableWiring(t, filepath.Join(t.TempDir(), "run-2.log"), hash, accepting, &recovery)
	if err != nil {
		t.Fatal(err)
	}
	decide := newDecider(&wireGuard{}, engine, recorder, "test", hash, directInbox(t, recorder, hash))
	reply, err := decide(context.Background(), adapterInput(t, "adapter:4", 4, hash))
	if err != nil || reply.Sequence != 4 {
		t.Fatalf("adapter:4 sent at 4: reply at %d, %v; want it recorded at 4", reply.Sequence, err)
	}
	answeredAt(t, decide, "adapter:4", 4, hash)
	refusedAt(t, decide, "adapter:4", 2, hash)
	refusedAt(t, decide, "adapter:4", 3, hash)
	if got := len(recorder.Entries()); got != 4 {
		t.Errorf("the recorder holds %d records, want 4: an input sent again records nothing", got)
	}
}

// durableWiring is an engine over a durable recorder logging to a new file
// at path, resumed from recovered when it is not nil.
func durableWiring(t *testing.T, path, hash string, handler replay.Handler, recovered *journal.Recovery) (*replay.Engine, *journal.Recorder, error) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	log, err := journal.CreateLog(file, journal.NewLogHeader(hash, "test"))
	if err != nil {
		t.Fatal(err)
	}
	recorder := journal.NewDurableRecorder(handler, log, journal.DefaultMaxRecords)
	engine, err := replay.New(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != nil {
		err = journal.Resume(context.Background(), replay.HandlerFunc(engine.Apply), recorder, *recovered)
	}
	return engine, recorder, err
}

// logOperatorTail records an adapter's input at 1 and operator inputs at 2
// and 3 through engine, as a run that stopped after delivering them leaves
// its log.
func logOperatorTail(t *testing.T, engine *replay.Engine, hash string) {
	t.Helper()
	if _, err := engine.Apply(context.Background(), adapterInput(t, "adapter:1", 1, hash)); err != nil {
		t.Fatal(err)
	}
	for n, at := range []time.Time{time.Date(2024, time.January, 2, 16, 0, 0, 0, time.UTC), time.Date(2024, time.January, 2, 17, 0, 0, 0, time.UTC)} {
		if _, err := engine.Apply(context.Background(), operatorRecord(t, at).Envelope(uint64(n)+2, "test", hash)); err != nil {
			t.Fatal(err)
		}
	}
}

// operatorRecord is the owner's kill switch for the joint account at at.
func operatorRecord(t *testing.T, at time.Time) operator.Record {
	t.Helper()
	r, err := operator.KillSwitch(event.KillSwitchPayload{AccountID: "joint", AsOf: at, Engaged: true, Operator: "owner", Detail: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// writeOperatorRecord writes operatorRecord(at) into the inbox dir.
func writeOperatorRecord(t *testing.T, dir string, at time.Time) {
	t.Helper()
	r := operatorRecord(t, at)
	if err := os.WriteFile(filepath.Join(dir, r.FileName()), r.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// answeredAt asserts that decide answers id sent again at sequence.
func answeredAt(t *testing.T, decide transport.Decider, id string, sequence uint64, hash string) {
	t.Helper()
	if _, err := decide(context.Background(), adapterInput(t, id, sequence, hash)); err != nil {
		t.Errorf("%s sent again at %d: %v, want its committed reply", id, sequence, err)
	}
}

// refusedAt asserts that decide refuses id sent again at sequence as
// another input under a committed ID.
func refusedAt(t *testing.T, decide transport.Decider, id string, sequence uint64, hash string) {
	t.Helper()
	if _, err := decide(context.Background(), adapterInput(t, id, sequence, hash)); err == nil || !strings.Contains(err.Error(), "committed with different content") {
		t.Errorf("%s sent again at %d: err = %v, want it refused", id, sequence, err)
	}
}

// TestAMovedInputSentAgainChangedOrLaterIsRefused: with an inbox, an input
// an operator input moved is answered again only as it was sent. Sent again
// with different content at the sequence it was sent with, or unchanged at a
// later sequence, it is another input under a committed ID, refused, and
// nothing is recorded (ADR 0027, amendment "re-delivery on request": an
// input sent under a committed input's ID with different content is
// refused).
func TestAMovedInputSentAgainChangedOrLaterIsRefused(t *testing.T) {
	dir := t.TempDir()
	opts := inboxOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.beforeClose = func(int) {
		writeKillSwitch(t, opts.operatorDir, "joint", boostSession(0).Add(-time.Hour), true)
	}
	feed.sessions(0, 1)
	if feed.lastReply.Sequence != feed.last.Sequence+1 {
		t.Fatalf("the close was sent at %d and recorded at %d, want it moved by the operator input", feed.last.Sequence, feed.lastReply.Sequence)
	}
	var payload map[string]any
	if err := json.Unmarshal(feed.last.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["instrument_ids"] = []string{"QQQ"}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	changed := feed.last
	changed.Payload, changed.PayloadHash = raw, event.HashPayload(raw)
	later := feed.last
	later.Sequence = feed.lastReply.Sequence + 1
	for name, in := range map[string]event.Envelope{"with different content": changed, "at a later sequence": later} {
		if _, err := feed.client.Decide(context.Background(), in); err == nil || !strings.Contains(err.Error(), "committed with different content") {
			t.Errorf("%s sent again %s: err = %v, want it refused", in.ID, name, err)
		}
	}
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the engine: %v", err)
	}
	entries := recoverFile(t, opts.logPath).Entries
	var inputs []string
	for _, e := range entries {
		if e.Kind == journal.KindInput {
			inputs = append(inputs, e.Envelope.ID)
		}
	}
	if n := len(inputs); n == 0 || inputs[n-1] != feed.last.ID || slices.Index(inputs, feed.last.ID) != n-1 {
		t.Errorf("the log's inputs are %v, want %s once and last: a refused input records nothing", inputs, feed.last.ID)
	}
}

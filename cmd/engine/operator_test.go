package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/operator"
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

package targetweight_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// feedRun builds a handler for cfg and feeds it the configuration then
// inputs, numbering them, and returns the first error with the input's ID.
func feedRun(t *testing.T, cfg event.TargetWeightConfigurationPayload, decideFrom time.Time, inputs []event.Envelope) (string, error) {
	t.Helper()
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: decideFrom})
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{hash: event.TargetWeightConfigurationHash(cfg)}
	all := append([]event.Envelope{s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", decideFrom, cfg)}, inputs...)
	for i, in := range all {
		in.Sequence = uint64(i + 1)
		if _, err := h.Apply(context.Background(), in); err != nil {
			return in.ID, err
		}
	}
	return "", nil
}

// heldRestart returns the inputs the last of the restarts after the inputs
// afters feeds.
func heldRestart(t *testing.T, afters ...string) []event.Envelope {
	t.Helper()
	r := simulateMomentum(t, momentumOptions{restartAfter: afters})
	inputs, err := targetweight.RestartInputs(entryEnvelopes(r.earlier[len(r.earlier)-1]), targetweight.Options{DecideFrom: closeOf("2025-01-02")}, r.restartAt)
	if err != nil {
		t.Fatal(err)
	}
	return inputs
}

func withPayload(t *testing.T, e event.Envelope, payload any) event.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	e.Payload, e.PayloadHash = raw, event.HashPayload(raw)
	return e
}

func runOptions(t *testing.T, at time.Time, n int) event.Envelope {
	t.Helper()
	s := &stream{hash: event.TargetWeightConfigurationHash(momentumConfiguration())}
	return s.envelope(t, event.RunOptionsEventType, event.RunOptionsSchemaVersion, "run-options", at, event.RunOptionsPayload{DecideFrom: closeOf("2025-01-02"), RecoveredRecords: n})
}

func TestMomentumRestartInputRules(t *testing.T) {
	decideFrom := closeOf("2025-01-02")
	inputs := heldRestart(t, "session:2025-02-28")
	index := func(id string) int {
		for i, e := range inputs {
			if e.ID == id {
				return i
			}
		}
		t.Fatalf("no input %s", id)
		return -1
	}
	clone := func() []event.Envelope { return append([]event.Envelope(nil), inputs...) }
	cleared := func(decision string) event.Envelope {
		s := &stream{hash: event.TargetWeightConfigurationHash(momentumConfiguration())}
		at := closeOf("2025-03-03")
		return s.envelope(t, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared", at,
			event.RebalanceClearedPayload{AccountID: iraAccount, DecisionID: decision, Action: event.RebalanceResume, ClearedAt: at})
	}
	var options event.RunOptionsPayload
	decode(t, inputs[0], &options)

	for _, c := range []struct {
		name   string
		inputs func() []event.Envelope
		at     string
		want   string
	}{
		{"options after another input", func() []event.Envelope {
			in := clone()
			return append([]event.Envelope{in[index("calendar")]}, in...)
		}, inputs[0].ID, "straight after the configuration"},
		{"options for another first session", func() []event.Envelope {
			in := clone()
			p := options
			p.DecideFrom = closeOf("2025-01-03")
			in[0] = withPayload(t, in[0], p)
			return in
		}, inputs[0].ID, "built to decide from"},
		{"a record of another configuration", func() []event.Envelope {
			in := clone()
			in[1].ConfigurationHash = "sha256:other"
			return in
		}, inputs[1].ID, "another configuration"},
		{"a plan before its decision", func() []event.Envelope {
			in := clone()
			at := index("order-planned:" + febSell("AAA"))
			return append([]event.Envelope{in[0], in[at]}, in[1:]...)
		}, "order-planned:" + febSell("AAA"), "not the rebalance recovered"},
		{"a release with no plan", func() []event.Envelope {
			in := clone()
			at := index("risk-order-released:" + febSell("AAA"))
			return append([]event.Envelope{in[0], in[at]}, in[1:]...)
		}, "risk-order-released:" + febSell("AAA"), "no recorded plan"},
		{"a bar in the prelude", func() []event.Envelope {
			in := clone()
			at := index("bar:AAA:2025-01-02")
			return append([]event.Envelope{in[0], in[at]}, in[1:]...)
		}, "bar:AAA:2025-01-02", "not a record a momentum restart reads"},
		{"a clearing of another decision", func() []event.Envelope {
			return append(clone(), cleared("targets-decided:ira:2025-01-31T21:00:00Z"))
		}, "cleared", "not a held rebalance"},
		{"a history that does not give the recorded sells", func() []event.Envelope {
			in := clone()
			at := index("snapshot:2025-02-28")
			var p event.AccountCashSnapshotPayload
			decode(t, in[at], &p)
			p.NetLiquidation += 500
			in[at] = withPayload(t, in[at], p)
			return in
		}, "session:2025-02-28", "does not give the sells recorded"},
		{"a history without the decision's snapshot", func() []event.Envelope {
			in := clone()
			at := index("snapshot:2025-02-28")
			return append(in[:at], in[at+1:]...)
		}, "session:2025-02-28", "no account snapshot as of"},
	} {
		t.Run(c.name, func(t *testing.T) {
			at, err := feedRun(t, momentumConfiguration(), decideFrom, c.inputs())
			if err == nil || at != c.at || !strings.Contains(err.Error(), c.want) {
				t.Errorf("at %q: %v, want %q at %q", at, err, c.want, c.at)
			}
		})
	}

	// A clearing in a run with nothing held is refused.
	if _, err := feedRun(t, momentumConfiguration(), decideFrom, []event.Envelope{cleared(febDecision)}); err == nil || !strings.Contains(err.Error(), "not a held rebalance") {
		t.Errorf("clearing with nothing held: %v", err)
	}
	// Run options twice are refused.
	if _, err := feedRun(t, momentumConfiguration(), decideFrom, []event.Envelope{runOptions(t, decideFrom, 0), runOptions(t, decideFrom, 0)}); err == nil || !strings.Contains(err.Error(), "come once") {
		t.Errorf("run options twice: %v", err)
	}
}

// TestAResumeWithoutTheReplayedHistoryIsRefused restarts with the sells
// filled but feeds none of the earlier market inputs, so the buy phase has
// nothing to be planned from: the clearing is refused rather than planning
// from anything else.
func TestAResumeWithoutTheReplayedHistoryIsRefused(t *testing.T) {
	inputs := heldRestart(t, "session:2025-02-28", "fill:"+febSell("CCC"))
	var options event.RunOptionsPayload
	decode(t, inputs[0], &options)
	prelude := inputs[:1+options.RecoveredRecords]
	s := &stream{hash: event.TargetWeightConfigurationHash(momentumConfiguration())}
	at := closeOf("2025-03-03")
	resume := s.envelope(t, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared", at,
		event.RebalanceClearedPayload{AccountID: iraAccount, DecisionID: febDecision, Action: event.RebalanceResume, ClearedAt: at})
	id, err := feedRun(t, momentumConfiguration(), closeOf("2025-01-02"), append(append([]event.Envelope(nil), prelude...), resume))
	if err == nil || id != "cleared" || !strings.Contains(err.Error(), "cannot resume") {
		t.Errorf("at %q: %v", id, err)
	}
	// With the sells still working, the resume is accepted, and the buy
	// phase is refused when the last sell ends.
	working := heldRestart(t, "session:2025-02-28")
	decode(t, working[0], &options)
	r := simulateMomentum(t, momentumOptions{})
	var venue []event.Envelope
	for _, e := range r.venue {
		if strings.HasSuffix(e.ID, febSell("AAA")) || strings.HasSuffix(e.ID, febSell("CCC")) {
			venue = append(venue, e)
		}
	}
	feed := append(append(append([]event.Envelope(nil), working[:1+options.RecoveredRecords]...), resume), venue...)
	id, err = feedRun(t, momentumConfiguration(), closeOf("2025-01-02"), feed)
	if err == nil || id != "fill:"+febSell("CCC") || !strings.Contains(err.Error(), "never reached its decision session") {
		t.Errorf("at %q: %v", id, err)
	}
}

func TestRestartInputsRules(t *testing.T) {
	opts := targetweight.Options{DecideFrom: closeOf("2025-01-02")}
	r := simulateMomentum(t, momentumOptions{stopAfter: "2025-01-10"})
	earlier := entryEnvelopes(r.entries)
	at := closeOf("2025-01-10").Add(time.Hour)
	for _, c := range []struct {
		name    string
		earlier []event.Envelope
		opts    targetweight.Options
		at      time.Time
		want    string
	}{
		{"no first session", earlier, targetweight.Options{}, at, "first session"},
		{"no time", earlier, opts, time.Time{}, "its time"},
		{"no journal", nil, opts, at, "opens with its configuration"},
		{"no configuration first", earlier[1:], opts, at, "opens with its configuration"},
		{"configured twice", append(append([]event.Envelope(nil), earlier...), earlier[0]), opts, at, "configured twice"},
		{"another configuration", func() []event.Envelope {
			e := append([]event.Envelope(nil), earlier...)
			e[3].ConfigurationHash = "sha256:other"
			return e
		}(), opts, at, "another configuration"},
	} {
		if _, err := targetweight.RestartInputs(c.earlier, c.opts, c.at); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// A run that never restarted records no run options for replay.
	if _, err := targetweight.RecordedOptions(r.inputs); err == nil || !strings.Contains(err.Error(), "no run options") {
		t.Errorf("recorded options of a fresh run: %v", err)
	}
	// Restarting before any decision recovers no book and holds nothing.
	inputs, err := targetweight.RestartInputs(earlier, opts, at)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := feedRun(t, momentumConfiguration(), opts.DecideFrom, inputs); err != nil {
		t.Fatalf("at %s: %v", id, err)
	}
}

func TestBoostRestartInputRules(t *testing.T) {
	earlier := simulate(t, options{closes: syntheticCloses()[:5]})
	opts := targetweight.Options{DecideFrom: sessionEnd(0)}
	inputs, err := targetweight.RestartInputs(entryEnvelopes(earlier.entries), opts, sessionEnd(4).Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	bar := earlier.inputs[1]
	recovered := ofType(earlier.decisions, event.TargetsDecidedEventType)[4]
	recovered.CausationID, recovered.CorrelationID = "", ""
	for _, c := range []struct {
		name   string
		inputs []event.Envelope
		at     string
		want   string
	}{
		{"a bar in the prelude", append([]event.Envelope{inputs[0], bar}, inputs[1:]...), bar.ID, "not a record a Boost restart reads"},
		{"a recovered decision after the run options", []event.Envelope{withPayload(t, inputs[0], event.RunOptionsPayload{DecideFrom: sessionEnd(0)}), recovered}, recovered.ID, "in the restart prelude"},
		{"an engine state at an unknown version", func() []event.Envelope {
			e := ofType(simulate(t, options{noMark: map[int]bool{0: true}}).decisions, event.EngineStateEventType)[0]
			e.SchemaVersion = 9
			return []event.Envelope{withPayload(t, inputs[0], event.RunOptionsPayload{DecideFrom: sessionEnd(0), RecoveredRecords: 1}), e}
		}(), "", "not one this build reads"},
	} {
		t.Run(c.name, func(t *testing.T) {
			at, err := feedRun(t, testConfiguration(), sessionEnd(0), c.inputs)
			if err == nil || (c.at != "" && at != c.at) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("at %q: %v, want %q", at, err, c.want)
			}
		})
	}
	if id, err := feedRun(t, testConfiguration(), sessionEnd(0), inputs); err != nil {
		t.Fatalf("the restart inputs themselves fail at %s: %v", id, err)
	}
}

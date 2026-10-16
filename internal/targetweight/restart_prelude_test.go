package targetweight_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// edit returns e with its payload's field set to value.
func edit(t *testing.T, e event.Envelope, field string, value any) event.Envelope {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatal(err)
	}
	m[field] = value
	return withPayload(t, e, m)
}

// find returns the index of the input with ID id.
func find(t *testing.T, inputs []event.Envelope, id string) int {
	t.Helper()
	for i, e := range inputs {
		if e.ID == id {
			return i
		}
	}
	t.Fatalf("no input %s", id)
	return -1
}

// preludeCase changes one input of a restart's inputs and expects the
// restarted run to refuse it.
type preludeCase struct {
	name   string
	change func(t *testing.T, in []event.Envelope) []event.Envelope
	at     string
	want   string
}

func runPreludeCases(t *testing.T, cfg event.TargetWeightConfigurationPayload, decideFrom time.Time, inputs []event.Envelope, cases []preludeCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at, err := feedRun(t, cfg, decideFrom, c.change(t, append([]event.Envelope(nil), inputs...)))
			if err == nil || at != c.at || !strings.Contains(err.Error(), c.want) {
				t.Errorf("at %q: %v, want %q at %q", at, err, c.want, c.at)
			}
		})
	}
}

// TestMomentumPreludeRefusesRecordsThatDoNotFit feeds a restart's prelude
// with one record altered: each is refused where it is read, never taken as
// recovered state.
func TestMomentumPreludeRefusesRecordsThatDoNotFit(t *testing.T) {
	inputs := heldRestart(t, "session:2025-02-28")
	plannedAAA, releasedAAA := "order-planned:"+febSell("AAA"), "risk-order-released:"+febSell("AAA")
	boostDecision := ofType(simulate(t, options{closes: syntheticCloses()[:1]}).decisions, event.TargetsDecidedEventType)[0]
	at := closeOf("2025-03-03")
	s := &stream{hash: event.TargetWeightConfigurationHash(momentumConfiguration())}
	cleared := s.envelope(t, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared", at,
		event.RebalanceClearedPayload{AccountID: "joint", DecisionID: febDecision, Action: event.RebalanceResume, ClearedAt: at})
	insert := func(in []event.Envelope, after string, e event.Envelope) []event.Envelope {
		i := find(t, in, after) + 1
		return append(in[:i], append([]event.Envelope{e}, in[i:]...)...)
	}
	runPreludeCases(t, momentumConfiguration(), closeOf("2025-01-02"), inputs, []preludeCase{
		{"a decision for another account", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, febDecision)
			in[i] = edit(t, in[i], "account_id", "joint")
			return in
		}, febDecision, "account \"joint\"'s"},
		{"a Boost decision", func(t *testing.T, in []event.Envelope) []event.Envelope {
			e := edit(t, boostDecision, "account_id", iraAccount)
			e.ConfigurationHash = s.hash
			return insert(in, febDecision, e)
		}, boostDecision.ID, "not a momentum decision"},
		{"a plan for another account", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, plannedAAA)
			in[i] = edit(t, in[i], "account_id", "joint")
			return in
		}, plannedAAA, "account \"joint\"'s"},
		{"a plan recorded twice", func(t *testing.T, in []event.Envelope) []event.Envelope {
			return insert(in, plannedAAA, in[find(t, in, plannedAAA)])
		}, plannedAAA, "planned twice"},
		{"a release for another account", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, releasedAAA)
			in[i] = edit(t, in[i], "account_id", "joint")
			return in
		}, releasedAAA, "account \"joint\"'s"},
		{"a release of another quantity", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, releasedAAA)
			in[i] = edit(t, in[i], "quantity", 1)
			return in
		}, releasedAAA, "does not describe the order"},
		{"a release recorded twice", func(t *testing.T, in []event.Envelope) []event.Envelope {
			return insert(in, releasedAAA, in[find(t, in, releasedAAA)])
		}, releasedAAA, "released twice"},
		{"a clearing for another account", func(t *testing.T, in []event.Envelope) []event.Envelope {
			return append(in, cleared)
		}, "cleared", "account \"joint\"'s"},
		{"an engine state that does not decode", func(t *testing.T, in []event.Envelope) []event.Envelope {
			e := ofType(simulateMomentum(t, momentumOptions{noMark: map[string]string{"2025-02-28": "AAA"}}).decisions, event.EngineStateEventType)[0]
			e.Payload = json.RawMessage(`[]`)
			return insert(in, releasedAAA, e)
		}, "engine-state:" + febSell("AAA"), "decode"},
		{"an engine state the gate refuses", func(t *testing.T, in []event.Envelope) []event.Envelope {
			e := ofType(simulateMomentum(t, momentumOptions{noMark: map[string]string{"2025-02-28": "AAA"}}).decisions, event.EngineStateEventType)[0]
			e.SchemaVersion = event.EngineStateSchemaVersion
			return insert(in, releasedAAA, e)
		}, "engine-state:" + febSell("AAA"), "schema version 1"},
		{"a history whose decision session lacks a held bar", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, "bar:IEF:2025-02-28")
			in = append(in[:i], in[i+1:]...)
			j := find(t, in, "session:2025-02-28")
			var p event.SessionClosedPayload
			decode(t, in[j], &p)
			var ids []string
			for _, id := range p.InstrumentIDs {
				if id != "IEF" {
					ids = append(ids, id)
				}
			}
			p.InstrumentIDs = ids
			in[j] = withPayload(t, in[j], p)
			return in
		}, "session:2025-02-28", "cannot value"},
	})

	// A refused order in the prelude decodes and leaves nothing to recover,
	// but one for another account is refused.
	degraded := heldRestartWith(t, momentumOptions{noMark: map[string]string{"2025-02-28": "AAA"}}, "session:2025-02-28")
	refused := "risk-order-refused:" + febSell("AAA")
	runPreludeCases(t, momentumConfiguration(), closeOf("2025-01-02"), degraded, []preludeCase{
		{"a refusal for another account", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, refused)
			in[i] = edit(t, in[i], "account_id", "joint")
			return in
		}, refused, "account \"joint\"'s"},
	})

	// An earlier restart's record that held a rebalance this prelude never
	// recovered is refused.
	twice := heldRestart(t, "session:2025-02-28", "fill:"+febSell("AAA"))
	record := "restart-recovered:ira:run-options:2025-02-28T21:00:01Z"
	runPreludeCases(t, momentumConfiguration(), closeOf("2025-01-02"), twice, []preludeCase{
		{"a held rebalance not recovered", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, record)
			var p event.RestartRecoveredPayload
			decode(t, in[i], &p)
			p.Rebalance.DecisionID = "targets-decided:ira:2025-01-31T21:00:00Z"
			in[i] = withPayload(t, in[i], p)
			return in
		}, record, "not the rebalance recovered"},
		{"a restart record for another account", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, record)
			in[i] = edit(t, in[i], "account_id", "joint")
			return in
		}, record, "account \"joint\"'s"},
	})
}

// heldRestartWith is heldRestart for a run shaped by opts.
func heldRestartWith(t *testing.T, opts momentumOptions, afters ...string) []event.Envelope {
	t.Helper()
	opts.restartAfter = afters
	r := simulateMomentum(t, opts)
	inputs, err := targetweight.RestartInputs(entryEnvelopes(r.earlier[len(r.earlier)-1]), targetweight.Options{DecideFrom: closeOf("2025-01-02")}, r.restartAt)
	if err != nil {
		t.Fatal(err)
	}
	return inputs
}

// TestRestartPhasesFromTheRecords recovers each phase a rebalance can be in
// at a restart from the records alone.
func TestRestartPhasesFromTheRecords(t *testing.T) {
	phase := func(opts momentumOptions) event.RecoveredRebalance {
		t.Helper()
		return recoveredRecord(t, entryDecisions(simulateMomentum(t, opts).entries)).Rebalance
	}
	// A restart in the same input that ended the sells: the buys were planned
	// there, so they are working, and the rebalance is held.
	if got := phase(momentumOptions{restartAfter: []string{"fill:" + febSell("CCC")}}); got.Phase != event.RebalanceBuysWorking || !got.Held {
		t.Errorf("after the last sell: %+v", got)
	}
	// An abandoned rebalance restarted again stays abandoned and is not held.
	if got := phase(momentumOptions{
		restartAfter: []string{"session:2025-02-28", "fill:" + febSell("AAA")},
		inject:       clearing("ack:"+febSell("AAA"), event.RebalanceAbandon),
	}); got.Phase != event.RebalanceAbandoned || got.Held {
		t.Errorf("abandoned: %+v", got)
	}
	// A restart after a withheld decision recovers no rebalance.
	none := simulateMomentum(t, momentumOptions{noSnapshot: map[string]bool{"2025-01-31": true}, restartAfter: []string{"session:2025-01-31"}})
	got := recoveredRecord(t, entryDecisions(none.entries))
	if got.Rebalance.DecisionID != "" || got.BookDecisionID != "" || got.LastDecisionID == "" {
		t.Errorf("after a withheld decision: %+v", got)
	}
}

// TestAHeldRebalanceWithholdsTheNextDecision restarts with January's buys
// working and never clears: February's decision is withheld, the rebalance
// still in progress.
func TestAHeldRebalanceWithholdsTheNextDecision(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{restartAfter: []string{"session:2025-01-31"}})
	decided := targets(t, entryDecisions(r.entries))
	if len(decided) != 1 || decided[0].WithheldReason != event.WithheldRebalanceInProgress || !strings.Contains(decided[0].Detail, "held until the owner clears it") {
		t.Fatalf("February after a held restart: %+v", decided)
	}
	if n := len(ofType(entryDecisions(r.entries), event.OrderPlannedEventType)); n != 0 {
		t.Errorf("%d orders planned while held", n)
	}
}

func TestRunOptionsInputRules(t *testing.T) {
	cfg := momentumConfiguration()
	from := closeOf("2025-01-02")
	good := runOptions(t, from, 0)
	for _, c := range []struct {
		name string
		e    event.Envelope
		want string
	}{
		{"an unknown version", func() event.Envelope { e := good; e.SchemaVersion = 9; return e }(), "schema version 9"},
		{"another configuration", func() event.Envelope { e := good; e.ConfigurationHash = "sha256:other"; return e }(), "another configuration"},
	} {
		if _, err := feedRun(t, cfg, from, []event.Envelope{c.e}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	bad := good
	bad.SchemaVersion = 9
	if _, err := targetweight.RecordedOptions([]event.Envelope{good, bad}); err == nil || !strings.Contains(err.Error(), "schema version 9") {
		t.Errorf("recorded options at an unknown version: %v", err)
	}
	if _, err := targetweight.RestartInputs(entryEnvelopes(simulateMomentum(t, momentumOptions{stopAfter: "2025-01-03"}).entries),
		targetweight.Options{DecideFrom: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}, from); err == nil || !strings.Contains(err.Error(), "decide from") {
		t.Errorf("an unwritable first session: %v", err)
	}
}

func TestBoostPreludeRefusesRecordsThatDoNotFit(t *testing.T) {
	noMark := map[int]bool{0: true}
	earlier := simulate(t, options{noMark: noMark, closes: syntheticCloses()[:5]})
	inputs, err := targetweight.RestartInputs(entryEnvelopes(earlier.entries), targetweight.Options{DecideFrom: sessionEnd(0)}, sessionEnd(4).Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	decision := "targets-decided:joint:" + sessionEnd(0).Format("2006-01-02T15:04:05Z07:00")
	planned := ofType(earlier.decisions, event.OrderPlannedEventType)[0].ID
	runPreludeCases(t, testConfiguration(), sessionEnd(0), inputs, []preludeCase{
		{"a decision for another account", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, decision)
			in[i] = edit(t, in[i], "account_id", iraAccount)
			return in
		}, decision, "another configuration or account"},
		{"a plan that does not decode", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, planned)
			in[i] = edit(t, in[i], "quantity", "many")
			return in
		}, planned, "decode"},
		{"a plan for another account", func(t *testing.T, in []event.Envelope) []event.Envelope {
			i := find(t, in, planned)
			in[i] = edit(t, in[i], "account_id", iraAccount)
			return in
		}, planned, "account \"ira\"'s"},
	})

	// A second Boost restart reads the first's record back.
	first, err := targetweight.RestartInputs(entryEnvelopes(earlier.entries), targetweight.Options{DecideFrom: sessionEnd(0)}, sessionEnd(4).Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	restarted := simulate(t, options{noMark: noMark, restart: first, from: 5, closes: syntheticCloses()[:8]})
	again, err := targetweight.RestartInputs(entryEnvelopes(restarted.entries), targetweight.Options{DecideFrom: sessionEnd(0)}, sessionEnd(7).Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	second := simulate(t, options{noMark: noMark, restart: again, from: 8})
	got := recoveredRecord(t, second.decisions)
	if got.EngineState != event.EngineStateDegraded || got.LastDecisionID != "targets-decided:joint:"+sessionEnd(7).Format("2006-01-02T15:04:05Z07:00") {
		t.Errorf("second Boost restart recovered %+v", got)
	}
}

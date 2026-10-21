package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/operator"
)

const at = "2026-10-17T14:30:05Z"

// invoke runs the tool with args and returns what it printed.
func invoke(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := run(args, &out)
	return out.String(), err
}

// inboxRecords returns the operator records in dir, decoded as the engine
// decodes them.
func inboxRecords(t *testing.T, dir string) []operator.Record {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []operator.Record
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		r, err := operator.Decode(raw)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if e.Name() != r.FileName() {
			t.Errorf("%s holds %s", e.Name(), r.FileName())
		}
		out = append(out, r)
	}
	return out
}

// TestTheKillSwitchIsRecordedInTheInbox: the tool records the owner's kill
// switch as an operator.kill-switch record the engine delivers before the
// adapter's next input (ADR 0026, point 3), says what it recorded, and
// leaves nothing else in the inbox.
func TestTheKillSwitchIsRecordedInTheInbox(t *testing.T) {
	inbox := t.TempDir()
	out, err := invoke(t, "kill-switch", "-inbox", inbox, "-account", "joint", "-state", "engaged", "-by", "owner", "-detail", "news before the open", "-at", at)
	if err != nil {
		t.Fatal(err)
	}
	records := inboxRecords(t, inbox)
	if len(records) != 1 {
		t.Fatalf("the inbox holds %d records, want 1", len(records))
	}
	var p event.KillSwitchPayload
	if err := json.Unmarshal(records[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if !p.Engaged || p.AccountID != "joint" || p.Operator != "owner" || p.Detail != "news before the open" || p.AsOf.Format(time.RFC3339) != at {
		t.Errorf("recorded %+v", p)
	}
	for _, want := range []string{"recorded operator.kill-switch:joint:" + at, "engaged", records[0].FileName(), "before the adapter's next input"} {
		if !strings.Contains(out, want) {
			t.Errorf("printed %q, want it to say %q", out, want)
		}
	}

	if _, err := invoke(t, "kill-switch", "-inbox", inbox, "-account", "joint", "-state", "released", "-by", "owner", "-detail", "clear", "-at", "2026-10-17T15:00:00Z"); err != nil {
		t.Fatal(err)
	}
	records = inboxRecords(t, inbox)
	if err := json.Unmarshal(records[1].Payload, &p); err != nil || len(records) != 2 || p.Engaged {
		t.Errorf("the release: %+v %v", records, err)
	}
}

// TestTheClearanceIsRecordedInTheInbox: the tool records the owner's
// clearance of a rebalance in progress, naming its decision and every order
// of it not terminal (ADR 0026, point 5).
func TestTheClearanceIsRecordedInTheInbox(t *testing.T) {
	inbox := t.TempDir()
	out, err := invoke(t, "rebalance-cleared", "-inbox", inbox, "-account", "ira", "-decision", "targets-decided:ira:2026-09-30T20:00:00Z",
		"-orders", "ira-buy-2,ira-buy-1", "-by", "owner", "-detail", "cancelled at IBKR", "-at", at)
	if err != nil {
		t.Fatal(err)
	}
	records := inboxRecords(t, inbox)
	var p event.RebalanceClearedPayload
	if err := json.Unmarshal(records[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.DecisionID != "targets-decided:ira:2026-09-30T20:00:00Z" || strings.Join(p.ClientOrderIDs, ",") != "ira-buy-1,ira-buy-2" {
		t.Errorf("recorded %+v", p)
	}
	if !strings.Contains(out, "recorded operator.rebalance.cleared:ira:"+at) {
		t.Errorf("printed %q", out)
	}
}

// TestTheAcknowledgementIsRecordedInTheInbox: the tool records the owner's
// acknowledgement of one alert, named by the key the alert states, as an
// operator.alert.acknowledged record the engine delivers before the
// adapter's next input, which stops the reporter repeating that alert (ADR
// 0019, amended 2026-09-24, "Alerting").
func TestTheAcknowledgementIsRecordedInTheInbox(t *testing.T) {
	inbox := t.TempDir()
	const key = "engine-state:halted:risk-limit-breached:||:2026-10-17T14:00:00Z@joint"
	out, err := invoke(t, "acknowledge", "-inbox", inbox, "-account", "joint", "-alert", key, "-by", "owner", "-detail", "seen; reconciling at IBKR", "-at", at)
	if err != nil {
		t.Fatal(err)
	}
	records := inboxRecords(t, inbox)
	if len(records) != 1 || records[0].Type != event.AlertAcknowledgedEventType {
		t.Fatalf("the inbox holds %+v, want one acknowledgement", records)
	}
	var p event.AlertAcknowledgedPayload
	if err := json.Unmarshal(records[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.AccountID != "joint" || p.AlertKey != key || p.AcknowledgedBy != "owner" || p.Detail != "seen; reconciling at IBKR" || p.AcknowledgedAt.Format(time.RFC3339) != at {
		t.Errorf("recorded %+v", p)
	}
	for _, want := range []string{"recorded operator.alert.acknowledged:joint:" + at, "alert " + key, records[0].FileName(), "before the adapter's next input"} {
		if !strings.Contains(out, want) {
			t.Errorf("printed %q, want it to say %q", out, want)
		}
	}
}

// TestTheAcknowledgementKeyIsTrimmedAndNeedsNoDetail: a key pasted with
// surrounding whitespace or a trailing newline is recorded trimmed, and
// -detail is optional for this command.
func TestTheAcknowledgementKeyIsTrimmedAndNeedsNoDetail(t *testing.T) {
	inbox := t.TempDir()
	if _, err := invoke(t, "acknowledge", "-inbox", inbox, "-account", "joint", "-alert", "  key-1\n", "-by", "owner", "-at", at); err != nil {
		t.Fatal(err)
	}
	records := inboxRecords(t, inbox)
	var p event.AlertAcknowledgedPayload
	if len(records) != 1 || json.Unmarshal(records[0].Payload, &p) != nil || p.AlertKey != "key-1" || p.Detail != "" {
		t.Errorf("recorded %+v, want key-1 with no detail", records)
	}
	for _, key := range []string{"a\nb", "a\r\nb", "   ", "\n"} {
		if _, err := invoke(t, "acknowledge", "-inbox", t.TempDir(), "-account", "joint", "-alert", key, "-by", "owner", "-at", at); err == nil || !strings.Contains(err.Error(), "-alert") && !strings.Contains(err.Error(), "alert key is required") {
			t.Errorf("key %q: %v, want a refusal naming the alert key", key, err)
		}
	}
}

// TestAnInputTheToolCannotRecordIsRefused: every input is validated before
// anything is written, and a record is never overwritten, so a refusal
// leaves the inbox as it was.
func TestAnInputTheToolCannotRecordIsRefused(t *testing.T) {
	inbox := t.TempDir()
	kill := []string{"-inbox", inbox, "-account", "joint", "-by", "owner", "-detail", "x", "-at", at}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no command":       {nil, "usage"},
		"an unknown one":   {[]string{"halt"}, "unknown command"},
		"a bad flag":       {[]string{"kill-switch", "-nope"}, "flag provided but not defined"},
		"no state":         {append([]string{"kill-switch"}, kill...), "-state"},
		"a state misnamed": {append([]string{"kill-switch", "-state", "on"}, kill...), "-state"},
		"no inbox":         {[]string{"kill-switch", "-state", "engaged", "-account", "joint", "-by", "owner", "-detail", "x"}, "-inbox"},
		"no inbox folder":  {[]string{"kill-switch", "-state", "engaged", "-inbox", filepath.Join(inbox, "absent"), "-account", "joint", "-by", "owner", "-detail", "x", "-at", at}, "absent"},
		"a bad time":       {append([]string{"kill-switch", "-state", "engaged"}, append(kill, "-at", "today")...), "-at"},
		"no operator":      {[]string{"kill-switch", "-state", "engaged", "-inbox", inbox, "-account", "joint", "-detail", "x", "-at", at}, "operator is required"},
		"no orders":        {[]string{"rebalance-cleared", "-inbox", inbox, "-account", "ira", "-decision", "d", "-by", "owner", "-detail", "x", "-at", at}, "client order ids"},
		"a repeated order": {[]string{"rebalance-cleared", "-inbox", inbox, "-account", "ira", "-decision", "d", "-orders", "a,a", "-by", "owner", "-detail", "x", "-at", at}, "ascending"},
		"no alert key":     {[]string{"acknowledge", "-inbox", inbox, "-account", "joint", "-by", "owner", "-at", at}, "alert key is required"},
		"no acknowledger":  {[]string{"acknowledge", "-inbox", inbox, "-account", "joint", "-alert", "k", "-at", at}, "acknowledged by is required"},
		"an ack bad time":  {[]string{"acknowledge", "-inbox", inbox, "-account", "joint", "-alert", "k", "-by", "owner", "-at", "today"}, "-at"},
		"an ack extra arg": {[]string{"acknowledge", "-inbox", inbox, "-account", "joint", "-alert", "k", "-by", "owner", "-at", at, "k2"}, "k2"},
		"extra arguments":  {append([]string{"kill-switch", "-state", "engaged"}, append(kill, "extra")...), "extra"},
	} {
		if _, err := invoke(t, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: run = %v, want a refusal saying %q", name, err, c.want)
		}
	}
	if entries, _ := os.ReadDir(inbox); len(entries) != 0 {
		t.Errorf("refusals left %d files in the inbox", len(entries))
	}
	args := append([]string{"kill-switch", "-state", "engaged"}, kill...)
	if _, err := invoke(t, args...); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, args...); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second record of one input in one second: %v", err)
	}
	if entries, _ := os.ReadDir(inbox); len(entries) != 1 {
		t.Errorf("the inbox holds %d files, want the one record", len(entries))
	}
}

// previousLog writes a durable log at path: a configuration input, then one
// input with decisions.
func previousLog(t *testing.T, path string, decisions ...event.Envelope) journal.Recovery {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	log, err := journal.CreateLog(file, journal.NewLogHeader("sha256:cfg", "boost/1.6.0/test"))
	if err != nil {
		t.Fatal(err)
	}
	stamp := func(id, eventType string, version, sequence uint64, payload any) event.Envelope {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		when := time.Date(2026, time.October, 16, 20, 0, 0, 0, time.UTC)
		return event.Envelope{ID: id, Type: eventType, SchemaVersion: uint32(version), EnvelopeVersion: event.CurrentEnvelopeVersion,
			EventTime: when, RecordedAt: when, Sequence: sequence, Source: "test", StrategyVersion: "boost/1.6.0/test",
			ConfigurationHash: "sha256:cfg", PayloadHash: event.HashPayload(raw), Payload: raw}
	}
	if err := log.Append([]journal.Entry{{Kind: journal.KindInput, Envelope: stamp("configuration", event.TargetWeightConfigurationEventType, 5, 1, map[string]string{"x": "y"})}}); err != nil {
		t.Fatal(err)
	}
	if len(decisions) > 0 {
		group := []journal.Entry{{Kind: journal.KindInput, Envelope: stamp("session:1", event.SessionClosedEventType, 1, 2, map[string]string{"x": "y"})}}
		for i, d := range decisions {
			group = append(group, journal.Entry{Kind: journal.KindDecision, Envelope: stamp(d.ID, d.Type, uint64(d.SchemaVersion), uint64(i+1), d.Payload)})
		}
		if err := log.Append(group); err != nil {
			t.Fatal(err)
		}
	}
	_ = file.Close()
	reread, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reread.Close() }()
	recovery, err := journal.RecoverLog(reread)
	if err != nil {
		t.Fatal(err)
	}
	return recovery
}

func encoded(t *testing.T, p any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestTheApprovalStatesWhatThePreviousLogEndsIn: the tool writes the owner's
// operator.run.approved file for a fresh run (ADR 0027, amended 2026-10-17)
// from the previous log itself: its file name and final record hash,
// Boost's latch, and the incident it ended in, carried in force with its
// state and sale attempt, or cleared by its ID when the owner says so. It
// warns that a kill switch left engaged is released by a fresh run.
func TestTheApprovalStatesWhatThePreviousLogEndsIn(t *testing.T) {
	dir := t.TempDir()
	halted := event.Envelope{ID: "engine-state:joint:1", Type: event.EngineStateEventType, SchemaVersion: event.EngineStateScopedVersion,
		Payload: encoded(t, event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "test"})}
	decided := event.Envelope{ID: "targets-decided:joint:1", Type: event.TargetsDecidedEventType, SchemaVersion: event.TargetsDecidedSchemaVersion,
		Payload: encoded(t, event.TargetsDecidedPayload{Boost: event.BoostFigures{LatchedAfter: true}})}
	logPath := filepath.Join(dir, "run-1.log")
	recovery := previousLog(t, logPath, decided, halted)

	carried := filepath.Join(dir, "carried.json")
	out, err := invoke(t, "approve", "-log", logPath, "-account", "joint", "-by", "owner", "-out", carried, "-at", at)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(carried)
	if err != nil {
		t.Fatal(err)
	}
	p, err := event.DecodeOperatorRunApproved(raw)
	if err != nil {
		t.Fatalf("the engine would refuse the approval: %v", err)
	}
	if p.PreviousLog != "run-1.log" || p.PreviousLogFinalHash != recovery.FinalRecordHash || !*p.Boost.Latched || p.IncidentInForce == nil ||
		p.IncidentInForce.IncidentID != "engine-state:joint:1" || p.IncidentInForce.State != event.EngineStateHalted || *p.IncidentInForce.SaleAttempted {
		t.Errorf("approval %+v", p)
	}
	for _, want := range []string{"run-1.log", "halted", "engine-state:joint:1", carried, "-approval"} {
		if !strings.Contains(out, want) {
			t.Errorf("printed %q, want it to say %q", out, want)
		}
	}

	cleared := filepath.Join(dir, "cleared.json")
	if _, err := invoke(t, "approve", "-log", logPath, "-account", "joint", "-by", "owner", "-out", cleared, "-clear-incident", "engine-state:joint:1", "-at", at); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(cleared)
	if p, err = event.DecodeOperatorRunApproved(raw); err != nil || p.IncidentID != "engine-state:joint:1" || p.IncidentInForce != nil {
		t.Errorf("cleared: %+v %v", p, err)
	}

	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no log":                   {[]string{"approve", "-account", "joint", "-by", "owner", "-out", filepath.Join(dir, "a.json")}, "-log"},
		"no out":                   {[]string{"approve", "-log", logPath, "-account", "joint", "-by", "owner"}, "-out"},
		"an existing approval":     {[]string{"approve", "-log", logPath, "-account", "joint", "-by", "owner", "-out", carried, "-at", at}, "already exists"},
		"another incident cleared": {[]string{"approve", "-log", logPath, "-account", "joint", "-by", "owner", "-out", filepath.Join(dir, "b.json"), "-clear-incident", "x", "-at", at}, "engine-state:joint:1"},
		"no approver":              {[]string{"approve", "-log", logPath, "-account", "joint", "-out", filepath.Join(dir, "c.json"), "-at", at}, "approved by"},
		"a missing log":            {[]string{"approve", "-log", filepath.Join(dir, "absent.log"), "-account", "joint", "-by", "owner", "-out", filepath.Join(dir, "d.json"), "-at", at}, "absent.log"},
	} {
		if _, err := invoke(t, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: run = %v, want a refusal saying %q", name, err, c.want)
		}
	}

	short := filepath.Join(dir, "run-2.log")
	previousLog(t, short)
	if _, err := invoke(t, "approve", "-log", short, "-account", "joint", "-by", "owner", "-out", filepath.Join(dir, "e.json"), "-at", at); err == nil || !strings.Contains(err.Error(), "nothing past its configuration") {
		t.Errorf("a log cut short: %v", err)
	}
	damaged := filepath.Join(dir, "run-3.log")
	if err := os.WriteFile(damaged, []byte("not a log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, "approve", "-log", damaged, "-account", "joint", "-by", "owner", "-out", filepath.Join(dir, "f.json"), "-at", at); err == nil || !strings.Contains(err.Error(), "run-3.log") {
		t.Errorf("a damaged log: %v", err)
	}
}

// TestTheApprovalWarnsOfAKillSwitchLeftEngaged: a fresh run starts with the
// kill switch released, so the tool says to record it again.
func TestTheApprovalWarnsOfAKillSwitchLeftEngaged(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run-1.log")
	file, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	log, err := journal.CreateLog(file, journal.NewLogHeader("sha256:cfg", "boost/1.6.0/test"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := operator.KillSwitch(event.KillSwitchPayload{AccountID: "joint", AsOf: time.Date(2026, time.October, 16, 15, 0, 0, 0, time.UTC), Engaged: true, Operator: "owner", Detail: "x"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := r.Envelope(1, "boost/1.6.0/test", "sha256:cfg")
	cfg.ID, cfg.Type, cfg.SchemaVersion, cfg.Source = "configuration", event.TargetWeightConfigurationEventType, 5, "engine"
	for _, e := range []event.Envelope{cfg, r.Envelope(2, "boost/1.6.0/test", "sha256:cfg")} {
		if err := log.Append([]journal.Entry{{Kind: journal.KindInput, Envelope: e}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = file.Close()
	out, err := invoke(t, "approve", "-log", logPath, "-account", "joint", "-by", "owner", "-out", filepath.Join(dir, "a.json"), "-at", at)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kill switch") || !strings.Contains(out, "operator kill-switch") {
		t.Errorf("printed %q, want a warning to engage the kill switch again", out)
	}
}

// TestTheInboxAndApprovalDirectoriesAreFlushed: a record or approval is
// reported only after its directory is flushed, so the name the tool says it
// recorded survives a power loss with the file (ADR 0027 §3).
func TestTheInboxAndApprovalDirectoriesAreFlushed(t *testing.T) {
	var flushed []string
	original := syncDirectory
	syncDirectory = func(dir string) error { flushed = append(flushed, dir); return original(dir) }
	t.Cleanup(func() { syncDirectory = original })

	inbox := t.TempDir()
	if _, err := invoke(t, "kill-switch", "-inbox", inbox, "-account", "joint", "-state", "engaged", "-by", "owner", "-detail", "x", "-at", at); err != nil {
		t.Fatal(err)
	}
	if len(flushed) != 1 || flushed[0] != inbox {
		t.Errorf("flushed %v after recording, want the inbox %s", flushed, inbox)
	}

	flushed = nil
	syncDirectory = func(string) error { return errors.New("injected flush fault") }
	out, err := invoke(t, "kill-switch", "-inbox", inbox, "-account", "joint", "-state", "released", "-by", "owner", "-detail", "x", "-at", "2026-10-17T15:00:00Z")
	if err == nil || !strings.Contains(err.Error(), "injected flush fault") || strings.Contains(out, "recorded") {
		t.Errorf("a failed flush: printed %q, err %v; want the fault and nothing reported recorded", out, err)
	}

	syncDirectory = func(dir string) error { flushed = append(flushed, dir); return original(dir) }
	dir := t.TempDir()
	logPath := killSwitchLog(t, dir, "joint")
	approval := filepath.Join(dir, "approvals", "a.json")
	if err := os.Mkdir(filepath.Dir(approval), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, "approve", "-log", logPath, "-account", "joint", "-by", "owner", "-out", approval, "-at", at); err != nil {
		t.Fatal(err)
	}
	if len(flushed) != 1 || flushed[0] != filepath.Dir(approval) {
		t.Errorf("flushed %v after the approval, want %s", flushed, filepath.Dir(approval))
	}
}

// killSwitchLog writes dir/run-1.log: a configuration and the kill switch
// engaged for account, the log a run that ended with it engaged leaves.
func killSwitchLog(t *testing.T, dir, account string, more ...operator.Record) string {
	t.Helper()
	logPath := filepath.Join(dir, "run-1.log")
	file, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	log, err := journal.CreateLog(file, journal.NewLogHeader("sha256:cfg", "boost/1.12.0/test"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := operator.KillSwitch(event.KillSwitchPayload{AccountID: account, AsOf: time.Date(2026, time.October, 16, 15, 0, 0, 0, time.UTC), Engaged: true, Operator: "owner", Detail: "x"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := r.Envelope(1, "boost/1.12.0/test", "sha256:cfg")
	cfg.ID, cfg.Type, cfg.SchemaVersion, cfg.Source = "configuration", event.TargetWeightConfigurationEventType, 5, "engine"
	inputs := []event.Envelope{cfg, r.Envelope(2, "boost/1.12.0/test", "sha256:cfg")}
	for i, m := range more {
		inputs = append(inputs, m.Envelope(uint64(3+i), "boost/1.12.0/test", "sha256:cfg"))
	}
	for _, e := range inputs {
		if err := log.Append([]journal.Entry{{Kind: journal.KindInput, Envelope: e}}); err != nil {
			t.Fatal(err)
		}
	}
	return logPath
}

// TestTheApprovalWarnsOfTheSwitchThisAccountLeftEngaged: only this
// account's kill switch counts; another account's release, which the
// engine refused, does not hide this account's engagement.
func TestTheApprovalWarnsOfTheSwitchThisAccountLeftEngaged(t *testing.T) {
	dir := t.TempDir()
	other, err := operator.KillSwitch(event.KillSwitchPayload{AccountID: "ira", AsOf: time.Date(2026, time.October, 16, 16, 0, 0, 0, time.UTC), Engaged: false, Operator: "owner", Detail: "mistyped account"})
	if err != nil {
		t.Fatal(err)
	}
	logPath := killSwitchLog(t, dir, "joint", other)
	out, err := invoke(t, "approve", "-log", logPath, "-account", "joint", "-by", "owner", "-out", filepath.Join(dir, "a.json"), "-at", at)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "operator kill-switch") {
		t.Errorf("printed %q, want a warning to engage the kill switch again", out)
	}
}

// TestEachClearedOrderIDIsTrimmed: "-orders a, b" names orders a and b, not
// " b".
func TestEachClearedOrderIDIsTrimmed(t *testing.T) {
	inbox := t.TempDir()
	if _, err := invoke(t, "rebalance-cleared", "-inbox", inbox, "-account", "ira", "-decision", "targets-decided:ira:2026-09-30T20:00:00Z",
		"-orders", " ira-buy-2 , ira-buy-1", "-by", "owner", "-detail", "cancelled at IBKR", "-at", at); err != nil {
		t.Fatal(err)
	}
	var p event.RebalanceClearedPayload
	if err := json.Unmarshal(inboxRecords(t, inbox)[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.ClientOrderIDs, ",") != "ira-buy-1,ira-buy-2" {
		t.Errorf("recorded orders %q, want each ID trimmed", p.ClientOrderIDs)
	}
}

// TestARecordTheEngineCouldNotReadIsRefused: the inbox belongs to the user
// that writes the owner's instructions (ti-operator on the server), and a
// record is readable through the inbox's group only, so run as anyone but
// the inbox's owner the tool would leave a record in the wrong hands; it
// refuses instead, naming the user to run it as.
func TestARecordTheEngineCouldNotReadIsRefused(t *testing.T) {
	originalOwner, originalUser := inboxOwner, effectiveUser
	t.Cleanup(func() { inboxOwner, effectiveUser = originalOwner, originalUser })
	inboxOwner = func(os.FileInfo) (int, bool) { return 4242, true }
	effectiveUser = func() int { return 501 }

	inbox := t.TempDir()
	_, err := invoke(t, "kill-switch", "-inbox", inbox, "-account", "joint", "-state", "engaged", "-by", "owner", "-detail", "x", "-at", at)
	if err == nil || !strings.Contains(err.Error(), "4242") || !strings.Contains(err.Error(), "sudo -u") {
		t.Errorf("err %v, want a refusal naming the inbox's owner", err)
	}
	if entries, _ := os.ReadDir(inbox); len(entries) != 0 {
		t.Errorf("the inbox holds %d file(s) after a refusal", len(entries))
	}

	effectiveUser = func() int { return 0 }
	if _, err := invoke(t, "kill-switch", "-inbox", inbox, "-account", "joint", "-state", "engaged", "-by", "owner", "-detail", "x", "-at", at); err == nil {
		t.Error("root wrote a record the engine's user could not read")
	}
	effectiveUser = func() int { return 4242 }
	if _, err := invoke(t, "kill-switch", "-inbox", inbox, "-account", "joint", "-state", "engaged", "-by", "owner", "-detail", "x", "-at", at); err != nil {
		t.Errorf("the inbox's owner: %v", err)
	}
}

// TestARecordIsReadableThroughTheInboxsGroup: the engine reads the inbox
// through its group and runs as a user other than the record's writer
// (ADR 0026, kill-switch amendment; docs/manual/operator.md, "Who can write
// an instruction"), so each record is mode 0640 in the inbox's group: the
// engine can read it, and only its writer can change it.
func TestARecordIsReadableThroughTheInboxsGroup(t *testing.T) {
	inbox := t.TempDir()
	if _, err := invoke(t, "kill-switch", "-inbox", inbox, "-account", "joint", "-state", "engaged", "-by", "owner", "-detail", "x", "-at", at); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(inbox)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(inbox)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the inbox holds %v (%v), want one record", entries, err)
	}
	info, err := os.Stat(filepath.Join(inbox, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("the record is mode %o, want 0640", info.Mode().Perm())
	}
	if got, want := info.Sys().(*syscall.Stat_t).Gid, dir.Sys().(*syscall.Stat_t).Gid; got != want {
		t.Errorf("the record's group is %d, want the inbox's %d", got, want)
	}
}

// TestTheApprovalCarriesACashScopeInForce: a log that ended Degraded on a
// reconciliation's unexplained cash is approved with that cash scope carried
// in force (a restart never narrows a Degraded scope; ADR 0019, amended
// 2026-09-24), and the tool says which cash component stays frozen.
func TestTheApprovalCarriesACashScopeInForce(t *testing.T) {
	dir := t.TempDir()
	degraded := event.Envelope{ID: "engine-state:joint:1", Type: event.EngineStateEventType, SchemaVersion: event.EngineStateScopedVersion,
		Payload: encoded(t, event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonReconciliationUnexplained, Detail: "test",
			ScopeCashComponents: []string{event.ReconciliationComponentCash}})}
	logPath := filepath.Join(dir, "run-1.log")
	previousLog(t, logPath, degraded)

	path := filepath.Join(dir, "carried.json")
	out, err := invoke(t, "approve", "-log", logPath, "-account", "joint", "-by", "owner", "-out", path, "-at", at)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := event.DecodeOperatorRunApproved(raw)
	if err != nil {
		t.Fatalf("the engine would refuse the approval: %v", err)
	}
	if in := p.IncidentInForce; in == nil || in.State != event.EngineStateDegraded || len(in.ScopeCashComponents) != 1 || in.ScopeCashComponents[0] != event.ReconciliationComponentCash {
		t.Errorf("incident in force %+v, want degraded on %s", p.IncidentInForce, event.ReconciliationComponentCash)
	}
	if !strings.Contains(out, event.ReconciliationComponentCash) {
		t.Errorf("printed %q, want it to name the cash component %s", out, event.ReconciliationComponentCash)
	}
}

// TestTheFirstRunsApprovalStatesTheStartingBook: an account's first run is
// approved with the holdings and total cash the owner checked in IBKR, which
// the adapter compares the run's first reading after a close with (ADR 0019
// and ADR 0027, amended 2026-10-18); a fresh run's approval may state the
// book the owner confirms after a change no fill explains. What is stated
// is read strictly: a holding twice, a quantity or cash that is no number,
// or a first run with a previous log, is refused.
func TestTheFirstRunsApprovalStatesTheStartingBook(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.json")
	out, err := invoke(t, "approve", "-first-run", "-account", "joint", "-by", "owner", "-positions", "SPY=147,IEF=3", "-cash", "-4211.37", "-currency", "USD", "-out", first, "-at", at)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	p, err := event.DecodeOperatorRunApproved(raw)
	if err != nil {
		t.Fatalf("the engine would refuse the approval: %v", err)
	}
	b := p.StartingBook
	if !p.FirstRun() || *p.Boost.Latched || b == nil || b.TotalCash != -4211.37 || b.Currency != "USD" ||
		len(b.Positions) != 2 || b.Positions[0] != (event.AccountPosition{InstrumentID: "IEF", Quantity: 3}) || b.Positions[1].Quantity != 147 {
		t.Fatalf("approval %+v, book %+v", p, b)
	}
	for _, want := range []string{"first run", "IEF 3", "SPY 147", "-4211.37", first} {
		if !strings.Contains(out, want) {
			t.Errorf("printed %q, want it to say %q", out, want)
		}
	}
	empty := filepath.Join(dir, "empty.json")
	if _, err := invoke(t, "approve", "-first-run", "-account", "joint", "-by", "owner", "-positions", "", "-cash", "4000", "-currency", "USD", "-out", empty, "-at", at); err != nil {
		t.Fatalf("a first run holding nothing: %v", err)
	}

	logPath := filepath.Join(dir, "run-1.log")
	previousLog(t, logPath, event.Envelope{ID: "targets-decided:joint:1", Type: event.TargetsDecidedEventType, SchemaVersion: event.TargetsDecidedSchemaVersion,
		Payload: encoded(t, event.TargetsDecidedPayload{Boost: event.BoostFigures{LatchedAfter: false}})})
	stated := filepath.Join(dir, "stated.json")
	if _, err := invoke(t, "approve", "-log", logPath, "-account", "joint", "-by", "owner", "-positions", "SPY=100", "-cash", "250.5", "-currency", "USD", "-out", stated, "-at", at); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(stated)
	if p, err = event.DecodeOperatorRunApproved(raw); err != nil || p.FirstRun() || p.StartingBook == nil || p.StartingBook.TotalCash != 250.5 {
		t.Errorf("a fresh run's approval stating the book: %+v %v", p, err)
	}

	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a first run with a log":   {[]string{"-first-run", "-log", logPath, "-positions", "", "-cash", "1", "-currency", "USD"}, "-first-run"},
		"a first run with no cash": {[]string{"-first-run", "-positions", "", "-currency", "USD"}, "-cash"},
		"a first run, no holdings": {[]string{"-first-run", "-cash", "1", "-currency", "USD"}, "-positions"},
		"no currency":              {[]string{"-first-run", "-positions", "", "-cash", "1"}, "-currency"},
		"a holding twice":          {[]string{"-first-run", "-positions", "SPY=1,SPY=2", "-cash", "1", "-currency", "USD"}, "twice"},
		"a quantity not a count":   {[]string{"-first-run", "-positions", "SPY=1.5", "-cash", "1", "-currency", "USD"}, "SPY=1.5"},
		"cash not a number":        {[]string{"-first-run", "-positions", "", "-cash", "lots", "-currency", "USD"}, "lots"},
		"cash not finite":          {[]string{"-first-run", "-positions", "", "-cash", "NaN", "-currency", "USD"}, "NaN"},
		"half a book":              {[]string{"-log", logPath, "-cash", "1", "-currency", "USD"}, "-positions"},
	} {
		args := append([]string{"approve", "-account", "joint", "-by", "owner", "-out", filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json"), "-at", at}, c.args...)
		if _, err := invoke(t, args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

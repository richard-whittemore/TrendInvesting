package operator_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/operator"
)

var at = time.Date(2026, time.October, 17, 14, 30, 5, 0, time.UTC)

func engage() event.KillSwitchPayload {
	return event.KillSwitchPayload{AccountID: "joint", AsOf: at, Engaged: true, Operator: "owner", Detail: "news before the open"}
}

func clearance() event.RebalanceClearedPayload {
	return event.RebalanceClearedPayload{AccountID: "ira", AsOf: at, DecisionID: "targets-decided:ira:2026-09-30T20:00:00Z",
		ClientOrderIDs: []string{"ira-buy-1"}, Operator: "owner", Detail: "cancelled at IBKR"}
}

func acknowledgement() event.AlertAcknowledgedPayload {
	return event.AlertAcknowledgedPayload{AccountID: "joint", AlertKey: "engine-state:halted:risk-limit-breached:||:2026-10-17T14:00:00Z@joint",
		AcknowledgedBy: "owner", AcknowledgedAt: at, Detail: "seen; reconciling at IBKR"}
}

// TestARecordNamesItsInputByTypeAccountAndTime: an operator input is
// recorded under an ID made of its type, account and time, and a file name
// that sorts by time, so the engine applies the inbox in the order the
// operator recorded it and no two records of one type and account share a
// second.
func TestARecordNamesItsInputByTypeAccountAndTime(t *testing.T) {
	r, err := operator.KillSwitch(engage())
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "operator.kill-switch:joint:2026-10-17T14:30:05Z" || r.Type != event.KillSwitchEventType || r.SchemaVersion != event.KillSwitchSchemaVersion ||
		!r.At.Equal(at) || r.AccountID != "joint" || r.FileName() != "20261017T143005Z-operator.kill-switch.json" {
		t.Errorf("record %+v, file %s", r, r.FileName())
	}
	c, err := operator.RebalanceCleared(clearance())
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "operator.rebalance.cleared:ira:2026-10-17T14:30:05Z" || c.Type != event.RebalanceClearedEventType || c.FileName() != "20261017T143005Z-operator.rebalance.cleared.json" {
		t.Errorf("record %+v", c)
	}
	a, err := operator.AlertAcknowledged(acknowledgement())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "operator.alert.acknowledged:joint:2026-10-17T14:30:05Z" || a.Type != event.AlertAcknowledgedEventType ||
		a.SchemaVersion != event.AlertAcknowledgedSchemaVersion || !a.At.Equal(at) || a.FileName() != "20261017T143005Z-operator.alert.acknowledged.json" {
		t.Errorf("record %+v", a)
	}
}

// TestAnInvalidInputIsNeverRecorded: the tool refuses an input its payload
// would refuse, so the inbox holds only inputs the engine can read.
func TestAnInvalidInputIsNeverRecorded(t *testing.T) {
	bad := engage()
	bad.Operator = ""
	if _, err := operator.KillSwitch(bad); err == nil || !strings.Contains(err.Error(), "operator is required") {
		t.Errorf("KillSwitch = %v", err)
	}
	empty := clearance()
	empty.ClientOrderIDs = nil
	if _, err := operator.RebalanceCleared(empty); err == nil || !strings.Contains(err.Error(), "client order ids") {
		t.Errorf("RebalanceCleared = %v", err)
	}
	keyless := acknowledgement()
	keyless.AlertKey = ""
	if _, err := operator.AlertAcknowledged(keyless); err == nil || !strings.Contains(err.Error(), "alert key is required") {
		t.Errorf("AlertAcknowledged = %v", err)
	}
}

// TestADecodedRecordIsExactlyOneTheToolWrites: the engine reads back only a
// record the tool would write, byte for byte in its payload, so a hand-edited
// or damaged file is refused rather than read as some other instruction.
func TestADecodedRecordIsExactlyOneTheToolWrites(t *testing.T) {
	for _, build := range []func() (operator.Record, error){
		func() (operator.Record, error) { return operator.KillSwitch(engage()) },
		func() (operator.Record, error) { return operator.RebalanceCleared(clearance()) },
		func() (operator.Record, error) { return operator.AlertAcknowledged(acknowledgement()) },
	} {
		r, err := build()
		if err != nil {
			t.Fatal(err)
		}
		raw := r.Encode()
		if !bytes.HasSuffix(raw, []byte("\n")) {
			t.Errorf("%s: a record file ends in a newline", r.ID)
		}
		got, err := operator.Decode(raw)
		if err != nil || got.ID != r.ID || !bytes.Equal(got.Payload, r.Payload) || !got.At.Equal(r.At) {
			t.Errorf("Decode = %+v, %v; want %+v", got, err, r)
		}
	}
	good, _ := operator.KillSwitch(engage())
	edit := func(f func(map[string]any)) []byte {
		raw := good.Encode()
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		f(m)
		out, _ := json.Marshal(m)
		return out
	}
	for name, c := range map[string]struct {
		raw  []byte
		want string
	}{
		"not json":              {[]byte("not json"), "decode the operator record"},
		"an unknown key":        {edit(func(m map[string]any) { m["extra"] = 1 }), "unknown field"},
		"an unknown type":       {edit(func(m map[string]any) { m["type"] = "operator.unknown" }), "not an operator input"},
		"another version":       {edit(func(m map[string]any) { m["schema_version"] = 2 }), "schema version 2"},
		"a released ID":         {edit(func(m map[string]any) { m["id"] = "operator.kill-switch:joint:2026-10-17T14:30:06Z" }), "is not the record"},
		"an edited detail":      {edit(func(m map[string]any) { m["payload"].(map[string]any)["detail"] = "other" }), "is not the record"},
		"a payload key":         {edit(func(m map[string]any) { m["payload"].(map[string]any)["extra"] = true }), "is not the record"},
		"an engaged null":       {edit(func(m map[string]any) { m["payload"].(map[string]any)["engaged"] = nil }), "engaged must be stated"},
		"a bad clearance":       {edit(func(m map[string]any) { m["type"] = event.RebalanceClearedEventType }), "rebalance cleared"},
		"a bad acknowledgement": {edit(func(m map[string]any) { m["type"] = event.AlertAcknowledgedEventType }), "alert acknowledged"},
		"an invalid input":      {edit(func(m map[string]any) { m["payload"].(map[string]any)["operator"] = "" }), "operator is required"},
		"a second record":       {append(good.Encode(), good.Encode()...), "after the record"},
		"trailing bytes":        {append(good.Encode(), []byte("}")...), "after the record"},
	} {
		if _, err := operator.Decode(c.raw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Decode = %v, want a refusal saying %q", name, err, c.want)
		}
	}
}

// TestARecordIsDeliveredAsAnOperatorInput: the engine wraps a record as an
// input at the sequence it continues, stamped with the run's identity and
// the operator as its source; its times are the operator's, so a journal
// replays it identically.
func TestARecordIsDeliveredAsAnOperatorInput(t *testing.T) {
	r, _ := operator.KillSwitch(engage())
	e := r.Envelope(7, "boost/1.6.0/build", "sha256:cfg")
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if e.ID != r.ID || e.Type != event.KillSwitchEventType || e.SchemaVersion != event.KillSwitchSchemaVersion || e.Sequence != 7 ||
		e.Source != operator.Source || !e.EventTime.Equal(at) || !e.RecordedAt.Equal(at) || e.StrategyVersion != "boost/1.6.0/build" ||
		e.ConfigurationHash != "sha256:cfg" || e.PayloadHash != event.HashPayload(r.Payload) {
		t.Errorf("envelope %+v", e)
	}
}

package event_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// The owner's decision of 2026-10-18 (ADR 0019, ADR 0025 point 5 and ADR
// 0026 point 1, amended): one account reading taken after the close, with
// nothing working at IBKR and no execution since, stands for the account as
// of that close. The broker snapshot then states the close as its as-of
// time and the true read time beside it; otherwise it states the read time
// and why the reading could not stand for the close. The mark is the
// delivered bar's official close, with IBKR's last price beside it as
// evidence only.

// postCloseSnapshot is a version-2 broker snapshot stamped as of the close,
// read 70 minutes after it.
func postCloseSnapshot() event.BrokerSnapshotPayload {
	p := validBrokerSnapshot()
	p.MarketOpen = false
	p.OpenOrders = []event.BrokerOrder{}
	p.ReadAt = twSession.Add(70 * time.Minute)
	p.AsOfClose = true
	return p
}

// readTimeSnapshot is a version-2 broker snapshot stamped at its read time,
// stating why the reading could not stand for the close.
func readTimeSnapshot() event.BrokerSnapshotPayload {
	p := validBrokerSnapshot()
	p.AsOf = twSession.Add(70 * time.Minute)
	p.ReadAt = p.AsOf
	p.NotAsOfClose = "IBKR shows 1 open order for the account"
	return p
}

func TestBrokerSnapshotVersion2StatesItsReadTime(t *testing.T) {
	for name, p := range map[string]event.BrokerSnapshotPayload{
		"as of the close": postCloseSnapshot(), "at its read time": readTimeSnapshot(),
	} {
		if err := p.ValidateSchema(event.BrokerSnapshotReadAtVersion); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("%s: Validate: %v", name, err)
		}
		if err := p.ValidateSchema(event.BrokerSnapshotSchemaVersion); err == nil || !strings.Contains(err.Error(), "version 1") {
			t.Errorf("%s at version 1: err = %v, want the version-2 fields refused", name, err)
		}
	}
	type P = event.BrokerSnapshotPayload
	check := func(name string, base P, mutate func(*P), want string) {
		t.Helper()
		p := base
		mutate(&p)
		err := p.ValidateSchema(event.BrokerSnapshotReadAtVersion)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, want)
		}
	}
	check("no read time", postCloseSnapshot(), func(p *P) { p.ReadAt, p.AsOfClose = time.Time{}, false; p.NotAsOfClose = "x" }, "read at is required")
	check("sub-second read time", postCloseSnapshot(), func(p *P) { p.ReadAt = p.ReadAt.Add(time.Millisecond) }, "whole second")
	check("read before its as-of", readTimeSnapshot(), func(p *P) { p.ReadAt = p.AsOf.Add(-time.Second) }, "read at")
	check("as of the close, read at it", postCloseSnapshot(), func(p *P) { p.ReadAt = p.AsOf }, "after the close")
	check("as of the close with a reason", postCloseSnapshot(), func(p *P) { p.NotAsOfClose = "x" }, "states no reason")
	check("read time with no reason", readTimeSnapshot(), func(p *P) { p.NotAsOfClose = "" }, "why")
	check("read time stamped other than its read", readTimeSnapshot(), func(p *P) { p.ReadAt = p.AsOf.Add(time.Minute) }, "its read time")
	if err := postCloseSnapshot().ValidateSchema(3); err == nil {
		t.Error("version 3: want refused")
	}
	v1 := validBrokerSnapshot()
	if err := v1.ValidateSchema(event.BrokerSnapshotSchemaVersion); err != nil {
		t.Errorf("version 1: %v", err)
	}
	if err := v1.ValidateSchema(event.BrokerSnapshotReadAtVersion); err == nil || !strings.Contains(err.Error(), "read at is required") {
		t.Errorf("version 1 payload read at version 2: err = %v, want its read time required", err)
	}
	if got := event.BrokerSnapshotVersions(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("BrokerSnapshotVersions = %v, want [1 2]", got)
	}
}

func TestBrokerSnapshotVersion1EncodesAsBefore(t *testing.T) {
	raw, err := json.Marshal(validBrokerSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"read_at", "as_of_close", "not_as_of_close"} {
		if strings.Contains(string(raw), field) {
			t.Errorf("a version-1 snapshot encodes %s: %s", field, raw)
		}
	}
	raw, err = json.Marshal(postCloseSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	var back event.BrokerSnapshotPayload
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.ReadAt.Equal(postCloseSnapshot().ReadAt) || !back.AsOfClose || back.NotAsOfClose != "" {
		t.Errorf("round trip = %+v from %s", back, raw)
	}
}

func evidencedMark() event.MarkRecordedPayload {
	p := validMarkRecorded()
	p.Source = "marketdata"
	p.BrokerLast, p.BrokerLastSet = 560.4, true
	return p
}

func TestMarkVersion2CarriesIBKRsLastPriceAsEvidence(t *testing.T) {
	unset := evidencedMark()
	unset.BrokerLast, unset.BrokerLastSet = 0, false
	for name, p := range map[string]event.MarkRecordedPayload{"with IBKR's last": evidencedMark(), "without it": unset} {
		if err := p.ValidateSchema(event.MarkRecordedEvidenceVersion); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := evidencedMark().ValidateSchema(event.MarkRecordedSchemaVersion); err == nil || !strings.Contains(err.Error(), "version 1") {
		t.Errorf("evidence at version 1: err = %v, want refused", err)
	}
	if err := validMarkRecorded().ValidateSchema(event.MarkRecordedSchemaVersion); err != nil {
		t.Errorf("version 1: %v", err)
	}
	if err := evidencedMark().ValidateSchema(3); err == nil {
		t.Error("version 3: want refused")
	}
	type P = event.MarkRecordedPayload
	for name, c := range map[string]struct {
		mutate func(*P)
		want   string
	}{
		"last not finite":         {func(p *P) { p.BrokerLast = math.NaN() }, "broker last must be finite"},
		"last set not positive":   {func(p *P) { p.BrokerLast = 0 }, "positive"},
		"last unset with a value": {func(p *P) { p.BrokerLastSet = false }, "none is set"},
	} {
		p := evidencedMark()
		c.mutate(&p)
		if err := p.ValidateSchema(event.MarkRecordedEvidenceVersion); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
		if err := p.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
	raw, err := json.Marshal(validMarkRecorded())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "broker_last") {
		t.Errorf("a version-1 mark encodes the evidence: %s", raw)
	}
	if got := event.MarkRecordedVersions(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("MarkRecordedVersions = %v, want [1 2]", got)
	}
}

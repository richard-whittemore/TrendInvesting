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
// delivered bar's official close, with IBKR's last price beside it, which
// the gate's price band compares with when it is set.

// postCloseSnapshot is a version-2 broker snapshot stamped as of the close,
// read 70 minutes after it.
func postCloseSnapshot() event.BrokerSnapshotPayload {
	p := validBrokerSnapshot()
	p.MarketOpen = false
	p.OpenOrders = []event.BrokerOrder{}
	p.ReadAt = twSession.Add(70 * time.Minute)
	p.AsOfClose = true
	p.ReadNetLiquidation = p.NetLiquidation + 12.5
	return p
}

// readTimeSnapshot is a version-2 broker snapshot stamped at its read time,
// stating why the reading could not stand for the close.
func readTimeSnapshot() event.BrokerSnapshotPayload {
	p := validBrokerSnapshot()
	p.AsOf = twSession.Add(70 * time.Minute)
	p.ReadAt = p.AsOf
	p.ReadNetLiquidation = p.NetLiquidation
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

// TestBrokerSnapshotVersion2StatesIBKRsNetLiquidationAndTheCashDrift: a
// snapshot as of the close values its net liquidation at the official close
// and states IBKR's own at the read beside it, with the cash drift since the
// previous close the adapter allowed; a snapshot at its read time is IBKR's
// own reading, so the two figures agree and it carries no drift (ADR 0019,
// amended 2026-10-18).
func TestBrokerSnapshotVersion2StatesIBKRsNetLiquidationAndTheCashDrift(t *testing.T) {
	type P = event.BrokerSnapshotPayload
	atClose := postCloseSnapshot()
	atClose.ReadNetLiquidation, atClose.CashDrift = 8411.7, -3.25
	read := readTimeSnapshot()
	for name, p := range map[string]P{"as of the close": atClose, "at its read time": read} {
		if err := p.ValidateSchema(event.BrokerSnapshotReadAtVersion); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]struct {
		base   P
		mutate func(*P)
		want   string
	}{
		"read net liquidation not finite": {atClose, func(p *P) { p.ReadNetLiquidation = math.Inf(1) }, "net liquidation at the read must be finite"},
		"cash drift not finite":           {atClose, func(p *P) { p.CashDrift = math.NaN() }, "cash drift must be finite"},
		"read time, two figures":          {read, func(p *P) { p.ReadNetLiquidation = p.NetLiquidation + 1 }, "IBKR's own"},
		"read time with a drift":          {read, func(p *P) { p.CashDrift = 1 }, "cash drift"},
		"unreadable with a value": {atClose, func(p *P) {
			p.Unreadable, p.NetLiquidation = []string{event.BrokerComponentNetLiquidation}, 0
		}, "unreadable net liquidation"},
	} {
		p := c.base
		c.mutate(&p)
		if err := p.ValidateSchema(event.BrokerSnapshotReadAtVersion); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if got := atClose.AlarmNetLiquidation(); got != 8411.7 {
		t.Errorf("as of the close, the alarm's net liquidation = %v, want IBKR's own at the read", got)
	}
	if got := read.AlarmNetLiquidation(); got != read.NetLiquidation {
		t.Errorf("at its read time, the alarm's net liquidation = %v, want the snapshot's", got)
	}
	v1 := validBrokerSnapshot()
	v1.ReadNetLiquidation = v1.NetLiquidation
	if err := v1.ValidateSchema(event.BrokerSnapshotSchemaVersion); err == nil {
		t.Errorf("version 1 with IBKR's net liquidation beside it: err = %v, want refused", err)
	}
	raw, err := json.Marshal(atClose)
	if err != nil {
		t.Fatal(err)
	}
	var back P
	if err := json.Unmarshal(raw, &back); err != nil || back.ReadNetLiquidation != 8411.7 || back.CashDrift != -3.25 {
		t.Errorf("round trip = %+v (%v) from %s", back, err, raw)
	}
}

// TestRiskFiguresNameTheBandsPriceOnlyWhenAMarkIsSet: an order's figures name
// which price the band compared, IBKR's last price or the official close,
// and only beside a mark (ADR 0026 point 3, amended 2026-10-18).
func TestRiskFiguresNameTheBandsPriceOnlyWhenAMarkIsSet(t *testing.T) {
	for _, source := range []string{"", event.MarkSourceBrokerLast, event.MarkSourceOfficialClose} {
		p := validOrderReleased()
		p.Figures.MarkSource = source
		if err := p.Validate(); err != nil {
			t.Errorf("mark source %q: %v", source, err)
		}
	}
	p := validOrderReleased()
	p.Figures.MarkSource = "quote"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "mark source") {
		t.Errorf("an unknown mark source: err = %v", err)
	}
	p = validOrderReleased()
	p.Figures.Mark, p.Figures.MarkSet, p.Figures.MarkDeviation, p.Figures.MarkSource = 0, false, 0, event.MarkSourceOfficialClose
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "mark source") {
		t.Errorf("a mark source with no mark: err = %v", err)
	}
}

// TestACloseStampedSnapshotStatesIBKRsNetLiquidationAtTheRead: a snapshot as
// of the close with a readable net liquidation states IBKR's own at the read
// as a positive figure, the margin alarm's basis; one left out (zero, as an
// omitted key decodes) or not positive is refused, so an absent figure never
// reads as a non-positive balance and never fires the sale back to 98% (ADR
// 0019, "An unavailable balance is not zero"; ADR 0015, fail closed).
func TestACloseStampedSnapshotStatesIBKRsNetLiquidationAtTheRead(t *testing.T) {
	for name, figure := range map[string]float64{"left out": 0, "negative": -1} {
		p := postCloseSnapshot()
		p.ReadNetLiquidation = figure
		if err := p.ValidateSchema(event.BrokerSnapshotReadAtVersion); err == nil || !strings.Contains(err.Error(), "net liquidation at the read") {
			t.Errorf("%s: err = %v, want IBKR's net liquidation at the read required", name, err)
		}
	}
	p := postCloseSnapshot()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "read_net_liquidation")
	raw, _ = json.Marshal(fields)
	var back event.BrokerSnapshotPayload
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.ValidateSchema(event.BrokerSnapshotReadAtVersion); err == nil {
		t.Error("a close-stamped snapshot with no read_net_liquidation key was accepted")
	}
	unread := postCloseSnapshot()
	unread.Unreadable, unread.NetLiquidation, unread.ReadNetLiquidation = []string{event.BrokerComponentNetLiquidation}, 0, 0
	if err := unread.ValidateSchema(event.BrokerSnapshotReadAtVersion); err != nil {
		t.Errorf("an unreadable net liquidation states no figure at the read: %v", err)
	}
}

// TestACloseStampedSnapshotsCashDriftIsWithinItsBound: the adapter stamps a
// reading as of the close only when its cash drift since the previous close
// is within CashDriftBound of its net liquidation, so the engine refuses a
// close-stamped snapshot that states a larger one (ADR 0019, amended
// 2026-10-18). The bound is 1%: SPY's largest quarterly dividend on a
// holding at 147% of net liquidation exceeds 0.5%.
func TestACloseStampedSnapshotsCashDriftIsWithinItsBound(t *testing.T) {
	if event.CashDriftBound != 0.01 {
		t.Fatalf("CashDriftBound = %v, want 0.01", event.CashDriftBound)
	}
	p := postCloseSnapshot()
	p.NetLiquidation = 10000
	for _, drift := range []float64{100, -100, 0, 3.25} {
		p.CashDrift = drift
		if err := p.ValidateSchema(event.BrokerSnapshotReadAtVersion); err != nil {
			t.Errorf("drift %v of 10000: %v", drift, err)
		}
	}
	for _, drift := range []float64{100.01, -100.01, 10000} {
		p.CashDrift = drift
		if err := p.ValidateSchema(event.BrokerSnapshotReadAtVersion); err == nil || !strings.Contains(err.Error(), "cash drift") {
			t.Errorf("drift %v of 10000: err = %v, want refused beyond the bound", drift, err)
		}
	}
}

// TestACloseStampedSnapshotStatesTheUnexplainedCash: beside the day's drift,
// a snapshot as of the close states the cash IBKR shows beyond the last
// reconciliation the engine explained and every fill since
// (UnexplainedCash), so drift accepted day after day is seen as it adds up;
// a snapshot at its read time and version 1 state none (ADR 0019, amended
// 2026-10-18).
func TestACloseStampedSnapshotStatesTheUnexplainedCash(t *testing.T) {
	p := postCloseSnapshot()
	p.CashDrift, p.UnexplainedCash = 3.25, 412.5
	if err := p.ValidateSchema(event.BrokerSnapshotReadAtVersion); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back event.BrokerSnapshotPayload
	if err := json.Unmarshal(raw, &back); err != nil || back.UnexplainedCash != 412.5 {
		t.Errorf("round trip = %+v (%v) from %s", back, err, raw)
	}
	p.UnexplainedCash = math.NaN()
	if err := p.ValidateSchema(event.BrokerSnapshotReadAtVersion); err == nil || !strings.Contains(err.Error(), "unexplained cash must be finite") {
		t.Errorf("not finite: err = %v", err)
	}
	read := readTimeSnapshot()
	read.UnexplainedCash = 1
	if err := read.ValidateSchema(event.BrokerSnapshotReadAtVersion); err == nil || !strings.Contains(err.Error(), "unexplained cash") {
		t.Errorf("read time: err = %v, want refused", err)
	}
	v1 := validBrokerSnapshot()
	v1.UnexplainedCash = 1
	if err := v1.ValidateSchema(event.BrokerSnapshotSchemaVersion); err == nil {
		t.Error("version 1 stating unexplained cash: want refused")
	}
}

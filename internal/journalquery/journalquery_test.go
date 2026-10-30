package journalquery_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
)

type rec struct {
	kind, typ string
	schema    uint32
	at        time.Time
	payload   any
}

func at(m time.Month, d int) time.Time { return time.Date(2026, m, d, 21, 0, 0, 0, time.UTC) }

func build(t *testing.T, recs ...rec) []byte {
	t.Helper()
	var entries []journal.Entry
	var start, end time.Time
	for i, r := range recs {
		raw, err := json.Marshal(r.payload)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, journal.Entry{Kind: r.kind, Envelope: event.Envelope{
			ID: fmt.Sprintf("e%d", i+1), Type: r.typ, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: r.schema,
			EventTime: r.at, RecordedAt: r.at, Sequence: uint64(i + 1), Source: "fixture", StrategyVersion: "s", ConfigurationHash: "h",
			PayloadHash: event.HashPayload(raw), Payload: raw,
		}})
		if r.kind == journal.KindInput {
			if start.IsZero() || r.at.Before(start) {
				start = r.at
			}
			if r.at.After(end) {
				end = r.at
			}
		}
	}
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("h", "s", start, end), entries); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func snap(when time.Time, p ...event.AccountPosition) rec {
	return rec{journal.KindInput, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, when,
		event.AccountPositionsSnapshotPayload{AccountID: "A", AsOf: when, NetLiquidation: 1, Currency: "USD", Positions: p}}
}

var fillSeq int

// fillRec is a valid fill with an id of its own.
func fillRec(kind, id string, qty int64, commission float64, when time.Time) rec {
	fillSeq++
	p := event.FillPayload{
		InstrumentID: id, Kind: kind, FillID: fmt.Sprintf("f%d", fillSeq), Direction: event.DirectionLong,
		Quantity: qty, Price: 2, Level: 2, FilledAt: when, Commission: commission,
	}
	switch kind {
	case event.FillKindEntry:
		p.ProposalID = "p"
	case event.FillKindStop:
		p.CampaignID, p.UnitIDs = "c", []string{"u"}
	default:
		p.ProposalID, p.CampaignID = "p", "c"
	}
	return rec{journal.KindInput, event.FillEventType, event.FillSchemaVersion, when, p}
}

var figures = event.RiskFigures{NetLiquidation: 1, ReferencePrice: 3}

func plannedPayload(instrument, side string) event.OrderPlannedPayload {
	return event.OrderPlannedPayload{
		AccountID: "A", Tag: "t", ClientOrderID: "c", InstrumentID: instrument, Side: side, Phase: side, OrderType: event.OrderTypeLimit,
		Quantity: 1, LimitPrice: 3, NetLiquidation: 1, ReferencePrice: 3, PriceBand: 0.05,
	}
}

func dec(typ string, schema uint32, when time.Time, p any) rec {
	return rec{journal.KindDecision, typ, schema, when, p}
}

func load(t *testing.T, name string, data []byte) journalquery.Journal {
	t.Helper()
	j, err := journalquery.Load(name, data)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func cell(t journalquery.Table, row int, column string) string {
	for i, c := range t.Columns {
		if c == column {
			return t.Rows[row][i]
		}
	}
	return "<no column " + column + ">"
}

func TestEveryReportOverOneHandBuiltJournal(t *testing.T) {
	d := at(time.March, 2)
	data := build(t,
		snap(at(time.January, 1), event.AccountPosition{InstrumentID: "AAA", Quantity: 10}),
		fillRec(event.FillKindEntry, "AAA", 5, 0.1, d),
		fillRec(event.FillKindEntry, "AAA", 5, 0.2, d),
		fillRec(event.FillKindExit, "AAA", 3, 0.3, d),
		dec(event.OrderPlannedEventType, 1, d, plannedPayload("AAA", "buy")),
		dec(event.OrderReleasedEventType, 1, d, event.OrderReleasedPayload{AccountID: "A", ClientOrderID: "c", PlanID: "p", Tag: "t", InstrumentID: "AAA", Side: "buy", Quantity: 1, LimitPrice: 3, Mode: "paper", Figures: figures}),
		dec(event.OrderRefusedEventType, 1, d, event.OrderRefusedPayload{AccountID: "A", ClientOrderID: "c", PlanID: "p", InstrumentID: "AAA", Side: "sell", Quantity: 1, LimitPrice: 3, Reason: "order-size", Detail: "d", Figures: figures}),
		rec{journal.KindInput, event.OrderLifecycleEventType, 1, d, event.OrderLifecyclePayload{InstrumentID: "AAA", OrderID: "o", Tag: "stop", Status: "submitted", Quantity: -7, StopPrice: 1.5, OccurredAt: d}},
		dec(event.EngineStateEventType, 1, d, event.EngineStatePayload{State: "halted", Reason: "risk-limit-breached", Detail: "x"}),
		rec{journal.KindInput, event.AccountSnapshotEventType, event.AccountSnapshotSchemaVersion, d, map[string]any{"equity": 1}},
		snap(at(time.March, 3), event.AccountPosition{InstrumentID: "AAA", Quantity: 12}, event.AccountPosition{InstrumentID: "ZZZ", Quantity: 1}),
	)
	js := []journalquery.Journal{load(t, "j", data)}
	f, err := journalquery.NewFilter("A", "", "2026-03-01", "2026-03-31")
	if err != nil {
		t.Fatal(err)
	}

	totals, err := journalquery.Totals(js, f)
	if err != nil {
		t.Fatal(err)
	}
	// AAA: 2 entries bought 10, 1 exit sold 3; commission 0.1+0.2+0.3 = 0.6.
	if got := strings.Join(totals.Rows[len(totals.Rows)-1], " "); got != "TOTAL all 3 10 3 0.60" {
		t.Errorf("totals: %q", got)
	}

	orders, err := journalquery.Orders(js, f)
	if err != nil || len(orders.Rows) != 4 || cell(orders, 3, "event") != "lifecycle-submitted" || cell(orders, 3, "quantity") != "-7" {
		t.Errorf("orders: %v %v", orders.Rows, err)
	}

	refusals, err := journalquery.Refusals(js, f)
	if err != nil || len(refusals.Rows) != 2 || cell(refusals, 1, "event") != "engine-state" || cell(refusals, 1, "instrument") != "-" {
		t.Errorf("refusals: %v %v", refusals.Rows, err)
	}

	// Baseline AAA 10, +5 +5 -3 = 17, but the snapshot reports 12; ZZZ is
	// reported but never filled.
	rec, err := journalquery.Reconcile(js, f)
	var d2 *journalquery.DisagreementError
	if !errors.As(err, &d2) || d2.Count != 2 || len(rec.Rows) != 2 {
		t.Fatalf("reconcile: %v %v", rec.Rows, err)
	}
	if cell(rec, 0, "fills") != "17" || cell(rec, 0, "snapshot") != "12" || cell(rec, 1, "instrument") != "ZZZ" {
		t.Errorf("reconcile rows: %v", rec.Rows)
	}
	if !strings.Contains(err.Error(), "2 disagreement") {
		t.Errorf("error: %v", err)
	}

	// A filter that matches no comparison leaves nothing checked, which
	// reconcile refuses to call a pass.
	none, _ := journalquery.NewFilter("B", "", "", "")
	if _, err := journalquery.Reconcile(js, none); err == nil || !strings.Contains(err.Error(), "compared nothing") {
		t.Errorf("filtered reconcile: %v", err)
	}
}

func TestTableFormats(t *testing.T) {
	tab := journalquery.Table{Columns: []string{"a", "b"}, Rows: [][]string{{"1", `x"y`}, {"2", "z"}}}
	for format, want := range map[string]string{
		"text": "a  b\n1  x\"y\n2  z\n",
		"csv":  "a,b\n1,\"x\"\"y\"\n2,z\n",
		"json": `[{"a":"1","b":"x\"y"},{"a":"2","b":"z"}]` + "\n",
	} {
		var out bytes.Buffer
		if err := tab.Write(&out, format); err != nil || out.String() != want {
			t.Errorf("%s: %q %v", format, out.String(), err)
		}
		if !journalquery.ValidFormat(format) {
			t.Errorf("%s not valid", format)
		}
	}
	if err := tab.Write(&bytes.Buffer{}, "xml"); err == nil || journalquery.ValidFormat("xml") {
		t.Error("xml accepted")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteErrorsSurface(t *testing.T) {
	tab := journalquery.Table{Columns: []string{"a"}, Rows: [][]string{{"1"}}}
	for _, format := range []string{"text", "csv", "json"} {
		if err := tab.Write(failWriter{}, format); err == nil {
			t.Errorf("%s: write error swallowed", format)
		}
	}
}

func TestNewFilterRefusesBadDates(t *testing.T) {
	for _, c := range [][2]string{{"x", ""}, {"", "x"}, {"2026-02-01", "2026-01-01"}} {
		if _, err := journalquery.NewFilter("", "", c[0], c[1]); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
}

func TestLoadRefusals(t *testing.T) {
	if _, err := journalquery.Load("empty", nil); err == nil {
		t.Error("empty file accepted")
	}
	data := build(t, snap(at(time.March, 3)))
	_, err := journalquery.Load("j", bytes.Replace(data, []byte(`"net_liquidation":1`), []byte(`"net_liquidation":2`), 1))
	if err == nil || !strings.Contains(err.Error(), "record 1") {
		t.Errorf("broken chain: %v", err)
	}
}

func TestUnreadablePayloadsAreRefusedByEveryReport(t *testing.T) {
	d := at(time.March, 2)
	for _, r := range []rec{
		{journal.KindInput, event.FillEventType, event.FillSchemaVersion, d, "not an object"},
		{journal.KindDecision, event.OrderPlannedEventType, 1, d, "x"},
		{journal.KindDecision, event.OrderReleasedEventType, 1, d, "x"},
		{journal.KindDecision, event.OrderRefusedEventType, 1, d, "x"},
		{journal.KindInput, event.OrderLifecycleEventType, 1, d, "x"},
		{journal.KindDecision, event.EngineStateEventType, 9, d, "x"},
		{journal.KindInput, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, d, "x"},
	} {
		js := []journalquery.Journal{load(t, "j", build(t, rec{journal.KindInput, "other.thing", 1, d, 1}, r))}
		if _, err := journalquery.Orders(js, journalquery.Filter{}); err == nil {
			t.Errorf("%s: orders accepted", r.typ)
		}
		for name, fn := range map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
			"fills": journalquery.Fills, "totals": journalquery.Totals, "refusals": journalquery.Refusals,
			"positions": journalquery.Positions, "reconcile": journalquery.Reconcile,
		} {
			if _, err := fn(js, journalquery.Filter{}); err == nil {
				t.Errorf("%s: %s accepted", r.typ, name)
			}
		}
	}
}

func TestFillsPositionsAndTotalsAcrossInstrumentsAndFilters(t *testing.T) {
	d := at(time.March, 2)
	data := build(t,
		fillRec(event.FillKindEntry, "BBB", 4, 0.5, d),
		fillRec(event.FillKindEntry, "AAA", 2, 0.25, d),
		fillRec(event.FillKindEntry, "AAA", 1, 0.25, at(time.March, 2).AddDate(-1, 0, 0)),
		fillRec(event.FillKindStop, "BBB", 1, 0.5, at(time.April, 2)),
		dec(event.OrderPlannedEventType, 1, d, plannedPayload("BBB", "buy")),
		dec(event.OrderRefusedEventType, 1, d, event.OrderRefusedPayload{AccountID: "A", ClientOrderID: "c", PlanID: "p", InstrumentID: "AAA", Side: "sell", Quantity: 1, LimitPrice: 3, Reason: "order-size", Detail: "d", Figures: figures}),
	)
	js := []journalquery.Journal{load(t, "j", data)}
	all := journalquery.Filter{}

	fills, err := journalquery.Fills(js, all)
	if err != nil || len(fills.Rows) != 4 || cell(fills, 3, "quantity") != "-1" {
		t.Fatalf("fills: %v %v", fills.Rows, err)
	}
	only, _ := journalquery.NewFilter("", "AAA", "", "")
	if f, _ := journalquery.Fills(js, only); len(f.Rows) != 2 {
		t.Errorf("filtered fills: %v", f.Rows)
	}
	if o, _ := journalquery.Orders(js, only); len(o.Rows) != 1 || cell(o, 0, "event") != "refused" {
		t.Errorf("filtered orders: %v", o.Rows)
	}
	if r, _ := journalquery.Refusals(js, journalquery.Filter{Account: "B"}); len(r.Rows) != 0 {
		t.Errorf("other account refusals: %v", r.Rows)
	}

	pos, err := journalquery.Positions(js, all)
	if err != nil || len(pos.Rows) != 4 || cell(pos, 3, "position") != "3" {
		t.Errorf("positions: %v %v", pos.Rows, err)
	}

	tot, _ := journalquery.Totals(js, all)
	want := []string{"AAA 2025 1 1 0 0.25", "AAA 2026 1 2 0 0.25", "BBB 2026 2 4 1 1.00", "TOTAL all 4 7 1 1.50"}
	for i, w := range want {
		if got := strings.Join(tot.Rows[i], " "); got != w {
			t.Errorf("totals row %d: %q want %q", i, got, w)
		}
	}
	if p, _ := journalquery.Positions(js, only); len(p.Rows) != 2 {
		t.Errorf("filtered positions: %v", p.Rows)
	}

	scoped := build(t,
		fillRec(event.FillKindEntry, "AAA", 1, 0, d),
		dec(event.EngineStateEventType, 2, d, event.EngineStatePayload{State: "degraded", Reason: "risk-limit-breached", Detail: "x", ScopeInstrumentIDs: []string{"AAA", "BBB"}}))
	sj := []journalquery.Journal{load(t, "s", scoped)}
	if r, _ := journalquery.Refusals(sj, journalquery.Filter{Instrument: "BBB"}); len(r.Rows) != 1 || cell(r, 0, "instrument") != "AAA;BBB" {
		t.Errorf("scoped engine state kept for BBB: %v", r.Rows)
	}
	if r, _ := journalquery.Refusals(sj, journalquery.Filter{Instrument: "CCC"}); len(r.Rows) != 0 {
		t.Errorf("scoped engine state kept for CCC: %v", r.Rows)
	}
	outside, _ := journalquery.NewFilter("", "", "2030-01-01", "")
	if r, _ := journalquery.Refusals(sj, outside); len(r.Rows) != 0 {
		t.Errorf("engine state outside the range: %v", r.Rows)
	}

	two := build(t, snap(d), rec{journal.KindInput, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, d,
		event.AccountPositionsSnapshotPayload{AccountID: "B", AsOf: d, NetLiquidation: 1, Currency: "USD"}},
		fillRec(event.FillKindEntry, "AAA", 1, 0, d))
	if _, err := journalquery.Positions([]journalquery.Journal{load(t, "two", two)}, all); err == nil || !strings.Contains(err.Error(), "attributed") {
		t.Errorf("two accounts: %v", err)
	}
}

// validFill is a fill that passes event.FillPayload.Validate.
func validFill(id string, qty int64, when time.Time) event.FillPayload {
	return event.FillPayload{
		InstrumentID: "AAA", Kind: event.FillKindEntry, FillID: id, ProposalID: "p-1", Direction: event.DirectionLong,
		Quantity: qty, Price: 2, FilledAt: when, Level: 2, Commission: 0.5,
	}
}

func fillInput(p event.FillPayload) rec {
	return rec{journal.KindInput, event.FillEventType, event.FillSchemaVersion, p.FilledAt, p}
}

func TestJSONKeysAndValuesThatMatchStayValid(t *testing.T) {
	tab := journalquery.Table{Columns: []string{"journal", "note"}, Rows: [][]string{{"journal", "note"}}}
	var out bytes.Buffer
	if err := tab.Write(&out, "json"); err != nil {
		t.Fatal(err)
	}
	var got []map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != 1 || got[0]["journal"] != "journal" || got[0]["note"] != "note" {
		t.Errorf("json %q: %v %v", out.String(), got, err)
	}
}

func TestRepeatedDeliveryOfOneFillCountsOnce(t *testing.T) {
	d := at(time.March, 2)
	data := build(t, fillInput(validFill("f-1", 5, d)), fillInput(validFill("f-1", 5, d)), fillInput(validFill("f-2", 3, d)))
	js := []journalquery.Journal{load(t, "j", data)}
	tot, err := journalquery.Totals(js, journalquery.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tot.Rows[len(tot.Rows)-1], " "); got != "TOTAL all 2 8 0 1.00" {
		t.Errorf("totals counted a repeated delivery: %q", got)
	}
	if f, _ := journalquery.Fills(js, journalquery.Filter{}); len(f.Rows) != 2 {
		t.Errorf("fills lists a repeated delivery: %v", f.Rows)
	}
}

func TestConflictingUsesOfOneFillIDAreRefused(t *testing.T) {
	d := at(time.March, 2)
	data := build(t, fillInput(validFill("f-1", 5, d)), fillInput(validFill("f-1", 6, d)))
	js := []journalquery.Journal{load(t, "j", data)}
	if _, err := journalquery.Totals(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "f-1") {
		t.Errorf("conflicting fill id: %v", err)
	}
}

func TestInvalidPayloadsAreRefusedBeforeAnyFigure(t *testing.T) {
	d := at(time.March, 2)
	negative := validFill("f-1", -4, d)
	unknown := validFill("f-2", 4, d)
	unknown.Kind = "teleport"
	badSnap := rec{journal.KindInput, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, d,
		event.AccountPositionsSnapshotPayload{AccountID: "A", AsOf: d, NetLiquidation: -1, Currency: "USD"}}
	for name, r := range map[string]rec{"negative quantity": fillInput(negative), "unknown kind": fillInput(unknown), "snapshot": badSnap} {
		js := []journalquery.Journal{load(t, "j", build(t, r))}
		for rname, fn := range map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
			"fills": journalquery.Fills, "totals": journalquery.Totals, "positions": journalquery.Positions, "reconcile": journalquery.Reconcile,
		} {
			if _, err := fn(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "record 1") {
				t.Errorf("%s: %s accepted or unnamed: %v", name, rname, err)
			}
		}
	}
}

func TestUnknownSnapshotVersionsAreRefusedNotSkipped(t *testing.T) {
	d := at(time.March, 2)
	for _, v := range []uint32{event.AccountSnapshotCashVersion + 1} {
		r := rec{journal.KindInput, event.AccountSnapshotEventType, v, d, event.AccountPositionsSnapshotPayload{AccountID: "A", AsOf: d, NetLiquidation: 1, Currency: "USD"}}
		js := []journalquery.Journal{load(t, "j", build(t, r))}
		if _, err := journalquery.Reconcile(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "record 1") {
			t.Errorf("snapshot v%d skipped: %v", v, err)
		}
	}
}

func TestEquivalentRepresentationsOfOneFillAreOneExecution(t *testing.T) {
	d := at(time.March, 2)
	// null versus empty unit_ids, and one instant written in two offsets: the
	// reducer (strategy.acceptedFillState.matches) treats both as identical.
	nullIDs := validFill("f-1", 5, d)
	emptyIDs := validFill("f-1", 5, d)
	emptyIDs.UnitIDs = []string{}
	zone := validFill("f-1", 5, d.In(time.FixedZone("EST", -5*3600)))
	data := build(t, fillInput(nullIDs), fillInput(emptyIDs), fillInput(zone), fillInput(validFill("f-2", 3, d)))
	js := []journalquery.Journal{load(t, "j", data)}
	f, err := journalquery.Fills(js, journalquery.Filter{})
	if err != nil || len(f.Rows) != 2 {
		t.Fatalf("equivalent representations were not one fill: %v %v", f.Rows, err)
	}
}

// syncBuffer is the SyncWriter a durable log is written to in tests.
type syncBuffer struct{ bytes.Buffer }

func (*syncBuffer) Sync() error { return nil }

// buildLog writes recs through the journal package's own durable-log writer:
// each input starts a group and the decisions after it join that group. When
// uncommitted is set, the last group is left without its commit line.
func buildLog(t *testing.T, uncommitted bool, recs ...rec) []byte {
	t.Helper()
	var groups [][]journal.Entry
	for i, r := range recs {
		raw, err := json.Marshal(r.payload)
		if err != nil {
			t.Fatal(err)
		}
		e := journal.Entry{Kind: r.kind, Envelope: event.Envelope{
			ID: fmt.Sprintf("e%d", i+1), Type: r.typ, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: r.schema,
			EventTime: r.at, RecordedAt: r.at, Sequence: uint64(i + 1), Source: "fixture", StrategyVersion: "s", ConfigurationHash: "h",
			PayloadHash: event.HashPayload(raw), Payload: raw,
		}}
		if r.kind == journal.KindInput {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], e)
	}
	var buf syncBuffer
	log, err := journal.CreateLog(&buf, journal.NewLogHeader("h", "s"))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if err := log.Append(g); err != nil {
			t.Fatal(err)
		}
	}
	out := buf.Bytes()
	if uncommitted {
		// Drop the final commit line: the last group is then a torn tail.
		trimmed := bytes.TrimRight(out, "\n")
		out = trimmed[:bytes.LastIndexByte(trimmed, '\n')+1]
	}
	return out
}

func durableRecs() []rec {
	d := at(time.March, 2)
	return []rec{
		fillRec(event.FillKindEntry, "AAA", 5, 0.1, d),
		dec(event.OrderPlannedEventType, event.OrderPlannedSchemaVersion, d, plannedPayload("AAA", "buy")),
		fillRec(event.FillKindEntry, "BBB", 4, 0.2, d),
	}
}

func TestADurableLogReadsLikeTheEquivalentJournal(t *testing.T) {
	recs := durableRecs()
	fromJournal := []journalquery.Journal{load(t, "run", build(t, recs...))}
	fromLog := []journalquery.Journal{load(t, "run", buildLog(t, false, recs...))}
	for name, report := range map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
		"fills": journalquery.Fills, "totals": journalquery.Totals, "orders": journalquery.Orders,
	} {
		want, err := report(fromJournal, journalquery.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := report(fromLog, journalquery.Filter{})
		if err != nil {
			t.Fatalf("%s over a durable log: %v", name, err)
		}
		if len(got.Rows) == 0 || fmt.Sprint(got.Rows) != fmt.Sprint(want.Rows) {
			t.Errorf("%s over a durable log = %v, want %v", name, got.Rows, want.Rows)
		}
	}
}

func TestADurableLogReportsOnlyCommittedGroups(t *testing.T) {
	js := []journalquery.Journal{load(t, "run", buildLog(t, true, durableRecs()...))}
	f, err := journalquery.Fills(js, journalquery.Filter{})
	if err != nil || len(f.Rows) != 1 || cell(f, 0, "instrument") != "AAA" {
		t.Fatalf("a torn final group was reported: %v %v", f.Rows, err)
	}
}

func TestADamagedDurableLogIsRefusedByName(t *testing.T) {
	data := buildLog(t, false, durableRecs()...)
	data = bytes.Replace(data, []byte(`"quantity":5`), []byte(`"quantity":6`), 1)
	_, err := journalquery.Load("run.log", data)
	var damaged *journal.LogDamageError
	if !errors.As(err, &damaged) || !strings.Contains(err.Error(), "run.log") {
		t.Fatalf("want a *journal.LogDamageError naming run.log, got %v", err)
	}
}

func TestADurableLogWithNothingCommittedReportsNoRows(t *testing.T) {
	var buf syncBuffer
	if _, err := journal.CreateLog(&buf, journal.NewLogHeader("h", "s")); err != nil {
		t.Fatal(err)
	}
	f, err := journalquery.Fills([]journalquery.Journal{load(t, "run", buf.Bytes())}, journalquery.Filter{})
	if err != nil || len(f.Rows) != 0 {
		t.Fatalf("empty log: %v %v", f.Rows, err)
	}
}

func symbolChange(from, to string, when time.Time) rec {
	return dec(event.InstrumentSymbolChangedEventType, event.InstrumentSymbolChangedSchemaVersion, when, event.InstrumentSymbolChangedPayload{
		InstrumentID: from, NewInstrumentID: to, CorporateActionID: "ca-1", EffectiveAt: when,
		Rule: event.RuleSymbolChangeCarriesInstrumentState, ADR: event.ADRSymbolChangeCarriesInstrumentState,
	})
}

func TestARecordedSymbolChangeMovesTheHoldingToTheNewId(t *testing.T) {
	d := at(time.March, 2)
	data := build(t,
		snap(at(time.January, 1), event.AccountPosition{InstrumentID: "OLD", Quantity: 100}),
		symbolChange("OLD", "NEW", d),
		fillRec(event.FillKindExit, "NEW", 100, 0, d),
		snap(at(time.March, 3)),
	)
	js := []journalquery.Journal{load(t, "j", data)}
	pos, err := journalquery.Positions(js, journalquery.Filter{})
	if err != nil || len(pos.Rows) != 1 || cell(pos, 0, "instrument") != "NEW" || cell(pos, 0, "position") != "0" {
		t.Fatalf("positions after a rename: %v %v", pos.Rows, err)
	}
	rec, err := journalquery.Reconcile(js, journalquery.Filter{})
	if err != nil {
		t.Fatalf("a correct snapshot disagreed after a rename: %v %v", rec.Rows, err)
	}
	for _, r := range rec.Rows {
		if r[len(r)-1] != "ok" {
			t.Errorf("row %v", r)
		}
	}
}

func TestOverlappingRunFilesAreRefusedNotCountedTwice(t *testing.T) {
	d := at(time.March, 2)
	one, two := fillInput(validFill("f-1", 5, d)), fillInput(validFill("f-2", 3, d))
	// A resumed run's file starts with every record of the run it resumes.
	first := load(t, "first.log", build(t, one))
	resumed := load(t, "resumed.log", build(t, one, two))
	// An independent run shares no record, even where its ids repeat.
	other := load(t, "other.log", build(t, fillInput(validFill("g-1", 9, d))))

	reports := map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
		"orders": journalquery.Orders, "fills": journalquery.Fills, "refusals": journalquery.Refusals,
		"positions": journalquery.Positions, "reconcile": journalquery.Reconcile, "totals": journalquery.Totals,
	}
	for name, report := range reports {
		_, err := report([]journalquery.Journal{first, resumed}, journalquery.Filter{})
		if err == nil || !strings.Contains(err.Error(), "first.log") || !strings.Contains(err.Error(), "resumed.log") || !strings.Contains(err.Error(), "overlap") {
			t.Errorf("%s: overlapping files were not refused by name: %v", name, err)
		}
		if _, err := report([]journalquery.Journal{first, other}, journalquery.Filter{}); !ranOrComparedNothing(name, err) {
			t.Errorf("%s: independent runs were refused: %v", name, err)
		}
	}
}

// ranOrComparedNothing accepts a report that ran and, for reconcile alone,
// the refusal to call a check with no snapshot to compare a pass.
func ranOrComparedNothing(report string, err error) bool {
	return err == nil || (report == "reconcile" && strings.Contains(err.Error(), "compared nothing"))
}

func configRec() rec {
	return rec{journal.KindInput, event.ConfigurationEventType, event.ConfigurationSchemaVersion, at(time.January, 1), map[string]string{"name": "baseline"}}
}

func overlapReports() map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error) {
	return map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
		"orders": journalquery.Orders, "fills": journalquery.Fills, "refusals": journalquery.Refusals,
		"positions": journalquery.Positions, "reconcile": journalquery.Reconcile, "totals": journalquery.Totals,
	}
}

func TestIndependentRunsWithTheSameConfigurationCombine(t *testing.T) {
	d, later := at(time.March, 2), at(time.April, 2)
	cfg := configRec()
	pairs := map[string][2][]rec{
		"only the configuration is shared":            {{cfg, fillInput(validFill("f-1", 5, d))}, {cfg, fillInput(validFill("g-1", 9, later))}},
		"the same id and payload at other times":      {{cfg, fillInput(validFill("f-1", 5, d))}, {cfg, fillInput(validFill("f-1", 5, later))}},
		"a first record in common, then they part":    {{cfg, fillInput(validFill("f-1", 5, d)), fillInput(validFill("f-2", 3, d))}, {cfg, fillInput(validFill("f-1", 5, d)), fillInput(validFill("g-2", 4, d))}},
		"a run holding nothing but its configuration": {{cfg}, {cfg, fillInput(validFill("f-1", 5, d))}},
	}
	for name, pair := range pairs {
		a, b := load(t, "a.log", build(t, pair[0]...)), load(t, "b.log", build(t, pair[1]...))
		for report, run := range overlapReports() {
			if _, err := run([]journalquery.Journal{a, b}, journalquery.Filter{}); !ranOrComparedNothing(report, err) {
				t.Errorf("%s, %s: independent runs were refused: %v", name, report, err)
			}
		}
	}
}

func TestAResumedLogNamedWithItsPredecessorIsRefused(t *testing.T) {
	d := at(time.March, 2)
	cfg := configRec()
	first := load(t, "first.log", build(t, cfg, fillInput(validFill("f-1", 5, d))))
	resumed := load(t, "resumed.log", build(t, cfg, fillInput(validFill("f-1", 5, d)), fillInput(validFill("f-2", 3, d))))
	for report, run := range overlapReports() {
		for _, order := range [][]journalquery.Journal{{first, resumed}, {resumed, first}} {
			_, err := run(order, journalquery.Filter{})
			if err == nil || !strings.Contains(err.Error(), "first.log") || !strings.Contains(err.Error(), "resumed.log") || !strings.Contains(err.Error(), "overlap") {
				t.Errorf("%s: a resumed log and its predecessor were not refused by name: %v", report, err)
			}
		}
	}
}

func TestTwoCopiesOfOneJournalAreRefused(t *testing.T) {
	d := at(time.March, 2)
	data := build(t, configRec(), fillInput(validFill("f-1", 5, d)))
	a, b := load(t, "run.jsonl", data), load(t, "copy.jsonl", data)
	for report, run := range overlapReports() {
		_, err := run([]journalquery.Journal{a, b}, journalquery.Filter{})
		if err == nil || !strings.Contains(err.Error(), "run.jsonl") || !strings.Contains(err.Error(), "copy.jsonl") {
			t.Errorf("%s: copies of one journal were not refused by name: %v", report, err)
		}
	}
}

func TestPositionsThatWouldOverflowAreRefusedNotWrapped(t *testing.T) {
	d := at(time.March, 2)
	cases := map[string][]rec{
		"a rename onto a held id": {
			snap(at(time.January, 1), event.AccountPosition{InstrumentID: "NEW", Quantity: 1}, event.AccountPosition{InstrumentID: "OLD", Quantity: math.MaxInt64}),
			symbolChange("OLD", "NEW", d),
		},
		"a fill past the largest position": {
			snap(at(time.January, 1), event.AccountPosition{InstrumentID: "AAA", Quantity: math.MaxInt64}),
			fillInput(validFill("f-1", 1, d)),
		},
	}
	for name, recs := range cases {
		js := []journalquery.Journal{load(t, "j", build(t, recs...))}
		for report, run := range map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
			"positions": journalquery.Positions, "reconcile": journalquery.Reconcile,
		} {
			if _, err := run(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "overflow") {
				t.Errorf("%s, %s: want an overflow refusal, got %v", name, report, err)
			}
		}
	}
}

// TestTheSafetyVersionsOfOrderRecordsAreRead: a sale back to 98% is
// strategy.order.planned version 2, and figures with a broker snapshot or a
// safety refusal reason are risk.order.released and risk.order.refused
// version 3; every report reads them. A record at an earlier version carrying
// a later version's field, and a version this build does not know, are
// refused, never guessed at (ADR 0015).
func TestTheSafetyVersionsOfOrderRecordsAreRead(t *testing.T) {
	d := at(time.March, 2)
	sale := plannedPayload("AAA", "sell")
	sale.TargetWeight, sale.NetLiquidation, sale.HeldQuantity, sale.ReferencePrice, sale.LimitPrice = 0.98, 8530, 147, 90, 85.5
	sale.Quantity = 54
	sale.Sale = event.SaleFigures{
		Kind: event.SaleKindIncident, Trigger: event.SaleTriggerExit, SnapshotID: "broker-snapshot:1",
		KeepShares: 93, ToTarget: 54, WithinHolding: 147, NeverBelow: 54,
	}
	broker := figures
	broker.BrokerSet, broker.BrokerHolding, broker.RiskReducing = true, 147, true
	released := event.OrderReleasedPayload{AccountID: "A", ClientOrderID: "c", PlanID: "p", Tag: "t", InstrumentID: "AAA", Side: "sell", Quantity: 54, LimitPrice: 85.5, Mode: "paper", Figures: broker}
	refused := event.OrderRefusedPayload{AccountID: "A", ClientOrderID: "k", PlanID: "p", Tag: "t", InstrumentID: "AAA", Side: "sell", Quantity: 1, LimitPrice: 3, Reason: event.RefusedKillSwitch, Detail: "d", Figures: figures}
	cashReleased := released
	cashReleased.Side, cashReleased.Figures = "buy", cashFigures
	iraRefused := refused
	iraRefused.Reason = event.RefusedInsufficientCash
	input := rec{journal.KindInput, "other.thing", 1, d, 1}
	js := []journalquery.Journal{load(t, "j", build(t, input,
		dec(event.OrderPlannedEventType, event.OrderPlannedSafetyVersion, d, sale),
		dec(event.OrderReleasedEventType, event.OrderReleasedSafetyVersion, d, released),
		dec(event.OrderRefusedEventType, event.OrderRefusedSafetyVersion, d, refused),
	))}
	if orders, err := journalquery.Orders(js, journalquery.Filter{}); err != nil || len(orders.Rows) == 0 {
		t.Fatalf("orders: %v %+v", err, orders)
	}
	r, err := journalquery.Refusals(js, journalquery.Filter{})
	if err != nil || len(r.Rows) != 1 || cell(r, 0, "reason") != event.RefusedKillSwitch {
		t.Fatalf("refusals: %v %+v", err, r)
	}

	for name, bad := range map[string]rec{
		"a sale at version 1":            dec(event.OrderPlannedEventType, event.OrderPlannedSchemaVersion, d, sale),
		"broker figures at version 2":    dec(event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, d, released),
		"a safety reason at version 2":   dec(event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, d, refused),
		"cash figures at version 1":      dec(event.OrderReleasedEventType, 1, d, cashReleased),
		"an IRA reason at version 1":     dec(event.OrderRefusedEventType, 1, d, iraRefused),
		"planned at an unknown version":  dec(event.OrderPlannedEventType, event.OrderPlannedSafetyVersion+1, d, sale),
		"released at an unknown version": dec(event.OrderReleasedEventType, event.OrderReleasedSafetyVersion+1, d, released),
		"refused at an unknown version":  dec(event.OrderRefusedEventType, event.OrderRefusedPendingCashVersion+1, d, refused),
	} {
		js := []journalquery.Journal{load(t, "j", build(t, input, bad))}
		if _, err := journalquery.Orders(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "record 2") {
			t.Errorf("%s: accepted or unnamed: %v", name, err)
		}
	}
}

// The target-weight account's record versions (ADR 0026): account.snapshot
// version 4 (positions and cash), execution.fill version 5 (a limit order's
// execution, by side), execution.order.lifecycle version 2 (a limit order's
// state change) and risk.order.released/refused version 2 (account-wide
// exposure and the IRA cash check).
func cashSnap(when time.Time, p ...event.AccountPosition) rec {
	return rec{journal.KindInput, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, when, event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: "I", AsOf: when, NetLiquidation: 100, Currency: "USD", Positions: p},
		SettledCash:                     40, TotalCash: 50,
	}}
}

func limitFill(id, side string, qty int64, commission float64, when time.Time) rec {
	return rec{journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, when, event.TargetWeightFillPayload{
		AccountID: "I", ClientOrderID: "c-" + id, FillID: id, InstrumentID: "AAA", Side: side, Quantity: qty, Price: 2, Commission: commission, FilledAt: when,
	}}
}

func limitLifecycle(when time.Time) rec {
	return rec{journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, when, event.LimitOrderLifecyclePayload{
		AccountID: "I", ClientOrderID: "c1", OrderID: "o1", InstrumentID: "AAA", Side: "buy", Status: "submitted", Quantity: 5, LimitPrice: 2.5, OccurredAt: when,
	}}
}

var cashFigures = event.RiskFigures{NetLiquidation: 100, ReferencePrice: 2.5, CashChecked: true, BuyCost: 12.5, CashAvailable: 40}

func TestTheTargetWeightAccountsRecordVersionsAreRead(t *testing.T) {
	d := at(time.March, 2)
	data := build(t,
		cashSnap(at(time.January, 1), event.AccountPosition{InstrumentID: "AAA", Quantity: 10}),
		limitFill("f-1", "buy", 5, 0.1, d),
		limitFill("f-2", "sell", 3, 0.2, d),
		// A repeated delivery of f-1, its instant written in another offset.
		limitFill("f-1", "buy", 5, 0.1, d.In(time.FixedZone("EST", -5*3600))),
		limitLifecycle(d),
		dec(event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, d, event.OrderReleasedPayload{AccountID: "I", ClientOrderID: "c1", PlanID: "p", Tag: "t", InstrumentID: "AAA", Side: "buy", Quantity: 5, LimitPrice: 2.5, Mode: "paper", Figures: cashFigures}),
		dec(event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, d, event.OrderRefusedPayload{AccountID: "I", ClientOrderID: "c2", PlanID: "p", InstrumentID: "AAA", Side: "buy", Quantity: 50, LimitPrice: 2.5, Reason: event.RefusedInsufficientCash, Detail: "d", Figures: cashFigures}),
		cashSnap(at(time.March, 3), event.AccountPosition{InstrumentID: "AAA", Quantity: 12}),
	)
	js := []journalquery.Journal{load(t, "ira", data)}
	f, err := journalquery.NewFilter("I", "AAA", "", "")
	if err != nil {
		t.Fatal(err)
	}

	fills, err := journalquery.Fills(js, f)
	if err != nil || len(fills.Rows) != 2 {
		t.Fatalf("fills: %v %v", fills.Rows, err)
	}
	if got := strings.Join(fills.Rows[1], " "); got != "ira 3 2026-03-02 AAA sell -3 2.00 0.20" {
		t.Errorf("sell fill row: %q", got)
	}
	tot, err := journalquery.Totals(js, f)
	if err != nil || strings.Join(tot.Rows[len(tot.Rows)-1], " ") != "TOTAL all 2 5 3 0.30" {
		t.Errorf("totals: %v %v", tot.Rows, err)
	}

	orders, err := journalquery.Orders(js, f)
	if err != nil || len(orders.Rows) != 3 {
		t.Fatalf("orders: %v %v", orders.Rows, err)
	}
	if got := strings.Join(orders.Rows[0], " "); got != "ira 5 2026-03-02 I AAA lifecycle-submitted buy 5 2.50 c1" {
		t.Errorf("limit lifecycle row: %q", got)
	}
	refusals, err := journalquery.Refusals(js, f)
	if err != nil || len(refusals.Rows) != 1 || cell(refusals, 0, "reason") != event.RefusedInsufficientCash {
		t.Errorf("refusals: %v %v", refusals.Rows, err)
	}

	pos, err := journalquery.Positions(js, f)
	if err != nil || len(pos.Rows) != 2 || cell(pos, 0, "kind") != "buy" || cell(pos, 0, "position") != "15" || cell(pos, 1, "position") != "12" {
		t.Errorf("positions: %v %v", pos.Rows, err)
	}
	rec, err := journalquery.Reconcile(js, f)
	if err != nil || len(rec.Rows) != 1 || cell(rec, 0, "status") != "ok" || cell(rec, 0, "account") != "I" {
		t.Errorf("reconcile: %v %v", rec.Rows, err)
	}
}

// A dry-run engine's synthetic terminal report (execution.order.lifecycle
// version 3, ADR 0026, point 3, "Mode", amended 2026-10-17) is shown as synthetic, and is never
// an execution: no fill, commission or position change follows from it.
func TestASyntheticReportIsShownAndCountsNoFill(t *testing.T) {
	d := at(time.March, 2)
	synthetic := event.LimitOrderLifecyclePayload{
		AccountID: "I", ClientOrderID: "c1", InstrumentID: "AAA", Side: "sell", Status: event.OrderLifecycleStatusCanceled,
		Quantity: 5, LimitPrice: 2.5, OccurredAt: d, Message: "dry-run: never sent", Synthetic: true,
	}
	data := build(t,
		cashSnap(at(time.January, 1), event.AccountPosition{InstrumentID: "AAA", Quantity: 10}),
		dec(event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, d, event.OrderReleasedPayload{AccountID: "I", ClientOrderID: "c1", PlanID: "p", Tag: "t", InstrumentID: "AAA", Side: "sell", Quantity: 5, LimitPrice: 2.5, Mode: "dry-run", Figures: cashFigures}),
		dec(event.OrderLifecycleEventType, event.OrderLifecycleSyntheticVersion, d, synthetic),
	)
	js := []journalquery.Journal{load(t, "dry", data)}
	orders, err := journalquery.Orders(js, journalquery.Filter{})
	if err != nil || len(orders.Rows) != 2 {
		t.Fatalf("orders: %v %v", orders.Rows, err)
	}
	if got := strings.Join(orders.Rows[1], " "); got != "dry 3 2026-03-02 I AAA synthetic-lifecycle-canceled sell 5 2.50 c1" {
		t.Errorf("synthetic lifecycle row: %q", got)
	}
	fills, err := journalquery.Fills(js, journalquery.Filter{})
	if err != nil || len(fills.Rows) != 0 {
		t.Errorf("fills: %v %v", fills.Rows, err)
	}
	tot, err := journalquery.Totals(js, journalquery.Filter{})
	if err != nil || strings.Join(tot.Rows[len(tot.Rows)-1], " ") != "TOTAL all 0 0 0 0.00" {
		t.Errorf("totals: %v %v", tot.Rows, err)
	}
	pos, err := journalquery.Positions(js, journalquery.Filter{})
	if err != nil || len(pos.Rows) != 0 {
		t.Errorf("positions: %v %v", pos.Rows, err)
	}

	// Each version holds only its own kind of report.
	venue := synthetic
	venue.Synthetic, venue.OrderID = false, "o1"
	for name, r := range map[string]rec{
		"synthetic at version 2": dec(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, d, synthetic),
		"venue's at version 3":   dec(event.OrderLifecycleEventType, event.OrderLifecycleSyntheticVersion, d, venue),
	} {
		js := []journalquery.Journal{load(t, "j", build(t, rec{journal.KindInput, "other.thing", 1, d, 1}, r))}
		if _, err := journalquery.Orders(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "version 3") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRiskRecordsOfTheEarlierVersionStillRead(t *testing.T) {
	d := at(time.March, 2)
	data := build(t, rec{journal.KindInput, "other.thing", 1, d, 1},
		dec(event.OrderReleasedEventType, 1, d, event.OrderReleasedPayload{AccountID: "A", ClientOrderID: "c", PlanID: "p", Tag: "t", InstrumentID: "AAA", Side: "buy", Quantity: 1, LimitPrice: 3, Mode: "paper", Figures: figures}),
		dec(event.OrderRefusedEventType, 1, d, event.OrderRefusedPayload{AccountID: "A", ClientOrderID: "c", PlanID: "p", InstrumentID: "AAA", Side: "sell", Quantity: 1, LimitPrice: 3, Reason: "order-size", Detail: "d", Figures: figures}),
	)
	orders, err := journalquery.Orders([]journalquery.Journal{load(t, "j", data)}, journalquery.Filter{})
	if err != nil || len(orders.Rows) != 2 {
		t.Errorf("version 1 risk records: %v %v", orders.Rows, err)
	}
}

func TestTheNextVersionOfEveryReadRecordIsRefused(t *testing.T) {
	d := at(time.March, 2)
	for _, r := range []rec{
		{journal.KindInput, event.FillEventType, event.FillTargetWeightFractionalVersion + 1, d, map[string]any{}},
		{journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleFractionalSyntheticVersion + 1, d, map[string]any{}},
		{journal.KindDecision, event.OrderPlannedEventType, event.OrderPlannedFractionalVersion + 1, d, map[string]any{}},
		{journal.KindDecision, event.OrderReleasedEventType, event.OrderReleasedFractionalVersion + 1, d, map[string]any{}},
		{journal.KindDecision, event.OrderRefusedEventType, event.OrderRefusedFractionalVersion + 1, d, map[string]any{}},
		{journal.KindInput, event.AccountSnapshotEventType, event.AccountSnapshotFractionalCashVersion + 1, d, map[string]any{}},
	} {
		js := []journalquery.Journal{load(t, "j", build(t, rec{journal.KindInput, "other.thing", 1, d, 1}, r))}
		if _, err := journalquery.Orders(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "does not read") {
			t.Errorf("%s v%d: %v", r.typ, r.schema, err)
		}
	}
}

func TestInvalidTargetWeightRecordsAreRefused(t *testing.T) {
	d := at(time.March, 2)
	for name, r := range map[string]rec{
		"fill":      {journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, d, event.TargetWeightFillPayload{AccountID: "I"}},
		"lifecycle": {journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, d, event.LimitOrderLifecyclePayload{AccountID: "I"}},
		"snapshot":  {journal.KindInput, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, d, event.AccountCashSnapshotPayload{SettledCash: 1}},
	} {
		js := []journalquery.Journal{load(t, "j", build(t, r))}
		if _, err := journalquery.Orders(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "record 1:") || strings.Contains(err.Error(), "does not read") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAFillIDUsedByBothFillVersionsIsRefused(t *testing.T) {
	d := at(time.March, 2)
	data := build(t, fillInput(validFill("f-1", 5, d)), limitFill("f-1", "buy", 5, 0.5, d))
	if _, err := journalquery.Fills([]journalquery.Journal{load(t, "j", data)}, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "f-1") {
		t.Errorf("one fill id in two versions: %v", err)
	}
}

func TestTurtleFillIsRefusedWhenAnotherAccountOnlyAppearsOnAFill(t *testing.T) {
	d := at(time.March, 2)
	// Account A has a snapshot; account I appears only on a limit fill. A
	// Turtle fill names no account, so with two accounts in the journal it
	// has no account to be placed in.
	js := []journalquery.Journal{load(t, "j", build(t, snap(d), limitFill("f-1", event.OrderSideBuy, 1, 0, d), fillInput(validFill("f-2", 3, d))))}
	for name, fn := range map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
		"positions": journalquery.Positions, "reconcile": journalquery.Reconcile,
	} {
		if _, err := fn(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "cannot be attributed") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A sale back to 98% the gate withheld (risk.sale.withheld) is listed with
// the refusals, by its reason, the lapse of a withheld sale included, and is
// read by every report; a version this build does not know, or an invalid
// record, is refused.
func TestWithheldSalesAreListedWithTheRefusals(t *testing.T) {
	d := at(time.March, 2)
	withheld := event.SaleWithheldPayload{
		AccountID: "A", Tag: "engine-state:A:x", ClientOrderID: "s", InstrumentID: "AAA", Kind: event.SaleKindIncident,
		Trigger: event.SaleTriggerExit, Reason: event.SaleWithheldStaleBrokerSnapshot, Detail: "stale", SnapshotID: "broker:1", Alert: true,
	}
	lapsed := withheld
	lapsed.Reason, lapsed.Detail, lapsed.Alert = event.SaleWithheldLapsed, "the exit is no longer refused", false
	input := rec{journal.KindInput, "other.thing", 1, d, 1}
	js := []journalquery.Journal{load(t, "j", build(t, input,
		dec(event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, d, withheld),
		dec(event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, d, lapsed),
	))}
	r, err := journalquery.Refusals(js, journalquery.Filter{})
	if err != nil || len(r.Rows) != 2 {
		t.Fatalf("refusals: %v %+v", err, r)
	}
	if cell(r, 0, "event") != "sale-withheld" || cell(r, 0, "reason") != event.SaleWithheldStaleBrokerSnapshot || cell(r, 0, "instrument") != "AAA" || cell(r, 0, "account") != "A" {
		t.Errorf("withheld row: %v", r.Rows[0])
	}
	if cell(r, 1, "reason") != event.SaleWithheldLapsed || cell(r, 1, "detail") != "the exit is no longer refused" {
		t.Errorf("lapsed row: %v", r.Rows[1])
	}
	if r, _ := journalquery.Refusals(js, journalquery.Filter{Instrument: "BBB"}); len(r.Rows) != 0 {
		t.Errorf("an instrument filter: %v", r.Rows)
	}
	if o, err := journalquery.Orders(js, journalquery.Filter{}); err != nil || len(o.Rows) != 0 {
		t.Errorf("a withheld sale is no order: %v %v", o.Rows, err)
	}
	for name, bad := range map[string]rec{
		"unknown version": dec(event.SaleWithheldEventType, event.SaleWithheldSchemaVersion+1, d, withheld),
		"invalid":         dec(event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, d, event.SaleWithheldPayload{AccountID: "A"}),
	} {
		js := []journalquery.Journal{load(t, "j", build(t, input, bad))}
		if _, err := journalquery.Refusals(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "record 2") {
			t.Errorf("%s: accepted or unnamed: %v", name, err)
		}
	}
}

// TestTheGlidePathRecordVersionsAreRead covers the glide path's record
// versions (ADR 0026 point 5): strategy.target-weight.configuration version
// 3 (the glide section) and strategy.targets.decided version 3 (the glide
// figures and each target's sleeve shares). No report draws a figure from
// either, so a journal holding them reads in every report, and two runs
// that begin with the same version-3 configuration are not taken for one.
func TestTheGlidePathRecordVersionsAreRead(t *testing.T) {
	d := at(time.March, 2)
	configuration := rec{journal.KindInput, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, at(time.January, 1),
		map[string]any{"strategy_id": event.TargetWeightStrategyMomentum, "glide": map[string]any{"birth": "1972-02", "retirement_age_years": 65}}}
	decided := dec(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, d, event.TargetsDecidedPayload{
		AccountID: "I", StrategyID: event.TargetWeightStrategyMomentum, DecisionTime: d, SessionPeriodEnd: d, Outcome: event.TargetsOutcomeDecided,
		Targets: []event.TargetWeight{{InstrumentID: "AAA", Weight: 0.5, Shares: []event.SleeveShare{{Sleeve: "momentum", Weight: 0.5}}}},
		Glide:   event.GlideFigures{RebalanceSession: "2026-03-03", AgeMonths: 649, BondShare: 0.32},
	})
	run := func(fillID string) []byte {
		return build(t, configuration, cashSnap(at(time.January, 1)), decided, limitFill(fillID, "buy", 5, 0.1, d))
	}
	js := []journalquery.Journal{load(t, "a", run("f-a")), load(t, "b", run("f-b"))}
	for report, query := range map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
		"fills": journalquery.Fills, "totals": journalquery.Totals, "orders": journalquery.Orders,
		"refusals": journalquery.Refusals, "positions": journalquery.Positions, "reconcile": journalquery.Reconcile,
	} {
		if _, err := query(js, journalquery.Filter{}); !ranOrComparedNothing(report, err) {
			t.Errorf("%s: %v", report, err)
		}
	}
	fills, err := journalquery.Fills(js, journalquery.Filter{})
	if err != nil || len(fills.Rows) != 2 {
		t.Errorf("fills across both runs: %v %v", fills.Rows, err)
	}
}

// TestTheCompactUniverseInputsAreRead: a momentum journal records its
// configuration, version 6, declaring the compact universe input, then that
// input: the universe snapshot, the ranked members' close histories and any
// session retraction. No report draws a figure from them, so a journal
// holding them reads in every report, its fills unchanged.
func TestTheCompactUniverseInputsAreRead(t *testing.T) {
	d := at(time.March, 2)
	data := build(t,
		rec{journal.KindInput, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, at(time.January, 1),
			map[string]any{"strategy_id": event.TargetWeightStrategyMomentum, "momentum": map[string]any{"universe_input": event.UniverseInputCompact}}},
		rec{journal.KindInput, event.SessionRetractedEventType, event.SessionRetractedSchemaVersion, at(time.January, 1),
			event.SessionRetractedPayload{Source: "marketdata-retraction", Session: "2026-03-03", Reason: "storm"}},
		rec{journal.KindInput, event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, d,
			event.UniverseSnapshotPayload{Session: "2026-03-02", Source: "massive", Rows: []event.UniverseRow{{InstrumentID: "AAA", Type: "CS", Close: 2, Volume: 1}}}},
		rec{journal.KindInput, event.CloseHistoryEventType, event.CloseHistorySchemaVersion, d,
			event.CloseHistoryPayload{InstrumentID: "AAA", Session: "2026-03-02", AdjustedBase: "2026-01-02", Closes: []event.SessionClose{{Session: "2026-03-02", Close: 2}}}},
		cashSnap(at(time.January, 1)),
		limitFill("f-1", "buy", 5, 0.1, d),
	)
	js := []journalquery.Journal{load(t, "ira", data)}
	for report, query := range map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
		"fills": journalquery.Fills, "totals": journalquery.Totals, "orders": journalquery.Orders,
		"refusals": journalquery.Refusals, "positions": journalquery.Positions, "reconcile": journalquery.Reconcile,
	} {
		if _, err := query(js, journalquery.Filter{}); !ranOrComparedNothing(report, err) {
			t.Errorf("%s: %v", report, err)
		}
	}
	if fills, err := journalquery.Fills(js, journalquery.Filter{}); err != nil || len(fills.Rows) != 1 {
		t.Errorf("fills: %v %v", fills.Rows, err)
	}
}

// TestSkippedTradesAreListedWithTheOrders: a trade the order planner sized
// but could not order is recorded so no planned trade disappears silently
// (ADR 0026, point 8), so the orders report lists it as skipped, with its
// side, no shares ordered, its reference price, and in the note its reason
// and the unrounded shares the rule asked for. Filters apply to it as to any
// order; a version this build does not know, or an invalid record, is
// refused.
func TestSkippedTradesAreListedWithTheOrders(t *testing.T) {
	d := at(time.March, 2)
	skipped := event.OrderSkippedPayload{AccountID: "A", Tag: "t", ClientOrderID: "c-1", InstrumentID: "AAA", Side: event.OrderSideBuy,
		Phase: event.OrderSideBuy, Reason: event.OrderSkippedBelowOneShare, Shares: 0.4, TargetWeight: 0.5, NetLiquidation: 1, ReferencePrice: 3}
	input := rec{journal.KindInput, "other.thing", 1, d, 1}
	js := []journalquery.Journal{load(t, "j", build(t, input, dec(event.OrderSkippedEventType, event.OrderSkippedSchemaVersion, d, skipped)))}
	o, err := journalquery.Orders(js, journalquery.Filter{})
	if err != nil || len(o.Rows) != 1 {
		t.Fatalf("orders: %v %+v", err, o)
	}
	for column, want := range map[string]string{"event": "skipped", "account": "A", "instrument": "AAA", "side": "buy", "quantity": "0", "price": "3.00",
		"note": "below-one-share: 0.4 shares wanted"} {
		if got := cell(o, 0, column); got != want {
			t.Errorf("%s = %q, want %q", column, got, want)
		}
	}
	if o, _ := journalquery.Orders(js, journalquery.Filter{Instrument: "BBB"}); len(o.Rows) != 0 {
		t.Errorf("an instrument filter: %v", o.Rows)
	}
	for name, bad := range map[string]rec{
		"unknown version": dec(event.OrderSkippedEventType, event.OrderSkippedSchemaVersion+1, d, skipped),
		"invalid":         dec(event.OrderSkippedEventType, event.OrderSkippedSchemaVersion, d, event.OrderSkippedPayload{AccountID: "A"}),
	} {
		js := []journalquery.Journal{load(t, "j", build(t, input, bad))}
		if _, err := journalquery.Orders(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "record 2") {
			t.Errorf("%s: accepted or unnamed: %v", name, err)
		}
	}
}

// TestSkippedSharesInTheOrdersNoteAreBounded: the recorded shares are the
// planner's unrounded float (ADR 0026, point 8), so 0.1+0.2 is noted to the
// schema's six decimals, not as binary noise.
func TestSkippedSharesInTheOrdersNoteAreBounded(t *testing.T) {
	d := at(time.March, 2)
	a, b := 0.1, 0.2
	skipped := event.OrderSkippedPayload{AccountID: "A", Tag: "t", ClientOrderID: "c-1", InstrumentID: "AAA", Side: event.OrderSideBuy,
		Phase: event.OrderSideBuy, Reason: event.OrderSkippedBelowOneShare, Shares: a + b, TargetWeight: 0.5, NetLiquidation: 1, ReferencePrice: 3}
	input := rec{journal.KindInput, "other.thing", 1, d, 1}
	js := []journalquery.Journal{load(t, "j", build(t, input, dec(event.OrderSkippedEventType, event.OrderSkippedSchemaVersion, d, skipped)))}
	o, err := journalquery.Orders(js, journalquery.Filter{})
	if err != nil || len(o.Rows) != 1 {
		t.Fatalf("orders: %v %+v", err, o)
	}
	if got, want := cell(o, 0, "note"), "below-one-share: 0.3 shares wanted"; got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}

func reconcilable() []rec {
	d := at(time.March, 2)
	return []rec{
		snap(d),
		fillRec(event.FillKindEntry, "AAA", 5, 0.1, d),
		snap(at(time.March, 3), event.AccountPosition{InstrumentID: "AAA", Quantity: 5}),
	}
}

func TestReconcileOfACompleteLogStillSucceeds(t *testing.T) {
	js := []journalquery.Journal{load(t, "run.log", buildLog(t, false, reconcilable()...))}
	tab, err := journalquery.Reconcile(js, journalquery.Filter{})
	if err != nil || len(tab.Rows) != 1 || cell(tab, 0, "status") != "ok" {
		t.Fatalf("complete log: %v %v", tab.Rows, err)
	}
}

func TestReconcileOfALogWithNothingCommittedFailsInWords(t *testing.T) {
	var buf syncBuffer
	if _, err := journal.CreateLog(&buf, journal.NewLogHeader("h", "s")); err != nil {
		t.Fatal(err)
	}
	js := []journalquery.Journal{load(t, "empty.log", buf.Bytes())}
	_, err := journalquery.Reconcile(js, journalquery.Filter{})
	if err == nil || !strings.Contains(err.Error(), "empty.log") || !strings.Contains(err.Error(), "holds no committed record") {
		t.Fatalf("want a refusal naming the empty log, got %v", err)
	}
}

func TestReconcileOfACutShortLogNamesWhereItWasCut(t *testing.T) {
	data := buildLog(t, true, reconcilable()...)
	js := []journalquery.Journal{load(t, "cut.log", data)}
	if js[0].Recovery == nil || js[0].Recovery.Torn == nil {
		t.Fatal("fixture is not cut short")
	}
	_, err := journalquery.Reconcile(js, journalquery.Filter{})
	want := fmt.Sprintf("after record %d (byte %d)", len(js[0].Records), js[0].Recovery.Torn.Offset)
	if err == nil || !strings.Contains(err.Error(), "cut.log") || !strings.Contains(err.Error(), "cut short") || !strings.Contains(err.Error(), want) {
		t.Fatalf("want a refusal naming %q, got %v", want, err)
	}
}

func TestReconcileOfALogCutMidRecordNamesWhereItWasCut(t *testing.T) {
	data := buildLog(t, false, reconcilable()...)
	data = data[:len(data)-7]
	js := []journalquery.Journal{load(t, "mid.log", data)}
	_, err := journalquery.Reconcile(js, journalquery.Filter{})
	if err == nil || !strings.Contains(err.Error(), "cut short") || !strings.Contains(err.Error(), "mid.log") {
		t.Fatalf("want a cut-short refusal, got %v", err)
	}
}

func TestReconcileThatComparedNothingFails(t *testing.T) {
	d := at(time.March, 2)
	js := []journalquery.Journal{load(t, "nosnap.jsonl", build(t, fillRec(event.FillKindEntry, "AAA", 5, 0.1, d)))}
	_, err := journalquery.Reconcile(js, journalquery.Filter{})
	if err == nil || !strings.Contains(err.Error(), "compared nothing") {
		t.Fatalf("want a compared-nothing refusal, got %v", err)
	}
}

func TestLoadKeepsTheVerificationResult(t *testing.T) {
	j := load(t, "j", build(t, reconcilable()...))
	if j.Verification == nil || j.Verification.FinalRecordHash == "" || j.Verification.RecordCount != 3 {
		t.Fatalf("verification not kept: %+v", j.Verification)
	}
	if err := j.CheckFinalHash(j.Verification.FinalRecordHash); err != nil {
		t.Errorf("the recorded final hash was refused: %v", err)
	}
	err := j.CheckFinalHash("deadbeef")
	if err == nil || !strings.Contains(err.Error(), "final record hash") || !strings.Contains(err.Error(), "deadbeef") {
		t.Errorf("a final-hash mismatch was not reported: %v", err)
	}
	if (journalquery.Journal{}).FinalRecordHash() != "" {
		t.Error("a journal that was never loaded states a final hash")
	}
	if !strings.Contains(err.Error(), "end-of-run journal") || strings.Contains(err.Error(), "missing") {
		t.Errorf("the mismatch must name the kind of file and never claim missing records: %v", err)
	}
}

func TestCheckFinalHashRefusesADurableLog(t *testing.T) {
	l := load(t, "l", buildLog(t, false, reconcilable()...))
	for _, want := range []string{l.Recovery.FinalRecordHash, "deadbeef"} {
		err := l.CheckFinalHash(want)
		if err == nil || !strings.Contains(err.Error(), "the registry anchors the end-of-run journal's final record hash; this file is a durable log") {
			t.Errorf("a durable log must be refused as the wrong kind of file for the anchor, got %v", err)
		}
	}
}

func TestReconcileRefusesInputRecordedAfterAStop(t *testing.T) {
	d := at(time.March, 2)
	stop := rec{journal.KindInput, event.AdapterRunStoppedEventType, 1, d, event.AdapterRunStoppedPayload{Reason: event.AdapterRunStoppedReasonDelisted, InstrumentID: "AAA", Detail: "delisted at the venue"}}
	base := reconcilable()
	recs := []rec{base[0], base[1], stop, base[2]}
	js := []journalquery.Journal{load(t, "stopped.jsonl", build(t, recs...))}
	_, err := journalquery.Reconcile(js, journalquery.Filter{})
	if err == nil || !strings.Contains(err.Error(), "after the run was stopped") {
		t.Fatalf("want a refusal about input after the stop, got %v", err)
	}
}

func TestReconcileOfAnAllCashAccountCountsEmptyAgainstEmptyAsCompared(t *testing.T) {
	js := []journalquery.Journal{load(t, "cash.jsonl", build(t, snap(at(time.March, 2)), snap(at(time.March, 3))))}
	tab, err := journalquery.Reconcile(js, journalquery.Filter{})
	if err != nil || len(tab.Rows) != 1 || cell(tab, 0, "status") != "ok" || cell(tab, 0, "fills") != "0" || cell(tab, 0, "snapshot") != "0" {
		t.Fatalf("an empty snapshot after an empty baseline is a comparison: %v %v", tab.Rows, err)
	}
}

func TestReconcileOfABaselineAloneStillComparedNothing(t *testing.T) {
	js := []journalquery.Journal{load(t, "one.jsonl", build(t, snap(at(time.March, 2))))}
	if _, err := journalquery.Reconcile(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "compared nothing") {
		t.Fatalf("want compared nothing, got %v", err)
	}
}

func TestReconcileReportsFillsAfterTheLastSnapshotAsNotCovered(t *testing.T) {
	recs := append(reconcilable(), fillRec(event.FillKindEntry, "AAA", 2, 0.1, at(time.March, 4)))
	js := []journalquery.Journal{load(t, "trail.jsonl", build(t, recs...))}
	tab, err := journalquery.Reconcile(js, journalquery.Filter{})
	if err == nil || !strings.Contains(err.Error(), "1 fills not yet covered by a snapshot") || len(tab.Rows) != 1 {
		t.Fatalf("want the trailing fill reported with the table, got %v %v", tab.Rows, err)
	}
	var d *journalquery.DisagreementError
	if errors.As(err, &d) {
		t.Errorf("a trailing fill is not a disagreement: %v", err)
	}
	allowed := journalquery.Filter{AllowTrailingFills: true}
	if tab, err := journalquery.Reconcile(js, allowed); err != nil || len(tab.Rows) != 1 {
		t.Fatalf("the trailing fills were allowed: %v %v", tab.Rows, err)
	}
}

func TestReconcileFailsWhenOneJournalAmongSeveralComparesNothing(t *testing.T) {
	good := load(t, "good.jsonl", build(t, reconcilable()...))
	other := load(t, "other.jsonl", build(t, fillRec(event.FillKindEntry, "BBB", 3, 0.1, at(time.April, 2))))
	_, err := journalquery.Reconcile([]journalquery.Journal{good, other}, journalquery.Filter{})
	if err == nil || !strings.Contains(err.Error(), "compared nothing") || !strings.Contains(err.Error(), "other.jsonl") {
		t.Fatalf("want the journal that compared nothing named, got %v", err)
	}
}

func TestCheckFinalHashRefusesAnEmptyAnchor(t *testing.T) {
	j := load(t, "j", build(t, reconcilable()...))
	l := load(t, "l", buildLog(t, false, reconcilable()...))
	for name, journal := range map[string]journalquery.Journal{"journal": j, "log": l, "zero": {}} {
		if err := journal.CheckFinalHash(""); err == nil || !strings.Contains(err.Error(), "no anchor given") {
			t.Errorf("%s: an empty anchor must be an error, got %v", name, err)
		}
	}
}

func TestReconcileRefusesInputRecordedAfterAStopInADurableLog(t *testing.T) {
	d := at(time.March, 2)
	stop := rec{journal.KindInput, event.AdapterRunStoppedEventType, 1, d, event.AdapterRunStoppedPayload{Reason: event.AdapterRunStoppedReasonDelisted, InstrumentID: "AAA", Detail: "delisted at the venue"}}
	base := reconcilable()
	js := []journalquery.Journal{load(t, "stopped.log", buildLog(t, false, base[0], base[1], stop, base[2]))}
	_, err := journalquery.Reconcile(js, journalquery.Filter{})
	if err == nil || !strings.Contains(err.Error(), "after the run was stopped") {
		t.Fatalf("want a refusal about input after the stop, got %v", err)
	}
}

func TestADurableLogMayCompleteOnceAfterAStopButTakeNoOtherInput(t *testing.T) {
	d := at(time.March, 2)
	stop := rec{journal.KindInput, event.AdapterRunStoppedEventType, 1, d, event.AdapterRunStoppedPayload{Reason: event.AdapterRunStoppedReasonDelisted, InstrumentID: "AAA", Detail: "delisted at the venue"}}
	completed := rec{journal.KindInput, event.RunCompletedEventType, 1, d, map[string]any{}}
	decision := dec(event.OrderSkippedEventType, event.OrderSkippedSchemaVersion, d, event.OrderSkippedPayload{AccountID: "A", Tag: "t", ClientOrderID: "c-1", InstrumentID: "AAA", Side: event.OrderSideBuy,
		Phase: event.OrderSideBuy, Reason: event.OrderSkippedBelowOneShare, Shares: 0.5, TargetWeight: 0.5, NetLiquidation: 1, ReferencePrice: 3})
	base := reconcilable()
	ok := load(t, "ok.log", buildLog(t, false, base[1], stop, decision, completed))
	if why := ok.Incomplete(); why != "" {
		t.Errorf("one completion after a stop is allowed, got %q", why)
	}
	twice := load(t, "twice.log", buildLog(t, false, base[1], stop, completed, base[2]))
	if why := twice.Incomplete(); !strings.Contains(why, "after the run was stopped") {
		t.Errorf("input after the completion must be refused, got %q", why)
	}
}

func TestReconcileNamesTrailingFillsBesideDisagreements(t *testing.T) {
	d := at(time.March, 2)
	recs := []rec{snap(d), fillRec(event.FillKindEntry, "AAA", 5, 0.1, d), snap(at(time.March, 3), event.AccountPosition{InstrumentID: "AAA", Quantity: 4}), fillRec(event.FillKindEntry, "AAA", 1, 0.1, at(time.March, 4))}
	js := []journalquery.Journal{load(t, "both.jsonl", build(t, recs...))}
	_, err := journalquery.Reconcile(js, journalquery.Filter{})
	var dis *journalquery.DisagreementError
	if !errors.As(err, &dis) || dis.Trailing != 1 || !strings.Contains(err.Error(), "1 fills not yet covered by a snapshot") {
		t.Fatalf("want the disagreement naming the trailing fill, got %v", err)
	}
}

// ADR 0026 D7-F: owner tables print shares while records retain exact units.
func TestFractionalQueryShowsShares(t *testing.T) {
	p := plannedPayload("AAA", "buy")
	p.QuantityScale, p.Quantity, p.FractionalIncrement = 10000, 72500, "0.0001"
	r := event.OrderReleasedPayload{AccountID: p.AccountID, ClientOrderID: p.ClientOrderID, PlanID: "plan", Tag: p.Tag,
		InstrumentID: p.InstrumentID, Side: p.Side, Quantity: p.Quantity, QuantityScale: p.QuantityScale, LimitPrice: p.LimitPrice,
		Mode: "paper", Send: true, Figures: cashFigures}
	refused := event.OrderRefusedPayload{AccountID: p.AccountID, ClientOrderID: p.ClientOrderID, PlanID: "plan", Tag: p.Tag,
		InstrumentID: p.InstrumentID, Side: p.Side, Quantity: p.Quantity, QuantityScale: p.QuantityScale, LimitPrice: p.LimitPrice,
		Reason: event.RefusedInsufficientCash, Detail: "synthetic cash shortfall", Figures: cashFigures}
	for _, decision := range []rec{
		dec(event.OrderPlannedEventType, p.SchemaVersion(), at(time.March, 2), p),
		dec(event.OrderReleasedEventType, r.SchemaVersion(), at(time.March, 2), r),
		dec(event.OrderRefusedEventType, refused.SchemaVersion(), at(time.March, 2), refused),
	} {
		data := build(t, snap(at(time.March, 1)), decision)
		js := []journalquery.Journal{load(t, "fractional", data)}
		got, err := journalquery.Orders(js, journalquery.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if cell(got, 0, "quantity") != "7.25" {
			t.Fatalf("units shown as shares: %+v", got)
		}
	}
}

func TestFractionalInputQueryShowsShares(t *testing.T) {
	d := at(time.March, 2)
	snapRec := cashSnap(d, event.AccountPosition{InstrumentID: "AAA", Quantity: 72500})
	s := snapRec.payload.(event.AccountCashSnapshotPayload)
	s.QuantityScale = 10000
	snapRec.payload, snapRec.schema = s, s.SchemaVersion()
	fillRec := limitFill("synthetic", "sell", 12500, 0.25, d)
	f := fillRec.payload.(event.TargetWeightFillPayload)
	f.QuantityScale = 10000
	fillRec.payload, fillRec.schema = f, f.SchemaVersion()
	lifeRec := limitLifecycle(d)
	l := lifeRec.payload.(event.LimitOrderLifecyclePayload)
	l.QuantityScale, l.Quantity = 10000, 72500
	lifeRec.payload, lifeRec.schema = l, l.SchemaVersion()
	for name, input := range map[string]rec{"snapshot": snapRec, "fill": fillRec, "lifecycle": lifeRec} {
		t.Run(name, func(t *testing.T) {
			js := []journalquery.Journal{load(t, name, build(t, input))}
			fills, err := journalquery.Fills(js, journalquery.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			if name == "fill" && cell(fills, 0, "quantity") != "-1.25" {
				t.Fatalf("fill units: %+v", fills)
			}
			orders, err := journalquery.Orders(js, journalquery.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			if name == "lifecycle" && cell(orders, 0, "quantity") != "7.25" {
				t.Fatalf("order units: %+v", orders)
			}
		})
	}
	for _, scale := range []int64{0, 1} {
		for _, input := range []rec{snapRec, fillRec, lifeRec} {
			raw, _ := json.Marshal(input.payload)
			var fields map[string]any
			_ = json.Unmarshal(raw, &fields)
			fields["quantity_scale"] = scale
			input.payload = fields
			js := []journalquery.Journal{load(t, "bad-scale", build(t, input))}
			if _, err := journalquery.Fills(js, journalquery.Filter{}); err == nil {
				t.Fatal("accepted fractional version at wrong scale")
			}
		}
	}
	js := []journalquery.Journal{load(t, "fractional", build(t, snapRec, fillRec))}
	for _, query := range []func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){journalquery.Totals, journalquery.Positions, journalquery.Reconcile} {
		if _, err := query(js, journalquery.Filter{}); err == nil {
			t.Fatal("whole-share aggregation accepted fractional units")
		}
	}
}

func TestMalformedFractionalUnitsCannotPassALegacySurface(t *testing.T) {
	d := at(time.March, 2)
	p := map[string]any{"instrument_id": "AAA", "order_id": "synthetic", "tag": "stop", "status": "submitted", "quantity": 7, "stop_price": 1.5, "occurred_at": d, "quantity_scale": "invalid"}
	js := []journalquery.Journal{load(t, "malformed-scale", build(t, snap(d), rec{journal.KindInput, event.OrderLifecycleEventType, 1, d, p}))}
	if _, err := journalquery.Orders(js, journalquery.Filter{}); err == nil {
		t.Fatal("accepted malformed fractional unit metadata")
	}
}

func TestQuantityScaleOnAnUnversionedPayloadIsRefused(t *testing.T) {
	// A payload whose fields do not depend on its version carries no quantity
	// scale; one that claims a scale is refused rather than read as whole shares.
	d := at(time.March, 2)
	r := rec{journal.KindInput, event.KillSwitchEventType, event.KillSwitchSchemaVersion, d,
		map[string]any{"account_id": "A", "as_of": d, "engaged": true, "operator": "owner", "detail": "news",
			"quantity_scale": 10000}}
	js := []journalquery.Journal{load(t, "j", build(t, r))}
	_, err := journalquery.Orders(js, journalquery.Filter{})
	if err == nil || !strings.Contains(err.Error(), "quantity_scale is not supported at this version") {
		t.Fatalf("scale on an unversioned payload accepted or unnamed: %v", err)
	}
}

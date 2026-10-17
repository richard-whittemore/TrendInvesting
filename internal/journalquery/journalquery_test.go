package journalquery_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

	// No Reconcile filter match leaves no rows and no error.
	none, _ := journalquery.NewFilter("B", "", "", "")
	if tab, err := journalquery.Reconcile(js, none); err != nil || len(tab.Rows) != 0 {
		t.Errorf("filtered reconcile: %v %v", tab.Rows, err)
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
	for _, v := range []uint32{event.AccountSnapshotPositionsVersion + 1} {
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

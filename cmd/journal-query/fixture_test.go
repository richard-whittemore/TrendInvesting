package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// ev is one hand-built journal record. The journals these tests query are
// small enough that every expected figure in a test is added up by hand from
// the events listed here, never read back from the code under test.
type ev struct {
	kind    string
	typ     string
	schema  uint32
	at      time.Time
	payload any
}

func day(year int, month time.Month, d int) time.Time {
	return time.Date(year, month, d, 21, 0, 0, 0, time.UTC)
}

func fill(kind, instrument string, qty int64, price, commission float64, at time.Time) ev {
	p := event.FillPayload{
		InstrumentID: instrument, Kind: kind, FillID: fmt.Sprintf("f-%s-%d", instrument, at.Unix()),
		Direction: event.DirectionLong, Quantity: qty, Price: price, FilledAt: at, Commission: commission,
	}
	switch kind {
	case event.FillKindEntry:
		p.ProposalID = "p-1"
	case event.FillKindAdd, event.FillKindExit:
		p.ProposalID, p.CampaignID = "p-1", "c-1"
	case event.FillKindStop:
		p.CampaignID, p.UnitIDs = "c-1", []string{"u-1"}
	}
	return ev{kind: journal.KindInput, typ: event.FillEventType, schema: event.FillSchemaVersion, at: at, payload: p}
}

func snapshot(account string, at time.Time, positions ...event.AccountPosition) ev {
	return ev{kind: journal.KindInput, typ: event.AccountSnapshotEventType, schema: event.AccountSnapshotPositionsVersion, at: at,
		payload: event.AccountPositionsSnapshotPayload{AccountID: account, AsOf: at, NetLiquidation: 100000, Currency: "USD", Positions: positions}}
}

func pos(instrument string, qty int64) event.AccountPosition {
	return event.AccountPosition{InstrumentID: instrument, Quantity: qty}
}

// writeJournal writes evs as a journal named name under a temp dir and returns
// its path. The header's span is derived from the inputs, as Write requires.
func writeJournal(t *testing.T, name string, evs ...ev) string {
	t.Helper()
	var entries []journal.Entry
	var start, end time.Time
	for i, e := range evs {
		raw, err := json.Marshal(e.payload)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, journal.Entry{Kind: e.kind, Envelope: event.Envelope{
			ID: fmt.Sprintf("evt-%d", i+1), Type: e.typ, EnvelopeVersion: event.CurrentEnvelopeVersion,
			SchemaVersion: e.schema, EventTime: e.at, RecordedAt: e.at, Sequence: uint64(i + 1), Source: "fixture",
			StrategyVersion: "synthetic/1", ConfigurationHash: "sha256:synthetic",
			PayloadHash: event.HashPayload(raw), Payload: raw,
		}})
		if e.kind == journal.KindInput {
			if start.IsZero() || e.at.Before(start) {
				start = e.at
			}
			if end.IsZero() || e.at.After(end) {
				end = e.at
			}
		}
	}
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("sha256:synthetic", "synthetic/1", start, end), entries); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// query runs the command and returns what it printed and its error.
func query(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(args, &out)
	return out.String(), err
}

// tradingJournal is the shared hand-built journal:
//
//	AAA 2025: entry +100 comm 1.00, add +50 comm 0.50
//	BBB 2025: entry +200 comm 2.00
//	AAA 2026: stop -150 comm 1.50
//	BBB 2026: exit -200 comm 2.00
func tradingJournal(t *testing.T) string {
	return writeJournal(t, "trading.jsonl",
		fill(event.FillKindEntry, "AAA", 100, 10, 1.00, day(2025, 3, 3)),
		fill(event.FillKindAdd, "AAA", 50, 11, 0.50, day(2025, 3, 10)),
		fill(event.FillKindEntry, "BBB", 200, 5, 2.00, day(2025, 6, 2)),
		fill(event.FillKindStop, "AAA", 150, 12, 1.50, day(2026, 1, 5)),
		fill(event.FillKindExit, "BBB", 200, 6, 2.00, day(2026, 2, 2)),
	)
}

// lines splits output into lines with runs of spaces collapsed, so a test
// asserts on a row's cells without depending on column padding.
func lines(out string) []string {
	var res []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		res = append(res, strings.Join(strings.Fields(l), " "))
	}
	return res
}

func requireLines(t *testing.T, out string, want ...string) {
	t.Helper()
	got := lines(out)
	if !slices.Equal(got, want) {
		t.Errorf("output lines differ\n got: %q\nwant: %q", got, want)
	}
}

func decision(typ string, schema uint32, at time.Time, payload any) ev {
	return ev{kind: journal.KindDecision, typ: typ, schema: schema, at: at, payload: payload}
}

func planned(account, cid, instrument, side string, qty int64, limit float64, tag string, at time.Time) ev {
	return decision(event.OrderPlannedEventType, event.OrderPlannedSchemaVersion, at, event.OrderPlannedPayload{
		AccountID: account, Tag: tag, ClientOrderID: cid, InstrumentID: instrument, Side: side, Phase: side,
		OrderType: event.OrderTypeLimit, Quantity: qty, LimitPrice: limit,
	})
}

func released(account, cid, instrument, side string, qty int64, limit float64, at time.Time) ev {
	return decision(event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, at, event.OrderReleasedPayload{
		AccountID: account, PlanID: "plan-" + cid, Tag: "t", ClientOrderID: cid, InstrumentID: instrument, Side: side,
		Quantity: qty, LimitPrice: limit, Mode: event.TradingModePaper, Send: true,
	})
}

func refused(account, cid, instrument, side string, qty int64, limit float64, reason, detail string, at time.Time) ev {
	return decision(event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, at, event.OrderRefusedPayload{
		AccountID: account, PlanID: "plan-" + cid, Tag: "t", ClientOrderID: cid, InstrumentID: instrument, Side: side,
		Quantity: qty, LimitPrice: limit, Reason: reason, Detail: detail,
	})
}

func engineState(state, reason, detail string, at time.Time, scope ...string) ev {
	return decision(event.EngineStateEventType, event.EngineStateScopedVersion, at,
		event.EngineStatePayload{State: state, Reason: reason, Detail: detail, ScopeInstrumentIDs: scope})
}

// ordersJournal is the shared hand-built live-style journal for account A1:
// AAA's buy is planned and released; BBB's sell is planned and refused; the
// engine then degrades BBB.
func ordersJournal(t *testing.T) string {
	d := day(2026, 3, 2)
	return writeJournal(t, "orders.jsonl",
		snapshot("A1", d),
		planned("A1", "co-1", "AAA", "buy", 100, 10.10, "t1", d),
		released("A1", "co-1", "AAA", "buy", 100, 10.10, d),
		planned("A1", "co-2", "BBB", "sell", 40, 4.90, "t2", d),
		refused("A1", "co-2", "BBB", "sell", 40, 4.90, event.RefusedOrderSize, "over-limit", d),
		engineState(event.EngineStateDegraded, event.EngineStateReasonRiskLimitBreached, "breach", d, "BBB"),
	)
}

// positionsJournal is hand-built for account A1. Position arithmetic:
//
//	baseline snapshot (record 1): AAA 100
//	AAA +50 -> 150, AAA -30 -> 120, BBB +10 -> 10      (records 2-4)
//	snapshot (record 5): AAA 120, BBB 10               agrees
//	AAA -120 -> 0                                      (record 6)
//	snapshot (record 7): AAA 20, BBB 10                AAA disagrees: fills say 0
func positionsJournal(t *testing.T) string {
	return writeJournal(t, "positions.jsonl",
		snapshot("A1", day(2026, 1, 2), pos("AAA", 100)),
		fill(event.FillKindEntry, "AAA", 50, 10, 0, day(2026, 1, 5)),
		fill(event.FillKindStop, "AAA", 30, 10, 0, day(2026, 1, 6)),
		fill(event.FillKindEntry, "BBB", 10, 10, 0, day(2026, 1, 6)),
		snapshot("A1", day(2026, 1, 10), pos("AAA", 120), pos("BBB", 10)),
		fill(event.FillKindExit, "AAA", 120, 10, 0, day(2026, 1, 12)),
		snapshot("A1", day(2026, 1, 20), pos("AAA", 20), pos("BBB", 10)),
	)
}

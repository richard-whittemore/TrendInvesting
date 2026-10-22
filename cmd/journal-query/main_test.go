package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
)

func TestFillsForAnInstrumentListsEachFillWithQuantityPriceAndCommission(t *testing.T) {
	out, err := query(t, "fills", "-instrument", "AAA", tradingJournal(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"2025-03-03", "entry", "100", "10", "1.00",
		"2025-03-10", "add", "50", "11", "0.50",
		"2026-01-05", "stop", "-150", "12", "1.50",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "BBB") {
		t.Errorf("a query for AAA printed BBB:\n%s", out)
	}
}

func TestAJournalWhoseChainBreaksIsRefusedNamingTheRecordAndPrintsNothing(t *testing.T) {
	path := tradingJournal(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The third record is BBB's entry, the first fill with a commission of 2.
	tampered := bytes.Replace(data, []byte(`"commission":2}`), []byte(`"commission":9}`), 1)
	if bytes.Equal(data, tampered) {
		t.Fatal("the fixture no longer contains the commission this test tampers with")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := query(t, "fills", path)
	if err == nil {
		t.Fatal("a journal with a broken chain was queried")
	}
	for _, want := range []string{"refused", "hash chain", "record 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q lacks %q", err, want)
		}
	}
	if out != "" {
		t.Errorf("figures from a refused journal were printed:\n%s", out)
	}
}

func TestOneBrokenJournalAmongSeveralRefusesTheWholeQuery(t *testing.T) {
	good := tradingJournal(t)
	bad := tradingJournal(t)
	data, _ := os.ReadFile(bad)
	if err := os.WriteFile(bad, bytes.Replace(data, []byte(`"price":10`), []byte(`"price":99`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := query(t, "fills", good, bad)
	if err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("want a refusal naming %s, got %v", bad, err)
	}
	if out != "" {
		t.Errorf("a partial answer was printed:\n%s", out)
	}
}

func TestTotalsSumCommissionsAndQuantitiesPerInstrumentPerYear(t *testing.T) {
	out, err := query(t, "totals", tradingJournal(t))
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out,
		"instrument year fills bought sold commission",
		"AAA 2025 2 150 0 1.50",
		"AAA 2026 1 0 150 1.50",
		"BBB 2025 1 200 0 2.00",
		"BBB 2026 1 0 200 2.00",
		"TOTAL all 5 350 350 7.00",
	)
}

func TestDateRangeKeepsOnlyFillsInsideItsWholeUTCDays(t *testing.T) {
	path := tradingJournal(t)
	out, err := query(t, "totals", "-from", "2025-06-01", "-to", "2026-01-31", path)
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out,
		"instrument year fills bought sold commission",
		"AAA 2026 1 0 150 1.50",
		"BBB 2025 1 200 0 2.00",
		"TOTAL all 2 200 150 3.50",
	)

	// Both ends are inclusive of the whole day: the AAA stop filled at 21:00
	// on 2026-01-05 is the only fill on that date.
	out, err = query(t, "fills", "-from", "2026-01-05", "-to", "2026-01-05", path)
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out,
		"journal seq time instrument kind quantity price commission",
		path+" 4 2026-01-05 AAA stop -150 12.00 1.50",
	)
}

func TestMalformedOrBackwardsDatesAreRefused(t *testing.T) {
	path := tradingJournal(t)
	for _, args := range [][]string{
		{"fills", "-from", "2026/01/05", path},
		{"fills", "-to", "tomorrow", path},
		{"fills", "-from", "2026-02-01", "-to", "2026-01-01", path},
	} {
		if _, err := query(t, args...); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}

func TestCSVAndJSONCarryTheSameFiguresAsTheTextTable(t *testing.T) {
	path := tradingJournal(t)

	out, err := query(t, "totals", "-instrument", "BBB", "-format", "csv", path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "instrument,year,fills,bought,sold,commission\nBBB,2025,1,200,0,2.00\nBBB,2026,1,0,200,2.00\nTOTAL,all,2,200,200,4.00\n"; out != want {
		t.Errorf("csv:\n got %q\nwant %q", out, want)
	}

	out, err = query(t, "totals", "-instrument", "BBB", "-format", "json", path)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"instrument":"BBB","year":"2025","fills":"1","bought":"200","sold":"0","commission":"2.00"},` +
		`{"instrument":"BBB","year":"2026","fills":"1","bought":"0","sold":"200","commission":"2.00"},` +
		`{"instrument":"TOTAL","year":"all","fills":"2","bought":"200","sold":"200","commission":"4.00"}]` + "\n"
	if out != want {
		t.Errorf("json:\n got %s\nwant %s", out, want)
	}
}

func TestAnUnknownFormatIsRefusedBeforeAnyJournalIsRead(t *testing.T) {
	if _, err := query(t, "fills", "-format", "xml", "/does/not/exist"); err == nil || !strings.Contains(err.Error(), "xml") {
		t.Fatalf("want a refusal naming the format, got %v", err)
	}
}

func TestOrdersListsPlannedReleasedAndRefusedForAnInstrument(t *testing.T) {
	path := ordersJournal(t)

	out, err := query(t, "orders", "-instrument", "BBB", path)
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out,
		"journal seq time account instrument event side quantity price note",
		path+" 4 2026-03-02 A1 BBB planned sell 40 4.90 t2",
		path+" 5 2026-03-02 A1 BBB refused sell 40 4.90 order-size",
	)

	out, err = query(t, "orders", "-instrument", "AAA", path)
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out,
		"journal seq time account instrument event side quantity price note",
		path+" 2 2026-03-02 A1 AAA planned buy 100 10.10 t1",
		path+" 3 2026-03-02 A1 AAA released buy 100 10.10 mode=paper,send=true",
	)
}

func TestRefusalsListsRefusedOrdersAndEngineStateChangesAndHonoursTheInstrumentFilter(t *testing.T) {
	path := ordersJournal(t)

	out, err := query(t, "refusals", path)
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out,
		"journal seq time account instrument event reason detail",
		path+" 5 2026-03-02 A1 BBB refused order-size over-limit",
		path+" 6 2026-03-02 A1 BBB engine-state degraded:risk-limit-breached breach",
	)

	out, err = query(t, "refusals", "-instrument", "AAA", path)
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out, "journal seq time account instrument event reason detail")
}

func TestAccountFilterKeepsOnlyThatAccountsOrdersAcrossJournals(t *testing.T) {
	d := day(2026, 3, 3)
	other := writeJournal(t, "other.jsonl",
		snapshot("A2", d),
		planned("A2", "co-9", "AAA", "buy", 7, 20, "t9", d),
	)
	mine := ordersJournal(t)

	out, err := query(t, "orders", "-account", "A2", mine, other)
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out,
		"journal seq time account instrument event side quantity price note",
		other+" 2 2026-03-03 A2 AAA planned buy 7 20.00 t9",
	)
}

func TestPositionsRebuildsTheHoldingAfterEachFillFromTheBaselineSnapshot(t *testing.T) {
	path := positionsJournal(t)
	out, err := query(t, "positions", path)
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out,
		"journal seq time account instrument kind change position",
		path+" 2 2026-01-05 A1 AAA entry 50 150",
		path+" 3 2026-01-06 A1 AAA stop -30 120",
		path+" 4 2026-01-06 A1 BBB entry 10 10",
		path+" 6 2026-01-12 A1 AAA exit -120 0",
	)

	// Filtering shows fewer rows but never changes the running position.
	out, err = query(t, "positions", "-instrument", "AAA", "-from", "2026-01-06", path)
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out,
		"journal seq time account instrument kind change position",
		path+" 3 2026-01-06 A1 AAA stop -30 120",
		path+" 6 2026-01-12 A1 AAA exit -120 0",
	)
}

func TestReconcileReportsADisagreementWithTheSnapshotInsteadOfHidingIt(t *testing.T) {
	path := positionsJournal(t)
	out, err := query(t, "reconcile", path)
	requireLines(t, out,
		"journal seq time account instrument fills snapshot status",
		path+" 5 2026-01-10 A1 AAA 120 120 ok",
		path+" 5 2026-01-10 A1 BBB 10 10 ok",
		path+" 7 2026-01-20 A1 AAA 0 20 DISAGREES",
		path+" 7 2026-01-20 A1 BBB 10 10 ok",
	)
	if err == nil || !strings.Contains(err.Error(), "1 disagreement") {
		t.Fatalf("want an error counting 1 disagreement, got %v", err)
	}
}

func TestReconcileSucceedsWhenFillsAgreeWithEverySnapshot(t *testing.T) {
	path := writeJournal(t, "agree.jsonl",
		snapshot("A1", day(2026, 1, 2)),
		fill(event.FillKindEntry, "AAA", 5, 10, 0, day(2026, 1, 5)),
		snapshot("A1", day(2026, 1, 6), pos("AAA", 5)),
	)
	out, err := query(t, "reconcile", path)
	if err != nil {
		t.Fatal(err)
	}
	requireLines(t, out,
		"journal seq time account instrument fills snapshot status",
		path+" 3 2026-01-06 A1 AAA 5 5 ok",
	)
}

func TestFillsThatCannotBeAttributedToOneAccountAreRefusedNotGuessed(t *testing.T) {
	d := day(2026, 1, 5)
	path := writeJournal(t, "two-accounts.jsonl",
		snapshot("A1", d), snapshot("A2", d),
		fill(event.FillKindEntry, "AAA", 5, 10, 0, d),
	)
	for _, report := range []string{"positions", "reconcile"} {
		if _, err := query(t, report, path); err == nil || !strings.Contains(err.Error(), "attribut") {
			t.Errorf("%s: want a refusal about attribution, got %v", report, err)
		}
	}
}

func TestNoQueryChangesAJournalByAByte(t *testing.T) {
	paths := []string{tradingJournal(t), ordersJournal(t), positionsJournal(t)}
	before := make([][]byte, len(paths))
	for i, p := range paths {
		var err error
		if before[i], err = os.ReadFile(p); err != nil {
			t.Fatal(err)
		}
	}
	// The files are made read-only too, so a write would fail loudly rather
	// than only be caught by the comparison.
	for _, p := range paths {
		if err := os.Chmod(p, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	// assertUnchanged compares every fixture's bytes with the bytes read
	// before any query. It runs after each call, before that call's error is
	// judged, so a write made on the way to a refusal is still caught.
	assertUnchanged := func(call string) {
		t.Helper()
		for i, p := range paths {
			after, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before[i], after) {
				t.Errorf("%s changed after %s", p, call)
			}
		}
	}
	for _, report := range []string{"orders", "fills", "refusals", "totals", "positions"} {
		for _, format := range []string{"text", "csv", "json"} {
			args := append([]string{report, "-format", format}, paths...)
			_, err := query(t, args...)
			assertUnchanged(report + " " + format)
			if err != nil {
				t.Fatalf("%s %s: %v", report, format, err)
			}
		}
	}
	// reconcile runs on positionsJournal alone. It holds snapshots, so the
	// rebuild and the table are reached, and its disagreement on 2026-01-20 is
	// refused with the table printed. Given all three paths, reconcile stops at
	// tradingJournal (no snapshot, so nothing compared) before either.
	for _, format := range []string{"text", "csv", "json"} {
		out, err := query(t, "reconcile", "-format", format, paths[2])
		assertUnchanged("reconcile " + format)
		var dis *journalquery.DisagreementError
		if !errors.As(err, &dis) || out == "" {
			t.Fatalf("reconcile %s: want the table and a DisagreementError, got %q, %v", format, out, err)
		}
	}
}

// syncBuffer is the SyncWriter the journal package's durable log is written to.
type syncBuffer struct{ bytes.Buffer }

func (*syncBuffer) Sync() error { return nil }

func TestADurableLogIsReadLikeTheJournalOfTheSameFills(t *testing.T) {
	evs := []ev{
		fill(event.FillKindEntry, "AAA", 100, 10, 1.00, day(2025, 3, 3)),
		fill(event.FillKindExit, "AAA", 100, 11, 1.00, day(2025, 4, 3)),
	}
	var buf syncBuffer
	log, err := journal.CreateLog(&buf, journal.NewLogHeader("sha256:synthetic", "synthetic/1"))
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range evs {
		raw, err := json.Marshal(e.payload)
		if err != nil {
			t.Fatal(err)
		}
		group := []journal.Entry{{Kind: e.kind, Envelope: event.Envelope{
			ID: fmt.Sprintf("evt-%d", i+1), Type: e.typ, EnvelopeVersion: event.CurrentEnvelopeVersion,
			SchemaVersion: e.schema, EventTime: e.at, RecordedAt: e.at, Sequence: uint64(i + 1), Source: "fixture",
			StrategyVersion: "synthetic/1", ConfigurationHash: "sha256:synthetic",
			PayloadHash: event.HashPayload(raw), Payload: raw,
		}}}
		if err := log.Append(group); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	journalPath := writeJournal(t, "run.jsonl", evs...)
	want, err := query(t, "fills", journalPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := query(t, "fills", path)
	if err != nil {
		t.Fatalf("durable log: %v", err)
	}
	// The journal column names the file, so compare the rows without it.
	if strip(got, path) != strip(want, journalPath) || !strings.Contains(got, "AAA") {
		t.Errorf("durable log fills\n%s\nwant the journal's\n%s", got, want)
	}
}

// strip removes the file name and the column padding it caused.
func strip(out, name string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(out, name, "")), " ")
}

func TestAPayloadAtASchemaVersionThisBuildDoesNotReadIsRefusedNotGuessed(t *testing.T) {
	future := fill(event.FillKindEntry, "AAA", 1, 10, 0, day(2026, 1, 5))
	future.schema = event.FillTargetWeightVersion + 1
	path := writeJournal(t, "future.jsonl", future)
	if _, err := query(t, "fills", path); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("want a schema version refusal, got %v", err)
	}
}

func TestAnUnknownReportOrMissingJournalIsRefused(t *testing.T) {
	if _, err := query(t, "gossip", tradingJournal(t)); err == nil {
		t.Error("an unknown report was accepted")
	}
	if _, err := query(t, "fills"); err == nil {
		t.Error("a query with no journal was accepted")
	}
	if _, err := query(t); err == nil {
		t.Error("no report was accepted")
	}
}

// TestTheOperatorReportShowsTheKillSwitch: the operator's recorded inputs
// are visible from the command line, what each did and who recorded it.
func TestTheOperatorReportShowsTheKillSwitch(t *testing.T) {
	when := day(2026, 10, 17)
	path := writeJournal(t, "operator.jsonl", ev{journal.KindInput, event.KillSwitchEventType, event.KillSwitchSchemaVersion, when,
		event.KillSwitchPayload{AccountID: "joint", AsOf: when, Engaged: true, Operator: "owner", Detail: "news before the open"}})
	out, err := query(t, "operator", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kill-switch", "engaged", "owner", "news before the open", "joint"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestReconcileOfAnEmptyLogExitsWithAMessageInWords(t *testing.T) {
	var buf syncBuffer
	if _, err := journal.CreateLog(&buf, journal.NewLogHeader("sha256:synthetic", "synthetic/1")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := query(t, "reconcile", path)
	if err == nil || !strings.Contains(err.Error(), "holds no committed record") || out != "" {
		t.Fatalf("want a refusal in words and no table, got %q, %v", out, err)
	}
}

func TestReconcileOfATornTailLogExitsWithAMessageInWords(t *testing.T) {
	var buf syncBuffer
	log, err := journal.CreateLog(&buf, journal.NewLogHeader("sha256:synthetic", "synthetic/1"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		raw := []byte(`{"n":1}`)
		entry := journal.Entry{Kind: journal.KindInput, Envelope: event.Envelope{
			ID: fmt.Sprintf("e%d", i), Type: "synthetic.input", EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: 1,
			EventTime: time.Date(2026, 1, i, 21, 0, 0, 0, time.UTC), RecordedAt: time.Date(2026, 1, i, 21, 0, 0, 0, time.UTC),
			Sequence: uint64(i), Source: "fixture", StrategyVersion: "s", ConfigurationHash: "h", PayloadHash: event.HashPayload(raw), Payload: raw,
		}}
		if err := log.Append([]journal.Entry{entry}); err != nil {
			t.Fatal(err)
		}
	}
	// Drop the final commit line: the last group is then an uncommitted tail.
	trimmed := bytes.TrimRight(buf.Bytes(), "\n")
	data := trimmed[:bytes.LastIndexByte(trimmed, '\n')+1]
	path := filepath.Join(t.TempDir(), "torn.log")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := query(t, "reconcile", path)
	if err == nil || !strings.Contains(err.Error(), "cut short") || out != "" {
		t.Fatalf("want a cut-short refusal and no table, got %q, %v", out, err)
	}
}

func TestReconcileThatComparedNothingExitsNonZero(t *testing.T) {
	path := writeJournal(t, "nosnap.jsonl", fill(event.FillKindEntry, "AAA", 5, 10, 0, day(2026, 1, 5)))
	out, err := query(t, "reconcile", path)
	if err == nil || !strings.Contains(err.Error(), "compared nothing") || out != "" {
		t.Fatalf("want a compared-nothing refusal and no table, got %q, %v", out, err)
	}
}

func TestReconcileWithFillsAfterTheLastSnapshotFailsUnlessAllowed(t *testing.T) {
	path := writeJournal(t, "trailing.jsonl",
		snapshot("A1", day(2026, 1, 2), pos("AAA", 100)),
		fill(event.FillKindEntry, "AAA", 50, 10, 0, day(2026, 1, 5)),
		snapshot("A1", day(2026, 1, 10), pos("AAA", 150)),
		fill(event.FillKindEntry, "AAA", 5, 10, 0, day(2026, 1, 12)),
	)
	out, err := query(t, "reconcile", path)
	if err == nil || !strings.Contains(err.Error(), "1 fills not yet covered by a snapshot") || !strings.Contains(out, "ok") {
		t.Fatalf("want the table and the trailing-fill error, got %q, %v", out, err)
	}
	if _, err := query(t, "reconcile", "-allow-trailing-fills", path); err != nil {
		t.Fatalf("the trailing fill was allowed: %v", err)
	}
}

func TestReconcileWithTheRegistryAnchorChecksTheFinalRecordHash(t *testing.T) {
	path := writeJournal(t, "anchored.jsonl",
		snapshot("A1", day(2026, 1, 2)),
		fill(event.FillKindEntry, "AAA", 5, 10, 0, day(2026, 1, 5)),
		snapshot("A1", day(2026, 1, 6), pos("AAA", 5)),
	)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	j, err := journalquery.Load(path, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := query(t, "reconcile", "-final-hash", j.FinalRecordHash(), path); err != nil {
		t.Fatalf("the recorded anchor matches: %v", err)
	}
	out, err := query(t, "reconcile", "-final-hash", "sha256:deadbeef", path)
	if err == nil || !strings.Contains(err.Error(), "not the registered run's complete end-of-run journal") || out != "" {
		t.Fatalf("want a mismatch refusal and no table, got %q, %v", out, err)
	}
	if _, err := query(t, "reconcile", "-final-hash", "", path); err == nil {
		t.Fatal("an empty anchor is not agreement")
	}
	if _, err := query(t, "reconcile", "-final-hash", j.FinalRecordHash(), path, path); err == nil {
		t.Fatal("one anchor names one journal: several journals with it are refused")
	}
}

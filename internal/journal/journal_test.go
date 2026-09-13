package journal_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

const (
	testConfigurationHash = "sha256:0123456789abcdef"
	testStrategyVersion   = "turtle-baseline/1.1.0+dev"
)

func at(day int) time.Time {
	return time.Date(2026, time.February, day, 0, 0, 0, 0, time.UTC)
}

// testEnvelope builds a valid envelope at the given sequence, identified so a
// failure names which record it came from.
func testEnvelope(sequence uint64) event.Envelope {
	payload := json.RawMessage(fmt.Sprintf(`{"n":%d}`, sequence))
	return event.Envelope{
		ID:                fmt.Sprintf("evt-%d", sequence),
		Type:              "test.event",
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     1,
		EventTime:         at(int(sequence)),
		RecordedAt:        at(int(sequence)),
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// testInputs is a contiguous run of input envelopes, for the tests that
// drive a handler rather than write a file directly.
func testInputs(n int) []event.Envelope {
	out := make([]event.Envelope, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, testEnvelope(uint64(i)))
	}
	return out
}

// testEntries alternates input and decision so the kind genuinely varies
// down the chain rather than being one constant the tests could not tell
// from a missing field.
func testEntries(n int) []journal.Entry {
	out := make([]journal.Entry, 0, n)
	for i := 1; i <= n; i++ {
		kind := journal.KindInput
		if i%2 == 0 {
			kind = journal.KindDecision
		}
		out = append(out, journal.Entry{Kind: kind, Envelope: testEnvelope(uint64(i))})
	}
	return out
}

// headerSeed is the chain's starting value: the hash of the header's own
// canonical bytes, recomputed here independently of the writer so a journal
// cannot be re-attributed to a different run without breaking record 1.
func headerSeed(h journal.Header) [sha256.Size]byte {
	return sha256.Sum256(event.CanonicalBytes(map[string]any{
		"journal_version":    h.JournalVersion,
		"chain_algorithm":    h.ChainAlgorithm,
		"configuration_hash": h.ConfigurationHash,
		"strategy_version":   h.StrategyVersion,
		"span_start":         h.SpanStart.UTC().Format(time.RFC3339Nano),
		"span_end":           h.SpanEnd.UTC().Format(time.RFC3339Nano),
	}))
}

// editHeaderLine rewrites the header without touching a single record — the
// re-attribution a chain that only covered records would have missed.
func editHeaderLine(t *testing.T, written []byte, mutate func(map[string]any)) []byte {
	t.Helper()

	lines := bytes.Split(bytes.TrimRight(written, "\n"), []byte("\n"))
	var header map[string]any
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	mutate(header)
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	lines[0] = encoded
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

// chainHash is the record hash for one entry following previous, recomputed
// here independently of the writer so the two definitions cannot drift.
func chainHash(previous [sha256.Size]byte, kind string, envelope event.Envelope) [sha256.Size]byte {
	hashed := append([]byte{}, previous[:]...)
	hashed = append(hashed, kind...)
	hashed = append(hashed, event.CanonicalEnvelopeBytes(envelope)...)
	return sha256.Sum256(hashed)
}

func hashString(sum [sha256.Size]byte) string {
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeVerbatim renders header and records exactly as given, chain hashes
// included — what an editor with a text editor leaves behind, and what a
// test that builds its own records needs.
func writeVerbatim(t *testing.T, header journal.Header, records []journal.Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	lines := []any{header}
	for _, record := range records {
		lines = append(lines, record)
	}
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		buf.Write(append(encoded, '\n'))
	}
	return buf.Bytes()
}

func testHeader() journal.Header {
	return journal.NewHeader(testConfigurationHash, testStrategyVersion, at(1), at(3))
}

// writeJournal writes a three-record journal and returns its bytes.
func writeJournal(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := journal.Write(&buf, testHeader(), testEntries(3)); err != nil {
		t.Fatalf("journal.Write() error = %v", err)
	}
	return buf.Bytes()
}

func TestWriteThenVerifyAcceptsAnUntouchedJournal(t *testing.T) {
	t.Parallel()

	written := writeJournal(t)

	verification, err := journal.Verify(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Verify() error = %v, want nil", err)
	}
	if verification.RecordCount != 3 {
		t.Errorf("RecordCount = %d, want 3", verification.RecordCount)
	}
	if verification.Header.ConfigurationHash != testConfigurationHash {
		t.Errorf("Header.ConfigurationHash = %q, want %q", verification.Header.ConfigurationHash, testConfigurationHash)
	}
	if verification.Header.StrategyVersion != testStrategyVersion {
		t.Errorf("Header.StrategyVersion = %q, want %q", verification.Header.StrategyVersion, testStrategyVersion)
	}
	if !verification.Header.SpanStart.Equal(at(1)) || !verification.Header.SpanEnd.Equal(at(3)) {
		t.Errorf("span = [%s, %s], want [%s, %s]", verification.Header.SpanStart, verification.Header.SpanEnd, at(1), at(3))
	}
	if verification.FinalRecordHash == "" {
		t.Error("FinalRecordHash is empty; it is what a run registry anchors (#39)")
	}
}

// TestTheFirstRecordChainsFromTheHeader pins the start of the chain. The
// header states which run the journal is — its configuration hash, strategy
// version and span — and seeding the chain with it is what stops a journal
// being silently re-attributed to a different run while every record still
// verifies (ADR 0017).
func TestTheFirstRecordChainsFromTheHeader(t *testing.T) {
	t.Parallel()

	_, records, err := journal.Read(bytes.NewReader(writeJournal(t)))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}

	want := hashString(chainHash(headerSeed(testHeader()), journal.KindInput, testEnvelope(1)))
	if records[0].RecordHash != want {
		t.Fatalf("first record hash = %q, want %q", records[0].RecordHash, want)
	}
}

// TestEditingAnyHeaderFieldBreaksTheChainAtTheFirstRecord: rewriting what
// run a journal claims to be, without touching one record, is caught.
func TestEditingAnyHeaderFieldBreaksTheChainAtTheFirstRecord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		field  string
		mutate func(map[string]any)
	}{
		{"configuration_hash", func(h map[string]any) { h["configuration_hash"] = "sha256:a-different-configuration" }},
		{"strategy_version", func(h map[string]any) { h["strategy_version"] = "turtle-baseline/9.9.9+dev" }},
		{"span_start", func(h map[string]any) { h["span_start"] = at(2).UTC().Format(time.RFC3339Nano) }},
		{"span_end", func(h map[string]any) { h["span_end"] = at(9).UTC().Format(time.RFC3339Nano) }},
		{"chain_algorithm", func(h map[string]any) { h["chain_algorithm"] = "sha256(something-else)" }},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()

			edited := editHeaderLine(t, writeJournal(t), tt.mutate)

			_, err := journal.Verify(bytes.NewReader(edited))
			var broken *journal.ChainBrokenError
			if !errors.As(err, &broken) {
				t.Fatalf("journal.Verify() error = %v, want a ChainBrokenError: editing %s re-attributes the journal", err, tt.field)
			}
			if broken.Sequence != 1 {
				t.Fatalf("chain reported broken at record %d, want 1: the header seeds the chain", broken.Sequence)
			}
		})
	}
}

// TestEachRecordChainsFromThePreviousRecordHash recomputes the whole chain
// independently of the writer, so the two definitions cannot drift.
func TestEachRecordChainsFromThePreviousRecordHash(t *testing.T) {
	t.Parallel()

	_, records, err := journal.Read(bytes.NewReader(writeJournal(t)))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("read %d records, want 3", len(records))
	}

	previous := headerSeed(testHeader())
	for i, record := range records {
		if record.Sequence != uint64(i+1) {
			t.Fatalf("record %d has sequence %d, want %d", i, record.Sequence, i+1)
		}
		sum := chainHash(previous, record.Kind, record.Envelope)
		if want := hashString(sum); record.RecordHash != want {
			t.Fatalf("record %d hash = %q, want %q", record.Sequence, record.RecordHash, want)
		}
		previous = sum
	}
}

// TestTheEnvelopeInAJournalRecordIsUnchangedByTheChain is #48's load-bearing
// constraint: a chain field on the envelope would make the same decision
// event hash differently depending on its position in a journal, and
// byte-identical replay would become unsatisfiable.
func TestTheEnvelopeInAJournalRecordIsUnchangedByTheChain(t *testing.T) {
	t.Parallel()

	lines := bytes.Split(bytes.TrimRight(writeJournal(t), "\n"), []byte("\n"))
	var raw struct {
		Envelope json.RawMessage `json:"envelope"`
	}
	if err := json.Unmarshal(lines[1], &raw); err != nil {
		t.Fatalf("decode record: %v", err)
	}

	standalone, err := json.Marshal(testEnvelope(1))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !bytes.Equal(raw.Envelope, standalone) {
		t.Fatalf("the envelope recorded in a journal differs from the envelope itself:\n in journal  %s\n standalone  %s", raw.Envelope, standalone)
	}
}

// TestVerifyReportsTheFirstBrokenLinkBySequence: any edit to a recorded
// envelope, at any position, is detected and attributed.
func TestVerifyReportsTheFirstBrokenLinkBySequence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		edit         func(*journal.Record)
		record       int // index into the record lines
		wantSequence uint64
	}{
		{
			name:         "a record's payload",
			record:       1,
			wantSequence: 2,
			edit: func(r *journal.Record) {
				r.Envelope.Payload = json.RawMessage(`{"n":222}`)
			},
		},
		{
			name:         "a record's envelope field",
			record:       1,
			wantSequence: 2,
			edit: func(r *journal.Record) {
				r.Envelope.Source = "someone-else"
			},
		},
		{
			name:         "the last record",
			record:       2,
			wantSequence: 3,
			edit: func(r *journal.Record) {
				r.Envelope.EventTime = r.Envelope.EventTime.Add(time.Hour)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			edited := editRecordLine(t, writeJournal(t), tt.record, tt.edit)

			_, err := journal.Verify(bytes.NewReader(edited))
			var broken *journal.ChainBrokenError
			if !errors.As(err, &broken) {
				t.Fatalf("journal.Verify() error = %v, want a ChainBrokenError", err)
			}
			if broken.Sequence != tt.wantSequence {
				t.Fatalf("chain reported broken at record %d, want %d", broken.Sequence, tt.wantSequence)
			}
			// The message names the record too: a verifier's output is read
			// by a person, not only matched on by a program.
			if want := fmt.Sprintf("record %d", tt.wantSequence); !strings.Contains(broken.Error(), want) {
				t.Fatalf("ChainBrokenError.Error() = %q, want it to name %q", broken.Error(), want)
			}
		})
	}
}

// TestFlippingARecordsKindBreaksTheChain is why the kind is inside the hash
// and not merely beside it. Replay equivalence reads the kind to decide what
// to feed in and what to compare against, so a record relabelled from
// decision to input changes what that check is even asking — and the chain
// is what makes that relabelling visible.
func TestFlippingARecordsKindBreaksTheChain(t *testing.T) {
	t.Parallel()

	edited := editRecordLine(t, writeJournal(t), 1, func(r *journal.Record) {
		if r.Kind != journal.KindDecision {
			t.Fatalf("the fixture's second record is a %s; this test needs a decision to relabel", r.Kind)
		}
		r.Kind = journal.KindInput
	})

	_, err := journal.Verify(bytes.NewReader(edited))
	var broken *journal.ChainBrokenError
	if !errors.As(err, &broken) {
		t.Fatalf("journal.Verify() error = %v, want a ChainBrokenError", err)
	}
	if broken.Sequence != 2 {
		t.Fatalf("chain reported broken at record %d, want 2", broken.Sequence)
	}
}

// TestVerifyRejectsAnUnrecognisedRecordKind: the kind is a closed set, and a
// record claiming anything else is rejected even when its chain is perfectly
// consistent — which is exactly what a forger who recomputed the chain would
// leave behind.
func TestVerifyRejectsAnUnrecognisedRecordKind(t *testing.T) {
	t.Parallel()

	envelope := testEnvelope(1)
	sum := chainHash(headerSeed(testHeader()), "neither", envelope)
	forged := writeVerbatim(t, testHeader(), []journal.Record{{
		Sequence:   1,
		Kind:       "neither",
		Envelope:   envelope,
		RecordHash: hashString(sum),
	}})

	_, err := journal.Verify(bytes.NewReader(forged))
	if err == nil {
		t.Fatal("journal.Verify() error = nil, want one naming the unrecognised kind")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Fatalf("journal.Verify() error = %v, want one naming the unrecognised kind", err)
	}
}

func TestWriteRefusesAnUnrecognisedKind(t *testing.T) {
	t.Parallel()

	entries := testEntries(2)
	entries[1].Kind = ""

	var buf bytes.Buffer
	err := journal.Write(&buf, testHeader(), entries)
	if err == nil {
		t.Fatal("journal.Write() error = nil, want one naming the unrecognised kind")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Fatalf("journal.Write() error = %v, want one naming the unrecognised kind", err)
	}
}

// TestVerifyDetectsATruncatedFile: a file cut short mid-record is not a
// journal, and must not verify as a shorter one.
func TestVerifyDetectsATruncatedFile(t *testing.T) {
	t.Parallel()

	written := writeJournal(t)
	truncated := written[:len(written)-20]

	if _, err := journal.Verify(bytes.NewReader(truncated)); err == nil {
		t.Fatal("journal.Verify() error = nil, want one naming the unreadable record")
	}
}

// TestDroppingWholeTrailingRecordsIsNotDetectedByTheChainAlone records the
// limit of what a hash chain can do inside one file: a prefix of a valid
// chain is itself a valid chain. Anchoring each run's final record hash in
// the git-committed run registry (#39) is what closes this, and this test
// exists so the gap is stated rather than assumed away.
func TestDroppingWholeTrailingRecordsIsNotDetectedByTheChainAlone(t *testing.T) {
	t.Parallel()

	written := writeJournal(t)
	full, err := journal.Verify(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Verify() error = %v", err)
	}

	lines := bytes.Split(bytes.TrimRight(written, "\n"), []byte("\n"))
	shortened := append(bytes.Join(lines[:len(lines)-1], []byte("\n")), '\n')

	prefix, err := journal.Verify(bytes.NewReader(shortened))
	if err != nil {
		t.Fatalf("journal.Verify() on a prefix error = %v, want nil: the chain cannot see what is missing", err)
	}
	if prefix.RecordCount != full.RecordCount-1 {
		t.Fatalf("prefix RecordCount = %d, want %d", prefix.RecordCount, full.RecordCount-1)
	}
	if prefix.FinalRecordHash == full.FinalRecordHash {
		t.Fatal("the prefix's final record hash equals the whole journal's; anchoring it would not detect the truncation")
	}
}

func TestVerifyFailsClosedOnAnUnrecognisedHeaderVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version uint32
		wantErr string
	}{
		{name: "a newer journal", version: journal.FormatVersion + 1, wantErr: "newer"},
		{name: "an older journal", version: journal.FormatVersion - 1, wantErr: "no upcaster"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			written := writeJournal(t)
			lines := bytes.Split(bytes.TrimRight(written, "\n"), []byte("\n"))
			var header map[string]any
			if err := json.Unmarshal(lines[0], &header); err != nil {
				t.Fatalf("decode header: %v", err)
			}
			header["journal_version"] = tt.version
			replaced, err := json.Marshal(header)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			lines[0] = replaced

			_, err = journal.Verify(bytes.NewReader(append(bytes.Join(lines, []byte("\n")), '\n')))
			if err == nil {
				t.Fatalf("journal.Verify() error = nil, want one containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("journal.Verify() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestVerifyRejectsANonContiguousRecordSequence: a removed record leaves a
// hole in the journal's own numbering, which is reported as such rather than
// as a broken link.
func TestVerifyRejectsANonContiguousRecordSequence(t *testing.T) {
	t.Parallel()

	written := writeJournal(t)
	lines := bytes.Split(bytes.TrimRight(written, "\n"), []byte("\n"))
	// Header, record 1, record 3.
	kept := [][]byte{lines[0], lines[1], lines[3]}

	_, err := journal.Verify(bytes.NewReader(append(bytes.Join(kept, []byte("\n")), '\n')))
	if err == nil {
		t.Fatal("journal.Verify() error = nil, want one naming the sequence gap")
	}
	if !strings.Contains(err.Error(), "sequence") {
		t.Fatalf("journal.Verify() error = %v, want one naming the sequence gap", err)
	}
}

func TestWriteRefusesToRecordAnInvalidEnvelope(t *testing.T) {
	t.Parallel()

	entries := testEntries(2)
	entries[1].Envelope.PayloadHash = "not-the-payload-hash"

	var buf bytes.Buffer
	err := journal.Write(&buf, testHeader(), entries)
	if err == nil {
		t.Fatal("journal.Write() error = nil, want one naming the invalid envelope")
	}
	if !strings.Contains(err.Error(), "payload hash") {
		t.Fatalf("journal.Write() error = %v, want one naming the invalid envelope", err)
	}
}

func TestWriteRefusesAnIncompleteHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*journal.Header)
		wantErr string
	}{
		{
			name:    "no configuration hash",
			mutate:  func(h *journal.Header) { h.ConfigurationHash = "" },
			wantErr: "configuration hash",
		},
		{
			name:    "no strategy version",
			mutate:  func(h *journal.Header) { h.StrategyVersion = "" },
			wantErr: "strategy version",
		},
		{
			name:    "no span",
			mutate:  func(h *journal.Header) { h.SpanStart = time.Time{} },
			wantErr: "span",
		},
		{
			name:    "a span that runs backwards",
			mutate:  func(h *journal.Header) { h.SpanEnd = h.SpanStart.Add(-time.Hour) },
			wantErr: "span",
		},
		{
			name:    "an unrecognised journal version",
			mutate:  func(h *journal.Header) { h.JournalVersion = journal.FormatVersion + 1 },
			wantErr: "journal version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			header := testHeader()
			tt.mutate(&header)

			var buf bytes.Buffer
			err := journal.Write(&buf, header, testEntries(1))
			if err == nil {
				t.Fatalf("journal.Write() error = nil, want one containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("journal.Write() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestWriteRefusesAnEmptyJournal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := journal.Write(&buf, testHeader(), nil); err == nil {
		t.Fatal("journal.Write() error = nil, want one refusing a journal with no records")
	}
}

// TestWritingTheSameRunTwiceProducesIdenticalBytes: a journal is evidence,
// and two writes of the same events must not differ in their bytes or their
// chain.
func TestWritingTheSameRunTwiceProducesIdenticalBytes(t *testing.T) {
	t.Parallel()

	if !bytes.Equal(writeJournal(t), writeJournal(t)) {
		t.Fatal("two writes of the same envelopes produced different bytes")
	}
}

// editRecordLine decodes record line index (0-based among the record lines,
// so the header is not counted), applies edit, and rewrites the line WITHOUT
// recomputing the chain — the careless editor of #48.
func editRecordLine(t *testing.T, written []byte, index int, edit func(*journal.Record)) []byte {
	t.Helper()

	lines := bytes.Split(bytes.TrimRight(written, "\n"), []byte("\n"))
	var record journal.Record
	if err := json.Unmarshal(lines[index+1], &record); err != nil {
		t.Fatalf("decode record %d: %v", index, err)
	}
	edit(&record)
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	lines[index+1] = encoded
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

// Package journal writes and verifies a run's journal: the complete ordered
// stream of input and decision events, one record per line, under a header
// naming the configuration the run used.
//
// # Tamper-evidence, not tamper-proofing
//
// Each record carries a hash chained over the previous record's hash, the
// record's own kind, and the canonical bytes of its envelope, and the chain
// is seeded with the hash of the header (ADR 0017). Altering event k breaks
// every link after k, and altering the header — which run the journal claims
// to be — breaks all of them, so neither a silent edit to recorded history
// nor a silent re-attribution of it is possible without a secret. It is evidence, not proof: whoever
// can rewrite one record can rewrite the whole file, and dropping records
// from the end leaves a valid chain. Anchoring each run's final record hash outside the system —
// the git-committed run registry — is what closes that, and is why Verify
// reports the final hash.
//
// # Two properties, two checks
//
// Verify answers "was this file edited after it was written". Replay
// equivalence answers "does this engine still produce these decisions", and
// never looks at the chain — which is why the chain lives in the journal
// record and never on the envelope (ADR 0017): a chain field on an envelope
// would make the same decision hash differently depending on its position in
// a journal, and byte-identical replay would become unsatisfiable. A journal
// can fail either check independently, and the two failures mean different
// things.
//
// Which records are inputs and which are decisions is stated on the record
// (Kind) and covered by the chain, because that claim is what replay
// equivalence reads to decide what to feed in and what to compare against.
package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// FormatVersion is the journal format this build writes and can read. Read
// rejects any other value in both directions, fail closed, because no
// upcaster is implemented — the same rule ADR 0015 applies to the envelope.
const FormatVersion uint32 = 1

// ChainAlgorithm names how a record hash is computed, recorded in the header
// so a verifier is told rather than assuming. The algorithm string is itself
// inside the seed, so a journal cannot claim to have been chained some other
// way either.
const ChainAlgorithm = "seed=sha256(canonical_header_bytes); record=sha256(previous_record_hash||kind||canonical_envelope_bytes)"

// maxLineBytes bounds one journal line while reading. A record is one
// envelope and its payload, far smaller than this; the bound exists so a
// corrupt file cannot make a verifier allocate without limit.
const maxLineBytes = 8 << 20

// Header is a journal's first line: what the run was, and how to check it.
type Header struct {
	JournalVersion uint32 `json:"journal_version"`
	ChainAlgorithm string `json:"chain_algorithm"`
	// ConfigurationHash and StrategyVersion are the run's identity: results
	// are retained under their configuration hash (ADR 0012), and two builds
	// sharing a rules version must replay each other's journals (ADR 0016).
	ConfigurationHash string `json:"configuration_hash"`
	StrategyVersion   string `json:"strategy_version"`
	// SpanStart and SpanEnd are the first and last INPUT event time of the
	// run: the period the journal covers.
	SpanStart time.Time `json:"span_start"`
	SpanEnd   time.Time `json:"span_end"`
}

// NewHeader returns the header for a run, filling in the format version and
// chain algorithm this build writes.
func NewHeader(configurationHash, strategyVersion string, spanStart, spanEnd time.Time) Header {
	return Header{
		JournalVersion:    FormatVersion,
		ChainAlgorithm:    ChainAlgorithm,
		ConfigurationHash: configurationHash,
		StrategyVersion:   strategyVersion,
		SpanStart:         spanStart,
		SpanEnd:           spanEnd,
	}
}

func (h Header) validate() error {
	var errs []error
	if h.JournalVersion != FormatVersion {
		errs = append(errs, fmt.Errorf("journal version %d is not the version %d this build writes", h.JournalVersion, FormatVersion))
	}
	if h.ChainAlgorithm != ChainAlgorithm {
		errs = append(errs, fmt.Errorf("chain algorithm %q is not the algorithm %q this build computes", h.ChainAlgorithm, ChainAlgorithm))
	}
	if h.ConfigurationHash == "" {
		errs = append(errs, errors.New("configuration hash is required"))
	}
	if h.StrategyVersion == "" {
		errs = append(errs, errors.New("strategy version is required"))
	}
	switch {
	case h.SpanStart.IsZero() || h.SpanEnd.IsZero():
		errs = append(errs, errors.New("the span the journal covers is required at both ends"))
	case h.SpanEnd.Before(h.SpanStart):
		errs = append(errs, fmt.Errorf("the span runs backwards: %s to %s", h.SpanStart.Format(time.RFC3339), h.SpanEnd.Format(time.RFC3339)))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("journal: invalid header: %w", err)
	}
	return nil
}

// The two kinds of event a journal records. An input is an event the run was
// given — a configuration, a bar, a fill; a decision is one the reducer
// produced from it.
//
// The distinction is stated on the record rather than inferred from
// Envelope.Source (ADR 0017). A producer's name answers a different
// question, and nothing stops one stamping a reducer's source on something
// that is not a decision — while replay equivalence depends on this split
// being exactly right, since it feeds the inputs in and compares the
// decisions against what comes out.
const (
	KindInput    = "input"
	KindDecision = "decision"
)

// Entry is one event to record, and what it was in the run.
type Entry struct {
	Kind     string
	Envelope event.Envelope
}

func validKind(kind string) bool {
	return kind == KindInput || kind == KindDecision
}

// Record is one line of a journal: an envelope, what it was in the run, its
// position in the journal, and the chain hash that attests everything up to
// and including it.
//
// The envelope is recorded exactly as it was produced. Nothing about the
// chain is written into it (ADR 0017).
type Record struct {
	Sequence   uint64         `json:"sequence"`
	Kind       string         `json:"kind"`
	Envelope   event.Envelope `json:"envelope"`
	RecordHash string         `json:"record_hash"`
}

// Chain computes a journal's record hashes in order, starting from its
// header. It is the one definition of the chain: a writer advances one, and
// a verifier recomputes with another.
//
// Use NewChain: a Chain must be seeded from the header it belongs to, and
// the zero value would chain a journal to no run at all.
type Chain struct {
	previous [sha256.Size]byte
}

// NewChain returns the chain for a journal under header.
//
// The chain is SEEDED with the header's own hash rather than with zeros,
// which is what binds the journal to the run it claims to be (ADR 0017).
// The header states the configuration hash, the strategy version, the span
// and the chain algorithm; outside the chain, all four could be rewritten —
// re-attributing a journal to a different configuration or a different
// build — while every record still verified. Seeded, any edit to any header
// field breaks record 1 and therefore every record after it.
func NewChain(header Header) *Chain {
	return &Chain{previous: sha256.Sum256(canonicalHeaderBytes(header))}
}

// canonicalHeaderBytes renders the header through the project's one
// canonical encoder (event.CanonicalBytes; ADR 0016), with timestamps as RFC
// 3339 in UTC so a journal round trip and a non-UTC process hash the same
// bytes — the same treatment CanonicalEnvelopeBytes gives an envelope.
func canonicalHeaderBytes(header Header) []byte {
	return event.CanonicalBytes(map[string]any{
		"journal_version":    header.JournalVersion,
		"chain_algorithm":    header.ChainAlgorithm,
		"configuration_hash": header.ConfigurationHash,
		"strategy_version":   header.StrategyVersion,
		"span_start":         header.SpanStart.UTC().Format(time.RFC3339Nano),
		"span_end":           header.SpanEnd.UTC().Format(time.RFC3339Nano),
	})
}

// Next returns the record hash for an entry, following everything already
// passed to this Chain.
//
// The kind is hashed alongside the envelope, not beside it: flipping a
// record from decision to input changes what a replay feeds in versus what
// it compares against, so a chain that left the kind out would protect the
// envelope but not the record's own claim about it (ADR 0017). The two kinds
// are a closed set and canonical envelope bytes always begin with '{', so
// the concatenation needs no separator to stay unambiguous.
func (c *Chain) Next(kind string, envelope event.Envelope) string {
	var hashed []byte
	hashed = append(hashed, c.previous[:]...)
	hashed = append(hashed, kind...)
	hashed = append(hashed, event.CanonicalEnvelopeBytes(envelope)...)
	sum := sha256.Sum256(hashed)
	c.previous = sum
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ChainBrokenError reports the first record whose hash does not follow from
// the record before it: the file was edited after it was written.
type ChainBrokenError struct {
	Sequence uint64
	Want     string
	Got      string
}

func (e *ChainBrokenError) Error() string {
	return fmt.Sprintf("journal: the chain is broken at record %d: the record states %s, but its envelope following the previous record hashes to %s", e.Sequence, e.Got, e.Want)
}

// Write records header and every entry, in order, as a journal.
//
// Every entry passes envelope validation before writing (ADR 0017).
// Encoding and I/O errors can still leave partial output; callers must
// retain the error alongside that evidence.
func Write(w io.Writer, header Header, entries []Entry) error {
	if err := header.validate(); err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("journal: a journal records at least one event; a run that recorded nothing is not evidence of anything")
	}
	for i, entry := range entries {
		if !validKind(entry.Kind) {
			return fmt.Errorf("journal: record %d: kind %q is neither %q nor %q", i+1, entry.Kind, KindInput, KindDecision)
		}
		if err := entry.Envelope.Validate(); err != nil {
			return fmt.Errorf("journal: record %d: %w", i+1, err)
		}
	}
	// Checked here rather than left to the caller: Recorder derives the span
	// from the run, but Write takes a header from anyone, and a span nothing
	// compares to the records is one a caller can state wrongly.
	start, end, ok := inputSpan(entries, entrySpanOf)
	if err := checkSpan(header, start, end, ok); err != nil {
		return err
	}

	buffered := bufio.NewWriter(w)
	if err := writeLine(buffered, header); err != nil {
		return err
	}
	chain := NewChain(header)
	var sequence uint64
	for _, entry := range entries {
		sequence++
		record := Record{
			Sequence:   sequence,
			Kind:       entry.Kind,
			Envelope:   entry.Envelope,
			RecordHash: chain.Next(entry.Kind, entry.Envelope),
		}
		if err := writeLine(buffered, record); err != nil {
			return err
		}
	}
	return buffered.Flush()
}

func writeLine(w *bufio.Writer, v any) error {
	// ADR 0017 hashes the payload bytes as supplied. Validate requires
	// compact JSON; disabling HTML escaping preserves the remaining bytes.
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return fmt.Errorf("journal: encode line: %w", err)
	}
	if _, err := w.Write(encoded.Bytes()); err != nil {
		return fmt.Errorf("journal: write line: %w", err)
	}
	return nil
}

// Read parses a journal into its header and records, without checking the
// chain. An unrecognised format version fails closed here, so a caller can
// never act on a journal this build does not understand.
func Read(r io.Reader) (Header, []Record, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return Header{}, nil, fmt.Errorf("journal: read header: %w", err)
		}
		return Header{}, nil, errors.New("journal: the file is empty; a journal begins with a header line")
	}
	var header Header
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		// bufio.Scanner hands back the partial line it holds when the reader
		// fails mid-line, so a read failure first surfaces here as malformed
		// JSON. Report the read failure itself: it is the cause, and a caller
		// deciding what happened (a cancellation, an I/O error) needs it.
		if readErr := scanner.Err(); readErr != nil {
			return Header{}, nil, fmt.Errorf("journal: read header: %w", readErr)
		}
		return Header{}, nil, fmt.Errorf("journal: decode header: %w", err)
	}
	if err := checkFormatVersion(header.JournalVersion); err != nil {
		return Header{}, nil, err
	}

	var records []Record
	for line := 2; scanner.Scan(); line++ {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var record Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			if readErr := scanner.Err(); readErr != nil {
				return header, nil, fmt.Errorf("journal: read the record on line %d: %w", line, readErr)
			}
			return header, nil, fmt.Errorf("journal: decode the record on line %d: %w", line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return header, nil, fmt.Errorf("journal: read: %w", err)
	}
	return header, records, nil
}

// checkFormatVersion fails closed in both directions: this build has no
// upcaster for an older journal and cannot know what a newer one means (ADR
// 0015's rule, applied to the journal format).
func checkFormatVersion(version uint32) error {
	switch {
	case version > FormatVersion:
		return fmt.Errorf("journal: journal version %d is newer than the version %d this build reads", version, FormatVersion)
	case version < FormatVersion:
		return fmt.Errorf("journal: journal version %d is older than the version %d this build reads, and no upcaster is registered", version, FormatVersion)
	}
	return nil
}

// CheckIdentity confirms every record states the run the header names: the
// same strategy version and the same configuration hash. It reports the
// first record that does not.
//
// Envelope.Validate requires both fields to be non-empty and nothing more,
// and the chain covers what each record says without comparing records to
// one another, so a journal can be internally consistent, verify cleanly,
// and still carry records from a run the header does not describe.
//
// This matters most for the INPUT stream. Replay equivalence feeds inputs to
// a reducer built from the header, and an input's own identity fields are
// never compared to anything; recorded decisions are already pinned, because
// the reducer stamps its emissions with the header's identity and any
// difference shows up as a divergence. Checking inputs here is what makes the
// header's claim load-bearing for the half of the journal that the byte
// comparison cannot reach.
//
// This is a separate question from chain verification (ADR 0017) and is kept
// a separate call for the same reason: "this file was edited" and "this file
// describes more than one run" are different findings.
func CheckIdentity(header Header, records []Record) error {
	for _, record := range records {
		if record.Envelope.StrategyVersion != header.StrategyVersion {
			return fmt.Errorf("journal: record %d states strategy version %q, but the header names the run %q", record.Sequence, record.Envelope.StrategyVersion, header.StrategyVersion)
		}
		if record.Envelope.ConfigurationHash != header.ConfigurationHash {
			return fmt.Errorf("journal: record %d states configuration %q, but the header names the run %q", record.Sequence, record.Envelope.ConfigurationHash, header.ConfigurationHash)
		}
	}
	return nil
}

// CheckSpan confirms the header states exactly the span its records cover:
// the first and last event time among the INPUTS, which is the period the run
// was given. It is the reader's half of the rule Write enforces on a writer.
//
// The chain covers the header's span (ADR 0017), so an edit to it is
// detectable — but only against a chain nobody repaired, and a run that
// composes its own header is trusted to state the span honestly rather than
// checked. Comparing the claim with the records is what makes it falsifiable,
// which matters most for a journal whose header was composed anywhere but
// Recorder.
//
// The span is a claim about the input stream alone: a decision is attributed
// to the input that caused it, and nothing stamps a decision's event time, so
// a decision outside the span is not the span's business. Records with no
// input among them fail closed — a file of decisions alone has no span it
// could state truthfully.
//
// Like CheckIdentity this is a separate question from chain verification, and
// a separate call for the same reason: "this file was edited" and "this file
// claims a span it did not cover" are different findings.
func CheckSpan(header Header, records []Record) error {
	start, end, ok := inputSpan(records, recordSpanOf)
	return checkSpan(header, start, end, ok)
}

func entrySpanOf(entry Entry) (string, time.Time) { return entry.Kind, entry.Envelope.EventTime }

func recordSpanOf(record Record) (string, time.Time) {
	return record.Kind, record.Envelope.EventTime
}

// inputSpan is the earliest and latest event time among the inputs in items,
// and whether items held an input at all.
//
// Widening in both directions is deliberate. Nothing requires a composed
// stream to be sorted by event time — several instruments interleave, and a
// fill is delivered around the bar it belongs to — so a span taken as "the
// first input's time, widened forwards" would report a journal as starting
// later than the earliest event it holds.
func inputSpan[T any](items []T, of func(T) (string, time.Time)) (start, end time.Time, ok bool) {
	for _, item := range items {
		kind, at := of(item)
		if kind != KindInput {
			continue
		}
		switch {
		case !ok:
			start, end, ok = at, at, true
		case at.Before(start):
			start = at
		case at.After(end):
			end = at
		}
	}
	return start, end, ok
}

// checkSpan compares a header's stated span with the one its records derive.
func checkSpan(header Header, start, end time.Time, ok bool) error {
	if !ok {
		return errors.New("journal: the header states a span but no record is an input: the span is the first and last input event time, and a journal of decisions alone is not evidence of anything the run was given")
	}
	if !header.SpanStart.Equal(start) || !header.SpanEnd.Equal(end) {
		return fmt.Errorf("journal: the header states the span %s to %s, but the inputs recorded run from %s to %s: a header cannot state a span the run did not cover",
			spanTime(header.SpanStart), spanTime(header.SpanEnd), spanTime(start), spanTime(end))
	}
	return nil
}

func spanTime(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

// Split separates a journal's two interleaved streams by each record's own
// Kind (ADR 0017): every input, in recording order, then every decision, in
// recording order. This is the split replay equivalence reads to decide what
// to feed a fresh reducer and what to compare its output against — Kind
// lives on the record and is covered by the chain precisely so this split
// cannot be forged undetectably (see Chain.Next).
//
// A record whose Kind is outside the closed set fails closed rather than
// being silently sorted into one pile or the other.
func Split(records []Record) (inputs, decisions []event.Envelope, err error) {
	for _, record := range records {
		switch record.Kind {
		case KindInput:
			inputs = append(inputs, record.Envelope)
		case KindDecision:
			decisions = append(decisions, record.Envelope)
		default:
			return nil, nil, fmt.Errorf("journal: record %d states kind %q, which is neither %q nor %q", record.Sequence, record.Kind, KindInput, KindDecision)
		}
	}
	return inputs, decisions, nil
}

// Verification is what a journal that verifies reports about itself. The
// final record hash is the value a run registry anchors, turning "detectable
// if you kept the original" into "detectable, full stop" (ADR 0017).
type Verification struct {
	Header          Header
	RecordCount     uint64
	FinalRecordHash string
}

// Verify checks the chain (ADR 0017), then validates the header and envelopes
// using Write's validators. A broken chain remains a *ChainBrokenError;
// invalid content in an intact chain is a separate validation error naming
// the header or first invalid record. Replay equivalence remains a separate
// check: Verify does not execute inputs or compare the recorded decisions.
func Verify(r io.Reader) (Verification, error) {
	header, records, err := Read(r)
	if err != nil {
		return Verification{}, err
	}
	if len(records) == 0 {
		return Verification{}, errors.New("journal: the journal records no events at all")
	}

	// Seeded from the header as READ, never from what it ought to say, so a
	// rewritten header fails at record 1 instead of being trusted.
	chain := NewChain(header)
	var final string
	var want uint64
	for _, record := range records {
		want++
		if record.Sequence != want {
			return Verification{}, fmt.Errorf("journal: record %d states sequence %d: a journal's own sequence is contiguous from 1, so a record is missing or out of order", want, record.Sequence)
		}
		if !validKind(record.Kind) {
			return Verification{}, fmt.Errorf("journal: record %d states kind %q, which is neither %q nor %q", record.Sequence, record.Kind, KindInput, KindDecision)
		}
		computed := chain.Next(record.Kind, record.Envelope)
		if !strings.EqualFold(computed, record.RecordHash) {
			return Verification{}, &ChainBrokenError{Sequence: record.Sequence, Want: computed, Got: record.RecordHash}
		}
		final = computed
	}

	if err := header.validate(); err != nil {
		return Verification{}, err
	}
	for _, record := range records {
		if err := record.Envelope.Validate(); err != nil {
			return Verification{}, fmt.Errorf("journal: record %d: %w", record.Sequence, err)
		}
	}

	return Verification{
		Header:          header,
		RecordCount:     uint64(len(records)),
		FinalRecordHash: final,
	}, nil
}

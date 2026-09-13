// Package journal writes and verifies a run's journal: the complete ordered
// stream of input and decision events, one record per line, under a header
// naming the configuration the run used.
//
// # Tamper-evidence, not tamper-proofing
//
// Each record carries a hash chained over the previous record's hash, the
// record's own kind, and the canonical bytes of its envelope (ADR 0017), so
// altering event k breaks every link after k and a silent edit to recorded
// history is detectable without a secret. It is evidence, not proof: whoever can rewrite one record
// can rewrite the whole file, and dropping records from the end leaves a
// valid chain. Anchoring each run's final record hash outside the system —
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
// so a verifier is told rather than assuming.
const ChainAlgorithm = "sha256(previous_record_hash||kind||canonical_envelope_bytes)"

// ZeroRecordHash is the predecessor of the first record: 32 zero bytes. It
// starts the chain, so the whole chain is reproducible from the envelope
// stream alone.
const ZeroRecordHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

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

// Chain computes a journal's record hashes in order, starting from
// ZeroRecordHash. It is the one definition of the chain: a writer advances
// one, and a verifier recomputes with another.
type Chain struct {
	previous [sha256.Size]byte
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
	canonical := event.CanonicalEnvelopeBytes(envelope)
	hashed := make([]byte, 0, len(c.previous)+len(kind)+len(canonical))
	hashed = append(hashed, c.previous[:]...)
	hashed = append(hashed, kind...)
	hashed = append(hashed, canonical...)
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
// Every entry is validated before anything is written, so a run that
// produced one that cannot be journalled fails before leaving a partial
// file behind rather than after.
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

	buffered := bufio.NewWriter(w)
	if err := writeLine(buffered, header); err != nil {
		return err
	}
	var chain Chain
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
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("journal: encode line: %w", err)
	}
	if _, err := w.Write(append(encoded, '\n')); err != nil {
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

// Verification is what a journal that verifies reports about itself. The
// final record hash is the value a run registry anchors, turning "detectable
// if you kept the original" into "detectable, full stop" (ADR 0017).
type Verification struct {
	Header          Header
	RecordCount     uint64
	FinalRecordHash string
}

// Verify recomputes the chain over a journal and reports the first broken
// link by sequence (as a *ChainBrokenError).
//
// It answers one question — was this file edited after it was written — and
// deliberately not the other: whether replaying the inputs still produces
// the recorded decisions is replay equivalence's question, and keeping the
// two apart is what makes the two failures distinguishable.
func Verify(r io.Reader) (Verification, error) {
	header, records, err := Read(r)
	if err != nil {
		return Verification{}, err
	}
	if len(records) == 0 {
		return Verification{}, errors.New("journal: the journal records no events at all")
	}

	var chain Chain
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

	return Verification{
		Header:          header,
		RecordCount:     uint64(len(records)),
		FinalRecordHash: final,
	}, nil
}

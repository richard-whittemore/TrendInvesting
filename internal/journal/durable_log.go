package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// LogFormatVersion is the durable log format this build writes and can
// read. RecoverLog rejects any other value in both directions, for the same
// reason Read does (ADR 0015's rule, applied to the log format).
const LogFormatVersion uint32 = 1

// LogChainAlgorithm names how a durable log's record hashes are computed.
// The record formula is the journal's (ADR 0017); only the seed differs,
// because a log's header states no span (ADR 0027).
const LogChainAlgorithm = "seed=sha256(canonical_log_header_bytes); record=sha256(previous_record_hash||kind||canonical_envelope_bytes)"

// SyncWriter is what a durable log is written to: an io.Writer whose Sync
// returns only once everything written is on stable storage, as *os.File's
// does. Opening the file is the composition root's job; this package never
// opens a path.
type SyncWriter interface {
	io.Writer
	Sync() error
}

// LogHeader is a durable log's first line: which run it records. It states
// the run's identity and no span, which is what lets it be written before
// the first input arrives (ADR 0027).
type LogHeader struct {
	LogVersion        uint32 `json:"log_version"`
	ChainAlgorithm    string `json:"chain_algorithm"`
	ConfigurationHash string `json:"configuration_hash"`
	StrategyVersion   string `json:"strategy_version"`
}

// NewLogHeader returns the durable log header for a run, filling in the
// format version and chain algorithm this build writes.
func NewLogHeader(configurationHash, strategyVersion string) LogHeader {
	return LogHeader{
		LogVersion:        LogFormatVersion,
		ChainAlgorithm:    LogChainAlgorithm,
		ConfigurationHash: configurationHash,
		StrategyVersion:   strategyVersion,
	}
}

func (h LogHeader) validate() error {
	var errs []error
	if h.LogVersion != LogFormatVersion {
		errs = append(errs, fmt.Errorf("log version %d is not the version %d this build writes", h.LogVersion, LogFormatVersion))
	}
	if h.ChainAlgorithm != LogChainAlgorithm {
		errs = append(errs, fmt.Errorf("chain algorithm %q is not the algorithm %q this build computes", h.ChainAlgorithm, LogChainAlgorithm))
	}
	if h.ConfigurationHash == "" {
		errs = append(errs, errors.New("configuration hash is required"))
	}
	if h.StrategyVersion == "" {
		errs = append(errs, errors.New("strategy version is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("journal: invalid durable log header: %w", err)
	}
	return nil
}

// newLogChain seeds a chain with the log header's own hash, so an edit to
// the run a log claims to record breaks record 1, exactly as ADR 0017's seed
// does for a journal.
func newLogChain(header LogHeader) *Chain {
	return &Chain{previous: sha256.Sum256(canonicalLogHeaderBytes(header))}
}

// canonicalLogHeaderBytes renders the log header through the project's one
// canonical encoder (event.CanonicalBytes; ADR 0016).
func canonicalLogHeaderBytes(header LogHeader) []byte {
	return event.CanonicalBytes(map[string]any{
		"log_version":        header.LogVersion,
		"chain_algorithm":    header.ChainAlgorithm,
		"configuration_hash": header.ConfigurationHash,
		"strategy_version":   header.StrategyVersion,
	})
}

// logLine is one line of a durable log after its header: exactly one of a
// record, chained as a journal record is, or the commit that closes the
// group of records one input caused (ADR 0027).
type logLine struct {
	Record *Record    `json:"record,omitempty"`
	Commit *logCommit `json:"commit,omitempty"`
}

// logCommit closes a group by naming its last record and that record's
// hash. A group without one was never made durable as a whole, so none of
// its decisions was released.
type logCommit struct {
	Sequence   uint64 `json:"sequence"`
	RecordHash string `json:"record_hash"`
}

// LogFailedError reports a durable log that a write or a flush failed on.
// The log refuses every later append: after a failed fsync the kernel may
// already have dropped the unflushed pages, so a retry that succeeds proves
// nothing about them (ADR 0027).
type LogFailedError struct {
	Err error
}

func (e *LogFailedError) Error() string {
	return fmt.Sprintf("journal: the durable log failed and accepts nothing further; restart from it to recover what it holds: %v", e.Err)
}

func (e *LogFailedError) Unwrap() error { return e.Err }

// LogWriter streams a run's records to stable storage as they are decided:
// a header, then for each input one group (the input, the decisions it
// caused, and a commit), flushed before Append returns (ADR 0027).
type LogWriter struct {
	w         SyncWriter
	header    LogHeader
	chain     *Chain
	sequence  uint64
	finalHash string
	failed    error
}

// CreateLog writes header to w and flushes it, so a log exists on disk
// before any input is recorded in it. w must be empty: a log is evidence,
// and is never appended to by a second process (ADR 0018).
func CreateLog(w SyncWriter, header LogHeader) (*LogWriter, error) {
	if err := header.validate(); err != nil {
		return nil, err
	}
	encoded, err := encodeLine(header)
	if err != nil {
		return nil, err
	}
	if err := writeAll(w, encoded); err != nil {
		return nil, fmt.Errorf("journal: write the durable log header: %w", err)
	}
	if err := w.Sync(); err != nil {
		return nil, fmt.Errorf("journal: flush the durable log header: %w", err)
	}
	return &LogWriter{w: w, header: header, chain: newLogChain(header)}, nil
}

// Header returns the header this log was created under.
func (l *LogWriter) Header() LogHeader { return l.header }

// FinalRecordHash returns the hash of the last committed record, or "" for
// a log with none.
func (l *LogWriter) FinalRecordHash() string { return l.finalHash }

// Append writes one group — an input followed by the decisions it caused —
// and its commit in a single write, then flushes, and returns only once the
// group is durable. This is the fsync policy: one flush per input, after the
// whole group and before any of its decisions may be released (ADR 0027).
//
// A group that is not one valid input and its valid decisions is refused
// before anything is written, and the log stays usable. A write or flush
// that fails returns a *LogFailedError, and every later Append returns it
// again without touching w.
func (l *LogWriter) Append(group []Entry) error {
	if l.failed != nil {
		return l.failed
	}
	if err := validateGroup(group); err != nil {
		return err
	}
	chain := *l.chain
	sequence := l.sequence
	var lastHash string
	var buf bytes.Buffer
	for _, entry := range group {
		sequence++
		lastHash = chain.Next(entry.Kind, entry.Envelope)
		encoded, err := encodeLine(logLine{Record: &Record{Sequence: sequence, Kind: entry.Kind, Envelope: entry.Envelope, RecordHash: lastHash}})
		if err != nil {
			return err
		}
		buf.Write(encoded)
	}
	encoded, err := encodeLine(logLine{Commit: &logCommit{Sequence: sequence, RecordHash: lastHash}})
	if err != nil {
		return err
	}
	buf.Write(encoded)

	if err := writeAll(l.w, buf.Bytes()); err != nil {
		l.failed = &LogFailedError{Err: fmt.Errorf("write: %w", err)}
		return l.failed
	}
	if err := l.w.Sync(); err != nil {
		l.failed = &LogFailedError{Err: fmt.Errorf("flush: %w", err)}
		return l.failed
	}
	l.chain = &chain
	l.sequence = sequence
	l.finalHash = lastHash
	return nil
}

// validateGroup applies the writer's validators to one group: exactly one
// input, first, then only decisions, every envelope valid (ADR 0017).
func validateGroup(group []Entry) error {
	if len(group) == 0 {
		return errors.New("journal: a durable log group records one input; this one is empty")
	}
	for i, entry := range group {
		want := KindDecision
		if i == 0 {
			want = KindInput
		}
		if entry.Kind != want {
			return fmt.Errorf("journal: entry %d of a durable log group is %q; a group is one input followed by its decisions", i+1, entry.Kind)
		}
		if err := entry.Envelope.Validate(); err != nil {
			return fmt.Errorf("journal: entry %d of a durable log group: %w", i+1, err)
		}
	}
	return nil
}

func encodeLine(v any) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, fmt.Errorf("journal: encode line: %w", err)
	}
	return encoded.Bytes(), nil
}

func writeAll(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return err
}

// TornTail is the part of a durable log after its last commit that a crash
// part way through an Append left behind. It was never committed, so none
// of its decisions was released, and it is excluded from what a restart
// recovers. The file itself is never modified.
type TornTail struct {
	// Offset is where the tail begins: the byte after the last commit line.
	Offset int64
	// Length is how many bytes it holds, to the end of the file.
	Length int64
	// Records is how many complete, valid record lines of the uncommitted
	// group it holds before its final fragment, if any.
	Records int
}

// LogDamageError reports a durable log that is damaged in a way a crash
// during an Append cannot cause. Nothing is recovered from it: the bytes a
// repair would drop may hold a committed group whose orders were sent.
type LogDamageError struct {
	Line   int
	Offset int64
	Reason string
}

func (e *LogDamageError) Error() string {
	return fmt.Sprintf("journal: the durable log is damaged at line %d (byte %d): %s; that is not the torn tail a crash leaves, so nothing is recovered from it and a person must decide what it holds", e.Line, e.Offset, e.Reason)
}

// Recovery is what a durable log holds: every committed group, in order,
// and the torn tail after them if a crash left one.
type Recovery struct {
	Header          LogHeader
	Entries         []Entry
	FinalRecordHash string
	Torn            *TornTail
}

// JournalHeader returns the ADR 0017 journal header for the recovered
// entries, with the span derived from their inputs exactly as Recorder
// derives it, so the entries can be written as a journal that verifies.
func (r Recovery) JournalHeader() (Header, error) {
	start, end, ok := inputSpan(r.Entries, entrySpanOf)
	if !ok {
		return Header{}, errors.New("journal: the durable log commits no input, so there is no span to state")
	}
	header := NewHeader(r.Header.ConfigurationHash, r.Header.StrategyVersion, start, end)
	if err := header.validate(); err != nil {
		return Header{}, err
	}
	return header, nil
}

// RecoverLog reads a durable log and returns every committed group in it.
//
// The truncated-tail rule (ADR 0027): a log is recoverable when it is a
// complete valid header, then committed groups whose every line is complete
// and valid and whose chain is intact, then optionally a torn tail of
// complete valid record lines of one uncommitted group followed by at most
// one final fragment with no newline. The torn tail is reported, not
// recovered. Anything else returns a *LogDamageError.
func RecoverLog(r io.Reader) (Recovery, error) {
	reader := bufio.NewReader(r)
	headerLine, err := readLogLine(reader)
	if err != nil {
		return Recovery{}, fmt.Errorf("journal: read the durable log header: %w", err)
	}
	if headerLine.n == 0 {
		return Recovery{}, errors.New("journal: the durable log is empty; it begins with a header line, so nothing was ever recorded in it")
	}
	if !headerLine.complete {
		return Recovery{}, errors.New("journal: the durable log's header line is incomplete; a log is created with its header flushed before any input, so nothing was ever recorded in it")
	}
	var header LogHeader
	if err := decodeStrict(headerLine.bytes, &header); err != nil {
		return Recovery{}, fmt.Errorf("journal: decode the durable log header: %w", err)
	}
	if err := checkLogVersion(header.LogVersion); err != nil {
		return Recovery{}, err
	}
	if err := header.validate(); err != nil {
		return Recovery{}, err
	}

	scan := logScan{chain: newLogChain(header), offset: headerLine.n, committedOffset: headerLine.n}
	for line := 2; ; line++ {
		next, err := readLogLine(reader)
		if err != nil {
			return Recovery{}, fmt.Errorf("journal: read the durable log at line %d: %w", line, err)
		}
		if next.n == 0 {
			break
		}
		if !next.complete {
			scan.offset += next.n
			break
		}
		if reason := scan.accept(next, scan.offset+next.n); reason != "" {
			return Recovery{}, &LogDamageError{Line: line, Offset: scan.offset, Reason: reason}
		}
		scan.offset += next.n
	}

	recovery := Recovery{Header: header, Entries: scan.committed, FinalRecordHash: scan.committedHash}
	if scan.offset > scan.committedOffset {
		recovery.Torn = &TornTail{Offset: scan.committedOffset, Length: scan.offset - scan.committedOffset, Records: len(scan.pending)}
	}
	return recovery, nil
}

// logScan is RecoverLog's position in a log: what is committed, and the
// group being read since the last commit.
type logScan struct {
	chain           *Chain
	sequence        uint64
	offset          int64
	committed       []Entry
	committedOffset int64
	committedHash   string
	pending         []Entry
	pendingHash     string
}

// accept checks one complete line, ending at byte end, against everything
// before it and takes it, or returns why the log is damaged.
func (s *logScan) accept(line logLineRead, end int64) string {
	if line.tooLong {
		return fmt.Sprintf("a line is longer than the %d bytes a record may take", maxLineBytes)
	}
	var parsed logLine
	if err := decodeStrict(line.bytes, &parsed); err != nil {
		return fmt.Sprintf("the line is not a durable log line: %v", err)
	}
	switch {
	case parsed.Record != nil && parsed.Commit == nil:
		return s.acceptRecord(*parsed.Record)
	case parsed.Commit != nil && parsed.Record == nil:
		return s.acceptCommit(*parsed.Commit, end)
	default:
		return "a durable log line is exactly one of a record and a commit"
	}
}

func (s *logScan) acceptRecord(record Record) string {
	if record.Sequence != s.sequence+1 {
		return fmt.Sprintf("the record states sequence %d after %d; a log's sequence is contiguous from 1", record.Sequence, s.sequence)
	}
	want := KindDecision
	if len(s.pending) == 0 {
		want = KindInput
	}
	if record.Kind != want {
		return fmt.Sprintf("record %d is %q where the group needs %q: a group is one input followed by its decisions, closed by a commit", record.Sequence, record.Kind, want)
	}
	if err := record.Envelope.Validate(); err != nil {
		return fmt.Sprintf("record %d: %v", record.Sequence, err)
	}
	computed := s.chain.Next(record.Kind, record.Envelope)
	if computed != record.RecordHash {
		return fmt.Sprintf("the chain is broken at record %d: the record states %s, but its envelope following the previous record hashes to %s", record.Sequence, record.RecordHash, computed)
	}
	s.sequence = record.Sequence
	s.pending = append(s.pending, Entry{Kind: record.Kind, Envelope: record.Envelope})
	s.pendingHash = computed
	return ""
}

func (s *logScan) acceptCommit(commit logCommit, end int64) string {
	if len(s.pending) == 0 {
		return "a commit closes no group"
	}
	if commit.Sequence != s.sequence || commit.RecordHash != s.pendingHash {
		return fmt.Sprintf("the commit names record %d (%s), but the group ends at record %d (%s)", commit.Sequence, commit.RecordHash, s.sequence, s.pendingHash)
	}
	s.committed = append(s.committed, s.pending...)
	s.pending = nil
	s.committedHash = s.pendingHash
	s.committedOffset = end
	return ""
}

// logLineRead is one line read from a durable log: its bytes without the
// newline, whether a newline ended it, whether it ran past maxLineBytes
// (its bytes are then truncated), and how many bytes it took in the file.
type logLineRead struct {
	bytes    []byte
	complete bool
	tooLong  bool
	n        int64
}

// readLogLine reads up to and including the next newline, holding at most
// maxLineBytes of it so a damaged file cannot make recovery allocate
// without limit. A line not ended by a newline is the end of the file.
func readLogLine(r *bufio.Reader) (logLineRead, error) {
	var line logLineRead
	for {
		chunk, err := r.ReadSlice('\n')
		line.n += int64(len(chunk))
		if !line.tooLong {
			if len(line.bytes)+len(chunk) > maxLineBytes+1 {
				line.tooLong = true
			} else {
				line.bytes = append(line.bytes, chunk...)
			}
		}
		switch {
		case err == nil:
			line.complete = true
			line.bytes = bytes.TrimSuffix(line.bytes, []byte("\n"))
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line, nil
		default:
			return logLineRead{}, err
		}
	}
}

// decodeStrict decodes one JSON value that must fill the whole line and name
// no field the type does not declare: a durable log is read back only by the
// build family that wrote it, so anything unexpected is damage.
func decodeStrict(line []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the line holds more than one JSON value")
	}
	return nil
}

// checkLogVersion fails closed in both directions, as checkFormatVersion
// does for a journal.
func checkLogVersion(version uint32) error {
	switch {
	case version > LogFormatVersion:
		return fmt.Errorf("journal: durable log version %d is newer than the version %d this build reads", version, LogFormatVersion)
	case version < LogFormatVersion:
		return fmt.Errorf("journal: durable log version %d is older than the version %d this build reads, and no upcaster is registered", version, LogFormatVersion)
	}
	return nil
}

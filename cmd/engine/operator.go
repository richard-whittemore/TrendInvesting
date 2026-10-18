package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/operator"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// refusedSuffix names the note the engine writes beside an inbox record it
// refused, saying why; a record with a note is never delivered again.
const refusedSuffix = ".refused"

// operatorInbox is the folder of -operator, where the operator tool records
// the owner's kill switch and rebalance clearances (internal/operator).
// Before each adapter input, under the wire guard's lock, every record not
// yet delivered is delivered as an input at the next positions of the run's
// one input sequence, in file-name order, which is the order the operator
// recorded them; the adapter's input follows them. Its decisions and theirs
// return to the adapter together, carrying the last sequence recorded, which
// the adapter's next input continues (the wire contract, engine.go). The
// durable log is the evidence (ADR 0027): a record is delivered once, when
// its ID is in no earlier log of the directory of -log, this run's resumed
// log included, and in no earlier exchange of this run. A record the engine
// cannot read is never delivered; one the run refuses is journalled as
// delivered with no effect. Each is noted beside it in the inbox
// (refusedSuffix) and on the engine's output, and never delivered again.
type operatorInbox struct {
	dir               string
	delivered         map[string]bool
	out               io.Writer
	strategyVersion   string
	configurationHash string
}

// openOperatorInbox checks -operator names a directory and gathers the IDs
// of the operator inputs every log in the directory of -log records, so none
// is delivered twice.
func openOperatorInbox(opts options, comp composition, out io.Writer) (*operatorInbox, error) {
	info, err := os.Stat(opts.operatorDir)
	if err != nil {
		return nil, fmt.Errorf("engine: the operator inbox: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("engine: the operator inbox %s is not a directory", opts.operatorDir)
	}
	_, recovered, err := readLogs(opts)
	if err != nil {
		return nil, err
	}
	inbox := &operatorInbox{
		dir: opts.operatorDir, delivered: map[string]bool{}, out: out,
		strategyVersion: comp.strategyVersion, configurationHash: comp.configurationHash,
	}
	for _, recovery := range recovered {
		for _, entry := range recovery.Entries {
			if entry.Kind == journal.KindInput && entry.Envelope.Source == operator.Source {
				inbox.delivered[entry.Envelope.ID] = true
			}
		}
	}
	return inbox, nil
}

// pending returns the records not yet delivered, in file-name order,
// noting each file that cannot be read as a record.
func (b *operatorInbox) pending() []operator.Record {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		b.report("engine: list the operator inbox %s: %v; its records wait for the next input", b.dir, err)
		return nil
	}
	names := make([]string, 0, len(entries))
	noted := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, refusedSuffix):
			noted[strings.TrimSuffix(name, refusedSuffix)] = true
		case entry.Type().IsRegular() && strings.HasSuffix(name, ".json"):
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var records []operator.Record
	for _, name := range names {
		if noted[name] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(b.dir, name))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				b.report("engine: read the operator record %s: %v; it waits for the next input", name, err)
			}
			continue
		}
		r, err := operator.Decode(raw)
		if err != nil {
			b.refuse(name, err)
			continue
		}
		if !b.delivered[r.ID] {
			records = append(records, r)
		}
	}
	return records
}

// deliver applies every pending record through engine at the sequence the
// adapter's input names, returning their decisions and the sequence the
// adapter's input is recorded at. When the adapter's input does not
// continue the stream, nothing is delivered, and the engine refuses that
// input as it would without an inbox.
func (b *operatorInbox) deliver(ctx context.Context, engine *replay.Engine, input event.Envelope) (decisions []event.Envelope, sequence uint64) {
	next, ok := engine.Next()
	if !ok || next != input.Sequence {
		return nil, input.Sequence
	}
	for _, r := range b.pending() {
		if ctx.Err() != nil {
			break
		}
		out, err := engine.Apply(ctx, r.Envelope(next, b.strategyVersion, b.configurationHash))
		b.delivered[r.ID] = true
		decisions = append(decisions, out...)
		if after, _ := engine.Next(); after == next+1 {
			next = after
			b.report("engine: recorded operator input %s at sequence %d", r.ID, after-1)
		}
		if err != nil {
			b.refuse(r.FileName(), err)
		}
	}
	return decisions, next
}

// refuse notes beside the record named name why it was refused.
func (b *operatorInbox) refuse(name string, why error) {
	b.report("engine: refused operator record %s: %v", name, why)
	note := filepath.Join(b.dir, name+refusedSuffix)
	if err := os.WriteFile(note, []byte(why.Error()+"\n"), 0o600); err != nil { // #nosec G703 -- the note is written beside a record in the inbox the operator named with -operator.
		b.report("engine: note the refusal of %s at %s: %v", name, note, err)
	}
}

func (b *operatorInbox) report(format string, args ...any) {
	_, _ = fmt.Fprintf(b.out, format+"\n", args...)
}

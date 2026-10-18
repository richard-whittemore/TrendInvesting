package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/operator"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// refusedSuffix names the note the engine writes beside an inbox record it
// refused, saying why; a record with a note is never delivered again.
const refusedSuffix = ".refused"

// deliveredSuffix names the marker the engine writes beside an inbox record
// once its input is flushed to the durable log.
const deliveredSuffix = ".delivered"

// operatorInbox is the folder of -operator, where the operator tool records
// the owner's kill switch and rebalance clearances (internal/operator).
// Before each adapter input, under the wire guard's lock, every record not
// yet delivered is delivered as an input at the next positions of the run's
// one input sequence, in file-name order, which is the order of the times
// the records state (the operator's -at, which may backdate a record, not
// the order they were written in); the adapter's input follows them. Its
// decisions and theirs return to the adapter together, carrying the last
// sequence recorded, which the adapter's next input continues (the wire
// contract, engine.go).
//
// The durable log is the evidence (ADR 0027): a record is delivered once,
// when its ID is in no earlier log of the directory of -log, this run's
// resumed log included, and in no earlier exchange of this run, and it has
// no delivered marker (deliveredSuffix) beside it, which is written once its
// input is flushed to the log, so a log later moved out of the directory
// (quarantined, or a run started with -log elsewhere) cannot cause a second
// delivery. The owner's instructions are ordered by the time they state: a
// kill switch stating a time no later than the newest kill switch already
// journalled for its account is refused, so a stale instruction never
// undoes a later one.
//
// A record the engine cannot read is never delivered; one the run refuses
// is journalled as delivered with no effect. Each is noted beside it in the
// inbox (refusedSuffix) and on the engine's output, and never delivered
// again. A record whose input could not be made durable was not delivered:
// it gets no note, and the run stops (ADR 0027 §3: a failed write or flush
// ends the log), so the next start delivers it.
//
// Each record is read once: what was decoded, and which names are settled,
// is kept, so an adapter input re-reads only the inbox's listing.
type operatorInbox struct {
	dir string
	// delivered holds the IDs journalled in a readable log or in this run.
	delivered map[string]bool
	// settled holds the file names never to read again: noted, marked
	// delivered, or delivered by this run.
	settled map[string]bool
	// decoded holds the records read but not yet delivered, by file name.
	decoded map[string]operator.Record
	// latestKillSwitch is the newest kill switch journalled for each
	// account, by the time it states.
	latestKillSwitch  map[string]operator.Record
	out               io.Writer
	strategyVersion   string
	configurationHash string
}

// openOperatorInbox checks -operator names a directory and gathers the IDs
// of the operator inputs every log in the directory of -log records, so none
// is delivered twice, and the newest kill switch each records.
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
		dir: opts.operatorDir, delivered: map[string]bool{}, settled: map[string]bool{},
		decoded: map[string]operator.Record{}, latestKillSwitch: map[string]operator.Record{}, out: out,
		strategyVersion: comp.strategyVersion, configurationHash: comp.configurationHash,
	}
	for _, recovery := range recovered {
		for _, entry := range recovery.Entries {
			env := entry.Envelope
			if entry.Kind != journal.KindInput || env.Source != operator.Source {
				continue
			}
			inbox.delivered[env.ID] = true
			if env.Type != event.KillSwitchEventType {
				continue
			}
			var p event.KillSwitchPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				inbox.journalledKillSwitch(operator.Record{ID: env.ID, Type: env.Type, At: env.EventTime, AccountID: p.AccountID})
			}
		}
	}
	return inbox, nil
}

// journalledKillSwitch keeps r as its account's newest kill switch when it
// states a later time than the one kept.
func (b *operatorInbox) journalledKillSwitch(r operator.Record) {
	if latest, ok := b.latestKillSwitch[r.AccountID]; !ok || r.At.After(latest.At) {
		b.latestKillSwitch[r.AccountID] = r
	}
}

// pending returns the records not yet delivered, in file-name order,
// noting each file that cannot be read as a record. A record the logs
// already hold is marked delivered, so it is never read again.
func (b *operatorInbox) pending() []operator.Record {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		b.report("engine: list the operator inbox %s: %v; its records wait for the next input", b.dir, err)
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, refusedSuffix):
			b.settle(strings.TrimSuffix(name, refusedSuffix))
		case strings.HasSuffix(name, deliveredSuffix):
			b.settle(strings.TrimSuffix(name, deliveredSuffix))
		case entry.Type().IsRegular() && strings.HasSuffix(name, ".json"):
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var records []operator.Record
	for _, name := range names {
		if b.settled[name] {
			continue
		}
		r, ok := b.decoded[name]
		if !ok {
			raw, err := os.ReadFile(filepath.Join(b.dir, name))
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					b.report("engine: read the operator record %s: %v; it waits for the next input", name, err)
				}
				continue
			}
			if r, err = operator.Decode(raw); err != nil {
				b.refuse(name, err)
				continue
			}
			b.decoded[name] = r
		}
		if b.delivered[r.ID] {
			b.markDelivered(name, "it is journalled in an earlier log of the -log directory")
			continue
		}
		records = append(records, r)
	}
	return records
}

// settle records that the file named name is never read again.
func (b *operatorInbox) settle(name string) {
	b.settled[name] = true
	delete(b.decoded, name)
}

// deliver applies every pending record through engine at the sequence the
// adapter's input names, returning their decisions and the sequence the
// adapter's input is recorded at. When the adapter's input does not
// continue the stream, nothing is delivered, and the engine refuses that
// input as it would without an inbox. A record whose input was not
// journalled and made durable ends the delivery with failed: it stays
// pending, unnoted, and the run stops (operatorInbox).
func (b *operatorInbox) deliver(ctx context.Context, engine *replay.Engine, input event.Envelope) (decisions []event.Envelope, sequence uint64, failed error) {
	next, ok := engine.Next()
	if !ok || next != input.Sequence {
		return nil, input.Sequence, nil
	}
	for _, r := range b.pending() {
		select {
		case <-ctx.Done():
			// The wire abandoned this exchange: what remains waits, and the
			// wire guard refuses the adapter's input.
			return decisions, next, nil
		default:
		}
		name := r.FileName()
		if latest, ok := b.latestKillSwitch[r.AccountID]; ok && r.Type == event.KillSwitchEventType && !r.At.After(latest.At) {
			b.refuse(name, fmt.Errorf("operator: kill switch %s states %s, no later than %s, already journalled for account %s; the owner's instructions are ordered by the time they state, so this one would undo a later one: record it again with the current time if it is still meant",
				r.ID, r.At.Format(time.RFC3339), latest.ID, r.AccountID))
			continue
		}
		out, after, failed := b.apply(ctx, engine, r, next)
		if failed != nil {
			return decisions, next, failed
		}
		decisions, next = append(decisions, out...), after
	}
	return decisions, next, nil
}

// apply delivers r at sequence next, returning its decisions and the
// sequence after it. A refusal of r's own, journalled, is noted beside it;
// any other error, where r's input was not journalled and made durable,
// is returned as failed and leaves r pending.
func (b *operatorInbox) apply(ctx context.Context, engine *replay.Engine, r operator.Record, next uint64) (decisions []event.Envelope, after uint64, failed error) {
	name := r.FileName()
	out, err := engine.Apply(ctx, r.Envelope(next, b.strategyVersion, b.configurationHash))
	after, _ = engine.Next()
	var durability *journal.DurabilityError
	if err != nil && (after != next+1 || errors.As(err, &durability) || ctx.Err() != nil) {
		return nil, next, fmt.Errorf("engine: operator record %s was not delivered, so it stays in the inbox and is delivered when the run is resumed: %w", name, err)
	}
	b.delivered[r.ID] = true
	b.report("engine: recorded operator input %s at sequence %d", r.ID, after-1)
	if r.Type == event.KillSwitchEventType {
		b.journalledKillSwitch(r)
	}
	b.markDelivered(name, fmt.Sprintf("it is journalled at sequence %d", after-1))
	if err != nil {
		b.refuse(name, err)
	}
	return out, after, nil
}

// markDelivered writes and flushes the marker beside the record named name
// saying it was delivered and where, after its input is flushed to the
// durable log, so it is never delivered again even if that log is moved. A
// marker that cannot be written is reported; the log still holds the
// input.
func (b *operatorInbox) markDelivered(name, where string) {
	b.settle(name)
	marker := filepath.Join(b.dir, name+deliveredSuffix)
	err := writeFlushed(marker, []byte("delivered: "+where+"\n"))
	if err == nil {
		err = syncDir(b.dir)
	}
	if err != nil {
		b.report("engine: mark %s delivered at %s: %v", name, marker, err)
	}
}

// writeFlushed writes raw at path and flushes it.
func writeFlushed(path string, raw []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- the marker is written beside a record in the inbox the operator named with -operator.
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// refuse notes beside the record named name why it was refused.
func (b *operatorInbox) refuse(name string, why error) {
	b.settle(name)
	b.report("engine: refused operator record %s: %v", name, why)
	note := filepath.Join(b.dir, name+refusedSuffix)
	if err := os.WriteFile(note, []byte(why.Error()+"\n"), 0o600); err != nil { // #nosec G703 -- the note is written beside a record in the inbox the operator named with -operator.
		b.report("engine: note the refusal of %s at %s: %v", name, note, err)
	}
}

func (b *operatorInbox) report(format string, args ...any) {
	_, _ = fmt.Fprintf(b.out, format+"\n", args...)
}

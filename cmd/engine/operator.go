package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/operator"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// refusedSuffix names the note the engine writes, in its notes folder under an inbox
// record's name, when it refused the record, saying why; a record with a
// note is never delivered again. An earlier layout wrote it beside the
// record in the inbox, and one found there still settles the record.
const refusedSuffix = ".refused"

// deliveredSuffix names the marker the engine writes, in its notes folder under
// an inbox record's name, once the record's input is flushed to the durable
// log (found beside the record in the inbox, it settles the record too).
const deliveredSuffix = ".delivered"

// operatorClockSkew is how far after the engine's clock an operator record
// may state its time: the operator tool and the engine may run on machines
// whose clocks differ by this much. A later time is a mistyped -at.
const operatorClockSkew = 5 * time.Minute

// operatorInbox is the folder of -operator, where the operator tool records
// the owner's kill switch, rebalance clearances and alert acknowledgements
// (internal/operator).
// Before each adapter input, under the wire guard's lock, every record not
// yet delivered is delivered as an input at the next positions of the run's
// one input sequence, in file-name order, which is the order of the times
// the records state (the operator's -at, which may backdate a record, not
// the order they were written in); the adapter's input follows them. Its
// decisions return to the adapter carrying the sequence it was recorded at,
// which the adapter's next input continues; an input sent again is answered
// from its committed group and delivers nothing (the wire contract,
// engine.go).
//
// The durable log is the evidence (ADR 0027): a record is delivered once,
// when its ID is in no earlier log of the directory of -log, this run's
// resumed log included, and in no earlier exchange of this run, and it has
// no delivered marker (deliveredSuffix) beside it, which is written once its
// input is flushed to the log, so a log later moved out of the directory
// (quarantined, or a run started with -log elsewhere) cannot cause a second
// delivery. The owner's instructions count in the order of the times they
// state (ADR 0019, amendment of 2026-10-17, "operator instructions count in
// the order of the times they state"): a kill switch stating a time earlier
// than the newest one the gate applied for its account is refused by the
// gate (risklimit.Gate.SetKillSwitch), so a stale instruction never undoes a
// later one, and its refusal is journalled. A record of any kind stating a
// time later than the engine's clock, beyond operatorClockSkew, would order
// every truthful instruction before it, so it is never delivered.
//
// A record the engine cannot read, or that states a time more than
// operatorClockSkew ahead of the engine's clock, is never delivered; one
// the run refuses is journalled as delivered with no effect. Each is noted
// under its file name in the engine's notes folder (operatorNotesDir,
// refusedSuffix) and on the engine's output, and
// never delivered again. A record whose input the recorder did not take (a
// failed write or flush, which ends the log, ADR 0027 §3, or the recorder's
// record bound or emission ceiling, ADR 0017) was not delivered: it gets no
// note, and the run stops, so the next start delivers it.
//
// Each record is read once: what was decoded, and which names are settled,
// is kept, so an adapter input re-reads only the inbox's listing. Every
// note and marker is written in the notes folder under the name of the
// file the record was read from, whatever that name.
type operatorInbox struct {
	dir string
	// notes is the engine's own folder beside the inbox, where it writes
	// its delivered markers and refusal notes (operatorNotesDir).
	notes string
	// delivered holds the IDs journalled in a readable log or in this run.
	delivered map[string]bool
	// settled holds the file names never to read again: noted, marked
	// delivered, or delivered by this run.
	settled map[string]bool
	// decoded holds the records read but not yet delivered, by file name.
	decoded map[string]operator.Record
	// recorder is the run's recorder, which says whether a record's input
	// was appended to the log (journal.Recorder.Committed).
	recorder          *journal.Recorder
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
	if err := checkInboxIdentity(opts.operatorDir, info); err != nil {
		return nil, err
	}
	notes, err := openOperatorNotes(opts.operatorDir)
	if err != nil {
		return nil, err
	}
	_, recovered, err := readLogs(opts)
	if err != nil {
		return nil, err
	}
	inbox := &operatorInbox{
		dir: opts.operatorDir, notes: notes, delivered: map[string]bool{}, settled: map[string]bool{},
		decoded: map[string]operator.Record{}, out: out,
		strategyVersion: comp.strategyVersion, configurationHash: comp.configurationHash,
	}
	for _, recovery := range recovered {
		for _, entry := range recovery.Entries {
			if env := entry.Envelope; entry.Kind == journal.KindInput && env.Source == operator.Source {
				inbox.delivered[env.ID] = true
			}
		}
	}
	return inbox, nil
}

// inboxRecord is a record and the name of the file it was read from, which
// need not be the name the operator tool gives it (operator.Record.FileName).
type inboxRecord struct {
	name string
	operator.Record
}

// pending returns the records not yet delivered, in file-name order,
// noting each file that cannot be read as a record. A record the logs
// already hold is marked delivered, so it is never read again.
func (b *operatorInbox) pending() ([]inboxRecord, error) {
	noted, err := os.ReadDir(b.notes)
	if err != nil {
		return nil, fmt.Errorf("engine: list the engine's operator notes %s, which say which records are settled, so the inbox cannot be read safely: %w", b.notes, err)
	}
	for _, entry := range noted {
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, refusedSuffix):
			b.settle(strings.TrimSuffix(name, refusedSuffix))
		case strings.HasSuffix(name, deliveredSuffix):
			b.settle(strings.TrimSuffix(name, deliveredSuffix))
		}
	}
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		b.report("engine: list the operator inbox %s: %v; its records wait for the next input", b.dir, err)
		return nil, nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case !entry.Type().IsRegular():
		case strings.HasSuffix(name, ".json"):
			names = append(names, name)
		case strings.HasSuffix(name, refusedSuffix):
			// A marker an earlier layout wrote beside the record.
			b.settle(strings.TrimSuffix(name, refusedSuffix))
		case strings.HasSuffix(name, deliveredSuffix):
			b.settle(strings.TrimSuffix(name, deliveredSuffix))
		}
	}
	sort.Strings(names)
	var records []inboxRecord
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
		records = append(records, inboxRecord{name: name, Record: r})
	}
	return records, nil
}

// settle records that the file named name is never read again.
func (b *operatorInbox) settle(name string) {
	b.settled[name] = true
	delete(b.decoded, name)
}

// deliver applies every pending record through engine at the sequence the
// adapter's input names, returning the sequence the adapter's input is
// recorded at. The handlers decide nothing from an operator input (it sets
// what later decisions read), so the adapter's reply is its own input's
// committed group. When the adapter's input does not
// continue the stream, nothing is delivered, and the engine refuses that
// input as it would without an inbox. A record whose input was not
// journalled and made durable ends the delivery with failed: it stays
// pending, unnoted, and the run stops (operatorInbox).
func (b *operatorInbox) deliver(ctx context.Context, engine *replay.Engine, input event.Envelope) (sequence uint64, failed error) {
	next, ok := engine.Next()
	if !ok || next != input.Sequence {
		return input.Sequence, nil
	}
	records, err := b.pending()
	if err != nil {
		return next, err
	}
	for _, r := range records {
		select {
		case <-ctx.Done():
			// The wire abandoned this exchange: what remains waits, and the
			// wire guard refuses the adapter's input.
			return next, nil
		default:
		}
		if now := time.Now(); r.At.After(now.Add(operatorClockSkew)) {
			b.refuse(r.name, fmt.Errorf("operator: %s %s states %s, later than now (%s) by more than the %s the clocks may differ; it would order every instruction stating the true time before it, so it is never delivered: record it again with the current time",
				r.Type, r.ID, r.At.Format(time.RFC3339), now.UTC().Format(time.RFC3339), operatorClockSkew))
			continue
		}
		after, failed := b.apply(ctx, engine, r, next)
		if failed != nil {
			return next, failed
		}
		next = after
	}
	return next, nil
}

// apply delivers r at sequence next, returning the sequence after it.
// Whether r was delivered is the recorder's to say, not the error's: the
// replay engine moves past next on any error, and the recorder refuses an
// input of its own (its record bound, its emission ceiling, a failed write)
// without appending it (ADR 0027). An input the recorder took was
// journalled, and a refusal of it, by the handler, is noted beside r; one it
// did not take is returned as failed and leaves r pending.
func (b *operatorInbox) apply(ctx context.Context, engine *replay.Engine, r inboxRecord, next uint64) (after uint64, failed error) {
	name := r.name
	_, err := engine.Apply(ctx, r.Envelope(next, b.strategyVersion, b.configurationHash))
	if committed, ok := b.recorder.Committed(r.ID); !ok || committed.Input.Sequence != next {
		if err == nil {
			err = errors.New("the recorder did not take it")
		}
		return next, fmt.Errorf("engine: operator record %s was not delivered, so it stays in the inbox and is delivered at the next start: %w", name, err)
	}
	after = next + 1
	b.delivered[r.ID] = true
	b.report("engine: recorded operator input %s at sequence %d", r.ID, after-1)
	b.markDelivered(name, fmt.Sprintf("it is journalled at sequence %d", after-1))
	if err != nil {
		b.refuse(name, err)
	}
	return after, nil
}

// markDelivered writes and flushes the marker beside the record named name
// saying it was delivered and where, after its input is flushed to the
// durable log, so it is never delivered again even if that log is moved. A
// marker that cannot be written is reported; the log still holds the
// input.
func (b *operatorInbox) markDelivered(name, where string) {
	b.settle(name)
	marker := filepath.Join(b.notes, name+deliveredSuffix)
	err := writeFlushed(marker, []byte("delivered: "+where+"\n"))
	if err == nil {
		err = syncDir(b.notes)
	}
	if err != nil {
		b.report("engine: mark %s delivered at %s: %v", name, marker, err)
	}
}

// writeFlushed writes raw at path and flushes it.
func writeFlushed(path string, raw []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- the marker is written in the engine's notes folder beside the inbox the operator named with -operator.
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
	note := filepath.Join(b.notes, name+refusedSuffix)
	if err := os.WriteFile(note, []byte(why.Error()+"\n"), 0o600); err != nil { // #nosec G703 -- the note is written in the engine's notes folder beside the inbox the operator named with -operator.
		b.report("engine: note the refusal of %s at %s: %v", name, note, err)
	}
}

func (b *operatorInbox) report(format string, args ...any) {
	_, _ = fmt.Fprintf(b.out, format+"\n", args...)
}

// operatorNotesDir is the engine's own folder for what it notes about the
// records of inbox: the inbox's path followed by "-notes"
// (/var/lib/trend-investing/engines/<account>/operator-notes on the
// server). The engine cannot write the inbox (checkInboxIdentity), so its
// notes are kept beside it, under the same name a record has.
func operatorNotesDir(inbox string) string {
	return filepath.Clean(inbox) + "-notes"
}

// The owner of a file, the user the engine runs as and its groups, as the
// operating system says; seams a test sets other identities through.
var (
	fileOwner = func(_ string, info os.FileInfo) (uid, gid int, known bool) {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, 0, false
		}
		return int(st.Uid), int(st.Gid), true
	}
	engineUser   = os.Geteuid
	engineGroups = func() ([]int, error) {
		groups, err := os.Getgroups()
		return append(groups, os.Getegid()), err
	}
)

// checkInboxIdentity refuses an inbox whose instructions might not be the
// owner's, or that the engine cannot read. An instruction's writer is an
// identity of its own, a user the engine and every network-facing unit do
// not run as (ti-operator on the server; ADR 0026, kill-switch amendment;
// docs/manual/operator.md, "Who can write an instruction"), so the engine
// refuses an inbox it could write to itself: one its group or others may
// write, one owned by root or by the engine's own user, or any inbox when
// it runs as root. It reads the inbox only through its group, since the tool
// writes each record 0640 in that group: the engine's user must be in the
// inbox's group, the mode must be 0750 (no access for others), and a record
// there must open. A folder or record it cannot read would leave a kill
// switch unread while it decides, so that is refused too (fail closed, ADR
// 0019).
func checkInboxIdentity(dir string, info os.FileInfo) error {
	perm := info.Mode().Perm()
	if perm&0o022 != 0 {
		return fmt.Errorf("engine: the operator inbox %s is mode %o, writable by its group or by others; the provisioner creates it 0750 (docs/manual/server.md)", dir, perm)
	}
	uid, gid, known := fileOwner(dir, info)
	if !known {
		return fmt.Errorf("engine: cannot tell who owns the operator inbox %s, so an instruction in it may not be the owner's", dir)
	}
	me := engineUser()
	switch {
	case me == 0:
		return fmt.Errorf("engine: the engine runs as root, which can write any operator inbox, so an instruction in %s may not be the owner's; run it as its own user (ti-trader on the server)", dir)
	case uid == 0:
		return fmt.Errorf("engine: the operator inbox %s is owned by root; it must belong to the user that writes the owner's instructions (ti-operator on the server, docs/manual/server.md)", dir)
	case uid == me:
		return fmt.Errorf("engine: the operator inbox %s is owned by the engine's own user (%d), which could then write its own instructions; it must belong to a user the engine does not run as (ti-operator on the server, docs/manual/server.md)", dir, uid)
	}
	groups, err := engineGroups()
	if err != nil {
		return fmt.Errorf("engine: the engine's groups, to check it can read the operator inbox %s: %w", dir, err)
	}
	if perm&0o007 != 0 {
		return fmt.Errorf("engine: the operator inbox %s is mode %o, readable by others; the records are written 0640 in its group, so the engine reads them only through that group: chmod 0750 %s", dir, perm, dir)
	}
	if !slices.Contains(groups, gid) || perm&0o050 != 0o050 {
		return fmt.Errorf("engine: the engine's user cannot list and read the operator inbox %s (mode %o, group %d): its group must be one the engine's user is in, with read and list (chgrp ti-trader and chmod 0750; docs/manual/server.md)", dir, perm, gid)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("engine: the engine cannot list the operator inbox %s: %w", dir, err)
	}
	for _, entry := range entries {
		if name := entry.Name(); entry.Type().IsRegular() && strings.HasSuffix(name, ".json") {
			file, err := os.Open(filepath.Join(dir, name)) // #nosec G304 -- the probe reads a record in the inbox the operator named with -operator.
			if err != nil {
				return fmt.Errorf("engine: the engine cannot read the operator record %s in %s, so a kill switch could wait unread: records are written 0640 in the inbox's group, which the engine's user must be in: %w", name, dir, err)
			}
			_ = file.Close()
			break
		}
	}
	return nil
}

// openOperatorNotes creates the engine's notes folder beside inbox, mode
// 0700, or checks the one there is the engine's own: a note settles a
// record for good, so a note another user could write would silence the
// owner's kill switch.
func openOperatorNotes(inbox string) (string, error) {
	notes := operatorNotesDir(inbox)
	if err := os.Mkdir(notes, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("engine: create the engine's operator notes %s: %w", notes, err)
	}
	info, err := os.Lstat(notes)
	if err != nil {
		return "", fmt.Errorf("engine: the engine's operator notes: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("engine: the engine's operator notes %s is not a directory", notes)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return "", fmt.Errorf("engine: the engine's operator notes %s is mode %o, writable by its group or by others; the engine creates it 0700", notes, perm)
	}
	if perm := info.Mode().Perm(); perm&0o700 != 0o700 {
		return "", fmt.Errorf("engine: the engine cannot list and read its operator notes %s (mode %o); the engine creates it 0700", notes, perm)
	}
	if uid, _, known := fileOwner(notes, info); !known || uid != engineUser() {
		return "", fmt.Errorf("engine: the engine's operator notes %s is not the engine's own (owner %d, the engine runs as %d); remove it and the engine creates it", notes, uid, engineUser())
	}
	return notes, nil
}

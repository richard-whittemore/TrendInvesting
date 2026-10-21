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
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// flexStatusSuffix names the Flex reader's status file beside its ledger
// (adapter/ibkr/flex.py STATUS_SUFFIX).
const flexStatusSuffix = ".status.json"

// flexLedger is -cash-ledger, the Flex reader's ledger of the instance's
// deposits and withdrawals (/var/lib/trend-investing/flex/<instance>/
// cash-movements.jsonl), and its status file beside it. Before each adapter
// input, under the wire guard's lock and after the operator inbox's records,
// every ledger record and the reader's current status not yet journalled in
// this run are delivered as inputs at the next sequences of the run's one
// input sequence: the records in the ledger's order, as
// account.cash-movement version 2, then the status, as account.flex-status,
// so a status that read is journalled after the records it covers. The
// handler bridges the reconciliation's cash with them (ADR 0019, amended
// 2026-10-21).
//
// The engine reads the files and never writes them: the reader writes them
// (ti-flex@<instance>), and nothing the reader writes can place an order. A
// record is journalled once in a run, its ID being the reader's; a run's own
// log, a resumed one included, says which are journalled. A fresh run gets
// every record again: those dated before its book are not applied.
//
// Each line is checked as the reader writes it (event.DecodeFlexLedger). A
// ledger with a line that is not is refused whole: none of its records is
// delivered, the refusal is said once on the engine's output, and a cash
// difference a refused record would have explained is a difference, never
// passed over. A status the engine cannot read is not delivered, and said
// likewise; the last status journalled stands. A ledger or status not yet
// written delivers nothing. A record or status whose input the recorder did
// not take (a failed write or flush, which ends the log, or its record bound
// or emission ceiling) stops the run, as an operator record does, so the
// next start delivers it.
type flexLedger struct {
	path      string
	accountID string
	// delivered holds the IDs of the ledger records and statuses journalled
	// in this run.
	delivered map[string]bool
	// said is the last refusal said for the ledger and for the status, so
	// each is said once until it changes.
	saidLedger, saidStatus string
	recorder               *journal.Recorder
	out                    io.Writer
	// readFile reads a file; nil is os.ReadFile.
	readFile          func(string) ([]byte, error)
	strategyVersion   string
	configurationHash string
}

// openFlexLedger checks -cash-ledger names a ledger of this engine's own
// instance and gathers the Flex inputs recovered, which this run already
// holds. The ledger's records carry no account: the reader writes each
// instance's ledger in its own folder, named for the instance as the
// engine's state folder is (flex/<instance>/ and engines/<instance>/), so a
// ledger in a folder named for another instance is another account's and is
// refused, as is a folder that does not exist. Only a target-weight run that
// keeps a durable log reads one.
func openFlexLedger(opts options, comp composition, recovered *journal.Recovery, out io.Writer) (*flexLedger, error) {
	if comp.accountID == "" {
		return nil, errors.New("engine: -cash-ledger applies to a target-weight account; a Turtle configuration reads no Flex ledger")
	}
	if opts.logPath == "" {
		return nil, errors.New("engine: -cash-ledger requires -log: the Flex inputs are journalled in the run's durable log, which says which are delivered")
	}
	folder := filepath.Dir(filepath.Clean(opts.cashLedger))
	info, err := os.Stat(folder)
	if err != nil {
		return nil, fmt.Errorf("engine: the Flex ledger's folder: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("engine: the Flex ledger's folder %s is not a directory", folder)
	}
	instance, state := filepath.Base(folder), filepath.Base(filepath.Dir(filepath.Clean(opts.logPath)))
	if instance != state {
		return nil, fmt.Errorf("engine: the Flex ledger %s is instance %s's, but this engine's state folder is %s's: a ledger is read only by its own instance's engine (docs/manual/reconciliation.md)", opts.cashLedger, instance, state)
	}
	l := &flexLedger{
		path: opts.cashLedger, accountID: comp.accountID, delivered: map[string]bool{}, out: out,
		strategyVersion: comp.strategyVersion, configurationHash: comp.configurationHash,
	}
	if recovered != nil {
		for _, entry := range recovered.Entries {
			if entry.Kind == journal.KindInput && entry.Envelope.Source == event.FlexSource {
				l.delivered[entry.Envelope.ID] = true
			}
		}
	}
	return l, nil
}

// pending returns the Flex inputs not yet journalled in this run: the
// ledger's records, then the status. The status is read first: the reader
// writes its ledger before the status saying "read" (adapter/ibkr/flex.py,
// read_once), so a ledger read after the status holds every transfer that
// status covers, and a reading that finishes between the two reads only
// adds records, never a coverage the delivered records lack (ADR 0019,
// amended 2026-10-21).
func (l *flexLedger) pending() []event.Envelope {
	var out []event.Envelope
	rawStatus, statusRead := l.read(l.path+flexStatusSuffix, &l.saidStatus)
	if raw, ok := l.read(l.path, &l.saidLedger); ok {
		records, err := event.DecodeFlexLedger(raw)
		if err != nil {
			l.say(&l.saidLedger, fmt.Sprintf("engine: the Flex ledger %s is refused, so none of its transfers is delivered: %v", l.path, err))
		} else {
			l.saidLedger = ""
			for _, r := range records {
				if !l.delivered[r.ID] {
					out = append(out, l.envelope(r.ID, event.CashMovementEventType, event.CashMovementTransferVersion, r.Payload.AsOf, r.Payload))
				}
			}
		}
	}
	if statusRead {
		status, err := event.DecodeFlexStatus(rawStatus, l.accountID)
		if err != nil {
			l.say(&l.saidStatus, fmt.Sprintf("engine: the Flex status %s%s is not delivered: %v", l.path, flexStatusSuffix, err))
		} else {
			l.saidStatus = ""
			id := "ibkr-flex-status:" + status.CheckedAt.UTC().Format(time.RFC3339) + ":" + status.State + ":" + status.CoveredTo
			if !l.delivered[id] {
				out = append(out, l.envelope(id, event.FlexStatusEventType, event.FlexStatusSchemaVersion, status.CheckedAt, status))
			}
		}
	}
	return out
}

// read returns the file at path, or false when it is not there yet or
// cannot be read, which is said once.
func (l *flexLedger) read(path string, said *string) ([]byte, bool) {
	readFile := l.readFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	raw, err := readFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false
	case err != nil:
		l.say(said, fmt.Sprintf("engine: read %s: %v", path, err))
		return nil, false
	}
	return raw, true
}

func (l *flexLedger) say(said *string, text string) {
	if *said == text {
		return
	}
	*said = text
	_, _ = fmt.Fprintln(l.out, text)
}

func (l *flexLedger) envelope(id, eventType string, version uint32, at time.Time, payload any) event.Envelope {
	raw, err := json.Marshal(payload)
	if err != nil {
		// Unreachable: both payloads are plain structs of strings, numbers
		// and times already validated.
		panic(fmt.Sprintf("engine: encode %s: %v", id, err))
	}
	return event.Envelope{
		ID: id, Type: eventType, SchemaVersion: version, EnvelopeVersion: event.CurrentEnvelopeVersion,
		EventTime: at, RecordedAt: at, Source: event.FlexSource,
		StrategyVersion: l.strategyVersion, ConfigurationHash: l.configurationHash,
		PayloadHash: event.HashPayload(raw), Payload: raw,
	}
}

// deliver applies every pending Flex input through engine at the sequence
// the adapter's input names, returning the sequence the adapter's input is
// recorded at, as operatorInbox.deliver does. An input the handler refused
// was journalled, is said, and is not delivered again; one the recorder did
// not take ends the delivery with failed, and the run stops.
func (l *flexLedger) deliver(ctx context.Context, engine *replay.Engine, input event.Envelope) (sequence uint64, failed error) {
	next, ok := engine.Next()
	if !ok || next != input.Sequence {
		return input.Sequence, nil
	}
	for _, in := range l.pending() {
		select {
		case <-ctx.Done():
			return next, nil
		default:
		}
		in.Sequence = next
		_, err := engine.Apply(ctx, in)
		if committed, ok := l.recorder.Committed(in.ID); !ok || committed.Input.Sequence != next {
			if err == nil {
				err = errors.New("the recorder did not take it")
			}
			return next, fmt.Errorf("engine: Flex input %s was not delivered, so it is delivered at the next start: %w", in.ID, err)
		}
		l.delivered[in.ID] = true
		_, _ = fmt.Fprintf(l.out, "engine: recorded Flex input %s at sequence %d\n", in.ID, next)
		if err != nil {
			_, _ = fmt.Fprintf(l.out, "engine: the run refused Flex input %s: %v\n", in.ID, err)
		}
		next++
	}
	return next, nil
}

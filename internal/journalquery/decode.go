package journalquery

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// item is one journal record the queries read, decoded. Record types the
// queries do not read are skipped; a type they do read at a schema version
// they do not know is an error, never a guess (ADR 0015).
//
// Exactly one payload pointer is set.
type item struct {
	journal string
	seq     uint64
	at      time.Time
	// account is the account the record belongs to: its own, where the
	// payload names one, otherwise the journal's single account (see
	// attributeAccounts), otherwise empty.
	account string

	fill      *event.FillPayload
	planned   *event.OrderPlannedPayload
	released  *event.OrderReleasedPayload
	refused   *event.OrderRefusedPayload
	lifecycle *event.OrderLifecyclePayload
	engine    *event.EngineStatePayload
	snapshot  *event.AccountPositionsSnapshotPayload
}

func decode(j Journal) ([]item, error) {
	var items []item
	for _, record := range j.Records {
		env := record.Envelope
		it := item{journal: j.Name, seq: record.Sequence, at: env.EventTime}
		var err error
		switch env.Type {
		case event.FillEventType:
			it.fill = new(event.FillPayload)
			err = read(env, []uint32{event.FillSchemaVersion}, it.fill)
			it.at = it.fill.FilledAt
		case event.OrderPlannedEventType:
			it.planned = new(event.OrderPlannedPayload)
			err = read(env, []uint32{event.OrderPlannedSchemaVersion}, it.planned)
			it.account = it.planned.AccountID
		case event.OrderReleasedEventType:
			it.released = new(event.OrderReleasedPayload)
			err = read(env, []uint32{event.OrderReleasedSchemaVersion}, it.released)
			it.account = it.released.AccountID
		case event.OrderRefusedEventType:
			it.refused = new(event.OrderRefusedPayload)
			err = read(env, []uint32{event.OrderRefusedSchemaVersion}, it.refused)
			it.account = it.refused.AccountID
		case event.OrderLifecycleEventType:
			it.lifecycle = new(event.OrderLifecyclePayload)
			err = read(env, []uint32{event.OrderLifecycleSchemaVersion}, it.lifecycle)
		case event.EngineStateEventType:
			it.engine = new(event.EngineStatePayload)
			err = read(env, []uint32{event.EngineStateSchemaVersion, event.EngineStateScopedVersion}, it.engine)
		case event.AccountSnapshotEventType:
			// Version 2 (equity and cash) carries no positions and is not
			// read here; version 3 is the broker's positions.
			if env.SchemaVersion != event.AccountSnapshotPositionsVersion {
				continue
			}
			it.snapshot = new(event.AccountPositionsSnapshotPayload)
			err = read(env, []uint32{event.AccountSnapshotPositionsVersion}, it.snapshot)
			it.account = it.snapshot.AccountID
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: record %d: %w", j.Name, record.Sequence, err)
		}
		items = append(items, it)
	}
	attributeAccounts(items)
	return items, nil
}

// attributeAccounts gives a record that names no account (a fill, an order
// lifecycle change, an engine-state change) the journal's account, when the
// journal's other records name exactly one. With none or several it leaves
// them unattributed rather than guessing.
func attributeAccounts(items []item) {
	seen := map[string]bool{}
	for _, it := range items {
		if it.account != "" {
			seen[it.account] = true
		}
	}
	if len(seen) != 1 {
		return
	}
	var only string
	for account := range seen {
		only = account
	}
	for i := range items {
		if items[i].account == "" {
			items[i].account = only
		}
	}
}

func read(env event.Envelope, schemas []uint32, into any) error {
	known := false
	for _, s := range schemas {
		known = known || env.SchemaVersion == s
	}
	if !known {
		return fmt.Errorf("%s at schema version %d, which this build does not read (it reads %v)", env.Type, env.SchemaVersion, schemas)
	}
	if err := json.Unmarshal(env.Payload, into); err != nil {
		return fmt.Errorf("decode %s: %w", env.Type, err)
	}
	return nil
}

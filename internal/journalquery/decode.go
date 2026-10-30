package journalquery

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
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

	fill      *fill
	planned   *event.OrderPlannedPayload
	released  *event.OrderReleasedPayload
	refused   *event.OrderRefusedPayload
	withheld  *event.SaleWithheldPayload
	skipped   *event.OrderSkippedPayload
	lifecycle *lifecycle
	engine    *event.EngineStatePayload
	snapshot  *event.AccountPositionsSnapshotPayload
	renamed   *event.InstrumentSymbolChangedPayload
	killed    *event.KillSwitchPayload
	cleared   *event.RebalanceClearedPayload
	approved  *event.OperatorRunApprovedPayload
}

// fill is one execution in either fill version the queries read: a Turtle
// fill (version 4), whose Kind is entry, add, stop or exit, or a
// target-weight limit order's fill (versions 5 and 6, ADR 0026 D7-F), whose Kind is its
// side, buy or sell. Quantity is unsigned units at QuantityScale. Exactly one of
// turtle and limit holds the record as written.
type fill struct {
	InstrumentID  string
	Kind          string
	Quantity      int64
	QuantityScale int64
	Price         float64
	Commission    float64

	turtle *event.FillPayload
	limit  *event.TargetWeightFillPayload
}

// lifecycle is one order state change in either lifecycle version: a Turtle
// stop order's (version 1), priced at its stop and noted with its tag, or a
// target-weight limit order's (version 2, ADR 0026), priced at its limit and
// noted with its client order id. Synthetic marks a dry-run engine's own
// report on an order it never sent (version 3), which is never an execution.
// Versions 4 and 5 carry the same distinction at fractional scale (ADR 0026 D7-F).
type lifecycle struct {
	InstrumentID, Status, Side, Note string
	Quantity                         int64
	QuantityScale                    int64
	Price                            float64
	Synthetic                        bool
}

type seenFill struct {
	seq     uint64
	payload fill
}

func decode(j Journal) ([]item, error) {
	var items []item
	fills := map[string]seenFill{}
	for _, record := range j.Records {
		env := record.Envelope
		it := item{journal: j.Name, seq: record.Sequence, at: env.EventTime}
		var err error
		switch env.Type {
		case event.FillEventType:
			it.fill, err = readFill(env)
			if it.fill != nil && it.fill.limit != nil {
				it.at, it.account = it.fill.limit.FilledAt, it.fill.limit.AccountID
			} else if it.fill != nil {
				it.at = it.fill.turtle.FilledAt
			}
		case event.OrderPlannedEventType:
			it.planned = new(event.OrderPlannedPayload)
			err = read(env, event.OrderPlannedVersions(), it.planned)
			it.account = it.planned.AccountID
		case event.OrderReleasedEventType:
			// Version 2 adds the IRA cash figures and account-wide exposure
			// to version 1's fields, and version 3 the broker figures, so
			// one payload type reads all three.
			it.released = new(event.OrderReleasedPayload)
			err = read(env, event.OrderReleasedVersions(), it.released)
			it.account = it.released.AccountID
		case event.OrderRefusedEventType:
			it.refused = new(event.OrderRefusedPayload)
			err = read(env, event.OrderRefusedVersions(), it.refused)
			it.account = it.refused.AccountID
		case event.SaleWithheldEventType:
			it.withheld = new(event.SaleWithheldPayload)
			err = read(env, []uint32{event.SaleWithheldSchemaVersion}, it.withheld)
			it.account = it.withheld.AccountID
		case event.OrderSkippedEventType:
			it.skipped = new(event.OrderSkippedPayload)
			err = read(env, event.OrderSkippedVersions(), it.skipped)
			it.account = it.skipped.AccountID
		case event.OrderLifecycleEventType:
			it.lifecycle, it.account, err = readLifecycle(env)
		case event.InstrumentSymbolChangedEventType:
			it.renamed = new(event.InstrumentSymbolChangedPayload)
			err = read(env, []uint32{event.InstrumentSymbolChangedSchemaVersion}, it.renamed)
		case event.EngineStateEventType:
			it.engine = new(event.EngineStatePayload)
			err = read(env, []uint32{event.EngineStateSchemaVersion, event.EngineStateScopedVersion}, it.engine)
		case event.KillSwitchEventType:
			it.killed = new(event.KillSwitchPayload)
			err = read(env, []uint32{event.KillSwitchSchemaVersion}, it.killed)
			it.at, it.account = it.killed.AsOf, it.killed.AccountID
		case event.RebalanceClearedEventType:
			it.cleared = new(event.RebalanceClearedPayload)
			err = read(env, []uint32{event.RebalanceClearedSchemaVersion}, it.cleared)
			it.at, it.account = it.cleared.AsOf, it.cleared.AccountID
		case event.OperatorRunApprovedEventType:
			// An approval is read at the version it was recorded at, version
			// 1 (no cash scope) included, by the rule every reader of a
			// recorded approval applies (ADR 0015).
			var p event.OperatorRunApprovedPayload
			if p, err = event.DecodeRecordedOperatorRunApproved(env.Payload, env.SchemaVersion); err != nil {
				err = fmt.Errorf("%s: %w", env.Type, err)
			}
			it.approved = &p
			it.at, it.account = it.approved.ApprovedAt, it.approved.AccountID
		case event.AccountSnapshotEventType:
			// Version 2 (equity and cash) carries no positions and is not
			// read here. Positions and cash may use whole or fractional
			// quantities (ADR 0026 D7-F); readSnapshot checks their scale.
			if env.SchemaVersion == event.AccountSnapshotSchemaVersion {
				continue
			}
			it.snapshot, err = readSnapshot(env)
			if it.snapshot != nil {
				it.account = it.snapshot.AccountID
			}
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: record %d: %w", j.Name, record.Sequence, err)
		}
		if it.fill != nil {
			// A repeated delivery of one fill is one execution (the reducer
			// ignores it too); the same id with different content is refused.
			if first, dup := fills[it.fill.id()]; dup {
				if !sameFill(first.payload, *it.fill) {
					return nil, fmt.Errorf("%s: record %d: fill %q conflicts with record %d, which used the same fill id differently", j.Name, record.Sequence, it.fill.id(), first.seq)
				}
				continue
			}
			fills[it.fill.id()] = seenFill{seq: record.Sequence, payload: *it.fill}
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
	seen := namedAccounts(items)
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

// namedAccounts is the set of accounts the records themselves name. It is the
// one rule for how many accounts a journal has: attribution and the fill
// replay both read it.
func namedAccounts(items []item) map[string]bool {
	seen := map[string]bool{}
	for _, it := range items {
		if it.account != "" {
			seen[it.account] = true
		}
	}
	return seen
}

// versioned is a payload whose fields depend on its schema version.
type versioned interface {
	SchemaVersion() uint32
	ValidateSchema(schemaVersion uint32) error
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
	// Legacy payloads without quantity metadata must still refuse it (ADR 0015).
	var units struct {
		QuantityScale int64 `json:"quantity_scale"`
	}
	if err := json.Unmarshal(env.Payload, &units); err != nil {
		return err
	}
	if _, ok := into.(versioned); !ok && units.QuantityScale != 0 {
		return fmt.Errorf("%s: quantity_scale is not supported at this version", env.Type)
	}
	// Chain verification does not check payload values, and a journal keeps
	// rejected inputs, so a record is validated before it can move a figure.
	// A payload whose fields depend on its version is checked against the
	// version it was recorded at, so a record carrying a later version's
	// field is refused (ADR 0015).
	if v, ok := into.(versioned); ok {
		if err := v.ValidateSchema(env.SchemaVersion); err != nil {
			return fmt.Errorf("%s: %w", env.Type, err)
		}
		return nil
	}
	if v, ok := into.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("%s: %w", env.Type, err)
		}
	}
	return nil
}

// readFill decodes Turtle and whole or fractional target-weight fills
// (ADR 0026 D7-F); unknown versions or mismatched scales are refused.
func readFill(env event.Envelope) (*fill, error) {
	if slices.Contains(event.TargetWeightFillVersions(), env.SchemaVersion) {
		p := new(event.TargetWeightFillPayload)
		if err := read(env, event.TargetWeightFillVersions(), p); err != nil {
			return nil, err
		}
		return &fill{InstrumentID: p.InstrumentID, Kind: p.Side, Quantity: p.Quantity, QuantityScale: p.QuantityScale, Price: p.Price, Commission: p.Commission, limit: p}, nil
	}
	p := new(event.FillPayload)
	if err := read(env, []uint32{event.FillSchemaVersion}, p); err != nil {
		return nil, err
	}
	return &fill{InstrumentID: p.InstrumentID, Kind: p.Kind, Quantity: p.Quantity, Price: p.Price, Commission: p.Commission, turtle: p}, nil
}

// readLifecycle decodes an order state change of any version, with the
// account a limit order's change names; any other version is refused. A
// limit order's versions 2/4 are venue reports and 3/5 synthetic reports,
// and each is refused at the other's version.
func readLifecycle(env event.Envelope) (*lifecycle, string, error) {
	limit := event.LimitOrderLifecycleVersions()
	if slices.Contains(limit, env.SchemaVersion) {
		p := new(event.LimitOrderLifecyclePayload)
		if err := read(env, limit, p); err != nil {
			return nil, "", err
		}
		return &lifecycle{InstrumentID: p.InstrumentID, Status: p.Status, Side: p.Side, Note: p.ClientOrderID, Quantity: p.Quantity, QuantityScale: p.QuantityScale, Price: p.LimitPrice, Synthetic: p.Synthetic}, p.AccountID, nil
	}
	p := new(event.OrderLifecyclePayload)
	if err := read(env, append([]uint32{event.OrderLifecycleSchemaVersion}, limit...), p); err != nil {
		return nil, "", err
	}
	return &lifecycle{InstrumentID: p.InstrumentID, Status: p.Status, Note: p.Tag, Quantity: p.Quantity, Price: p.StopPrice}, "", nil
}

// readSnapshot decodes whole or fractional positions, with or without cash
// (ADR 0026 D7-F). Cash is not reported; the declared scale is validated.
func readSnapshot(env event.Envelope) (*event.AccountPositionsSnapshotPayload, error) {
	if slices.Contains(event.AccountCashSnapshotVersions(), env.SchemaVersion) {
		p := new(event.AccountCashSnapshotPayload)
		if err := read(env, event.AccountCashSnapshotVersions(), p); err != nil {
			return nil, err
		}
		return &p.AccountPositionsSnapshotPayload, nil
	}
	p := new(event.AccountPositionsSnapshotPayload)
	if err := read(env, event.AccountPositionsSnapshotVersions(), p); err != nil {
		return nil, err
	}
	return p, nil
}

func (f fill) id() string {
	if f.limit != nil {
		return f.limit.FillID
	}
	return f.turtle.FillID
}

// sameFill reports whether two deliveries state one execution. Two Turtle
// fills follow the reducer's rules (strategy.acceptedFillState.matches): an
// absent and an empty unit_ids list are the same. For either version one
// instant is the same however its offset is written, and every other field
// is compared exactly. Fills of different versions are never the same.
func sameFill(x, y fill) bool {
	switch {
	case x.limit != nil && y.limit != nil:
		return x.limit.SameExecution(*y.limit)
	case x.turtle != nil && y.turtle != nil:
		a, b := *x.turtle, *y.turtle
		a.FilledAt, b.FilledAt = a.FilledAt.UTC(), b.FilledAt.UTC()
		if len(a.UnitIDs) == 0 {
			a.UnitIDs = nil
		}
		if len(b.UnitIDs) == 0 {
			b.UnitIDs = nil
		}
		return reflect.DeepEqual(a, b)
	}
	return false
}

// decodeWholeQuantities preserves the fail-closed contract of aggregations
// that still add whole shares (ADR 0015; ADR 0026 D7-F). Detail tables use
// decode and format each record at its own scale.
func decodeWholeQuantities(j Journal) ([]item, error) {
	for _, record := range j.Records {
		var units struct {
			QuantityScale int64 `json:"quantity_scale"`
		}
		if err := json.Unmarshal(record.Envelope.Payload, &units); err != nil {
			return nil, err
		}
		if units.QuantityScale != 0 {
			return nil, fmt.Errorf("%s: fractional quantities require the journal-query aggregation migration", record.Envelope.Type)
		}
	}
	return decode(j)
}

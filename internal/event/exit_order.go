package event

import (
	"errors"
	"fmt"
	"time"
)

// ExitOrderSetEventType identifies the exit-order decision payload for the
// Envelope's Type field: one Unit's Exit Order (CONTEXT.md: "Exit Order")
// came into force at this level.
//
// It adds to, and replaces nothing in, the two events it is derived from.
// ProtectiveStopSetEventType still records every Unit's own stop, and
// ExitProposalEventType still records an Exit-Channel exit for the whole
// Campaign; the fill simulator and the reducer's own logic read those two.
// This event is their per-Unit combination — the one level at which that
// Unit's single sell order should rest — so that a consumer mirroring
// orders never has to combine two strategy levels itself.
const ExitOrderSetEventType = "strategy.exit-order.set"

// ExitOrderSetSchemaVersion is the current schema version of
// ExitOrderSetPayload, for the Envelope's SchemaVersion field (ADR 0015).
const ExitOrderSetSchemaVersion uint32 = 1

// RuleExitOrderHigherOfStopAndExitChannel names the rule for
// ExitOrderSetPayload.Rule: a Unit's Exit Order rests at the higher of its
// own Protective Stop and, while an Exit-Channel exit is proposed, the Exit
// Channel level. A long Unit sold at the higher of two sell levels is sold
// at the first one price reaches, and one order at that level never sells
// more than the Unit holds.
const RuleExitOrderHigherOfStopAndExitChannel = "exit-order.higher-of-stop-and-exit-channel"

// ADRExitOrderRestsAtTheLevel is the ADR ExitOrderSetPayload.ADR cites: ADR
// 0005, under which a Protective Stop and an Exit-Channel exit are both a
// resting order at their level, filled in the bar whose range first covers
// it.
const ADRExitOrderRestsAtTheLevel = "0005"

// The two values ExitOrderSetPayload.Source carries: which of the two levels
// governs the Exit Order. A closed set, for the same "groupable journal"
// reasoning ProtectiveStopSetPayload.Reason follows.
const (
	// ExitOrderSourceProtectiveStop means the Unit's own Protective Stop
	// governs: no Exit-Channel exit is proposed, or the Exit Channel level
	// is not above the stop. A tie names the stop.
	ExitOrderSourceProtectiveStop = "protective-stop"
	// ExitOrderSourceExitChannel means a proposed Exit-Channel exit's level
	// governs, because it is strictly above the Unit's Protective Stop.
	ExitOrderSourceExitChannel = "exit-channel"
)

// ExitOrderSetPayload records one Unit's Exit Order coming into force at
// Level, for that Unit's own Quantity.
//
// ProtectiveStop and ExitChannelLevel restate the two levels it was derived
// from, so Level and Source are re-derivable from this payload alone:
// ExitChannelLevel is zero when no Exit-Channel exit is proposed, and Level
// is then the Protective Stop; otherwise Level is the higher of the two,
// with a tie naming the Protective Stop. A higher-of-two choice is exact —
// no arithmetic is involved — so Validate compares for exact equality, the
// same discipline every derived level in this package uses.
type ExitOrderSetPayload struct {
	CampaignID   string `json:"campaign_id"`
	InstrumentID string `json:"instrument_id"`
	// UnitIndex is which Unit this order belongs to: each Unit carries its
	// own order, because each carries its own Protective Stop.
	UnitIndex int `json:"unit_index"`
	// Level is where this Unit's Exit Order now rests.
	Level float64 `json:"level"`
	// Quantity is this Unit's own shares: the Exit Orders of a Campaign's
	// held Units together cover exactly its holding.
	Quantity int64 `json:"quantity"`
	// Source is one of the ExitOrderSource* constants.
	Source string `json:"source"`
	// ProtectiveStop is this Unit's Protective Stop at AsOf.
	ProtectiveStop float64 `json:"protective_stop"`
	// ExitChannelLevel is the level of the Exit-Channel exit proposed for
	// this Unit's Campaign at AsOf, or zero when none is.
	ExitChannelLevel float64 `json:"exit_channel_level"`
	// AsOf is when this level came into force: the time of the input that
	// changed it — a fill's timestamp, a completed bar's period end, or the
	// instant the input stream ended.
	AsOf time.Time `json:"as_of"`
	Rule string    `json:"rule"`
	ADR  string    `json:"adr"`
}

// Validate checks that the payload identifies the Campaign, the Unit and the
// moment the level came into force, that Quantity is positive, that both
// restated levels are usable, and that Level and Source are exactly what
// the two restated levels derive.
func (p ExitOrderSetPayload) Validate() error {
	var errs []error
	if p.CampaignID == "" {
		errs = append(errs, errors.New("campaign id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.UnitIndex < 1 {
		errs = append(errs, fmt.Errorf("unit index must be at least 1, got %d", p.UnitIndex))
	}
	if p.Quantity <= 0 {
		errs = append(errs, fmt.Errorf("quantity must be positive, got %d", p.Quantity))
	}
	switch p.Source {
	case ExitOrderSourceProtectiveStop, ExitOrderSourceExitChannel:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("source %q is not a recognised exit order source", p.Source))
	}
	switch {
	case p.AsOf.IsZero():
		errs = append(errs, errors.New("as of is required"))
	case !writableTime(p.AsOf):
		errs = append(errs, errors.New("as of cannot be written as RFC 3339"))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}

	stopUsable := false
	switch {
	case !isFinite(p.ProtectiveStop):
		errs = append(errs, errors.New("protective stop must be finite"))
	case p.ProtectiveStop <= 0:
		errs = append(errs, errors.New("protective stop must be positive: a long position cannot be stopped out at or below zero"))
	default:
		stopUsable = true
	}
	exitUsable := false
	switch {
	case !isFinite(p.ExitChannelLevel):
		errs = append(errs, errors.New("exit channel level must be finite"))
	case p.ExitChannelLevel < 0:
		errs = append(errs, errors.New("exit channel level must not be negative: zero means no exit-channel exit is proposed"))
	default:
		exitUsable = true
	}
	switch {
	case !isFinite(p.Level):
		errs = append(errs, errors.New("level must be finite"))
	case p.Level <= 0:
		errs = append(errs, errors.New("level must be positive: a long position cannot be sold at or below zero"))
	}

	if stopUsable && exitUsable {
		wantLevel, wantSource := p.ProtectiveStop, ExitOrderSourceProtectiveStop
		if p.ExitChannelLevel > p.ProtectiveStop {
			wantLevel, wantSource = p.ExitChannelLevel, ExitOrderSourceExitChannel
		}
		if isFinite(p.Level) && p.Level != wantLevel {
			errs = append(errs, fmt.Errorf(
				"stated level %v does not match the higher of protective stop %v and exit channel level %v (%v)",
				p.Level, p.ProtectiveStop, p.ExitChannelLevel, wantLevel))
		}
		if p.Source != wantSource {
			errs = append(errs, fmt.Errorf("source %q does not name the governing level: want %q", p.Source, wantSource))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid exit order set payload: %w", err)
	}
	return nil
}

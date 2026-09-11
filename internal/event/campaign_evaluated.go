package event

import (
	"errors"
	"fmt"
	"time"
)

// CampaignEvaluatedEventType identifies the per-bar Campaign decision payload
// for the Envelope's Type field: while an instrument is in an open Campaign,
// this is the one decision event a completed bar produces for it.
//
// CONTEXT.md defines a Setup as an Eligible instrument NOT in a Campaign, so
// an instrument already in a Campaign gets no Setup-evaluated event (#11's
// Findings) — but that left a bar in a Campaign emitting nothing at all
// except the invariant check (#12's Concerns explicitly named this a hole
// for #13 to fill). This event is that fill: the levels in force on this
// bar — the Protective Stop and the Exit Channel low — are journaled every
// bar a Campaign is open, whether or not either one triggers anything.
const CampaignEvaluatedEventType = "strategy.campaign.evaluated"

// CampaignEvaluatedSchemaVersion is the current schema version of
// CampaignEvaluatedPayload, for the Envelope's SchemaVersion field.
const CampaignEvaluatedSchemaVersion uint32 = 1

// CampaignEvaluatedPayload carries the outcome of evaluating one open
// Campaign on one completed bar: the Protective Stop currently in force, the
// Exit Channel low computed from the preceding ExitChannelLength completed
// bars (CONTEXT.md: "Exit Channel"; The Turtle Rules p.26, ADR 0002) and
// whether it is warmed up, and whether this bar's low breached it.
//
// ExitChannelLow is "the Exit Channel low in force for deciding this bar":
// computed from the completed bars PRECEDING it, never including this bar's
// own low — the same look-ahead discipline SetupEvaluatedPayload.N and
// EntryChannelHigh already document, mirrored on the exit side (see
// indicator.ExitChannel's doc comment).
//
// ExitChannelReady means at least ExitChannelLength completed bars have been
// added to the Exit Channel window. This can be false only if the Campaign
// opened very early in an instrument's history (fewer than ExitChannelLength
// completed bars existed before it did) — the window is fed every completed
// bar regardless of Campaign state, specifically so it is warm the moment a
// Campaign opens under ordinary circumstances. ExitChannelLow is 0 while not
// ready, the same zero convention EntryChannelHigh uses, so a consumer
// reading a level without checking readiness first sees an unambiguous "not
// evaluable" value rather than a stale one; Validate enforces both
// directions of that convention.
//
// ExitConditionMet is true only when ExitChannelReady is true AND this bar's
// low fell strictly below ExitChannelLow (The Turtle Rules p.26: price
// "falls below" the channel, mirroring p.19's "exceeds" for the Entry
// Channel — a tie is not a breach). Validate rejects ExitConditionMet==true
// while ExitChannelReady==false: an unready channel has no real level to
// have fallen below.
type CampaignEvaluatedPayload struct {
	CampaignID   string    `json:"campaign_id"`
	InstrumentID string    `json:"instrument_id"`
	PeriodEnd    time.Time `json:"period_end"`
	// ProtectiveStop is the Campaign's Protective Stop as it stands on this
	// bar (CONTEXT.md: "Every open Campaign has one at all times").
	ProtectiveStop float64 `json:"protective_stop"`
	// ExitChannelLow and ExitChannelReady are the Exit Channel's own reading
	// (see the type's doc comment).
	ExitChannelLow   float64 `json:"exit_channel_low"`
	ExitChannelReady bool    `json:"exit_channel_ready"`
	// ExitConditionMet is whether this bar's low breached the Exit Channel
	// (see the type's doc comment). When true, the same bar also emits an
	// ExitProposalPayload naming the same CampaignID and Level.
	ExitConditionMet bool `json:"exit_condition_met"`
}

// Validate checks that the payload identifies the Campaign, instrument and
// period, that ProtectiveStop is a usable positive number (every open
// Campaign has one — an invariant internal/strategy enforces independently;
// this Validate only checks the shape of the number it was handed), that
// ExitChannelLow is never negative and matches the zero-while-not-ready
// convention EntryChannelHigh already uses, and that ExitConditionMet is
// never true while the channel is not ready.
func (p CampaignEvaluatedPayload) Validate() error {
	var errs []error
	if p.CampaignID == "" {
		errs = append(errs, errors.New("campaign id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.PeriodEnd.IsZero() {
		errs = append(errs, errors.New("period end is required"))
	}

	switch {
	case !isFinite(p.ProtectiveStop):
		errs = append(errs, errors.New("protective stop must be finite"))
	case p.ProtectiveStop <= 0:
		errs = append(errs, errors.New("protective stop must be positive"))
	}

	switch {
	case !isFinite(p.ExitChannelLow):
		errs = append(errs, errors.New("exit channel low must be finite"))
	case p.ExitChannelLow < 0:
		errs = append(errs, errors.New("exit channel low must not be negative"))
	case p.ExitChannelReady && p.ExitChannelLow <= 0:
		errs = append(errs, errors.New("exit channel low must be positive when ready"))
	case !p.ExitChannelReady && p.ExitChannelLow != 0:
		errs = append(errs, errors.New("exit channel low must be zero while not ready"))
	}

	if !p.ExitChannelReady && p.ExitConditionMet {
		errs = append(errs, errors.New("exit condition met must be false while the exit channel is not ready"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid campaign evaluated payload: %w", err)
	}
	return nil
}

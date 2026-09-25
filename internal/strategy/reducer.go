// Package strategy holds the deterministic reducers that turn a validated
// event stream into decision events (docs/architecture.md: "Replay
// engine"). It implements replay.Handler and depends only on internal/event,
// internal/indicator, internal/sizing, and internal/replay: no LEAN,
// database, transport, or wall-clock access (AGENTS.md rule 7; enforced by
// depguard/forbidigo).
package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
	"github.com/richard-whittemore/TrendInvesting/internal/universe"
)

// sourceReducer is the value stamped into every envelope this package
// emits, identifying the component that produced it (docs/architecture.md:
// "the source that emitted the event").
const sourceReducer = "reducer"

// Reducer implements replay.Handler: it tracks each
// instrument's True Range, N (CONTEXT.md: "True Range", "N"), and Entry
// Channel (CONTEXT.md: "Entry Channel"; ADR 0002) from completed bars. It
// emits one Setup-evaluated decision event per bar, reporting N, the Entry
// Channel, the Setup's Tier, and its distance to entry in N; when the bar's
// high exceeds the Entry Channel (a Breakout, Tier A), it additionally emits
// a Signal, after the Setup-evaluated event. The sizing outcome that Signal
// produced — either a trade proposal or a recorded decline (see sizeUnit) —
// follows when the bar's Session closes, with every other Signal of that
// Session and after its Adds (ADR 0010, ADR 0021; session.go). It requires a
// configuration event before any bar and fails closed if a bar arrives first,
// accepts exactly one configuration event per run (matching its constructor's
// configuration hash — see applyConfiguration), and rejects a duplicate or
// out-of-order bar for any one instrument (see applyCompletedBar), and a bar
// outside the open Session (see admitToSession).
//
// N and the Entry Channel are computed from the split-adjusted price view
// only (ADR 0004): signal computation must never see raw prices, so a
// Campaign's entries and exits stay consistent with the levels a live
// system would have seen on the day.
//
// Every input to a bar's decision — N, the Entry Channel, and so the Setup's
// Tier, the Signal, the Unit size and the Protective Stop intent — is read
// from the state left by the bars BEFORE it, and the bar is folded into that
// state only afterwards. CONTEXT.md, under "Completed bar": the decision bar
// is never an input to its own decision. See applyCompletedBar's evaluate and
// advance blocks.
//
// This reducer holds mutable per-instrument state (docs/architecture.md
// notes a Handler applies events to "deterministic domain state"); it is not
// safe for concurrent use, matching replay.Engine's sequential Run.
type Reducer struct {
	strategyVersion   string
	configurationHash string

	configured bool
	// streamEnded records that event.RunCompletedEventType has been applied.
	// A run ends once, and an input arriving after it contradicts the fact
	// that event states, so Apply fails closed rather than absorbing it (see
	// applyRunCompleted).
	streamEnded bool
	// runStopped records that event.AdapterRunStoppedEventType has been
	// applied: the adapter deliberately stopped this run (ADR 0012). Only
	// event.RunCompletedEventType may follow it (see Apply), so a stop that
	// some further bar or fill then contradicted — market data arriving
	// after the run claimed to have deliberately ended — cannot be recorded.
	runStopped         bool
	entryChannelLength int
	// exitChannelLength is event.ConfigurationPayload.ExitChannelLength
	// (20 in the Baseline, The Turtle Rules p.26, ADR 0002), captured once
	// from the configuration event alongside entryChannelLength.
	exitChannelLength int
	tierBDistanceInN  float64

	// Sizing configuration, captured once from the configuration event.
	//
	// Both forms of the Sizing Mode are kept: configuredSizingMode is the
	// value the configuration event declared and is what a proposal is
	// stamped with; sizingMode is the same choice in internal/sizing's own
	// vocabulary and is what the arithmetic is called with. Keeping both
	// means the value written to the journal is the one that was configured,
	// never a string conversion back from the arithmetic package — such a
	// conversion would quietly launder a drift between the two enumerations
	// into an audit record instead of failing on it.
	configuredSizingMode event.SizingMode
	sizingMode           sizing.Mode
	unitVolatilityFrac   float64
	stopMultiple         float64
	riskAtStopFraction   float64
	dollarsPerPoint      float64
	// maxUnits is event.ConfigurationPayload.MaxUnits (ADR
	// 0008: 4 Units per instrument in the Baseline), captured once from the
	// configuration event and frozen onto every Campaign it opens
	// (campaignState.maxUnits) — a later reconfiguration (unsupported
	// mid-run in any case, see applyConfiguration) must never change how
	// many Units an already-open Campaign may hold.
	maxUnits int
	// maxUnitsPerIndustry, maxUnitsPerSector and maxUnitsTotalLong are ADR
	// 0008's other three Unit caps (event.ConfigurationPayload's own fields
	// of the same names), captured once alongside maxUnits. Unlike maxUnits
	// they are never frozen onto a Campaign: a group or total-long cap is
	// checked against every OPEN Campaign together (unit_caps.go's
	// capExceeded), not against one Campaign's own state, so there is
	// nothing campaign-shaped to freeze — only maxUnits and a Campaign's own
	// frozen classification (openCampaign) matter to what unit_caps.go reads
	// from a Campaign already open.
	maxUnitsPerIndustry int
	maxUnitsPerSector   int
	maxUnitsTotalLong   int
	// buyOrderType, gapBufferN, slippageN and commission are what a hold is
	// computed from (hold.go; ADR 0005 and ADR 0020, as amended 2026-09-24):
	// the order an entry or Add rests as, its price cap's distance above the
	// level in N, and ADR 0013's slippage and commission, captured once from
	// the configuration event.
	buyOrderType event.OrderType
	gapBufferN   float64
	slippageN    float64
	commission   sizing.CommissionSchedule
	// notionalAccount is ADR 0007's Notional Account (CONTEXT.md),
	// initialised to the configured starting equity and driven by
	// event.AccountSnapshotEventType and event.CashMovementEventType events
	// (see notional.go's applyAccountSnapshot/applyCashMovement).
	notionalAccount *NotionalAccount
	// drawdownStepsSeen counts every Drawdown Step applied since the ladder
	// was last reset — by a re-basing or a full recovery — for
	// DrawdownStepAppliedPayload.StepNumber (1-based).
	drawdownStepsSeen int
	// lastAccountEventAt/hasAccountEvent enforce chronology across the
	// WHOLE shared account timeline (ADR 0007 rule 4): both account
	// snapshots and cash movements share this one per-account clock, the
	// same shape as instrumentState's lastPeriodEnd/bar chronology check in
	// applyCompletedBar.
	lastAccountEventAt time.Time
	hasAccountEvent    bool
	// accountCurrency is pinned from the Currency of the first account
	// snapshot or cash movement accepted, and every later account event of
	// either type must match it exactly. A multi-currency account is out of
	// scope for this project: without this check, a later event stated in a different
	// currency would be silently scaled and compared against figures stated
	// in the pinned one. Empty until the first account event is accepted;
	// AccountSnapshotPayload.Validate/CashMovementPayload.Validate already
	// require Currency non-empty, so the empty string is unambiguous as
	// "not yet pinned".
	accountCurrency string
	// availableCash is ADR 0020's basis: the snapshot's available cash less
	// accepted withdrawals, floored at zero (ADR 0020's cash-movement
	// amendment). Deposits do not credit it; a later snapshot replaces it
	// outright. Both sizeUnit and evaluateAdd read it, less fillDebits and
	// holds, through cashAtPreviousClose.
	//
	// availableCashAsOf is the snapshot timestamp, unchanged by movements;
	// hasAvailableCash remains false until the first snapshot is accepted.
	// cashAtPreviousClose fails closed on an absent snapshot or one stamped
	// later than the decision bar's previous close, so current-bar credits
	// cannot fund that bar's decisions (ADR 0010).
	availableCash     float64
	availableCashAsOf time.Time
	hasAvailableCash  bool
	// fillDebits holds the actual cost of every entry and Add fill the
	// basis cannot yet reflect, in recorded order: ADR 0020's "available =
	// basis - every actual fill cost". A snapshot drops the ones it can
	// reflect (applyAccountSnapshot). Reducer.begin copies it, since it is
	// small: a snapshot empties it of everything up to its own as-of.
	fillDebits []fillDebit
	// holds holds every standing reservation, one per outstanding entry or
	// Add proposal, in placement order (hold.go; ADR 0020, as amended
	// 2026-09-24: "available = basis - fill debits - holds"). A proposal's
	// fill, expiry or cancellation releases it; a snapshot never does.
	// Reducer.begin copies it, since it is small: it holds at most one hold
	// per instrument with a proposal outstanding.
	holds []hold

	instruments map[string]*instrumentState
	// delisted records, per instrument, the EffectiveAt of the delisting that
	// ended its life in this run (ADR 0009). It is the memory that makes
	// delisting.go's named invariant true rather than merely stated: a
	// delisted instrument cannot un-delist and cannot be traded again in this
	// run. Every entry is terminal — nothing ever removes one.
	//
	// It is held here rather than on instrumentState because a delisting
	// legitimately names an instrument this reducer has no state for at all
	// (see applyDelisting), and fabricating indicator state for one would make
	// the reducer look as though it had evaluated an instrument it never saw.
	//
	// Three places read it, and two of them are the capital-safety guards:
	// applyCompletedBar refuses to evaluate a delisted instrument as a Setup,
	// applyFill refuses an execution naming one, and applyDelisting itself
	// treats a repeated notice as stating no new fact.
	delisted map[string]time.Time
	// classifications records, per instrument, every declared
	// event.MarketInstrumentClassificationEventType fact (ADR 0009; ADR
	// 0021's own market.corporate-action fact is the precedent this
	// mirrors): a producer's translation of internal/universe.Port's answer
	// into a journalled, replayable record, so that ADR 0009's own
	// Consequences — "membership changes only through declared criteria on
	// declared dates, so it can be reproduced exactly on replay" — hold for
	// the classification input as well as for the price/volume criteria the
	// reducer already derives from bars. Held here rather than on
	// instrumentState because a classification legitimately arrives for an
	// instrument this reducer has no bar for yet (evaluateUniverse's own doc
	// comment).
	//
	// Each classificationRecord distinguishes the declaration currently in
	// force (active) from any later one still waiting for its own
	// EffectiveAt (pending) — a fact effective in the future must never
	// decide an earlier Session's eligibility (ADR 0009 is point-in-time).
	//
	// An instrument absent from this map, or present with no active
	// declaration yet (every one of its own still pending), has never been
	// classified for gating purposes. While the universe gate is off
	// (universeEnabled false — every existing fixture, golden journal and
	// decision-corpus scenario), that and every other instrument is left
	// completely ungated regardless. While the gate is on, such an
	// instrument, or one classified but present with no completed
	// evaluation yet (instrumentState.universe.evaluated false), is declined
	// ineligible for a NEW Campaign — it is not a candidate merely by
	// default (universeIneligible's own doc comment; ADR 0009's amendment of
	// 2026-09-25, the owner's decision).
	classifications map[string]classificationRecord
	// universeCriteria is ADR 0009's three thresholds
	// (event.ConfigurationPayload.UniverseMinPrice/UniverseMinDollarVolume/
	// UniverseMinHistoryBars), captured once from the configuration event.
	// universeEnabled is whether they are all positive (the gate on) rather
	// than all zero (off) — ConfigurationPayload.Validate has already
	// refused any other combination, so reading one field's sign is
	// sufficient, but the derivation lives in one named place
	// (universeGateOn, universe.go) rather than being re-read inline at
	// every call site.
	universeCriteria universe.Criteria
	// acceptedFills is defined and explained in
	// campaign.go: every fill this reducer has accepted, for the WHOLE
	// run, keyed by FillID — not per instrument — so that a fill id reused
	// across two different instruments is caught as a reconciliation
	// failure rather than accepted twice (each instrument's history no
	// longer being kept separately), and so a re-delivery stays idempotent
	// no matter how much has happened since it was first accepted.
	acceptedFills map[string]acceptedFillState

	// The Session (CONTEXT.md: "Session"; ADR 0021). sessionOpen is true
	// from a Session's first bar until its market.session.closed, and
	// sessionPeriodEnd is that Session's period end. lastClosedSession is
	// the period end of the last Session closed, valid once hasClosedSession
	// is true: Sessions follow one another strictly, so no bar may arrive
	// for it or any earlier period end. sessionDelistedBars names the open
	// Session's bars for delisted instruments, which decide nothing but are
	// still bars the close must name.
	sessionOpen         bool
	sessionPeriodEnd    time.Time
	hasClosedSession    bool
	lastClosedSession   time.Time
	sessionDelistedBars []string
}

// instrumentState is one instrument's running True Range/N/Entry Channel
// state. previousClose and hasPreviousClose together let TrueRange compute
// the gap terms once a prior bar exists, and are left at their zero values
// for the first bar of a given instrument, per the choice documented on
// indicator.TrueRange. lastPeriodEnd is the PeriodEnd of the last bar
// accepted for this instrument, and is the zero time.Time before any bar has
// been seen (CompletedBarPayload.Validate already rejects a zero PeriodEnd,
// so the zero value is unambiguous as "no bar yet").
type instrumentState struct {
	previousClose    float64
	hasPreviousClose bool
	lastPeriodEnd    time.Time
	n                *indicator.WilderAverage
	entryChannel     *indicator.EntryChannel
	// exitChannel is fed one more completed bar's low every
	// completed bar, whether or not the instrument is currently in a
	// Campaign — so the window is already warm the moment a Campaign opens
	// (see reducer.go's applyCompletedBar). Evaluate-then-add, exactly like
	// entryChannel and n.
	exitChannel *indicator.ExitChannel

	// splitAdjustedCloses, rawCloses and rawVolumes are the bounded history
	// the ranking seam reads (session.go: rankSignal; CONTEXT.md: "Strength";
	// ADR 0010, as amended by the owner's decision of 2026-09-25).
	// splitAdjustedCloses holds the last indicator.StrengthLookbackBars+1
	// split-adjusted closes (ADR 0004) Strength divides by N(d) from;
	// rawCloses and rawVolumes hold the last indicator.DollarVolumeWindow raw
	// closes and volumes the median dollar volume tie-break reads — the same
	// definition ADR 0009's eligibility test will share. All three are fed
	// once per completed bar, in applyCompletedBar's advance block, alongside
	// previousClose: unlike N and the channels above, ranking runs only once
	// the Session has fully closed (session.go), so this bar's own close and
	// volume are already completed facts by then and need no evaluate-before-
	// advance discipline.
	splitAdjustedCloses *indicator.RollingWindow
	rawCloses           *indicator.RollingWindow
	rawVolumes          *indicator.RollingWindow
	// completedBars counts every completed bar this reducer has accepted for
	// the instrument, without bound (unlike the bounded windows above): ADR
	// 0009's history criterion reads the total, not a recent window. Fed
	// unconditionally in applyCompletedBar's advance block, alongside the
	// three fields above.
	completedBars int
	// universe is this instrument's most recently recorded ADR 0009
	// eligibility verdict (session.go: evaluateUniverse), evaluated once per
	// calendar month at the Session close for the first trading day of that
	// month. evaluated is false until the instrument has both a declared
	// classification (Reducer.classifications) and at least one monthly
	// evaluation; sizeUnit's eligibility gate (universeIneligible) applies no
	// restriction at all while it is false — see Reducer.classifications'
	// own doc comment for why an unclassified instrument is left ungated
	// rather than asserted ineligible.
	universe universeState

	// Two additions, both defined and explained in campaign.go.
	// pendingProposal is a trade proposal emitted and not yet resolved, and is
	// deliberately NOT position state: no Campaign is ever derived from it
	// alone. campaign is the instrument's open Campaign, and is nil unless a
	// recorded fill brought one into being.
	pendingProposal *pendingProposalState
	campaign        *campaignState
	// pendingSignal is this Session's Signal, awaiting the Session's close to
	// be sized (session.go), and addDue records that this Session's bar
	// reached the open Campaign's next Add rung without proposing an exit.
	// Both are set at the bar and consumed at the close: ADR 0010 decides a
	// day's Adds and entries only after every exit, across instruments
	// (ADR 0021).
	pendingSignal *pendingSignalState
	addDue        bool
	// pendingExitProposal is defined and explained in
	// campaign.go: an exit proposal (strategy.exit.proposed) emitted and not
	// yet resolved, holding the same "not position state" property
	// pendingProposal does — a Campaign closes only from a recorded exit
	// fill, never from this proposal alone.
	pendingExitProposal *pendingExitProposalState
	// pendingAddProposal is defined and explained in
	// campaign.go: an Add proposal (strategy.add.proposed) emitted and not
	// yet resolved, holding the same "not position state" property
	// pendingProposal/pendingExitProposal do — a further Unit joins the
	// Campaign only from a recorded Add fill, never from this proposal
	// alone.
	pendingAddProposal *pendingAddProposalState
	// lastBarHigh/lastBarPeriodEnd/lastBarEarliestFillAt are memory of
	// the most recently completed bar, set unconditionally at the end of
	// every applyCompletedBar call regardless of Campaign state. They exist
	// because the same-bar Add chain (see campaign.go's evaluateAdd and
	// applyAddFill) re-evaluates the Add opportunity from an ADD FILL's own
	// Apply call — a LATER call than the bar event that produced the
	// opportunity for it — so the bar's high, its own period end (what the
	// resulting proposal is attributed to), and the earliest instant an
	// execution for it could exist all have to outlive the single Apply call
	// that read the bar itself.
	lastBarHigh           float64
	lastBarPeriodEnd      time.Time
	lastBarEarliestFillAt time.Time
	// lastClosingFillAt is the FilledAt of the most recent fill that closed a
	// Campaign for this instrument — a stop fill or an exit fill alike — and
	// is the zero time.Time before any Campaign for this instrument has ever
	// closed (every fill's FilledAt is required non-zero, so the zero value
	// is unambiguous as "never"). It is never cleared once set, including
	// across a later Campaign's whole life: see
	// checkBarConfirmsCampaignClosing (campaign.go) for why that is safe.
	lastClosingFillAt time.Time
	// lastSplitAt is the EffectiveAt of the last split applied to this
	// instrument, and the zero time before any: each split applies once, in
	// order, so a redelivered one can never reduce a Unit twice (split.go;
	// ADR 0023).
	lastSplitAt time.Time
}

// NewReducer returns a Reducer that stamps every decision it emits with
// Source "reducer", the given strategyVersion, and a configurationHash
// derived from payload (event.ConfigurationHash; ADR 0016) — the single
// place in this codebase a configuration hash is computed, so a caller can
// never construct a Reducer whose stored hash disagrees with what
// event.ConfigurationHash would compute for the same payload.
//
// strategyVersion remains an opaque string supplied by the caller: composing
// it from the configuration's StrategyID, this project's declared rules
// version, and the running build (ADR 0016) is the caller's job, via
// event.ComposeStrategyVersion and strategy.RulesVersion, not something this
// constructor does — the strategy version and the configuration hash are
// independent provenance axes (ADR 0015 draws the same line between the
// envelope shape and the strategy version), and only the hash has exactly
// one payload to derive itself from.
//
// payload is validated here (ConfigurationPayload.Validate), so an invalid
// configuration can never produce a hash at all, rather than surfacing only
// later when the matching configuration event arrives in Apply.
func NewReducer(strategyVersion string, payload event.ConfigurationPayload) (*Reducer, error) {
	if strategyVersion == "" {
		return nil, errors.New("strategy: strategy version is required")
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: invalid configuration payload: %w", err)
	}
	return &Reducer{
		strategyVersion:   strategyVersion,
		configurationHash: event.ConfigurationHash(payload),
		instruments:       make(map[string]*instrumentState),
		delisted:          make(map[string]time.Time),
		classifications:   make(map[string]classificationRecord),
		acceptedFills:     make(map[string]acceptedFillState),
	}, nil
}

// Apply implements replay.Handler. It recognises the following input event
// types:
//
//   - event.ConfigurationEventType: recorded, and required before any bar or
//     account snapshot.
//
//   - event.CompletedBarEventType: updates that instrument's True Range/N
//     and emits one event.SetupEvaluatedEventType decision.
//
//   - event.SessionClosedEventType: ends the open Session and decides its
//     Adds, then its entries, across instruments (ADR 0021) — see
//     session.go's applySessionClosed.
//
//   - event.FillEventType: the only input that may change position state
//     (see campaign.go).
//
//   - event.AccountSnapshotEventType: feeds actual equity to the Notional
//     Account (ADR 0007: re-basing, the Drawdown Step ladder, and recovery,
//     in that order) — see notional.go's applyAccountSnapshot.
//
//   - event.CashMovementEventType: scales the Notional Account for a
//     deposit or withdrawal (ADR 0007) — see notional.go's
//     applyCashMovement.
//
//   - event.MarketCorporateActionEventType: a fact about an instrument's own
//     listing, external to any decision this system made — today, only a
//     Delisting Exit (CONTEXT.md; ADR 0009), which forces an open Campaign
//     closed at the last available price — see delisting.go's
//     applyCorporateAction.
//
//   - event.MarketInstrumentClassificationEventType: a declared fact about
//     an instrument's own listing (ADR 0009), read at each monthly universe
//     evaluation — see universe.go's applyInstrumentClassification.
//
//   - event.AdapterRunStoppedEventType: record, no decision — an adapter's
//     own report that it deliberately stopped this run (ADR 0012), the same
//     rule applyConfiguration follows for the event that opens one. Only
//     event.RunCompletedEventType may follow it (checked below) — see
//     applyAdapterRunStopped.
//
//   - event.OrderLifecycleEventType: record, no decision — a venue's report
//     that an order was acknowledged, amended, cancelled or refused, which
//     moves no position (docs/architecture.md) and is journalled for
//     reconciliation (ADR 0019) — see order_lifecycle.go's
//     applyOrderLifecycle.
//
//   - event.RunCompletedEventType: expires outstanding proposals across the
//     whole universe and records resulting Exit Order changes (ADR 0011).
//
// Any other event type fails closed rather than being silently ignored
// (docs/development.md principle 4: "Fail closed on unknown schemas").
func (r *Reducer) Apply(_ context.Context, envelope event.Envelope) ([]event.Envelope, error) {
	return r.transact(func(tx *transition) ([]event.Envelope, error) {
		return tx.apply(envelope)
	})
}

func (r *transition) apply(envelope event.Envelope) ([]event.Envelope, error) {
	if r.streamEnded {
		return nil, fmt.Errorf("strategy: the input stream has already ended, so %q at sequence %d cannot exist; a run ends once (see applyRunCompleted)", envelope.Type, envelope.Sequence)
	}
	if r.runStopped && envelope.Type != event.RunCompletedEventType {
		return nil, fmt.Errorf("strategy: the run was deliberately stopped, so %q at sequence %d cannot exist; only %s may follow a stop (see applyAdapterRunStopped)", envelope.Type, envelope.Sequence, event.RunCompletedEventType)
	}
	switch envelope.Type {
	case event.ConfigurationEventType:
		return r.applyConfiguration(envelope)
	case event.CompletedBarEventType:
		return r.applyCompletedBar(envelope)
	case event.SessionClosedEventType:
		return r.applySessionClosed(envelope)
	case event.FillEventType:
		return r.applyFill(envelope)
	case event.AccountSnapshotEventType:
		return r.applyAccountSnapshot(envelope)
	case event.CashMovementEventType:
		return r.applyCashMovement(envelope)
	case event.MarketCorporateActionEventType:
		return r.applyCorporateAction(envelope)
	case event.MarketInstrumentClassificationEventType:
		return r.applyInstrumentClassification(envelope)
	case event.RunCompletedEventType:
		return r.applyRunCompleted(envelope)
	case event.AdapterRunStoppedEventType:
		return r.applyAdapterRunStopped(envelope)
	case event.OrderLifecycleEventType:
		return r.applyOrderLifecycle(envelope)
	default:
		return nil, fmt.Errorf("strategy: unrecognized event type %q", envelope.Type)
	}
}

// applyConfiguration handles event.ConfigurationEventType.
//
// Three checks guard the provenance the reducer stamps on every later
// decision (Source, StrategyVersion, ConfigurationHash), and the shape of
// the payload itself:
//
//   - The envelope's SchemaVersion must equal event.ConfigurationSchemaVersion.
//     This is the payload-level counterpart of ADR 0015's envelope-level
//     rule: an older schema is rejected, never silently upgraded, until an
//     explicit upcaster exists. Without this check, a schema-1 configuration
//     (recorded before TierBDistanceInN existed) would still decode: the
//     missing field unmarshals as the float64 zero value, ConfigurationPayload.Validate
//     accepts a zero TierBDistanceInN as legitimately "no Tier B window", and
//     Tier B silently changes meaning for that run rather than the run being
//     rejected as incompatible.
//   - The event's ConfigurationHash must match the hash this Reducer was
//     constructed with. Without this check a valid configuration envelope
//     carrying a *different* hash would still be accepted, and every
//     decision emitted afterwards would be stamped with the constructor's
//     hash rather than the one the configuration event actually declared —
//     an audit record that attributes a decision to the wrong configuration.
//   - A reducer accepts exactly one configuration event per run. ADR 0006
//     freezes a Campaign's N and Unit size at first entry precisely because
//     mid-stream reconfiguration is not a supported concept here; a second
//     configuration event (even one with a matching hash) is rejected
//     rather than silently re-applied.
func (r *transition) applyConfiguration(envelope event.Envelope) ([]event.Envelope, error) {
	if r.configured {
		return nil, fmt.Errorf("strategy: reducer is already configured (configuration hash %q); mid-stream reconfiguration is not supported (ADR 0006 freezes configuration at entry)", r.configurationHash)
	}
	if envelope.SchemaVersion != event.ConfigurationSchemaVersion {
		return nil, fmt.Errorf("strategy: configuration payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.ConfigurationSchemaVersion)
	}
	if envelope.ConfigurationHash != r.configurationHash {
		return nil, fmt.Errorf("strategy: configuration event's configuration hash %q does not match the reducer's configuration hash %q", envelope.ConfigurationHash, r.configurationHash)
	}
	var payload event.ConfigurationPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return nil, fmt.Errorf("strategy: decode configuration payload: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: invalid configuration payload: %w", err)
	}
	sizingMode, err := sizingModeFor(payload.SizingMode)
	if err != nil {
		return nil, err
	}
	r.entryChannelLength = payload.EntryChannelLength
	r.exitChannelLength = payload.ExitChannelLength
	r.tierBDistanceInN = payload.TierBDistanceInN
	r.configuredSizingMode = payload.SizingMode
	r.sizingMode = sizingMode
	r.unitVolatilityFrac = payload.UnitVolatilityFraction
	r.stopMultiple = payload.StopMultiple
	r.riskAtStopFraction = payload.RiskAtStopFraction
	r.dollarsPerPoint = payload.DollarsPerPoint
	r.maxUnits = payload.MaxUnits
	r.maxUnitsPerIndustry = payload.MaxUnitsPerIndustry
	r.maxUnitsPerSector = payload.MaxUnitsPerSector
	r.maxUnitsTotalLong = payload.MaxUnitsTotalLong
	r.buyOrderType = payload.BuyOrderType
	r.gapBufferN = payload.GapBufferN
	r.universeCriteria = universe.Criteria{
		MinPrice:        payload.UniverseMinPrice,
		MinDollarVolume: payload.UniverseMinDollarVolume,
		MinHistoryBars:  payload.UniverseMinHistoryBars,
	}
	r.slippageN = payload.SlippageN
	r.commission = sizing.CommissionSchedule{
		PerShare:                    payload.Commission.PerShare,
		MinimumPerOrder:             payload.Commission.MinimumPerOrder,
		MaximumFractionOfTradeValue: payload.Commission.MaximumFractionOfTradeValue,
	}
	// ADR 0007's Notional Account, at its configured starting value: before
	// any account.snapshot arrives it equals StartingEquity exactly
	// (applyAccountSnapshot, in notional.go, is what steps it down;
	// applyAccountSnapshot/applyCashMovement re-base, recover, and scale it).
	notionalAccount, err := NewNotionalAccount(payload.NotionalAccount.StartingEquity, payload.NotionalAccount.RebasingMonth, payload.NotionalAccount.RebasingDay)
	if err != nil {
		// Unreachable: ConfigurationPayload.Validate has already required
		// StartingEquity to be finite and positive and the rebasing
		// month/day to form a date that recurs every year, which is
		// everything NewNotionalAccount checks. Guarded anyway, matching
		// this project's fail-closed style.
		return nil, fmt.Errorf("strategy: %w", err)
	}
	r.notionalAccount = notionalAccount
	r.configured = true
	return nil, nil
}

// sizingModeFor maps the configuration contract's Sizing Mode onto
// internal/sizing's own.
//
// internal/sizing declares its own Mode rather than importing internal/event,
// so that the arithmetic stays as free of the wire contract as
// internal/indicator is. That leaves exactly one place where the two
// enumerations meet — here — and it fails closed rather than defaulting, so a
// mode this build does not know about can never be silently sized as if it
// were the Baseline. The two enumerations' string values are pinned equal by
// TestSizingModeConstantsMatchTheEventContract.
//
// ConfigurationPayload.Validate has already rejected any unrecognised mode by
// the time this is reached, so the default branch is unreachable in practice;
// it exists so that adding a third mode to the contract without extending
// this mapping fails closed instead of falling through.
func sizingModeFor(mode event.SizingMode) (sizing.Mode, error) {
	switch mode {
	case event.SizingModeVolatilityNormalised:
		return sizing.ModeVolatilityNormalised, nil
	case event.SizingModeFixedRiskAtStop:
		return sizing.ModeFixedRiskAtStop, nil
	default:
		return "", fmt.Errorf("strategy: sizing mode %q is not a recognised sizing mode; failing closed rather than sizing a position under an unknown principle (ADR 0003)", mode)
	}
}

// applyCompletedBar handles event.CompletedBarEventType. Like
// applyConfiguration, it rejects a schema version other than
// event.CompletedBarSchemaVersion before decoding, for the same reason (ADR
// 0015's envelope-level rule, applied here at the payload level): a schema
// this build was not written against must never be silently interpreted as
// the current one.
func (r *transition) applyCompletedBar(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received a completed bar before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.CompletedBarSchemaVersion {
		return nil, fmt.Errorf("strategy: completed bar payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.CompletedBarSchemaVersion)
	}

	var bar event.CompletedBarPayload
	if err := json.Unmarshal(envelope.Payload, &bar); err != nil {
		return nil, fmt.Errorf("strategy: decode completed bar payload: %w", err)
	}
	if err := bar.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: invalid completed bar payload: %w", err)
	}

	// A bar for a delisted instrument decides nothing (ADR 0009: a delisting
	// ends the instrument's life in this run — see r.delisted). CONTEXT.md
	// defines a Setup as an ELIGIBLE instrument not in a Campaign, and a
	// delisted instrument is not eligible, so evaluating one here would
	// journal a claim that is false by the project's own vocabulary — the
	// identical reason no Setup-evaluated event is emitted for an instrument
	// already in a Campaign, below. Read before stateFor, so a delisting for
	// an instrument this reducer never saw does not acquire indicator state
	// from the bars that follow it.
	//
	// Absorbed rather than failed closed, which is this package's default and
	// is what applyFill does with a fill for the same instrument. The
	// asymmetry is deliberate and rests on what each input is. A delisting
	// notice is a fact about a listing, and it may legitimately name an
	// instrument this strategy has no stake in whatsoever; making a later bar
	// for that instrument halt the run would turn applyDelisting's no-op into
	// a trap, failing a whole multi-instrument run over a name it never
	// traded. A fill is a report that money moved, which is never absorbable.
	// Nothing is hidden either way: the corporate action is itself an input
	// envelope in the journal, so a reader can see why the instrument fell
	// silent.
	if err := r.admitToSession(bar); err != nil {
		return nil, err
	}
	if _, delisted := r.delisted[bar.InstrumentID]; delisted {
		return nil, r.admitDelistedBar(bar)
	}

	state, err := r.stateFor(bar.InstrumentID)
	if err != nil {
		return nil, err
	}

	// Bar chronology, per instrument. A duplicate or out-of-order bar would
	// silently advance the True Range/N accumulator and overwrite
	// previousClose, corrupting every later gap calculation for this
	// instrument — this is not only the upstream journal's job to prevent:
	// the reducer is the last line before the arithmetic that sizes real
	// positions, so it must not trust that what it was handed is
	// chronological. Chronology is per instrument, not global: the engine's
	// input Sequence, not PeriodEnd, is what orders the stream across
	// different instruments (docs/architecture.md).
	if !state.lastPeriodEnd.IsZero() && !bar.PeriodEnd.After(state.lastPeriodEnd) {
		return nil, fmt.Errorf("strategy: instrument %q: bar period end %s is not strictly after the last recorded period end %s; rejecting a duplicate or out-of-order bar",
			bar.InstrumentID, bar.PeriodEnd.Format(time.RFC3339), state.lastPeriodEnd.Format(time.RFC3339))
	}

	// This bar is the first thing able to contradict an open Campaign's
	// opening fill timestamp, or the instrument's
	// most recent CLOSING fill timestamp (a stop or an exit alike) — see
	// checkBarConfirmsCampaignOpening and checkBarConfirmsCampaignClosing for
	// why each check lives at this end rather than in applyFill/applyStopFill/
	// applyExitFill. Before any state is read or advanced, so a rejected bar
	// leaves nothing half-applied.
	if err := checkBarConfirmsCampaignOpening(state, bar); err != nil {
		return nil, err
	}
	if err := checkBarConfirmsCampaignClosing(state, bar); err != nil {
		return nil, err
	}

	// The capital-safety invariant — every open Campaign has a
	// Protective Stop at all times — checked at the start of every
	// completed bar, before anything else about this bar is read. See
	// checkCampaignHasAProtectiveStop's doc comment for why a violation can
	// only be memory corruption, not a bad input, and why the halt envelope
	// is returned alongside the error.
	if halt, err := r.checkCampaignHasAProtectiveStop(state, bar, envelope); err != nil {
		r.failureEmissions = []event.Envelope{halt}
		return nil, err
	}

	// ADR 0004: signal computation, including N and the Entry Channel, runs
	// on the split-adjusted view only.
	view := bar.SplitAdjusted
	tr := indicator.TrueRange(view.High, view.Low, state.previousClose, state.hasPreviousClose)

	// --- Evaluate. Every input to this bar's decision is read here, from the
	// state left by the bars BEFORE it. Nothing this bar contributes is
	// folded in until the "advance" block below.
	//
	// This is the fix for the prototype's headline look-ahead bug (see
	// indicator.EntryChannel's doc comment), applied uniformly rather than
	// only to the channel. The rule is CONTEXT.md's, under "Completed bar":
	// the decision bar is never an input
	// to its own decision. For N specifically it is also what ADR 0005
	// requires: the entry is a resting order that fills *inside* the breakout
	// bar, so the Unit size and the Protective Stop have to be computable
	// before that bar opens. A bar that updated N before being decided would
	// shrink its own Unit and tighten its own stop by having been volatile —
	// using information that did not exist when the order was placed.
	//
	// NReady means "N is a usable volatility reading", not merely "bar-count
	// warm-up complete": twenty flat bars (high==low==close) legitimately
	// warm up indicator.WilderAverage with N==0, which a volatility-normalised
	// sizing step could not safely divide by. Such an instrument is reported
	// not ready rather than erroring the run, and becomes ready again the
	// moment True Range is non-zero (see indicator.WilderAverage's doc
	// comment). decisionN is 0 whenever nReady is false, since
	// WilderAverage.Value is 0 until warm-up completes and a not-ready N past
	// warm-up is not-ready precisely because it is 0.
	decisionN := state.n.Value()
	nReady := state.n.Ready() && decisionN > 0

	// previousPeriodEnd is the period end of the bar BEFORE this one — the moment this bar
	// opened — captured here because the advance block below overwrites it. It
	// is the earliest instant at which an order proposed on this bar could
	// have executed; see transition.applyFill for the window it bounds.
	previousPeriodEnd := state.lastPeriodEnd

	entryChannelHigh, entryChannelReady := state.entryChannel.Extreme()
	// The Turtle Rules p.19: a Breakout "exceeds" the channel, so the
	// comparison is strict; a tie is not a breakout.
	breakout := entryChannelReady && view.High > entryChannelHigh

	// exitChannelLow is the Exit Channel low in force for deciding THIS bar — computed
	// from the preceding ExitChannelLength completed bars only, the same
	// evaluate-then-add discipline as N and the Entry Channel (see
	// indicator.ExitChannel's doc comment for the look-ahead rationale,
	// mirroring indicator.EntryChannel's). Read here, before either channel
	// is advanced, and used below regardless of whether this instrument is
	// currently in a Campaign — the Exit Channel is fed every completed bar
	// so it is already warm the moment a Campaign opens.
	exitChannelLow, exitChannelReady := state.exitChannel.Extreme()

	// --- Advance. Every read that decides this bar has now happened, so the
	// bar can be folded into the running state for the NEXT bar to see. The
	// per-instrument tracking itself is unchanged: this bar's True Range and
	// high/low still enter N, the Entry Channel and the Exit Channel, just
	// after the decision rather than before it.
	state.n.Add(tr)
	state.entryChannel.Add(view.High)
	state.exitChannel.Add(view.Low)
	state.previousClose = view.Close
	state.hasPreviousClose = true
	state.lastPeriodEnd = bar.PeriodEnd
	// Strength (CONTEXT.md: "Strength"; ADR 0010) and the dollar-volume
	// tie-break (ADR 0009; ADR 0010, as amended by the owner's decision of
	// 2026-09-25) both read this bar's own close and volume once they are
	// completed facts, at the Session's close (session.go: rankSignal) —
	// unlike N and the channels above, which must exclude this bar to keep
	// the resting order computable before it opens (ADR 0005). Fed here,
	// unconditionally, so the history is warm the moment ranking needs it.
	state.splitAdjustedCloses.Add(view.Close)
	state.rawCloses.Add(bar.Raw.Close)
	state.rawVolumes.Add(bar.Raw.Volume)
	// ADR 0009's history criterion: the total completed bars this reducer
	// has ever accepted for the instrument, unbounded (unlike the three
	// windows above), fed the same unconditional way.
	state.completedBars++
	// Remembered unconditionally, regardless of Campaign state, so the
	// same-bar Add chain (campaign.go's evaluateAdd/applyAddFill) can read
	// THIS bar's high, period end and earliest-fill-at bound from a LATER
	// Apply call — the Add fill's own — after this bar's own call has
	// already returned. See instrumentState's own doc comment on these
	// fields for why they must outlive a single Apply call.
	state.lastBarHigh = view.High
	state.lastBarPeriodEnd = bar.PeriodEnd
	state.lastBarEarliestFillAt = previousPeriodEnd

	// --- The previous bar's outstanding business, resolved so that it is
	// EMITTED before any decision this bar produces — the ordering ADR 0010
	// applies within a day, exits before entries. It sits below the advance
	// block rather than above the evaluate block only so that the
	// evaluate/advance pair stays contiguous; it reads and writes none of
	// that state. A trade proposal, an exit proposal or an Add proposal that
	// no fill arrived for expires with its bar, per ADR 0011; see
	// transition.expireEntryProposal, transition.expireExitProposal and
	// transition.expireAddProposal for why the expiry is emitted rather than
	// dropped. The one exception is a fill-chained Add (ADR 0011, as amended
	// 2026-09-24), which survives this bar while its Campaign is still open.
	// If this bar then proposes an exit, the surviving Add expires with it
	// below, so an exit and an Add proposal are still never outstanding
	// together (ADR 0010's exit precedence).
	var emissions []event.Envelope
	if state.pendingProposal != nil {
		expired, err := r.expireEntryProposal(state, bar, envelope)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, expired)
	}
	if state.pendingExitProposal != nil {
		expired, err := r.expireExitProposal(state, bar, envelope)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, expired)
	}
	if pending := state.pendingAddProposal; pending != nil && pending.survivesNextBar && state.campaign != nil {
		// ADR 0011's fill-chain extension counts actual instrument bars,
		// keeping the proposal and its ADR 0020 hold through the first. It
		// never outlives its Campaign: a stop fill that closes the Campaign
		// cancels the Add at once (applyStopFill), and this check keeps the
		// extension from applying to a closed Campaign regardless.
		pending.survivesNextBar = false
	} else if state.pendingAddProposal != nil {
		expired, err := r.expireAddProposal(state, bar, envelope)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, expired)
	}

	// --- While a Campaign is open, no new entry is evaluated (below):
	// N and the Entry Channel above are still tracked, so both are already
	// warm for the very next bar once the Campaign closes and the instrument
	// is a Setup again, but no Setup-evaluated/Signal/proposal path runs:
	// CONTEXT.md defines a Setup as an Eligible instrument NOT in a Campaign,
	// so emitting a Setup-evaluated event here would journal a claim that is
	// false by the project's own vocabulary. Instead, evaluateCampaign runs
	// the Campaign's own per-bar decision: the Protective Stop and Exit
	// Channel levels in force, and — on a breach — the exit proposal.
	//
	// # The ADR 0010 ordering hook
	//
	// evaluateCampaign runs at the bar, so "exits are evaluated and
	// journaled before Adds" (ADR 0010) holds by construction: the Add is
	// decided only when the bar's Session closes, after every instrument's
	// exits (ADR 0021). It is marked due only when this bar did not itself
	// propose an exit — evaluateCampaign clears state.pendingExitProposal
	// before it runs (the top-of-function expiry block above) and sets it
	// again only if THIS bar breaches the Exit Channel, so checking it here
	// after the call is exactly "did this bar propose an exit", with no
	// separate return value needed: a bar that would both Add and exit
	// results in the exit only.
	if state.campaign != nil {
		campaignEmissions, err := r.evaluateCampaign(state, bar, exitChannelLow, exitChannelReady, previousPeriodEnd, envelope)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, campaignEmissions...)

		// A bar that would both Add and exit results in the exit only (ADR
		// 0010): a fill-chained Add that survived the expiry block above
		// expires with the bar that proposes the exit, and its hold is
		// released (ADR 0011 and ADR 0020, as amended 2026-09-24).
		if state.pendingAddProposal != nil && state.pendingExitProposal != nil {
			expired, err := r.expireAddProposal(state, bar, envelope)
			if err != nil {
				return nil, err
			}
			emissions = append(emissions, expired)
		}

		// Every held Unit's Exit Order moved by this bar — by the exit
		// proposal it raised, or by the expiry of the previous bar's — after
		// the Campaign's own evaluation and before the Add (exit_order.go).
		exitOrderEmissions, err := r.emitExitOrderChanges(state, bar.PeriodEnd, "bar", envelope)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, exitOrderEmissions...)

		// The Add itself is decided when the Session closes, after every
		// instrument's exits (ADR 0010, ADR 0021; session.go). Marked due
		// whenever no exit was proposed, regardless of the Campaign's
		// current Unit count: a Campaign already at (or, via a shared
		// group or total-long cap, effectively at) its cap still needs
		// evaluateAdd to run so a reached rung is DECLINED and journalled
		// (ADR 0008), rather than silently never being considered again.
		// evaluateAdd itself is what decides whether a reached rung
		// produces a proposal or a cap decline (campaign.go).
		state.addDue = state.pendingExitProposal == nil

		return emissions, nil
	}

	// Tier follows three cases, per DistanceToEntryInN's sign and magnitude
	// (see SetupEvaluatedPayload's doc comment): a strict breakout
	// (distance < 0) is TierA; a tie or an approach within the configured
	// distance (0 <= distance <= TierBDistanceInN) is TierB — since
	// TierBDistanceInN is always non-negative (ConfigurationPayload.Validate),
	// a tie (distance exactly 0) is always at least TierB, never TierNone,
	// so ADR 0011's Watchlist surfaces it as the closest possible approach
	// without a breakout; anything farther is TierNone.
	ready := nReady && entryChannelReady
	tier := event.TierNone
	var distanceToEntryInN float64
	if ready {
		distanceToEntryInN = (entryChannelHigh - view.High) / decisionN
		switch {
		case breakout:
			tier = event.TierA
		case distanceToEntryInN >= 0 && distanceToEntryInN <= r.tierBDistanceInN:
			tier = event.TierB
		}
	}

	reportedEntryChannelHigh := entryChannelHigh
	if !entryChannelReady {
		// Matches N's convention (indicator.WilderAverage: zero while not
		// ready): a channel high computed from fewer than
		// EntryChannelLength bars is not a real Entry Channel level and
		// must never be read as one.
		reportedEntryChannelHigh = 0
	}

	decisionPayload := event.SetupEvaluatedPayload{
		InstrumentID:       bar.InstrumentID,
		PeriodEnd:          bar.PeriodEnd,
		N:                  decisionN,
		NReady:             nReady,
		EntryChannelHigh:   reportedEntryChannelHigh,
		EntryChannelReady:  entryChannelReady,
		Tier:               tier,
		DistanceToEntryInN: distanceToEntryInN,
	}
	if err := decisionPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: built invalid setup evaluated payload: %w", err)
	}
	payloadBytes, err := json.Marshal(decisionPayload)
	if err != nil {
		return nil, fmt.Errorf("strategy: marshal setup evaluated payload: %w", err)
	}

	// One Setup-evaluated event exists per (instrument, completed bar), so
	// that pair identifies it uniquely — see stamp for why the ID is built
	// this way rather than generated.
	decision := r.stamp(
		decisionID("setup-evaluated", bar.InstrumentID, bar.PeriodEnd),
		event.SetupEvaluatedEventType, event.SetupEvaluatedSchemaVersion,
		bar.PeriodEnd, envelope, payloadBytes,
	)
	emissions = append(emissions, decision)

	// Tier A is a Signal. No Signal while N or the channel is not ready
	// (tier is TierA only when ready is true, above), and never on a tie
	// (a tie is TierB, not TierA — breakout, and therefore tier==TierA,
	// requires a strict >). Emitted after the Setup-evaluated event.
	if tier == event.TierA {
		signalPayload := event.SignalPayload{
			InstrumentID: bar.InstrumentID,
			PeriodEnd:    bar.PeriodEnd,
			// Rule names the mechanism (a breakout above the preceding
			// EntryChannelLength bars' high), not one of Faith's system
			// labels: System 1 is a 20-day channel and System 2 a 55-day
			// one (ADR 0002), so a name tied to "System 2" would misdescribe
			// a Variant configured with EntryChannelLength 20 — that is
			// System 1's channel, not System 2's. ADR names the rule's
			// defining ADR (0002 defines the breakout rule and selects 55
			// for the Baseline), not "the Baseline" or "the Variant
			// currently running": whether this run IS the Baseline or a
			// declared Variant is what the envelope's ConfigurationHash and
			// StrategyVersion establish (ADR 0012), never this field. The
			// length this Signal was actually computed with, not a
			// hard-coded 55, is EntryChannelLength below.
			Rule:               event.RuleEntryChannelBreakout,
			ADR:                event.ADREntryChannelBreakout,
			Direction:          event.DirectionLong,
			EntryChannelLength: r.entryChannelLength,
			EntryChannelHigh:   entryChannelHigh,
			BreakoutHigh:       view.High,
			N:                  decisionN,
		}
		if err := signalPayload.Validate(); err != nil {
			return nil, fmt.Errorf("strategy: built invalid signal payload: %w", err)
		}
		signalBytes, err := json.Marshal(signalPayload)
		if err != nil {
			return nil, fmt.Errorf("strategy: marshal signal payload: %w", err)
		}
		signalID := decisionID("signal", bar.InstrumentID, bar.PeriodEnd)
		signal := r.stamp(signalID, event.SignalEventType, event.SignalSchemaVersion, bar.PeriodEnd, envelope, signalBytes)
		emissions = append(emissions, signal)

		// The Signal is sized into a trade proposal when its Session closes,
		// ranked against the Session's other Signals and after its exits and
		// Adds (ADR 0010, ADR 0021; session.go). Exactly one emission then
		// follows the Signal — a proposal, or a decline saying why there is
		// none — so a Signal is never left with nothing after it (see
		// sizeUnit).
		//
		// The entry level is entryChannelHigh — the level a resting buy-stop
		// actually sits at (ADR 0005) — not view.High, the breakout bar's
		// own high. See sizeUnit's doc comment for why.
		state.pendingSignal = &pendingSignalState{
			signalID:       signalID,
			periodEnd:      bar.PeriodEnd,
			entryLevel:     entryChannelHigh,
			n:              decisionN,
			nReady:         nReady,
			earliestFillAt: previousPeriodEnd,
		}
	}

	return emissions, nil
}

// sizeUnit turns a Signal into either a trade proposal or a recorded decline,
// and returns exactly one envelope either way.
//
// "Either way" is the point. A Signal that produces no position must leave a
// journal entry saying so, or "no Signal today" and "a Signal whose sizing
// produced nothing" become indistinguishable on replay — and the second is a
// fact about the strategy's capacity that a reviewer needs. So every path
// below that refuses to propose emits a strategy.proposal.declined event with
// an enumerated reason instead; none of them silently returns nothing.
//
// The Notional Account is read from r.notionalAccount.Current() (ADR 0007):
// its configured starting value until an account.snapshot applies a
// Drawdown Step, never actual account equity. Yearly re-basing and recovery
// are handled separately (notional.go). A proposal places a hold for its
// Unit (hold.go; ADR 0020, as amended 2026-09-24), so every later proposal
// is checked against the cash and cap headroom it leaves.
//
// entryLevel is the Entry Channel high the breakout exceeded — the level a
// resting buy-stop actually sits at under ADR 0005, not the breakout bar's
// own high. Faith's own wording is "the price exceeded by a single tick the
// high ... of the preceding 55 days" [T p.19]; the Baseline's tick increment
// is zero, with the Signal's own strict exceedance doing that work, declared
// as a baseline-declared adaptation under ADR 0012 rather than an invented
// constant — a nominal tick is not a stable quantity on split-adjusted
// prices (ADR 0004: it scales with the adjustment factor), and ADR 0013's
// 0.05 N of slippage against the trader already exceeds a cent for every
// instrument the Baseline's universe admits, so a tick would be swallowed by
// slippage anyway. What fills at this level is the fill model's decision,
// and slippage is a fill concern (ADR 0013), so
// nothing is applied to it here.
// previousClose is the period end of the bar BEFORE the decision bar — the
// moment the decision bar opened — and is what ADR 0010's cash basis is
// measured at, not the decision bar's own close.
//
// strength is the Signal's own Strength, already computed by rankSignal
// (session.go) for the ranking pass that placed this Signal ahead of or
// behind the Session's others (ADR 0010, as amended by the owner's decision
// of 2026-09-25). Every path below carries it into whichever decision event
// results, proposed or declined, so "each Signal's Strength appears in its
// decision event" holds for every one of sizeUnit's outcomes.
func (r *transition) sizeUnit(instrumentID string, periodEnd time.Time, input event.Envelope, signalID string, entryLevel, n float64, nReady bool, previousClose time.Time, strength float64) (event.Envelope, error) {
	// Unreachable from this reducer: Tier A requires a ready N, and a
	// Signal is only emitted at Tier A. Guarded anyway — .greptile/rules.md
	// requires a zero, negative or not-yet-warm volatility value to fail
	// closed, and "it cannot happen here" is not a reason to divide by it if
	// the Tier logic above ever changes.
	if !nReady {
		return r.decline(instrumentID, periodEnd, input, signalID, event.DeclineReasonNNotReady,
			fmt.Sprintf("n is not a usable volatility reading (n %v); no unit can be sized from it", n), 0, 0, strength)
	}

	unit, err := sizing.SizeUnit(sizing.Inputs{
		Mode:                   r.sizingMode,
		NotionalAccount:        r.notionalAccount.Current(),
		UnitVolatilityFraction: r.unitVolatilityFrac,
		StopMultiple:           r.stopMultiple,
		RiskAtStopFraction:     r.riskAtStopFraction,
		N:                      n,
		DollarsPerPoint:        r.dollarsPerPoint,
	})
	if err != nil {
		// Not a decline: the configuration has already been validated and N
		// is known usable, so an error here means an input this reducer
		// believed sound produced impossible arithmetic. That is a defect,
		// and a defect stops the run rather than being journalled as a
		// routine decline.
		return event.Envelope{}, fmt.Errorf("strategy: instrument %q at %s: %w",
			instrumentID, periodEnd.Format(time.RFC3339), err)
	}

	if unit.Quantity <= 0 {
		// The Turtle Rules p.15 names this outcome directly: small accounts
		// lose diversification because truncation is coarse. It is a fact
		// about the account, not an error.
		return r.decline(instrumentID, periodEnd, input, signalID, event.DeclineReasonQuantityBelowOneUnit,
			fmt.Sprintf("notional account %v under %s sizing, with n %v and dollars per point %v, sizes fewer than one whole unit",
				r.notionalAccount.Current(), r.configuredSizingMode, n, r.dollarsPerPoint), 0, 0, strength)
	}

	// The Protective Stop intent, in the expression order
	// event.TradeProposalPayload.Validate re-derives it in, so the two agree
	// bit for bit (CONTEXT.md: "Protective Stop"; The Turtle Rules p.22's 2N
	// stop in the Baseline).
	protectiveStopIntent := entryLevel - sizing.Product(r.stopMultiple, n)
	if protectiveStopIntent <= 0 {
		// A long equity cannot trade below zero, so this stop is
		// unreachable: the Unit would in fact risk the whole position rather
		// than the derived fraction. Declining is the fail-closed answer, and
		// journalling it is how the condition becomes visible instead of
		// looking like a bar that simply did not signal.
		return r.decline(instrumentID, periodEnd, input, signalID, event.DeclineReasonStopIntentNotPositive,
			fmt.Sprintf("protective stop intent %v (entry level %v - stop multiple %v x n %v) is not a reachable price for a long position",
				protectiveStopIntent, entryLevel, r.stopMultiple, n), 0, 0, strength)
	}

	// ADR 0008's four Unit caps, checked against POST-TRADE exposure: the
	// Units that would be held, across every open Campaign sharing a cap's
	// grouping, once this new Campaign's opening Unit joined them
	// (unit_caps.go). No Campaign exists yet for this instrument (a Signal
	// only fires for a Setup, CONTEXT.md), so the per-instrument cap can
	// never bind here — only the group and total-long caps can, once
	// several Campaigns already share this instrument's classification or
	// the account's total long exposure is already near its limit.
	instrumentClass := r.classificationOf(instrumentID)
	if capName, limit, exposure, exceeded := r.capExceeded(instrumentID, instrumentClass); exceeded {
		return r.declineCap(instrumentID, periodEnd, input, signalID, capName, limit, exposure, strength)
	}

	availableCash, err := r.cashAtPreviousClose(instrumentID, previousClose)
	if err != nil {
		return event.Envelope{}, err
	}
	// The hold this Unit would place is what it must be able to fund: its
	// worst-case cost at its price cap, slippage and commission included
	// (ADR 0020, as amended 2026-09-24; hold.go's buyHold).
	cost, priceCap, costRepresentable := r.buyHold(unit.Quantity, entryLevel, n)
	if !costRepresentable {
		// More than any cash that can be held, so the Unit is skipped on the
		// same rule an unaffordable one is (ADR 0010) — journalled, with the
		// operands in Detail, rather than stopping the run on a cost no
		// payload can carry.
		return r.decline(instrumentID, periodEnd, input, signalID, event.DeclineReasonUnitCostNotRepresentable,
			fmt.Sprintf("unit hold (%s) leaves the representable range, so it exceeds any cash that could fund it; spendable cash at the attempt was %v",
				r.buyHoldDetail(unit.Quantity, entryLevel, n, priceCap), availableCash), 0, 0, strength)
	}
	if cost > availableCash {
		// No partial Unit, ever: the whole Unit is skipped (ADR 0010), never
		// resized down to what the available cash would cover.
		return r.decline(instrumentID, periodEnd, input, signalID, event.DeclineReasonInsufficientCash,
			fmt.Sprintf("unit hold %v (%s) exceeds spendable cash at the attempt %v, after fill debits and standing holds",
				cost, r.buyHoldDetail(unit.Quantity, entryLevel, n, priceCap), availableCash),
			cost, availableCash, strength)
	}

	proposal := event.TradeProposalPayload{
		InstrumentID: instrumentID,
		PeriodEnd:    periodEnd,
		SignalID:     signalID,
		// Rule names what the sizing computes, per Sizing Mode, never the
		// parameter values it ran with — the same reasoning the Signal's rule
		// name follows. ADR 0003 is the defining decision for both
		// modes: it is what declares that there are two and that choosing
		// between them is a declared experiment.
		Rule:                   sizingRuleFor(r.configuredSizingMode),
		ADR:                    event.ADRUnitSizing,
		Direction:              event.DirectionLong,
		EntryLevel:             entryLevel,
		Quantity:               unit.Quantity,
		N:                      n,
		SizingMode:             r.configuredSizingMode,
		UnitVolatilityFraction: r.unitVolatilityFrac,
		StopMultiple:           r.stopMultiple,
		RiskAtStop:             unit.RiskAtStop,
		RealisedRiskAtStop:     unit.RealisedRiskAtStop,
		DollarsPerPoint:        r.dollarsPerPoint,
		NotionalAccount:        r.notionalAccount.Current(),
		ProtectiveStopIntent:   protectiveStopIntent,
		OrderType:              r.buyOrderType,
		GapBufferN:             r.gapBufferN,
		PriceCap:               priceCap,
		Strength:               strength,
	}
	if err := proposal.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: built invalid trade proposal payload: %w", err)
	}
	proposalBytes, err := json.Marshal(proposal)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: marshal trade proposal payload: %w", err)
	}
	proposalID := decisionID("proposal", instrumentID, periodEnd)
	// Reserved now, before the next proposal of this pass is checked (ADR
	// 0020, as amended 2026-09-24): the Unit's cost against cash, and its
	// Unit against every cap it counts towards.
	if err := r.placeHold(proposalID, instrumentID, instrumentClass, cost); err != nil {
		return event.Envelope{}, err
	}
	return r.stamp(
		proposalID,
		event.TradeProposalEventType, event.TradeProposalSchemaVersion,
		periodEnd, input, proposalBytes,
	), nil
}

// decline builds the strategy.proposal.declined emission for one Signal that
// produced no position (Kind ProposalDeclinedKindEntry — see declineAdd,
// campaign.go, for the Add-kind counterpart).
//
// requiredCash and availableCash are only meaningful for reason
// event.DeclineReasonInsufficientCash; every other caller passes 0, 0
// (ProposalDeclinedPayload.Validate rejects a non-zero value for any other
// reason). strength is the Signal's own Strength (ADR 0010, as amended by the
// owner's decision of 2026-09-25); every caller except session.go's
// insufficient-history path passes the value rankSignal already computed for
// it, since ranking runs, and Strength is known, before any of these reasons
// can be reached.
func (r *transition) decline(instrumentID string, periodEnd time.Time, input event.Envelope, signalID, reason, detail string, requiredCash, availableCash, strength float64) (event.Envelope, error) {
	return r.stampDecline(instrumentID, periodEnd, input, event.ProposalDeclinedPayload{
		Kind:          event.ProposalDeclinedKindEntry,
		SignalID:      signalID,
		Reason:        reason,
		Detail:        detail,
		RequiredCash:  requiredCash,
		AvailableCash: availableCash,
		Strength:      strength,
	})
}

// declineCap is decline's counterpart for ADR 0008: a Signal that fired and
// sized, but whose resulting Campaign a Unit cap
// (event.DeclineReasonUnitCapExceeded) blocks, naming the cap that bound,
// its configured limit, and the post-trade exposure the Unit would have
// produced (unit_caps.go: capExceeded).
func (r *transition) declineCap(instrumentID string, periodEnd time.Time, input event.Envelope, signalID, capName string, limit, exposure int, strength float64) (event.Envelope, error) {
	return r.stampDecline(instrumentID, periodEnd, input, event.ProposalDeclinedPayload{
		Kind:              event.ProposalDeclinedKindEntry,
		SignalID:          signalID,
		Reason:            event.DeclineReasonUnitCapExceeded,
		Detail:            capDetail(capName, limit, exposure),
		Cap:               capName,
		CapLimit:          limit,
		PostTradeExposure: exposure,
		Strength:          strength,
	})
}

// stampDecline finalises payload with the identifying fields every
// entry-kind decline shares (InstrumentID, PeriodEnd), validates, marshals
// and stamps it — the common tail decline and declineCap share, whatever
// reason and figures produced the decline.
func (r *transition) stampDecline(instrumentID string, periodEnd time.Time, input event.Envelope, payload event.ProposalDeclinedPayload) (event.Envelope, error) {
	payload.InstrumentID = instrumentID
	payload.PeriodEnd = periodEnd
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: built invalid proposal declined payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: marshal proposal declined payload: %w", err)
	}
	return r.stamp(
		decisionID("proposal-declined", instrumentID, periodEnd),
		event.ProposalDeclinedEventType, event.ProposalDeclinedSchemaVersion,
		periodEnd, input, payloadBytes,
	), nil
}

// cashAtPreviousClose returns spendable cash for a decision on the bar that
// opened at previousClose. ADR 0010 requires the snapshot to be known at the
// previous close; ADR 0020's cash-movement amendment reduces its cash by
// accepted withdrawals without advancing that timestamp; and ADR 0020 takes
// from it every entry and Add fill the snapshot cannot reflect and, as
// amended 2026-09-24, every hold still standing: "available = basis -
// every actual fill cost - every hold still standing". Exits in bar t free
// capital for bar t+1 and never for bar t.
//
// The result can be negative. The zero floor bounds the basis only, and a
// fill the basis could not fund is recorded at its actual cost (ADR 0020,
// "The invariant"); no Unit can then be funded.
//
// It fails closed twice over, because either state would size a Unit against
// cash the decision was not entitled to:
//
//   - no account.snapshot has supplied a figure at all, which must not be
//     read as infinite cash;
//   - the figure that was supplied is stamped LATER than previousClose, so it
//     reports an account the decision bar has already begun to change. A
//     snapshot dated after the decision bar is the plainest case; a
//     zero previousClose — an instrument with no bar before this one — is the
//     same refusal, since a payload's AsOf is never the zero time
//     (AccountSnapshotPayload.Validate).
//
// Accepting a snapshot onto the account timeline (applyAccountSnapshot,
// notional.go) is deliberately separate from deciding whether it may be
// spent: chronology there is per account, and this is per decision.
func (r *transition) cashAtPreviousClose(instrumentID string, previousClose time.Time) (float64, error) {
	if !r.hasAvailableCash {
		return 0, fmt.Errorf(
			"strategy: instrument %q: no account.snapshot has ever supplied an available-cash figure; refusing to size a unit as though cash were infinite (ADR 0010)",
			instrumentID)
	}
	if r.availableCashAsOf.After(previousClose) {
		return 0, fmt.Errorf(
			"strategy: instrument %q: the available-cash figure as of %s is not cash known at the previous close %s; refusing to size a unit against cash the decision bar had not yet earned (ADR 0010)",
			instrumentID, r.availableCashAsOf.Format(time.RFC3339), previousClose.Format(time.RFC3339))
	}
	return r.availableCash - r.fillDebitTotal() - r.holdTotal(), nil
}

// unitCost is what one whole Unit costs to put on under ADR 0010: its
// quantity x the order's own resting level (ADR 0005 — never a fill price the
// reducer cannot know yet) x dollars per point, and whether that product is a
// number this system can state.
//
// It reports false when three finite operands multiply past the float64
// range. No guard on the operands can rule that out — no rule caps the
// Notional Account, a contract multiplier is configured, and a bar's prices
// need only be finite — and +Inf is not a figure a decline can carry, since
// JSON cannot encode it. A caller that gets false must skip the Unit without
// recording the cost, never record the cost.
//
// A bare product feeding a comparison, never an addition or subtraction, so
// it needs no sizing.Product barrier (docs/development.md: "a*b*c with no
// addition is not fusible and needs nothing"), and callers compare it with a
// plain > rather than a subtracted difference for the identical reason.
func unitCost(quantity int64, level, dollarsPerPoint float64) (float64, bool) {
	cost := float64(quantity) * level * dollarsPerPoint
	return cost, !math.IsInf(cost, 0) && !math.IsNaN(cost)
}

// sizingRuleFor names the rule a proposal cites, per Sizing Mode.
//
// The default is deliberately not a fallback to the Baseline's name: a
// proposal is only ever built after sizing.SizeUnit has accepted the mode, so
// reaching it would mean a mode this build cannot size produced a position.
// An empty rule fails event.TradeProposalPayload.Validate, so the run stops
// rather than journalling a proposal that names the wrong principle.
func sizingRuleFor(mode event.SizingMode) string {
	switch mode {
	case event.SizingModeVolatilityNormalised:
		return event.RuleUnitSizingVolatilityNormalised
	case event.SizingModeFixedRiskAtStop:
		return event.RuleUnitSizingFixedRiskAtStop
	default:
		return ""
	}
}

// decisionID builds an emitted envelope's ID from the kind of decision, the
// instrument, and the completed bar it belongs to.
//
// Exactly one decision of each kind exists per (instrument, completed bar),
// so that triple identifies it uniquely — which makes the ID deterministic
// and reproducible on replay without any randomness or wall-clock read (both
// forbidden in internal/; see .golangci.yml's forbidigo rules). The timestamp
// layout is fixed and includes nanoseconds so two bars can never collapse to
// the same ID through formatting.
func decisionID(kind, instrumentID string, periodEnd time.Time) string {
	return fmt.Sprintf("%s:%s:%s", kind, instrumentID, periodEnd.UTC().Format("2006-01-02T15:04:05.000000000Z"))
}

// stamp fills in the envelope fields every emission from this reducer shares.
//
// EventTime is the completed bar's period end and RecordedAt is the input
// envelope's RecordedAt — never time.Now(), which .golangci.yml forbids in
// internal/ precisely because a decision must depend only on the ordered
// event stream. Sequence, CausationID and CorrelationID are deliberately left
// unset: replay.Engine assigns them, overwriting whatever a handler sets, so
// a handler cannot claim causation it did not have (docs/architecture.md).
func (r *transition) stamp(id, eventType string, schemaVersion uint32, periodEnd time.Time, input event.Envelope, payload json.RawMessage) event.Envelope {
	return event.Envelope{
		ID:                id,
		Type:              eventType,
		SchemaVersion:     schemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         periodEnd,
		RecordedAt:        input.RecordedAt,
		Source:            sourceReducer,
		StrategyVersion:   r.strategyVersion,
		ConfigurationHash: r.configurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

func (r *transition) stateFor(instrumentID string) (*instrumentState, error) {
	if state, ok := r.instrument(instrumentID); ok {
		return state, nil
	}
	n, err := indicator.NewWilderAverage(indicator.DefaultPeriod)
	if err != nil {
		// Unreachable: indicator.DefaultPeriod is a positive compile-time
		// constant, so NewWilderAverage never rejects it. Failing closed
		// anyway rather than panicking, in case that ever changes.
		return nil, fmt.Errorf("strategy: %w", err)
	}
	entryChannel, err := indicator.NewEntryChannel(r.entryChannelLength)
	if err != nil {
		// Unreachable in practice: applyConfiguration only sets
		// entryChannelLength from a payload that ConfigurationPayload.Validate
		// has already required to be positive. Failing closed anyway rather
		// than panicking, in case that ever changes.
		return nil, fmt.Errorf("strategy: %w", err)
	}
	// The Exit Channel, built alongside the Entry Channel and fed
	// every completed bar regardless of Campaign state (see
	// applyCompletedBar), so it is already warm the moment a Campaign
	// opens.
	exitChannel, err := indicator.NewExitChannel(r.exitChannelLength)
	if err != nil {
		// Unreachable in practice, for the same reason entryChannel's guard
		// above is: ConfigurationPayload.Validate already requires
		// ExitChannelLength to be positive. Failing closed anyway rather
		// than panicking, in case that ever changes.
		return nil, fmt.Errorf("strategy: %w", err)
	}
	// The ranking seam's own bounded history (session.go: rankSignal). Both
	// capacities are package constants, so, like n/entryChannel/exitChannel
	// above, these never actually fail; guarded anyway rather than
	// panicking.
	splitAdjustedCloses, err := indicator.NewRollingWindow(indicator.StrengthLookbackBars + 1)
	if err != nil {
		return nil, fmt.Errorf("strategy: %w", err)
	}
	rawCloses, err := indicator.NewRollingWindow(indicator.DollarVolumeWindow)
	if err != nil {
		return nil, fmt.Errorf("strategy: %w", err)
	}
	rawVolumes, err := indicator.NewRollingWindow(indicator.DollarVolumeWindow)
	if err != nil {
		return nil, fmt.Errorf("strategy: %w", err)
	}
	state := &instrumentState{
		n:                   n,
		entryChannel:        entryChannel,
		exitChannel:         exitChannel,
		splitAdjustedCloses: splitAdjustedCloses,
		rawCloses:           rawCloses,
		rawVolumes:          rawVolumes,
	}
	r.addInstrument(instrumentID, state)
	return state, nil
}

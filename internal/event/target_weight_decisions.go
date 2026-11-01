package event

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// TargetsDecidedEventType identifies a target-weight strategy's decision for
// one account at one decision time (ADR 0026, point 1). It decides targets
// only; it never names an order.
const TargetsDecidedEventType = "strategy.targets.decided"

// TargetsDecidedSchemaVersion is the current schema version of TargetsDecidedPayload.
//
//   - Version 1 recorded Boost decisions only.
//   - Version 2 adds momentum decisions: StrategyID momentum, the Momentum
//     figures, and the withheld reason rebalance-in-progress. Boost is
//     omitted from a momentum decision and Momentum from a Boost one, so a
//     Boost decision's payload bytes are version 1's.
//   - Version 3 adds the glide path to momentum decisions: the Glide figures
//     (the rebalance session, the owner's age in months and the bond share
//     b), and each target's sleeve Shares, which record what the momentum
//     and bond sleeves each contribute to a fund both hold (ADR 0026,
//     point 1). Both are omitted from a Boost decision, whose payload bytes
//     are still version 1's.
//   - Version 4 records D7-L's substitution in momentum decisions (ADR 0026,
//     point 2, "D7-L"): the MinimumLot figures the picks were valued against,
//     the name status unaffordable, and each unaffordable name's one share
//     and target value. All are omitted when unset, so a Boost decision's
//     bytes are still version 1's. The withheld reason histories-missing,
//     which only a run reading the compact universe input emits, is a
//     reason of this version too: no journal read before that input has it.
//     So is labels-missing, which only a run under configuration version 7
//     emits, and a name's GapSessions, which rules 1.15.0 and later record and
//     which are omitted when empty, so a Boost decision's bytes are still
//     version 1's (ADR 0026 point 5, amended 2026-10-18).
//   - Version 5 records Boost's pending re-plan (ADR 0026, point 2, "Boost",
//     amended 2026-10-18): BoostFigures.ReplanPending, set when the next
//     decided session plans the difference to the target in force after a
//     refusal that left the engine Normal, so a restart from the decision
//     recovers it (ADR 0026, point 5). It is omitted when unset, so a
//     decision without a pending re-plan has version 4's bytes. The name
//     status held-one-lot-short and a name's HeldQuantity, HeldValue and
//     HeldShare, which rules 1.17.0 and later record for a held pick D7-L
//     keeps one lot short (ADR 0026, point 2, "D7-L", amended 2026-10-18),
//     are of this version too; the figures are omitted when unset, so a
//     decision with no such name has the bytes it had before.
//   - Version 6 records why a Boost decision re-plans (ADR 0026, point 2,
//     "Boost", amended 2026-10-18): BoostFigures.Replan, the cause, refused,
//     part-filled, unfilled or rejected, with such an order's client order
//     ID and its filled and released shares, omitted when the decision
//     entered with no re-plan pending; BoostFigures.ReplanWithheld, the
//     rejection that made the account's orders rejected in a row two;
//     BoostFigures.VenueRejections, the rejections in a row,
//     BoostFigures.RejectedInARow, the orders it counts, and
//     BoostFigures.UndecidedOrders, the released orders the venue has not
//     yet ruled on, and BoostFigures.UndecidedBeforeRejection, how many of
//     them were released before the latest rejection counted;
//     BoostFigures.NoVenue, dry-run, when no venue receives the orders, so
//     none is listed;
//     BoostFigures.ClearedOrders, the orders the owner settled as never sent
//     (operator.rebalance.cleared), each with its release;
//     BoostFigures.WorkingOrders, the orders the venue accepted that still
//     work besides the watched one, each with its executions;
//     BoostFigures.RejectedWhileStopped and RejectionsWhileStopped, the
//     rejections read after that stop and their count;
//     BoostFigures.Watched, the order watched for an unfilled remainder with
//     its executions, its end and its venue order ID, so a restart watches
//     it still; and
//     BoostFigures.GapSessions, the sessions with no bar the decision's
//     readings span (ADR 0026, point 5, amended 2026-10-18). Each is omitted
//     when unset.
//   - Version 7 records BoostFigures.RetiredOrders, the orders retired as
//     never sent (execution.order.retired), each with its release and when
//     it was retired, so a run restarted from the decision refuses a venue
//     report on one as the full run does (ADR 0026, point 2, "Boost", and
//     point 5, amended 2026-10-22), a retired sale back to 98% with its tag
//     and the attempt retired, and BoostFigures.NamedOrders, the orders a
//     venue named, so the restarted run refuses their retirement as the
//     full run does, and BoostFigures.RestoredSale, the sale attempt the
//     owner's approval restored, so the restarted run restores it as the
//     approved run holds it. Each is omitted when none, so a decision with
//     no retired or named order and no restored sale has version 6's bytes.
//   - Version 8 records the run's own live sale back to 98%
//     (BoostRestoredSale.Order) whether or not a venue has named it and
//     whether or not part of it has filled, with its side, the attempt it
//     is, whether a venue has named it, its venue order ID and its
//     executions so far, so a run restarted from the decision holds it as
//     released, named as it was and filled as far as it was, and reads the
//     venue's later reports on it as the full run does (ADR 0026, point 2,
//     "Boost", and point 5, amended 2026-10-22). Version 7 recorded the sale
//     only while no venue had named it and nothing had filled, with its
//     client order ID, shares, limit and trigger; a version 7 record is
//     read as it was written (TargetsDecidedVersions, ValidateSchema).
//   - Version 9 records the run's own sale back to 98% once it has ended
//     (BoostRestoredSale.Order), canceled or invalid with when the venue
//     ended it (Status, EndedAt), or filled in full, whenever a venue order
//     ID is known for it, with its executions as before, so a run restarted
//     from the decision reads a late fill after the cancel (ADR 0022), an
//     execution sent again and a later lifecycle report on it as the full
//     run does (ADR 0026, point 5: recovered, never recomputed). Version 8
//     recorded the sale only while it was live; a version 8 record is read
//     as it was written.
//   - Version 10 records each momentum pick withheld for a missing mark,
//     with its adapter evidence (ADR 0026, amendment of 2026-10-29).
const TargetsDecidedSchemaVersion uint32 = 10

// TargetsDecidedRetiredVersion is version 7, the last version before the
// live sale's side, attempt, naming and executions were recorded; a reader
// reads it beside the later versions (ADR 0015).
const TargetsDecidedRetiredVersion uint32 = 7

// TargetsDecidedLiveSaleVersion is version 8, the last version before the
// run's own ended sale was recorded; a reader reads it beside the current
// version (ADR 0015).
const TargetsDecidedLiveSaleVersion uint32 = 8

// TargetsDecidedEndedSaleVersion is version 9, before per-pick withholding
// was recorded; historical decisions remain readable as written (ADR 0015).
const TargetsDecidedEndedSaleVersion uint32 = 9

// TargetsDecidedVersions are the schema versions a reader of the log reads
// a decision at: versions 7, 8 and 9, which earlier builds wrote, and the
// current one.
func TargetsDecidedVersions() []uint32 {
	return []uint32{TargetsDecidedRetiredVersion, TargetsDecidedLiveSaleVersion, TargetsDecidedEndedSaleVersion, TargetsDecidedSchemaVersion, TargetsDecidedFractionalVersion}
}

// The two outcomes of a target decision (ADR 0026, point 1).
const (
	TargetsOutcomeDecided  = "decided"
	TargetsOutcomeWithheld = "withheld"
)

// The closed set of reasons a decision is withheld (ADR 0026, points 1 and 5).
const (
	// WithheldMissingBars: a session closed without the instrument's bar.
	// Boost skips the session and its next bar continues the indicators
	// (ADR 0026, point 5, amended 2026-10-18).
	WithheldMissingBars = "missing-bars"
	// WithheldStaleAccountSnapshot: no account snapshot as of the session's
	// close, so nothing can be valued at the previous close (ADR 0020).
	WithheldStaleAccountSnapshot = "stale-account-snapshot"
	// WithheldMissingUniverse: no market.universe.snapshot for the decision
	// time, so momentum cannot rank (ADR 0026, point 1, "Universe").
	WithheldMissingUniverse = "missing-universe"
	// WithheldInsufficientTrendHistory: fewer than 200 SPY closes, which the
	// backtest treats as risk-on and live withholds (ADR 0026, point 5).
	WithheldInsufficientTrendHistory = "insufficient-trend-history"
	// WithheldNoEligibleNames: the ranking left no name to hold, none of
	// them skipped as unaffordable or kept held one lot short.
	WithheldNoEligibleNames = "no-eligible-names"
	// WithheldRebalanceInProgress: an order of an earlier decision is not
	// yet terminal, so no new rebalance is planned until it is (ADR 0026,
	// point 5: nothing is sent mid-rebalance until the owner clears it).
	WithheldRebalanceInProgress = "rebalance-in-progress"
	// WithheldAllUnaffordable: D7-L skipped as unaffordable or kept held one
	// lot short every pick with a positive target, so no affordable stock
	// could be picked. The book is kept, since a bad net liquidation or bad
	// raw closes would look the same (ADR 0026, point 2, "D7-L", and point
	// 5, "fail closed").
	WithheldAllUnaffordable = "all-unaffordable"
	// WithheldHistoriesMissing: a run reading the compact universe input
	// reached a decision session without a market.close.history for the
	// members it ranks, so it cannot score them and does not decide from the
	// bars of the names held alone (ADR 0026, point 5, "fail closed").
	WithheldHistoriesMissing = "histories-missing"
	// WithheldLabelsMissing: more than the configuration's
	// MaxUnlabelledShare of the ranked members have no sector label, a label
	// outage rather than a few names the sources do not cover. A name with
	// no label is ranked uncapped, so deciding would trade a book with the
	// sector cap silently absent; the book is kept (ADR 0026, point 5, "fail
	// closed", and point 8).
	WithheldLabelsMissing = "labels-missing"
)

// TargetWeight is one instrument's target as a fraction of net liquidation.
// A momentum target records the share each sleeve contributes, momentum then
// bond, whose sum in that order is Weight (plan.md section 2, "Bond sleeve
// and glide path": a fund both sleeves hold is one target, the sum, with each
// sleeve's share in the journal). A Boost target has none.
type TargetWeight struct {
	InstrumentID string        `json:"instrument_id"`
	Weight       float64       `json:"weight"`
	Shares       []SleeveShare `json:"shares,omitempty"`
}

// The closed set of sleeves a momentum target's shares name.
const (
	SleeveMomentum = "momentum"
	SleeveBond     = "bond"
)

// SleeveShare is the part of a target one sleeve contributes.
type SleeveShare struct {
	Sleeve string  `json:"sleeve"`
	Weight float64 `json:"weight"`
}

// GlideFigures are the glide path's figures behind a momentum decision (ADR
// 0026, point 1): the rebalance session the age is counted on, YYYY-MM-DD,
// the owner's age on it in whole months, and the bond share b. The momentum
// book is scaled to 1 - b and the bond sleeve holds 98% of b; the momentum
// Names' weights are the book's before that scaling.
type GlideFigures struct {
	RebalanceSession string  `json:"rebalance_session"`
	AgeMonths        int     `json:"age_months"`
	BondShare        float64 `json:"bond_share"`
}

// BoostFigures are the figures behind a Boost decision, so the journal alone
// can check it (ADR 0026, point 1): the adjusted close, RSI(2), both means,
// the latched boost state before and after (template.py:89-94), and the
// target in force before this decision (template.py's last_want, unset before
// the first decision, template.py:97-100). An indicator not ready carries 0.
type BoostFigures struct {
	InstrumentID     string  `json:"instrument_id"`
	Close            float64 `json:"close"`
	RSI              float64 `json:"rsi"`
	RSIReady         bool    `json:"rsi_ready"`
	TrendMean        float64 `json:"trend_mean"`
	TrendMeanReady   bool    `json:"trend_mean_ready"`
	ExitMean         float64 `json:"exit_mean"`
	ExitMeanReady    bool    `json:"exit_mean_ready"`
	LatchedBefore    bool    `json:"latched_before"`
	LatchedAfter     bool    `json:"latched_after"`
	TargetInForce    float64 `json:"target_in_force"`
	TargetInForceSet bool    `json:"target_in_force_set"`
	// ReplanPending is set when, after this decision, the next decided
	// session plans the difference to the target in force though the target
	// has not changed: this session's order, or an earlier one's still
	// waiting, was refused and the refusal left the engine Normal (ADR 0026,
	// point 2, "Boost", amended 2026-10-18). A restart recovers it from the
	// last decision with the target in force (ADR 0026, point 5). Omitted
	// when unset.
	ReplanPending bool `json:"replan_pending,omitempty"`
	// Replan is the cause of the re-plan this decision planned, or, on a
	// withheld decision, of the one still pending after it (ADR 0026, point
	// 2, "Boost", amended 2026-10-18). Omitted when the decision entered with
	// none pending.
	Replan BoostReplan `json:"replan,omitzero"`
	// GapSessions are the sessions, YYYY-MM-DD and ascending, that closed
	// with no bar since the last bar the indicators read before this
	// decision's: the gap this decision's readings span (ADR 0026, point 5,
	// amended 2026-10-18). Omitted when empty.
	GapSessions []string `json:"gap_sessions,omitempty"`
	// ReplanWithheld is the rejection that made the account's orders the
	// venue rejected in a row two, whatever their targets and whichever
	// order it was, the watched one or an older one rejected late, after
	// which no session buys until the owner starts a fresh run (ADR 0026,
	// point 2, "Boost", amended 2026-10-18): cause rejected, with its client
	// order ID and its filled and released shares. It is recorded once, on
	// the decision of the session at whose close it was read or, when that
	// session only fed the indicators (a restarted run's warm-up), on the
	// first decision after it, and omitted otherwise; a rejection after it
	// is RejectedWhileStopped.
	ReplanWithheld BoostReplan       `json:"replan_withheld,omitzero"`
	Watched        BoostWatchedOrder `json:"watched,omitzero"`
	// VenueRejections counts, as they stand after this decision, the orders
	// decided sessions released that the venue rejected (invalid) in a row,
	// whatever their targets, since the last such order it accepted (an
	// execution, or any lifecycle report but submitted), each counted when
	// the venue's first word on it is read, however late. At
	// BoostVenueRejectionLimit, two, no session buys until the owner starts
	// a fresh run, and the count stays at two, never above it (ADR 0026,
	// point 2, "Boost", amended 2026-10-18), and a restart from the decision
	// recovers it (ADR 0026, point 5). Omitted when none.
	VenueRejections int `json:"venue_rejections,omitempty"`
	// RejectedInARow names the orders VenueRejections counts, in the order
	// their rejections were read: exactly VenueRejections of them, so at
	// the stop the two rejections that stopped buys, which stay named until
	// the fresh run (ADR 0026, point 2, "Boost", amended 2026-10-18). An
	// order the venue accepted empties it with the count before the stop. A
	// restart from the decision recovers it (ADR 0026, point 5). Omitted
	// when none.
	RejectedInARow []BoostRejectedOrder `json:"rejected_in_a_row,omitempty"`
	// UndecidedOrders are the orders decided sessions released that the
	// venue has not yet ruled on, accepted or rejected, in release order: a
	// late verdict on any of them counts as it would have when it came
	// (ADR 0026, point 2, "Boost", amended 2026-10-18). Each but the watched
	// order records its side, shares and limit as released and its venue
	// order ID once a lifecycle report or a broker snapshot's open-order
	// listing named it, so a run restarted from the
	// decision reads the venue's reports on it, and nets a working sell, as
	// on an order it released (ADR 0026, point 5; ADR 0019, amended
	// 2026-10-15); the watched order's own are in Watched. Omitted when none.
	UndecidedOrders []BoostUndecidedOrder `json:"undecided_orders,omitempty"`
	// ClearedOrders are the orders the owner settled as never sent
	// (operator.rebalance.cleared), in the order cleared, with the release
	// the gate still counts: a never-sent order leaves UndecidedOrders but
	// stays released until the owner's fresh run, so a sell is still counted
	// as sold. Each records its side, shares and limit as released, and
	// none is also undecided; the watched order, if cleared, is among them
	// as well as in Watched. A run restarted from the decision restores
	// them (ADR 0026, point 2, "Boost", amended 2026-10-18, and point 5).
	// Omitted when none.
	ClearedOrders []BoostUndecidedOrder `json:"cleared_orders,omitempty"`
	// RetiredOrders are the orders retired as never sent
	// (execution.order.retired), in the order retired, each with its release
	// and when it was retired: ended with nothing filled, so a venue report
	// on one proves the retirement wrong and is refused. None is also
	// undecided, cleared or working; the watched order, if retired, is among
	// them as well as in Watched. A run restarted from the decision restores
	// them (ADR 0026, point 2, "Boost", and point 5, amended 2026-10-22).
	// Omitted when none.
	RetiredOrders []BoostRetiredOrder `json:"retired_orders,omitempty"`
	// RestoredSale is the record of the incident's sale back to 98% as the
	// run holds it: the one the owner's approval of this run restored
	// (IncidentInForce) or, with none restored, that of the incident in
	// force on Boost's instrument, approved or not: whether it was attempted,
	// by an earlier run or by this one and not since retired, and how many
	// of its attempts earlier runs retired as never sent, so a run restarted
	// from the decision restores it as this run holds it and never attempts
	// it again, nor reuses a retired order's client order ID (ADR 0019,
	// amended 2026-10-15; ADR 0026, point 2, "Order identity", and point 5:
	// recovered, never recomputed). Omitted when there is neither.
	RestoredSale *BoostRestoredSale `json:"restored_sale,omitempty"`
	// NamedOrders are the client order IDs, strictly ascending, of the
	// watched, undecided and working orders a venue has named (a lifecycle
	// report, an execution or a broker snapshot's open-order listing): each
	// reached IBKR, so its retirement is refused, and a run restarted from
	// the decision refuses it as the full run does (ADR 0026, point 2,
	// "Boost", and point 5, amended 2026-10-22). Omitted when none.
	NamedOrders []string `json:"named_orders,omitempty"`
	// WorkingOrders are the orders the venue accepted (an execution, or any
	// lifecycle report but submitted) that still work, neither fully filled
	// nor ended, besides the watched order, in the order the venue accepted
	// them: a later
	// order replaced them as the watched one. Each records its release, its
	// executions and its venue order ID as Watched does, and no end, so a
	// run restarted from the decision reads the venue's later fills and
	// cancel of it, and nets a working sell, as on an order it released (ADR
	// 0026, point 5; ADR 0019, amended 2026-10-15). None is also the
	// watched, an undecided or a cleared order. Omitted when none.
	WorkingOrders []BoostWatchedOrder `json:"working_orders,omitempty"`
	// RejectedWhileStopped are the account's orders the venue rejected after
	// two rejections in a row stopped Boost's buys, read since the last
	// decision: a sell sent while the stop is in force, or an order released
	// before it, each cause rejected with its order and shares, recorded
	// once and paged once (ADR 0026, points 2, "Boost", amended 2026-10-18,
	// and 4). Omitted when none.
	RejectedWhileStopped []BoostReplan `json:"rejected_while_stopped,omitempty"`
	// RejectionsWhileStopped counts those rejections since the stop, as they
	// stand after this decision, so a page can say which one it is and a
	// restart from the decision counts on from it. Omitted when none.
	RejectionsWhileStopped int `json:"rejections_while_stopped,omitempty"`
	// UndecidedBeforeRejection counts the first of UndecidedOrders, in
	// release order, that were released before the latest released of the
	// orders RejectedInARow names: the venue's acceptance of one of them,
	// read late, is older than the run of rejections and does not end it,
	// so a re-planned order rejected again is always the second in a row
	// (ADR 0026, point 2, "Boost", amended 2026-10-18). A restart from the
	// decision recovers it (ADR 0026, point 5). Omitted when none.
	UndecidedBeforeRejection int `json:"undecided_before_rejection,omitempty"`
	// NoVenue is the run's mode, dry-run, when no venue receives the orders
	// it releases: none will ever be ruled on, so none is listed in
	// UndecidedOrders and no verdict is counted (ADR 0026, point 3, "Mode",
	// and point 2, "Boost", amended 2026-10-18). Omitted in paper and live.
	NoVenue string `json:"no_venue,omitempty"`
}

// BoostVenueRejectionLimit is how many of the account's orders the venue
// may reject in a row before Boost buys nothing more until the owner starts
// a fresh run (ADR 0026, point 2, "Boost", amended 2026-10-18).
const BoostVenueRejectionLimit = 2

// BoostRejectedOrder is one of the orders BoostFigures.VenueRejections
// counts: its client order ID and, when a lifecycle report or a broker
// snapshot's open-order listing named it, the venue's own ID for it, so the
// record and the page can name it.
type BoostRejectedOrder struct {
	ClientOrderID string `json:"client_order_id"`
	VenueOrderID  string `json:"venue_order_id,omitempty"`
}

// BoostUndecidedOrder is an order a decided session released that the
// venue has not yet ruled on (BoostFigures.UndecidedOrders): its client
// order ID and, unless it is the watched order, its side, shares and limit
// as released and its venue order ID once a lifecycle report or a broker
// snapshot's open-order listing named it.
type BoostUndecidedOrder struct {
	ClientOrderID string  `json:"client_order_id"`
	Side          string  `json:"side,omitempty"`
	Released      int64   `json:"released,omitempty"`
	LimitPrice    float64 `json:"limit_price,omitempty"`
	VenueOrderID  string  `json:"venue_order_id,omitempty"`
}

// BoostRetiredOrder is an order retired as never sent
// (BoostFigures.RetiredOrders): its client order ID, its side, shares and
// limit as released, and when it was retired.
type BoostRetiredOrder struct {
	ClientOrderID string    `json:"client_order_id"`
	Side          string    `json:"side"`
	Released      int64     `json:"released"`
	LimitPrice    float64   `json:"limit_price"`
	RetiredAt     time.Time `json:"retired_at"`
	// SaleTag and SaleAttempt are, for a retired sale back to 98%, the tag
	// it was planned under and the number of the attempt retired, the first
	// being 1, so a run restarted from the decision gives the next attempt a
	// client order ID of its own (orderplan.SaleAttemptClientOrderID; ADR
	// 0026, point 2, "Order identity", and point 5). Both are omitted for
	// any other order.
	SaleTag     string `json:"sale_tag,omitempty"`
	SaleAttempt int    `json:"sale_attempt,omitempty"`
}

// BoostRestoredSale is the incident's sale attempt as the run holds it
// (BoostFigures.RestoredSale): its incident, whether the sale was attempted,
// and how many of its attempts earlier runs retired as never sent.
type BoostRestoredSale struct {
	IncidentID      string `json:"incident_id"`
	Attempted       bool   `json:"attempted"`
	RetiredAttempts int    `json:"retired_attempts"`
	// Order is the sale this run attempted itself and released, while it is
	// still live: not ended and not filled in full, whether or not a venue
	// has named it and whether or not part of it has filled; and, once it
	// has ended (canceled or invalid) or filled in full, whenever a venue
	// order ID is known for it, so a late fill after its cancel (ADR 0022),
	// an execution sent again and a later lifecycle report are read after a
	// restart as in the full run. A run restarted
	// from the decision restores it as this run's own attempt, released,
	// never as one an earlier run spent, with the executions reported so
	// far and named as it was, so the venue's later reports on it are read,
	// and its retirement as never sent frees the attempt only while no venue
	// has named it, as in the full run (ADR 0019, amended 2026-10-17,
	// reading 4; ADR 0026, point 2, "Boost", and point 5: recovered, never
	// recomputed). Omitted otherwise: an attempt that was withheld for a
	// person, or that ended with no venue order ID known, stays spent.
	Order *BoostRestoredSaleOrder `json:"order,omitempty"`
}

// BoostRestoredSaleOrder is the sell a restored sale attempt released
// (BoostRestoredSale.Order): its client order ID, side, shares, limit and
// what fired it, so a retirement of it re-arms the sale by its own trigger;
// the attempt it is, the first being 1, whose client order ID follows every
// retired attempt's (ADR 0026, point 2, "Order identity"); whether a venue
// has named it (a lifecycle report, an execution or a broker snapshot's
// open-order listing), so its retirement is refused after a restart as in
// the full run; its venue order ID once one is known, by which IBKR's
// listing of it with no client order ID echoed is matched; and the shares
// its fill reports executed so far with each execution, so a restarted run
// nets the sell by them and reads an execution sent again as a redelivery
// (ADR 0019, "Explained means", and amended 2026-10-15); and, once a venue
// ended it, its end, canceled or invalid, and when the earliest such report
// occurred, to the second, as the gate holds it (Status, EndedAt; version
// 9). A sale whose executions come to its released shares filled in full.
// Version 7 recorded the first four fields only; Side and Attempt are
// omitted in such a record, and every field after Attempt is omitted when
// unset.
type BoostRestoredSaleOrder struct {
	ClientOrderID string             `json:"client_order_id"`
	Side          string             `json:"side,omitempty"`
	Released      int64              `json:"released"`
	LimitPrice    float64            `json:"limit_price"`
	Trigger       string             `json:"trigger"`
	Attempt       int                `json:"attempt,omitempty"`
	Named         bool               `json:"named,omitempty"`
	VenueOrderID  string             `json:"venue_order_id,omitempty"`
	Filled        int64              `json:"filled,omitempty"`
	Fills         []BoostWatchedFill `json:"fills,omitempty"`
	Status        string             `json:"status,omitempty"`
	EndedAt       time.Time          `json:"ended_at,omitzero"`
}

// recordsVersion8 reports whether o records any field only version 8 or
// later records.
func (o BoostRestoredSaleOrder) recordsVersion8() bool {
	return o.Side != "" || o.Attempt != 0 || o.Named || o.VenueOrderID != "" || o.Filled != 0 || len(o.Fills) > 0 || o.recordsVersion9()
}

// recordsVersion9 reports whether o records a sale that is no longer live,
// which only version 9 records: ended by the venue, or filled in full.
func (o BoostRestoredSaleOrder) recordsVersion9() bool {
	return !o.Live()
}

// Live reports whether o is still live: not ended by the venue and not
// filled in full.
func (o BoostRestoredSaleOrder) Live() bool {
	return o.Status == "" && o.EndedAt.IsZero() && o.Filled < o.Released
}

// Watched is o as a watched order records the same sell, with its
// executions, its end and its venue order ID: the rule its release,
// executions and end are checked by, and the form a restarted run restores
// them from (BoostWatchedOrder.Fill).
func (o BoostRestoredSaleOrder) Watched() BoostWatchedOrder {
	return BoostWatchedOrder{ClientOrderID: o.ClientOrderID, Side: o.Side, Released: o.Released, LimitPrice: o.LimitPrice,
		Filled: o.Filled, Fills: o.Fills, Status: o.Status, EndedAt: o.EndedAt, VenueOrderID: o.VenueOrderID}
}

// Equal reports whether o and other record the same sell, each execution's
// instant the same however its UTC offset is written.
func (o BoostRestoredSaleOrder) Equal(other BoostRestoredSaleOrder) bool {
	return o.Trigger == other.Trigger && o.Attempt == other.Attempt && o.Named == other.Named && o.Watched().Equal(other.Watched())
}

// Equal reports whether s and o record the same sale attempt.
func (s *BoostRestoredSale) Equal(o *BoostRestoredSale) bool {
	if s == nil || o == nil {
		return s == o
	}
	if s.IncidentID != o.IncidentID || s.Attempted != o.Attempted || s.RetiredAttempts != o.RetiredAttempts || (s.Order == nil) != (o.Order == nil) {
		return false
	}
	return s.Order == nil || s.Order.Equal(*o.Order)
}

// carriesRelease reports whether o records any of the order as released.
func (o BoostUndecidedOrder) carriesRelease() bool {
	return o.Side != "" || o.Released != 0 || o.LimitPrice != 0 || o.VenueOrderID != ""
}

// BoostWatchedOrder is the order the latest decided Boost session
// released, while it is watched for an unfilled remainder (ADR 0026, point
// 2, "Boost", amended 2026-10-18): its client order ID, side, released
// shares and limit as released, the shares its fill reports executed so
// far and each of those executions, its terminal status once the venue
// reported it (canceled or invalid) and when that end occurred, and whether
// it is the one re-plan of an order the venue rejected. A run restarted
// from the decision watches it still and reads the venue's reports on it
// (ADR 0026, point 5): an execution it holds, sent again, is a redelivery
// that changes nothing (ADR 0019, "Explained means"), and a sell is netted
// from its executions and its end as any working sell (ADR 0019, amended
// 2026-10-15).
type BoostWatchedOrder struct {
	ClientOrderID  string             `json:"client_order_id"`
	Side           string             `json:"side"`
	Released       int64              `json:"released"`
	LimitPrice     float64            `json:"limit_price"`
	Filled         int64              `json:"filled,omitempty"`
	Fills          []BoostWatchedFill `json:"fills,omitempty"`
	Status         string             `json:"status,omitempty"`
	EndedAt        time.Time          `json:"ended_at,omitzero"`
	AfterRejection bool               `json:"after_rejection,omitempty"`
	// VenueOrderID is the venue's own ID for the order, from the first
	// lifecycle report or broker snapshot's open-order listing that names
	// it, omitted before one: a run restarted from the
	// decision matches IBKR's listing of the order by it when IBKR echoes no
	// client order ID (ADR 0026, point 5, amended 2026-10-18).
	VenueOrderID string `json:"venue_order_id,omitempty"`
}

// BoostWatchedFill is one execution of a watched order, as its fill report
// stated it; the order names its account, instrument and side.
type BoostWatchedFill struct {
	FillID     string    `json:"fill_id"`
	Quantity   int64     `json:"quantity"`
	Price      float64   `json:"price"`
	Commission float64   `json:"commission,omitempty"`
	FilledAt   time.Time `json:"filled_at"`
}

// IsZero reports whether no order is watched; encoding/json's omitzero
// reads it.
func (w BoostWatchedOrder) IsZero() bool { return w.Equal(BoostWatchedOrder{}) }

// Equal reports whether w and o record the same watched order, each instant
// the same however its UTC offset is written.
func (w BoostWatchedOrder) Equal(o BoostWatchedOrder) bool {
	return w.ClientOrderID == o.ClientOrderID && w.Side == o.Side && w.Released == o.Released && w.LimitPrice == o.LimitPrice &&
		w.Filled == o.Filled && w.Status == o.Status && w.EndedAt.Equal(o.EndedAt) && w.AfterRejection == o.AfterRejection &&
		w.VenueOrderID == o.VenueOrderID &&
		slices.EqualFunc(w.Fills, o.Fills, func(a, b BoostWatchedFill) bool {
			return a.FillID == b.FillID && a.Quantity == b.Quantity && a.Price == b.Price && a.Commission == b.Commission && a.FilledAt.Equal(b.FilledAt)
		})
}

// Fill returns the fill report the watched execution f restates, for an
// order of accountID in instrumentID.
func (w BoostWatchedOrder) Fill(accountID, instrumentID string, f BoostWatchedFill) TargetWeightFillPayload {
	return TargetWeightFillPayload{
		AccountID: accountID, ClientOrderID: w.ClientOrderID, FillID: f.FillID, InstrumentID: instrumentID,
		Side: w.Side, Quantity: f.Quantity, Price: f.Price, Commission: f.Commission, FilledAt: f.FilledAt,
	}
}

// IsZero reports whether b is empty, as a momentum decision's Boost figures
// are; encoding/json's omitzero reads it.
func (b BoostFigures) IsZero() bool {
	return b.InstrumentID == "" && b.Close == 0 && b.RSI == 0 && !b.RSIReady && b.TrendMean == 0 && !b.TrendMeanReady &&
		b.ExitMean == 0 && !b.ExitMeanReady && !b.LatchedBefore && !b.LatchedAfter && b.TargetInForce == 0 &&
		!b.TargetInForceSet && !b.ReplanPending && b.Replan == (BoostReplan{}) && len(b.GapSessions) == 0 &&
		b.ReplanWithheld == (BoostReplan{}) && b.Watched.IsZero() && b.VenueRejections == 0 &&
		len(b.RejectedInARow) == 0 && len(b.UndecidedOrders) == 0 && len(b.ClearedOrders) == 0 && len(b.RetiredOrders) == 0 && b.RestoredSale == nil && len(b.NamedOrders) == 0 && len(b.WorkingOrders) == 0 && len(b.RejectedWhileStopped) == 0 && b.RejectionsWhileStopped == 0 &&
		b.UndecidedBeforeRejection == 0 && b.NoVenue == ""
}

// validateGaps checks the gap sessions against the decided session's date:
// dates, strictly ascending, each before it.
func (b BoostFigures) validateGaps(session time.Time) []error {
	var errs []error
	day := session.UTC().Format(time.DateOnly)
	for i, s := range b.GapSessions {
		switch _, err := time.Parse(time.DateOnly, s); {
		case err != nil:
			errs = append(errs, fmt.Errorf("boost gap session %q is not a YYYY-MM-DD date", s))
		case i > 0 && s <= b.GapSessions[i-1]:
			errs = append(errs, fmt.Errorf("boost gap sessions must be strictly ascending; %s follows %s", s, b.GapSessions[i-1]))
		case s >= day:
			errs = append(errs, fmt.Errorf("boost gap session %s is not before the session %s whose bar the decision read", s, day))
		}
	}
	return errs
}

// The closed set of causes of a Boost re-plan (ADR 0026, point 2, "Boost",
// amended 2026-10-18).
const (
	// ReplanCauseRefused: the last planned order was refused and the refusal
	// left the engine Normal.
	ReplanCauseRefused = "refused"
	// ReplanCausePartFilled: the last released order was canceled (a DAY
	// order's expiry included) with fewer shares filled than released and
	// more than none.
	ReplanCausePartFilled = "part-filled"
	// ReplanCauseUnfilled: the last released order was canceled with
	// nothing filled.
	ReplanCauseUnfilled = "unfilled"
	// ReplanCauseRejected: the venue refused the last planned order before
	// it rested (invalid), a reason that may last, so it is re-planned once;
	// the rejection that makes two in a row is withheld
	// (BoostFigures.ReplanWithheld).
	ReplanCauseRejected = "rejected"
)

// BoostReplan is why a Boost decision plans the difference to the target in
// force though the target has not changed: the cause and, for an order that
// ended part-filled, unfilled or rejected, its client order ID and its
// filled and released shares.
type BoostReplan struct {
	Cause         string `json:"cause"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	Filled        int64  `json:"filled,omitempty"`
	Released      int64  `json:"released,omitempty"`
}

func (r BoostReplan) validate() []error {
	switch r.Cause {
	case "":
		if r != (BoostReplan{}) {
			return []error{errors.New("boost re-plan figures need a re-plan cause")}
		}
	case ReplanCauseRefused:
		if r.ClientOrderID != "" || r.Filled != 0 || r.Released != 0 {
			return []error{errors.New("a boost re-plan after a refusal names no order or shares")}
		}
	case ReplanCausePartFilled:
		var errs []error
		if r.ClientOrderID == "" {
			errs = append(errs, errors.New("a part-fill re-plan names its order"))
		}
		if r.Filled <= 0 || r.Filled >= r.Released {
			errs = append(errs, fmt.Errorf("a part-fill re-plan has filled shares between none and the %d released; got %d", r.Released, r.Filled))
		}
		return errs
	case ReplanCauseRejected:
		var errs []error
		if r.ClientOrderID == "" {
			errs = append(errs, errors.New("a rejected re-plan names its order"))
		}
		if r.Released <= 0 {
			errs = append(errs, errors.New("a rejected re-plan names the shares released"))
		}
		if r.Filled < 0 || r.Filled >= r.Released {
			errs = append(errs, fmt.Errorf("a rejected re-plan has fewer filled shares than the %d released; got %d", r.Released, r.Filled))
		}
		return errs
	case ReplanCauseUnfilled:
		var errs []error
		if r.ClientOrderID == "" {
			errs = append(errs, errors.New("an unfilled re-plan names its order"))
		}
		if r.Filled != 0 {
			errs = append(errs, fmt.Errorf("an unfilled re-plan has no filled shares; got %d", r.Filled))
		}
		if r.Released <= 0 {
			errs = append(errs, errors.New("an unfilled re-plan names the shares released"))
		}
		return errs
	default:
		return []error{fmt.Errorf("boost re-plan cause %q is not refused, part-filled, unfilled or rejected", r.Cause)}
	}
	return nil
}

// TargetsDecidedPayload is one account's target decision.
//
// A withheld decision carries no targets, but it still carries the Boost
// figures: the latched state advances with every bar (template.py:89-94), and
// a restart recovers it from the last decision of either outcome (ADR 0026,
// point 5).
type TargetsDecidedPayload struct {
	// CashBuffer is the IRA reserve in dollars at the decision snapshot's
	// net liquidation (ADR 0026, owner decision 2026-10-31). Historical
	// absence reads as zero and encodes unchanged (ADR 0015).
	CashBuffer    float64 `json:"cash_buffer,omitempty"`
	QuantityScale int64   `json:"quantity_scale,omitempty"`
	AccountID     string  `json:"account_id"`
	StrategyID    string  `json:"strategy_id"`
	// DecisionTime is when the decision was taken; SessionPeriodEnd is the
	// session whose closes it used. A decision never precedes its session.
	DecisionTime     time.Time `json:"decision_time"`
	SessionPeriodEnd time.Time `json:"session_period_end"`
	Outcome          string    `json:"outcome"`
	WithheldReason   string    `json:"withheld_reason,omitempty"`
	Detail           string    `json:"detail,omitempty"`
	// Targets is sorted strictly ascending by instrument.
	Targets []TargetWeight `json:"targets"`
	// WithheldPicks is sorted by instrument; only each named target's
	// unbought increase stays in cash for the month. Existing holdings are
	// unchanged by withholding (ADR 0026, amendment of 2026-10-29).
	WithheldPicks []WithheldPick `json:"withheld_picks,omitempty"`
	// Boost is set for StrategyID boost and Momentum for momentum; the other
	// is zero and omitted.
	Boost    BoostFigures    `json:"boost,omitzero"`
	Momentum MomentumFigures `json:"momentum,omitzero"`
	// Glide is set for StrategyID momentum, on either outcome.
	Glide GlideFigures `json:"glide,omitzero"`
}

// MomentumFigures are the figures behind a momentum decision, so the journal
// alone can check it (ADR 0026, point 1): the session whose closes it used,
// the risk-off inputs, the universe filter counts and every ranked name.
// A decision withheld before the ranking records no names.
type MomentumFigures struct {
	// CloseSession is the last session before the decision, YYYY-MM-DD.
	CloseSession string `json:"close_session"`
	// The trend test: TrendCloses counts the trend instrument's closes,
	// TrendClose and TrendMean are 0 when it was not computed.
	TrendInstrument string  `json:"trend_instrument"`
	TrendCloses     int     `json:"trend_closes"`
	TrendClose      float64 `json:"trend_close"`
	TrendMean       float64 `json:"trend_mean"`
	RiskOff         bool    `json:"risk_off"`
	// How many universe rows each filter kept: all rows, the listing type,
	// the price floor, and the members ranked.
	UniverseRows           int `json:"universe_rows"`
	UniverseListingType    int `json:"universe_listing_type"`
	UniverseAtOrAboveFloor int `json:"universe_at_or_above_floor"`
	UniverseMembers        int `json:"universe_members"`
	// Names holds every universe member in universe rank order.
	Names []MomentumName `json:"names"`
	// MinimumLot is set exactly when D7-L valued the picks: the momentum
	// rule decided a book under it, or withheld one as all-unaffordable. It
	// holds net liquidation V at the previous close and the momentum sleeve's
	// scale, 1 - b; a pick's target value is its weight x MomentumScale x V.
	// The figures stay when the run then withholds a decided book (missing
	// bars, a rebalance in progress); a decision the rule withheld for any
	// other reason, no-eligible-names included, compared no pick and has none.
	MinimumLot MinimumLotFigures `json:"minimum_lot,omitzero"`
}

// MinimumLotFigures are what D7-L valued each pick against.
type MinimumLotFigures struct {
	NetLiquidation float64 `json:"net_liquidation"`
	MomentumScale  float64 `json:"momentum_scale"`
}

// The closed set of MomentumName.Status values: what the rule did with a
// universe member (internal/targetweight/momentum's Status).
const (
	MomentumStatusScored       = "scored"
	MomentumStatusPicked       = "picked"
	MomentumStatusSectorCapped = "sector-capped"
	MomentumStatusBeyondTopN   = "beyond-top-n"
	// MomentumStatusNoSector: a name with no sector label, left out by
	// rules 1.12.0 and earlier. Rules 1.13.0 rank and pick such a name
	// uncapped (ADR 0026 point 8, the owner's decision of 2026-10-18) and
	// never record it; it stays in the schema so a decision recorded
	// before still reads.
	MomentumStatusNoSector       = "no-sector"
	MomentumStatusZeroVolatility = "zero-volatility"
	MomentumStatusShortHistory   = "short-history"
	MomentumStatusGap            = "gap"
	// MomentumStatusUnaffordable: D7-L skipped the pick, since one share at
	// its previous raw close exceeds its target value.
	MomentumStatusUnaffordable = "unaffordable"
	// MomentumStatusHeldOneLotShort: a pick the account holds whose one
	// share at its previous raw close exceeds its target value. D7-L keeps
	// it: it keeps its place, sector slot and weight, and the planner
	// places no order in it, no buy and no sell, at that rebalance (ADR
	// 0026, point 2, "D7-L", amended 2026-10-18).
	MomentumStatusHeldOneLotShort = "held-one-lot-short"
)

// MomentumName is one ranked universe member: its universe rank and dollar
// volume, sector label and source, what the rule did with it, its closes,
// score, volatility and score rank (0 when unscored), its weight (0 unless
// picked; a pick has 0 when the whole book is risk-off) and whether the backtest would have picked it (ADR 0026, point 8).
type MomentumName struct {
	InstrumentID      string  `json:"instrument_id"`
	UniverseRank      int     `json:"universe_rank"`
	DollarVolume      float64 `json:"dollar_volume"`
	Sector            string  `json:"sector"`
	SectorSource      string  `json:"sector_source"`
	Status            string  `json:"status"`
	Closes            int     `json:"closes"`
	Score             float64 `json:"score"`
	Volatility        float64 `json:"volatility"`
	ScoreRank         int     `json:"score_rank"`
	Weight            float64 `json:"weight"`
	BacktestWouldPick bool    `json:"backtest_would_pick"`
	// OneShare and TargetValue are set for an unaffordable name, and for a
	// held name kept one lot short: its previous raw close and the target
	// value it exceeded.
	OneShare    float64 `json:"one_share,omitzero"`
	TargetValue float64 `json:"target_value,omitzero"`
	// HeldQuantity, HeldValue and HeldShare are set for a held name kept one
	// lot short: the whole shares it keeps, their value at its previous raw
	// close, and that value as a fraction of the net liquidation it was
	// valued against, so the concentration a kept holding carries is on the
	// record (ADR 0026, point 2, "D7-L", amended 2026-10-18).
	HeldQuantity int64   `json:"held_quantity,omitzero"`
	HeldValue    float64 `json:"held_value,omitzero"`
	HeldShare    float64 `json:"held_share,omitzero"`
	// GapSessions are the sessions, YYYY-MM-DD and ascending, with no close
	// in the window the name was scored or refused on: a gap name's, or a
	// held name's scored across its gap (ADR 0026 point 5, amended
	// 2026-10-18). Omitted for a complete window.
	GapSessions []string `json:"gap_sessions,omitempty"`
}

// Validate checks the identity, the outcome and its reason, the targets and
// the Boost figures.
func (p TargetsDecidedPayload) Validate() error {
	if !isFinite(p.CashBuffer) || p.CashBuffer < 0 || p.CashBuffer != 0 && (p.StrategyID != TargetWeightStrategyMomentum || p.Outcome != TargetsOutcomeDecided) {
		return errors.New("cash buffer must be finite, nonnegative and only on a decided IRA")
	}
	// ADR 0026: minimum-lot substitutions belong only to D7-L; interpreting
	// their whole-share holding evidence as fractional ticks changes its value.
	if p.QuantityScale == FractionalQuantityScale && (p.Momentum.MinimumLot != (MinimumLotFigures{}) || p.WithheldReason == WithheldAllUnaffordable) {
		return errors.New("fractional decisions carry no D7-L minimum-lot evidence")
	}

	if p.QuantityScale != 0 && p.StrategyID != TargetWeightStrategyMomentum {
		return errors.New("fractional quantity decisions require momentum")
	}
	if err := quantityScaleError(p.QuantityScale); err != nil {
		return err
	}
	errs := p.withheldPickErrors()
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	boost := p.StrategyID == TargetWeightStrategyBoost
	switch p.StrategyID {
	case TargetWeightStrategyBoost:
		errs = append(errs, p.Boost.validate()...)
		errs = append(errs, p.Boost.validateGaps(p.SessionPeriodEnd)...)
		if !p.Momentum.isZero() {
			errs = append(errs, errors.New("a boost decision carries no momentum figures"))
		}
		if p.Glide != (GlideFigures{}) {
			errs = append(errs, errors.New("a boost decision carries no glide figures"))
		}
	case TargetWeightStrategyMomentum:
		errs = append(errs, p.Momentum.validate()...)
		errs = append(errs, p.Glide.validate(p.Momentum.CloseSession)...)
		if !p.Boost.IsZero() {
			errs = append(errs, errors.New("a momentum decision carries no boost figures"))
		}
	default:
		errs = append(errs, fmt.Errorf("strategy id %q is not a target-weight strategy this schema records", p.StrategyID))
	}
	decisionErrs := requiredTime("decision time", p.DecisionTime)
	sessionErrs := requiredTime("session period end", p.SessionPeriodEnd)
	errs = append(errs, decisionErrs...)
	errs = append(errs, sessionErrs...)
	if len(decisionErrs) == 0 && len(sessionErrs) == 0 && p.DecisionTime.Before(p.SessionPeriodEnd) {
		errs = append(errs, errors.New("decision time is before the session whose closes it used"))
	}
	switch p.Outcome {
	case TargetsOutcomeDecided:
		if p.WithheldReason != "" {
			errs = append(errs, errors.New("a decided outcome carries no withheld reason"))
		}
		if len(p.Targets) == 0 {
			errs = append(errs, errors.New("a decided outcome carries at least one target"))
		}
		if boost && !(p.Boost.Close > 0) {
			errs = append(errs, errors.New("a decided Boost outcome carries a positive close"))
		}
	case TargetsOutcomeWithheld:
		switch p.WithheldReason {
		case WithheldMissingBars, WithheldStaleAccountSnapshot, WithheldMissingUniverse,
			WithheldInsufficientTrendHistory, WithheldNoEligibleNames, WithheldRebalanceInProgress, WithheldAllUnaffordable,
			WithheldHistoriesMissing, WithheldLabelsMissing:
		default:
			errs = append(errs, fmt.Errorf("withheld reason %q is not recognised", p.WithheldReason))
		}
		if p.Detail == "" {
			errs = append(errs, errors.New("a withheld outcome needs its detail"))
		}
		if len(p.Targets) != 0 {
			errs = append(errs, errors.New("a withheld outcome carries no targets"))
		}
	default:
		errs = append(errs, fmt.Errorf("outcome %q is not decided or withheld", p.Outcome))
	}
	for i, target := range p.Targets {
		if target.InstrumentID == "" {
			errs = append(errs, fmt.Errorf("target %d: instrument id is required", i+1))
		} else if i > 0 && target.InstrumentID <= p.Targets[i-1].InstrumentID {
			errs = append(errs, fmt.Errorf("targets must be strictly ascending by instrument; %q follows %q", target.InstrumentID, p.Targets[i-1].InstrumentID))
		}
		if !isFinite(target.Weight) || target.Weight < 0 {
			errs = append(errs, fmt.Errorf("target %d: weight must be finite and not negative", i+1))
		}
		if boost && len(target.Shares) > 0 {
			errs = append(errs, fmt.Errorf("target %d: a boost target carries no sleeve shares", i+1))
		}
		if p.StrategyID == TargetWeightStrategyMomentum {
			errs = append(errs, target.validateShares(i+1)...)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid targets decided payload: %w", err)
	}
	return nil
}

// validateShares checks a momentum target's sleeve shares: at least one,
// momentum before bond, each sleeve once, each finite and not negative, and
// their sum in that order exactly the target's weight.
func (t TargetWeight) validateShares(n int) []error {
	if len(t.Shares) == 0 {
		return []error{fmt.Errorf("target %d: a momentum target needs its sleeve shares", n)}
	}
	var errs []error
	sum := 0.0
	for i, s := range t.Shares {
		switch {
		case s.Sleeve != SleeveMomentum && s.Sleeve != SleeveBond:
			errs = append(errs, fmt.Errorf("target %d: sleeve %q is not momentum or bond", n, s.Sleeve))
		case i > 0 && (s.Sleeve != SleeveBond || t.Shares[i-1].Sleeve != SleeveMomentum):
			errs = append(errs, fmt.Errorf("target %d: sleeve shares come momentum then bond, each once", n))
		}
		if !isFinite(s.Weight) || s.Weight < 0 {
			errs = append(errs, fmt.Errorf("target %d: sleeve share must be finite and not negative", n))
		}
		sum += s.Weight
	}
	if sum != t.Weight {
		errs = append(errs, fmt.Errorf("target %d: sleeve shares sum to %v, not the weight %v", n, sum, t.Weight))
	}
	return errs
}

func (g GlideFigures) validate(closeSession string) []error {
	var errs []error
	if g.AgeMonths < 1 {
		errs = append(errs, errors.New("glide age in months must be positive"))
	}
	if !isFinite(g.BondShare) || g.BondShare < 0 || g.BondShare > 1 {
		errs = append(errs, errors.New("glide bond share must lie in [0, 1]"))
	}
	if _, err := time.Parse(time.DateOnly, g.RebalanceSession); err != nil {
		errs = append(errs, fmt.Errorf("glide rebalance session %q is not a YYYY-MM-DD date", g.RebalanceSession))
	} else if g.RebalanceSession <= closeSession {
		errs = append(errs, errors.New("glide rebalance session must come after the close session"))
	}
	return errs
}

func (m MomentumFigures) isZero() bool {
	return m.CloseSession == "" && m.TrendInstrument == "" && m.TrendCloses == 0 && m.TrendClose == 0 &&
		m.TrendMean == 0 && !m.RiskOff && m.UniverseRows == 0 && m.UniverseListingType == 0 &&
		m.UniverseAtOrAboveFloor == 0 && m.UniverseMembers == 0 && len(m.Names) == 0 && m.MinimumLot == (MinimumLotFigures{})
}

func (m MomentumFigures) validate() []error {
	var errs []error
	if _, err := time.Parse(time.DateOnly, m.CloseSession); err != nil {
		errs = append(errs, fmt.Errorf("momentum close session %q is not a YYYY-MM-DD date", m.CloseSession))
	}
	if m.TrendInstrument == "" {
		errs = append(errs, errors.New("momentum trend instrument is required"))
	}
	for _, v := range []float64{m.TrendClose, m.TrendMean} {
		if !isFinite(v) || v < 0 {
			errs = append(errs, errors.New("momentum trend close and mean must be finite and not negative"))
		}
	}
	for _, n := range []int{m.TrendCloses, m.UniverseRows, m.UniverseListingType, m.UniverseAtOrAboveFloor, m.UniverseMembers} {
		if n < 0 {
			errs = append(errs, errors.New("momentum counts must not be negative"))
		}
	}
	if len(m.Names) != m.UniverseMembers && len(m.Names) != 0 {
		errs = append(errs, fmt.Errorf("momentum records %d names for %d universe members", len(m.Names), m.UniverseMembers))
	}
	lot := m.MinimumLot != (MinimumLotFigures{})
	if lot && (!isFinite(m.MinimumLot.NetLiquidation) || m.MinimumLot.NetLiquidation <= 0 ||
		!isFinite(m.MinimumLot.MomentumScale) || m.MinimumLot.MomentumScale <= 0 || m.MinimumLot.MomentumScale > 1) {
		errs = append(errs, errors.New("momentum minimum lot needs a positive net liquidation and a scale in (0, 1]"))
	}
	for i, n := range m.Names {
		errs = append(errs, n.validate(i+1)...)
		errs = append(errs, n.validateGaps(m.CloseSession)...)
		if (n.Status == MomentumStatusUnaffordable || n.Status == MomentumStatusHeldOneLotShort) && !lot {
			errs = append(errs, fmt.Errorf("momentum name %s: an unaffordable name needs the minimum lot figures", n.InstrumentID))
		}
		if n.Status == MomentumStatusHeldOneLotShort && lot {
			errs = append(errs, n.validateHoldingFigures(m.MinimumLot.NetLiquidation)...)
		}
	}
	return errs
}

func (n MomentumName) validate(rank int) []error {
	var errs []error
	if n.InstrumentID == "" {
		errs = append(errs, fmt.Errorf("momentum name %d: instrument id is required", rank))
	}
	if n.UniverseRank != rank {
		errs = append(errs, fmt.Errorf("momentum name %s: universe rank %d is not its place %d", n.InstrumentID, n.UniverseRank, rank))
	}
	switch n.Status {
	case MomentumStatusScored, MomentumStatusPicked, MomentumStatusSectorCapped, MomentumStatusBeyondTopN,
		MomentumStatusNoSector, MomentumStatusZeroVolatility, MomentumStatusShortHistory, MomentumStatusGap:
		if n.OneShare != 0 || n.TargetValue != 0 {
			errs = append(errs, fmt.Errorf("momentum name %s: only an unaffordable name records a one-share price and target value", n.InstrumentID))
		}
	case MomentumStatusUnaffordable, MomentumStatusHeldOneLotShort:
		if !isFinite(n.OneShare) || !isFinite(n.TargetValue) || n.TargetValue < 0 || !(n.OneShare > n.TargetValue) {
			errs = append(errs, fmt.Errorf("momentum name %s: an unaffordable name's one share exceeds its target value", n.InstrumentID))
		}
	default:
		errs = append(errs, fmt.Errorf("momentum name %s: status %q is not recognised", n.InstrumentID, n.Status))
	}
	if (n.Sector == "") != (n.SectorSource == "") {
		errs = append(errs, fmt.Errorf("momentum name %s: a sector label and its source come together", n.InstrumentID))
	}
	if n.Closes < 0 || n.ScoreRank < 0 {
		errs = append(errs, fmt.Errorf("momentum name %s: closes and score rank must not be negative", n.InstrumentID))
	}
	for _, v := range []float64{n.DollarVolume, n.Volatility, n.Weight} {
		if !isFinite(v) || v < 0 {
			errs = append(errs, fmt.Errorf("momentum name %s: dollar volume, volatility and weight must be finite and not negative", n.InstrumentID))
		}
	}
	if !isFinite(n.Score) {
		errs = append(errs, fmt.Errorf("momentum name %s: score must be finite", n.InstrumentID))
	}
	if n.Weight > 0 && n.Status != MomentumStatusPicked && n.Status != MomentumStatusHeldOneLotShort {
		errs = append(errs, fmt.Errorf("momentum name %s: only a picked name has a weight", n.InstrumentID))
	}
	errs = append(errs, n.validateHolding()...)
	return errs
}

// validateHolding checks the holding a held name kept one lot short records
// (ADR 0026, point 2, "D7-L", amended 2026-10-18): it keeps its weight, and
// its whole shares, their value and that value's share of net liquidation
// are all positive and finite. No other status records a holding.
func (n MomentumName) validateHolding() []error {
	if n.Status != MomentumStatusHeldOneLotShort {
		if n.HeldQuantity != 0 || n.HeldValue != 0 || n.HeldShare != 0 {
			return []error{fmt.Errorf("momentum name %s: only a held name kept one lot short records a holding", n.InstrumentID)}
		}
		return nil
	}
	var errs []error
	if !(n.Weight > 0) {
		errs = append(errs, fmt.Errorf("momentum name %s: a held name kept one lot short keeps its weight", n.InstrumentID))
	}
	if n.HeldQuantity < 1 || !isFinite(n.HeldValue) || !(n.HeldValue > 0) || !isFinite(n.HeldShare) || !(n.HeldShare > 0) {
		errs = append(errs, fmt.Errorf("momentum name %s: a held name kept one lot short records the holding it keeps, its value and its share of net liquidation", n.InstrumentID))
	}
	return errs
}

// validateHoldingFigures checks that a held name's recorded holding agrees
// with the record, so the concentration it reports is the one it holds (ADR
// 0026, point 2, "D7-L", amended 2026-10-18): its value is its shares at the
// one-share price and its share is that value over net liquidation, each
// recomputed exactly as the writer computes it, so no tolerance is needed.
func (n MomentumName) validateHoldingFigures(netLiquidation float64) []error {
	var errs []error
	if n.HeldValue != float64(float64(n.HeldQuantity)*n.OneShare) {
		errs = append(errs, fmt.Errorf("momentum name %s: held value %v is not its %d shares x one share %v", n.InstrumentID, n.HeldValue, n.HeldQuantity, n.OneShare))
	}
	if n.HeldShare != n.HeldValue/netLiquidation {
		errs = append(errs, fmt.Errorf("momentum name %s: held share %v is not its held value over net liquidation %v", n.InstrumentID, n.HeldShare, netLiquidation))
	}
	return errs
}

// ValidateWritten is the check on a decision being written now: Validate,
// a live sale back to 98% states its side and attempt (version 8), and a
// momentum name left out for a gap names the sessions it is missing (ADR
// 0026 point 5, amended 2026-10-18). Validate, which readers use, does
// not require it, so a record written before gap sessions existed is read.
func (p TargetsDecidedPayload) ValidateWritten() error {
	errs := []error{p.Validate()}
	// A live sale written now states its side and its attempt (version 8);
	// Validate reads a version 7 record, which states neither.
	if err := p.Boost.RestoredSale.versioned(); err != nil {
		errs = append(errs, err)
	}
	for _, n := range p.Momentum.Names {
		if n.Status == MomentumStatusGap && len(n.GapSessions) == 0 {
			errs = append(errs, fmt.Errorf("momentum name %s: a gap name names its missing sessions", n.InstrumentID))
		}
	}
	return errors.Join(errs...)
}

// ValidateSchema checks p as recorded at version: Validate; at version 8 or
// later, a sale back to 98% that states its side, a sell, and its attempt,
// the first being 1, which version 8 records; below version 8, no field of
// the sale that only version 8 or later records; and below version 9, no
// sale that ended or filled in full, which only version 9 records, so a
// record carrying a later version's field is refused. Below version 10,
// per-pick withholding is likewise refused (ADR 0015; ADR 0026,
// point 2, "Order identity"). A version above the current one, or none, is
// refused.
func (p TargetsDecidedPayload) ValidateSchema(version uint32) error {
	if err := quantitySchema(p.QuantityScale, version, TargetsDecidedFractionalVersion); err != nil {
		return err
	}
	if version == TargetsDecidedFractionalVersion {
		return p.Validate()
	}
	if version < TargetsDecidedSchemaVersion && len(p.WithheldPicks) > 0 {
		return fmt.Errorf("a version %d decision records withheld picks, which only version 10 and later record", version)
	}
	r := p.Boost.RestoredSale
	switch {
	case version == 0 || version > TargetsDecidedSchemaVersion:
		return fmt.Errorf("targets decided schema version %d is not between 1 and %d", version, TargetsDecidedSchemaVersion)
	case version < TargetsDecidedLiveSaleVersion:
		if r != nil && r.Order != nil && r.Order.recordsVersion8() {
			return fmt.Errorf("a version %d decision's restored sale records its side, attempt, naming, venue order ID, executions or end, which only version 8 and later record", version)
		}
	case version < TargetsDecidedEndedSaleVersion:
		if r != nil && r.Order != nil && r.Order.recordsVersion9() {
			return fmt.Errorf("a version %d decision's restored sale records a sale that ended or filled in full, which only version 9 records", version)
		}
		if err := r.versioned(); err != nil {
			return errors.Join(p.Validate(), err)
		}
	default:
		if err := r.versioned(); err != nil {
			return errors.Join(p.Validate(), err)
		}
	}
	return p.Validate()
}

// versioned refuses a live sale back to 98% that does not state its side, a
// sell, and its attempt, the first being 1, as every version 8 record does
// (ADR 0026, point 2, "Order identity"). A record with no live sale states
// neither, and passes.
func (s *BoostRestoredSale) versioned() error {
	if s == nil || s.Order == nil || s.Order.Side == OrderSideSell && s.Order.Attempt >= 1 {
		return nil
	}
	return fmt.Errorf("a restored sale's order at version 8 records its side, a sell, and its attempt, the first being 1; got %q and %d", s.Order.Side, s.Order.Attempt)
}

// validateGaps checks a name's gap sessions: dates, strictly ascending, on
// or before the close session; a short history, refused before any window
// is read, names none. A gap name may name none here, because a record
// written before gap sessions existed names none and its log must still be
// read; ValidateWritten holds a record being written now to the full rule.
func (n MomentumName) validateGaps(closeSession string) []error {
	var errs []error
	if n.Status == MomentumStatusShortHistory && len(n.GapSessions) > 0 {
		errs = append(errs, fmt.Errorf("momentum name %s: a short history records no gap sessions", n.InstrumentID))
	}
	for i, s := range n.GapSessions {
		switch _, err := time.Parse(time.DateOnly, s); {
		case err != nil:
			errs = append(errs, fmt.Errorf("momentum name %s: gap session %q is not a YYYY-MM-DD date", n.InstrumentID, s))
		case i > 0 && s <= n.GapSessions[i-1]:
			errs = append(errs, fmt.Errorf("momentum name %s: gap sessions must be strictly ascending; %s follows %s", n.InstrumentID, s, n.GapSessions[i-1]))
		case s > closeSession:
			errs = append(errs, fmt.Errorf("momentum name %s: gap session %s is after the close session %s", n.InstrumentID, s, closeSession))
		}
	}
	return errs
}

func (b BoostFigures) validate() []error {
	var errs []error
	if b.InstrumentID == "" {
		errs = append(errs, errors.New("boost instrument id is required"))
	}
	if !isFinite(b.Close) || b.Close < 0 {
		errs = append(errs, errors.New("boost close must be finite and not negative"))
	}
	for _, f := range []struct {
		name  string
		value float64
		ready bool
	}{{"rsi", b.RSI, b.RSIReady}, {"trend mean", b.TrendMean, b.TrendMeanReady}, {"exit mean", b.ExitMean, b.ExitMeanReady}} {
		switch {
		case !isFinite(f.value) || f.value < 0:
			errs = append(errs, fmt.Errorf("boost %s must be finite and not negative", f.name))
		case !f.ready && f.value != 0:
			errs = append(errs, fmt.Errorf("boost %s carries a value but is not ready", f.name))
		}
	}
	switch {
	case !isFinite(b.TargetInForce) || b.TargetInForce < 0:
		errs = append(errs, errors.New("boost target in force must be finite and not negative"))
	case !b.TargetInForceSet && b.TargetInForce != 0:
		errs = append(errs, errors.New("boost target in force carries a value but none is set"))
	}
	errs = append(errs, b.Replan.validate()...)
	errs = append(errs, b.Watched.validate()...)
	switch {
	case b.VenueRejections < 0:
		errs = append(errs, fmt.Errorf("boost venue rejections in a row are never below none; got %d", b.VenueRejections))
	case b.VenueRejections > BoostVenueRejectionLimit:
		errs = append(errs, fmt.Errorf("boost venue rejections in a row stay at two once reached; got %d", b.VenueRejections))
	}
	errs = append(errs, b.validateRejections()...)
	switch {
	case b.RejectionsWhileStopped < 0, b.RejectionsWhileStopped > 0 && b.VenueRejections != BoostVenueRejectionLimit,
		len(b.RejectedWhileStopped) > b.RejectionsWhileStopped:
		errs = append(errs, fmt.Errorf("boost rejections while buys are stopped are counted only once two in a row stopped them, at least as many as recorded; got %d counted, %d recorded, %d in a row",
			b.RejectionsWhileStopped, len(b.RejectedWhileStopped), b.VenueRejections))
	}
	for _, r := range b.RejectedWhileStopped {
		if r.Cause != ReplanCauseRejected {
			errs = append(errs, fmt.Errorf("a boost order rejected while buys are stopped is a rejection's; got cause %q", r.Cause))
			continue
		}
		errs = append(errs, r.validate()...)
	}
	if w := b.ReplanWithheld; w != (BoostReplan{}) {
		if w.Cause != ReplanCauseRejected {
			errs = append(errs, fmt.Errorf("a withheld boost re-plan is a rejection's; got cause %q", w.Cause))
		} else {
			errs = append(errs, w.validate()...)
		}
	}
	return errs
}

// validateRejections checks the orders the count names and the orders not
// yet ruled on: the count names exactly as many orders as it counts, each
// once; an undecided order is named once and is not among them; each but
// the watched one records its release, the watched one does not and, being
// the latest released, comes last, and it has had no execution and no end,
// either of which is the venue's word on it.
func (b BoostFigures) validateRejections() []error {
	var errs []error
	if len(b.RejectedInARow) != b.VenueRejections {
		errs = append(errs, fmt.Errorf("boost names %d orders rejected in a row; the count is %d", len(b.RejectedInARow), b.VenueRejections))
	}
	rejected := map[string]bool{}
	for _, o := range b.RejectedInARow {
		switch {
		case o.ClientOrderID == "":
			errs = append(errs, errors.New("a boost order rejected in a row names its client order ID"))
		case rejected[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost names order %s rejected in a row twice", o.ClientOrderID))
		}
		rejected[o.ClientOrderID] = true
	}
	seen := map[string]bool{}
	for i, o := range b.UndecidedOrders {
		switch {
		case o.ClientOrderID == "":
			errs = append(errs, errors.New("an undecided boost order names its client order ID"))
		case seen[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost names undecided order %s twice", o.ClientOrderID))
		case rejected[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost order %s is both rejected and undecided", o.ClientOrderID))
		}
		seen[o.ClientOrderID] = true
		watched := o.ClientOrderID != "" && o.ClientOrderID == b.Watched.ClientOrderID
		switch {
		case watched && i != len(b.UndecidedOrders)-1:
			errs = append(errs, fmt.Errorf("the watched order %s is the last undecided boost order, the latest released", o.ClientOrderID))
		case watched && (b.Watched.Filled > 0 || b.Watched.Status != ""):
			errs = append(errs, fmt.Errorf("the watched order %s has executed or ended, so the venue has ruled on it; it is not undecided", o.ClientOrderID))
		}
		switch {
		case watched:
			if o.carriesRelease() {
				errs = append(errs, fmt.Errorf("undecided boost order %s is the watched order, whose release is recorded as watched", o.ClientOrderID))
			}
		case o.Side != OrderSideBuy && o.Side != OrderSideSell, o.Released <= 0, !isFinite(o.LimitPrice), o.LimitPrice <= 0:
			errs = append(errs, fmt.Errorf("undecided boost order %s, not the watched one, records its side, released shares and positive limit; got %q %d at %v",
				o.ClientOrderID, o.Side, o.Released, o.LimitPrice))
		}
	}
	// A cleared order is one the owner settled as never sent: named once, not
	// also undecided, its release as released and no venue order ID, since
	// no venue named it.
	cleared := map[string]bool{}
	for _, o := range b.ClearedOrders {
		switch {
		case o.ClientOrderID == "":
			errs = append(errs, errors.New("a cleared boost order names its client order ID"))
		case cleared[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost names cleared order %s twice", o.ClientOrderID))
		case seen[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost order %s is both cleared and undecided", o.ClientOrderID))
		}
		cleared[o.ClientOrderID] = true
		switch {
		case o.Side != OrderSideBuy && o.Side != OrderSideSell, o.Released <= 0, !isFinite(o.LimitPrice), o.LimitPrice <= 0:
			errs = append(errs, fmt.Errorf("cleared boost order %s records its side, released shares and positive limit; got %q %d at %v",
				o.ClientOrderID, o.Side, o.Released, o.LimitPrice))
		case o.VenueOrderID != "":
			errs = append(errs, fmt.Errorf("cleared boost order %s names venue order %s; no venue named an order the adapter never sent", o.ClientOrderID, o.VenueOrderID))
		}
	}
	// The orders released before the latest rejection counted are the first
	// of those listed; the watched order, the latest released, is never one.
	older := len(b.UndecidedOrders)
	if older > 0 && b.UndecidedOrders[older-1].ClientOrderID == b.Watched.ClientOrderID {
		older--
	}
	switch k := b.UndecidedBeforeRejection; {
	case k < 0 || k > older:
		errs = append(errs, fmt.Errorf("boost counts %d undecided orders released before the latest rejection; between none and %d are listed before the watched order", k, older))
	case k > 0 && b.VenueRejections == 0:
		errs = append(errs, fmt.Errorf("boost counts %d undecided orders released before the latest rejection, but counts no rejection", k))
	}
	// A dry-run's orders reach no venue, so none awaits a verdict and none
	// is counted (ADR 0026, point 3, "Mode").
	switch {
	case b.NoVenue == "":
	case b.NoVenue != TradingModeDryRun:
		errs = append(errs, fmt.Errorf("boost records no venue only for a dry-run, not %q", b.NoVenue))
	case len(b.UndecidedOrders) > 0 || b.VenueRejections > 0 || len(b.ClearedOrders) > 0 || len(b.RetiredOrders) > 0 || len(b.WorkingOrders) > 0 || len(b.NamedOrders) > 0:
		errs = append(errs, errors.New("a dry-run boost decision lists no undecided, cleared, retired, named or working order and counts no rejection: no venue receives its orders, so there is nothing to rule on, clear or retire"))
	}
	if r := b.RestoredSale; r != nil && (r.IncidentID == "" || r.RetiredAttempts < 0) {
		errs = append(errs, fmt.Errorf("a restored sale attempt names its incident and counts no fewer than no retired attempts; got %q and %d", r.IncidentID, r.RetiredAttempts))
	}
	if r := b.RestoredSale; r != nil && r.Order != nil {
		switch o := r.Order; {
		case !r.Attempted:
			errs = append(errs, errors.New("a restored sale attempt that records its released order is attempted"))
		case o.ClientOrderID == "" || o.Released <= 0 || !(o.LimitPrice > 0):
			errs = append(errs, fmt.Errorf("a restored sale's order names its client order ID, shares released and limit; got %q, %d, %v", o.ClientOrderID, o.Released, o.LimitPrice))
		case o.Trigger != SaleTriggerExit && o.Trigger != SaleTriggerMarginUrgent:
			errs = append(errs, fmt.Errorf("a restored sale's order was fired by exit or margin-urgent, not %q", o.Trigger))
		case o.Side != "" && o.Side != OrderSideSell:
			errs = append(errs, fmt.Errorf("a restored sale's order is a sell, not a %q", o.Side))
		case o.Attempt < 0:
			errs = append(errs, fmt.Errorf("a restored sale's order is attempt %d; the first attempt is 1", o.Attempt))
		case (o.Filled > 0 || o.VenueOrderID != "") && !o.Named:
			errs = append(errs, fmt.Errorf("a restored sale's order with %d filled and venue order %q is named: only a venue's report or listing gives either", o.Filled, o.VenueOrderID))
		case !o.Live() && o.VenueOrderID == "":
			errs = append(errs, fmt.Errorf("a restored sale's order %s that ended or filled in full is recorded only under the venue order ID a venue gave it", o.ClientOrderID))
		default:
			// Its release, executions and end by the watched order's rule,
			// the fills summing to the filled shares; a sale no longer live
			// may have filled in full. A version 7 record states no side; it
			// is a sell all the same.
			w := o.Watched()
			w.Side = OrderSideSell
			for _, err := range w.check(true) {
				errs = append(errs, fmt.Errorf("a restored sale's order: %w", err))
			}
			// Its end is the gate's, to the second (Status, EndedAt), so
			// recovery restores it as recorded (ADR 0026); a fraction of a
			// second would be truncated away, past the zero time to none.
			if !o.EndedAt.IsZero() && o.EndedAt.Nanosecond() != 0 {
				errs = append(errs, errors.New("a restored sale's order's ended_at must be a whole second; the gate holds it to the second"))
			}
		}
	}
	// A retired order is one retired as never sent: named once, in no list
	// but Watched's of the orders still awaited or working, its release as
	// released and when it was retired.
	retired := map[string]bool{}
	for _, o := range b.RetiredOrders {
		switch {
		case o.ClientOrderID == "":
			errs = append(errs, errors.New("a retired boost order names its client order ID"))
		case retired[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost names retired order %s twice", o.ClientOrderID))
		case seen[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost order %s is both retired and undecided", o.ClientOrderID))
		case cleared[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost order %s is both retired and cleared", o.ClientOrderID))
		case rejected[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost order %s is both retired and rejected", o.ClientOrderID))
		}
		retired[o.ClientOrderID] = true
		switch {
		case o.Side != OrderSideBuy && o.Side != OrderSideSell, o.Released <= 0, !isFinite(o.LimitPrice), o.LimitPrice <= 0:
			errs = append(errs, fmt.Errorf("retired boost order %s records its side, released shares and positive limit; got %q %d at %v",
				o.ClientOrderID, o.Side, o.Released, o.LimitPrice))
		case o.RetiredAt.IsZero():
			errs = append(errs, fmt.Errorf("retired boost order %s records when it was retired", o.ClientOrderID))
		case !writableTime(o.RetiredAt):
			errs = append(errs, fmt.Errorf("retired boost order %s: retired_at %v cannot be written as RFC 3339", o.ClientOrderID, o.RetiredAt))
		}
		// A retired sale back to 98% records its tag and the attempt
		// retired, the first being 1; any other order records neither.
		switch {
		case (o.SaleTag == "") != (o.SaleAttempt == 0):
			errs = append(errs, fmt.Errorf("retired boost order %s records a sale's tag and the attempt retired together, or neither; got %q and attempt %d", o.ClientOrderID, o.SaleTag, o.SaleAttempt))
		case o.SaleAttempt < 0:
			errs = append(errs, fmt.Errorf("retired boost order %s records sale attempt %d; the first attempt is 1", o.ClientOrderID, o.SaleAttempt))
		}
	}
	if w := b.Watched; retired[w.ClientOrderID] && (w.Filled > 0 || w.Status != OrderLifecycleStatusCanceled) {
		errs = append(errs, fmt.Errorf("the watched order %s is retired, so it ended canceled with nothing filled; got %q with %d filled", w.ClientOrderID, w.Status, w.Filled))
	}
	// A working order is an accepted one that works besides the watched
	// order: named, still working, named once, and in no other list. An
	// execution is of one order, so no fill id is recorded under two of
	// them, the watched order and the working ones (ADR 0019, "Explained
	// means"; fail closed, AGENTS.md).
	working := map[string]bool{}
	fillOf := map[string]string{}
	for _, f := range b.Watched.Fills {
		fillOf[f.FillID] = b.Watched.ClientOrderID
	}
	for _, o := range b.WorkingOrders {
		errs = append(errs, o.validate()...)
		for _, f := range o.Fills {
			if other, ok := fillOf[f.FillID]; ok && other != o.ClientOrderID {
				errs = append(errs, fmt.Errorf("boost records fill id %s on two orders, %s and %s", f.FillID, other, o.ClientOrderID))
			}
			fillOf[f.FillID] = o.ClientOrderID
		}
		switch {
		case o.ClientOrderID == "":
			errs = append(errs, errors.New("a working boost order names its client order ID"))
		case o.Status != "":
			errs = append(errs, fmt.Errorf("working boost order %s has ended; a working order has not", o.ClientOrderID))
		case o.ClientOrderID == b.Watched.ClientOrderID:
			errs = append(errs, fmt.Errorf("working boost order %s is the watched order, recorded as watched", o.ClientOrderID))
		case working[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost names working order %s twice", o.ClientOrderID))
		case seen[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost order %s is both working and undecided", o.ClientOrderID))
		case cleared[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost order %s is both working and cleared", o.ClientOrderID))
		case rejected[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost order %s is both working and rejected", o.ClientOrderID))
		case retired[o.ClientOrderID]:
			errs = append(errs, fmt.Errorf("boost order %s is both working and retired", o.ClientOrderID))
		}
		working[o.ClientOrderID] = true
	}
	// A venue order ID is the venue's ID for one order: a restarted run
	// matches IBKR's listing with no client order ID echoed by it, and an ID
	// two orders carry would match the wrong one (ADR 0026, point 5, amended
	// 2026-10-18; fail closed, AGENTS.md).
	venueOf := map[string]string{}
	named := func(client, venueID string) {
		if venueID == "" {
			return
		}
		if other, ok := venueOf[venueID]; ok && other != client {
			errs = append(errs, fmt.Errorf("boost orders %s and %s both name venue order %s; a venue order ID names one order", other, client, venueID))
		}
		venueOf[venueID] = client
	}
	named(b.Watched.ClientOrderID, b.Watched.VenueOrderID)
	for _, o := range b.UndecidedOrders {
		named(o.ClientOrderID, o.VenueOrderID)
	}
	for _, o := range b.WorkingOrders {
		named(o.ClientOrderID, o.VenueOrderID)
	}
	// The run's own live sale is recorded once, as the restored sale's
	// order, and in no other list; no execution or venue order ID of it is
	// another order's.
	if r := b.RestoredSale; r != nil && r.Order != nil && r.Order.ClientOrderID != "" {
		o := r.Order
		if id := o.ClientOrderID; id == b.Watched.ClientOrderID || seen[id] || cleared[id] || rejected[id] || retired[id] || working[id] {
			errs = append(errs, fmt.Errorf("restored sale %s is also the watched, an undecided, a cleared, a rejected, a retired or a working order; the run's own live sale is recorded once", id))
		}
		for _, f := range o.Fills {
			if other, ok := fillOf[f.FillID]; ok && other != o.ClientOrderID {
				errs = append(errs, fmt.Errorf("boost records fill id %s on two orders, %s and %s", f.FillID, other, o.ClientOrderID))
			}
		}
		named(o.ClientOrderID, o.VenueOrderID)
	}
	// An order a venue named is one the decision restores as released: the
	// watched order, an undecided or a working one, named once, ascending.
	for i, id := range b.NamedOrders {
		switch {
		case i > 0 && id <= b.NamedOrders[i-1]:
			errs = append(errs, fmt.Errorf("boost named orders must be strictly ascending; %q follows %q", id, b.NamedOrders[i-1]))
		case id == "" || id != b.Watched.ClientOrderID && !seen[id] && !working[id]:
			errs = append(errs, fmt.Errorf("boost named order %q is not the watched, an undecided or a working order", id))
		}
	}
	return errs
}

func (w BoostWatchedOrder) validate() []error { return w.check(false) }

// check is validate, with the shares filled allowed to reach those released
// when filledInFull: the run's own sale back to 98% is recorded filled in
// full, where a watched or working order filled in full is recorded no
// more.
func (w BoostWatchedOrder) check(filledInFull bool) []error {
	if w.IsZero() {
		return nil
	}
	var errs []error
	if w.ClientOrderID == "" {
		errs = append(errs, errors.New("a watched boost order names its client order ID"))
	}
	if w.Side != OrderSideBuy && w.Side != OrderSideSell {
		errs = append(errs, fmt.Errorf("a watched boost order is a buy or a sell; got %q", w.Side))
	}
	if !isFinite(w.LimitPrice) || w.LimitPrice <= 0 {
		errs = append(errs, errors.New("a watched boost order has a positive limit price"))
	}
	if w.Released <= 0 || w.Filled < 0 || w.Filled > w.Released || w.Filled == w.Released && !filledInFull {
		errs = append(errs, fmt.Errorf("a watched boost order has released shares and fewer filled; got %d of %d", w.Filled, w.Released))
	}
	switch w.Status {
	case "", OrderLifecycleStatusCanceled, OrderLifecycleStatusInvalid:
	default:
		errs = append(errs, fmt.Errorf("a watched boost order's status is canceled, invalid or none; got %q", w.Status))
	}
	switch {
	case (w.Status == "") != w.EndedAt.IsZero():
		errs = append(errs, errors.New("a watched boost order has ended_at exactly when it has a status: when the venue ended it"))
	case !w.EndedAt.IsZero() && !writableTime(w.EndedAt):
		errs = append(errs, errors.New("a watched boost order's ended_at cannot be written as RFC 3339"))
	}
	var sum int64
	overflow := false
	seen := map[string]bool{}
	for _, f := range w.Fills {
		switch {
		case f.FillID == "":
			errs = append(errs, errors.New("a watched boost order's fill names its fill id"))
		case seen[f.FillID]:
			errs = append(errs, fmt.Errorf("a watched boost order records fill id %s twice", f.FillID))
		}
		seen[f.FillID] = true
		if f.Quantity <= 0 {
			errs = append(errs, fmt.Errorf("a watched boost order's fill %s has a positive quantity; got %d", f.FillID, f.Quantity))
		}
		if !isFinite(f.Price) || f.Price <= 0 {
			errs = append(errs, fmt.Errorf("a watched boost order's fill %s has a finite, positive price", f.FillID))
		}
		if !isFinite(f.Commission) || f.Commission < 0 {
			errs = append(errs, fmt.Errorf("a watched boost order's fill %s has a finite commission, not negative", f.FillID))
		}
		errs = append(errs, wholeSecondTime("a watched boost order's fill "+f.FillID+" filled at", f.FilledAt)...)
		// Each fill is checked against the shares released before it is
		// added, so the sum stays within them and can never wrap (fail
		// closed, AGENTS.md).
		switch {
		case f.Quantity <= 0:
		case f.Quantity > max(w.Released, 0)-sum:
			errs = append(errs, fmt.Errorf("a watched boost order's fills come to more shares than released, %d", w.Released))
			overflow = true
		default:
			sum += f.Quantity
		}
	}
	if !overflow && sum != w.Filled {
		errs = append(errs, fmt.Errorf("a watched boost order's fills sum to %d shares; it records %d filled", sum, w.Filled))
	}
	return errs
}

// OrderPlannedEventType identifies one order the strategy-independent planner
// derived from a target decision (ADR 0026, point 2).
const OrderPlannedEventType = "strategy.order.planned"

// The schema versions of OrderPlannedPayload (ADR 0015). A writer journals
// each payload at SchemaVersion, the lowest version that holds it.
//
//   - Version 1 is an ordinary planned order, with no Sale.
//   - Version 2 adds Sale, the figures of ADR 0019's sale back to 98%
//     (amendments of 2026-10-15 and 2026-10-17). It reads every version-1 field.
const (
	OrderPlannedSchemaVersion uint32 = 1
	OrderPlannedSafetyVersion uint32 = 2
)

// Order sides, which are also the planner's phases (ADR 0026, point 2,
// "Order identity").
const (
	OrderSideBuy  = "buy"
	OrderSideSell = "sell"
)

// OrderTypeLimit is the only order type planned: a limit at the price band's
// edge, marketable but never filled worse than the band (ADR 0026, point 2).
const OrderTypeLimit = "limit"

// OrderPlannedPayload is one planned order and the figures it was sized from:
// net liquidation V, the holding q and the raw reference close p, all as of
// the previous close (ADR 0026, point 2, "Valuation basis").
type OrderPlannedPayload struct {
	// RoundingSlack is the per-buy execution reserve under ADR 0026,
	// "Per-execution rounding reserve", in dollars, separate from fees.
	RoundingSlack       float64 `json:"rounding_slack,omitzero"`
	FractionalIncrement string  `json:"fractional_increment,omitzero"`
	QuantityScale       int64   `json:"quantity_scale,omitempty"`
	AccountID           string  `json:"account_id"`
	// Tag is the ID of the strategy.targets.decided envelope this order
	// serves; ClientOrderID derives from it, the instrument and the phase.
	Tag           string `json:"tag"`
	ClientOrderID string `json:"client_order_id"`
	InstrumentID  string `json:"instrument_id"`
	Side          string `json:"side"`
	Phase         string `json:"phase"`
	OrderType     string `json:"order_type"`
	// Quantity is positive in QuantityScale units; Side gives the sign
	// (ADR 0026, D7-F). HeldQuantity uses the same units.
	Quantity       int64   `json:"quantity"`
	LimitPrice     float64 `json:"limit_price"`
	TargetWeight   float64 `json:"target_weight"`
	NetLiquidation float64 `json:"net_liquidation"`
	HeldQuantity   int64   `json:"held_quantity"`
	ReferencePrice float64 `json:"reference_price"`
	PriceBand      float64 `json:"price_band"`
	// Sale is set only on a sale back to 98% of net liquidation (ADR 0019,
	// amended 2026-10-15 and 2026-10-16; ADR 0026, point 4). It is omitted
	// otherwise, so an ordinary order's bytes do not change.
	Sale SaleFigures `json:"sale,omitzero"`
}

// SchemaVersion returns the lowest schema version that holds p.
func (p OrderPlannedPayload) SchemaVersion() uint32 {
	if p.QuantityScale != 0 {
		return OrderPlannedFractionalVersion
	}
	if p.Sale.set() {
		return OrderPlannedSafetyVersion
	}
	return OrderPlannedSchemaVersion
}

// ValidateSchema checks that p is one schemaVersion holds, then validates it.
func (p OrderPlannedPayload) ValidateSchema(schemaVersion uint32) error {
	if err := quantitySchema(p.QuantityScale, schemaVersion, OrderPlannedFractionalVersion); err != nil {
		return err
	}
	if schemaVersion == OrderPlannedFractionalVersion {
		return p.Validate()
	}
	if err := checkSafetyVersion(OrderPlannedEventType, schemaVersion, p.SchemaVersion(), OrderPlannedSafetyVersion); err != nil {
		return err
	}
	return p.Validate()
}

// OrderPlannedVersions, OrderReleasedVersions and OrderRefusedVersions
// return every schema version of each record this build reads: 1 to the
// latest, since each version reads every earlier one's fields. Every reader
// of the log reads by these lists, so none falls behind the version the
// planner or the gate writes (ADR 0015). Each call returns a new slice.
func OrderPlannedVersions() []uint32 {
	return []uint32{OrderPlannedSchemaVersion, OrderPlannedSafetyVersion, OrderPlannedFractionalVersion}
}

// OrderReleasedVersions: see OrderPlannedVersions. Version 1 checked the
// order's own instrument's exposure, and is read as it was written.
func OrderReleasedVersions() []uint32 {
	return []uint32{1, OrderReleasedSchemaVersion, OrderReleasedSafetyVersion, OrderReleasedFractionalVersion}
}

// OrderRefusedVersions: see OrderPlannedVersions.
func OrderRefusedVersions() []uint32 {
	return []uint32{1, OrderRefusedSchemaVersion, OrderRefusedSafetyVersion, OrderRefusedPendingCashVersion, OrderRefusedFractionalVersion}
}

// checkSafetyVersion refuses a schema version outside 1 to latest, and one
// below need, the earliest version that holds the payload.
func checkSafetyVersion(eventType string, schemaVersion, need, latest uint32) error {
	switch {
	case schemaVersion < 1 || schemaVersion > latest:
		return fmt.Errorf("%s: schema version %d is not recognised", eventType, schemaVersion)
	case schemaVersion < need:
		return fmt.Errorf("%s: schema version %d cannot hold the payload's sale figures, cash or broker figures or refusal reason; it needs version %d", eventType, schemaVersion, need)
	}
	return nil
}

// Validate checks identity, side, type, quantity and prices, and that the
// limit lies on the side of the reference price its order type requires: a
// buy's limit at or above it, a sell's at or below. A sale back to 98% also
// checks its sizing figures.
func (p OrderPlannedPayload) Validate() error {
	if p.RoundingSlack != 0 && (!ValidRoundingSlack(p.RoundingSlack) || p.QuantityScale != FractionalQuantityScale || p.Side != OrderSideBuy) {
		return errors.New("rounding slack requires a fractional buy and [0.01, 1.00] dollars")
	}
	if p.QuantityScale == FractionalQuantityScale && p.Sale.set() {
		return errors.New("fractional IRA orders carry no margin-account sale figures")
	}
	if p.QuantityScale != 0 {
		if step, err := FractionalIncrementTicks(p.FractionalIncrement); err != nil {
			return err
		} else if p.Quantity%step != 0 {
			return errors.New("planned quantity must be a multiple of the fractional increment")
		}
	} else if p.FractionalIncrement != "" {
		return errors.New("fractional increment requires fractional quantities")
	}
	if err := quantityScaleError(p.QuantityScale); err != nil {
		return err
	}
	var errs []error
	errs = append(errs, orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)...)
	if p.Tag == "" {
		errs = append(errs, errors.New("tag is required: an order serves a journalled decision"))
	}
	if p.Phase != OrderSideBuy && p.Phase != OrderSideSell {
		errs = append(errs, fmt.Errorf("phase %q is not buy or sell", p.Phase))
	}
	if p.OrderType != OrderTypeLimit {
		errs = append(errs, fmt.Errorf("order type %q is not %q", p.OrderType, OrderTypeLimit))
	}
	if !isFinite(p.TargetWeight) || p.TargetWeight < 0 {
		errs = append(errs, errors.New("target weight must be finite and not negative"))
	}
	if !isFinite(p.NetLiquidation) || p.NetLiquidation <= 0 {
		errs = append(errs, errors.New("net liquidation must be finite and positive"))
	}
	if p.HeldQuantity < 0 {
		errs = append(errs, errors.New("held quantity must not be negative: the planner never plans against a short"))
	}
	referenceOK := isFinite(p.ReferencePrice) && p.ReferencePrice > 0
	if !referenceOK {
		errs = append(errs, errors.New("reference price must be finite and positive"))
	}
	if !isFinite(p.PriceBand) || p.PriceBand <= 0 || p.PriceBand >= 1 {
		errs = append(errs, errors.New("price band must lie strictly between 0 and 1"))
	}
	if referenceOK && isFinite(p.LimitPrice) {
		if p.Side == OrderSideBuy && p.LimitPrice < p.ReferencePrice {
			errs = append(errs, errors.New("a buy's limit must not be below its reference price"))
		}
		if p.Side == OrderSideSell && p.LimitPrice > p.ReferencePrice {
			errs = append(errs, errors.New("a sell's limit must not be above its reference price"))
		}
	}
	if p.Sale.set() {
		errs = append(errs, p.Sale.validate(p)...)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order planned payload: %w", err)
	}
	return nil
}

func orderIdentity(accountID, clientOrderID, instrumentID, side string, quantity int64, limit float64) []error {
	var errs []error
	if accountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if clientOrderID == "" {
		errs = append(errs, errors.New("client order id is required"))
	}
	if instrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if side != OrderSideBuy && side != OrderSideSell {
		errs = append(errs, fmt.Errorf("side %q is not buy or sell", side))
	}
	if quantity <= 0 {
		errs = append(errs, errors.New("quantity must be positive"))
	}
	if !isFinite(limit) || limit <= 0 {
		errs = append(errs, errors.New("limit price must be finite and positive"))
	}
	return errs
}

// The risk gate's two outcomes for a planned order (ADR 0026, point 3). Only
// a released order, once durably journalled, may be sent.
const (
	OrderReleasedEventType = "risk.order.released"
	OrderRefusedEventType  = "risk.order.refused"
)

// The schema versions of OrderReleasedPayload and OrderRefusedPayload (ADR
// 0015). A writer journals each payload at SchemaVersion: the lowest version
// that holds it, and never below version 2, since every figure this build
// writes is account-wide.
//
//   - Version 1 checked the order's own instrument's exposure.
//   - Version 2 checks account-wide exposure, and adds the IRA's cash check
//     to RiskFigures (CashChecked, BuyCost, CashAvailable) and its refusal
//     reasons (ira-exposure, insufficient-cash, sell-exceeds-holding,
//     unvalued-holding) to the closed set.
//   - Version 3 adds the figures' broker fields (BrokerSet, BrokerHolding,
//     WorkingSells, RiskReducing) and the refusal reasons kill-switch,
//     broker-holding, sale-attempted and stale-broker-snapshot, and the
//     refusal's alert (ADR 0019, amended 2026-10-15 and
//     2026-10-17; ADR 0026, points 3 and 4).
//   - Version 4 of OrderRefusedPayload adds the refusal reason
//     pending-cash-decrease (ADR 0019, amended 2026-10-21).
//
// Each version reads every earlier version's fields, and refuses a later
// version's.
const (
	OrderReleasedSchemaVersion uint32 = 2
	OrderReleasedSafetyVersion uint32 = 3
	OrderRefusedSchemaVersion  uint32 = 2
	OrderRefusedSafetyVersion  uint32 = 3
	// OrderRefusedPendingCashVersion adds the refusal reason
	// pending-cash-decrease (ADR 0019, amended 2026-10-21).
	OrderRefusedPendingCashVersion uint32 = 4
)

// The closed set of reasons the risk gate refuses an order (ADR 0026, point 3).
const (
	// RefusedUndecidedOrder: the tag names no journalled decision. Halts.
	RefusedUndecidedOrder = "undecided-order"
	// RefusedMarginExposure: an exposure-increasing order would leave the
	// account above its pre-trade limit.
	RefusedMarginExposure = "margin-exposure"
	// RefusedOrderCount: the plan has more orders than the limit allows.
	RefusedOrderCount = "order-count"
	// RefusedOrderSize: the order is a larger share of net liquidation than allowed.
	RefusedOrderSize = "order-size"
	// RefusedPriceBand: the reference price lies outside the band around the mark.
	RefusedPriceBand = "price-band"
	// RefusedMissingMark: no mark recorded for the instrument at or after the
	// session's close, so the band cannot be checked.
	RefusedMissingMark = "missing-mark"
	// RefusedEngineDegraded: the account is Degraded, which allows no buy and
	// freezes the instruments its scope names (ADR 0026, point 4).
	RefusedEngineDegraded = "engine-degraded"
	// RefusedEngineHalted: the account is Halted, which allows no order.
	RefusedEngineHalted = "engine-halted"
	// RefusedIRAExposure: a buy would leave an IRA's exposure above its
	// limit, 100% (ADR 0026, point 3).
	RefusedIRAExposure = "ira-exposure"
	// RefusedInsufficientCash: a buy's worst-case cost exceeds the IRA's
	// cash available under D5, net of committed buys; an IRA never borrows.
	RefusedInsufficientCash = "insufficient-cash"
	// RefusedSellExceedsHolding: a sell exceeds the holding net of every
	// working or pending sell (ADR 0019, amended 2026-09-24).
	RefusedSellExceedsHolding = "sell-exceeds-holding"
	// RefusedUnvaluedHolding: an exposure-increasing order while a holding
	// has no reference price, so account-wide exposure cannot be valued.
	RefusedUnvaluedHolding = "unvalued-holding"
	// RefusedKillSwitch: the kill switch is engaged, which refuses every
	// order except ADR 0019's one sale back to 98% in a margin account
	// (ADR 0026, point 3). It changes no engine state.
	RefusedKillSwitch = "kill-switch"
	// RefusedBrokerHolding: a sell exceeds the broker-reported holding net of
	// every working or pending sell (ADR 0026, point 2.5), or, while Degraded,
	// there is no broker snapshot to size it from (ADR 0019, amended
	// 2026-09-24).
	RefusedBrokerHolding = "broker-holding"
	// RefusedSaleAttempted: the incident's one sale back to 98% was already
	// attempted for the instrument, and is never retried automatically
	// (ADR 0019, amended 2026-10-15).
	RefusedSaleAttempted = "sale-attempted"
	// RefusedStaleBrokerSnapshot: a sell would be checked against a latest
	// broker snapshot older at its planned time than the configured execution
	// MaxSnapshotAge, or taken after it (ADR 0019: "a stale matching
	// snapshot is not success"). It changes no engine state; the sell is
	// planned again at the next decision.
	RefusedStaleBrokerSnapshot = "stale-broker-snapshot"
	// RefusedPendingCashDecrease: a buy while a cash decrease is pending a
	// reading of IBKR's statement, so it may be a withdrawal and the cash a
	// buy is sized on may not be there (ADR 0019, amended 2026-10-21). It
	// changes no engine state; sells and exits go.
	RefusedPendingCashDecrease = "pending-cash-decrease"
)

// The prices a price band compares an order's reference price with
// (RiskFigures.MarkSource; ADR 0026 point 3, amended 2026-10-18).
const (
	MarkSourceBrokerLast    = "broker-last"
	MarkSourceOfficialClose = "official-close"
)

// RiskFigures are the values the gate checked an order against, so the
// journal alone can check the outcome. Mark is 0 when MarkSet is false.
// Exposure is account-wide: every holding at its reference price over net
// liquidation. CashChecked is true for an IRA's buy, whose worst-case BuyCost
// was checked against CashAvailable; otherwise both are 0.
type RiskFigures struct {
	NetLiquidation   float64 `json:"net_liquidation"`
	ReferencePrice   float64 `json:"reference_price"`
	ExposureBefore   float64 `json:"exposure_before"`
	ExposureAfter    float64 `json:"exposure_after"`
	OrderFraction    float64 `json:"order_fraction"`
	Mark             float64 `json:"mark"`
	MarkSet          bool    `json:"mark_set"`
	MarkDeviation    float64 `json:"mark_deviation"`
	OrdersInDecision int     `json:"orders_in_decision"`
	CashChecked      bool    `json:"cash_checked,omitzero"`
	BuyCost          float64 `json:"buy_cost,omitzero"`
	// RoundingSlack is the component of BuyCost held for partial-execution
	// cent rounding (ADR 0026, "Per-execution rounding reserve").
	RoundingSlack float64 `json:"rounding_slack,omitzero"`
	CashAvailable float64 `json:"cash_available,omitzero"`
	// RiskReducing marks an order ADR 0026 point 4 calls risk-reducing: a
	// sell within the broker-reported holding net of every working or
	// pending sell (BrokerSet), or, with no readable broker snapshot, within
	// the account snapshot's holding it was sized from. It is exempt from
	// the order count, size and price band.
	RiskReducing bool `json:"risk_reducing,omitempty"`
	// MarkSource names the price the band compared the reference price with,
	// for a mark at MarkRecordedEvidenceVersion: IBKR's last price when the
	// mark carries one (MarkSourceBrokerLast), else the delivered bar's
	// official close (MarkSourceOfficialClose; ADR 0026 point 3, amended
	// 2026-10-18). Omitted for an earlier mark, so its figures keep their
	// bytes.
	MarkSource string `json:"mark_source,omitempty"`
	// The latest broker snapshot's holding and working sells for the
	// instrument, when the gate had one (BrokerSet). WorkingSells also counts
	// the sells the gate released after that snapshot, which it cannot yet
	// show (ADR 0019, amended 2026-10-15: "never below it"). All four fields are
	// omitted when unset, so figures without a broker snapshot keep their
	// bytes.
	BrokerSet     bool  `json:"broker_set,omitempty"`
	BrokerHolding int64 `json:"broker_holding,omitempty"`
	WorkingSells  int64 `json:"working_sells,omitempty"`
}

func (f RiskFigures) validate(side string) []error {
	var errs []error
	if !isFinite(f.NetLiquidation) || f.NetLiquidation <= 0 {
		errs = append(errs, errors.New("figures net liquidation must be finite and positive"))
	}
	if !isFinite(f.ReferencePrice) || f.ReferencePrice <= 0 {
		errs = append(errs, errors.New("figures reference price must be finite and positive"))
	}
	for _, v := range []struct {
		name  string
		value float64
	}{{"exposure before", f.ExposureBefore}, {"exposure after", f.ExposureAfter}, {"order fraction", f.OrderFraction}, {"mark deviation", f.MarkDeviation}} {
		if !isFinite(v.value) || v.value < 0 {
			errs = append(errs, fmt.Errorf("figures %s must be finite and not negative", v.name))
		}
	}
	switch {
	case !isFinite(f.Mark) || f.Mark < 0:
		errs = append(errs, errors.New("figures mark must be finite and not negative"))
	case f.MarkSet && f.Mark == 0:
		errs = append(errs, errors.New("figures mark must be positive when set"))
	case !f.MarkSet && (f.Mark != 0 || f.MarkDeviation != 0):
		errs = append(errs, errors.New("figures mark carries a value but none is set"))
	}
	switch {
	case f.MarkSource != "" && f.MarkSource != MarkSourceBrokerLast && f.MarkSource != MarkSourceOfficialClose:
		errs = append(errs, fmt.Errorf("figures mark source %q is not %s or %s", f.MarkSource, MarkSourceBrokerLast, MarkSourceOfficialClose))
	case f.MarkSource != "" && !f.MarkSet:
		errs = append(errs, errors.New("figures mark source names a price but no mark is set"))
	}
	if f.OrdersInDecision < 0 {
		errs = append(errs, errors.New("figures orders in decision must not be negative"))
	}
	if f.WorkingSells < 0 {
		errs = append(errs, errors.New("figures working sells must not be negative"))
	}
	if f.RiskReducing && side != OrderSideSell {
		errs = append(errs, errors.New("figures risk-reducing marks a sell only: no buy is risk-reducing"))
	}
	if !f.BrokerSet && (f.BrokerHolding != 0 || f.WorkingSells != 0) {
		errs = append(errs, errors.New("figures broker holding and working sells carry values but no broker snapshot is set"))
	}
	switch {
	case f.RoundingSlack != 0 && (!ValidRoundingSlack(f.RoundingSlack) || !f.CashChecked || side != OrderSideBuy || f.BuyCost < f.RoundingSlack):
		errs = append(errs, errors.New("figures rounding slack requires a cash-checked buy, [0.01, 1.00] dollars, and inclusion in buy cost"))
	case !isFinite(f.BuyCost) || f.BuyCost < 0 || !isFinite(f.CashAvailable):
		errs = append(errs, errors.New("figures buy cost must be finite and not negative, and cash available finite"))
	case !f.CashChecked && (f.BuyCost != 0 || f.CashAvailable != 0):
		errs = append(errs, errors.New("figures cash carries a value but was not checked"))
	}
	return errs
}

// OrderReleasedPayload is a planned order the gate passed. Send is false in
// dry-run: the order is journalled and never sent (ADR 0026, point 3, "Mode").
type OrderReleasedPayload struct {
	QuantityScale int64       `json:"quantity_scale,omitempty"`
	AccountID     string      `json:"account_id"`
	PlanID        string      `json:"plan_id"`
	Tag           string      `json:"tag"`
	ClientOrderID string      `json:"client_order_id"`
	InstrumentID  string      `json:"instrument_id"`
	Side          string      `json:"side"`
	Quantity      int64       `json:"quantity"`
	LimitPrice    float64     `json:"limit_price"`
	Mode          string      `json:"mode"`
	Send          bool        `json:"send"`
	Figures       RiskFigures `json:"figures"`
}

// need returns the earliest version of risk.order.released and
// risk.order.refused that holds f: 3 for the broker fields, 2 for the cash
// figures, otherwise 1.
func (f RiskFigures) need() uint32 {
	switch {
	case f.BrokerSet || f.RiskReducing || f.BrokerHolding != 0 || f.WorkingSells != 0:
		return OrderReleasedSafetyVersion
	case f.CashChecked || f.BuyCost != 0 || f.CashAvailable != 0:
		return 2
	}
	return 1
}

// SchemaVersion returns the version a writer journals p at.
func (p OrderReleasedPayload) SchemaVersion() uint32 {
	if p.QuantityScale != 0 {
		return OrderReleasedFractionalVersion
	}
	return max(p.Figures.need(), OrderReleasedSchemaVersion)
}

// ValidateSchema checks that p is one schemaVersion holds, then validates it.
func (p OrderReleasedPayload) ValidateSchema(schemaVersion uint32) error {
	if err := quantitySchema(p.QuantityScale, schemaVersion, OrderReleasedFractionalVersion); err != nil {
		return err
	}
	if schemaVersion == OrderReleasedFractionalVersion {
		return p.Validate()
	}
	if err := checkSafetyVersion(OrderReleasedEventType, schemaVersion, p.Figures.need(), OrderReleasedSafetyVersion); err != nil {
		return err
	}
	return p.Validate()
}

// Validate checks identity, mode, the dry-run rule and the figures.
func (p OrderReleasedPayload) Validate() error {
	if p.Figures.RoundingSlack != 0 && p.QuantityScale != FractionalQuantityScale {
		return errors.New("rounding slack requires fractional quantities")
	}
	if p.QuantityScale == FractionalQuantityScale && p.Figures.BrokerHolding < 0 {
		return errors.New("fractional broker holding must not be negative")
	}
	if err := quantityScaleError(p.QuantityScale); err != nil {
		return err
	}
	var errs []error
	errs = append(errs, orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)...)
	if p.PlanID == "" {
		errs = append(errs, errors.New("plan id is required"))
	}
	if p.Tag == "" {
		errs = append(errs, errors.New("tag is required: no order is released without a journalled decision"))
	}
	if !validTradingMode(p.Mode) {
		errs = append(errs, fmt.Errorf("mode %q is not dry-run, paper or live", p.Mode))
	}
	if p.Mode == TradingModeDryRun && p.Send {
		errs = append(errs, errors.New("a dry-run release is never sent"))
	}
	errs = append(errs, p.Figures.validate(p.Side)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order released payload: %w", err)
	}
	return nil
}

// OrderRefusedPayload is a planned order the gate refused, with its reason
// from the closed set and the figures behind it. Tag may be empty: an
// untagged order is exactly what RefusedUndecidedOrder refuses.
type OrderRefusedPayload struct {
	QuantityScale int64       `json:"quantity_scale,omitempty"`
	AccountID     string      `json:"account_id"`
	PlanID        string      `json:"plan_id"`
	Tag           string      `json:"tag"`
	ClientOrderID string      `json:"client_order_id"`
	InstrumentID  string      `json:"instrument_id"`
	Side          string      `json:"side"`
	Quantity      int64       `json:"quantity"`
	LimitPrice    float64     `json:"limit_price"`
	Reason        string      `json:"reason"`
	Detail        string      `json:"detail"`
	Figures       RiskFigures `json:"figures"`
	// Alert marks the refusal that raises the urgent alert: the first
	// stale-broker-snapshot refusal of the account's stale episode (ADR 0019,
	// amendment of 2026-10-17, reading 10), or the first kill-switch refusal
	// of one engagement (ADR 0026, point 3, kill switch: "alert once"), or
	// the first pending-cash-decrease refusal of one pending cash difference
	// (ADR 0019, amended 2026-10-21). The later refusals of the same episode,
	// engagement or difference are recorded without it and send nothing.
	// Only a journal recorded under rules version 1.16.0 or later flags a
	// kill-switch refusal.
	Alert bool `json:"alert,omitempty"`
}

// need returns the earliest version that holds p: its figures', or the
// version its reason joined at.
func (p OrderRefusedPayload) need() uint32 {
	switch p.Reason {
	case RefusedPendingCashDecrease:
		return OrderRefusedPendingCashVersion
	case RefusedKillSwitch, RefusedBrokerHolding, RefusedSaleAttempted, RefusedStaleBrokerSnapshot:
		return OrderRefusedSafetyVersion
	case RefusedIRAExposure, RefusedInsufficientCash, RefusedSellExceedsHolding, RefusedUnvaluedHolding:
		return max(p.Figures.need(), 2)
	}
	return p.Figures.need()
}

// SchemaVersion returns the version a writer journals p at.
func (p OrderRefusedPayload) SchemaVersion() uint32 {
	if p.QuantityScale != 0 {
		return OrderRefusedFractionalVersion
	}
	return max(p.need(), OrderRefusedSchemaVersion)
}

// ValidateSchema checks that p is one schemaVersion holds, then validates it.
func (p OrderRefusedPayload) ValidateSchema(schemaVersion uint32) error {
	if err := quantitySchema(p.QuantityScale, schemaVersion, OrderRefusedFractionalVersion); err != nil {
		return err
	}
	if schemaVersion == OrderRefusedFractionalVersion {
		return p.Validate()
	}
	if err := checkSafetyVersion(OrderRefusedEventType, schemaVersion, p.need(), OrderRefusedPendingCashVersion); err != nil {
		return err
	}
	return p.Validate()
}

// Validate checks identity, the reason, the detail and the figures.
func (p OrderRefusedPayload) Validate() error {
	if p.Figures.RoundingSlack != 0 && p.QuantityScale != FractionalQuantityScale {
		return errors.New("rounding slack requires fractional quantities")
	}
	if p.QuantityScale == FractionalQuantityScale && p.Figures.BrokerHolding < 0 {
		return errors.New("fractional broker holding must not be negative")
	}
	if err := quantityScaleError(p.QuantityScale); err != nil {
		return err
	}
	var errs []error
	errs = append(errs, orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)...)
	if p.PlanID == "" {
		errs = append(errs, errors.New("plan id is required"))
	}
	switch p.Reason {
	case RefusedUndecidedOrder, RefusedMarginExposure, RefusedOrderCount, RefusedOrderSize,
		RefusedPriceBand, RefusedMissingMark, RefusedEngineDegraded, RefusedEngineHalted,
		RefusedIRAExposure, RefusedInsufficientCash, RefusedSellExceedsHolding, RefusedUnvaluedHolding,
		RefusedKillSwitch, RefusedBrokerHolding, RefusedSaleAttempted, RefusedStaleBrokerSnapshot,
		RefusedPendingCashDecrease:
	default:
		errs = append(errs, fmt.Errorf("refusal reason %q is not recognised", p.Reason))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required: a refusal without its figures cannot be checked"))
	}
	if p.Alert && p.Reason != RefusedStaleBrokerSnapshot && p.Reason != RefusedKillSwitch && p.Reason != RefusedPendingCashDecrease {
		errs = append(errs, errors.New("only a stale-broker-snapshot, kill-switch or pending-cash-decrease refusal raises the alert"))
	}
	errs = append(errs, p.Figures.validate(p.Side)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order refused payload: %w", err)
	}
	return nil
}

// Fractional decision versions give every quantity figure an explicit fixed
// scale, leaving whole-share decision bytes intact (ADR 0015; ADR 0026, D7-F).
const (
	OrderPlannedFractionalVersion   uint32 = 3
	OrderReleasedFractionalVersion  uint32 = 4
	OrderRefusedFractionalVersion   uint32 = 5
	TargetsDecidedFractionalVersion uint32 = 11
)

// SchemaVersion states the target decision's quantity units (ADR 0015).
func (p TargetsDecidedPayload) SchemaVersion() uint32 {
	if p.QuantityScale != 0 {
		return TargetsDecidedFractionalVersion
	}
	return TargetsDecidedSchemaVersion
}

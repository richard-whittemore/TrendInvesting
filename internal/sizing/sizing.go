// Package sizing owns the pure arithmetic that turns a volatility reading
// into a whole number of shares or contracts, and the Risk at Stop that
// number implies (CONTEXT.md: "Unit", "Risk at Stop", "Sizing Mode"; ADR
// 0003).
//
// It is deliberately separate from internal/indicator. An indicator measures
// something about a price series; sizing decides how much capital to commit,
// which is a different kind of statement with a different failure mode — an
// indicator that is wrong produces a bad reading, sizing that is wrong
// produces a position the account cannot afford. Keeping it in its own
// package means every line that stands between N and a share count is in one
// file that a reviewer can read end to end.
//
// Like internal/indicator, this package has no knowledge of events or replay
// and imports nothing from internal/event: it declares its own Mode, and the
// reducer maps event.SizingMode onto it with an explicit fail-closed switch.
// That keeps the arithmetic testable, and reusable, without the wire
// contract; internal/strategy's TestSizingModeConstantsMatchTheEventContract
// pins the two enumerations' values equal so they cannot drift apart
// silently.
//
// Every entry point fails closed. .greptile/rules.md: "A zero, negative, or
// not-yet-warm volatility value must fail closed — never size a position from
// it." Non-finite inputs are rejected explicitly, before any ordered
// comparison, because every ordered comparison against NaN is false.
package sizing

import (
	"errors"
	"fmt"
	"math"
)

// Mode selects which quantity a position's size is keyed to (ADR 0003).
//
// The two modes are only equivalent at one particular Stop Multiple, so
// which one is in force is an explicit, declared choice rather than a
// consequence of some other parameter. Both the Notion notes and the earlier
// QuantConnect prototype collapsed them into the fixed-risk-at-stop form,
// which silently diverges from the source the moment the stop is widened.
type Mode string

// The two declared Sizing Modes. Their string values are identical to
// event.SizingMode's by design; see the package comment.
const (
	// ModeVolatilityNormalised is the Baseline (ADR 0003): a Unit is sized
	// so that a 1N move equals a declared fraction of the Notional Account
	// (The Turtle Rules p.14). The Protective Stop is a consequence, not an
	// input, so a wider stop leaves the share count unchanged and raises
	// Risk at Stop.
	ModeVolatilityNormalised Mode = "volatility-normalised"
	// ModeFixedRiskAtStop is the Sublime Variant [M p.56]: a Unit is sized
	// so that the entry-to-stop distance times the share count equals a
	// declared fraction of the Notional Account. A wider stop reduces the
	// share count and leaves Risk at Stop unchanged.
	ModeFixedRiskAtStop Mode = "fixed-risk-at-stop"
)

// maxExactWholeQuantity is 2^53, the largest whole number every float64 above
// which can no longer represent consecutive integers. A quotient at or above
// it cannot be truncated meaningfully — the fractional part the Turtle rule
// discards does not exist at that magnitude — and converting it to int64 is
// undefined behaviour above 2^63. A near-zero N against a large account is
// the realistic way to get there, so this is a fail-closed check rather than
// a theoretical one.
const maxExactWholeQuantity = float64(1 << 53)

// Inputs carries everything one Unit's size depends on. It is a struct
// rather than a long parameter list because the two modes need overlapping
// but not identical subsets, and because a caller that mixes up two adjacent
// float64 arguments is exactly the defect this package exists to prevent.
//
// UnitVolatilityFraction and RiskAtStopFraction are never both inputs:
// under ModeVolatilityNormalised the first sizes the Unit and the second
// must be zero (ADR 0003: Risk at Stop is derived, never configured); under
// ModeFixedRiskAtStop the second sizes the Unit and the first is carried
// only as the configuration that happened to be in force.
type Inputs struct {
	Mode Mode
	// NotionalAccount is the equity figure sizing is measured against
	// (CONTEXT.md: "Notional Account"; ADR 0007) — not actual account
	// equity.
	NotionalAccount float64
	// UnitVolatilityFraction is the fraction of the Notional Account that a
	// 1N move in one Unit represents (CONTEXT.md; The Turtle Rules p.14).
	// The Baseline runs 0.5 % (ADR 0003); Faith's own 1 % is a declared
	// Variant.
	UnitVolatilityFraction float64
	// StopMultiple is the number of N between a Unit's entry and its
	// Protective Stop (CONTEXT.md). The Baseline runs 2, transcribed from
	// The Turtle Rules p.22.
	StopMultiple float64
	// RiskAtStopFraction is the configured Risk at Stop, and is an input
	// only under ModeFixedRiskAtStop. Under ModeVolatilityNormalised it must
	// be zero; supplying a value there is an error, not an override.
	RiskAtStopFraction float64
	// N is the volatility reading in force (CONTEXT.md: "N"). It must be a
	// usable reading: a caller must already have checked readiness, since a
	// warm-up-complete-but-zero N is not sizeable.
	N float64
	// DollarsPerPoint is the instrument's contract multiplier: exactly 1 for
	// shares, and 42,000 for the Heating Oil contract in Faith's worked
	// example (The Turtle Rules p.15). It is a parameter so that example
	// reproduces rather than being approximated.
	DollarsPerPoint float64
}

// Unit is one Unit's sized outcome: how many shares or contracts, what
// fraction of the Notional Account the strategy set out to risk, and what
// fraction that whole-share quantity actually risks.
//
// All three travel together deliberately. Both risk figures are derived from
// the same inputs that produced Quantity, so returning them from one call
// removes the possibility of a caller stamping a risk figure into a journal
// that the quantity beside it does not actually imply.
type Unit struct {
	// Quantity is a whole number of shares or contracts, truncated toward
	// zero (The Turtle Rules p.14-15: 16.88 becomes 16). It is zero, with no
	// error, when the Notional Account cannot fund a single one — a
	// legitimate arithmetic outcome the caller must decline on, not a
	// failure.
	Quantity int64
	// RiskAtStop is the **declared budget**: the fraction of the Notional
	// Account the Sizing Mode is keyed to (CONTEXT.md: "Risk at Stop"). Under
	// ModeVolatilityNormalised it is exactly UnitVolatilityFraction x
	// StopMultiple and under ModeFixedRiskAtStop it is the configured input.
	// It is deliberately NOT adjusted for truncation: it is the parameter the
	// strategy declared, and ADR 0003 makes it a derived-or-configured
	// property of the configuration rather than of any one position.
	RiskAtStop float64
	// RealisedRiskAtStop is what the whole-share Quantity above **actually**
	// risks if the Protective Stop is hit:
	//
	//	Quantity x StopMultiple x N x DollarsPerPoint / NotionalAccount
	//
	// The gap between it and RiskAtStop is the truncation, and it always
	// points one way — a truncated position risks less than the budget, never
	// more, so RealisedRiskAtStop <= RiskAtStop always holds. Faith's Heating
	// Oil Unit makes the gap concrete: a declared 2 % becomes a realised
	// 1.895 % once 16.88 contracts truncate to 16 (The Turtle Rules p.15).
	//
	// A journal that recorded only the budget would overstate what an
	// individual Unit stands to lose, which is why both are carried onto the
	// proposal (event.TradeProposalPayload) rather than one standing for the
	// other.
	RealisedRiskAtStop float64
}

// SizeUnit sizes one Unit under the given Sizing Mode and returns both the
// quantity and the Risk at Stop that quantity implies.
//
// This is the entry point internal/strategy uses. It is deliberately the only
// place that dispatches on Mode, so that "which principle sized this Unit"
// has exactly one answer per call and cannot be assembled from two
// independently-chosen halves.
func SizeUnit(in Inputs) (Unit, error) {
	riskAtStop, err := RiskAtStop(in.Mode, in.UnitVolatilityFraction, in.StopMultiple, in.RiskAtStopFraction)
	if err != nil {
		return Unit{}, err
	}

	var quantity int64
	switch in.Mode {
	case ModeVolatilityNormalised:
		quantity, err = UnitQuantity(in.NotionalAccount, in.UnitVolatilityFraction, in.N, in.DollarsPerPoint)
	case ModeFixedRiskAtStop:
		quantity, err = FixedRiskAtStopQuantity(in.NotionalAccount, in.RiskAtStopFraction, in.StopMultiple, in.N, in.DollarsPerPoint)
	default:
		// Unreachable: RiskAtStop above already rejected every mode but
		// these two. Kept so that adding a third Mode without extending
		// this switch fails closed rather than silently sizing nothing.
		return Unit{}, unrecognisedModeError(in.Mode)
	}
	if err != nil {
		return Unit{}, err
	}

	return Unit{
		Quantity:   quantity,
		RiskAtStop: riskAtStop,
		// One fixed expression order, shared with
		// event.TradeProposalPayload.Validate's re-derivation so the two
		// agree bit for bit rather than approximately. The numerator is the
		// same product each quantity function already bounds by the budget,
		// which is what makes RealisedRiskAtStop <= RiskAtStop hold.
		RealisedRiskAtStop: RealisedRiskAtStop(quantity, in.StopMultiple, in.N, in.DollarsPerPoint, in.NotionalAccount),
	}, nil
}

// RealisedRiskAtStop returns the fraction of the Notional Account a
// whole-share quantity actually risks if its Protective Stop is hit:
//
//	quantity x stopMultiple x n x dollarsPerPoint / notionalAccount
//
// It is exported, and defined here once, so that every producer and every
// validator computes it in the identical expression order. Both
// sizing.SizeUnit and event.TradeProposalPayload.Validate call it, and the
// payload's derivation check compares with exact float64 equality — a second
// implementation, however algebraically identical, could differ in the last
// bit and turn a correct proposal into a rejected one.
//
// It performs no validation of its own: callers reach it only after their
// inputs have been checked, and it is called on a quantity that has already
// been produced from those same inputs.
func RealisedRiskAtStop(quantity int64, stopMultiple, n, dollarsPerPoint, notionalAccount float64) float64 {
	return float64(quantity) * (stopMultiple * n * dollarsPerPoint) / notionalAccount
}

// DrawdownStepRetainedFraction is the fraction of the Notional Account
// retained (equivalently, the complement of the 20% reduction) at each
// Drawdown Step: x0.8 (The Turtle Rules p.17, ADR 0007). Exported so
// internal/strategy can derive the drawdown ladder's asymptote from the same
// single constant DrawdownSteppedNotional uses, rather than a second,
// independently-stated 0.8.
const DrawdownStepRetainedFraction = 0.8

// DrawdownSteppedNotional returns the Notional Account after one Drawdown
// Step (CONTEXT.md: "Drawdown Step"; ADR 0007): before x
// DrawdownStepRetainedFraction.
//
// It is exported, and defined here once — the same "one copy of each
// derivation" discipline RealisedRiskAtStop above establishes — so that
// internal/strategy.NotionalAccount.Observe (the producer) and
// event.DrawdownStepAppliedPayload.Validate (the validator) compute the
// identical float64 value and an exact-equality comparison between them is
// meaningful rather than a source of false rejections. #65 tracks this
// discipline generally, including the risk that two textually identical
// expressions can be fused differently across architectures; that risk
// applies to an expression combining a multiply with an add or subtract
// (e.g. EntryLevel - StopMultiple*N), which a compiler may fuse as a single
// operation, not to this function's single multiplication, which has
// nothing to fuse with.
func DrawdownSteppedNotional(before float64) float64 {
	return DrawdownStepRetainedFraction * before
}

// UnitQuantity is Faith's Unit-sizing formula (The Turtle Rules p.14): one
// Unit is the Unit Volatility Fraction of the Notional Account divided by the
// market's dollar volatility, N times dollars per point.
//
//	quantity = floor( notionalAccount x unitVolatilityFraction
//	                  ------------------------------------------ )
//	                          n x dollarsPerPoint
//
// The worked example on p.15 — Heating Oil, N = 0.0141, a $1,000,000 account,
// a 42,000-gallon contract — gives 1,000,000 x 0.01 / (0.0141 x 42,000) =
// 16.88, truncated to **16 contracts**. Faith truncates; he does not round.
//
// dollarsPerPoint is a parameter rather than a constant so that example
// reproduces exactly. For shares it is 1: one point of price is one dollar
// per share.
//
// The Protective Stop is not an input here. Under this mode a wider stop buys
// the same number of shares and simply risks more at the stop, which is the
// whole distinction ADR 0003 exists to keep visible; see
// FixedRiskAtStopQuantity for the other principle.
//
// A quantity of zero is returned with no error when the account cannot fund a
// single share or contract — p.15 notes small accounts lose diversification
// precisely because truncation is this coarse. Deciding what to do about that
// belongs to the caller, which records a decline rather than a position.
func UnitQuantity(notionalAccount, unitVolatilityFraction, n, dollarsPerPoint float64) (int64, error) {
	errs := []error{
		checkNotionalAccount(notionalAccount),
		checkFraction("unit volatility fraction", unitVolatilityFraction),
		checkN(n),
		checkDollarsPerPoint(dollarsPerPoint),
	}
	if err := errors.Join(errs...); err != nil {
		return 0, fmt.Errorf("sizing: cannot size a unit: %w", err)
	}

	// The budget a 1N move is allowed to cost, and what one share or
	// contract costs per N. Both are formed here, in this order, and the
	// same order is used by the truncation guard below and by
	// event.TradeProposalPayload.Validate, so the invariant that guard
	// establishes is the exact invariant the validator re-checks.
	budget := notionalAccount * unitVolatilityFraction
	costPerUnitPerN := n * dollarsPerPoint
	return truncate(budget, costPerUnitPerN)
}

// FixedRiskAtStopQuantity is the Sublime sizing form [M p.56]: shares such
// that the entry-to-stop distance times the share count does not exceed the
// chosen risk fraction of the account.
//
//	quantity = floor( notionalAccount x riskAtStopFraction
//	                  --------------------------------------- )
//	                   stopMultiple x n x dollarsPerPoint
//
// **This is not the Baseline.** ADR 0003 makes the Baseline
// volatility-normalised (UnitQuantity) and this the Sublime Variant. The two
// are algebraically identical only when the Stop Multiple is 2 and the risk
// fraction is twice the Unit Volatility Fraction; away from that point they
// diverge, and collapsing them — as both the Notion notes and the earlier
// QuantConnect prototype did — silently re-scales the whole book the moment
// the 3xATR stop experiment runs.
//
// Under this form the entry-to-stop distance is stopMultiple x N x
// dollarsPerPoint, so a wider stop buys fewer shares and leaves Risk at Stop
// unchanged, which is the exact opposite of UnitQuantity's behaviour.
func FixedRiskAtStopQuantity(notionalAccount, riskAtStopFraction, stopMultiple, n, dollarsPerPoint float64) (int64, error) {
	errs := []error{
		checkNotionalAccount(notionalAccount),
		checkFraction("risk at stop fraction", riskAtStopFraction),
		checkStopMultiple(stopMultiple),
		checkN(n),
		checkDollarsPerPoint(dollarsPerPoint),
	}
	if err := errors.Join(errs...); err != nil {
		return 0, fmt.Errorf("sizing: cannot size a unit: %w", err)
	}

	budget := notionalAccount * riskAtStopFraction
	costPerUnitAtStop := stopMultiple * n * dollarsPerPoint
	return truncate(budget, costPerUnitAtStop)
}

// RiskAtStop derives the fraction of the Notional Account one Unit loses if
// its Protective Stop is hit (CONTEXT.md: "Risk at Stop").
//
// Under ModeVolatilityNormalised it is unitVolatilityFraction x stopMultiple
// and riskAtStopFraction must be zero: ADR 0003's central rule is that Risk
// at Stop is **derived, never configured** in this mode, so a supplied value
// is an error rather than an override. The Baseline's 0.5 % per N with a 2N
// stop derives 1 %; Faith's own 1 % per N with the same stop derives the 2 %
// he describes (The Turtle Rules p.14, p.22).
//
// Under ModeFixedRiskAtStop, riskAtStopFraction IS Risk at Stop — it is the
// sizing input [M p.56] — and unitVolatilityFraction plays no part.
//
// A derived value above 1 is rejected: it would mean one Unit loses more than
// the entire Notional Account at its own Protective Stop, which is a
// configuration defect rather than a market condition.
func RiskAtStop(mode Mode, unitVolatilityFraction, stopMultiple, riskAtStopFraction float64) (float64, error) {
	switch mode {
	case ModeVolatilityNormalised:
		errs := []error{
			checkFraction("unit volatility fraction", unitVolatilityFraction),
			checkStopMultiple(stopMultiple),
		}
		if riskAtStopFraction != 0 {
			// != 0 also catches NaN, which is the point: a NaN here is a
			// configured value, not an absent one.
			errs = append(errs, fmt.Errorf(
				"risk at stop must not be configured under volatility-normalised sizing (ADR 0003: it is derived as unit volatility fraction x stop multiple), got %v", riskAtStopFraction))
		}
		if err := errors.Join(errs...); err != nil {
			return 0, fmt.Errorf("sizing: cannot derive risk at stop: %w", err)
		}
		derived := unitVolatilityFraction * stopMultiple
		if derived > 1 {
			return 0, fmt.Errorf(
				"sizing: cannot derive risk at stop: derived risk at stop %v exceeds one: unit volatility fraction %v x stop multiple %v would lose more than the entire notional account at the protective stop",
				derived, unitVolatilityFraction, stopMultiple)
		}
		return derived, nil

	case ModeFixedRiskAtStop:
		if err := checkFraction("risk at stop fraction", riskAtStopFraction); err != nil {
			return 0, fmt.Errorf("sizing: cannot derive risk at stop: %w", err)
		}
		return riskAtStopFraction, nil

	default:
		return 0, unrecognisedModeError(mode)
	}
}

// truncate turns a budget and a per-unit cost into a whole quantity,
// truncated toward zero (The Turtle Rules p.14-15).
//
// The correction below is not defensive noise. Floating-point division is
// correctly rounded to nearest, so a quotient whose true value sits just
// below an integer can round UP to that integer; flooring the rounded value
// then returns one unit too many, and that unit costs more than the budget
// allows. The error only ever points this way — a true quotient at or above
// an integer can never round below it, since the correctly-rounded result of
// an exact integer quotient is that integer — so a single step down is
// always enough, and it is always the conservative direction. What is
// returned is therefore the floor of the *mathematical* quotient, not the
// floor of a rounded one.
//
// The comparison uses the same expression order the caller used to form
// cost, and the same order event.TradeProposalPayload.Validate re-checks, so
// the three agree bit for bit rather than approximately.
func truncate(budget, cost float64) (int64, error) {
	quotient := math.Floor(budget / cost)
	if quotient >= maxExactWholeQuantity {
		return 0, fmt.Errorf(
			"sizing: quantity %v exceeds the largest exactly representable whole quantity %v: refusing to truncate a value whose fractional part cannot be represented",
			quotient, maxExactWholeQuantity)
	}
	if quotient > 0 && quotient*cost > budget {
		quotient--
	}
	return int64(quotient), nil
}

func checkNotionalAccount(v float64) error {
	switch {
	case !isFinite(v):
		return errors.New("notional account must be finite")
	case v <= 0:
		return errors.New("notional account must be positive")
	}
	return nil
}

// checkFraction validates an equity fraction: finite, above zero, and at most
// the whole account.
func checkFraction(name string, v float64) error {
	switch {
	case !isFinite(v):
		return fmt.Errorf("%s must be finite", name)
	case v <= 0 || v > 1:
		return fmt.Errorf("%s must be greater than zero and at most one", name)
	}
	return nil
}

// checkN rejects the not-yet-warm and degenerate volatility readings that
// .greptile/rules.md requires to fail closed. N is never negative in a
// correct producer (True Range cannot be), but it can legitimately be zero
// for a flat instrument, and zero is not sizeable.
func checkN(v float64) error {
	switch {
	case !isFinite(v):
		return errors.New("n must be finite")
	case v <= 0:
		return errors.New("n must be positive: a zero, negative or not-yet-warm volatility reading must never size a position")
	}
	return nil
}

func checkStopMultiple(v float64) error {
	switch {
	case !isFinite(v):
		return errors.New("stop multiple must be finite")
	case v <= 0:
		return errors.New("stop multiple must be positive")
	}
	return nil
}

func checkDollarsPerPoint(v float64) error {
	switch {
	case !isFinite(v):
		return errors.New("dollars per point must be finite")
	case v <= 0:
		return errors.New("dollars per point must be positive")
	}
	return nil
}

func unrecognisedModeError(mode Mode) error {
	return fmt.Errorf("sizing: sizing mode %q is not a recognised sizing mode (ADR 0003 declares %q and %q)",
		mode, ModeVolatilityNormalised, ModeFixedRiskAtStop)
}

// isFinite reports whether v is a real number: not NaN and not an infinity.
// It exists so every check above can reject those explicitly, before any
// ordered comparison, since every ordered comparison against NaN is false and
// would otherwise let a NaN through every range check silently.
func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

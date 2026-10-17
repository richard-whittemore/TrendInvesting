package event

import (
	"errors"
	"fmt"
	"time"
)

// CompletedBarAdjustedVersion is version 2 of market.bar.completed
// (CompletedBarEventType): version 1's split-adjusted and raw views, and a
// split- and dividend-adjusted view, which the target-weight strategies'
// weights and indicators read (ADR 0026, point 1: "Weights use split- and
// dividend-adjusted closes"; Consequences: a third view beside
// split_adjusted and raw).
//
//   - Version 2 is CompletedBarAdjustedPayload. Version 1
//     (CompletedBarSchemaVersion) stays what the Turtle reducer, the
//     backtest and the LEAN adapter read and write; each consumer refuses
//     the other's version (ADR 0015).
const CompletedBarAdjustedVersion uint32 = 2

// ViewAdjusted labels the split- and dividend-adjusted view of a version 2
// bar.
const ViewAdjusted = "split-and-dividend-adjusted"

// CompletedBarAdjustedPayload is market.bar.completed version 2.
//
// The adjusted view is adjusted forward from AdjustedBase: on the
// instrument's first session on or after AdjustedBase (AdjustedBase itself
// when the instrument has a bar then; its first bar when it first traded
// later) it equals the raw view, and each split or cash dividend dated after
// that session, up to and including the bar's own session, scales it by the
// inverse of that event's backward ratio factor (a split's
// split_from/split_to; a dividend's (P - cash)/P, P the raw close of the
// session before its ex-date, on the ex-date's share basis). Its ratios
// between sessions are those of the backward-adjusted series that
// QuantConnect's Adjusted mode uses, which differs from it only by a
// constant per instrument; and since a bar's adjusted figures depend only on
// events up to its own session, a recorded bar is never restated when a
// later dividend is paid, the objection ADR 0004 raises against dividend
// adjustment. Levels are comparable only between bars of one instrument
// with the same AdjustedBase, which consumers therefore require.
type CompletedBarAdjustedPayload struct {
	CompletedBarPayload
	Adjusted PriceView `json:"adjusted"`
	// AdjustedBase is the session, YYYY-MM-DD, the run's adjusted views are
	// based at, the same for every instrument; on or before the bar's
	// session. An instrument with no bar on it is based at its first bar
	// after it.
	AdjustedBase string `json:"adjusted_base"`
}

// Validate checks version 1's fields, the adjusted view, and its base.
func (b CompletedBarAdjustedPayload) Validate() error {
	var errs []error
	if err := b.CompletedBarPayload.Validate(); err != nil {
		errs = append(errs, err)
	}
	for _, err := range b.Adjusted.validate(ViewAdjusted) {
		errs = append(errs, fmt.Errorf("adjusted view: %w", err))
	}
	switch base, err := time.Parse(time.DateOnly, b.AdjustedBase); {
	case err != nil:
		errs = append(errs, fmt.Errorf("adjusted base %q is not a YYYY-MM-DD date", b.AdjustedBase))
	case !b.PeriodEnd.IsZero() && b.AdjustedBase > b.PeriodEnd.UTC().Format(time.DateOnly):
		errs = append(errs, fmt.Errorf("adjusted base %s is after the bar's session %s", base.Format(time.DateOnly), b.PeriodEnd.UTC().Format(time.DateOnly)))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid adjusted completed bar payload: %w", err)
	}
	return nil
}

package event

import (
	"errors"
	"fmt"
	"time"
)

// CompletedBarEventType identifies the completed-bar payload for the
// Envelope's Type field.
const CompletedBarEventType = "market.bar.completed"

// CompletedBarSchemaVersion is the current schema version of
// CompletedBarPayload, for the Envelope's SchemaVersion field.
const CompletedBarSchemaVersion uint32 = 1

// View labels a PriceView. ADR 0004: signal computation runs on
// split-adjusted prices, order pricing and accounting run on raw prices, and
// the two must never be mixed. The label lets a calculation that received the
// wrong view detect the mistake rather than silently produce a wrong level.
const (
	ViewSplitAdjusted = "split-adjusted"
	ViewRaw           = "raw"
)

// PriceView is one open/high/low/close/volume view of a bar, explicitly
// labelled by View.
type PriceView struct {
	View   string  `json:"view"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
}

// validate checks that the view carries the expected label and is internally
// consistent OHLCV data. It returns the individual errors unwrapped so the
// caller can attribute them to "split-adjusted view" or "raw view".
func (v PriceView) validate(want string) []error {
	var errs []error
	switch v.View {
	case "":
		errs = append(errs, errors.New("view label is required"))
	case want:
		// labelled correctly
	default:
		errs = append(errs, fmt.Errorf("view label %q does not match expected %q", v.View, want))
	}
	if v.Open <= 0 {
		errs = append(errs, errors.New("open must be positive"))
	}
	if v.High <= 0 {
		errs = append(errs, errors.New("high must be positive"))
	}
	if v.Low <= 0 {
		errs = append(errs, errors.New("low must be positive"))
	}
	if v.Close <= 0 {
		errs = append(errs, errors.New("close must be positive"))
	}
	if v.Volume < 0 {
		errs = append(errs, errors.New("volume must not be negative"))
	}
	if v.High < v.Low {
		errs = append(errs, errors.New("high must be at least low"))
	}
	if v.High < v.Open {
		errs = append(errs, errors.New("high must be at least open"))
	}
	if v.High < v.Close {
		errs = append(errs, errors.New("high must be at least close"))
	}
	if v.Low > v.Open {
		errs = append(errs, errors.New("low must be at most open"))
	}
	if v.Low > v.Close {
		errs = append(errs, errors.New("low must be at most close"))
	}
	return errs
}

// CompletedBarPayload carries a completed bar's OHLCV data in both required
// price views (ADR 0004), for the instrument and period identified by
// InstrumentID and PeriodEnd.
type CompletedBarPayload struct {
	InstrumentID  string    `json:"instrument_id"`
	PeriodEnd     time.Time `json:"period_end"`
	SplitAdjusted PriceView `json:"split_adjusted"`
	Raw           PriceView `json:"raw"`
}

// Validate checks that the payload identifies an instrument and period, and
// that both price views are present, correctly labelled, and internally
// consistent. A bar carrying only one view, or an unlabelled or mislabelled
// view, is rejected.
func (b CompletedBarPayload) Validate() error {
	var errs []error
	if b.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if b.PeriodEnd.IsZero() {
		errs = append(errs, errors.New("period end is required"))
	}
	for _, err := range b.SplitAdjusted.validate(ViewSplitAdjusted) {
		errs = append(errs, fmt.Errorf("split-adjusted view: %w", err))
	}
	for _, err := range b.Raw.validate(ViewRaw) {
		errs = append(errs, fmt.Errorf("raw view: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid completed bar payload: %w", err)
	}
	return nil
}

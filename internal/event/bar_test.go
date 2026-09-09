package event_test

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validCompletedBar returns a payload that satisfies every validation rule so
// each table test only needs to describe its one deviation.
func validCompletedBar() event.CompletedBarPayload {
	return event.CompletedBarPayload{
		InstrumentID: "AAPL",
		PeriodEnd:    time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC),
		SplitAdjusted: event.PriceView{
			View:   event.ViewSplitAdjusted,
			Open:   100.0,
			High:   105.0,
			Low:    99.0,
			Close:  104.0,
			Volume: 1_000_000,
		},
		Raw: event.PriceView{
			View:   event.ViewRaw,
			Open:   50.0,
			High:   52.5,
			Low:    49.5,
			Close:  52.0,
			Volume: 1_000_000,
		},
	}
}

func TestCompletedBarPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CompletedBarPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing instrument id",
			mutate:  func(b *event.CompletedBarPayload) { b.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "zero period end",
			mutate:  func(b *event.CompletedBarPayload) { b.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			name:    "missing split-adjusted view entirely",
			mutate:  func(b *event.CompletedBarPayload) { b.SplitAdjusted = event.PriceView{} },
			wantErr: "split-adjusted view: view label is required",
		},
		{
			name:    "missing raw view entirely",
			mutate:  func(b *event.CompletedBarPayload) { b.Raw = event.PriceView{} },
			wantErr: "raw view: view label is required",
		},
		{
			name:    "unlabelled split-adjusted view",
			mutate:  func(b *event.CompletedBarPayload) { b.SplitAdjusted.View = "" },
			wantErr: "split-adjusted view: view label is required",
		},
		{
			name:    "unlabelled raw view",
			mutate:  func(b *event.CompletedBarPayload) { b.Raw.View = "" },
			wantErr: "raw view: view label is required",
		},
		{
			name:    "split-adjusted view mislabelled as raw",
			mutate:  func(b *event.CompletedBarPayload) { b.SplitAdjusted.View = event.ViewRaw },
			wantErr: `split-adjusted view: view label "raw" does not match expected "split-adjusted"`,
		},
		{
			name:    "raw view mislabelled as split-adjusted",
			mutate:  func(b *event.CompletedBarPayload) { b.Raw.View = event.ViewSplitAdjusted },
			wantErr: `raw view: view label "split-adjusted" does not match expected "raw"`,
		},
		{
			name:    "split-adjusted high below low",
			mutate:  func(b *event.CompletedBarPayload) { b.SplitAdjusted.High = 98.0 },
			wantErr: "split-adjusted view: high must be at least low",
		},
		{
			name:    "split-adjusted high below open",
			mutate:  func(b *event.CompletedBarPayload) { b.SplitAdjusted.Open = 106.0 },
			wantErr: "split-adjusted view: high must be at least open",
		},
		{
			name:    "split-adjusted high below close",
			mutate:  func(b *event.CompletedBarPayload) { b.SplitAdjusted.Close = 106.0 },
			wantErr: "split-adjusted view: high must be at least close",
		},
		{
			name:    "split-adjusted low above open",
			mutate:  func(b *event.CompletedBarPayload) { b.SplitAdjusted.Open = 98.5; b.SplitAdjusted.Low = 99.0 },
			wantErr: "split-adjusted view: low must be at most open",
		},
		{
			name:    "split-adjusted low above close",
			mutate:  func(b *event.CompletedBarPayload) { b.SplitAdjusted.Close = 98.5; b.SplitAdjusted.Low = 99.0 },
			wantErr: "split-adjusted view: low must be at most close",
		},
		{
			name:    "split-adjusted negative volume",
			mutate:  func(b *event.CompletedBarPayload) { b.SplitAdjusted.Volume = -1 },
			wantErr: "split-adjusted view: volume must not be negative",
		},
		{
			name:    "split-adjusted zero open",
			mutate:  func(b *event.CompletedBarPayload) { b.SplitAdjusted.Open = 0 },
			wantErr: "split-adjusted view: open must be positive",
		},
		{
			name:    "raw high below low",
			mutate:  func(b *event.CompletedBarPayload) { b.Raw.High = 49.0 },
			wantErr: "raw view: high must be at least low",
		},
		{
			name:    "raw negative volume",
			mutate:  func(b *event.CompletedBarPayload) { b.Raw.Volume = -1 },
			wantErr: "raw view: volume must not be negative",
		},
		{
			name:    "raw zero close",
			mutate:  func(b *event.CompletedBarPayload) { b.Raw.Close = 0 },
			wantErr: "raw view: close must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bar := validCompletedBar()
			if tt.mutate != nil {
				tt.mutate(&bar)
			}
			err := bar.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// A NaN or infinite value must be rejected explicitly, before the range
// checks: ordered float comparisons against NaN are always false in Go, so
// without an explicit finiteness check a NaN price would pass every range
// check silently and poison every downstream sizing calculation. +Inf and
// -Inf are likewise rejected rather than treated as very large/small but
// otherwise valid prices.
func TestCompletedBarPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name  string
		apply func(v *event.PriceView, f float64)
	}
	fields := []fieldCase{
		{"open", func(v *event.PriceView, f float64) { v.Open = f }},
		{"high", func(v *event.PriceView, f float64) { v.High = f }},
		{"low", func(v *event.PriceView, f float64) { v.Low = f }},
		{"close", func(v *event.PriceView, f float64) { v.Close = f }},
		{"volume", func(v *event.PriceView, f float64) { v.Volume = f }},
	}

	nonFinite := []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	}

	views := []struct {
		label string
		get   func(b *event.CompletedBarPayload) *event.PriceView
	}{
		{"split-adjusted view", func(b *event.CompletedBarPayload) *event.PriceView { return &b.SplitAdjusted }},
		{"raw view", func(b *event.CompletedBarPayload) *event.PriceView { return &b.Raw }},
	}

	for _, view := range views {
		for _, field := range fields {
			for _, nf := range nonFinite {
				t.Run(view.label+" "+field.name+" "+nf.name, func(t *testing.T) {
					t.Parallel()
					bar := validCompletedBar()
					field.apply(view.get(&bar), nf.value)

					err := bar.Validate()
					wantErr := view.label + ": " + field.name + " must be finite"
					if err == nil || !strings.Contains(err.Error(), wantErr) {
						t.Fatalf("Validate() error = %v, want substring %q", err, wantErr)
					}
				})
			}
		}
	}
}

// A bar missing both views must report both, not just the first, so an
// operator sees the whole gap in one pass.
func TestCompletedBarPayloadValidateAggregatesBothMissingViews(t *testing.T) {
	t.Parallel()

	bar := validCompletedBar()
	bar.SplitAdjusted = event.PriceView{}
	bar.Raw = event.PriceView{}

	err := bar.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{"split-adjusted view: view label is required", "raw view: view label is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestCompletedBarEventConstants(t *testing.T) {
	t.Parallel()

	if event.CompletedBarEventType == "" {
		t.Fatal("CompletedBarEventType must not be empty")
	}
	if event.CompletedBarSchemaVersion == 0 {
		t.Fatal("CompletedBarSchemaVersion must be positive")
	}
}

// Round-trip stability: encoding then decoding a valid payload must reproduce
// the exact same bytes on re-encoding. Floats are compared only by way of
// their encoded representation, never with ==, per the project's numeric
// policy.
func TestCompletedBarPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validCompletedBar()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.CompletedBarPayload
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded.Validate() error = %v", err)
	}

	reEncoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-Marshal() error = %v", err)
	}

	if !bytes.Equal(encoded, reEncoded) {
		t.Fatalf("round trip not stable:\n  first:  %s\n  second: %s", encoded, reEncoded)
	}
}

// The JSON wire format uses snake_case tags, matching the envelope's
// convention.
func TestCompletedBarPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validCompletedBar())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{"instrument_id", "period_end", "split_adjusted", "raw"} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}

	splitAdjusted, ok := asMap["split_adjusted"].(map[string]any)
	if !ok {
		t.Fatalf("split_adjusted is not an object: %s", encoded)
	}
	for _, key := range []string{"view", "open", "high", "low", "close", "volume"} {
		if _, ok := splitAdjusted[key]; !ok {
			t.Errorf("encoded split_adjusted view missing expected key %q: %s", key, encoded)
		}
	}
}

package event_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validAdjustedBar is version 2 of market.bar.completed: version 1's views
// and the split- and dividend-adjusted view, based at a session before the bar.
func validAdjustedBar() event.CompletedBarAdjustedPayload {
	return event.CompletedBarAdjustedPayload{
		CompletedBarPayload: validCompletedBar(),
		Adjusted: event.PriceView{
			View: event.ViewAdjusted, Open: 101, High: 106, Low: 100, Close: 105, Volume: 1_000_000,
		},
		AdjustedBase: "2026-01-02",
	}
}

func TestAdjustedBarVersionFollowsTheTurtlesVersion(t *testing.T) {
	t.Parallel()
	if event.CompletedBarAdjustedVersion != 2 || event.CompletedBarSchemaVersion != 1 {
		t.Fatalf("versions %d and %d; want the adjusted bar at 2 beside the Turtle's 1",
			event.CompletedBarAdjustedVersion, event.CompletedBarSchemaVersion)
	}
}

func TestAdjustedBarValidate(t *testing.T) {
	t.Parallel()
	if err := validAdjustedBar().Validate(); err != nil {
		t.Fatalf("valid adjusted bar: %v", err)
	}
	cases := map[string]struct {
		mutate func(*event.CompletedBarAdjustedPayload)
		want   string
	}{
		"no adjusted view":       {func(b *event.CompletedBarAdjustedPayload) { b.Adjusted = event.PriceView{} }, "adjusted view: view label is required"},
		"mislabelled view":       {func(b *event.CompletedBarAdjustedPayload) { b.Adjusted.View = event.ViewSplitAdjusted }, "adjusted view: view label"},
		"inconsistent view":      {func(b *event.CompletedBarAdjustedPayload) { b.Adjusted.High = 99 }, "adjusted view: high must be at least low"},
		"NaN adjusted close":     {func(b *event.CompletedBarAdjustedPayload) { b.Adjusted.Close = math.NaN() }, "adjusted view: close must be finite"},
		"version 1 fields":       {func(b *event.CompletedBarAdjustedPayload) { b.Raw = event.PriceView{} }, "raw view: view label is required"},
		"no base":                {func(b *event.CompletedBarAdjustedPayload) { b.AdjustedBase = "" }, "adjusted base"},
		"base not a date":        {func(b *event.CompletedBarAdjustedPayload) { b.AdjustedBase = "2026-13-01" }, "adjusted base"},
		"base after the session": {func(b *event.CompletedBarAdjustedPayload) { b.AdjustedBase = "2026-09-09" }, "after the bar's session"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := validAdjustedBar()
			c.mutate(&b)
			err := b.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate() = %v; want an error containing %q", err, c.want)
			}
		})
	}
	same := validAdjustedBar()
	same.AdjustedBase = same.PeriodEnd.UTC().Format("2006-01-02")
	if err := same.Validate(); err != nil {
		t.Fatalf("a bar on its base session: %v", err)
	}
}

func TestAdjustedBarWireShape(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(validAdjustedBar())
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"instrument_id", "period_end", "split_adjusted", "raw", "adjusted", "adjusted_base"} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing %q: %s", key, encoded)
		}
	}
	if len(asMap) != 6 {
		t.Errorf("encoded payload has %d keys, want 6: %s", len(asMap), encoded)
	}
	if view := asMap["adjusted"].(map[string]any)["view"]; view != "split-and-dividend-adjusted" {
		t.Errorf("adjusted view label %v", view)
	}
}

// A version 1 payload decodes as version 2 with no adjusted view and is
// refused, so a consumer of version 2 never mistakes a version 1 bar's
// split-adjusted close for an adjusted one.
func TestAdjustedBarRefusesAVersion1Payload(t *testing.T) {
	t.Parallel()
	v1, err := json.Marshal(validCompletedBar())
	if err != nil {
		t.Fatal(err)
	}
	var b event.CompletedBarAdjustedPayload
	if err := json.Unmarshal(v1, &b); err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "adjusted view") {
		t.Fatalf("Validate() = %v; want the missing adjusted view refused", err)
	}
	var old event.CompletedBarPayload
	if err := json.Unmarshal(v1, &old); err != nil || old.Validate() != nil {
		t.Fatalf("version 1 no longer decodes: %v", err)
	}
}

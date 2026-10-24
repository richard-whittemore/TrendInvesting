package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestShippedDryRunWarmUpLiesInsideMassivesFreeWindow holds the shipped
// dry-run configuration's warm-up start to the two-year history window of the
// market-data plan: the shipped warm_up_start must lie inside that window.
// The window is measured from a fixed reference date so the test never reads
// the wall clock; when the reference date moves forward, the shipped value
// must move with it. The manual's market-data chapter explains the rule.
func TestShippedDryRunWarmUpLiesInsideMassivesFreeWindow(t *testing.T) {
	reference := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	raw, err := os.ReadFile(boostDeployCfg)
	if err != nil {
		t.Fatal(err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("the shipped dry-run configuration does not decode: %v", err)
	}
	warmUp, err := cfg.Boost.WarmUpDate()
	if err != nil {
		t.Fatal(err)
	}
	if err := warmUpWindowError(warmUp, reference); err != nil {
		t.Errorf("the shipped warm_up_start %s %v", cfg.Boost.WarmUpStart, err)
	}
}

// warmUpWindowError reports why warmUp lies outside the two-year history
// window that ends at reference, or nil when it lies inside.
func warmUpWindowError(warmUp, reference time.Time) error {
	if earliest := reference.AddDate(-2, 0, 0); warmUp.Before(earliest) {
		return fmt.Errorf("is before %s, outside Massive's two-year window", earliest.Format("2006-01-02"))
	}
	if warmUp.After(reference) {
		return fmt.Errorf("is after the reference date %s", reference.Format("2006-01-02"))
	}
	return nil
}

func TestWarmUpWindowError(t *testing.T) {
	reference := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		warmUp  time.Time
		wantErr bool
	}{
		{"inside the window", time.Date(2025, time.January, 2, 0, 0, 0, 0, time.UTC), false},
		{"on the earliest day of the window", time.Date(2024, time.October, 23, 0, 0, 0, 0, time.UTC), false},
		{"before the window", time.Date(2024, time.October, 22, 0, 0, 0, 0, time.UTC), true},
		{"on the reference date", time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC), false},
		{"after the reference date", time.Date(2026, time.October, 24, 0, 0, 0, 0, time.UTC), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := warmUpWindowError(tc.warmUp, reference)
			if (err != nil) != tc.wantErr {
				t.Errorf("warmUpWindowError(%s) error = %v, want error %v",
					tc.warmUp.Format("2006-01-02"), err, tc.wantErr)
			}
		})
	}
}

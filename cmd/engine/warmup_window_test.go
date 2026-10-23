package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestShippedDryRunWarmUpLiesInsideMassivesFreeWindow holds the shipped
// dry-run configuration's warm-up start to the two years of history the
// owner's Massive plan reaches back (owner decision 2026-10-23), measured from
// a fixed reference date so the test never reads the wall clock. When the
// reference date moves past 2 January 2027 the shipped value must move too.
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
	if earliest := reference.AddDate(-2, 0, 0); warmUp.Before(earliest) {
		t.Errorf("the shipped warm_up_start %s is before %s, outside Massive's two-year window",
			cfg.Boost.WarmUpStart, earliest.Format("2006-01-02"))
	}
}

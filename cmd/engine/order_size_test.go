package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestShippedBoostLimitsPassAStepFromNoHoldingToTheBoost holds the shipped
// dry-run configuration to ADR 0026's rule that a limit refusing an order the
// rule itself makes is a configuration error (amendment, 2026-11-04): a run
// that starts with no SPY while the boost is latched buys the whole boosted
// weight in one order, so the order-size limit must admit it, and the
// exposure limit must too.
func TestShippedBoostLimitsPassAStepFromNoHoldingToTheBoost(t *testing.T) {
	raw, err := os.ReadFile(boostDeployCfg)
	if err != nil {
		t.Fatal(err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("the shipped dry-run configuration does not decode: %v", err)
	}
	boosted := cfg.Boost.InvestedFraction * cfg.Boost.BoostSize
	if cfg.Limits.MaxOrderFraction < boosted {
		t.Errorf("max_order_fraction %v refuses the step from no holding to the boost (%v of net liquidation)",
			cfg.Limits.MaxOrderFraction, boosted)
	}
	if cfg.Limits.MaxExposure < boosted {
		t.Errorf("max_exposure %v refuses the boost itself (%v of net liquidation)", cfg.Limits.MaxExposure, boosted)
	}
}

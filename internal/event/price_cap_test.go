package event_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestConfigurationGapBufferFollowsItsOrderType pins ADR 0005's amended
// configuration: a stop-limit's gap buffer is finite and at least zero, a
// stop-market order states none, and a record without an order type, which
// is what a schema-5 record decodes to, is rejected rather than read as
// either.
func TestConfigurationGapBufferFollowsItsOrderType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		orderType event.OrderType
		gapBuffer float64
		wantErr   string
	}{
		{"the Baseline's 1N stop-limit", event.OrderTypeStopLimit, 1, ""},
		{"a stop-limit capped at its own level", event.OrderTypeStopLimit, 0, ""},
		{"a 2N stop-limit", event.OrderTypeStopLimit, 2, ""},
		{"the uncapped Variant", event.OrderTypeStopMarket, 0, ""},
		{"a negative gap buffer", event.OrderTypeStopLimit, -0.5, "must not be negative"},
		{"a NaN gap buffer", event.OrderTypeStopLimit, math.NaN(), "must be finite"},
		{"an infinite gap buffer", event.OrderTypeStopLimit, math.Inf(1), "must be finite"},
		{"a stop-market order stating a buffer", event.OrderTypeStopMarket, 1, "must be zero for a stop-market order"},
		{"no order type, as a schema-5 record decodes", "", 0, "is not a recognised order type"},
		{"an unknown order type", "limit", 1, "is not a recognised order type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfiguration()
			c.BuyOrderType = tt.orderType
			c.GapBufferN = tt.gapBuffer
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestConfigurationRecordedBeforeThePriceCapIsRejected decodes a schema-5
// record, which has neither field, and requires it to fail closed (ADR
// 0015).
func TestConfigurationRecordedBeforeThePriceCapIsRejected(t *testing.T) {
	t.Parallel()

	c := validConfiguration()
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "buy_order_type")
	delete(fields, "gap_buffer_n")
	older, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var decoded event.ConfigurationPayload
	if err := json.Unmarshal(older, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err == nil || !strings.Contains(err.Error(), "buy order type") {
		t.Fatalf("a record without a buy order type validated as %v; want it rejected", err)
	}
}

// TestProposalPriceCapMatchesItsDerivation is invariant 5 of
// TradeProposalPayload.Validate and its Add counterpart: a stop-limit's cap
// is exactly its level plus the gap buffer in N (the decision N for an
// entry, the Campaign's frozen N for an Add), and a stop-market proposal
// carries no cap (ADR 0005, as amended 2026-09-24).
func TestProposalPriceCapMatchesItsDerivation(t *testing.T) {
	t.Parallel()

	type caps struct {
		orderType event.OrderType
		gapBuffer float64
		priceCap  float64
	}
	entryLevel, addLevel := validTradeProposal().EntryLevel, validAddProposal().Level
	n := proposalN
	tests := []struct {
		name    string
		entry   caps
		add     caps
		wantErr string
	}{
		{name: "the Baseline's 1N cap",
			entry: caps{event.OrderTypeStopLimit, 1, entryLevel + float64(1*n)},
			add:   caps{event.OrderTypeStopLimit, 1, addLevel + float64(1*n)}},
		{name: "a half-N cap",
			entry: caps{event.OrderTypeStopLimit, 0.5, entryLevel + float64(0.5*n)},
			add:   caps{event.OrderTypeStopLimit, 0.5, addLevel + float64(0.5*n)}},
		{name: "a cap at the level itself",
			entry: caps{event.OrderTypeStopLimit, 0, entryLevel},
			add:   caps{event.OrderTypeStopLimit, 0, addLevel}},
		{name: "the uncapped Variant",
			entry: caps{event.OrderTypeStopMarket, 0, 0},
			add:   caps{event.OrderTypeStopMarket, 0, 0}},
		{name: "a cap one ulp off its derivation",
			entry:   caps{event.OrderTypeStopLimit, 1, math.Nextafter(entryLevel+float64(1*n), math.Inf(1))},
			add:     caps{event.OrderTypeStopLimit, 1, math.Nextafter(addLevel+float64(1*n), math.Inf(1))},
			wantErr: "does not match the derivation"},
		{name: "a cap measured in the wrong buffer",
			entry:   caps{event.OrderTypeStopLimit, 2, entryLevel + float64(1*n)},
			add:     caps{event.OrderTypeStopLimit, 2, addLevel + float64(1*n)},
			wantErr: "does not match the derivation"},
		{name: "a stop-market proposal stating a cap",
			entry:   caps{event.OrderTypeStopMarket, 0, entryLevel},
			add:     caps{event.OrderTypeStopMarket, 0, addLevel},
			wantErr: "price cap must be zero"},
		{name: "a non-finite cap",
			entry:   caps{event.OrderTypeStopLimit, 1, math.Inf(1)},
			add:     caps{event.OrderTypeStopLimit, 1, math.NaN()},
			wantErr: "price cap must be finite"},
		{name: "no order type, as a schema-1 record decodes",
			entry:   caps{"", 0, 0},
			add:     caps{"", 0, 0},
			wantErr: "is not a recognised order type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entry := validTradeProposal()
			entry.OrderType, entry.GapBufferN, entry.PriceCap = tt.entry.orderType, tt.entry.gapBuffer, tt.entry.priceCap
			add := validAddProposal()
			add.OrderType, add.GapBufferN, add.PriceCap = tt.add.orderType, tt.add.gapBuffer, tt.add.priceCap
			for name, err := range map[string]error{"trade proposal": entry.Validate(), "add proposal": add.Validate()} {
				if tt.wantErr == "" {
					if err != nil {
						t.Errorf("%s: Validate() = %v, want nil", name, err)
					}
					continue
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("%s: Validate() = %v, want an error containing %q", name, err, tt.wantErr)
				}
			}
		})
	}
}

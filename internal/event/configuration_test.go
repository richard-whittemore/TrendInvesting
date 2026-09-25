package event_test

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validConfiguration returns a Baseline configuration payload (ADRs
// 0002/0003/0005/0007/0013) that satisfies every validation rule so each
// table test only needs to describe its one deviation.
func validConfiguration() event.ConfigurationPayload {
	return event.ConfigurationPayload{
		StrategyID:             "turtle-baseline",
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2.0,
		EntryChannelLength:     55,
		ExitChannelLength:      20,
		MaxUnits:               4,
		// ADR 0008's other three Unit caps: 6 Units per industry, 10 per
		// sector (also the Unclassified Group's cap, at the same
		// loosely-correlated level), 12 total long.
		MaxUnitsPerIndustry: 6,
		MaxUnitsPerSector:   10,
		MaxUnitsTotalLong:   12,
		SlippageN:           0.05,
		// #9: 1.0 is a Baseline-declared adaptation (ADR 0012's provenance
		// taxonomy), not a Faith number — used here only as a test fixture
		// default. Whoever owns the Baseline configuration (#50) must pick
		// this deliberately.
		TierBDistanceInN: 1.0,
		// #10: for US equities one point of price is exactly one dollar per
		// share, so the Baseline's multiplier is 1. It is a parameter, not a
		// hard-coded constant, because Faith's futures examples need the
		// contract multiplier (42,000 for Heating Oil, The Turtle Rules
		// p.15).
		DollarsPerPoint: 1,
		// #10: zero, and required to be zero, while the Sizing Mode is
		// volatility-normalised — Risk at Stop is derived there, never
		// configured (ADR 0003).
		RiskAtStopFraction: 0,
		NotionalAccount: event.NotionalAccountConfig{
			StartingEquity: 1_000_000,
			RebasingMonth:  1,
			RebasingDay:    1,
		},
		// #18: ADR 0013's commission model, as Interactive Brokers' published
		// US-stock Fixed pricing stood on 2026-09-11 — $0.005 per share, a
		// $1.00 minimum per order, capped at 1 % of trade value
		// (https://www.interactivebrokers.com/en/pricing/commissions-stocks.php).
		// A Baseline-declared adaptation under ADR 0012's provenance taxonomy:
		// the figures come from the broker's published schedule, not from a
		// methodology source, and every test that depends on them
		// parameterises them rather than folding them in.
		Commission: event.CommissionConfig{
			PerShare:                    0.005,
			MinimumPerOrder:             1.00,
			MaximumFractionOfTradeValue: 0.01,
		},
		// ADR 0005, as amended 2026-09-24: entries and Adds rest as
		// stop-limit orders capped at level + 1N in the Baseline.
		BuyOrderType: event.OrderTypeStopLimit,
		GapBufferN:   1,
		// ADR 0009's universe gate, all zero: off, as the owner's decision of
		// 2026-09-25 makes every existing fixture. A test of the gate itself
		// sets all three positive explicitly.
		UniverseMinPrice:        0,
		UniverseMinDollarVolume: 0,
		UniverseMinHistoryBars:  0,
	}
}

func TestConfigurationPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.ConfigurationPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing strategy id",
			mutate:  func(c *event.ConfigurationPayload) { c.StrategyID = "" },
			wantErr: "strategy id",
		},
		{
			name:    "strategy id with space",
			mutate:  func(c *event.ConfigurationPayload) { c.StrategyID = "my strategy" },
			wantErr: "valid identifier",
		},
		{
			name:    "strategy id with tab",
			mutate:  func(c *event.ConfigurationPayload) { c.StrategyID = "my\tstrategy" },
			wantErr: "valid identifier",
		},
		{
			name:    "strategy id with newline",
			mutate:  func(c *event.ConfigurationPayload) { c.StrategyID = "my\nstrategy" },
			wantErr: "valid identifier",
		},
		{
			name:    "strategy id with control character",
			mutate:  func(c *event.ConfigurationPayload) { c.StrategyID = "strategy\x01" },
			wantErr: "valid identifier",
		},
		{
			name: "strategy id too long",
			mutate: func(c *event.ConfigurationPayload) {
				c.StrategyID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}, // 65 chars
			wantErr: "valid identifier",
		},
		{
			name: "strategy id at length limit is accepted",
			mutate: func(c *event.ConfigurationPayload) {
				c.StrategyID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}, // 64 chars
		},
		{
			name:    "invalid sizing mode",
			mutate:  func(c *event.ConfigurationPayload) { c.SizingMode = "risk-parity" },
			wantErr: "sizing mode",
		},
		{
			name:    "empty sizing mode",
			mutate:  func(c *event.ConfigurationPayload) { c.SizingMode = "" },
			wantErr: "sizing mode",
		},
		{
			// Under fixed-risk-at-stop the risk fraction is the sizing
			// input [M p.56], so it must be present and positive — the exact
			// opposite of the requirement under volatility-normalised.
			name: "fixed-risk-at-stop is accepted with a risk at stop fraction",
			mutate: func(c *event.ConfigurationPayload) {
				c.SizingMode = event.SizingModeFixedRiskAtStop
				c.RiskAtStopFraction = 0.02
			},
			wantErr: "",
		},
		{
			name:    "fixed-risk-at-stop without a risk at stop fraction",
			mutate:  func(c *event.ConfigurationPayload) { c.SizingMode = event.SizingModeFixedRiskAtStop },
			wantErr: "risk at stop fraction must be greater than zero and at most one under fixed-risk-at-stop sizing",
		},
		{
			name: "fixed-risk-at-stop with a risk at stop fraction above one",
			mutate: func(c *event.ConfigurationPayload) {
				c.SizingMode = event.SizingModeFixedRiskAtStop
				c.RiskAtStopFraction = 1.5
			},
			wantErr: "risk at stop fraction must be greater than zero and at most one under fixed-risk-at-stop sizing",
		},
		{
			name: "fixed-risk-at-stop with a negative risk at stop fraction",
			mutate: func(c *event.ConfigurationPayload) {
				c.SizingMode = event.SizingModeFixedRiskAtStop
				c.RiskAtStopFraction = -0.02
			},
			wantErr: "risk at stop fraction must be greater than zero and at most one under fixed-risk-at-stop sizing",
		},
		{
			// #10's named negative test: Risk at Stop must not be directly
			// configurable in the Baseline mode. ADR 0003 makes it a derived
			// quantity (Unit Volatility Fraction x Stop Multiple); a
			// configuration that also states it is rejected rather than
			// having the stated value quietly ignored, because a run that
			// believes it risks a figure nothing in the arithmetic honours is
			// exactly the defect ADR 0003 was written to prevent.
			name:    "volatility-normalised must not configure risk at stop",
			mutate:  func(c *event.ConfigurationPayload) { c.RiskAtStopFraction = 0.01 },
			wantErr: "risk at stop must not be configured under volatility-normalised sizing",
		},
		{
			// Even the value the derivation would produce (0.005 x 2 = 0.01)
			// is rejected: the rule is that the field is absent in this mode,
			// not that it must agree.
			name:    "volatility-normalised must not configure even the derived value",
			mutate:  func(c *event.ConfigurationPayload) { c.RiskAtStopFraction = 0.005 * 2 },
			wantErr: "risk at stop must not be configured under volatility-normalised sizing",
		},
		{
			// NaN is not zero, so the "must be absent" rule catches it too.
			name:    "volatility-normalised with a NaN risk at stop",
			mutate:  func(c *event.ConfigurationPayload) { c.RiskAtStopFraction = math.NaN() },
			wantErr: "risk at stop must not be configured under volatility-normalised sizing",
		},
		{
			name:    "zero dollars per point",
			mutate:  func(c *event.ConfigurationPayload) { c.DollarsPerPoint = 0 },
			wantErr: "dollars per point must be positive",
		},
		{
			name:    "negative dollars per point",
			mutate:  func(c *event.ConfigurationPayload) { c.DollarsPerPoint = -1 },
			wantErr: "dollars per point must be positive",
		},
		{
			// Faith's Heating Oil contract multiplier (The Turtle Rules
			// p.15) must be as configurable as the equity multiplier of 1.
			name:    "a futures contract multiplier is accepted",
			mutate:  func(c *event.ConfigurationPayload) { c.DollarsPerPoint = 42_000 },
			wantErr: "",
		},
		{
			name:    "zero unit volatility fraction",
			mutate:  func(c *event.ConfigurationPayload) { c.UnitVolatilityFraction = 0 },
			wantErr: "unit volatility fraction",
		},
		{
			name:    "negative unit volatility fraction",
			mutate:  func(c *event.ConfigurationPayload) { c.UnitVolatilityFraction = -0.01 },
			wantErr: "unit volatility fraction",
		},
		{
			name:    "unit volatility fraction above one",
			mutate:  func(c *event.ConfigurationPayload) { c.UnitVolatilityFraction = 1.5 },
			wantErr: "unit volatility fraction",
		},
		{
			name:    "zero stop multiple",
			mutate:  func(c *event.ConfigurationPayload) { c.StopMultiple = 0 },
			wantErr: "stop multiple",
		},
		{
			name:    "negative stop multiple",
			mutate:  func(c *event.ConfigurationPayload) { c.StopMultiple = -2 },
			wantErr: "stop multiple",
		},
		{
			name:    "zero entry channel length",
			mutate:  func(c *event.ConfigurationPayload) { c.EntryChannelLength = 0 },
			wantErr: "entry channel length",
		},
		{
			name:    "negative entry channel length",
			mutate:  func(c *event.ConfigurationPayload) { c.EntryChannelLength = -55 },
			wantErr: "entry channel length",
		},
		{
			name:    "zero exit channel length",
			mutate:  func(c *event.ConfigurationPayload) { c.ExitChannelLength = 0 },
			wantErr: "exit channel length",
		},
		{
			name:    "negative exit channel length",
			mutate:  func(c *event.ConfigurationPayload) { c.ExitChannelLength = -20 },
			wantErr: "exit channel length",
		},
		{
			name:    "zero max units",
			mutate:  func(c *event.ConfigurationPayload) { c.MaxUnits = 0 },
			wantErr: "maximum units",
		},
		{
			name:    "negative max units",
			mutate:  func(c *event.ConfigurationPayload) { c.MaxUnits = -4 },
			wantErr: "maximum units",
		},
		{
			name:    "zero max units per industry",
			mutate:  func(c *event.ConfigurationPayload) { c.MaxUnitsPerIndustry = 0 },
			wantErr: "maximum units per industry",
		},
		{
			name:    "negative max units per industry",
			mutate:  func(c *event.ConfigurationPayload) { c.MaxUnitsPerIndustry = -6 },
			wantErr: "maximum units per industry",
		},
		{
			name:    "zero max units per sector",
			mutate:  func(c *event.ConfigurationPayload) { c.MaxUnitsPerSector = 0 },
			wantErr: "maximum units per sector",
		},
		{
			name:    "negative max units per sector",
			mutate:  func(c *event.ConfigurationPayload) { c.MaxUnitsPerSector = -10 },
			wantErr: "maximum units per sector",
		},
		{
			name:    "zero max units total long",
			mutate:  func(c *event.ConfigurationPayload) { c.MaxUnitsTotalLong = 0 },
			wantErr: "maximum units total long",
		},
		{
			name:    "negative max units total long",
			mutate:  func(c *event.ConfigurationPayload) { c.MaxUnitsTotalLong = -12 },
			wantErr: "maximum units total long",
		},
		{
			name:    "zero slippage rejected per ADR 0013",
			mutate:  func(c *event.ConfigurationPayload) { c.SlippageN = 0 },
			wantErr: "slippage",
		},
		{
			name:    "negative slippage rejected",
			mutate:  func(c *event.ConfigurationPayload) { c.SlippageN = -0.05 },
			wantErr: "slippage",
		},
		{
			// #18/ADR 0013: a per-share rate of zero is a legitimate
			// commission-free Variant (some venues charge nothing), so it is
			// accepted rather than refused. It is the CAP below that fails
			// closed, because an all-zero commission block is what an older
			// record decodes to.
			name:    "zero per-share commission is accepted",
			mutate:  func(c *event.ConfigurationPayload) { c.Commission.PerShare = 0 },
			wantErr: "",
		},
		{
			name:    "negative per-share commission rejected",
			mutate:  func(c *event.ConfigurationPayload) { c.Commission.PerShare = -0.005 },
			wantErr: "commission per share",
		},
		{
			name:    "zero commission minimum per order is accepted",
			mutate:  func(c *event.ConfigurationPayload) { c.Commission.MinimumPerOrder = 0 },
			wantErr: "",
		},
		{
			name:    "negative commission minimum per order rejected",
			mutate:  func(c *event.ConfigurationPayload) { c.Commission.MinimumPerOrder = -1 },
			wantErr: "commission minimum per order",
		},
		{
			// The cap is what makes an omitted commission block fail closed:
			// a schema-3 record decodes every commission field as zero, and a
			// cap of zero would charge nothing at all however large the
			// per-share rate — a silently free backtest, which is the
			// commission-side twin of ADR 0013's zero-slippage rule.
			name:    "zero commission cap rejected",
			mutate:  func(c *event.ConfigurationPayload) { c.Commission.MaximumFractionOfTradeValue = 0 },
			wantErr: "commission maximum fraction of trade value",
		},
		{
			name:    "negative commission cap rejected",
			mutate:  func(c *event.ConfigurationPayload) { c.Commission.MaximumFractionOfTradeValue = -0.01 },
			wantErr: "commission maximum fraction of trade value",
		},
		{
			name:    "commission cap above one rejected",
			mutate:  func(c *event.ConfigurationPayload) { c.Commission.MaximumFractionOfTradeValue = 1.5 },
			wantErr: "commission maximum fraction of trade value",
		},
		{
			name:    "zero tier b distance in n is accepted",
			mutate:  func(c *event.ConfigurationPayload) { c.TierBDistanceInN = 0 },
			wantErr: "",
		},
		{
			name:    "negative tier b distance in n rejected",
			mutate:  func(c *event.ConfigurationPayload) { c.TierBDistanceInN = -1.0 },
			wantErr: "tier b distance in n",
		},
		{
			name:    "zero notional account starting equity",
			mutate:  func(c *event.ConfigurationPayload) { c.NotionalAccount.StartingEquity = 0 },
			wantErr: "notional account starting equity",
		},
		{
			name:    "negative notional account starting equity",
			mutate:  func(c *event.ConfigurationPayload) { c.NotionalAccount.StartingEquity = -1 },
			wantErr: "notional account starting equity",
		},
		{
			name:    "rebasing month zero",
			mutate:  func(c *event.ConfigurationPayload) { c.NotionalAccount.RebasingMonth = 0 },
			wantErr: "rebasing date",
		},
		{
			name:    "rebasing month thirteen",
			mutate:  func(c *event.ConfigurationPayload) { c.NotionalAccount.RebasingMonth = 13 },
			wantErr: "rebasing date",
		},
		{
			name:    "rebasing day zero",
			mutate:  func(c *event.ConfigurationPayload) { c.NotionalAccount.RebasingDay = 0 },
			wantErr: "rebasing date",
		},
		{
			name:    "rebasing day thirty-two",
			mutate:  func(c *event.ConfigurationPayload) { c.NotionalAccount.RebasingDay = 32 },
			wantErr: "rebasing date",
		},
		{
			name: "rebasing date february thirtieth does not exist",
			mutate: func(c *event.ConfigurationPayload) {
				c.NotionalAccount.RebasingMonth = 2
				c.NotionalAccount.RebasingDay = 30
			},
			wantErr: "rebasing date",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfiguration()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			err := cfg.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// An entirely empty configuration must report every missing or invalid field
// in one aggregated error.
// A NaN or infinite value must be rejected explicitly, before the range
// checks: ordered float comparisons against NaN are always false in Go, so
// without an explicit finiteness check a NaN parameter would pass every
// range check silently and poison every downstream sizing calculation. +Inf
// and -Inf are likewise rejected rather than treated as very large/small but
// otherwise valid parameters.
func TestConfigurationPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name    string
		apply   func(c *event.ConfigurationPayload, f float64)
		wantErr string
	}
	fields := []fieldCase{
		{
			name:    "unit volatility fraction",
			apply:   func(c *event.ConfigurationPayload, f float64) { c.UnitVolatilityFraction = f },
			wantErr: "unit volatility fraction must be finite",
		},
		{
			name:    "stop multiple",
			apply:   func(c *event.ConfigurationPayload, f float64) { c.StopMultiple = f },
			wantErr: "stop multiple must be finite",
		},
		{
			name:    "slippage",
			apply:   func(c *event.ConfigurationPayload, f float64) { c.SlippageN = f },
			wantErr: "slippage must be finite",
		},
		{
			name:    "notional account starting equity",
			apply:   func(c *event.ConfigurationPayload, f float64) { c.NotionalAccount.StartingEquity = f },
			wantErr: "notional account starting equity must be finite",
		},
		{
			name:    "tier b distance in n",
			apply:   func(c *event.ConfigurationPayload, f float64) { c.TierBDistanceInN = f },
			wantErr: "tier b distance in n must be finite",
		},
		{
			name:    "dollars per point",
			apply:   func(c *event.ConfigurationPayload, f float64) { c.DollarsPerPoint = f },
			wantErr: "dollars per point must be finite",
		},
		{
			name:    "commission per share",
			apply:   func(c *event.ConfigurationPayload, f float64) { c.Commission.PerShare = f },
			wantErr: "commission per share must be finite",
		},
		{
			name:    "commission minimum per order",
			apply:   func(c *event.ConfigurationPayload, f float64) { c.Commission.MinimumPerOrder = f },
			wantErr: "commission minimum per order must be finite",
		},
		{
			name:    "commission maximum fraction of trade value",
			apply:   func(c *event.ConfigurationPayload, f float64) { c.Commission.MaximumFractionOfTradeValue = f },
			wantErr: "commission maximum fraction of trade value must be finite",
		},
		{
			// Only meaningful in the mode where the field is an input at
			// all; under volatility-normalised a NaN is rejected by the
			// must-be-absent rule instead (see the table above).
			name: "risk at stop fraction under fixed-risk-at-stop",
			apply: func(c *event.ConfigurationPayload, f float64) {
				c.SizingMode = event.SizingModeFixedRiskAtStop
				c.RiskAtStopFraction = f
			},
			wantErr: "risk at stop fraction must be finite",
		},
	}

	nonFinite := []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	}

	for _, field := range fields {
		for _, nf := range nonFinite {
			t.Run(field.name+" "+nf.name, func(t *testing.T) {
				t.Parallel()
				cfg := validConfiguration()
				field.apply(&cfg, nf.value)

				err := cfg.Validate()
				if err == nil || !strings.Contains(err.Error(), field.wantErr) {
					t.Fatalf("Validate() error = %v, want substring %q", err, field.wantErr)
				}
			})
		}
	}
}

func TestConfigurationPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var cfg event.ConfigurationPayload

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"strategy id",
		"sizing mode",
		"unit volatility fraction",
		"stop multiple",
		"entry channel length",
		"exit channel length",
		"maximum units",
		"slippage",
		"dollars per point",
		"notional account starting equity",
		"rebasing date",
		"commission maximum fraction of trade value",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestConfigurationEventConstants(t *testing.T) {
	t.Parallel()

	if event.ConfigurationEventType == "" {
		t.Fatal("ConfigurationEventType must not be empty")
	}
	if event.ConfigurationSchemaVersion == 0 {
		t.Fatal("ConfigurationSchemaVersion must be positive")
	}
}

// TestConfigurationSchemaVersionBumpedForSizingFields pins the explicit
// schema bumps this payload has taken: 2 for #9's TierBDistanceInN, 3 for
// #10's DollarsPerPoint and RiskAtStopFraction, 4 for #18's Commission, and 5
// for #55's MaxUnitsPerIndustry/MaxUnitsPerSector/MaxUnitsTotalLong, 6
// for BuyOrderType and GapBufferN (ADR 0005, as amended 2026-09-24), and 7
// for ADR 0009's three universe thresholds.
// A new field on an existing payload always changes the schema version
// (docs/development.md: a schema change is explicit in this project, never a
// silent field addition). For every bump through 6, the version must
// change, because every new field decodes as the zero value from an older
// record — a zero DollarsPerPoint divides by zero, a zero
// RiskAtStopFraction would size a fixed-risk-at-stop Unit from a risk
// budget of nothing, a zero commission cap would charge nothing at all on
// every order, and a zero Unit cap is not a legitimate limit at all
// (ConfigurationPayload.Validate's own doc comment). Version 7 is
// documentation rather than a rejection: its three fields' own zero value
// is a legitimate declared Variant (ConfigurationSchemaVersion's own
// version-7 note), so an older record recorded before they existed decodes
// them as zero and Validate accepts that exactly as it accepts any other
// configured zero for them.
func TestConfigurationSchemaVersionBumpedForSizingFields(t *testing.T) {
	t.Parallel()

	if event.ConfigurationSchemaVersion != 7 {
		t.Fatalf("ConfigurationSchemaVersion = %d, want 7", event.ConfigurationSchemaVersion)
	}
}

// TestConfigurationPayloadValidateUniverseThresholds pins ADR 0009's three
// universe thresholds' own rule, as amended 2026-09-25 (the owner's
// decision): each is finite and not negative, and the three switch
// together — all zero (the gate off, validConfiguration's own default) or
// all positive (the gate on) — with a partial mix, some zero and some
// positive, refused outright.
func TestConfigurationPayloadValidateUniverseThresholds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.ConfigurationPayload)
		wantErr string
	}{
		{
			name: "every universe threshold at zero is accepted (the gate is off)",
			mutate: func(p *event.ConfigurationPayload) {
				p.UniverseMinPrice = 0
				p.UniverseMinDollarVolume = 0
				p.UniverseMinHistoryBars = 0
			},
		},
		{
			name: "every universe threshold positive is accepted (the gate is on)",
			mutate: func(p *event.ConfigurationPayload) {
				p.UniverseMinPrice = 5
				p.UniverseMinDollarVolume = 5_000_000
				p.UniverseMinHistoryBars = 250
			},
		},
		{
			name: "price alone positive is a partial configuration and is refused",
			mutate: func(p *event.ConfigurationPayload) {
				p.UniverseMinPrice = 5
			},
			wantErr: "universe thresholds must be either all zero",
		},
		{
			name: "dollar volume alone positive is a partial configuration and is refused",
			mutate: func(p *event.ConfigurationPayload) {
				p.UniverseMinDollarVolume = 5_000_000
			},
			wantErr: "universe thresholds must be either all zero",
		},
		{
			name: "history bars alone positive is a partial configuration and is refused",
			mutate: func(p *event.ConfigurationPayload) {
				p.UniverseMinHistoryBars = 250
			},
			wantErr: "universe thresholds must be either all zero",
		},
		{
			name: "two of three positive, one still zero, is a partial configuration and is refused",
			mutate: func(p *event.ConfigurationPayload) {
				p.UniverseMinPrice = 5
				p.UniverseMinDollarVolume = 5_000_000
			},
			wantErr: "universe thresholds must be either all zero",
		},
		{
			name:    "negative universe min price",
			mutate:  func(p *event.ConfigurationPayload) { p.UniverseMinPrice = -1 },
			wantErr: "universe min price must not be negative",
		},
		{
			name:    "non-finite universe min price",
			mutate:  func(p *event.ConfigurationPayload) { p.UniverseMinPrice = math.NaN() },
			wantErr: "universe min price must be finite",
		},
		{
			name:    "negative universe min dollar volume",
			mutate:  func(p *event.ConfigurationPayload) { p.UniverseMinDollarVolume = -1 },
			wantErr: "universe min dollar volume must not be negative",
		},
		{
			name:    "non-finite universe min dollar volume",
			mutate:  func(p *event.ConfigurationPayload) { p.UniverseMinDollarVolume = math.Inf(1) },
			wantErr: "universe min dollar volume must be finite",
		},
		{
			name:    "negative universe min history bars",
			mutate:  func(p *event.ConfigurationPayload) { p.UniverseMinHistoryBars = -1 },
			wantErr: "universe min history bars must not be negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validConfiguration()
			tt.mutate(&payload)
			err := payload.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestConfigurationRecordedBeforeCommissionIsRejected is the #18 half of the
// bump above, asserted through Validate rather than through the version
// constant: a record written before the commission block existed decodes
// every one of its fields as zero, and the zero cap is what makes such a
// record fail closed instead of running a backtest that charges nothing.
func TestConfigurationRecordedBeforeCommissionIsRejected(t *testing.T) {
	t.Parallel()

	payload := validConfiguration()
	payload.Commission = event.CommissionConfig{}

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want an error for a configuration with no commission model")
	}
	if !strings.Contains(err.Error(), "commission maximum fraction of trade value") {
		t.Errorf("Validate() error = %v, want it to name the commission cap", err)
	}
}

// Round-trip stability: encoding then decoding a valid payload must reproduce
// the exact same bytes on re-encoding. Floats are compared only by way of
// their encoded representation, never with ==, per the project's numeric
// policy.
func TestConfigurationPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validConfiguration()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.ConfigurationPayload
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
func TestConfigurationPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validConfiguration())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{
		"strategy_id",
		"sizing_mode",
		"unit_volatility_fraction",
		"stop_multiple",
		"entry_channel_length",
		"exit_channel_length",
		"max_units",
		"slippage_n",
		"tier_b_distance_in_n",
		"dollars_per_point",
		"risk_at_stop_fraction",
		"notional_account",
		"commission",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}

	notionalAccount, ok := asMap["notional_account"].(map[string]any)
	if !ok {
		t.Fatalf("notional_account is not an object: %s", encoded)
	}
	for _, key := range []string{"starting_equity", "rebasing_month", "rebasing_day"} {
		if _, ok := notionalAccount[key]; !ok {
			t.Errorf("encoded notional_account missing expected key %q: %s", key, encoded)
		}
	}

	commission, ok := asMap["commission"].(map[string]any)
	if !ok {
		t.Fatalf("commission is not an object: %s", encoded)
	}
	for _, key := range []string{"per_share", "minimum_per_order", "maximum_fraction_of_trade_value"} {
		if _, ok := commission[key]; !ok {
			t.Errorf("encoded commission missing expected key %q: %s", key, encoded)
		}
	}
}

package event_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validWatchlistPublished returns a payload that satisfies every validation
// rule so each table test only needs to describe its one deviation: two
// entries, Tier A ranked ahead of Tier B by Strength, descending.
func validWatchlistPublished() event.WatchlistPublishedPayload {
	return event.WatchlistPublishedPayload{
		PeriodEnd: time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC),
		Rule:      event.RuleWatchlistRankedByStrength,
		ADR:       event.ADRWatchlistRankedByStrength,
		Entries: []event.WatchlistEntry{
			{InstrumentID: "AAPL", Tier: event.TierA, DistanceToEntryInN: -2.0, Strength: 1.5},
			{InstrumentID: "MSFT", Tier: event.TierB, DistanceToEntryInN: 0.5, Strength: 0.75},
		},
	}
}

func TestWatchlistPublishedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.WatchlistPublishedPayload)
		wantErr string
	}{
		{name: "valid, two entries"},
		{
			name: "valid, single tier b entry (a setup can enter tier a directly, never having been tier b, so a tier b-only watchlist is equally valid)",
			mutate: func(p *event.WatchlistPublishedPayload) {
				p.Entries = []event.WatchlistEntry{
					{InstrumentID: "MSFT", Tier: event.TierB, DistanceToEntryInN: 0.5, Strength: 0.75},
				}
			},
		},
		{
			name:    "zero period end",
			mutate:  func(p *event.WatchlistPublishedPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.WatchlistPublishedPayload) { p.Rule = "" },
			wantErr: "rule is required",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.WatchlistPublishedPayload) { p.ADR = "" },
			wantErr: "adr is required",
		},
		{
			name:   "valid, nil entries record a Session with no ranked Setups",
			mutate: func(p *event.WatchlistPublishedPayload) { p.Entries = nil },
		},
		{
			name:   "valid, empty entries record a Session with no ranked Setups",
			mutate: func(p *event.WatchlistPublishedPayload) { p.Entries = []event.WatchlistEntry{} },
		},
		{
			name: "missing instrument id",
			mutate: func(p *event.WatchlistPublishedPayload) {
				p.Entries[0].InstrumentID = ""
			},
			wantErr: "instrument id is required",
		},
		{
			name: "duplicate instrument id",
			mutate: func(p *event.WatchlistPublishedPayload) {
				p.Entries[1].InstrumentID = "AAPL"
			},
			wantErr: "appears more than once",
		},
		{
			name: "tier none has no place on the watchlist",
			mutate: func(p *event.WatchlistPublishedPayload) {
				p.Entries[0].Tier = event.TierNone
			},
			wantErr: "not tier a or tier b",
		},
		{
			name: "unrecognised tier value",
			mutate: func(p *event.WatchlistPublishedPayload) {
				p.Entries[0].Tier = "C"
			},
			wantErr: "not tier a or tier b",
		},
		{
			name: "tier a requires a negative distance",
			mutate: func(p *event.WatchlistPublishedPayload) {
				p.Entries[0].DistanceToEntryInN = 0
			},
			wantErr: "tier a requires a negative distance to entry in n",
		},
		{
			name: "tier b requires a non-negative distance",
			mutate: func(p *event.WatchlistPublishedPayload) {
				p.Entries[1].DistanceToEntryInN = -0.1
			},
			wantErr: "tier b requires a non-negative distance to entry in n",
		},
		{
			name: "tier b with a zero distance (a tie) is valid",
			mutate: func(p *event.WatchlistPublishedPayload) {
				p.Entries[1].DistanceToEntryInN = 0
			},
		},
		{
			name: "entries must be sorted by strength, descending",
			mutate: func(p *event.WatchlistPublishedPayload) {
				p.Entries[0].Strength, p.Entries[1].Strength = p.Entries[1].Strength, p.Entries[0].Strength
			},
			wantErr: "must be sorted by strength, descending",
		},
		{
			name: "tied strength is valid (the remaining tie-breaks are not carried on the wire)",
			mutate: func(p *event.WatchlistPublishedPayload) {
				p.Entries[1].Strength = p.Entries[0].Strength
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validWatchlistPublished()
			if tt.mutate != nil {
				tt.mutate(&payload)
			}
			err := payload.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// A NaN or infinite value must be rejected explicitly, the same way every
// other payload in this package rejects it (see
// TestSetupEvaluatedPayloadValidateRejectsNonFiniteFields).
func TestWatchlistPublishedPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name    string
		apply   func(p *event.WatchlistPublishedPayload, f float64)
		wantErr string
	}
	fields := []fieldCase{
		{
			name:    "distance to entry in n",
			apply:   func(p *event.WatchlistPublishedPayload, f float64) { p.Entries[0].DistanceToEntryInN = f },
			wantErr: "distance to entry in n must be finite",
		},
		{
			name:    "strength",
			apply:   func(p *event.WatchlistPublishedPayload, f float64) { p.Entries[0].Strength = f },
			wantErr: "strength must be finite",
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
			t.Run(field.name+"/"+nf.name, func(t *testing.T) {
				t.Parallel()
				payload := validWatchlistPublished()
				field.apply(&payload, nf.value)
				err := payload.Validate()
				if err == nil || !strings.Contains(err.Error(), field.wantErr) {
					t.Fatalf("Validate() error = %v, want substring %q", err, field.wantErr)
				}
			})
		}
	}
}

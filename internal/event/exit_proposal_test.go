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

// exitProposalPeriodEnd is the completed bar this fixture's exit proposal was
// raised on.
var exitProposalPeriodEnd = time.Date(2026, time.March, 20, 0, 0, 0, 0, time.UTC)

// validExitProposal returns an exit proposal for the whole 133-share Campaign
// validCampaignOpened describes, at a channel low of 180 — below
// campaignEntryPrice (201.25, fill_test.go), as a breach naturally is.
func validExitProposal() event.ExitProposalPayload {
	return event.ExitProposalPayload{
		CampaignID:   "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID: "AAPL",
		PeriodEnd:    exitProposalPeriodEnd,
		Reason:       event.ExitReasonExitChannel,
		Level:        180,
		Quantity:     133,
		Rule:         event.RuleExitChannelBreach,
		ADR:          event.ADRExitChannelBreach,
	}
}

func TestExitProposalPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.ExitProposalPayload)
		wantErr string
	}{
		{name: "valid exit proposal"},
		{
			name:    "missing campaign id",
			mutate:  func(p *event.ExitProposalPayload) { p.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.ExitProposalPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing period end",
			mutate:  func(p *event.ExitProposalPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			name:    "unrecognised reason",
			mutate:  func(p *event.ExitProposalPayload) { p.Reason = "margin-call" },
			wantErr: "reason",
		},
		{
			name:    "zero level",
			mutate:  func(p *event.ExitProposalPayload) { p.Level = 0 },
			wantErr: "level must be positive",
		},
		{
			name:    "negative level",
			mutate:  func(p *event.ExitProposalPayload) { p.Level = -1 },
			wantErr: "level must be positive",
		},
		{
			name:    "non-finite level",
			mutate:  func(p *event.ExitProposalPayload) { p.Level = math.NaN() },
			wantErr: "level must be finite",
		},
		{
			name:    "zero quantity",
			mutate:  func(p *event.ExitProposalPayload) { p.Quantity = 0 },
			wantErr: "quantity",
		},
		{
			name:    "negative quantity",
			mutate:  func(p *event.ExitProposalPayload) { p.Quantity = -1 },
			wantErr: "quantity",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.ExitProposalPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.ExitProposalPayload) { p.ADR = "" },
			wantErr: "adr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validExitProposal()
			if tt.mutate != nil {
				tt.mutate(&payload)
			}

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

// TestExitProposalPayloadValidateAggregatesEveryField mirrors the same
// pattern every other payload's zero-value test uses in this package.
func TestExitProposalPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.ExitProposalPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"campaign id",
		"instrument id",
		"period end",
		"reason",
		"level",
		"quantity",
		"rule",
		"adr",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestExitProposalEventConstants(t *testing.T) {
	t.Parallel()

	// "strategy.*", not "execution.*": like a trade proposal, this is a
	// decision the strategy made, not an external fact — nothing here assumes
	// a fill (ADR 0005 and #18 own the fill model).
	if event.ExitProposalEventType != "strategy.exit.proposed" {
		t.Errorf("ExitProposalEventType = %q, want %q", event.ExitProposalEventType, "strategy.exit.proposed")
	}
	if event.ExitProposalSchemaVersion != 1 {
		t.Errorf("ExitProposalSchemaVersion = %d, want 1", event.ExitProposalSchemaVersion)
	}
	if event.RuleExitChannelBreach != "exit.channel.breach" {
		t.Errorf("RuleExitChannelBreach = %q, want %q", event.RuleExitChannelBreach, "exit.channel.breach")
	}
	// ADR 0002 defines the Exit Channel breach rule itself (The Turtle Rules
	// p.26): a 20-bar low, all Units.
	if event.ADRExitChannelBreach != "0002" {
		t.Errorf("ADRExitChannelBreach = %q, want %q", event.ADRExitChannelBreach, "0002")
	}
	if event.ExitReasonExitChannel != "exit-channel" {
		t.Errorf("ExitReasonExitChannel = %q, want %q", event.ExitReasonExitChannel, "exit-channel")
	}
}

func TestExitProposalPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validExitProposal()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.ExitProposalPayload
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

func TestExitProposalPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validExitProposal())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{
		"campaign_id",
		"instrument_id",
		"period_end",
		"reason",
		"level",
		"quantity",
		"rule",
		"adr",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}

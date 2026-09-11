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

var campaignEvaluatedPeriodEnd = time.Date(2026, time.March, 20, 0, 0, 0, 0, time.UTC)

// validCampaignEvaluated returns a ready-channel, no-breach reading for the
// Campaign validCampaignOpened describes: the Exit Channel low (180) sits
// below the Campaign's Protective Stop, and both sit below the bar that
// produced this reading, so nothing here is a breach.
func validCampaignEvaluated() event.CampaignEvaluatedPayload {
	return event.CampaignEvaluatedPayload{
		CampaignID:       "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:     "AAPL",
		PeriodEnd:        campaignEvaluatedPeriodEnd,
		ProtectiveStop:   126.09441570423544,
		ExitChannelLow:   180,
		ExitChannelReady: true,
		ExitConditionMet: false,
	}
}

func TestCampaignEvaluatedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CampaignEvaluatedPayload)
		wantErr string
	}{
		{name: "valid campaign evaluated"},
		{
			name:    "missing campaign id",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing period end",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			name:    "zero protective stop",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ProtectiveStop = 0 },
			wantErr: "protective stop must be positive",
		},
		{
			name:    "negative protective stop",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ProtectiveStop = -1 },
			wantErr: "protective stop must be positive",
		},
		{
			name:    "non-finite protective stop",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ProtectiveStop = math.NaN() },
			wantErr: "protective stop must be finite",
		},
		{
			name:    "negative exit channel low",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ExitChannelLow = -1 },
			wantErr: "exit channel low must not be negative",
		},
		{
			name:    "non-finite exit channel low",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ExitChannelLow = math.NaN() },
			wantErr: "exit channel low must be finite",
		},
		{
			// Matches SetupEvaluatedPayload.EntryChannelHigh's convention: a
			// ready channel must report a real (positive) level.
			name:    "exit channel low zero while ready",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ExitChannelLow = 0 },
			wantErr: "exit channel low must be positive when ready",
		},
		{
			// The mirror case: not ready must report exactly zero, the same
			// convention N and EntryChannelHigh already use, so a consumer
			// reading a level without checking readiness first sees an
			// unambiguous "not evaluable" value rather than a stale one.
			name: "exit channel low nonzero while not ready",
			mutate: func(p *event.CampaignEvaluatedPayload) {
				p.ExitChannelReady = false
				p.ExitConditionMet = false
			},
			wantErr: "exit channel low must be zero while not ready",
		},
		{
			// The Exit Channel must be ready for a breach to be possible at
			// all: an unready channel has no real level to have fallen below.
			name: "exit condition met while not ready",
			mutate: func(p *event.CampaignEvaluatedPayload) {
				p.ExitChannelReady = false
				p.ExitChannelLow = 0
				p.ExitConditionMet = true
			},
			wantErr: "exit condition met must be false while the exit channel is not ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validCampaignEvaluated()
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

// TestCampaignEvaluatedPayloadValidateAcceptsNotReadyWithNoBreach covers the
// legitimate "not ready" reading directly: possible only when a Campaign
// opened very early (fewer than 20 preceding completed bars exist yet), it
// must validate cleanly with ExitChannelLow at its zero convention and
// ExitConditionMet false.
func TestCampaignEvaluatedPayloadValidateAcceptsNotReadyWithNoBreach(t *testing.T) {
	t.Parallel()

	payload := validCampaignEvaluated()
	payload.ExitChannelReady = false
	payload.ExitChannelLow = 0
	payload.ExitConditionMet = false

	if err := payload.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for a legitimate not-ready reading", err)
	}
}

// TestCampaignEvaluatedPayloadValidateAcceptsABreach covers the other
// legitimate combination: ready and breached.
func TestCampaignEvaluatedPayloadValidateAcceptsABreach(t *testing.T) {
	t.Parallel()

	payload := validCampaignEvaluated()
	payload.ExitConditionMet = true

	if err := payload.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for a legitimate breach reading", err)
	}
}

func TestCampaignEvaluatedPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.CampaignEvaluatedPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"campaign id",
		"instrument id",
		"period end",
		"protective stop",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestCampaignEvaluatedEventConstants(t *testing.T) {
	t.Parallel()

	if event.CampaignEvaluatedEventType != "strategy.campaign.evaluated" {
		t.Errorf("CampaignEvaluatedEventType = %q, want %q", event.CampaignEvaluatedEventType, "strategy.campaign.evaluated")
	}
	if event.CampaignEvaluatedSchemaVersion != 1 {
		t.Errorf("CampaignEvaluatedSchemaVersion = %d, want 1", event.CampaignEvaluatedSchemaVersion)
	}
}

func TestCampaignEvaluatedPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validCampaignEvaluated()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.CampaignEvaluatedPayload
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

func TestCampaignEvaluatedPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validCampaignEvaluated())
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
		"protective_stop",
		"exit_channel_low",
		"exit_channel_ready",
		"exit_condition_met",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}

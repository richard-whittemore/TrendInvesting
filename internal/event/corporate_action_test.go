package event_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the tests for the corporate-action event a Delisting Exit
// is built from (CONTEXT.md: "Delisting Exit"; ADR 0009).

func validCorporateAction() event.CorporateActionPayload {
	return event.CorporateActionPayload{
		InstrumentID: "AAPL",
		Kind:         event.CorporateActionKindDelisting,
		EffectiveAt:  time.Date(2026, time.February, 27, 0, 0, 0, 0, time.UTC),
	}
}

func TestCorporateActionPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CorporateActionPayload)
		wantErr string
	}{
		{name: "valid delisting"},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.CorporateActionPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing kind",
			mutate:  func(p *event.CorporateActionPayload) { p.Kind = "" },
			wantErr: "not a recognised corporate action kind",
		},
		{
			// Kind is a closed set: only a delisting is recognised today, so
			// a future corporate action (a merger, a spinoff) must be a new
			// value added deliberately, never a string a producer can send
			// today and have silently misread as a delisting.
			name:    "unrecognised kind",
			mutate:  func(p *event.CorporateActionPayload) { p.Kind = "merger" },
			wantErr: "not a recognised corporate action kind",
		},
		{
			name:    "missing effective at",
			mutate:  func(p *event.CorporateActionPayload) { p.EffectiveAt = time.Time{} },
			wantErr: "effective at",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validCorporateAction()
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

// TestCorporateActionPayloadValidateAggregatesEveryField mirrors this
// package's own convention (e.g. TestCampaignOpenedPayloadValidateAggregatesEveryField):
// every field's own error is reported at once, not just the first.
func TestCorporateActionPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	err := event.CorporateActionPayload{}.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want an error for a wholly empty payload")
	}
	for _, want := range []string{"instrument id", "not a recognised corporate action kind", "effective at"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want it to mention %q", err, want)
		}
	}
}

func TestCorporateActionPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validCorporateAction()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.CorporateActionPayload
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

// TestCorporateActionPayloadCarriesNoPrice pins the deliberate absence
// documented on the payload's own doc comment: JSON-encoding a delisting
// names only instrument_id, kind and effective_at, so a producer that tried
// to add a price would be adding a field this contract does not declare. The
// split terms are omitted when zero, so a schema-2 delisting encodes to the
// same bytes a schema-1 one did.
func TestCorporateActionPayloadCarriesNoPrice(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validCorporateAction())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	wantKeys := map[string]bool{"instrument_id": true, "kind": true, "effective_at": true}
	if len(fields) != len(wantKeys) {
		t.Fatalf("got %d field(s) %v, want exactly %v", len(fields), fields, wantKeys)
	}
	for key := range wantKeys {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing expected field %q", key)
		}
	}
}

// TestUpcastCorporateActionPayloadRefusesAnUndecodableCurrentSchemaRecord
// covers the current-schema decode branch (schema 3, ADR 0024): a record
// stamped with today's own schema version but whose bytes are not valid
// JSON for the payload, or that carries an unknown field, is refused rather
// than silently misread.
func TestUpcastCorporateActionPayloadRefusesAnUndecodableCurrentSchemaRecord(t *testing.T) {
	t.Parallel()

	_, err := event.UpcastCorporateActionPayload(event.MarketCorporateActionSchemaVersion, []byte(`{"instrument_id":"AAPL","kind":"dividend","effective_at":"2026-03-02T04:00:00Z","cash_amount":184,"currency":"USD","price":1}`))
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("UpcastCorporateActionPayload(current schema, unknown field) error = %v, want a decode failure", err)
	}
}

func TestCorporateActionEventConstants(t *testing.T) {
	t.Parallel()

	if event.MarketCorporateActionEventType != "market.corporate-action" {
		t.Errorf("MarketCorporateActionEventType = %q, want %q", event.MarketCorporateActionEventType, "market.corporate-action")
	}
	if event.MarketCorporateActionSchemaVersion != 3 {
		t.Errorf("MarketCorporateActionSchemaVersion = %d, want 3", event.MarketCorporateActionSchemaVersion)
	}
	if event.CorporateActionKindDelisting != "delisting" {
		t.Errorf("CorporateActionKindDelisting = %q, want %q", event.CorporateActionKindDelisting, "delisting")
	}
	if event.CorporateActionKindSplit != "split" {
		t.Errorf("CorporateActionKindSplit = %q, want %q", event.CorporateActionKindSplit, "split")
	}
	if event.CorporateActionKindSymbolChange != "symbol-change" {
		t.Errorf("CorporateActionKindSymbolChange = %q, want %q", event.CorporateActionKindSymbolChange, "symbol-change")
	}
	if event.CorporateActionKindDividend != "dividend" {
		t.Errorf("CorporateActionKindDividend = %q, want %q", event.CorporateActionKindDividend, "dividend")
	}
}

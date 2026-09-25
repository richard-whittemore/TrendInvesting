package registry

import (
	"encoding/json"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// These are synthetic arithmetic examples for ADR 0012, not source goldens.
func TestSectorShareHandComputed(t *testing.T) {
	for _, tc := range []struct {
		units map[string]int
		want  float64
	}{
		{nil, 0},
		{map[string]int{"tech": 2, "energy": 1}, 2.0 / 3},
		{map[string]int{"tech": 2, "energy": 2, "": 1}, 2.0 / 5},
		{map[string]int{"": 3}, 1},
		{map[string]int{"tech": 0}, 0},
	} {
		if got := largestSectorShare(tc.units); got != tc.want {
			t.Fatalf("share %v want %v", got, tc.want)
		}
	}
}

func exposureEntry(t *testing.T, typ string, schema uint32, payload any) journal.Entry {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return journal.Entry{Kind: journal.KindDecision, Envelope: event.Envelope{Type: typ, SchemaVersion: schema, Payload: raw}}
}

func TestCampaignExposureHandComputed(t *testing.T) {
	// Adds are Units of the same Campaign; re-entry in the same instrument is
	// a new Campaign. Unfilled proposals and input fills are not decisions.
	entries := []journal.Entry{
		exposureEntry(t, event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, map[string]any{"campaign_id": "a", "instrument_id": "AAA", "units": 1}),
		exposureEntry(t, event.CampaignUnitAddedEventType, event.CampaignUnitAddedSchemaVersion, map[string]any{"campaign_id": "a", "units": 2}),
		exposureEntry(t, event.CampaignUnitsStoppedEventType, event.CampaignUnitsStoppedSchemaVersion, map[string]any{"campaign_id": "a", "remaining_units": 1}),
		exposureEntry(t, event.CampaignExitedEventType, event.CampaignExitedSchemaVersion, map[string]any{"campaign_id": "a"}),
		exposureEntry(t, event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, map[string]any{"campaign_id": "b", "instrument_id": "AAA", "units": 1}),
	}
	got, err := CampaignExposure(entries)
	if err != nil {
		t.Fatal(err)
	}
	if got.IndependentCampaigns != 2 || got.PeakSectorConcentration != 1 {
		t.Fatalf("metrics %+v", got)
	}
	empty, err := CampaignExposure(nil)
	if err != nil || empty.IndependentCampaigns != 0 || empty.PeakSectorConcentration != 0 {
		t.Fatalf("empty %+v %v", empty, err)
	}
	ignored := entries[0]
	ignored.Kind = journal.KindInput
	got, err = CampaignExposure([]journal.Entry{ignored, exposureEntry(t, event.TradeProposalEventType, 1, nil)})
	if err != nil || got.IndependentCampaigns != 0 {
		t.Fatalf("non-decisions %+v %v", got, err)
	}
}

func TestReportSchemaUpcast(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		ok      bool
		missing bool
	}{
		{`{"schema_version":1}`, true, true},
		{`{"schema_version":2,"exposure":{"peak_sector_concentration":0.75,"independent_campaigns":4}}`, true, false},
		{`{"schema_version":2}`, false, false},
		{`{"schema_version":0}`, false, false},
		{`{"schema_version":3}`, false, false},
		{`{"schema_version":1,"surprise":true}`, false, false},
		{`{"schema_version":1,"exposure":{"peak_sector_concentration":1,"independent_campaigns":1}}`, false, false},
		{`{"schema_version":2,"exposure":{"peak_sector_concentration":2,"independent_campaigns":1}}`, false, false},
		{`{"schema_version":2,"exposure":"bad"}`, false, false},
		{`{"schema_version":2,"exposure":{"surprise":1}}`, false, false},
		{`{"schema_version":2,"exposure":null}`, true, true},
		{`{`, false, false},
	} {
		var r Report
		err := json.Unmarshal([]byte(tc.raw), &r)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: %v", tc.raw, err)
		}
		if tc.ok && (r.SchemaVersion != 2 || (r.Exposure == nil) != tc.missing) {
			t.Fatalf("upcast %+v", r)
		}
	}
}

func TestCampaignExposureRejectsInconsistentEvidence(t *testing.T) {
	opening := exposureEntry(t, event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, map[string]any{"campaign_id": "a", "units": 1})
	for _, tc := range []struct {
		typ    string
		schema uint32
		raw    string
	}{
		{event.CampaignOpenedEventType, 999, `{}`},
		{event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, `[]`},
		{event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, `{}`},
		{event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, `{"campaign_id":"a","units":1}`},
		{event.CampaignUnitAddedEventType, event.CampaignUnitAddedSchemaVersion, `{"campaign_id":"a","units":3}`},
		{event.CampaignUnitsStoppedEventType, event.CampaignUnitsStoppedSchemaVersion, `{"campaign_id":"a","remaining_units":2}`},
		{event.CampaignExitedEventType, event.CampaignExitedSchemaVersion, `{"campaign_id":"missing"}`},
	} {
		e := journal.Entry{Kind: journal.KindDecision, Envelope: event.Envelope{Type: tc.typ, SchemaVersion: tc.schema, Payload: json.RawMessage(tc.raw)}}
		if _, err := CampaignExposure([]journal.Entry{opening, e}); err == nil {
			t.Fatalf("accepted %s %s", tc.typ, tc.raw)
		}
	}
}

func TestLegacyReportRoundTripRetainsUnknownExposure(t *testing.T) {
	var r Report
	if err := json.Unmarshal([]byte(`{"schema_version":1}`), &r); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var next Report
	if err := json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	if next.Exposure != nil {
		t.Fatal("legacy unknown exposure became zero")
	}
}

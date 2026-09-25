package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// TestSessionExposureHandComputed checks ADR 0012's equally weighted Session
// samples using two ADR 0008 groups. The (tech, energy) counts are (0,0),
// (1,0), (2,2), (2,2), (1,3), (0,0): peak sector Units = 3; peak total = 4;
// nonempty shares = 1, 1/2, 1/2, 3/4, so their mean is 11/16. Repeating the
// unchanged book counts another Session; empty Sessions do not dilute the mean.
// These are synthetic arithmetic examples, not source goldens.
func TestSessionExposureHandComputed(t *testing.T) {
	var got exposureAccumulator
	for _, counts := range [][2]int{{0, 0}, {1, 0}, {2, 2}, {2, 2}, {1, 3}, {0, 0}} {
		got.observeSession(map[string]int{"tech": counts[0], "energy": counts[1]})
	}
	if got.PeakSectorUnits != 3 {
		t.Fatalf("peak sector = %d, want 3", got.PeakSectorUnits)
	}
	if got.TimeWeightedLargestSectorShare != 11.0/16 {
		t.Fatalf("mean share = %v, want 11/16", got.TimeWeightedLargestSectorShare)
	}
	if got.PeakConcurrentOpenUnits != 4 {
		t.Fatalf("peak total = %d, want 4", got.PeakConcurrentOpenUnits)
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

// TestCampaignExposureHandComputed follows ADR 0012's Campaign lifecycle:
// Session 1 closes with 2 Units; Session 2 stops, exits and re-enters with 1.
// Both are wholly Unclassified (ADR 0008), so peak sector = peak total = 2,
// mean share = (1+1)/2 = 1, and distinct Campaign openings = 2.
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
	for i := range entries {
		day := 2
		if i >= 2 {
			day = 3
		}
		entries[i].Envelope.EventTime = exposureSession(day).Envelope.EventTime
	}
	entries = append(entries[:2], append([]journal.Entry{exposureSession(2)}, entries[2:]...)...)
	entries = append(entries, exposureSession(3))
	got, err := CampaignExposure(entries)
	if err != nil {
		t.Fatal(err)
	}
	if got.IndependentCampaigns != 2 || got.TimeWeightedLargestSectorShare != 1 || got.PeakSectorUnits != 2 || got.PeakConcurrentOpenUnits != 2 {
		t.Fatalf("metrics %+v", got)
	}
	empty, err := CampaignExposure(nil)
	if err != nil || *empty != (ExposureMetrics{}) {
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
		{`{"schema_version":2,"exposure":{"peak_sector_units":3,"time_weighted_largest_sector_share":0.75,"peak_concurrent_open_units":4,"independent_campaigns":4}}`, true, false},
		{`{"schema_version":2}`, false, false},
		{`{"schema_version":0}`, false, false},
		{`{"schema_version":3}`, false, false},
		{`{"schema_version":1,"surprise":true}`, false, false},
		{`{"schema_version":1,"exposure":{"peak_sector_concentration":1,"independent_campaigns":1}}`, false, false},
		{`{"schema_version":2,"exposure":{"time_weighted_largest_sector_share":2,"independent_campaigns":1}}`, false, false},
		{`{"schema_version":2,"exposure":{"peak_sector_concentration":1}}`, false, false},
		{`{"schema_version":2,"exposure":{"peak_sector_units":-1}}`, false, false},
		{`{"schema_version":2,"exposure":{"peak_concurrent_open_units":-1}}`, false, false},
		{`{"schema_version":2,"exposure":{"peak_sector_units":3,"peak_concurrent_open_units":2}}`, false, false},
		{`{"schema_version":2,"exposure":{"time_weighted_largest_sector_share":-0.1}}`, false, false},
		{`{"schema_version":2,"exposure":{"independent_campaigns":-1}}`, false, false},
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
		if tc.ok && !tc.missing {
			want := ExposureMetrics{PeakSectorUnits: 3, TimeWeightedLargestSectorShare: 0.75, PeakConcurrentOpenUnits: 4, IndependentCampaigns: 4}
			if r.Exposure == nil || *r.Exposure != want {
				t.Fatalf("decoded exposure %+v, want %+v", r.Exposure, want)
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var next Report
			if err := json.Unmarshal(raw, &next); err != nil || next.Exposure == nil || *next.Exposure != want {
				t.Fatalf("round trip %+v, %v", next.Exposure, err)
			}
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

func exposureSession(day int) journal.Entry {
	return journal.Entry{Kind: journal.KindInput, Envelope: event.Envelope{
		Type: event.SessionClosedEventType, SchemaVersion: event.SessionClosedSchemaVersion,
		EventTime: time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC),
	}}
}

// TestCampaignExposureSessionBoundary checks ADR 0012 sampling after all
// same-Session decisions, including fills after ADR 0021's close marker.
// Session 1 opens then Adds twice and stops once: final Units = 2, not 3.
// Session 2 is unchanged at 2; Session 3 exits to 0; an incomplete Session 4
// opens again but has no sample. Peaks are 2, mean share is (1+1)/2 = 1,
// and the two distinct openings still count as two independent Campaigns.
func TestCampaignExposureSessionBoundary(t *testing.T) {
	entries := []journal.Entry{exposureSession(1)}
	for _, p := range []struct {
		typ     string
		schema  uint32
		payload map[string]any
	}{
		{event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, map[string]any{"campaign_id": "a", "units": 1}},
		{event.CampaignUnitAddedEventType, event.CampaignUnitAddedSchemaVersion, map[string]any{"campaign_id": "a", "units": 2}},
		{event.CampaignUnitAddedEventType, event.CampaignUnitAddedSchemaVersion, map[string]any{"campaign_id": "a", "units": 3}},
		{event.CampaignUnitsStoppedEventType, event.CampaignUnitsStoppedSchemaVersion, map[string]any{"campaign_id": "a", "remaining_units": 2}},
	} {
		e := exposureEntry(t, p.typ, p.schema, p.payload)
		e.Envelope.EventTime = exposureSession(1).Envelope.EventTime
		entries = append(entries, e)
	}
	entries = append(entries, exposureSession(2), exposureSession(3))
	exit := exposureEntry(t, event.CampaignExitedEventType, event.CampaignExitedSchemaVersion, map[string]any{"campaign_id": "a"})
	exit.Envelope.EventTime = exposureSession(3).Envelope.EventTime
	entries = append(entries, exit)
	opening := exposureEntry(t, event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, map[string]any{"campaign_id": "b", "units": 1})
	opening.Envelope.EventTime = exposureSession(4).Envelope.EventTime
	entries = append(entries, opening)
	got, err := CampaignExposure(entries)
	want := ExposureMetrics{PeakSectorUnits: 2, PeakConcurrentOpenUnits: 2, TimeWeightedLargestSectorShare: 1, IndependentCampaigns: 2}
	if err != nil || *got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

func TestCampaignExposureRejectsInvalidSession(t *testing.T) {
	for _, e := range []journal.Entry{
		{Kind: journal.KindInput, Envelope: event.Envelope{Type: event.SessionClosedEventType, SchemaVersion: 999}},
		{Kind: journal.KindInput, Envelope: event.Envelope{Type: event.SessionClosedEventType, SchemaVersion: event.SessionClosedSchemaVersion}},
	} {
		if _, err := CampaignExposure([]journal.Entry{e}); err == nil {
			t.Fatal("accepted invalid Session")
		}
	}
}

package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// ExposureMetrics reports ADR 0012's concentration and Campaign count.
// Concentration is a fraction of open Units, including the shared Unclassified
// Group (ADR 0008). An empty book contributes zero. Campaigns count once at
// opening, including those still open at the end; Adds never count as Campaigns.
type ExposureMetrics struct {
	PeakSectorConcentration float64 `json:"peak_sector_concentration"`
	IndependentCampaigns    int     `json:"independent_campaigns"`
}

// CampaignExposure observes committed decisions in journal order, including
// partial stops, never proposals or duplicate input fills (ADR 0008, ADR 0012).
// The current classification seam assigns every Campaign to the shared
// Unclassified Group (strategy/unit_caps.go). When point-in-time labels arrive,
// their frozen opening classification must also be carried into this report.
func CampaignExposure(entries []journal.Entry) (*ExposureMetrics, error) {
	m := &ExposureMetrics{}
	units := map[string]int{}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Kind != journal.KindDecision {
			continue
		}
		var schema uint32
		switch e.Envelope.Type {
		case event.CampaignOpenedEventType:
			schema = event.CampaignOpenedSchemaVersion
		case event.CampaignUnitAddedEventType:
			schema = event.CampaignUnitAddedSchemaVersion
		case event.CampaignUnitsStoppedEventType:
			schema = event.CampaignUnitsStoppedSchemaVersion
		case event.CampaignExitedEventType:
			schema = event.CampaignExitedSchemaVersion
		default:
			continue
		}
		if e.Envelope.SchemaVersion != schema {
			return nil, errors.New("report: unsupported Campaign schema")
		}
		// Read only the exposure fields from already validated reducer decisions.
		var p struct {
			CampaignID     string `json:"campaign_id"`
			Units          int    `json:"units"`
			RemainingUnits int    `json:"remaining_units"`
		}
		if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
			return nil, err
		}
		if p.CampaignID == "" {
			return nil, errors.New("report: missing Campaign identity")
		}
		old, open := units[p.CampaignID]
		switch e.Envelope.Type {
		case event.CampaignOpenedEventType:
			if seen[p.CampaignID] || p.Units != 1 {
				return nil, errors.New("report: duplicate or invalid Campaign opening")
			}
			seen[p.CampaignID] = true
			m.IndependentCampaigns++
			units[p.CampaignID] = p.Units
		case event.CampaignUnitAddedEventType:
			if !open || p.Units != old+1 {
				return nil, errors.New("report: inconsistent Campaign Add")
			}
			units[p.CampaignID] = p.Units
		case event.CampaignUnitsStoppedEventType:
			if !open || p.RemainingUnits < 0 || p.RemainingUnits >= old {
				return nil, errors.New("report: inconsistent Campaign stop")
			}
			units[p.CampaignID] = p.RemainingUnits
		case event.CampaignExitedEventType:
			if !open {
				return nil, errors.New("report: exit without an open Campaign")
			}
			delete(units, p.CampaignID)
		}
		// All current Campaigns have the same ADR 0008 correlation group.
		total := 0
		for _, n := range units {
			total += n
		}
		m.PeakSectorConcentration = max(m.PeakSectorConcentration, largestSectorShare(map[string]int{"": total}))
	}
	return m, nil
}

func largestSectorShare(units map[string]int) float64 {
	total, largest := 0, 0
	for _, n := range units {
		total += n
		largest = max(largest, n)
	}
	if total == 0 {
		return 0
	}
	return float64(largest) / float64(total)
}

// UnmarshalJSON explicitly upcasts schema 1's absent exposure to unknown (nil),
// never zero, without rewriting historical evidence (ADR 0015, ADR 0018).
// Schema 2's null exposure preserves that unknown value when re-encoded.
func (r *Report) UnmarshalJSON(raw []byte) error {
	type wire Report
	var decoded wire
	shape := struct {
		*wire
		Exposure json.RawMessage `json:"exposure"`
	}{wire: &decoded}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&shape); err != nil {
		return err
	}
	switch decoded.SchemaVersion {
	case 1:
		if shape.Exposure != nil {
			return errors.New("report: schema 1 cannot contain exposure")
		}
		decoded.SchemaVersion = ReportSchemaVersion
	case ReportSchemaVersion:
		if shape.Exposure == nil {
			return errors.New("report: schema 2 requires exposure")
		}
		d := json.NewDecoder(bytes.NewReader(shape.Exposure))
		d.DisallowUnknownFields()
		if err := d.Decode(&decoded.Exposure); err != nil {
			return err
		}
	default:
		return fmt.Errorf("report: unsupported schema version %d (ADR 0015)", decoded.SchemaVersion)
	}
	if m := decoded.Exposure; m != nil {
		if !finite(m.PeakSectorConcentration) || m.PeakSectorConcentration < 0 || m.PeakSectorConcentration > 1 || m.IndependentCampaigns < 0 {
			return errors.New("report: invalid exposure metrics")
		}
	}
	*r = Report(decoded)
	return nil
}

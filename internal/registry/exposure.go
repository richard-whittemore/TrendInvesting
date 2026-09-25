package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// ExposureMetrics reports Session-end exposure and Campaign counts (ADR 0012).
// PeakSectorUnits is the maximum open Unit count in any ADR 0008 group;
// PeakConcurrentOpenUnits is the maximum total open Units across those samples.
// TimeWeightedLargestSectorShare is the mean largest-group / total open Units
// over nonempty Sessions, weighting each Session equally; no such Sessions
// yields zero. Unclassified instruments are one group under ADR 0008: a run
// without classifications reports everything in that single group, as expected.
// Campaigns count once at opening, including those still open at the end;
// Adds never count as Campaigns.
type ExposureMetrics struct {
	PeakSectorUnits                int     `json:"peak_sector_units"`
	TimeWeightedLargestSectorShare float64 `json:"time_weighted_largest_sector_share"`
	PeakConcurrentOpenUnits        int     `json:"peak_concurrent_open_units"`
	IndependentCampaigns           int     `json:"independent_campaigns"`
}

type exposureAccumulator struct {
	ExposureMetrics
	nonemptySessions int
	shareSum         float64
}

// observeSession samples one final Session book grouped by ADR 0008 labels
// (ADR 0012). Each call represents one Session, including unchanged books;
// callers supply nonnegative Unit counts, with Unclassified in one group.
func (m *exposureAccumulator) observeSession(groups map[string]int) {
	total, largest := 0, 0
	for _, n := range groups {
		total += n
		largest = max(largest, n)
	}
	m.PeakSectorUnits = max(m.PeakSectorUnits, largest)
	m.PeakConcurrentOpenUnits = max(m.PeakConcurrentOpenUnits, total)
	if total == 0 {
		return
	}
	m.nonemptySessions++
	m.shareSum += float64(largest) / float64(total)
	m.TimeWeightedLargestSectorShare = m.shareSum / float64(m.nonemptySessions)
}

// CampaignExposure observes committed decisions in journal order, including
// partial stops, never proposals or duplicate input fills (ADR 0008, ADR 0012).
// It samples each closed Session after all its same-time decisions, including
// fills after the ADR 0021 close marker; an incomplete Session has no sample.
// Entries must be validated reducer evidence in journal order, with decisions
// stamped at their Session's period end.
// The current classification seam assigns every Campaign to the shared
// Unclassified Group (strategy/unit_caps.go). When point-in-time labels arrive,
// their frozen opening classification must also be carried into this report.
func CampaignExposure(entries []journal.Entry) (*ExposureMetrics, error) {
	m := &exposureAccumulator{}
	units := map[string]int{}
	seen := map[string]bool{}
	var session time.Time
	sample := func() {
		total := 0
		for _, n := range units {
			total += n
		}
		m.observeSession(map[string]int{"": total})
	}
	for _, e := range entries {
		if !session.IsZero() && e.Envelope.EventTime.After(session) {
			sample()
			session = time.Time{}
		}
		if e.Kind == journal.KindInput && e.Envelope.Type == event.SessionClosedEventType {
			if e.Envelope.SchemaVersion != event.SessionClosedSchemaVersion || e.Envelope.EventTime.IsZero() {
				return nil, errors.New("report: invalid Session close")
			}
			session = e.Envelope.EventTime
			continue
		}
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
	}
	if !session.IsZero() {
		sample()
	}
	return &m.ExposureMetrics, nil
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
		if !finite(m.TimeWeightedLargestSectorShare) || m.TimeWeightedLargestSectorShare < 0 || m.TimeWeightedLargestSectorShare > 1 || m.PeakSectorUnits < 0 || m.PeakConcurrentOpenUnits < m.PeakSectorUnits || m.IndependentCampaigns < 0 {
			return errors.New("report: invalid exposure metrics")
		}
	}
	*r = Report(decoded)
	return nil
}

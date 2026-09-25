package registry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

//go:embed protocol.json
var protocolJSON []byte

// Protocol fixes the research windows independently of trading configurations
// (ADR 0012, Proposed amendment 2026-09-25). There is no per-run override.
type Protocol struct {
	Version     uint32    `json:"version"`
	Split       time.Time `json:"split"`
	DaysPerYear float64   `json:"days_per_year"`
	Windows     []Window  `json:"windows"`
}

type Window struct {
	Name  string    `json:"name"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Contains uses inclusive starts and exclusive ends; shared named years
// deliberately belong to both regimes (ADR 0012, Proposed amendment).
func (w Window) Contains(at time.Time) bool { return !at.Before(w.Start) && at.Before(w.End) }

func ResearchProtocol() (Protocol, error) { return DecodeProtocol(protocolJSON) }

func DecodeProtocol(raw []byte) (Protocol, error) {
	var p Protocol
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return Protocol{}, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return Protocol{}, errors.New("protocol: trailing data")
	}
	if err := p.Validate(); err != nil {
		return Protocol{}, err
	}
	return p, nil
}

func (p Protocol) Validate() error {
	if p.Version != 1 || p.Split.IsZero() || !writableTime(p.Split) || !finite(p.DaysPerYear) || p.DaysPerYear <= 0 || len(p.Windows) != 7 {
		return errors.New("protocol: version, split, positive year length and seven windows required")
	}
	names := map[string]bool{}
	for _, w := range p.Windows {
		if w.Name == "" || names[w.Name] || w.Start.IsZero() || !writableTime(w.Start) || !writableTime(w.End) || !w.End.After(w.Start) {
			return errors.New("protocol: invalid or duplicate window")
		}
		names[w.Name] = true
	}
	return nil
}

// Fingerprint pins the whole protocol in each report and opening (ADR 0012).
// Validate must succeed before calling it.
func (p Protocol) Fingerprint() string {
	raw, _ := json.Marshal(p)
	return "sha256:" + event.HashPayload(raw)
}

func (p Protocol) WindowsAt(at time.Time) []string {
	var names []string
	for _, w := range p.Windows {
		if w.Contains(at) {
			names = append(names, w.Name)
		}
	}
	return names
}

func (p Protocol) Designation(start, end time.Time) string {
	switch {
	case start.IsZero() || end.IsZero():
		return "unknown"
	case end.Before(p.Split):
		return "in-sample"
	case start.Before(p.Split):
		return "mixed"
	default:
		return "out-of-sample"
	}
}

// PermitFit rejects any fitting span that might expose held-out data (ADR 0012).
func (p Protocol) PermitFit(start, end time.Time) error {
	if end.Before(start) || p.Designation(start, end) != "in-sample" {
		return errors.New("protocol: parameter fitting requires an entirely in-sample span")
	}
	return nil
}

type EquityPoint struct {
	At     time.Time `json:"at"`
	Equity float64   `json:"equity"`
}

type Metrics struct {
	Samples          int       `json:"samples"`
	Start            time.Time `json:"start"`
	End              time.Time `json:"end"`
	TotalReturn      float64   `json:"total_return"`
	AnnualisedReturn float64   `json:"annualised_return"`
	MaxDrawdown      float64   `json:"max_drawdown"`
	Primary          *float64  `json:"return_over_max_drawdown"`
	State            string    `json:"state"`
}

type WindowResult struct {
	Window  Window  `json:"window"`
	Metrics Metrics `json:"metrics"`
}

type Report struct {
	Protocol     Protocol       `json:"protocol"`
	ProtocolHash string         `json:"protocol_hash"`
	Designation  string         `json:"designation"`
	Full         Metrics        `json:"full"`
	InSample     Metrics        `json:"in_sample"`
	OutOfSample  Metrics        `json:"out_of_sample"`
	Windows      []WindowResult `json:"windows"`
}

// Report computes annualised net equity return / peak-to-trough fractional
// drawdown (ADR 0012). Equity must be ordered, finite and nonnegative, with
// positive opening equity. No external cash flows may be present.
func (p Protocol) Report(points []EquityPoint, start, end time.Time) (Report, error) {
	if err := p.Validate(); err != nil {
		return Report{}, err
	}
	if end.Before(start) {
		return Report{}, errors.New("report: backwards span")
	}
	for i, point := range points {
		if point.At.IsZero() || !writableTime(point.At) || !finite(point.Equity) || point.Equity < 0 || (i == 0 && point.Equity == 0) || (i > 0 && !point.At.After(points[i-1].At)) {
			return Report{}, errors.New("report: invalid or unordered equity curve")
		}
	}
	r := Report{Protocol: p, ProtocolHash: p.Fingerprint(), Designation: p.Designation(start, end)}
	r.Full = p.metrics(points)
	r.InSample = p.windowMetrics(points, time.Time{}, p.Split)
	r.OutOfSample = p.windowMetrics(points, p.Split, time.Time{})
	for _, w := range p.Windows {
		r.Windows = append(r.Windows, WindowResult{w, p.windowMetrics(points, w.Start, w.End)})
	}
	return r, nil
}

// windowMetrics carries the last pre-window mark to the boundary so the
// first in-window loss is retained, without inventing observations at the end
// of a partial window (ADR 0012, Proposed amendment).
func (p Protocol) windowMetrics(points []EquityPoint, start, end time.Time) Metrics {
	var selected []EquityPoint
	for i, point := range points {
		if point.At.Before(start) || (!end.IsZero() && !point.At.Before(end)) {
			continue
		}
		if len(selected) == 0 && i > 0 && points[i-1].At.Before(start) {
			selected = append(selected, EquityPoint{start, points[i-1].Equity})
		}
		// A boundary close is later than the carried opening mark even when the
		// timestamp is equal; metrics retains both equities for drawdown.
		selected = append(selected, point)
	}
	return p.metrics(selected)
}

func (p Protocol) metrics(points []EquityPoint) Metrics {
	m := Metrics{Samples: len(points), State: "no-data"}
	if len(points) == 0 {
		return m
	}
	m.Start = points[0].At
	m.End = points[len(points)-1].At
	if len(points) < 2 || !m.End.After(m.Start) || points[0].Equity == 0 {
		m.State = "insufficient-history"
		return m
	}
	peak := points[0].Equity
	for _, point := range points {
		peak = max(peak, point.Equity)
		m.MaxDrawdown = max(m.MaxDrawdown, (peak-point.Equity)/peak)
	}
	growth := points[len(points)-1].Equity / points[0].Equity
	m.TotalReturn = growth - 1
	years := m.End.Sub(m.Start).Hours() / (24 * p.DaysPerYear)
	m.AnnualisedReturn = math.Pow(growth, 1/years) - 1
	if !finite(m.TotalReturn) || !finite(m.AnnualisedReturn) {
		m.TotalReturn = 0
		m.AnnualisedReturn = 0
		m.State = "non-finite-return"
		return m
	}
	if m.MaxDrawdown == 0 {
		m.State = "zero-drawdown"
		return m
	}
	primary := m.AnnualisedReturn / m.MaxDrawdown
	if !finite(primary) {
		m.State = "non-finite-ratio"
		return m
	}
	m.Primary = &primary
	m.State = "ok"
	return m
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Opening records an attempt before held-out evaluation, including attempts
// that later fail. Identity is the Variant across all parameter hashes;
// every repeat gets its own hypothesis (ADR 0012, Proposed amendment).
type Opening struct {
	Variant           string   `json:"variant"`
	RunID             string   `json:"run_id"`
	ConfigurationHash string   `json:"configuration_hash"`
	ProtocolHash      string   `json:"protocol_hash"`
	Hypothesis        string   `json:"hypothesis"`
	Repeat            bool     `json:"repeat"`
	Prior             []string `json:"prior"`
}

func (p Protocol) Opening(run Run, prior []Opening) (Opening, error) {
	if err := checkRunID(run.RunID); err != nil {
		return Opening{}, err
	}
	if run.Variant == "" || run.Variant == Baseline {
		return Opening{}, errors.New("opening: a declared Variant is required")
	}
	o := Opening{Variant: run.Variant, RunID: run.RunID, ConfigurationHash: event.ConfigurationHash(run.Configuration), ProtocolHash: p.Fingerprint()}
	o.Hypothesis = o.ConfigurationHash + "/" + o.RunID
	for _, old := range prior {
		if old.Variant != o.Variant {
			continue
		}
		if old.Hypothesis == o.Hypothesis {
			return Opening{}, fmt.Errorf("opening: attempt %s already reserved", o.Hypothesis)
		}
		o.Prior = append(o.Prior, old.Hypothesis)
	}
	slices.Sort(o.Prior)
	o.Prior = slices.Compact(o.Prior)
	o.Repeat = len(o.Prior) > 0
	return o, nil
}

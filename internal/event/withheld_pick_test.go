package event_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// ADR 0026's amendment of 2026-10-29 keeps a missing-mark pick's share in
// cash and records its cause independently of a withheld whole rebalance.
func TestWithheldPicksVersionAndValidation(t *testing.T) {
	if got := event.TargetsDecidedVersions(); !slices.Equal(got, []uint32{7, 8, 9, 10, 11}) {
		t.Fatalf("decision versions = %v, want [7 8 9 10 11]", got)
	}
	valid := validMomentumWithheldPick
	p := valid()
	if err := p.ValidateSchema(10); err != nil {
		t.Fatal(err)
	}
	for _, version := range []uint32{7, 8, 9} {
		if err := p.ValidateSchema(version); err == nil || !strings.Contains(err.Error(), "version 10") {
			t.Errorf("version %d accepted withheld picks: %v", version, err)
		}
		old := validMomentumDecided()
		if err := old.ValidateSchema(version); err != nil {
			t.Errorf("old version %d: %v", version, err)
		}
	}
	for name, mutate := range map[string]func(*event.TargetsDecidedPayload){
		"empty instrument": func(p *event.TargetsDecidedPayload) { p.WithheldPicks[0].InstrumentID = "" },
		"not targeted":     func(p *event.TargetsDecidedPayload) { p.WithheldPicks[0].InstrumentID = "ZZZ" },
		"unknown reason":   func(p *event.TargetsDecidedPayload) { p.WithheldPicks[0].Reason = "price-band" },
		"empty detail":     func(p *event.TargetsDecidedPayload) { p.WithheldPicks[0].Detail = "" },
		"empty failure":    func(p *event.TargetsDecidedPayload) { p.WithheldPicks[0].Failure = "" },
		"unknown failure":  func(p *event.TargetsDecidedPayload) { p.WithheldPicks[0].Failure = "retry" },
		"duplicate":        func(p *event.TargetsDecidedPayload) { p.WithheldPicks = append(p.WithheldPicks, p.WithheldPicks[0]) },
		"unsorted": func(p *event.TargetsDecidedPayload) {
			p.WithheldPicks = append([]event.WithheldPick{{InstrumentID: "IEF", Reason: "missing-mark", Detail: "IBKR error 200", Failure: "final"}}, p.WithheldPicks...)
		},
		"whole month withheld": func(p *event.TargetsDecidedPayload) {
			p.Outcome = event.TargetsOutcomeWithheld
			p.WithheldReason = event.WithheldMissingBars
			p.Detail = "missing bars"
			p.Targets = nil
		},
		"boost": func(p *event.TargetsDecidedPayload) {
			picks := p.WithheldPicks
			*p = validTargetsDecided()
			p.WithheldPicks = picks
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := valid()
			mutate(&p)
			if err := p.ValidateSchema(10); err == nil {
				t.Fatal("accepted invalid withheld pick")
			}
		})
	}
	for _, failure := range []string{"transient", "final", "unknown"} {
		p := valid()
		p.WithheldPicks[0].Failure = failure
		if err := p.ValidateWritten(); err != nil {
			t.Errorf("failure %s: %v", failure, err)
		}
	}
}

func validMarkUnavailable() event.MarkUnavailablePayload {
	return event.MarkUnavailablePayload{InstrumentID: "AAA", AsOf: twSession, Source: "ibkr", Detail: "IBKR error 200", Failure: "final"}
}

func TestMarkUnavailableValidation(t *testing.T) {
	valid := validMarkUnavailable()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	transient := valid
	transient.Failure = "transient"
	if err := transient.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*event.MarkUnavailablePayload){
		"instrument": func(p *event.MarkUnavailablePayload) { p.InstrumentID = "" },
		"time":       func(p *event.MarkUnavailablePayload) { p.AsOf = time.Time{} },
		"source":     func(p *event.MarkUnavailablePayload) { p.Source = "" },
		"detail":     func(p *event.MarkUnavailablePayload) { p.Detail = "" },
		"failure":    func(p *event.MarkUnavailablePayload) { p.Failure = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			p := valid
			mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("accepted invalid mark failure")
			}
		})
	}
}

func validMomentumWithheldPick() event.TargetsDecidedPayload {
	p := validMomentumDecided()
	p.WithheldPicks = []event.WithheldPick{{InstrumentID: "AAA", Reason: "missing-mark", Detail: "contract lookup did not end within 10s, asked twice", Failure: "transient"}}
	return p
}

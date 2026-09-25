package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
)

// researchResult is an append-only sidecar of the existing registry entry
// (ADR 0012, Accepted amendment). Legacy entry and journal formats are unchanged.
type researchResult struct {
	Version           uint32             `json:"version"`
	RunID             string             `json:"run_id"`
	Variant           string             `json:"variant"`
	ConfigurationHash string             `json:"configuration_hash"`
	Status            registry.Status    `json:"status"`
	Fit               bool               `json:"fit"`
	Artefacts         registry.Artefacts `json:"artefacts"`
	Report            registry.Report    `json:"report"`
	Opening           *registry.Opening  `json:"opening,omitempty"`
	Error             string             `json:"error,omitempty"`
}

// prepareResearch reserves held-out exposure before the simulator executes.
// Failed reservations stop execution; failed runs retain their reservation
// (ADR 0012, Accepted amendment). Inputs are the same in-memory fixtures drive uses.
//
// It also returns the declared span it derived start and end from: the whole
// input this run was GIVEN, every bar and corporate action, not merely
// whatever the run goes on to apply before it might stop. finishResearch
// reports against this same span, on purpose (ADR 0012, Accepted amendment:
// "the designation uses all input dates"): the decision to reserve an
// opening is made from it, before execution, so the report beside that
// opening states the designation that decision was made under, never a
// narrower one a run's own early stop happened to leave behind.
func prepareResearch(opts options, cfg event.ConfigurationPayload, bars []event.CompletedBarPayload, actions []event.CorporateActionPayload) (opening *registry.Opening, start, end time.Time, err error) {
	p, err := registry.ResearchProtocol()
	if err != nil {
		return nil, start, end, err
	}
	include := func(at time.Time) {
		if start.IsZero() || at.Before(start) {
			start = at
		}
		if at.After(end) {
			end = at
		}
	}
	for _, b := range bars {
		include(b.PeriodEnd)
	}
	for _, a := range actions {
		include(a.EffectiveAt)
	}
	if opts.fit {
		if err := p.PermitFit(start, end); err != nil {
			return nil, start, end, err
		}
	}
	if opts.variant == "" || opts.variant == registry.Baseline || p.Designation(start, end) == "in-sample" {
		return nil, start, end, nil
	}
	if opts.registryPath == "" {
		return nil, start, end, errors.New("research: Variant out-of-sample evaluation requires a registry")
	}
	opening, err = reserveOpening(opts, cfg, p, start, end)
	return opening, start, end, err
}

func reserveOpening(opts options, cfg event.ConfigurationPayload, p registry.Protocol, declaredStart, declaredEnd time.Time) (*registry.Opening, error) {
	// An exclusive directory serialises local and shared-filesystem openings.
	// A crash leaves the lock in place; automatic stale-lock removal could
	// reopen unseen evidence, so recovery is an operator audit (ADR 0012).
	creating := missingDirs(opts.registryPath)
	if err := os.MkdirAll(opts.registryPath, 0o750); err != nil {
		return nil, err
	}
	lock := filepath.Join(opts.registryPath, ".research-lock")
	if err := os.Mkdir(lock, 0o750); err != nil {
		return nil, fmt.Errorf("research: cannot acquire registry lock; audit an abandoned lock before removing it: %w", err)
	}
	defer func() { _ = os.Remove(lock) }()
	for _, dir := range syncedAfter(opts.registryPath, opts.registryPath, creating) {
		if err := syncDir(dir); err != nil {
			return nil, err
		}
	}
	prior, err := p.PriorOpenings(registryStore{root: opts.registryPath})
	if err != nil {
		return nil, err
	}
	opening, err := p.Opening(registry.Run{RunID: opts.runID, Variant: opts.variant, Configuration: cfg}, declaredStart, declaredEnd, prior)
	if err != nil {
		return nil, err
	}
	dir, err := registry.Dir(opening.ConfigurationHash)
	if err != nil {
		return nil, err
	}
	if err := installResearch(opts.registryPath, filepath.Join(dir, opts.runID+".opening"), opening); err != nil {
		return nil, err
	}
	return &opening, nil
}

// installResearch publishes a complete, fsynced sidecar by exclusive hard link
// and never replaces evidence (ADR 0017, ADR 0018).
func installResearch(root, relative string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	destination := filepath.Join(root, relative)
	directory := filepath.Dir(destination)
	creating := missingDirs(directory)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(directory, ".research-*.partial")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), destination); err != nil {
		return err
	}
	for _, dir := range syncedAfter(directory, root, creating) {
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func finishResearch(opts options, cfg event.ConfigurationPayload, result outcome, out io.Writer) error {
	p, err := registry.ResearchProtocol()
	if err != nil {
		return err
	}
	report, err := p.Report(result.equity, result.declaredStart, result.declaredEnd)
	if err != nil {
		return err
	}
	r := researchResult{Version: 1, RunID: opts.runID, Variant: opts.variant, ConfigurationHash: event.ConfigurationHash(cfg), Status: result.status(), Fit: opts.fit, Report: report, Opening: result.opening}
	if err := result.failure(); err != nil {
		r.Error = err.Error()
	}
	if result.installed {
		// Unregistered runs still report their metrics to the operator.
		if opts.registryPath != "" {
			r.Artefacts, err = anchorJournal(opts.registryPath, opts.outPath)
			if err != nil {
				return err
			}
		}
	}
	if opts.registryPath != "" {
		dir, err := registry.Dir(r.ConfigurationHash)
		if err != nil {
			return err
		}
		if err := installResearch(opts.registryPath, filepath.Join(dir, opts.runID+".report"), r); err != nil {
			return fmt.Errorf("research: retain report: %w", err)
		}
	}
	var message bytes.Buffer
	if r.Opening != nil && r.Opening.Repeat {
		fmt.Fprintf(&message, "out-of-sample repeated: new hypothesis %s\n", r.Opening.Hypothesis)
	}
	if err := json.NewEncoder(&message).Encode(r); err != nil {
		return err
	}
	_, err = out.Write(message.Bytes())
	return err
}

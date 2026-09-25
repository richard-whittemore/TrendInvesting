package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// PriorOpenings reads reservations and legacy runs across every configuration
// hash. A failed legacy run of unknown span counts conservatively as an
// opening (ADR 0012, Accepted amendment) -- but only a LEGACY one: a run this
// protocol itself reported on (it carries a .report sidecar, scanned below
// for its runID) states its own exposure through its .opening sidecar, if it
// reserved one at all, and is never guessed at again from its span. Without
// that distinction, a run this protocol refused before it touched any
// held-out input -- a -fit rejection, in particular -- would be recorded
// with an unknown span and wrongly counted as exposure it never took. The
// caller must serialise read plus install.
func (p Protocol) PriorOpenings(store Store) ([]Opening, error) {
	dirs, err := store.ReadDir(".")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	slices.Sort(dirs)
	var prior []Opening
	for _, dir := range dirs {
		if !strings.HasPrefix(dir, "sha256-") {
			continue
		}
		hash := strings.Replace(dir, "-", ":", 1)
		names, err := store.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		slices.Sort(names)
		reported := map[string]bool{}
		for _, name := range names {
			if id, ok := strings.CutSuffix(name, ".report"); ok {
				reported[id] = true
			}
		}
		runs, err := Runs(store, hash)
		if err != nil {
			return nil, err
		}
		for _, run := range runs {
			if reported[run.RunID] {
				continue
			}
			if run.Variant != Baseline && p.Designation(run.SpanStart, run.SpanEnd) != "in-sample" {
				prior = append(prior, Opening{Variant: run.Variant, Hypothesis: run.ConfigurationHash + "/" + run.RunID})
			}
		}
		for _, name := range names {
			if !strings.HasSuffix(name, ".opening") {
				continue
			}
			raw, err := store.ReadFile(path.Join(dir, name))
			if err != nil {
				return nil, err
			}
			o, err := DecodeOpening(raw)
			if err != nil {
				return nil, err
			}
			if o.ConfigurationHash != hash || name != o.RunID+".opening" {
				return nil, errors.New("opening: identity does not match its registry path")
			}
			prior = append(prior, o)
		}
	}
	return prior, nil
}

func DecodeOpening(raw []byte) (Opening, error) {
	var o Opening
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&o); err != nil {
		return Opening{}, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return Opening{}, errors.New("opening: trailing data")
	}
	if err := checkRunID(o.RunID); err != nil {
		return Opening{}, err
	}
	if _, err := Dir(o.ConfigurationHash); err != nil {
		return Opening{}, err
	}
	if _, err := Dir(o.ProtocolHash); err != nil {
		return Opening{}, err
	}
	if o.Variant == "" || o.Variant == Baseline || o.Hypothesis != o.ConfigurationHash+"/"+o.RunID || o.Repeat != (len(o.Prior) > 0) {
		return Opening{}, errors.New("opening: invalid attribution")
	}
	// The declared span this opening reserved is retained so the reservation
	// can never be read with no dates behind it (ADR 0012, Accepted
	// amendment): a decoded opening is held to the same shape Opening itself
	// only ever produces.
	if o.DeclaredStart.IsZero() || o.DeclaredEnd.IsZero() || !writableTime(o.DeclaredStart) || !writableTime(o.DeclaredEnd) || o.DeclaredEnd.Before(o.DeclaredStart) {
		return Opening{}, errors.New("opening: invalid declared span")
	}
	return o, nil
}

// EquityCurve uses snapshot as-of times, not delayed delivery times (ADR 0021).
// Cash flows need a return-adjustment policy and are refused (ADR 0012,
// Accepted amendment); this fixture runner emits none.
func EquityCurve(entries []journal.Entry) ([]EquityPoint, error) {
	var points []EquityPoint
	for _, e := range entries {
		if e.Kind != journal.KindInput {
			continue
		}
		switch e.Envelope.Type {
		case event.CashMovementEventType:
			return nil, errors.New("report: external cash flows require an adjusted return methodology")
		case event.AccountSnapshotEventType:
			if e.Envelope.SchemaVersion != event.AccountSnapshotSchemaVersion {
				return nil, errors.New("report: unsupported account snapshot schema")
			}
			var p event.AccountSnapshotPayload
			if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
				return nil, err
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("report: invalid equity snapshot: %w", err)
			}
			points = append(points, EquityPoint{p.AsOf, p.Equity})
		}
	}
	return points, nil
}

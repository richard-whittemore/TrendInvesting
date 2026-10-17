// Command decision_equivalence feeds two encoded streams of the same
// recordings, the full one (a bar for every universe member every session)
// and the compact one (one universe snapshot per session and a close history
// per ranked member), through a fresh momentum run each, under the
// configuration given, and fails unless both runs emit the same records,
// byte for byte. On success it prints the compact run's
// strategy.targets.decided payloads, one per line, for the Python test to
// read.
//
// Usage: go run decision_equivalence.go CONFIG FULL COMPACT
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

const strategyVersion = "momentum/decision-equivalence"

func main() {
	if len(os.Args) != 4 {
		fail(fmt.Errorf("usage: decision_equivalence CONFIG FULL COMPACT"))
	}
	raw, err := os.ReadFile(os.Args[1])
	must(err)
	var cfg event.TargetWeightConfigurationPayload
	must(json.Unmarshal(raw, &cfg))
	full, err := decide(cfg, os.Args[2])
	must(err)
	compact, err := decide(cfg, os.Args[3])
	must(err)
	if len(full) != len(compact) {
		fail(fmt.Errorf("the full stream emitted %d records, the compact one %d", len(full), len(compact)))
	}
	for i := range full {
		if full[i].ID != compact[i].ID || !bytes.Equal(full[i].Payload, compact[i].Payload) {
			fail(fmt.Errorf("record %d differs:\nfull    %s %s\ncompact %s %s", i, full[i].ID, full[i].Payload, compact[i].ID, compact[i].Payload))
		}
	}
	for _, e := range compact {
		if e.Type == event.TargetsDecidedEventType {
			fmt.Println(string(e.Payload))
		}
	}
}

// decide runs one stream through a fresh momentum run, deciding from its
// first session, and returns everything the run emitted.
func decide(cfg event.TargetWeightConfigurationPayload, path string) ([]event.Envelope, error) {
	inputs, err := read(path)
	if err != nil {
		return nil, err
	}
	var from time.Time
	for _, e := range inputs {
		if e.Type == event.SessionClosedEventType {
			from = e.EventTime
			break
		}
	}
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: from})
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	config := event.Envelope{
		ID: "configuration", Type: event.TargetWeightConfigurationEventType, EnvelopeVersion: event.CurrentEnvelopeVersion,
		SchemaVersion: event.TargetWeightConfigurationSchemaVersion, EventTime: from, RecordedAt: from, Sequence: 1,
		CorrelationID: "decision-equivalence", Source: "decision-equivalence", StrategyVersion: strategyVersion,
		ConfigurationHash: h.ConfigurationHash(), PayloadHash: event.HashPayload(payload), Payload: payload,
	}
	var out []event.Envelope
	for _, e := range append([]event.Envelope{config}, inputs...) {
		// The composition root stamps each producer's inputs with the run's
		// identity; the encoder's test identity is not this run's.
		e.StrategyVersion, e.ConfigurationHash = strategyVersion, h.ConfigurationHash()
		emitted, err := h.Apply(context.Background(), e)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, e.ID, err)
		}
		out = append(out, emitted...)
	}
	return out, nil
}

func read(path string) ([]event.Envelope, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []event.Envelope
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<30)
	for scanner.Scan() {
		var e event.Envelope
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "decision_equivalence:", err)
	os.Exit(1)
}

// Command envelope_contract checks the market-data encoder's output against
// the Go contracts the target-weight engines read (ADR 0015): Python tests
// feed the exact stream, one envelope per line, on stdin.
//
// Every envelope must pass Envelope.Validate, carry one of the four
// type/version pairs the encoder writes, and decode into its payload with no
// unknown field (a misspelt key would otherwise decode as a zero value) and
// pass that payload's Validate. Sequences are contiguous and IDs unique, as a
// replay requires.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

type validated interface{ Validate() error }

func payloadFor(e event.Envelope) (validated, error) {
	switch {
	case e.Type == event.SessionCalendarEventType && e.SchemaVersion == event.SessionCalendarSchemaVersion:
		return &event.SessionCalendarPayload{}, nil
	case e.Type == event.UniverseSnapshotEventType && e.SchemaVersion == event.UniverseSnapshotSchemaVersion:
		return &event.UniverseSnapshotPayload{}, nil
	case e.Type == event.CompletedBarEventType && e.SchemaVersion == event.CompletedBarAdjustedVersion:
		return &event.CompletedBarAdjustedPayload{}, nil
	case e.Type == event.SessionClosedEventType && e.SchemaVersion == event.SessionClosedSchemaVersion:
		return &event.SessionClosedPayload{}, nil
	case e.Type == event.CloseHistoryEventType && e.SchemaVersion == event.CloseHistorySchemaVersion:
		return &event.CloseHistoryPayload{}, nil
	case e.Type == event.SessionRetractedEventType && e.SchemaVersion == event.SessionRetractedSchemaVersion:
		return &event.SessionRetractedPayload{}, nil
	}
	return nil, fmt.Errorf("unexpected type/schema %s/%d", e.Type, e.SchemaVersion)
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<30)
	seen := map[string]bool{}
	var last uint64
	n := 0
	for scanner.Scan() {
		n++
		var e event.Envelope
		must(n, json.Unmarshal(scanner.Bytes(), &e))
		must(n, e.Validate())
		if seen[e.ID] {
			must(n, fmt.Errorf("id %s repeated", e.ID))
		}
		seen[e.ID] = true
		if last != 0 && e.Sequence != last+1 {
			must(n, fmt.Errorf("sequence %d after %d", e.Sequence, last))
		}
		last = e.Sequence
		p, err := payloadFor(e)
		must(n, err)
		dec := json.NewDecoder(bytes.NewReader(e.Payload))
		dec.DisallowUnknownFields()
		must(n, dec.Decode(p))
		must(n, p.Validate())
	}
	must(n, scanner.Err())
	if n == 0 {
		must(n, fmt.Errorf("empty stream"))
	}
	fmt.Printf("envelope_contract: %d envelopes valid\n", n)
}

func must(line int, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "line %d: %v\n", line, err)
		os.Exit(1)
	}
}

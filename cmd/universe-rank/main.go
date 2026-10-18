// Command universe-rank tells the market-data step which universe members a
// momentum run will rank, so the step writes a market.close.history for
// those members only (the compact universe input; ADR 0026, point 1:
// ranking is a rule, so it lives in Go and the step never repeats it).
//
// It reads the run's target-weight configuration from -config and, on stdin,
// one JSON request per line, {"universe": <market.universe.snapshot payload>,
// "next_session": "YYYY-MM-DD"}, and writes one targetweight.HistoryRequest
// per line to stdout, in the same order. It reads and writes nothing else,
// and any request the run would refuse stops it with status 1.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

type request struct {
	Universe    event.UniverseSnapshotPayload `json:"universe"`
	NextSession string                        `json:"next_session"`
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "universe-rank:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("universe-rank", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the momentum run's JSON target-weight configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("-config is required")
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := strictDecode(raw, &cfg); err != nil {
		return fmt.Errorf("%s: %w", *configPath, err)
	}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<30)
	out := json.NewEncoder(stdout)
	for line := 1; scanner.Scan(); line++ {
		var req request
		if err := strictDecode(scanner.Bytes(), &req); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		answer, err := targetweight.MomentumHistoryRequest(cfg, req.Universe, req.NextSession)
		if err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		if err := out.Encode(answer); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// strictDecode decodes raw into v, refusing an unknown field: a misspelt key
// would otherwise decode as a zero value.
func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

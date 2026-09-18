package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// replayEquivalence compares recorded decisions with a fresh reducer's
// byte-identical, ordered output (ADR 0017), independently of chain verification.
// The reducer must use the journal's own header and configuration; external
// configuration would test a different run. Either check can fail independently.
//
// Recorded fills are simulator outputs (ADR 0005). Replaying them checks
// reducer Setup evaluation, sizing and Add/stop/exit decisions, but cannot
// prove that the simulator would reproduce those fills from bars alone.
func replayEquivalence(r io.Reader) (*replay.Divergence, error) {
	header, records, err := journal.Read(r)
	if err != nil {
		return nil, err
	}
	if err := journal.CheckIdentity(header, records); err != nil {
		return nil, err
	}
	// Neither of these is the chain's question. A journal whose chain was
	// repaired after the fact verifies perfectly while its header describes
	// more than one run, or claims a period the records it holds never
	// reached; reading it as the record of a run is where that has to fail
	// closed.
	if err := journal.CheckSpan(header, records); err != nil {
		return nil, err
	}
	inputs, decisions, err := journal.Split(records)
	if err != nil {
		return nil, err
	}
	emitted, err := replayJournalInputs(header, inputs)
	if err != nil {
		return nil, err
	}
	return replay.Equivalent(decisions, emitted), nil
}

// replayJournalInputs constructs the journal's declared reducer and returns
// its decisions. Before replay, header strategy ID and configuration hash must
// match the recorded configuration (ADR 0012), and the rules version must
// match strategy.RulesVersion (ADR 0016). The build suffix is traceability only.
// These identity failures are refusals, not decision divergences: changed
// engine rules do not establish that a journal is wrong.
func replayJournalInputs(header journal.Header, inputs []event.Envelope) ([]event.Envelope, error) {
	strategyID, rulesVersion, _, err := event.DecomposeStrategyVersion(header.StrategyVersion)
	if err != nil {
		return nil, fmt.Errorf("backtest: %w", err)
	}
	if rulesVersion != strategy.RulesVersion {
		return nil, fmt.Errorf("backtest: the journal's strategy version %q states rules version %q, but this build's rules version is %q: replay compares on the rules version alone (ADR 0016), and a mismatch means the engine has moved on, not that the journal is wrong", header.StrategyVersion, rulesVersion, strategy.RulesVersion)
	}

	payload, err := configurationPayloadFrom(inputs)
	if err != nil {
		return nil, err
	}
	if strategyID != payload.StrategyID {
		return nil, fmt.Errorf("backtest: the journal's header names strategy %q, but the configuration it records declares %q", strategyID, payload.StrategyID)
	}
	if recomputed := event.ConfigurationHash(payload); recomputed != header.ConfigurationHash {
		return nil, fmt.Errorf("backtest: the journal's header claims configuration %q, but the configuration its own input stream records hashes to %q", header.ConfigurationHash, recomputed)
	}
	reducer, err := strategy.NewReducer(header.StrategyVersion, payload)
	if err != nil {
		return nil, fmt.Errorf("backtest: %w", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		return nil, fmt.Errorf("backtest: %w", err)
	}
	emitted, err := engine.Run(context.Background(), inputs)
	if err != nil {
		return nil, fmt.Errorf("backtest: replay the journal's inputs: %w", err)
	}
	return emitted, nil
}

// configurationPayloadFrom finds the run's configuration event among the
// journal's own inputs: the reducer replay equivalence builds must come from
// what the run actually declared, never from a file supplied on the side.
func configurationPayloadFrom(inputs []event.Envelope) (event.ConfigurationPayload, error) {
	for _, input := range inputs {
		if input.Type != event.ConfigurationEventType {
			continue
		}
		var payload event.ConfigurationPayload
		if err := json.Unmarshal(input.Payload, &payload); err != nil {
			return event.ConfigurationPayload{}, fmt.Errorf("backtest: decode the journal's own configuration payload: %w", err)
		}
		return payload, nil
	}
	return event.ConfigurationPayload{}, errors.New("backtest: the journal records no configuration event; a reducer cannot be built without one")
}

// doReplay opens the journal at path and reports replay equivalence: either
// that it holds, or the first divergence.
func doReplay(path string, out io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("backtest: open the journal to replay: %w", err)
	}
	defer func() { _ = file.Close() }()

	divergence, err := replayEquivalence(file)
	if err != nil {
		return fmt.Errorf("backtest: %s: %w", path, err)
	}
	if divergence != nil {
		return fmt.Errorf("backtest: %s: replay diverges at decision %d: recorded %s, this build now produces %s",
			path, divergence.Index, describeDivergent(divergence.Want), describeDivergent(divergence.Got))
	}
	if _, err := fmt.Fprintf(out, "journal %s replays byte-identically\n", path); err != nil {
		return fmt.Errorf("backtest: report the replay: %w", err)
	}
	return nil
}

// describeDivergent names one side of a divergence for the CLI report. Want
// or Got is nil when the two streams differed only in length.
func describeDivergent(e *event.Envelope) string {
	if e == nil {
		return "nothing"
	}
	return fmt.Sprintf("%s (%s)", e.Type, e.ID)
}

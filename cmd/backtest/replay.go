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

// replayEquivalence asks one journal whether it replays: feed its own
// recorded inputs back through a freshly constructed reducer and report
// whether the decisions it emits are byte-identical, in order, to the
// decisions the journal recorded (ADR 0017).
//
// Replay equivalence is a different question from chain verification, and
// the two are deliberately separate calls: the chain answers "was this file
// edited after it was written", this answers "does this engine still produce
// these decisions", and a journal can fail either independently (ADR 0017).
//
// The reducer is built from the journal's OWN header and input stream, never
// from a configuration supplied on the side: a journal is self-describing
// evidence, and a replay that needed something outside the file would not be
// testing the file at all.
//
// This exercises the REDUCER's determinism. A journal's inputs include the
// fills the fill simulator decided (ADR 0005), not the reducer, so replaying
// them re-derives the reducer's own decisions — Setup evaluation, sizing,
// the Add/stop/exit rules — and says nothing about whether the simulator
// would produce the same fills again from the bars alone.
func replayEquivalence(r io.Reader) (*replay.Divergence, error) {
	header, records, err := journal.Read(r)
	if err != nil {
		return nil, err
	}
	if err := journal.CheckIdentity(header, records); err != nil {
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

// replayJournalInputs builds the reducer a journal's own header calls for and
// runs inputs through it, returning exactly what this build now decides.
//
// Three refusals come before any replay, and all are failures of a different
// kind from a divergence. Each checks one part of the header against what the
// journal itself records, so that every part of the run's identity is
// load-bearing rather than decorative:
//
//   - A header stating a rules version other than this build's
//     (strategy.RulesVersion) is refused. Replay compares runs on the rules
//     version alone; the build suffix is traceability only (ADR 0016). A
//     mismatch means this engine's rules have moved on since the journal was
//     written, which is not evidence that the journal is wrong.
//   - A header naming a strategy other than the one its own configuration
//     declares is refused. The strategy id is the other half of the header's
//     strategy version, and leaving it unchecked would let a journal claim
//     one strategy while having run another.
//   - A header whose configuration hash disagrees with the configuration the
//     journal's own input stream carries is refused. The header's hash is the
//     run's identity (ADR 0012), so replaying under a configuration the
//     header does not claim would answer a question nobody asked.
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

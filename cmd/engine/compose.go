package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// composition is the strategy this run decides under, selected by its
// configuration: the handler every input is applied to, the run's identity
// (ADR 0016), and the configuration input that opens its journal.
type composition struct {
	handler           replay.Handler
	configurationHash string
	strategyVersion   string
	configEnvelope    event.Envelope
	// mode is the trading mode the configuration declares (ADR 0026, point
	// 6, "Identity"), or modeBacktest for a Turtle configuration, which has
	// none and is run only as a backtest.
	mode string
	// accountID is the account a target-weight configuration names; empty
	// for a Turtle configuration.
	accountID string
}

// configurationKind is the one field read before a configuration is decoded:
// a target-weight configuration names its target-weight strategy (ADR 0026,
// point 6), and anything naming none of them is a Turtle configuration
// (AGENTS.md rule 4: the two share no rule and no state).
type configurationKind struct {
	StrategyID string `json:"strategy_id"`
}

// compose reads the configuration at path and composes its strategy:
//
//   - a Boost configuration (strategy_id "boost"), the one target-weight
//     strategy composed here, gets the target-weight handler, which plans
//     each decision's orders and passes
//     them through the risk gate (internal/targetweight, internal/orderplan,
//     internal/risklimit; ADR 0026). Its first session decided is asOf:
//     sessions before it only feed the indicators, and asOf is the
//     configuration input's own time, so a replay reads it back from the
//     journal;
//   - a momentum configuration (strategy_id "momentum") is refused: it is a
//     target-weight strategy this command does not compose, never a Turtle
//     configuration;
//   - any other is the Turtle reducer (internal/strategy), as cmd/backtest
//     composes it.
func compose(path, build string, asOf time.Time) (composition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return composition{}, fmt.Errorf("engine: read the configuration: %w", err)
	}
	var kind configurationKind
	if err := json.Unmarshal(raw, &kind); err != nil {
		return composition{}, fmt.Errorf("engine: decode the configuration in %s: %w", path, err)
	}
	switch kind.StrategyID {
	case event.TargetWeightStrategyBoost:
		return composeTargetWeight(path, raw, build, asOf)
	case event.TargetWeightStrategyMomentum:
		return composition{}, fmt.Errorf("engine: the configuration in %s names strategy_id %q, a target-weight strategy this engine does not compose: of the target-weight strategies it composes %q only",
			path, kind.StrategyID, event.TargetWeightStrategyBoost)
	}
	return composeTurtle(path, build, asOf)
}

func composeTurtle(path, build string, asOf time.Time) (composition, error) {
	cfg, err := readConfiguration(path)
	if err != nil {
		return composition{}, err
	}
	strategyVersion := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, build)
	reducer, err := strategy.NewReducer(strategyVersion, cfg)
	if err != nil {
		return composition{}, fmt.Errorf("engine: %w", err)
	}
	envelope, err := configurationEnvelope(cfg, strategyVersion, asOf)
	if err != nil {
		return composition{}, err
	}
	return composition{
		handler: reducer, configurationHash: event.ConfigurationHash(cfg), strategyVersion: strategyVersion,
		configEnvelope: envelope, mode: modeBacktest,
	}, nil
}

func composeTargetWeight(path string, raw []byte, build string, asOf time.Time) (composition, error) {
	var cfg event.TargetWeightConfigurationPayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return composition{}, fmt.Errorf("engine: decode the target-weight configuration in %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return composition{}, fmt.Errorf("engine: the configuration in %s is invalid: %w", path, err)
	}
	strategyVersion := event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, build)
	handler, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: asOf})
	if err != nil {
		return composition{}, fmt.Errorf("engine: %w", err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return composition{}, fmt.Errorf("engine: encode the configuration payload: %w", err)
	}
	hash := handler.ConfigurationHash()
	envelope := event.Envelope{
		ID:                "configuration:" + hash,
		Type:              event.TargetWeightConfigurationEventType,
		SchemaVersion:     event.TargetWeightConfigurationSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         asOf,
		RecordedAt:        asOf,
		Sequence:          configurationSequence,
		Source:            sourceEngine,
		StrategyVersion:   strategyVersion,
		ConfigurationHash: hash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}
	return composition{
		handler: handler, configurationHash: hash, strategyVersion: strategyVersion,
		configEnvelope: envelope, mode: cfg.Mode, accountID: cfg.AccountID,
	}, nil
}

// checkSendingGate refuses -mode live: a live engine releases its orders
// marked to be sent to a live account, and no live order goes out before the
// limited-live gate (AGENTS.md rule 5; docs/live-trading/plan.md section 9,
// Phase 5). Paper sends only to a paper account, which the configuration
// binds to its mode (event.PaperAccountPrefix), so -mode paper is the one
// mode that releases orders to send. Checked before anything is created.
func checkSendingGate(declared string) error {
	if declared == modeLive {
		return errors.New("engine: -mode live is refused until the limited-live gate (AGENTS.md rule 5; docs/live-trading/plan.md section 9, Phase 5): only -mode paper, bound to a paper account, releases orders to send")
	}
	return nil
}

// checkModeBinding refuses a run whose declared -mode is not the mode its
// configuration was hashed with: the mode is part of the configuration's
// identity (ADR 0026, point 6), so a dry-run configuration can never be
// started as paper or live, nor the reverse. A Turtle configuration declares
// no mode and runs only as a backtest.
func checkModeBinding(declared string, c composition) error {
	if declared == c.mode {
		return nil
	}
	if c.mode == modeBacktest {
		return fmt.Errorf("engine: -mode %s with a Turtle configuration: the Turtle reducer runs only in -mode backtest", declared)
	}
	return fmt.Errorf("engine: -mode %s does not match the configuration's mode %q; the mode is part of the hashed configuration (ADR 0026, point 6), so start the engine with the mode the configuration declares", declared, c.mode)
}

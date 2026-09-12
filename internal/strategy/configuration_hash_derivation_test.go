package strategy_test

import (
	"context"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// TestNewReducerDerivesConfigurationHashFromThePayload is #50/ADR 0016's
// central behavioural change: NewReducer no longer accepts an opaque
// configuration hash string — it takes the configuration PAYLOAD and
// derives the hash itself (event.ConfigurationHash), so there is exactly one
// place in this codebase the hash is computed. Changing MaxUnits (any field
// would do; event's own sensitivity test covers every field) between two
// otherwise-identical payloads must therefore change which
// ConfigurationHash a Reducer constructed from it will accept.
func TestNewReducerDerivesConfigurationHashFromThePayload(t *testing.T) {
	t.Parallel()

	base := validConfigurationPayload()
	varied := validConfigurationPayload()
	varied.MaxUnits = 6

	if event.ConfigurationHash(base) == event.ConfigurationHash(varied) {
		t.Fatal("fixture error: base and varied configuration payloads hash the same")
	}

	reducer, err := strategy.NewReducer(testStrategyVersion, varied)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	// An envelope declaring the BASE payload's hash must be rejected: the
	// reducer was constructed from varied, so it only recognises
	// event.ConfigurationHash(varied).
	wrongHash := configEnvelope(t, 1, day(0))
	wrongHash.ConfigurationHash = event.ConfigurationHash(base)
	if _, err := engine.Run(context.Background(), []event.Envelope{wrongHash}); err == nil {
		t.Fatal("Run() error = nil, want rejection: the envelope's configuration hash does not match the payload NewReducer was constructed with")
	}

	// A fresh reducer built from the SAME varied payload, given an envelope
	// stamped with that payload's own derived hash, must accept it.
	reducer2, err := strategy.NewReducer(testStrategyVersion, varied)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine2, err := replay.New(reducer2)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	matching := configEnvelope(t, 1, day(0))
	matching.ConfigurationHash = event.ConfigurationHash(varied)
	if _, err := engine2.Run(context.Background(), []event.Envelope{matching}); err != nil {
		t.Fatalf("Run() error = %v, want the matching derived hash to be accepted", err)
	}
}

// TestNewReducerRejectsAnInvalidConfigurationPayload confirms NewReducer
// validates the payload it is given (ConfigurationPayload.Validate) before
// ever deriving a hash from it: an invalid configuration can never produce a
// hash, rather than failing later only when a matching configuration event
// arrives.
func TestNewReducerRejectsAnInvalidConfigurationPayload(t *testing.T) {
	t.Parallel()

	if _, err := strategy.NewReducer(testStrategyVersion, event.ConfigurationPayload{}); err == nil {
		t.Fatal("NewReducer(..., ConfigurationPayload{}) error = nil, want error")
	}
}

// TestNewReducerRequiresAStrategyVersion confirms the strategyVersion
// parameter is still required, independently of the payload-derived hash.
func TestNewReducerRequiresAStrategyVersion(t *testing.T) {
	t.Parallel()

	if _, err := strategy.NewReducer("", validConfigurationPayload()); err == nil {
		t.Fatal("NewReducer(\"\", ...) error = nil, want error")
	}
}

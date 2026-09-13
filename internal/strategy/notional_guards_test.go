package strategy_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

func TestObserveSnapshotRefusesANonFiniteEquity(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}

	for _, equity := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprint(equity), func(t *testing.T) {
			_, _, _, err := account.ObserveSnapshot(jan(2, 2026), equity)
			if err == nil {
				t.Fatalf("ObserveSnapshot(%v) error = nil, want an unreadable equity to be refused", equity)
			}
			if !strings.Contains(err.Error(), "equity must be finite") {
				t.Errorf("error = %v, want it to name the equity", err)
			}
		})
	}
}

func TestApplyCashMovementRefusesANonFiniteEquityBefore(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}

	if _, err := account.ApplyCashMovement(math.NaN(), 100_000); err == nil {
		t.Fatal("ApplyCashMovement() error = nil, want a non-finite equity before to be refused")
	} else if !strings.Contains(err.Error(), "equity before must be finite") {
		t.Errorf("error = %v, want it to name the equity before", err)
	}
}

// TestApplyCashMovementChecksEachScaledFigureSeparately proves the three
// checks are not redundant.
//
// The three figures stand in a fixed order — the yearly starting figure at
// or above the measurement base, the base at or above the account itself,
// because only a Drawdown Step moves the lower two and it moves them
// downward — and every one is scaled by the SAME ratio. So on overflow the
// largest fails first and the first check is the only one that can fire.
// Rounding breaks that symmetry at the bottom of the float64 range: three
// products of the same ratio, differing by less than a factor of two, can
// round to a positive subnormal, a positive subnormal, and zero. The figure
// that reaches zero is the account itself, which the third check catches and
// neither of the first two would.
//
// The fixtures are absurd as accounts and exact as arithmetic, which is the
// point: the guards are not decoration, and a rejected cash movement leaves
// the account exactly as it found it rather than partially scaled.
func TestApplyCashMovementChecksEachScaledFigureSeparately(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		startingEquity float64
		wantErr        string
	}{
		{
			name:           "the account alone rounds to zero",
			startingEquity: 1e-307,
			wantErr:        "scaled notional account must be positive",
		},
		{
			name:           "the measurement base rounds to zero while the starting figure survives",
			startingEquity: 3e-308,
			wantErr:        "scaled measurement base must be positive",
		},
		{
			name:           "every figure rounds to zero together",
			startingEquity: 1e-308,
			wantErr:        "scaled yearly starting figure must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			account := steppedDownAccount(t, tt.startingEquity)

			// The smallest ratio a cash movement can produce: equity after is
			// one unit in the last place of equity before, so the subtraction
			// is exact and the ratio is 2^-53.
			const equityBefore = 1.0
			equityAfter := math.Ldexp(1, -53)

			_, err := account.ApplyCashMovement(equityBefore, equityAfter-equityBefore)
			if err == nil {
				t.Fatal("ApplyCashMovement() error = nil, want a scaled figure that rounded away to be refused")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tt.wantErr)
			}
		})
	}
}

// steppedDownAccount returns an account whose measurement base and account
// figure have been driven below its yearly starting figure by the Drawdown
// Step ladder, so that the three figures scaled by ApplyCashMovement are
// genuinely different values rather than three copies of one.
func steppedDownAccount(t *testing.T, startingEquity float64) *strategy.NotionalAccount {
	t.Helper()
	account, err := strategy.NewNotionalAccount(startingEquity, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount(%v) error = %v", startingEquity, err)
	}
	// Snapshots down to the deepest the ladder is defined for. The last one
	// or two are refused by the asymptote check (ADR 0007 is undefined below
	// a 50 % drawdown), which is the intended stopping point rather than a
	// failure: what this helper needs is the separation between the three
	// figures, and by then it has it.
	equity := startingEquity
	for i := 1; i <= 8; i++ {
		equity *= 0.85
		if _, _, _, err := account.ObserveSnapshot(jan(2, 2026).AddDate(0, 0, i), equity); err != nil {
			break
		}
	}
	return account
}

// TestReducerSurfacesACashMovementTheNotionalAccountCannotScale is the
// reducer's own side of the same seam. The payload's Validate has already
// checked equity before, the amount, and their sum with the identical
// arithmetic, so what reaches the account is exactly what Validate cannot
// see: a scaled figure that does not survive the scaling. The reducer wraps
// that rather than swallowing it, so a run whose Notional Account cannot be
// scaled stops instead of continuing against a figure nobody computed.
func TestReducerSurfacesACashMovementTheNotionalAccountCannotScale(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.NotionalAccount.StartingEquity = math.MaxFloat64

	// A deposit doubling equity doubles every figure with it, and twice the
	// largest float64 is not one.
	movement := cashMovementPayload(jan(2, 2026), 1, 1)

	_, err := runReducerOverAccountEvents(t, cfg, []event.Envelope{
		cashMovementEnvelopeFor(t, cfg, 2, movement),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a cash movement the account cannot be scaled by to stop the run")
	}
	if !strings.Contains(err.Error(), "scaled yearly starting figure must be finite") {
		t.Errorf("error = %v, want it to name the figure that could not be scaled", err)
	}
}

// TestRebasingFollowsTheConfiguredDateNotTheFirstOfJanuary is ADR 0007's
// re-basing date as a parameter, end to end. Under a 1 July date a January
// snapshot still belongs to the re-basing year that began the PREVIOUS July,
// so the account re-bases in July and not at the turn of the calendar year.
// Every fixture configured with the Baseline's own 1 January date leaves that
// indistinguishable from a hard-coded one, because no date is ever before
// 1 January.
func TestRebasingFollowsTheConfiguredDateNotTheFirstOfJanuary(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.NotionalAccount.RebasingMonth = 7
	cfg.NotionalAccount.RebasingDay = 1

	// August 2026 establishes the account's re-basing year. January 2027 is
	// still inside it — under a 1 January date that snapshot would re-base,
	// and it must not here. July 2027 begins the next one.
	snapshots := []event.AccountSnapshotPayload{
		accountSnapshotPayload(time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC), 990_000),
		accountSnapshotPayload(time.Date(2027, time.January, 4, 0, 0, 0, 0, time.UTC), 980_000),
		accountSnapshotPayload(time.Date(2027, time.July, 1, 0, 0, 0, 0, time.UTC), 970_000),
	}

	envelopes := make([]event.Envelope, 0, len(snapshots))
	for i, snap := range snapshots {
		envelopes = append(envelopes, accountSnapshotEnvelopeFor(t, cfg, uint64(i+2), snap))
	}

	emitted, err := runReducerOverAccountEvents(t, cfg, envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	rebasings := envelopesOfType(emitted, event.NotionalAccountRebasedEventType)
	if len(rebasings) != 1 {
		t.Fatalf("got %d re-basing(s), want exactly 1: the January snapshot is inside the re-basing year that began the previous July, and a 1 January date is what would re-base there", len(rebasings))
	}
	rebased := decodeNotionalAccountRebased(t, rebasings[0])
	if !rebased.AsOf.Equal(snapshots[2].AsOf) {
		t.Errorf("re-based at %s, want the configured re-basing date %s", rebased.AsOf.Format(time.RFC3339), snapshots[2].AsOf.Format(time.RFC3339))
	}
	if rebased.NewStartingFigure != 970_000 {
		t.Errorf("NewStartingFigure = %v, want the July snapshot's own equity 970,000", rebased.NewStartingFigure)
	}
	if rebased.PreviousStartingFigure != cfg.NotionalAccount.StartingEquity {
		t.Errorf("PreviousStartingFigure = %v, want the configured %v", rebased.PreviousStartingFigure, cfg.NotionalAccount.StartingEquity)
	}
}

// --- driving a reducer configured away from the Baseline fixture ----------
//
// The helpers in reducer_test.go and notional_reducer_test.go stamp every
// envelope with the Baseline fixture's own configuration hash, which a run
// configured differently would reject (ADR 0016). These stamp the hash of
// the configuration actually being replayed.

func runReducerOverAccountEvents(t *testing.T, cfg event.ConfigurationPayload, events []event.Envelope) ([]event.Envelope, error) {
	t.Helper()
	reducer, err := strategy.NewReducer(testStrategyVersion, cfg)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	envelopes := append([]event.Envelope{configEnvelopeFor(t, cfg, 1)}, events...)
	return engine.Run(context.Background(), envelopes)
}

func envelopeFor(t *testing.T, cfg event.ConfigurationPayload, id, eventType string, schemaVersion uint32, sequence uint64, at time.Time, payload any) event.Envelope {
	t.Helper()
	encoded := mustMarshal(t, payload)
	return event.Envelope{
		ID:                id,
		Type:              eventType,
		SchemaVersion:     schemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        at,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: event.ConfigurationHash(cfg),
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}
}

func configEnvelopeFor(t *testing.T, cfg event.ConfigurationPayload, sequence uint64) event.Envelope {
	t.Helper()
	return envelopeFor(t, cfg, "cfg-1", event.ConfigurationEventType, event.ConfigurationSchemaVersion, sequence, day(0), cfg)
}

func accountSnapshotEnvelopeFor(t *testing.T, cfg event.ConfigurationPayload, sequence uint64, payload event.AccountSnapshotPayload) event.Envelope {
	t.Helper()
	return envelopeFor(t, cfg, fmt.Sprintf("snapshot-%d", sequence), event.AccountSnapshotEventType, event.AccountSnapshotSchemaVersion, sequence, payload.AsOf, payload)
}

func cashMovementEnvelopeFor(t *testing.T, cfg event.ConfigurationPayload, sequence uint64, payload event.CashMovementPayload) event.Envelope {
	t.Helper()
	return envelopeFor(t, cfg, fmt.Sprintf("cash-movement-%d", sequence), event.CashMovementEventType, event.CashMovementSchemaVersion, sequence, payload.AsOf, payload)
}

func barEnvelopeFor(t *testing.T, cfg event.ConfigurationPayload, sequence uint64, payload event.CompletedBarPayload) event.Envelope {
	t.Helper()
	return envelopeFor(t, cfg, fmt.Sprintf("bar-%d", sequence), event.CompletedBarEventType, event.CompletedBarSchemaVersion, sequence, payload.PeriodEnd, payload)
}

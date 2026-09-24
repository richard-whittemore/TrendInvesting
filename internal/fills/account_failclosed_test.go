package fills_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// The simulated account fails closed wherever what it is told would make
// its statement untrue: a fill it cannot record, a sale of shares it does not
// hold, a holding it has no price for, and a statement the reducer refuses.
// A stand-in handler accepts everything, so each refusal is the account's
// own.

// openAccountWith returns a simulator keeping an account, and a handler that
// accepts every input and answers each with emit's decisions.
func openAccountWith(t *testing.T, emit func(event.Envelope) []event.Envelope) (*fills.Simulator, replay.Handler) {
	t.Helper()
	simulator, _ := newComposed(t, baselineConfig())
	if err := simulator.OpenAccount(accountCash); err != nil {
		t.Fatal(err)
	}
	return simulator, replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
		if emit == nil {
			return nil, nil
		}
		return emit(in), nil
	})
}

// fillFor is a fill of instrumentID, of kind, for 100 shares at 10.
func fillFor(t *testing.T, instrumentID, kind string) event.Envelope {
	t.Helper()
	payload := event.FillPayload{
		InstrumentID: instrumentID, Kind: kind, FillID: "fill-" + instrumentID + "-" + kind,
		Direction: event.DirectionLong, Quantity: 100, Price: 10, FilledAt: day(1), Level: 10, SlippageApplied: 0.1, Commission: 1,
	}
	return envelope(t, payload.FillID, event.FillEventType, event.FillSchemaVersion, day(1), payload)
}

// delistingExit is the Campaign-exited decision a Delisting Exit of
// instrumentID records.
func delistingExit(t *testing.T, instrumentID string) event.Envelope {
	t.Helper()
	return envelope(t, "exited-"+instrumentID, event.CampaignExitedEventType, event.CampaignExitedSchemaVersion, day(1), event.CampaignExitedPayload{
		CampaignID: "campaign-" + instrumentID, InstrumentID: instrumentID, Reason: event.ExitReasonDelisting,
	})
}

func TestTheAccountRefusesWhatItCannotRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	malformed := func(eventType string, version uint32) event.Envelope {
		return envelope(t, "malformed-"+eventType, eventType, version, day(1), json.RawMessage(`"not an object"`))
	}
	unknownKind := fillFor(t, "MSFT", "short")
	for _, tt := range []struct {
		name  string
		input event.Envelope
		emit  func(event.Envelope) []event.Envelope
		first event.Envelope
		want  string
	}{
		{name: "an undecodable fill", input: malformed(event.FillEventType, event.FillSchemaVersion), want: "decode"},
		{name: "an undecodable cash movement", input: malformed(event.CashMovementEventType, event.CashMovementSchemaVersion), want: "decode"},
		{name: "a fill of an unknown kind", input: unknownKind, want: "cannot record"},
		{name: "a sale of shares not held", input: fillFor(t, "MSFT", event.FillKindStop), want: "holds 0"},
		{
			name:  "a delisting of a holding with no price",
			first: fillFor(t, "MSFT", event.FillKindEntry),
			input: envelope(t, "delisting-MSFT", event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, day(1), event.CorporateActionPayload{
				InstrumentID: "MSFT", Kind: event.CorporateActionKindDelisting, EffectiveAt: day(1),
			}),
			emit: func(in event.Envelope) []event.Envelope {
				if in.Type == event.MarketCorporateActionEventType {
					return []event.Envelope{delistingExit(t, "MSFT")}
				}
				return nil
			},
			want: "no close to settle",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			simulator, handler := openAccountWith(t, tt.emit)
			if tt.first.ID != "" {
				if _, err := fills.Deliver(ctx, simulator, handler, tt.first); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fills.Deliver(ctx, simulator, handler, tt.input); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Deliver() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestADelistingOfNothingHeldMovesNothing: a Delisting Exit the account
// holds no shares for settles nothing.
func TestADelistingOfNothingHeldMovesNothing(t *testing.T) {
	t.Parallel()

	simulator, handler := openAccountWith(t, func(in event.Envelope) []event.Envelope {
		return []event.Envelope{delistingExit(t, "MSFT")}
	})
	action := envelope(t, "delisting-MSFT", event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, day(1), event.CorporateActionPayload{
		InstrumentID: "MSFT", Kind: event.CorporateActionKindDelisting, EffectiveAt: day(1),
	})
	if _, err := fills.Deliver(context.Background(), simulator, handler, action); err != nil {
		t.Fatal(err)
	}
	if got := simulator.Account(); got.Cash != accountCash || len(got.Holdings) != 0 {
		t.Fatalf("account = %+v, want it unmoved", got)
	}
}

// TestAHoldingWithNoPriceCannotBeStated: equity marks every holding at its
// close, so a holding of an instrument the run has no bar for leaves the
// close unstatable, and the Session stops rather than state equity without
// it.
func TestAHoldingWithNoPriceCannotBeStated(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	simulator, handler := openAccountWith(t, nil)
	if _, err := fills.Deliver(ctx, simulator, handler, fillFor(t, "MSFT", event.FillKindEntry)); err != nil {
		t.Fatal(err)
	}
	if _, err := fills.RunBar(ctx, simulator, handler, barEnvelope(t, warmUpBars()[0])); err == nil || !strings.Contains(err.Error(), "no close to value") {
		t.Fatalf("RunBar() error = %v, want the unpriced holding refused", err)
	}
}

// TestAStatementTheReducerRefusesStopsTheRun: wherever the pending
// statement is delivered, inside the next Session or before a cash
// movement, a refusal stops there.
func TestAStatementTheReducerRefusesStopsTheRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	refusing := func() (*fills.Simulator, replay.Handler) {
		simulator, _ := newComposed(t, baselineConfig())
		if err := simulator.OpenAccount(accountCash); err != nil {
			t.Fatal(err)
		}
		return simulator, replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
			if in.Type == event.AccountSnapshotEventType {
				return nil, errors.New("statement refused")
			}
			return nil, nil
		})
	}

	simulator, handler := refusing()
	if _, err := fills.RunBar(ctx, simulator, handler, barEnvelope(t, warmUpBars()[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := fills.RunBar(ctx, simulator, handler, barEnvelope(t, warmUpBars()[1])); err == nil || !strings.Contains(err.Error(), "statement refused") {
		t.Fatalf("RunBar() error = %v, want the refused statement", err)
	}

	simulator, handler = refusing()
	if _, err := fills.RunBar(ctx, simulator, handler, barEnvelope(t, warmUpBars()[0])); err != nil {
		t.Fatal(err)
	}
	movement := envelope(t, "deposit-1", event.CashMovementEventType, event.CashMovementSchemaVersion, day(2), event.CashMovementPayload{
		AsOf: day(2), Amount: 1, EquityBefore: accountCash, Currency: "USD",
	})
	if _, err := fills.Deliver(ctx, simulator, handler, movement); err == nil || !strings.Contains(err.Error(), "statement refused") {
		t.Fatalf("Deliver(cash movement) error = %v, want the refused statement", err)
	}
}

// TestWithoutAnAccountNothingIsStated: a simulator that keeps no account
// reports none and states nothing, and StateLastClose needs both parties.
func TestWithoutAnAccountNothingIsStated(t *testing.T) {
	t.Parallel()

	simulator, reducer := newComposed(t, baselineConfig())
	if got := simulator.Account(); got.Cash != 0 || got.Holdings != nil {
		t.Fatalf("Account() = %+v, want the zero Account", got)
	}
	if result, err := fills.StateLastClose(context.Background(), simulator, reducer); err != nil || len(result.Inputs) != 0 {
		t.Fatalf("StateLastClose() = %s, %v; want nothing stated", describe(result.Inputs), err)
	}
	if _, err := fills.StateLastClose(context.Background(), nil, nil); err == nil {
		t.Fatal("StateLastClose() without a simulator and handler succeeded")
	}
}

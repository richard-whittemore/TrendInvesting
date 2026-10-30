package targetweight

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

// TestSnapshotScaleRefusalPreservesState checks the positions-snapshot seam's
// account-unit invariant without adopting a refused snapshot (ADR 0026 D7-F).
func TestSnapshotScaleRefusalPreservesState(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Execution.ShareQuantity = event.ShareQuantityFractional
	cfg.Mode = event.TradingModePaper
	cfg.AccountID = "DUfixture"
	at := time.Date(2025, 1, 2, 21, 0, 0, 0, time.UTC)
	makeHandler := func() *Handler {
		t.Helper()
		h, err := New("momentum/test", cfg, Options{DecideFrom: at})
		if err != nil {
			t.Fatal(err)
		}
		previous := event.AccountPositionsSnapshotPayload{
			AccountID: cfg.AccountID, AsOf: at, QuantityScale: event.FractionalQuantityScale,
			NetLiquidation: 1000, Currency: "USD",
			Positions: []event.AccountPosition{{InstrumentID: "AAA", Quantity: 123456}},
		}
		h.snapshot = &previous
		if err := h.gate.ObserveAccount(previous); err != nil {
			t.Fatal(err)
		}
		if err := h.gate.SetBook(reconcile.PositionsBook("previous", previous)); err != nil {
			t.Fatal(err)
		}
		return h
	}
	h, unchanged := makeHandler(), makeHandler()
	p := event.AccountPositionsSnapshotPayload{
		AccountID: cfg.AccountID, AsOf: at.Add(time.Minute), NetLiquidation: 2000, Currency: "USD",
		Positions: []event.AccountPosition{{InstrumentID: "AAA", Quantity: 7}},
	}
	if err := p.ValidateSchema(event.AccountSnapshotPositionsVersion); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	in := event.Envelope{ID: "wrong-scale", Type: event.AccountSnapshotEventType,
		SchemaVersion: event.AccountSnapshotPositionsVersion, EventTime: p.AsOf, Payload: payload}
	out, err := h.applySnapshot(in)
	if err == nil || !strings.Contains(err.Error(), "quantity scale 0 differs from account scale 10000") {
		t.Fatalf("want gate scale refusal, got %v", err)
	}
	if out != nil {
		t.Fatalf("refused snapshot emitted events: %+v", out)
	}
	if !reflect.DeepEqual(h, unchanged) {
		t.Fatal("refused snapshot changed handler or gate state")
	}
}

package event_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func TestFractionalConfiguration(t *testing.T) {
	p := validMomentumConfiguration()
	before := event.TargetWeightConfigurationHash(p)
	p.Execution.ShareQuantity = event.ShareQuantityFractional
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.SchemaVersion() != event.TargetWeightConfigurationFractionalVersion {
		t.Fatal("fractional configuration needs its own schema")
	}
	implicit := event.TargetWeightConfigurationHash(p)
	p.Execution.FractionalIncrement = "0.0001"
	if event.TargetWeightConfigurationHash(p) != implicit || implicit == before {
		t.Fatal("default increment must canonicalize, D7-F must change hash")
	}
	for _, increment := range []string{"0", "-0.1", "1.1", "0.00001", "0.3", "1e-4", "NaN"} {
		p.Execution.FractionalIncrement = increment
		if p.Validate() == nil {
			t.Errorf("accepted increment %q", increment)
		}
	}
	for _, increment := range []string{"", "0.0001", "0.0025", "0.25", "1"} {
		p.Execution.FractionalIncrement = increment
		if err := p.Validate(); err != nil {
			t.Errorf("%q: %v", increment, err)
		}
	}
	p.Execution.ShareQuantity = event.ShareQuantityMinimumLot
	if p.Validate() == nil {
		t.Fatal("whole-share rule accepted fractional increment")
	}
}

func TestFractionalQuantityPrecision(t *testing.T) {
	for value, want := range map[string]int64{"0": 0, "12.3456": 123456, "7.25": 72500, "3.5": 35000} {
		got, err := event.ParseQuantity(value)
		if err != nil || got != want {
			t.Fatalf("%s = %d, %v", value, got, err)
		}
	}
	for _, value := range []string{"-1", "12.34567", "1.00000", "1e2", "+1", " 1", "", "922337203685478"} {
		if _, err := event.ParseQuantity(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	if got := event.QuantityValue(72500, event.FractionalQuantityScale, 12); got != 87 {
		t.Fatalf("value %v", got)
	}
}

func TestFractionalSchemas(t *testing.T) {
	p := validAccountCashSnapshot()
	p.QuantityScale = event.FractionalQuantityScale
	p.Positions[0].Quantity = 123456
	if err := p.ValidateSchema(event.AccountSnapshotFractionalCashVersion); err != nil {
		t.Fatal(err)
	}
	if p.ValidateSchema(event.AccountSnapshotCashVersion) == nil {
		t.Fatal("old cash schema accepted ticks")
	}
	p.Positions[0].Quantity = -1
	if p.Validate() == nil {
		t.Fatal("fractional short accepted")
	}
	p.Positions[0].Quantity = 123456
	p.QuantityScale = 100000
	if p.Validate() == nil {
		t.Fatal("five decimals accepted")
	}
	f := validTargetWeightFill()
	f.Quantity, f.QuantityScale = 35000, event.FractionalQuantityScale
	if err := f.ValidateSchema(event.FillTargetWeightFractionalVersion); err != nil {
		t.Fatal(err)
	}
	if f.ValidateSchema(event.FillTargetWeightVersion) == nil {
		t.Fatal("old fill schema accepted ticks")
	}
	l := validLimitOrderLifecycle()
	l.Quantity, l.QuantityScale = 72500, event.FractionalQuantityScale
	if err := l.ValidateSchema(event.OrderLifecycleFractionalVersion); err != nil {
		t.Fatal(err)
	}
	if l.ValidateSchema(event.OrderLifecycleLimitVersion) == nil {
		t.Fatal("old lifecycle schema accepted ticks")
	}
	o := validOrderReleased()
	o.Quantity, o.QuantityScale = 72500, event.FractionalQuantityScale
	if err := o.ValidateSchema(event.OrderReleasedFractionalVersion); err != nil {
		t.Fatal(err)
	}
	if o.ValidateSchema(event.OrderReleasedSafetyVersion) == nil {
		t.Fatal("old release accepted ticks")
	}
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var decoded event.OrderReleasedPayload
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Quantity != 72500 || decoded.QuantityScale != 10000 {
		t.Fatalf("round trip %s: %v", raw, err)
	}
}

func TestFractionalEveryRecordDeclaresQuantityUnits(t *testing.T) {
	positions := validAccountPositionsSnapshot()
	positions.QuantityScale = event.FractionalQuantityScale
	broker := readTimeSnapshot()
	broker.QuantityScale = event.FractionalQuantityScale
	planned := validOrderPlanned()
	planned.QuantityScale, planned.Quantity, planned.FractionalIncrement = event.FractionalQuantityScale, 72500, "0.25"
	refused := validOrderRefused()
	refused.QuantityScale = event.FractionalQuantityScale
	decision := validMomentumDecided()
	decision.QuantityScale = event.FractionalQuantityScale
	reconciliation := validReconciliation()
	reconciliation.QuantityScale = event.FractionalQuantityScale
	result := validReconciliationResult()
	result.QuantityScale = event.FractionalQuantityScale
	retired := validRetired()
	retired.QuantityScale = event.FractionalQuantityScale
	skip := validOrderSkipped()
	skip.QuantityScale, skip.RequestedValue, skip.Shares, skip.Reason = event.FractionalQuantityScale, "0.001", 0, event.OrderSkippedBelowIncrement
	synthetic := validLimitOrderLifecycle()
	synthetic.QuantityScale, synthetic.Synthetic, synthetic.Status, synthetic.OrderID, synthetic.Message = event.FractionalQuantityScale, true, event.OrderLifecycleStatusCanceled, "", "dry-run: never sent"
	type versioned interface {
		ValidateSchema(uint32) error
		SchemaVersion() uint32
	}
	for _, p := range []versioned{positions, broker, planned, refused, decision, reconciliation, result, retired, skip, synthetic} {
		t.Run(fmt.Sprintf("%T", p), func(t *testing.T) {
			if err := p.ValidateSchema(p.SchemaVersion()); err != nil {
				t.Fatalf("valid fractional: %v", err)
			}
			if p.ValidateSchema(99) == nil {
				t.Fatal("unknown version accepted")
			}
			if p.ValidateSchema(1) == nil {
				t.Fatal("old version accepted fractional scale")
			}
		})
	}
	planned.Quantity = 72501
	if planned.Validate() == nil {
		t.Fatal("quantity off configured increment accepted")
	}
	broker.Positions[0].Quantity = -1
	if broker.Validate() == nil {
		t.Fatal("negative fractional broker quantity accepted")
	}
	reconciliation.Positions[0].Quantity = -1
	if reconciliation.Validate() == nil {
		t.Fatal("negative fractional reconciliation quantity accepted")
	}
	raw, err := json.Marshal(skip)
	if err != nil || bytes.Contains(raw, []byte(`"shares":`)) {
		t.Fatalf("fractional skip carries floating-point shares: %s %v", raw, err)
	}
	oldSkip := validOrderSkipped()
	oldSkip.Shares = 0
	raw, err = json.Marshal(oldSkip)
	if err != nil || !bytes.Contains(raw, []byte(`"shares":0`)) {
		t.Fatalf("legacy zero-share skip changed: %s %v", raw, err)
	}
}

func TestFractionalQuantityWireNeverRounds(t *testing.T) {
	for _, raw := range []string{`{"quantity":123456.7}`, `{"quantity":"12.34567"}`, `{"quantity":9223372036854775808}`} {
		var p event.TargetWeightFillPayload
		if json.Unmarshal([]byte(raw), &p) == nil {
			t.Errorf("accepted noninteger or overflowing tick count %s", raw)
		}
	}
	for q, want := range map[int64]string{0: "0", 1: "0.0001", 35000: "3.5", 72500: "7.25", 123456: "12.3456", -37500: "-3.75"} {
		if got := event.FormatQuantity(q, event.FractionalQuantityScale); got != want {
			t.Errorf("%d formats %q, want %q", q, got, want)
		}
	}
	if event.FormatQuantity(7, 0) != "7" {
		t.Fatal("whole-share format changed")
	}
	if event.QuantityValue(7, 0, 12.5) != 87.5 {
		t.Fatal("whole-share valuation changed")
	}
	for _, scale := range []int64{-1, 1, 100, 100000} {
		p := validTargetWeightFill()
		p.QuantityScale = scale
		if p.Validate() == nil {
			t.Errorf("accepted quantity scale %d", scale)
		}
	}
}

// ADR 0026 keeps D7-L's minimum-lot substitution separate from D7-F.
func TestFractionalDecisionRefusesMinimumLotEvidence(t *testing.T) {
	for _, status := range []string{"", event.MomentumStatusUnaffordable, event.MomentumStatusHeldOneLotShort} {
		t.Run(status, func(t *testing.T) {
			p := validMomentumDecided()
			p.Momentum.MinimumLot = event.MinimumLotFigures{NetLiquidation: 30000, MomentumScale: 1}
			switch status {
			case event.MomentumStatusUnaffordable:
				p.Momentum.Names[1] = event.MomentumName{InstrumentID: "BBB", UniverseRank: 2, DollarVolume: 4e8, Status: status, Closes: 300, Score: 0.2, Volatility: 0.03, ScoreRank: 2, OneShare: 900, TargetValue: 800}
			case event.MomentumStatusHeldOneLotShort:
				n := &p.Momentum.Names[0]
				n.Status, n.OneShare, n.TargetValue = status, 20000, 14700
				n.HeldQuantity, n.HeldValue, n.HeldShare = 2, 40000, 40000.0/30000
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("valid legacy evidence: %v", err)
			}
			p.QuantityScale = event.FractionalQuantityScale
			if p.Validate() == nil {
				t.Fatal("D7-F accepted D7-L minimum-lot evidence")
			}
		})
	}
}

// ADR 0026 D7-F is an IRA rule: quantities cannot be negative and the
// margin-account sale back to 98% is not a fractional IRA order.
func TestFractionalRiskEvidenceRefusesShortsAndMarginSale(t *testing.T) {
	released := validOrderReleased()
	released.QuantityScale = event.FractionalQuantityScale
	released.Figures.BrokerSet, released.Figures.BrokerHolding = true, -1
	if released.Validate() == nil {
		t.Error("fractional release accepted a negative broker holding")
	}
	refused := validOrderRefused()
	refused.QuantityScale = event.FractionalQuantityScale
	refused.Figures.BrokerSet, refused.Figures.BrokerHolding = true, -1
	if refused.Validate() == nil {
		t.Error("fractional refusal accepted a negative broker holding")
	}
	planned := validSaleOrder()
	if err := planned.Validate(); err != nil {
		t.Fatalf("legacy sale: %v", err)
	}
	planned.QuantityScale = event.FractionalQuantityScale
	if planned.Validate() == nil {
		t.Error("fractional plan accepted margin-only sale figures")
	}
}

// TestFractionalOrderReportsTracksExactOutstanding implements ADR 0026 D7-F:
// a synthetic 3.5-share fill of 7.25 leaves exactly 3.75 shares outstanding.
func TestFractionalOrderReportsTracksExactOutstanding(t *testing.T) {
	at := time.Date(2025, 2, 3, 21, 0, 0, 0, time.UTC)
	release := event.OrderReleasedPayload{AccountID: "ira", ClientOrderID: "synthetic-sale", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 72500, QuantityScale: 10000, LimitPrice: 10}
	fill := event.TargetWeightFillPayload{AccountID: "ira", ClientOrderID: release.ClientOrderID, FillID: "synthetic-fill", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 35000, QuantityScale: 10000, Price: 10, FilledAt: at}
	book := event.NewOrderReports()
	book.Release(release)
	wrong := fill
	wrong.QuantityScale = 0
	if _, _, err := book.Fill(wrong); err == nil {
		t.Fatal("accepted whole-share fill on fractional release")
	}
	for _, quantity := range []int64{0, -1} {
		wrong = fill
		wrong.Quantity = quantity
		if _, _, err := book.Fill(wrong); err == nil {
			t.Fatalf("accepted nonpositive quantity %d", quantity)
		}
	}
	order, duplicate, err := book.Fill(fill)
	if err != nil || duplicate || order.Terminal || order.Released.Quantity-order.Filled != 37500 {
		t.Fatalf("partial fill: %+v %v %v", order, duplicate, err)
	}
	if _, duplicate, err = book.Fill(fill); err != nil || !duplicate {
		t.Fatalf("redelivery: %v %v", duplicate, err)
	}
	lifecycle := event.LimitOrderLifecyclePayload{AccountID: "ira", ClientOrderID: release.ClientOrderID, OrderID: "synthetic-order", InstrumentID: "AAA", Side: event.OrderSideSell, Status: event.OrderLifecycleStatusCanceled, Quantity: 72500, QuantityScale: 0, LimitPrice: 10, OccurredAt: at}
	if _, err := book.Lifecycle(lifecycle); err == nil {
		t.Fatal("accepted whole-share lifecycle on fractional release")
	}
	lifecycle.QuantityScale = 10000
	order, err = book.Lifecycle(lifecycle)
	if err != nil || !order.Terminal || order.Released.Quantity-order.Filled != 37500 {
		t.Fatalf("terminal remainder: %+v %v", order, err)
	}
}

// Schema migration preserves every legacy shape and rejects a different unit
// system even when its integer payload is otherwise valid (ADR 0015).
func checkQuantitySchemaRoundTrip[T interface {
	SchemaVersion() uint32
	ValidateSchema(uint32) error
}](t *testing.T, original T, legacy, fractional uint32) {
	t.Helper()
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, scale := range []int64{0, event.FractionalQuantityScale} {
		fields["quantity_scale"] = json.RawMessage(strconv.FormatInt(scale, 10))
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		var decoded T
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		want, other := legacy, fractional
		if scale != 0 {
			want, other = fractional, legacy
		}
		if got := decoded.SchemaVersion(); got != want {
			t.Fatalf("%T scale%d schema%d, want%d", decoded, scale, got, want)
		}
		if err := decoded.ValidateSchema(want); err != nil {
			t.Fatalf("%T scale%d: %v", decoded, scale, err)
		}
		if decoded.ValidateSchema(other) == nil {
			t.Fatalf("%T accepted mismatched schema%d", decoded, other)
		}
		if decoded.ValidateSchema(99) == nil {
			t.Fatalf("%T accepted unknown schema", decoded)
		}
	}
	fields["quantity_scale"] = json.RawMessage("100000")
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var invalid T
	if err := json.Unmarshal(encoded, &invalid); err != nil {
		t.Fatal(err)
	}
	if invalid.ValidateSchema(fractional) == nil {
		t.Fatalf("%T accepted five-place quantity scale", invalid)
	}
}

func TestFractionalLegacyAndNewSchemaRoundTrips(t *testing.T) {
	checkQuantitySchemaRoundTrip(t, validAccountPositionsSnapshot(), 3, 5)
	checkQuantitySchemaRoundTrip(t, validAccountCashSnapshot(), 4, 6)
	checkQuantitySchemaRoundTrip(t, validTargetWeightFill(), 5, 6)
	checkQuantitySchemaRoundTrip(t, validLimitOrderLifecycle(), 2, 4)
	checkQuantitySchemaRoundTrip(t, validOrderPlanned(), 1, 3)
	checkQuantitySchemaRoundTrip(t, validOrderReleased(), 2, 4)
	checkQuantitySchemaRoundTrip(t, validOrderRefused(), 2, 5)
	checkQuantitySchemaRoundTrip(t, validMomentumDecided(), 10, 11)
	checkQuantitySchemaRoundTrip(t, readTimeSnapshot(), 2, 3)
	checkQuantitySchemaRoundTrip(t, validReconciliation(), 1, 2)
	checkQuantitySchemaRoundTrip(t, validReconciliationResult(), 1, 2)
	checkQuantitySchemaRoundTrip(t, validRetired(), 1, 2)
	if got := validBrokerSnapshot().SchemaVersion(); got != 1 {
		t.Fatalf("legacy broker schema%d", got)
	}
	if err := validBrokerSnapshot().ValidateSchema(99); err == nil {
		t.Fatal("unknown broker schema accepted")
	}
	if got := validOrderSkipped().SchemaVersion(); got != 1 {
		t.Fatalf("legacy skip schema%d", got)
	}
}

func TestFractionalReaderVersionListsAreIndependent(t *testing.T) {
	for name, entry := range map[string]struct {
		read func() []uint32
		want []uint32
	}{
		"configuration":         {event.TargetWeightConfigurationVersions, []uint32{7, 8}},
		"positions":             {event.AccountPositionsSnapshotVersions, []uint32{3, 5}},
		"cash":                  {event.AccountCashSnapshotVersions, []uint32{4, 6}},
		"fill":                  {event.TargetWeightFillVersions, []uint32{5, 6}},
		"lifecycle inputs":      {event.LimitOrderLifecycleInputVersions, []uint32{2, 4}},
		"retirement":            {event.OrderRetiredVersions, []uint32{1, 2}},
		"reconciliation":        {event.ReconciliationVersions, []uint32{1, 2}},
		"reconciliation result": {event.ReconciliationResultVersions, []uint32{1, 2}},
		"skips":                 {event.OrderSkippedVersions, []uint32{1, 2}},
	} {
		t.Run(name, func(t *testing.T) {
			got := entry.read()
			if !slices.Equal(got, entry.want) {
				t.Fatalf("versions%v, want%v", got, entry.want)
			}
			got[0] = 99
			if !slices.Equal(entry.read(), entry.want) {
				t.Fatal("caller changed shared reader versions")
			}
		})
	}
}

func TestFractionalConfigurationDeclaresExecutionUnits(t *testing.T) {
	for _, fractional := range []bool{false, true} {
		p := validMomentumConfiguration()
		schema, scale, step := uint32(7), int64(0), int64(1)
		if fractional {
			p.Execution.ShareQuantity = event.ShareQuantityFractional
			p.Execution.FractionalIncrement = "0.25"
			schema, scale, step = 8, 10000, 2500
		}
		if err := p.ValidateSchema(schema); err != nil {
			t.Fatal(err)
		}
		if p.ValidateSchema(99) == nil {
			t.Fatal("unknown configuration schema accepted")
		}
		if p.Execution.QuantityScale() != scale {
			t.Fatal("wrong execution units")
		}
		got, err := p.Execution.IncrementTicks()
		if err != nil || got != step {
			t.Fatalf("increment%d %v", got, err)
		}
	}
	if event.QuantityUnits(0) != 1 || event.QuantityUnits(10000) != 10000 {
		t.Fatal("wrong raw share units")
	}
	if _, err := event.ParseQuantity("1."); err == nil {
		t.Fatal("empty decimal fraction accepted")
	}
	for _, price := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if !math.IsNaN(event.QuantityValue(1, 10000, price)) {
			t.Fatal("nonfinite price produced a usable valuation")
		}
	}
}

func TestFractionalSkipRejectsAmbiguousMonetaryEvidence(t *testing.T) {
	base := validOrderSkipped()
	base.QuantityScale, base.Shares, base.Reason, base.RequestedValue = 10000, 0, event.OrderSkippedBelowIncrement, "0.001"
	for name, mutate := range map[string]func(*event.OrderSkippedPayload){
		"invalid increment": func(p *event.OrderSkippedPayload) { p.FractionalIncrement = "0.3" },
		"negative value":    func(p *event.OrderSkippedPayload) { p.RequestedValue = "-1" },
		"rational fraction": func(p *event.OrderSkippedPayload) { p.RequestedValue = "1/3" },
		"no value":          func(p *event.OrderSkippedPayload) { p.RequestedValue = "" },
		"floating shares":   func(p *event.OrderSkippedPayload) { p.Shares = 0.1 },
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			if p.Validate() == nil {
				t.Fatal("accepted ambiguous fractional evidence")
			}
		})
	}
	planned := validOrderPlanned()
	planned.QuantityScale, planned.FractionalIncrement = 10000, "0.3"
	if planned.Validate() == nil {
		t.Fatal("planned fractional quantity accepted an invalid increment")
	}
}

func TestFractionalRequestedValueRequiresPlainDecimal(t *testing.T) {
	p := validOrderSkipped()
	p.QuantityScale, p.Shares, p.Reason = 10000, 0, event.OrderSkippedBelowIncrement
	for _, value := range []string{"0x10", "0b10", "1_000", "+1", "1e2", "1/2", "1..2", ".", "-"} {
		t.Run(value, func(t *testing.T) {
			p.RequestedValue = value
			if err := p.ValidateSchema(p.SchemaVersion()); err == nil {
				t.Fatalf("accepted %q", value)
			}
		})
	}
	for _, value := range []string{"0", "0.001", "123.4500", "-0"} {
		p.RequestedValue = value
		if err := p.ValidateSchema(p.SchemaVersion()); err != nil {
			t.Fatalf("rejected %q: %v", value, err)
		}
	}
}

// ADR 0026, "Worst-case cost" and D5: preserve exact cents, charge buys up,
// and credit sales down before applying fees.
func TestConservativeExecutionCents(t *testing.T) {
	for _, tc := range []struct {
		name            string
		quantity, scale int64
		price, fees     float64
		buy             string
		sale            float64
	}{
		{"half cent", 72500, 10000, 10.10, 0.01, "73.24", 73.21},
		{"above half cent", 72500, 10000, 10.101, 0.01, "73.25", 73.22},
		{"below cent", 1, 10000, 0.01, 0, "0.01", 0},
		{"whole exact cents", 3, 0, 0.07, 0, "0.21", 0.21},
		{"fractional exact cents", 2900, 10000, 1, 0, "0.29", 0.29},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := event.BuyCost(tc.quantity, tc.scale, tc.price, tc.fees).FloatString(2); got != tc.buy {
				t.Fatalf("buy %s, want %s", got, tc.buy)
			}
			if got := event.SaleProceeds(tc.quantity, tc.scale, tc.price, tc.fees); got != tc.sale {
				t.Fatalf("sale %v, want %v", got, tc.sale)
			}
		})
	}
}

package targetweight_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// The owner's decision of 2026-10-18 (ADR 0019, ADR 0026 point 1, amended):
// the IBKR adapter's one reading after the close, with nothing working at
// IBKR and no execution since, is the account as of that close. Its broker
// snapshot (version 2) is stamped as of the close with the read time beside
// it, and the mark (version 2) is the delivered bar's official close with
// IBKR's last price beside it as evidence. A Boost run fed so decides
// exactly as one fed snapshots and marks taken at the close when IBKR's last
// price is the close; the only difference recorded is which price the gate's
// price band compared the order with (mark_source).
func TestAPostCloseReadingDecidesAsAReadingAtTheClose(t *testing.T) {
	atClose := simulate(t, options{brokers: true})
	postClose := simulate(t, options{brokers: true, postClose: true, brokerLast: 1})
	if len(atClose.decisions) == 0 || len(postClose.decisions) != len(atClose.decisions) {
		t.Fatalf("decisions: %d post-close, %d at the close", len(postClose.decisions), len(atClose.decisions))
	}
	released := 0
	for i, want := range atClose.decisions {
		got := postClose.decisions[i]
		got.Payload = bytes.ReplaceAll(got.Payload, []byte(`,"mark_source":"broker-last"`), nil)
		if got.ID != want.ID || got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("decision %d differs:\n post-close %s %s %s\n at close   %s %s %s", i, got.ID, got.Type, got.Payload, want.ID, want.Type, want.Payload)
		}
		if got.Type == event.OrderReleasedEventType {
			released++
		}
	}
	if released == 0 {
		t.Fatal("setup: the run releases no order, so the stamp is never checked")
	}
	if refused := ofType(postClose.decisions, event.OrderRefusedEventType); len(refused) != 0 {
		t.Errorf("a post-close reading refused %d orders with IBKR's last price at the close", len(refused))
	}
}

// TestThePriceBandComparesIBKRsLastPriceWhenIBKRGivesOne: the gate's price
// band compares the order's reference price (the previous official close)
// with IBKR's last price when the mark carries one, and with the official
// close otherwise, and the order's figures record which (ADR 0026 point 3,
// amended 2026-10-18). With IBKR's last price twice the close every order
// the band checks is refused; with none, the band compares the close with
// itself and refuses nothing.
func TestThePriceBandComparesIBKRsLastPriceWhenIBKRGivesOne(t *testing.T) {
	far := simulate(t, options{brokers: true, postClose: true, brokerLast: 2})
	refused := ofType(far.decisions, event.OrderRefusedEventType)
	if len(refused) == 0 {
		t.Fatal("IBKR's last price at twice the close refused nothing: the band compared the close with itself")
	}
	// The first refusal is the band's; it degrades the account, which
	// refuses what follows for that reason.
	var p event.OrderRefusedPayload
	decode(t, refused[0], &p)
	if p.Reason != event.RefusedPriceBand || p.Figures.MarkSource != event.MarkSourceBrokerLast || p.Figures.Mark != 2*p.Figures.ReferencePrice {
		t.Fatalf("refused %s: %s %+v, want price-band against IBKR's last price", refused[0].ID, p.Reason, p.Figures)
	}
	none := simulate(t, options{brokers: true, postClose: true})
	released := ofType(none.decisions, event.OrderReleasedEventType)
	if len(released) == 0 || len(ofType(none.decisions, event.OrderRefusedEventType)) != 0 {
		t.Fatalf("without IBKR's last price: %d released, %d refused", len(released), len(ofType(none.decisions, event.OrderRefusedEventType)))
	}
	for _, e := range released {
		var p event.OrderReleasedPayload
		decode(t, e, &p)
		if !p.Figures.RiskReducing && p.Figures.MarkSource != event.MarkSourceOfficialClose {
			t.Errorf("released %s: mark source %q, want %q", e.ID, p.Figures.MarkSource, event.MarkSourceOfficialClose)
		}
	}
}

// TestAReadingStampedAtItsReadTimeWithholds: when the reading cannot stand
// for the close (an order still working, or an execution since), the adapter
// stamps its true read time, and every session it serves is withheld as
// stale-account-snapshot: no order is planned or released.
func TestAReadingStampedAtItsReadTimeWithholds(t *testing.T) {
	r := simulate(t, options{brokers: true, readTime: true})
	decided := targets(t, r.decisions)
	if len(decided) == 0 {
		t.Fatal("no decisions")
	}
	for i, d := range decided {
		if d.Outcome != event.TargetsOutcomeWithheld || d.WithheldReason != event.WithheldStaleAccountSnapshot {
			t.Fatalf("session %d: %s %s, want withheld %s", i, d.Outcome, d.WithheldReason, event.WithheldStaleAccountSnapshot)
		}
	}
	if got := ofType(r.decisions, event.OrderReleasedEventType); len(got) != 0 {
		t.Errorf("released %d orders from readings that do not stand for the close", len(got))
	}
}

// TestBrokerSnapshotsAndMarksAreReadAtEveryVersionWritten: a version the
// payloads do not define is refused (ADR 0015).
func TestBrokerSnapshotsAndMarksAreReadAtEveryVersionWritten(t *testing.T) {
	h, s := configured(t)
	at := sessionEnd(0)
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, 4, "m", at, event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: 100, Source: "marketdata"}), "schema version")
	mustRefuse(t, h, s.envelope(t, event.BrokerSnapshotEventType, 3, "b", at, event.BrokerSnapshotPayload{}), "schema version")
	evidence := event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: 100, Source: "marketdata", BrokerLast: 101, BrokerLastSet: true}
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, evidence), "version 1")
	mustApply(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedEvidenceVersion, "m", at, evidence))
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedContractVersion, "m3", at.Add(time.Second), evidence), "contract")
	evidence.AsOf = at.Add(time.Second)
	evidence.Contract = &event.MarkContract{ConID: 756733, Exchange: "SMART", PrimaryExchange: "ARCA", Currency: "USD"}
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedEvidenceVersion, "m2c", at.Add(time.Second), evidence), "version 3")
	mustApply(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedContractVersion, "m3", at.Add(time.Second), evidence))
}

// TestAMarkWithItsContractIsBandedAsAVersion2Mark: the IRA's adapter writes
// each mark at version 3, version 2 with the IBKR contract it resolved for
// the instrument (ADR 0026 point 3), so a stock newly picked has a mark and a
// contract before any order is planned. The price band reads it as version
// 2: against IBKR's last price beside it when set, so the run decides as with
// version-2 marks, and a last price at twice the mark refuses a checked buy.
func TestAMarkWithItsContractIsBandedAsAVersion2Mark(t *testing.T) {
	v2 := simulateMomentum(t, momentumOptions{brokerLast: 1})
	v3 := simulateMomentum(t, momentumOptions{brokerLast: 1, contracts: true})
	if len(released(t, v3.decisions)) == 0 || len(released(t, v3.decisions)) != len(released(t, v2.decisions)) {
		t.Fatalf("released: %d with contract marks, %d with version-2 marks", len(released(t, v3.decisions)), len(released(t, v2.decisions)))
	}
	for _, p := range released(t, v3.decisions) {
		if !p.Figures.MarkSet || p.Figures.MarkSource != event.MarkSourceBrokerLast {
			t.Errorf("released %s: figures %+v, want the band against IBKR's last price", p.ClientOrderID, p.Figures)
		}
	}
	far := simulateMomentum(t, momentumOptions{brokerLast: 2, contracts: true})
	band := 0
	for _, e := range ofType(far.decisions, event.OrderRefusedEventType) {
		var p event.OrderRefusedPayload
		decode(t, e, &p)
		if p.Reason == event.RefusedPriceBand {
			band++
		}
	}
	if band == 0 {
		t.Fatal("IBKR's last price at twice a version-3 mark refused no momentum order")
	}
}

// contractID is an invented IBKR contract ID for a fixture instrument.
func contractID(instrument string) int64 {
	var id int64 = 900000
	for _, r := range instrument {
		id = id*31 + int64(r)
	}
	return id%1_000_000_000 + 1
}

// TestAMomentumOrdersPriceBandComparesIBKRsLastPriceToo: the band's rule is
// one rule for every checked order (ADR 0026 point 3, amended 2026-10-18):
// a momentum order, like Boost's, is compared with IBKR's last price beside
// a version-2 mark, and its figures say so. With IBKR's last price at the
// mark the run decides as with version-1 marks, the source recorded; with it
// at twice the mark a checked buy is refused by the band.
func TestAMomentumOrdersPriceBandComparesIBKRsLastPriceToo(t *testing.T) {
	plain := simulateMomentum(t, momentumOptions{})
	same := simulateMomentum(t, momentumOptions{brokerLast: 1})
	if len(released(t, plain.decisions)) == 0 || len(released(t, same.decisions)) != len(released(t, plain.decisions)) {
		t.Fatalf("released: %d with IBKR's last at the mark, %d with version-1 marks", len(released(t, same.decisions)), len(released(t, plain.decisions)))
	}
	for _, p := range released(t, same.decisions) {
		if p.Figures.MarkSet && p.Figures.MarkSource != event.MarkSourceBrokerLast {
			t.Errorf("released %s: mark source %q, want %q", p.ClientOrderID, p.Figures.MarkSource, event.MarkSourceBrokerLast)
		}
	}
	far := simulateMomentum(t, momentumOptions{brokerLast: 2})
	band := 0
	for _, e := range ofType(far.decisions, event.OrderRefusedEventType) {
		var p event.OrderRefusedPayload
		decode(t, e, &p)
		if p.Reason == event.RefusedPriceBand {
			band++
			if p.Figures.MarkSource != event.MarkSourceBrokerLast {
				t.Errorf("refused %s: figures %+v, want the band against IBKR's last price", p.ClientOrderID, p.Figures)
			}
		}
	}
	if band == 0 {
		t.Fatal("IBKR's last price at twice the mark refused no momentum order: the band compared the close with itself")
	}
}

package targetweight_test

import (
	"bytes"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// The owner's decision of 2026-10-18 (ADR 0019, ADR 0026 point 1, amended):
// the IBKR adapter's one reading after the close, with nothing working at
// IBKR and no execution since, is the account as of that close. Its broker
// snapshot (version 2) is stamped as of the close with the read time beside
// it, and the mark (version 2) is the delivered bar's official close with
// IBKR's last price beside it as evidence. A Boost run fed so decides
// exactly as one fed snapshots and marks taken at the close; IBKR's last
// price is never decided from, however far it is from the close.
func TestAPostCloseReadingDecidesAsAReadingAtTheClose(t *testing.T) {
	atClose := simulate(t, options{brokers: true})
	postClose := simulate(t, options{brokers: true, postClose: true, brokerLast: 2})
	if len(atClose.decisions) == 0 || len(postClose.decisions) != len(atClose.decisions) {
		t.Fatalf("decisions: %d post-close, %d at the close", len(postClose.decisions), len(atClose.decisions))
	}
	released := 0
	for i, want := range atClose.decisions {
		got := postClose.decisions[i]
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
		t.Errorf("a post-close reading refused %d orders (IBKR's last price, twice the close, decided from?)", len(refused))
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
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, 3, "m", at, event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: 100, Source: "marketdata"}), "schema version")
	mustRefuse(t, h, s.envelope(t, event.BrokerSnapshotEventType, 3, "b", at, event.BrokerSnapshotPayload{}), "schema version")
	evidence := event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: 100, Source: "marketdata", BrokerLast: 101, BrokerLastSet: true}
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, evidence), "version 1")
	mustApply(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedEvidenceVersion, "m", at, evidence))
}

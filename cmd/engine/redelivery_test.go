package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// paperBoostOptions is a paper Boost invocation writing into dir, whose
// released orders are marked to be sent: an account's first run, approved
// with its starting book, so the adapter's first input follows the approval
// at paperFirstInput.
func paperBoostOptions(t *testing.T, dir string) (options, event.TargetWeightConfigurationPayload) {
	t.Helper()
	opts := boostOptions(t, dir)
	opts.mode, opts.configPath = modePaper, writeBoostConfiguration(t, dir, modePaper, "DU1234567")
	opts.approvalPath = writeFirstRunApproval(t, "DU1234567")
	return opts, boostConfiguration(t, opts.configPath)
}

// paperFirstInput is the sequence of the adapter's first input to a first
// paper run: after its configuration and its approval.
const paperFirstInput = approvalSequence + 1

// feedUntilReleased feeds sessions until the engine releases an order, so
// the feed's last input is the session close that released it.
func feedUntilReleased(t *testing.T, feed *boostFeed) int {
	t.Helper()
	for i := 0; i < 60; i++ {
		feed.sessions(i, i+1)
		if len(feed.released) > 0 {
			return i
		}
	}
	t.Fatal("no order was released in 60 sessions")
	return 0
}

func wireBytes(t *testing.T, e event.Envelope) []byte {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func logHoldsInput(recovery journal.Recovery, id string) bool {
	for _, e := range recovery.Entries {
		if e.Kind == journal.KindInput && e.Envelope.ID == id {
			return true
		}
	}
	return false
}

func countRecords(recovery journal.Recovery, kind, eventType string) int {
	n := 0
	for _, e := range recovery.Entries {
		if e.Kind == kind && e.Envelope.Type == eventType {
			n++
		}
	}
	return n
}

// TestAnInputSentAgainIsAnsweredWithItsCommittedReply: the adapter asks for
// the committed reply to an input by sending that input again, under its
// ID. It receives the reply already recorded, byte for byte, and the input
// is not applied a second time, so it never yields a second order (ADR
// 0027, section 4: durable means possibly sent).
func TestAnInputSentAgainIsAnsweredWithItsCommittedReply(t *testing.T) {
	dir := t.TempDir()
	opts, cfg := paperBoostOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, cfg, "test-build", paperFirstInput)
	feedUntilReleased(t, feed)

	again, err := feed.client.Decide(context.Background(), feed.last)
	if err != nil {
		t.Fatalf("ask again for the committed reply to %s: %v", feed.last.ID, err)
	}
	if want, got := wireBytes(t, feed.lastReply), wireBytes(t, again); !bytes.Equal(want, got) {
		t.Fatalf("the reply served again differs from the one recorded:\n first %s\n again %s", want, got)
	}
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the engine: %v", err)
	}

	recovery := recoverFile(t, opts.logPath)
	if got := countRecords(recovery, journal.KindDecision, event.OrderReleasedEventType); got != len(feed.released) {
		t.Errorf("the log holds %d released orders, want the %d first served", got, len(feed.released))
	}
	if got := countRecords(recovery, journal.KindInput, event.SessionClosedEventType); got != int(feed.sequence-2)/4 {
		t.Errorf("the log holds %d session closes, want %d: the input sent again is never applied again", got, (feed.sequence-2)/4)
	}

}

// TestAnotherInputUnderACommittedIDIsRefused: an input ID names one input,
// so a different input sent under a committed input's ID is refused, is
// never answered with the committed reply, and leaves nothing in the log.
func TestAnotherInputUnderACommittedIDIsRefused(t *testing.T) {
	dir := t.TempDir()
	opts, cfg := paperBoostOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, cfg, "test-build", paperFirstInput)
	feedUntilReleased(t, feed)

	other := feed.last
	other.Sequence = feed.sequence
	if _, err := feed.client.Decide(context.Background(), other); err == nil || !strings.Contains(err.Error(), "committed with different content") {
		t.Fatalf("a different input under committed ID %s: %v, want a refusal", other.ID, err)
	}
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the engine: %v", err)
	}
	recovery := recoverFile(t, opts.logPath)
	if last := recovery.Entries[len(recovery.Entries)-1].Envelope; last.Sequence == other.Sequence && last.ID == other.ID {
		t.Fatalf("the refused input was recorded")
	}
	if got := countRecords(recovery, journal.KindInput, event.SessionClosedEventType); got != int(feed.sequence-2)/4 {
		t.Errorf("the log holds %d session closes, want %d", got, (feed.sequence-2)/4)
	}
}

// TestAResumedEngineServesACommittedButUnsentOrderAgain: a crash after an
// input's group is flushed and before its reply reaches the adapter leaves
// a released order on disk that was never sent (ADR 0027, section 4). The
// engine resumed from that log answers the adapter's request for that
// input's reply with the reply recorded before the crash, byte for byte,
// order included, and records no second order.
func TestAResumedEngineServesACommittedButUnsentOrderAgain(t *testing.T) {
	dir := t.TempDir()
	first, cfg := paperBoostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, cfg, "test-build", paperFirstInput)
	session := feedUntilReleased(t, feed)
	// The adapter never acts on this reply: the engine stops before it is
	// sent, as far as the adapter knows.
	unsent, unsentReply := feed.last, feed.lastReply
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the first run: %v", err)
	}

	resumed := first
	resumed.logPath, resumed.outPath, resumed.resumePath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal"), first.logPath
	resumed.approvalPath = "" // a crash resumes from its log, its approval included
	stop = startEngine(t, resumed)
	after := newBoostFeed(t, resumed.socketPath, cfg, "test-build", feed.sequence)
	again, err := after.client.Decide(context.Background(), unsent)
	if err != nil {
		t.Fatalf("ask the resumed engine for the committed reply to %s: %v", unsent.ID, err)
	}
	if want, got := wireBytes(t, unsentReply), wireBytes(t, again); !bytes.Equal(want, got) {
		t.Fatalf("the resumed engine served a different reply:\n before %s\n after  %s", want, got)
	}
	var bundle decisionsPayload
	if err := json.Unmarshal(again.Payload, &bundle); err != nil {
		t.Fatal(err)
	}
	sendable := 0
	for _, d := range bundle.Decisions {
		var p event.OrderReleasedPayload
		if d.Type == event.OrderReleasedEventType && json.Unmarshal(d.Payload, &p) == nil && p.Send {
			sendable++
		}
	}
	if sendable != 1 {
		t.Fatalf("the reply served again carries %d orders to send, want the one released before the crash", sendable)
	}
	// The adapter's next input continues the recovered sequence.
	after.sessions(session+1, session+2)
	_ = after.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the resumed run: %v", err)
	}
	before, now := recoverFile(t, first.logPath), recoverFile(t, resumed.logPath)
	if got, want := countRecords(now, journal.KindDecision, event.OrderReleasedEventType), countRecords(before, journal.KindDecision, event.OrderReleasedEventType); got != want {
		t.Errorf("the resumed log holds %d released orders, want the %d recorded before the crash", got, want)
	}
	if got := countRecords(now, journal.KindInput, event.SessionClosedEventType); got != session+2 {
		t.Errorf("the resumed log holds %d session closes, want %d: the reply served again records nothing", got, session+2)
	}
}

// lifecycle builds the venue's report of a released order's new status.
func lifecycle(o event.OrderReleasedPayload, status string, at time.Time) event.LimitOrderLifecyclePayload {
	return event.LimitOrderLifecyclePayload{
		AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, OrderID: "ib-" + o.ClientOrderID, InstrumentID: o.InstrumentID,
		Side: o.Side, Status: status, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at,
	}
}

// TestABoostEngineRecordsTheAcknowledgementOfAnOrderItReleased: the venue's
// acknowledgement of a released order is recorded evidence (ADR 0022),
// carried as execution.order.lifecycle version 2 for a limit order, and a
// Boost engine records it and decides nothing from it. A report for an
// order this run never released is refused (docs/development.md principle
// 4): it is reconciliation's to explain (ADR 0019).
func TestABoostEngineRecordsTheAcknowledgementOfAnOrderItReleased(t *testing.T) {
	dir := t.TempDir()
	opts, cfg := paperBoostOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, cfg, "test-build", paperFirstInput)
	session := feedUntilReleased(t, feed)
	order := feed.released[0]
	at := boostSession(session).Add(time.Minute)

	feed.send(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:"+order.ClientOrderID, at, lifecycle(order, event.OrderLifecycleStatusSubmitted, at))
	var bundle decisionsPayload
	if err := json.Unmarshal(feed.lastReply.Payload, &bundle); err != nil || len(bundle.Decisions) != 0 {
		t.Fatalf("the acknowledgement decided %d (%v), want nothing", len(bundle.Decisions), err)
	}

	unknown := order
	unknown.ClientOrderID = "not-released-here"
	raw, _ := json.Marshal(lifecycle(unknown, event.OrderLifecycleStatusSubmitted, at))
	in := event.Envelope{
		ID: "ack:unknown", Type: event.OrderLifecycleEventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: event.OrderLifecycleLimitVersion,
		EventTime: at, RecordedAt: at, Sequence: feed.sequence, Source: "test-adapter",
		StrategyVersion: feed.version, ConfigurationHash: feed.hash, PayloadHash: event.HashPayload(raw), Payload: raw,
	}
	if _, err := feed.client.Decide(context.Background(), in); err == nil || !strings.Contains(err.Error(), "not-released-here") {
		t.Fatalf("a report for an order this run never released: %v, want a refusal naming it", err)
	}
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the engine: %v", err)
	}
	if !logHoldsInput(recoverFile(t, opts.logPath), "ack:"+order.ClientOrderID) {
		t.Fatal("the durable log does not hold the acknowledgement")
	}
}

// lifecycleIn reads the released order's lifecycle state from the durable
// log at path.
func lifecycleIn(t *testing.T, path, clientID string) report.OrderState {
	t.Helper()
	_, entries, err := report.LoadEntries(readFile(t, path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	orders, _, err := report.OrderLifecycles(entries, event.TargetWeightStrategyBoost)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orders {
		if o.ClientOrderID == clientID {
			return o.State
		}
	}
	t.Fatalf("%s holds no order %s", path, clientID)
	return ""
}

// TestTheLogRecordsAnOrderCommittedThenReleasedThenAcknowledged: the order
// a paper engine releases is committed in the log once its group is
// flushed, released once the adapter's next input shows it holds the reply,
// and acknowledged once the venue's report is recorded (ADR 0027, section
// 4; ADR 0022).
func TestTheLogRecordsAnOrderCommittedThenReleasedThenAcknowledged(t *testing.T) {
	dir := t.TempDir()
	opts, cfg := paperBoostOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, cfg, "test-build", paperFirstInput)
	session := feedUntilReleased(t, feed)
	order := feed.released[0]

	if got := lifecycleIn(t, opts.logPath, order.ClientOrderID); got != report.LifecycleCommitted {
		t.Fatalf("after its release: %s, want %s", got, report.LifecycleCommitted)
	}
	feed.sessions(session+1, session+2)
	if got := lifecycleIn(t, opts.logPath, order.ClientOrderID); got != report.LifecycleReleased {
		t.Fatalf("after the adapter's next input: %s, want %s", got, report.LifecycleReleased)
	}
	at := boostSession(session + 1).Add(time.Minute)
	feed.send(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:"+order.ClientOrderID, at, lifecycle(order, event.OrderLifecycleStatusSubmitted, at))
	if got := lifecycleIn(t, opts.logPath, order.ClientOrderID); got != report.LifecycleAcknowledged {
		t.Fatalf("after the venue's report: %s, want %s", got, report.LifecycleAcknowledged)
	}
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the engine: %v", err)
	}
}

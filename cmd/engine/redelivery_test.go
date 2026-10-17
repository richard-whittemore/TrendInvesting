package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// paperBoostOptions is a paper Boost invocation writing into dir, whose
// released orders are marked to be sent.
func paperBoostOptions(t *testing.T, dir string) (options, event.TargetWeightConfigurationPayload) {
	t.Helper()
	opts := boostOptions(t, dir)
	opts.mode, opts.configPath = modePaper, writeBoostConfiguration(t, dir, modePaper, "DU1234567")
	return opts, boostConfiguration(t, opts.configPath)
}

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
	feed := newBoostFeed(t, opts.socketPath, cfg, "test-build", 2)
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
	feed := newBoostFeed(t, opts.socketPath, cfg, "test-build", 2)
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
	feed := newBoostFeed(t, first.socketPath, cfg, "test-build", 2)
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

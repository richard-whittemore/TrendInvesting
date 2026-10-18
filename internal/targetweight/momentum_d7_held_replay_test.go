package targetweight_test

import (
	"bytes"
	"context"
	"flag"
	"os"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// updateHeldReplay rewrites the held-one-lot-short replay fixture from this
// run, so a deliberate change shows as a diff to read before accepting it.
var updateHeldReplay = flag.Bool("update", false, "rewrite internal/targetweight's committed replay fixtures from this run")

// heldReplayFixture is a synthetic run, every input invented by
// simulateMomentum, whose account snapshot holds 10 AAA at a raw close of
// about $14,000 a share, far above its target: January's decision keeps AAA
// one lot short (ADR 0026, point 2, "D7-L", amended 2026-10-18). It holds no
// market data and is committed, so the decision is pinned byte for byte on
// every machine that runs the tests.
const heldReplayFixture = "testdata/held-one-lot-short.journal.jsonl"

// TestHeldOneLotShortReplayFixture pins a decided book with a held name kept
// one lot short: the run writes the committed journal byte for byte, its
// inputs replayed through a fresh handler reproduce its decisions, and the
// January decision it pins records AAA held-one-lot-short with figures that
// validate as written.
func TestHeldOneLotShortReplayFixture(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{rawScale: dearAAA, held: map[string]int64{"AAA": 10, "HHH": 10}, stopAfter: "2025-02-03"})
	header, err := r.recorder.Header(r.hash, strategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := journal.Write(&buf, header, r.entries); err != nil {
		t.Fatal(err)
	}
	if *updateHeldReplay {
		if err := os.WriteFile(heldReplayFixture, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	committed, err := os.ReadFile(heldReplayFixture)
	if err != nil {
		t.Fatalf("read the replay fixture: %v; run `go test ./internal/targetweight -run TestHeldOneLotShortReplayFixture -update` and read the journal before committing it", err)
	}
	if !bytes.Equal(buf.Bytes(), committed) {
		t.Fatalf("the run differs from %s; run `go test ./internal/targetweight -run TestHeldOneLotShortReplayFixture -update` to see the change as a diff and read it before accepting it", heldReplayFixture)
	}

	readHeader, records, err := journal.Read(bytes.NewReader(committed))
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.CheckIdentity(readHeader, records); err != nil {
		t.Fatal(err)
	}
	inputs, decisions, err := journal.Split(records)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := targetweight.New(strategyVersion, momentumConfiguration(), targetweight.Options{DecideFrom: closeOf("2025-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := replay.New(handler)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if d := replay.Equivalent(decisions, replayed); d != nil {
		t.Fatalf("replay diverged: %+v", d)
	}

	decided := targets(t, decisions)
	if len(decided) != 1 || decided[0].Outcome != event.TargetsOutcomeDecided {
		t.Fatalf("the fixture pins %d target decisions, want one decided: %+v", len(decided), decided)
	}
	if err := decided[0].ValidateWritten(); err != nil {
		t.Errorf("the pinned decision does not validate: %v", err)
	}
	if aaa := nameOf(t, decided[0], "AAA"); aaa.Status != event.MomentumStatusHeldOneLotShort || aaa.HeldQuantity != 10 {
		t.Errorf("AAA pinned as %+v, want held-one-lot-short with its 10 shares", aaa)
	}
}

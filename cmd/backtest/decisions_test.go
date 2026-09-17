package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

func signalJournal(t *testing.T) string {
	t.Helper()
	return rewriteJournal(t, goldenJournal, func(_ *journal.Header, entries []journal.Entry) []journal.Entry {
		var selected []journal.Entry
		for _, e := range entries {
			if e.Kind == journal.KindInput || e.Envelope.Type == event.SignalEventType {
				selected = append(selected, e)
			}
		}
		return selected
	})
}

func TestDecisionLogGolden(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-decisions", signalJournal(t)}, &out)
	if err != nil {
		t.Fatal(err)
	}
	const want = "2026-01-22T00:00:00Z [decision 22] AAPL: Signal long because high 129.01 exceeded Entry Channel 127.01; N was 1 (rule entry.channel.breakout; ADR 0002).\n"
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", &out, want)
	}
}

func TestDecisionLogFilters(t *testing.T) {
	path := signalJournal(t)
	for _, tc := range []struct {
		args  []string
		lines int
	}{
		{[]string{"-date", "2026-01-22"}, 1},
		{[]string{"-date", "2026-01-21"}, 0},
		{[]string{"-instrument", "AAPL"}, 1},
		{[]string{"-instrument", "AA"}, 0},
		{[]string{"-date", "2026-01-22", "-instrument", "MSFT"}, 0},
		{[]string{"-date", "2026-01-22", "-instrument", "AAPL"}, 1},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), append([]string{"-decisions", path}, tc.args...), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Count(out.String(), "\n") != tc.lines {
			t.Fatalf("%v: %s", tc.args, &out)
		}
	}
}

func TestDecisionLogReferenceUsesFullStream(t *testing.T) {
	wantPath := signalJournal(t)
	gotPath := rewriteJournal(t, wantPath, func(_ *journal.Header, entries []journal.Entry) []journal.Entry {
		for i := range entries {
			if entries[i].Envelope.Type != event.SignalEventType {
				continue
			}
			var p event.SignalPayload
			if err := json.Unmarshal(entries[i].Envelope.Payload, &p); err != nil {
				t.Fatal(err)
			}
			p.BreakoutHigh = math.Nextafter(p.BreakoutHigh, math.Inf(1))
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			entries[i].Envelope.Payload = raw
			entries[i].Envelope.PayloadHash = event.HashPayload(raw)
		}
		return entries
	})
	want, err := decisionsFromJournal(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decisionsFromJournal(gotPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := replay.Diff(want, got)
	if err != nil || report == nil {
		t.Fatalf("%v %v", report, err)
	}
	var out bytes.Buffer
	err = run(context.Background(), []string{"-decisions", gotPath, "-reference", wantPath, "-instrument", "MSFT"}, &out)
	if err == nil || !strings.Contains(err.Error(), report.String()) || out.String() != report.String()+"\n" {
		t.Fatalf("%v\n%s", err, &out)
	}
	out.Reset()
	if err := run(context.Background(), []string{"-decisions", wantPath, "-reference", wantPath}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "no divergence\n") {
		t.Fatal(&out)
	}
}

func TestDecisionLogRejectsAmbiguousFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-decisions", goldenJournal, "-verify", goldenJournal},
		{"-decisions", goldenJournal, "-replay", goldenJournal},
		{"-decisions", goldenJournal, "-runs", "hash", "-registry", "runs"},
		{"-decisions", goldenJournal, "-diff-want", goldenJournal, "-diff-got", goldenJournal},
		{"-decisions", goldenJournal, "-config", "config"},
		{"-date", "2026-01-22"}, {"-instrument", "AAPL"}, {"-reference", goldenJournal},
		{"-decisions", goldenJournal, "-date", "2026-02-30"},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
		if out.Len() != 0 {
			t.Fatalf("output before refusal: %v: %s", args, &out)
		}
	}
}

func TestDecisionLogVerifiesBothJournalsBeforeOutput(t *testing.T) {
	raw, err := os.ReadFile(goldenJournal)
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Replace(raw, []byte(`"source":"fixture"`), []byte(`"source":"edited"`), 1)
	if bytes.Equal(raw, bad) {
		t.Fatal("mutation missed")
	}
	path := filepath.Join(t.TempDir(), "broken.jsonl")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-decisions", path, "-date", "1999-01-01"},
		{"-decisions", goldenJournal, "-reference", path},
	} {
		var out bytes.Buffer
		err := run(context.Background(), args, &out)
		if err == nil || !strings.Contains(err.Error(), "chain") || out.Len() != 0 {
			t.Fatalf("%v: %v %s", args, err, &out)
		}
	}
}

func TestDecisionDateUsesUTCEventTime(t *testing.T) {
	path := rewriteJournal(t, signalJournal(t), func(_ *journal.Header, entries []journal.Entry) []journal.Entry {
		for i := range entries {
			if entries[i].Kind != journal.KindDecision {
				continue
			}
			entries[i].Envelope.EventTime = entries[i].Envelope.EventTime.In(time.FixedZone("EST", -5*60*60))
			entries[i].Envelope.RecordedAt = entries[i].Envelope.RecordedAt.Add(24 * time.Hour)
		}
		return entries
	})
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-decisions", path, "-date", "2026-01-22"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "2026-01-22T00:00:00Z [decision 22]") {
		t.Fatal(&out)
	}
}

func TestDecisionLogIOAndIdentityFailures(t *testing.T) {
	foreign := rewriteJournal(t, goldenJournal, func(_ *journal.Header, entries []journal.Entry) []journal.Entry {
		entries[0].Envelope.ConfigurationHash = "foreign"
		return entries
	})
	for _, args := range [][]string{
		{"-decisions", "missing.jsonl"},
		{"-decisions", goldenJournal, "-reference", "missing.jsonl"},
		{"-decisions", foreign},
		{"-decisions", goldenJournal, "-reference", foreign},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out); err == nil || out.Len() != 0 {
			t.Fatalf("%v: %v %s", args, err, &out)
		}
	}
	sentinel := errors.New("output unavailable")
	err := run(context.Background(), []string{"-decisions", signalJournal(t)}, decisionFailWriter{sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v", err)
	}
}

type decisionFailWriter struct{ err error }

func (w decisionFailWriter) Write([]byte) (int, error) { return 0, w.err }

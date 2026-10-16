package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDemoPrintsTheDecisionLogAndReplaysItsJournal(t *testing.T) {
	out := filepath.Join(t.TempDir(), "boost.journal")
	var buf bytes.Buffer
	if err := run([]string{"-sessions", "320", "-out", out}, &buf); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if got := strings.Count(text, "decided"); got != 320 {
		t.Errorf("%d decided sessions, want 320", got)
	}
	if !strings.Contains(lines[0], "target 0.98") || !strings.Contains(lines[0], "buy") || !strings.Contains(lines[0], "released (send false)") {
		t.Errorf("first line should buy 98%% from cash in dry-run: %q", lines[0])
	}
	if !strings.Contains(text, "target 1.47") || !strings.Contains(text, "sell") {
		t.Error("the synthetic series should enter and exit a boost")
	}
	if !strings.Contains(text, "replayed: journal decisions match") {
		t.Errorf("missing replay confirmation:\n%s", lines[len(lines)-1])
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Fatalf("journal not written: %v", err)
	}
	// A second run writes the same bytes.
	again := filepath.Join(t.TempDir(), "again.journal")
	if err := run([]string{"-sessions", "320", "-out", again}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(out)
	b, _ := os.ReadFile(again)
	if !bytes.Equal(a, b) {
		t.Error("two runs of the demo must write byte-identical journals")
	}
}

func TestDemoRefusesBadArguments(t *testing.T) {
	for _, args := range [][]string{{"-sessions", "0"}, {"-nope"}, {"-out", filepath.Join(t.TempDir(), "missing", "x")}} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%v) succeeded", args)
		}
	}
}

// TestDemoRefusesToOverwriteAJournal checks that -out never replaces an
// existing file, so a rerun cannot destroy recorded evidence (AGENTS.md
// rule 6).
func TestDemoRefusesToOverwriteAJournal(t *testing.T) {
	out := filepath.Join(t.TempDir(), "boost.journal")
	if err := os.WriteFile(out, []byte("earlier run"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-sessions", "320", "-out", out}, &bytes.Buffer{}); err == nil {
		t.Fatal("run() wrote over an existing journal, want a refusal")
	}
	if got, _ := os.ReadFile(out); string(got) != "earlier run" {
		t.Errorf("existing journal = %q, want it unchanged", got)
	}
}

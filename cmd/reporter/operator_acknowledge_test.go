package main

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/operator"
)

// keyIn returns the alert key a repeating alert's text ends with, the key
// the owner copies into `operator acknowledge`.
func keyIn(t *testing.T, message string) string {
	t.Helper()
	const marker = "acknowledge with alert key: "
	i := strings.Index(message, marker)
	if i < 0 {
		t.Fatalf("the alert states no key to acknowledge:\n%s", message)
	}
	return strings.TrimSpace(message[i+len(marker):])
}

// TestAnAcknowledgementTheOperatorToolRecordsStopsTheRepeats: a Halted
// engine's alert repeats on both channels at the repeat interval until the
// owner acknowledges it. The owner copies its key from the alert into
// `operator acknowledge`, which records it in the engine's inbox; the journal
// gets that record as the engine's inbox delivers it, read back by
// operator.Decode and enveloped by Record.Envelope, the functions
// cmd/engine applies (that the engine delivers and journals it is
// cmd/engine's TestAnAlertAcknowledgementIsDeliveredAndJournalled); from then on neither channel repeats the alert (ADR 0019, amended
// 2026-09-24, "Alerting": "repeats at a fixed interval until the owner
// acknowledges it. Acknowledgement is recorded").
func TestAnAcknowledgementTheOperatorToolRecordsStopsTheRepeats(t *testing.T) {
	if testing.Short() {
		t.Skip("builds cmd/operator; skipped in -short")
	}
	tool := filepath.Join(t.TempDir(), "operator")
	if out, err := exec.Command("go", "build", "-o", tool, "../operator").CombinedOutput(); err != nil {
		t.Fatalf("build the operator tool: %v\n%s", err, out)
	}

	r := &relay{sent: map[string][]string{}}
	client := &http.Client{Transport: r}
	dir := t.TempDir()
	entries := haltedEntries(t)
	logPath := filepath.Join(dir, "joint.log")
	// The engine's durable log, which it appends the delivered record to.
	write := func() {
		t.Helper()
		writeEntriesLog(t, logPath, entries)
		if err := os.Chtimes(logPath, t0, t0); err != nil {
			t.Fatal(err)
		}
	}
	write()
	args := []string{"-journal", logPath, "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-repeat-interval", "30m"}
	runAt := func(after time.Duration) {
		t.Helper()
		if err := run(args, twoChannels(), func() time.Time { return t0.Add(after) }, &bytes.Buffer{}, client); err != nil {
			t.Fatal(err)
		}
	}
	halts := func(host string) int { return count(r.to(host), "engine halted") }

	runAt(time.Hour)
	runAt(time.Hour + 31*time.Minute)
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		if got := halts(host); got != 2 {
			t.Fatalf("%s: the halt and one repeat before the acknowledgement, got %d:\n%s", host, got, strings.Join(r.to(host), "\n---\n"))
		}
	}
	var halt string
	for _, m := range r.to("ntfy.private.test") {
		if strings.Contains(m, "engine halted") {
			halt = m
		}
	}
	key := keyIn(t, halt)

	inbox := t.TempDir()
	acknowledge := exec.Command(tool, "acknowledge", "-inbox", inbox, "-account", "joint", "-alert", key, "-by", "owner",
		"-detail", "seen; reconciling at IBKR", "-at", t0.Add(time.Hour+40*time.Minute).Format(time.RFC3339))
	if out, err := acknowledge.CombinedOutput(); err != nil || !strings.Contains(string(out), "recorded operator.alert.acknowledged:joint:") {
		t.Fatalf("operator acknowledge: %v\n%s", err, out)
	}
	files, err := os.ReadDir(inbox)
	if err != nil || len(files) != 1 {
		t.Fatalf("the inbox holds %v (%v), want the one record", files, err)
	}
	raw, err := os.ReadFile(filepath.Join(inbox, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	record, err := operator.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: record.Envelope(6, "v", "h")})
	write()

	runAt(2*time.Hour + 2*time.Minute)
	runAt(3 * time.Hour)
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		if got := halts(host); got != 2 {
			t.Errorf("%s: an acknowledged alert is never repeated, got %d halts:\n%s", host, got, strings.Join(r.to(host), "\n---\n"))
		}
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// TestTheCommandReportsNoDivergenceBetweenAJournalAndItself: the identical
// case for -diff-want/-diff-got, run against the real golden journal rather
// than a synthetic fixture.
func TestTheCommandReportsNoDivergenceBetweenAJournalAndItself(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"-diff-want", goldenJournal, "-diff-got", goldenJournal}, &out); err != nil {
		t.Fatalf("run(-diff-want, -diff-got) error = %v", err)
	}
	if !strings.Contains(out.String(), "no divergence") {
		t.Fatalf("diff reported:\n%s", out.String())
	}
}

// TestTheCommandReportsAOneULPProtectiveStopDivergence is #21's headline,
// asked of the command line: a Protective Stop Level moved by exactly one
// float64 ULP (math.Nextafter — a genuine one-bit divergence, not a
// hand-picked literal that happens to look different) must be reported with
// both values distinguishable, on the field that actually differs.
//
// This is the defect class docs/development.md names ("never leave a
// multiply-add fusible"): a level that differs in only its last bit is
// exactly what a fused-multiply-add produces on one architecture and not
// another. A reporter that rounded floats before printing them would show
// the same text for both sides and hide it.
func TestTheCommandReportsAOneULPProtectiveStopDivergence(t *testing.T) {
	_, wantPath := runBacktestTo(t)

	var wantLevel, gotLevel float64
	gotPath := rewriteJournal(t, wantPath, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		for i := range entries {
			if entries[i].Kind != journal.KindDecision || entries[i].Envelope.Type != event.ProtectiveStopSetEventType {
				continue
			}
			var payload event.ProtectiveStopSetPayload
			if err := json.Unmarshal(entries[i].Envelope.Payload, &payload); err != nil {
				t.Fatalf("decode the recorded protective stop: %v", err)
			}
			wantLevel = payload.Level
			payload.Level = math.Nextafter(payload.Level, math.Inf(1))
			gotLevel = payload.Level
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			entries[i].Envelope.Payload = encoded
			entries[i].Envelope.PayloadHash = event.HashPayload(encoded)
			return entries
		}
		t.Fatal("the fixture journal records no protective stop to alter")
		return entries
	})

	if wantLevel == gotLevel {
		t.Fatal("test setup: math.Nextafter did not move the level; this test would prove nothing")
	}
	wantText := strconv.FormatFloat(wantLevel, 'g', -1, 64)
	gotText := strconv.FormatFloat(gotLevel, 'g', -1, 64)
	if wantText == gotText {
		t.Fatalf("test setup: %s and %s render identically", wantText, gotText)
	}

	var out bytes.Buffer
	err := run([]string{"-diff-want", wantPath, "-diff-got", gotPath}, &out)
	if err == nil {
		t.Fatal("run(-diff-want, -diff-got) error = nil, want the divergence reported")
	}
	if !strings.Contains(err.Error(), "level") {
		t.Fatalf("run() error = %v, want it to name the level field", err)
	}
	if !strings.Contains(err.Error(), wantText) || !strings.Contains(err.Error(), gotText) {
		t.Fatalf("run() error = %v, want both %q and %q distinguishable", err, wantText, gotText)
	}

	// The machine-readable report on stdout must be able to make the same
	// distinction — CI reads this one, not the error text.
	var decoded struct {
		FieldPath string          `json:"field_path"`
		Want      json.RawMessage `json:"want"`
		Got       json.RawMessage `json:"got"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &decoded); err != nil {
		t.Fatalf("json.Unmarshal(stdout) error = %v; stdout was:\n%s", err, out.String())
	}
	if !strings.HasSuffix(decoded.FieldPath, "level") {
		t.Fatalf("field_path = %q, want it to end in %q", decoded.FieldPath, "level")
	}
	if string(decoded.Want) == string(decoded.Got) {
		t.Fatalf("the machine-readable report renders want and got identically: %s vs %s", decoded.Want, decoded.Got)
	}
	if string(decoded.Want) != wantText || string(decoded.Got) != gotText {
		t.Fatalf("want/got = %s/%s, want %s/%s", decoded.Want, decoded.Got, wantText, gotText)
	}
}

// TestTheCommandReportsWhereALongerJournalDiverges: two journals of
// different lengths are reported at the point one ends, not silently
// truncated to the shorter one's length.
func TestTheCommandReportsWhereALongerJournalDiverges(t *testing.T) {
	_, wantPath := runBacktestTo(t)

	var duplicatedType string
	gotPath := rewriteJournal(t, wantPath, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].Kind == journal.KindDecision {
				duplicatedType = entries[i].Envelope.Type
				return append(entries, entries[i])
			}
		}
		t.Fatal("the fixture journal records no decision to duplicate")
		return entries
	})

	var out bytes.Buffer
	err := run([]string{"-diff-want", wantPath, "-diff-got", gotPath}, &out)
	if err == nil {
		t.Fatal("run(-diff-want, -diff-got) error = nil, want the length mismatch reported")
	}
	if !strings.Contains(err.Error(), "ends") {
		t.Fatalf("run() error = %v, want it to say a stream ended", err)
	}
	if !strings.Contains(err.Error(), duplicatedType) {
		t.Fatalf("run() error = %v, want it to name the extra %s decision", err, duplicatedType)
	}
}

// TestDiffWantAndDiffGotMustBeGivenTogether: half of a two-flag operation is
// an operator error, refused before either file is opened.
func TestDiffWantAndDiffGotMustBeGivenTogether(t *testing.T) {
	_, path := runBacktestTo(t)

	tests := [][]string{
		{"-diff-want", path},
		{"-diff-got", path},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			err := run(args, &out)
			if err == nil {
				t.Fatalf("run(%v) error = nil, want a refusal", args)
			}
			if !strings.Contains(err.Error(), "-diff-want and -diff-got must be given together") {
				t.Fatalf("run(%v) error = %v, want it to name the missing pair", args, err)
			}
		})
	}
}

// TestDiffIsMutuallyExclusiveWithOtherOperations: the diff mode is subject
// to the same one-operation-per-invocation rule as -verify and -replay.
func TestDiffIsMutuallyExclusiveWithOtherOperations(t *testing.T) {
	_, path := runBacktestTo(t)

	var out bytes.Buffer
	err := run([]string{"-verify", path, "-diff-want", path, "-diff-got", path}, &out)
	if err == nil {
		t.Fatal("run() error = nil, want a refusal naming two operations")
	}
	if !strings.Contains(err.Error(), "-verify") || !strings.Contains(err.Error(), "-diff-want and -diff-got") {
		t.Fatalf("run() error = %v, want it to name both -verify and -diff-want and -diff-got", err)
	}
	if out.Len() > 0 {
		t.Fatalf("run() wrote a report before refusing:\n%s", out.String())
	}
}

// TestDiffRefusesAMissingJournal: a path that names no file is an operator
// error, reported as one rather than as a divergence.
func TestDiffRefusesAMissingJournal(t *testing.T) {
	_, path := runBacktestTo(t)

	var out bytes.Buffer
	err := run([]string{"-diff-want", "testdata/does-not-exist.jsonl", "-diff-got", path}, &out)
	if err == nil {
		t.Fatal("run(-diff-want, -diff-got) error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "does-not-exist.jsonl") {
		t.Fatalf("run() error = %v, want it to name the missing file", err)
	}
}

// TestDiffRefusesAJournalWhoseChainIsBroken: -diff has no reducer of its
// own to re-derive a comparison from — unlike -replay, which would still
// catch a forged decision by recomputing it from the input stream, -diff
// takes both sides' decisions straight from the file. An edited-but-
// unrepaired journal (the chain not recomputed, exactly what
// TestVerifyRefusesAnEditedJournal exercises for -verify) must be refused
// before it is compared, not silently treated as evidence.
func TestDiffRefusesAJournalWhoseChainIsBroken(t *testing.T) {
	written, goodPath := runBacktestTo(t)
	_, otherPath := runBacktestTo(t)

	edited := bytes.Replace(written, []byte(`"source":"fixture"`), []byte(`"source":"forged"`), 1)
	if bytes.Equal(edited, written) {
		t.Fatal("the fixture no longer contains the text this test edits")
	}
	brokenPath := filepath.Join(t.TempDir(), "broken.jsonl")
	if err := os.WriteFile(brokenPath, edited, 0o600); err != nil {
		t.Fatalf("write the edited journal: %v", err)
	}

	for _, args := range [][]string{
		{"-diff-want", brokenPath, "-diff-got", otherPath},
		{"-diff-want", goodPath, "-diff-got", brokenPath},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			err := run(args, &out)
			if err == nil {
				t.Fatal("run(-diff-want, -diff-got) error = nil, want the broken chain refused")
			}
			if !strings.Contains(err.Error(), "chain") {
				t.Fatalf("run() error = %v, want it to name the broken chain", err)
			}
			if out.Len() > 0 {
				t.Fatalf("run() reported a divergence for evidence it never validated:\n%s", out.String())
			}
		})
	}
}

// TestDiffRefusesAJournalThatDescribesMoreThanOneRun: a journal whose chain
// is intact (recomputed by the editor, exactly as
// TestReplayRefusesAJournalWhoseRecordsNameAnotherRun forges one for
// -replay) but whose records disagree with its own header is refused by
// journal.CheckIdentity — a different finding from a broken chain, and
// reported distinguishably from one.
func TestDiffRefusesAJournalThatDescribesMoreThanOneRun(t *testing.T) {
	_, wantPath := runBacktestTo(t)
	_, otherPath := runBacktestTo(t)

	const foreign = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	gotPath := rewriteJournal(t, wantPath, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		for i := range entries {
			if entries[i].Kind != journal.KindInput || entries[i].Envelope.Type != event.CompletedBarEventType {
				continue
			}
			entries[i].Envelope.ConfigurationHash = foreign
			return entries
		}
		t.Fatal("the fixture journal records no bar input to re-attribute")
		return entries
	})

	// The chain is recomputed by rewriteJournal, so this is CheckIdentity's
	// own refusal, not chain verification leaking through.
	raw, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("read the rewritten journal: %v", err)
	}
	if _, err := journal.Verify(bytes.NewReader(raw)); err != nil {
		t.Fatalf("journal.Verify() error = %v; the rewritten journal must verify or this test proves nothing about identity", err)
	}

	var out bytes.Buffer
	err = run([]string{"-diff-want", otherPath, "-diff-got", gotPath}, &out)
	if err == nil {
		t.Fatal("run(-diff-want, -diff-got) error = nil, want the foreign record refused")
	}
	if strings.Contains(err.Error(), "chain") {
		t.Fatalf("run() error = %v, want an identity refusal, not a chain one", err)
	}
	if !strings.Contains(err.Error(), foreign) {
		t.Fatalf("run() error = %v, want it to name the foreign configuration hash %s", err, foreign)
	}
	if out.Len() > 0 {
		t.Fatalf("run() reported a divergence for evidence it never validated:\n%s", out.String())
	}
}

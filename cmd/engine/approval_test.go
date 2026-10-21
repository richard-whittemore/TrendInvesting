package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// laterAsOf is a fresh run's start after a first run at boostAsOf: a fresh
// run declares a new start (ADR 0027, the restart model).
var laterAsOf = boostSession(3).Format(time.RFC3339)

// writeApproval writes an approval naming previous with finalHash and
// returns its path.
func writeApproval(t *testing.T, dir, previous, finalHash string, latched bool) string {
	t.Helper()
	raw, err := json.Marshal(event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: previous, PreviousLogFinalHash: finalHash,
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
		Boost: event.BoostStateInForce{Latched: new(latched)},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// refuse runs opts, bounded, and requires a refusal saying want that leaves
// no durable log behind.
func refuse(t *testing.T, name string, opts options, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := run(ctx, opts, io.Discard)
	cancel()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: run() = %v, want a refusal saying %q", name, err, want)
	}
	if _, statErr := os.Stat(opts.logPath); !os.IsNotExist(statErr) {
		t.Errorf("%s: the refused run left a durable log behind", name)
	}
}

// TestAFreshRunAfterAPriorRunNeedsTheOwnersApproval: a restart that is not a
// crash is a fresh run, and it decides nothing until the owner's recorded
// approval names the previous run's log and that log's final hash (owner
// decision 2026-10-16). The approval is the fresh log's second record, and
// the adapter's inputs follow it.
func TestAFreshRunAfterAPriorRunNeedsTheOwnersApproval(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	firstLog := recoverFile(t, first.logPath)
	finalHash := firstLog.FinalRecordHash
	latch, decided := lastLatch(t, firstLog.Entries)
	if !decided {
		t.Fatal("the first run decided nothing")
	}

	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	fresh.asOf = laterAsOf
	refuse(t, "no approval", fresh, "operator.run.approved")

	wrong := fresh
	wrong.approvalPath = writeApproval(t, dir, "run-1.log", "sha256:"+strings.Repeat("0", 64), true)
	refuse(t, "a wrong final hash", wrong, "final record hash")

	unlatched := fresh
	unlatched.approvalPath = writeApproval(t, dir, "run-1.log", finalHash, !latch)
	refuse(t, "a latch the previous run's last decision does not hold", unlatched, fmt.Sprintf("latched_after %t", latch))

	missing := fresh
	missing.approvalPath = writeApproval(t, dir, "run-0.log", finalHash, latch)
	refuse(t, "an unknown previous log", missing, "run-0.log")

	resumed := fresh
	resumed.resumePath, resumed.approvalPath = first.logPath, writeApproval(t, dir, "run-1.log", finalHash, latch)
	refuse(t, "an approval with -resume", resumed, "-approval and -resume")

	approved := fresh
	approved.approvalPath = writeApproval(t, dir, "run-1.log", finalHash, latch)
	stop = startEngine(t, approved)
	feed = newBoostFeed(t, approved.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 3)
	feed.sessions(3, 4)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the approved run: %v", err)
	}
	entries := recoverFile(t, approved.logPath).Entries
	if entries[1].Envelope.Type != event.OperatorRunApprovedEventType || entries[1].Envelope.Sequence != 2 {
		t.Fatalf("the fresh log's second record is %s at sequence %d, want the approval at 2", entries[1].Envelope.Type, entries[1].Envelope.Sequence)
	}
	for _, e := range entries {
		if e.Envelope.Type != event.TargetsDecidedEventType {
			continue
		}
		var p event.TargetsDecidedPayload
		if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Boost.LatchedBefore != latch {
			t.Errorf("%s: the fresh run did not start from the approved latch", e.Envelope.ID)
		}
		break
	}
}

// lastLatch returns the latched_after of the last strategy.targets.decided
// in entries, and whether there is one.
func lastLatch(t *testing.T, entries []journal.Entry) (latched, decided bool) {
	t.Helper()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Envelope.Type != event.TargetsDecidedEventType {
			continue
		}
		var p event.TargetsDecidedPayload
		if err := json.Unmarshal(entries[i].Envelope.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return p.Boost.LatchedAfter, true
	}
	return false, false
}

// TestAnApprovalMustNameTheLatestRun: with several earlier logs in the
// directory, only the most recent can be approved, and an approval with no
// earlier run is refused rather than recorded as fact.
func TestAnApprovalMustNameTheLatestRun(t *testing.T) {
	dir := t.TempDir()
	hashes := map[string]string{}
	for i, name := range []string{"run-1", "run-2"} {
		opts := boostOptions(t, dir)
		opts.logPath, opts.outPath = filepath.Join(dir, name+".log"), filepath.Join(dir, name+".journal")
		if name == "run-2" {
			// A new start: run-1 holds nothing past its configuration, and a
			// fresh run never repeats that start.
			opts.asOf = "2024-01-03T21:00:00Z"
			opts.approvalPath = writeApproval(t, dir, "run-1.log", hashes["run-1.log"], false)
		}
		if err := startEngine(t, opts)(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		hashes[name+".log"] = recoverFile(t, opts.logPath).FinalRecordHash
		at := time.Date(2026, time.October, 10+i, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(opts.logPath, at, at); err != nil {
			t.Fatal(err)
		}
	}
	older := boostOptions(t, dir)
	older.logPath, older.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	older.approvalPath = writeApproval(t, dir, "run-1.log", hashes["run-1.log"], false)
	refuse(t, "an approval of an older run", older, "run-2.log")

	// run-2 ended before deciding, so its latch in force is its approval's.
	latched := boostOptions(t, dir)
	latched.logPath, latched.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	latched.approvalPath = writeApproval(t, dir, "run-2.log", hashes["run-2.log"], true)
	refuse(t, "a latch the previous run's approval does not hold", latched, "latched false")

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumed.resumePath = filepath.Join(dir, "run-1.log")
	refuse(t, "a resume of an older run", resumed, "run-2.log")

	empty := boostOptions(t, t.TempDir())
	empty.approvalPath = writeApproval(t, dir, "run-1.log", hashes["run-1.log"], false)
	refuse(t, "an approval with no earlier run", empty, "no earlier run")
}

// TestAnApprovalFileStatesEveryKey: the approval is written outside the
// engine, so one that leaves out Boost's latch or the incident in force is
// refused before anything is created, never read as "not latched" or "no
// incident" (ADR 0026, point 5; ADR 0027, the restart model).
func TestAnApprovalFileStatesEveryKey(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	if err := startEngine(t, first)(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	finalHash := recoverFile(t, first.logPath).FinalRecordHash
	base := `"account_id":"joint","previous_log":"run-1.log","previous_log_final_hash":"` + finalHash + `","incident_id":"","approved_by":"owner","approved_at":"2026-10-17T13:00:00Z","starting_book":null`
	for name, c := range map[string]struct{ raw, want string }{
		"no boost":    {`{` + base + `,"incident_in_force":null}`, "boost"},
		"no latch":    {`{` + base + `,"boost":{},"incident_in_force":null}`, "latched"},
		"null latch":  {`{` + base + `,"boost":{"latched":null},"incident_in_force":null}`, "latched"},
		"no incident": {`{` + base + `,"boost":{"latched":false}}`, "incident_in_force"},
	} {
		fresh := boostOptions(t, dir)
		fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
		fresh.approvalPath = filepath.Join(t.TempDir(), "approval.json")
		if err := os.WriteFile(fresh.approvalPath, []byte(c.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		refuse(t, name, fresh, c.want)
	}
}

// TestACrashBeforeTheApprovalIsRecordedResumesWithIt: a fresh run records
// its configuration, then the owner's approval; a crash between the two
// leaves a log holding nothing past the configuration. Resuming it alone
// would start from no latch and no incident, losing the approved state in
// force (ADR 0026, point 5; ADR 0027, the restart model), so it is refused;
// it resumes with the same approval, checked against the run before it, and
// records it as the resumed log's second input.
func TestACrashBeforeTheApprovalIsRecordedResumesWithIt(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	firstLog := recoverFile(t, first.logPath)
	latch, _ := lastLatch(t, firstLog.Entries)

	// The crashed fresh run: its log holds the configuration only.
	scratch := t.TempDir()
	crashed := boostOptions(t, scratch)
	crashed.asOf = laterAsOf
	if err := startEngine(t, crashed)(); err != nil {
		t.Fatalf("the configuration-only run: %v", err)
	}
	if n := len(recoverFile(t, crashed.logPath).Entries); n != 1 {
		t.Fatalf("the configuration-only log holds %d records, want 1", n)
	}
	raw, err := os.ReadFile(crashed.logPath)
	if err != nil {
		t.Fatal(err)
	}
	crashedLog := filepath.Join(dir, "run-2.log")
	if err := os.WriteFile(crashedLog, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(crashedLog, later, later); err != nil {
		t.Fatal(err)
	}

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumed.resumePath, resumed.asOf = crashedLog, laterAsOf
	refuse(t, "a resume without the approval", resumed, "-approval")

	wrong := resumed
	wrong.approvalPath = writeApproval(t, dir, "run-2.log", recoverFile(t, crashedLog).FinalRecordHash, latch)
	refuse(t, "an approval of the crashed run itself", wrong, "run-1.log")

	unlatched := resumed
	unlatched.approvalPath = writeApproval(t, dir, "run-1.log", firstLog.FinalRecordHash, !latch)
	refuse(t, "a latch the run before does not hold", unlatched, fmt.Sprintf("latched_after %t", latch))

	approved := resumed
	approved.approvalPath = writeApproval(t, dir, "run-1.log", firstLog.FinalRecordHash, latch)
	stop = startEngine(t, approved)
	feed = newBoostFeed(t, approved.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 3)
	feed.sessions(3, 4)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	entries := recoverFile(t, approved.logPath).Entries
	if entries[1].Envelope.Type != event.OperatorRunApprovedEventType || entries[1].Envelope.Sequence != 2 {
		t.Fatalf("the resumed log's second record is %s at sequence %d, want the approval at 2", entries[1].Envelope.Type, entries[1].Envelope.Sequence)
	}
	for _, e := range entries {
		if e.Envelope.Type != event.TargetsDecidedEventType {
			continue
		}
		var p event.TargetsDecidedPayload
		if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Boost.LatchedBefore != latch {
			t.Errorf("%s: the resumed run did not start from the approved latch", e.Envelope.ID)
		}
		break
	}
}

// configurationOnlyLog records a Boost run that stops before any input,
// leaving a log holding its configuration only, and installs a copy of it
// as dir/name modified at at: the log a start at asOf cut short leaves,
// whether a first run, a fresh run before its approval, or a resume of
// either.
func configurationOnlyLog(t *testing.T, dir, name, asOf string, at time.Time) string {
	t.Helper()
	scratch := t.TempDir()
	opts := boostOptions(t, scratch)
	opts.asOf = asOf
	if err := startEngine(t, opts)(); err != nil {
		t.Fatalf("the configuration-only run: %v", err)
	}
	raw, err := os.ReadFile(opts.logPath)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestARepeatedCrashBeforeTheApprovalIsRecordedResumesWithTheSameApproval:
// a resume of a start cut short copies its configuration before recording
// the approval, so a second crash in that window leaves another log holding
// the configuration only, a prefix of the first: an abandoned attempt, not
// a run (ADR 0027 §6). The first is still the one resumed, and the approval
// to record is still the one naming the run before it (ADR 0027, the
// restart model; ADR 0026, point 5: the state in force is restored, never
// lost to a restart).
func TestARepeatedCrashBeforeTheApprovalIsRecordedResumesWithTheSameApproval(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	firstLog := recoverFile(t, first.logPath)
	latch, _ := lastLatch(t, firstLog.Entries)
	now := time.Now()
	cut := configurationOnlyLog(t, dir, "run-2.log", laterAsOf, now.Add(time.Hour))
	attempt := configurationOnlyLog(t, dir, "run-3.log", laterAsOf, now.Add(2*time.Hour))

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-4.log"), filepath.Join(dir, "run-4.journal")
	resumed.resumePath, resumed.asOf = attempt, laterAsOf
	resumed.approvalPath = writeApproval(t, dir, "run-1.log", firstLog.FinalRecordHash, latch)
	refuse(t, "a resume of the abandoned attempt", resumed, "abandoned")

	resumed.resumePath, resumed.approvalPath = cut, ""
	refuse(t, "a resume without the approval", resumed, "-approval")

	approved := resumed
	approved.approvalPath = writeApproval(t, dir, "run-1.log", firstLog.FinalRecordHash, latch)
	stop = startEngine(t, approved)
	if err := stop(); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	entries := recoverFile(t, approved.logPath).Entries
	if len(entries) < 2 || entries[1].Envelope.Type != event.OperatorRunApprovedEventType {
		t.Fatalf("the resumed log does not record the approval as its second input: %d record(s)", len(entries))
	}
}

// TestACutShortFirstRunResumesWithoutAnApprovalAfterRepeatedCrashes: a
// first run needs no approval, and neither does a resume of its start cut
// short, however many attempts the directory holds; a later attempt at the
// same start is a prefix of the first, an abandoned attempt and never an
// earlier run an approval must name (ADR 0027, the restart model; ADR 0027
// §6).
func TestACutShortFirstRunResumesWithoutAnApprovalAfterRepeatedCrashes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	cut := configurationOnlyLog(t, dir, "run-1.log", boostAsOf, now.Add(time.Hour))
	attempt := configurationOnlyLog(t, dir, "run-2.log", boostAsOf, now.Add(2*time.Hour))
	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumed.resumePath = attempt
	refuse(t, "a resume of the abandoned attempt", resumed, "abandoned")
	resumed.resumePath = cut
	if err := startEngine(t, resumed)(); err != nil {
		t.Fatalf("the resumed first run: %v", err)
	}
}

// TestAFreshRunCannotRepeatAStartThatRecordedNothing: a fresh run whose
// configuration input is the very one a log holding nothing past it opens
// with would leave a log no resume could tell from an attempt at that same
// start, so a crash before its approval was recorded would lose the
// approval (ADR 0027, the restart model). It is refused: resume that log,
// or declare a new start with -as-of.
func TestAFreshRunCannotRepeatAStartThatRecordedNothing(t *testing.T) {
	dir := t.TempDir()
	previous := configurationOnlyLog(t, dir, "run-1.log", boostAsOf, time.Now())
	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	fresh.approvalPath = writeApproval(t, dir, "run-1.log", recoverFile(t, previous).FinalRecordHash, false)
	refuse(t, "a repeated start", fresh, "-as-of")
}

// TestAFreshRunAfterACutShortStartIsCheckedAgainstTheRunBeforeIt: a log
// holding nothing past its configuration decided nothing and records no
// state, so a fresh run approved after it, with a new -as-of, is checked
// against the latest earlier log that holds more: the latch and incident
// that run ended with are still in force (ADR 0026, point 5; ADR 0019,
// amended 2026-09-24, "The state survives a restart"; ADR 0027, the
// restart model). The approval still names the latest log and its hash.
func TestAFreshRunAfterACutShortStartIsCheckedAgainstTheRunBeforeIt(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	latch, _ := lastLatch(t, recoverFile(t, first.logPath).Entries)
	cut := configurationOnlyLog(t, dir, "run-2.log", laterAsOf, time.Now().Add(time.Hour))
	cutHash := recoverFile(t, cut).FinalRecordHash

	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	fresh.asOf = boostSession(4).Format(time.RFC3339)
	unlatched := fresh
	unlatched.approvalPath = writeApproval(t, dir, "run-2.log", cutHash, !latch)
	refuse(t, "a latch the run before the cut-short start does not hold", unlatched, fmt.Sprintf("latched_after %t", latch))

	approved := fresh
	approved.approvalPath = writeApproval(t, dir, "run-2.log", cutHash, latch)
	if err := startEngine(t, approved)(); err != nil {
		t.Fatalf("the approved fresh run: %v", err)
	}
}

// committedPrefix installs dir/name as the first groups committed groups of
// the log at source, modified at at: the log a resume of source leaves when
// it stops partway through copying source's committed groups (ADR 0027 §6:
// the new log begins with the committed part of the old, byte for byte).
func committedPrefix(t *testing.T, source, dir, name string, groups int, at time.Time) string {
	t.Helper()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	kept, commits := lines[:1], 0
	for _, line := range lines[1:] {
		if commits == groups {
			break
		}
		kept = append(kept, line)
		if strings.Contains(line, `"commit"`) {
			commits++
		}
	}
	if commits != groups {
		t.Fatalf("%s commits %d group(s), fewer than %d", source, commits, groups)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(kept, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAResumeAbandonedPartwayIsNeverTheLatestRun: a resume creates its new
// log, then copies the old log's committed groups into it one by one (ADR
// 0027 §6), so a resume refused for divergence, or a crash during the copy,
// leaves a newer log holding a prefix of the old one's committed groups. That log is an abandoned attempt, not a run: the state it ends in
// is a cut-down copy of the old run's, so it is never the latest log. The
// old log is still the one resumed or approved, and the attempt is named
// when it is.
func TestAResumeAbandonedPartwayIsNeverTheLatestRun(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	firstLog := recoverFile(t, first.logPath)
	latch, _ := lastLatch(t, firstLog.Entries)
	abandoned := committedPrefix(t, first.logPath, dir, "run-2.log", 3, time.Now().Add(time.Hour))
	if n, all := len(recoverFile(t, abandoned).Entries), len(firstLog.Entries); n < 2 || n >= all {
		t.Fatalf("the abandoned attempt holds %d of %d records, want a strict prefix past the configuration", n, all)
	}

	resumedAttempt := boostOptions(t, dir)
	resumedAttempt.logPath, resumedAttempt.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumedAttempt.resumePath = abandoned
	refuse(t, "a resume of the abandoned attempt", resumedAttempt, "run-1.log")

	approvedAttempt := boostOptions(t, dir)
	approvedAttempt.logPath, approvedAttempt.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	approvedAttempt.approvalPath = writeApproval(t, dir, "run-2.log", recoverFile(t, abandoned).FinalRecordHash, latch)
	refuse(t, "an approval of the abandoned attempt", approvedAttempt, "run-1.log")

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumed.resumePath = first.logPath
	if err := startEngine(t, resumed)(); err != nil {
		t.Fatalf("the resume of the run the attempt abandoned: %v", err)
	}
}

// TestAResumeAbandonedBeforeItCopiedPastItsConfigurationIsNeverTheLatestRun:
// a resume creates its new log, then copies the old log's committed groups
// one by one, the configuration first (ADR 0027 §6), so a resume that stops
// before or just after that first group leaves a newer log, empty or
// holding the configuration only. Holding the configuration, it is a prefix
// of the old log's committed records under the same header, an abandoned
// attempt; empty, it is an abandoned start. Neither is a run, however
// little it copied: the crashed run is still the one resumed, and the
// abandoned log is named when it is.
func TestAResumeAbandonedBeforeItCopiedPastItsConfigurationIsNeverTheLatestRun(t *testing.T) {
	for _, groups := range []int{0, 1} {
		dir := t.TempDir()
		first := boostOptions(t, dir)
		first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
		stop := startEngine(t, first)
		feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
		feed.sessions(0, 3)
		_ = feed.client.Close()
		if err := stop(); err != nil {
			t.Fatalf("the first run: %v", err)
		}
		abandoned := committedPrefix(t, first.logPath, dir, "run-2.log", groups, time.Now().Add(time.Hour))
		if n := len(recoverFile(t, abandoned).Entries); n != groups {
			t.Fatalf("the abandoned attempt holds %d record(s), want %d", n, groups)
		}

		resumedAttempt := boostOptions(t, dir)
		resumedAttempt.logPath, resumedAttempt.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
		resumedAttempt.resumePath = abandoned
		refuse(t, fmt.Sprintf("a resume of an attempt abandoned after %d group(s)", groups), resumedAttempt, "abandoned")

		resumed := resumedAttempt
		resumed.resumePath = first.logPath
		if err := startEngine(t, resumed)(); err != nil {
			t.Fatalf("after %d group(s): the resume of the run the attempt abandoned: %v", groups, err)
		}
	}
}

// TestAFreshRunCannotRepeatTheStartOfAnEarlierRun: a fresh run declares a
// new start with a new -as-of. One whose configuration input is the one an
// earlier log opens with, under the same header, would, if it crashed
// before its approval was recorded, leave a log indistinguishable from a
// resume of that earlier run abandoned after its configuration (ADR 0027
// §6), so it is refused (ADR 0027, the restart model).
func TestAFreshRunCannotRepeatTheStartOfAnEarlierRun(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	firstLog := recoverFile(t, first.logPath)
	latch, _ := lastLatch(t, firstLog.Entries)
	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	fresh.approvalPath = writeApproval(t, dir, "run-1.log", firstLog.FinalRecordHash, latch)
	refuse(t, "a repeated start", fresh, "-as-of")
}

// TestAnUnrecoverableLogIsSetAsideInTheQuarantineFolder: every *.log in the
// state directory's root is read to find the latest run, so one the engine
// cannot recover (ADR 0027 §5) refuses every start, naming the step: move
// it, unchanged, into the quarantine folder beside it, never editing or
// deleting it (ADR 0018). The engine reads only the directory's root, so a
// log in the quarantine folder is never a run.
func TestAnUnrecoverableLogIsSetAsideInTheQuarantineFolder(t *testing.T) {
	dir := t.TempDir()
	damaged := filepath.Join(dir, "run-1.log")
	if err := os.WriteFile(damaged, []byte("not a durable log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	refuse(t, "a start beside an unrecoverable log", fresh, filepath.Join(dir, "quarantine"))

	if err := os.Mkdir(filepath.Join(dir, "quarantine"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(damaged, filepath.Join(dir, "quarantine", "run-1.log")); err != nil {
		t.Fatal(err)
	}
	if err := startEngine(t, fresh)(); err != nil {
		t.Fatalf("a first run once the unrecoverable log is quarantined: %v", err)
	}
}

// firstBoostRun runs a first engine whose log is dir/run-1.log through the
// first three sessions and returns its options and recovered log.
func firstBoostRun(t *testing.T, dir string) (options, journal.Recovery) {
	t.Helper()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	return first, recoverFile(t, first.logPath)
}

// TestAnEmptyLogIsAnAbandonedStartNeverARun: a run creates its log, header
// first, before it commits its first record, so a fresh run that crashes in
// that window, like a resume that crashes before its first copy, leaves a
// log holding no committed record. Nothing in it tells the two apart, and
// it records nothing to resume, so it is an abandoned start: never the
// latest run, never the source of an abandoned resume, and a resume of it
// is refused with the step for either case. The fresh run it was is started
// again, at its new -as-of, with its approval (ADR 0027, the restart model;
// ADR 0018: the empty log stays as evidence).
func TestAnEmptyLogIsAnAbandonedStartNeverARun(t *testing.T) {
	dir := t.TempDir()
	first, firstLog := firstBoostRun(t, dir)
	latch, _ := lastLatch(t, firstLog.Entries)
	empty := committedPrefix(t, first.logPath, dir, "run-2.log", 0, time.Now().Add(time.Hour))

	resumedEmpty := boostOptions(t, dir)
	resumedEmpty.logPath, resumedEmpty.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumedEmpty.resumePath, resumedEmpty.asOf = empty, laterAsOf
	refuse(t, "a resume of the empty log", resumedEmpty, "holds no committed record")
	refuse(t, "a resume of the empty log", resumedEmpty, "start the fresh run again")

	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	fresh.asOf = laterAsOf
	fresh.approvalPath = writeApproval(t, dir, "run-1.log", firstLog.FinalRecordHash, latch)
	if err := startEngine(t, fresh)(); err != nil {
		t.Fatalf("the fresh run started again beside its empty log: %v", err)
	}
	entries := recoverFile(t, fresh.logPath).Entries
	if len(entries) < 2 || entries[1].Envelope.Type != event.OperatorRunApprovedEventType {
		t.Fatalf("the fresh run does not record its approval as its second input: %d record(s)", len(entries))
	}
}

// TestAnEmptyLogAloneLeavesAFirstRun: an empty log beside no other is an
// abandoned start, not an earlier run, so the run started again is still a
// first run and needs no approval (ADR 0027, the restart model).
func TestAnEmptyLogAloneLeavesAFirstRun(t *testing.T) {
	source := t.TempDir()
	first, _ := firstBoostRun(t, source)
	dir := t.TempDir()
	committedPrefix(t, first.logPath, dir, "run-1.log", 0, time.Now())
	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	if err := startEngine(t, fresh)(); err != nil {
		t.Fatalf("a first run beside an empty log: %v", err)
	}
}

// TestALogThatCannotBeReadIsNeverQuarantined: a log the engine cannot open
// or read, for its permissions or a disk fault, may be an intact crashed
// run, so the refusal names the fault to fix and retry and gives no
// quarantine step; only a log whose content cannot be recovered (ADR 0027
// §5) is set aside (ADR 0018).
func TestALogThatCannotBeReadIsNeverQuarantined(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	dir := t.TempDir()
	first, _ := firstBoostRun(t, dir)
	if err := os.Chmod(first.logPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(first.logPath, 0o600) })
	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	fresh.asOf = laterAsOf
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := run(ctx, fresh, io.Discard)
	cancel()
	if err == nil || strings.Contains(err.Error(), "quarantine") || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("run() = %v, want a refusal saying the log could not be read, with no quarantine step", err)
	}
}

// TestAnAbandonedResumeNamesTheLogItCopied: a resume's log begins with the
// committed part of the run it resumed (ADR 0027 §6), so an attempt
// abandoned while copying a resumed run is also a prefix of every run that
// run continued; it is attributed to the newest log it is a prefix of, the
// one it copied, which is also the latest run to resume.
func TestAnAbandonedResumeNamesTheLogItCopied(t *testing.T) {
	source := t.TempDir()
	full, _ := firstBoostRun(t, source)
	dir := t.TempDir()
	now := time.Now()
	committedPrefix(t, full.logPath, dir, "run-1.log", 3, now.Add(time.Hour))
	all := 0
	for _, entry := range recoverFile(t, full.logPath).Entries {
		if entry.Kind == journal.KindInput {
			all++
		}
	}
	committedPrefix(t, full.logPath, dir, "run-2.log", all, now.Add(2*time.Hour))
	attempt := committedPrefix(t, full.logPath, dir, "run-3.log", 2, now.Add(3*time.Hour))
	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-4.log"), filepath.Join(dir, "run-4.journal")
	resumed.resumePath = attempt
	refuse(t, "a resume of the abandoned attempt", resumed, "a resume of run-2.log abandoned")
}

// TestAnAttemptAtAnAttemptsSourceNamesTheRunItCopied: two resumes of one
// crashed run, each abandoned while copying it, leave two attempts, the
// later a prefix of the earlier as well as of the run. An attempt is never
// the log another attempt copied: the operator is sent to the run both
// copied, the one the engine resumes (ADR 0027 §6).
func TestAnAttemptAtAnAttemptsSourceNamesTheRunItCopied(t *testing.T) {
	source := t.TempDir()
	full, _ := firstBoostRun(t, source)
	dir := t.TempDir()
	now := time.Now()
	all := 0
	for _, entry := range recoverFile(t, full.logPath).Entries {
		if entry.Kind == journal.KindInput {
			all++
		}
	}
	committedPrefix(t, full.logPath, dir, "run-1.log", all, now.Add(time.Hour))
	committedPrefix(t, full.logPath, dir, "run-2.log", 3, now.Add(2*time.Hour))
	attempt := committedPrefix(t, full.logPath, dir, "run-3.log", 2, now.Add(3*time.Hour))
	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-4.log"), filepath.Join(dir, "run-4.journal")
	resumed.resumePath = attempt
	refuse(t, "a resume of the second abandoned attempt", resumed, "a resume of run-1.log abandoned")
}

// TestAnUnreadableLogSupersededByANewerRunMayBeQuarantined: a log that
// cannot be read is never skipped, so it still stops every start; but when
// a newer log holding a record was modified after it, it is not the latest
// run, so once the operator confirms that newer log is no resume of it
// abandoned partway, the refusal's step is to move it, unchanged, into
// quarantine (ADR 0018), and the next start finds the newer run.
func TestAnUnreadableLogSupersededByANewerRunMayBeQuarantined(t *testing.T) {
	source := t.TempDir()
	full, fullLog := firstBoostRun(t, source)
	latch, _ := lastLatch(t, fullLog.Entries)
	dir := t.TempDir()
	now := time.Now()
	all := 0
	for _, entry := range fullLog.Entries {
		if entry.Kind == journal.KindInput {
			all++
		}
	}
	old := committedPrefix(t, full.logPath, dir, "run-1.log", all, now.Add(time.Hour))
	committedPrefix(t, full.logPath, dir, "run-2.log", all, now.Add(2*time.Hour))
	faultLogRead(t, old, 200, errDiskFault)

	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	fresh.asOf = laterAsOf
	fresh.approvalPath = writeApproval(t, dir, "run-2.log", fullLog.FinalRecordHash, latch)
	refuse(t, "a start beside a superseded unreadable log", fresh, "could not be read")
	refuse(t, "a start beside a superseded unreadable log", fresh, "run-2.log")
	refuse(t, "a start beside a superseded unreadable log", fresh, "into "+filepath.Join(dir, quarantineFolder))

	if err := os.MkdirAll(filepath.Join(dir, quarantineFolder), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, filepath.Join(dir, quarantineFolder, "run-1.log")); err != nil {
		t.Fatal(err)
	}
	if err := startEngine(t, fresh)(); err != nil {
		t.Fatalf("the fresh run after the unreadable log was quarantined: %v", err)
	}
}

// TestAnUnreadableLogThatMayBeTheLatestRunIsNeverQuarantined: a log that
// cannot be read, and that no newer log holding a record was modified
// after, is or may be the latest run, an intact crashed run, so the
// refusal names the fault to fix and gives no quarantine step, even beside
// a newer empty start.
func TestAnUnreadableLogThatMayBeTheLatestRunIsNeverQuarantined(t *testing.T) {
	source := t.TempDir()
	full, _ := firstBoostRun(t, source)
	dir := t.TempDir()
	now := time.Now()
	committedPrefix(t, full.logPath, dir, "run-1.log", 2, now.Add(time.Hour))
	latest := committedPrefix(t, full.logPath, dir, "run-2.log", 3, now.Add(2*time.Hour))
	committedPrefix(t, full.logPath, dir, "run-3.log", 0, now.Add(3*time.Hour))
	faultLogRead(t, latest, 200, errDiskFault)

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-4.log"), filepath.Join(dir, "run-4.journal")
	resumed.resumePath = latest
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := run(ctx, resumed, io.Discard)
	cancel()
	if err == nil || strings.Contains(err.Error(), "quarantine") || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("run() = %v, want a refusal saying the log could not be read, with no quarantine step", err)
	}
	for _, want := range []string{"never set aside while the disk may yet be fixed", "give-up steps"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("run() = %v, want the refusal to say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "never setting it aside") {
		t.Fatalf("run() = %v, want no claim the log is never set aside", err)
	}
}

// TestAnOldLogWithAPermissionFaultIsNeverQuarantined: a log the engine is
// not permitted to read is fixed by its owner or mode, never set aside,
// however old it is.
func TestAnOldLogWithAPermissionFaultIsNeverQuarantined(t *testing.T) {
	source := t.TempDir()
	full, _ := firstBoostRun(t, source)
	dir := t.TempDir()
	now := time.Now()
	old := committedPrefix(t, full.logPath, dir, "run-1.log", 2, now.Add(time.Hour))
	committedPrefix(t, full.logPath, dir, "run-2.log", 3, now.Add(2*time.Hour))
	faultLogRead(t, old, -1, fs.ErrPermission)

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumed.resumePath = filepath.Join(dir, "run-2.log")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := run(ctx, resumed, io.Discard)
	cancel()
	if err == nil || strings.Contains(err.Error(), "quarantine") || !strings.Contains(err.Error(), "owner or mode") {
		t.Fatalf("run() = %v, want a refusal naming the owner or mode to fix, with no quarantine step", err)
	}
}

// craftedLog writes a durable log at path under comp's header: the
// configuration input, then one input whose decisions are decisions.
func craftedLog(t *testing.T, path string, comp composition, decisions ...event.Envelope) journal.Recovery {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	log, err := journal.CreateLog(file, logHeader(comp))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]journal.Entry{{Kind: journal.KindInput, Envelope: comp.configEnvelope}}); err != nil {
		t.Fatal(err)
	}
	stamp := func(id, eventType string, version uint32, sequence uint64, payload any) event.Envelope {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		at := boostSession(1)
		return event.Envelope{
			ID: id, Type: eventType, SchemaVersion: version, EnvelopeVersion: event.CurrentEnvelopeVersion, EventTime: at, RecordedAt: at,
			Sequence: sequence, Source: "test", StrategyVersion: comp.strategyVersion, ConfigurationHash: comp.configurationHash,
			PayloadHash: event.HashPayload(raw), Payload: raw,
		}
	}
	group := []journal.Entry{{Kind: journal.KindInput, Envelope: stamp("session:1", event.SessionClosedEventType, event.SessionClosedSchemaVersion, 2,
		event.SessionClosedPayload{PeriodEnd: boostSession(1), InstrumentIDs: []string{"SPY"}})}}
	for i, d := range decisions {
		group = append(group, journal.Entry{Kind: journal.KindDecision, Envelope: stamp(d.ID, d.Type, d.SchemaVersion, uint64(i+1), d.Payload)})
	}
	if err := log.Append(group); err != nil {
		t.Fatal(err)
	}
	return recoverFile(t, path)
}

// TestAnApprovalCannotUnderstateTheIncidentsSaleAttempt: the previous run
// ended Halted after attempting the incident's one sale back to 98%, so an
// approval carrying that incident in force with the sale not attempted is
// refused; a fresh run would otherwise attempt it again (ADR 0019, amended
// 2026-10-15). Stated attempted, the fresh run starts.
func TestAnApprovalCannotUnderstateTheIncidentsSaleAttempt(t *testing.T) {
	dir := t.TempDir()
	comp, err := compose(boostConfigPath, "test-build", boostSession(0))
	if err != nil {
		t.Fatal(err)
	}
	encode := func(p any) json.RawMessage {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	halted := event.Envelope{ID: "engine-state:joint:1", Type: event.EngineStateEventType, SchemaVersion: event.EngineStateScopedVersion,
		Payload: encode(event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "test"})}
	sale := event.Envelope{ID: "order-planned:joint-sale-1", Type: event.OrderPlannedEventType, SchemaVersion: event.OrderPlannedSafetyVersion,
		Payload: encode(event.OrderPlannedPayload{AccountID: "joint", Tag: "engine-state:joint:1", ClientOrderID: "joint-sale-1", InstrumentID: "SPY",
			Side: event.OrderSideSell, Phase: event.OrderSideSell, OrderType: event.OrderTypeLimit, Quantity: 1, LimitPrice: 99,
			NetLiquidation: 4000, HeldQuantity: 10, ReferencePrice: 100, PriceBand: 0.05,
			Sale: event.SaleFigures{Kind: event.SaleKindIncident, Trigger: event.SaleTriggerExit}})}
	previous := craftedLog(t, filepath.Join(dir, "run-1.log"), comp, halted, sale)

	approval := func(attempted bool) string {
		raw, err := json.Marshal(event.OperatorRunApprovedPayload{
			AccountID: "joint", PreviousLog: "run-1.log", PreviousLogFinalHash: previous.FinalRecordHash,
			ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
			Boost:           event.BoostStateInForce{Latched: new(false)},
			IncidentInForce: &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(attempted)},
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "approval.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath, fresh.asOf = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal"), laterAsOf
	fresh.approvalPath = approval(false)
	refuse(t, "the sale stated not attempted", fresh, "records an attempt")

	fresh.approvalPath = approval(true)
	stop := startEngine(t, fresh)
	if err := stop(); err != nil {
		t.Fatalf("the approved run: %v", err)
	}
}

// TestAnApprovalMayStateAHandSaleAsTheIncidentsAttempt: the previous run
// ended Halted without attempting the incident's one sale, and the owner sold
// by hand, so the approval states it attempted (operator approve
// -sale-attempted) though the log records none; the fresh run starts, since
// stating an attempt is always accepted (ADR 0019, amended 2026-10-15).
func TestAnApprovalMayStateAHandSaleAsTheIncidentsAttempt(t *testing.T) {
	dir := t.TempDir()
	comp, err := compose(boostConfigPath, "test-build", boostSession(0))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "test"})
	if err != nil {
		t.Fatal(err)
	}
	halted := event.Envelope{ID: "engine-state:joint:1", Type: event.EngineStateEventType, SchemaVersion: event.EngineStateScopedVersion, Payload: raw}
	previous := craftedLog(t, filepath.Join(dir, "run-1.log"), comp, halted)

	approval, err := json.Marshal(event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-1.log", PreviousLogFinalHash: previous.FinalRecordHash,
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
		Boost:           event.BoostStateInForce{Latched: new(false)},
		IncidentInForce: &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath, fresh.asOf = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal"), laterAsOf
	fresh.approvalPath = filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(fresh.approvalPath, approval, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := startEngine(t, fresh)
	if err := stop(); err != nil {
		t.Fatalf("the approved run: %v", err)
	}
}

// writeFirstRunApproval writes a first run's approval for account, stating
// the starting book, and returns its path.
func writeFirstRunApproval(t *testing.T, account string) string {
	t.Helper()
	raw, err := json.Marshal(event.OperatorRunApprovedPayload{
		AccountID: account, ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
		Boost:        event.BoostStateInForce{Latched: new(false)},
		StartingBook: &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 4000, Currency: "USD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "first-run-approval.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAFirstPaperRunStartsOnlyWithTheOwnersStartingBook: an account's first
// run has no earlier run whose last account snapshot the adapter can compare
// its first reading with, so a first run that can reach a broker account
// (paper, live) decides nothing until the owner's approval states the
// holdings and cash it starts from (ADR 0019 and ADR 0027, amended
// 2026-10-18). The approval is its second record, as a fresh run's is, under
// an ID of its own; a fresh run's approval is never a first run's.
func TestAFirstPaperRunStartsOnlyWithTheOwnersStartingBook(t *testing.T) {
	dir := t.TempDir()
	opts := boostOptions(t, dir)
	opts.mode, opts.configPath = modePaper, writeBoostConfiguration(t, dir, modePaper, "DU1234567")
	refuse(t, "no approval", opts, "starting book")
	chained := opts
	chained.approvalPath = writeApproval(t, dir, "run-0.log", "sha256:"+strings.Repeat("0", 64), false)
	refuse(t, "a fresh run's approval", chained, "first run")
	other := opts
	other.approvalPath = writeFirstRunApproval(t, "DU7654321")
	refuse(t, "another account", other, "DU7654321")

	opts.approvalPath = writeFirstRunApproval(t, "DU1234567")
	if err := startEngine(t, opts)(); err != nil {
		t.Fatalf("a first paper run with its starting book: %v", err)
	}
	entries := recoverFile(t, opts.logPath).Entries
	approval := entries[1].Envelope
	if approval.Type != event.OperatorRunApprovedEventType || approval.Sequence != 2 || !strings.HasPrefix(approval.ID, "operator-run-approved:first-run:") {
		t.Fatalf("the first run's second record is %s %s at sequence %d, want its approval at 2", approval.Type, approval.ID, approval.Sequence)
	}
	p, err := event.DecodeRecordedOperatorRunApproved(approval.Payload, approval.SchemaVersion)
	if err != nil || !p.FirstRun() || p.StartingBook.TotalCash != 4000 {
		t.Fatalf("the recorded approval = %+v, %v", p, err)
	}

	fresh := opts
	fresh.logPath, fresh.outPath, fresh.asOf = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal"), laterAsOf
	fresh.approvalPath = writeFirstRunApproval(t, "DU1234567")
	refuse(t, "a first run's approval after a run", fresh, "first run")
}

// TestACutShortFirstPaperRunResumesWithItsApproval: a first paper run that
// crashed after its configuration and before its approval was recorded is
// resumed with the approval it was started with, which states the starting
// book; without it the resume is refused (ADR 0027, the restart model).
func TestACutShortFirstPaperRunResumesWithItsApproval(t *testing.T) {
	dir := t.TempDir()
	opts := boostOptions(t, dir)
	opts.mode, opts.configPath = modePaper, writeBoostConfiguration(t, dir, modePaper, "DU1234567")
	cut := configurationOnlyLogOf(t, dir, "run-1.log", opts, time.Now().Add(time.Hour))
	resumed := opts
	resumed.logPath, resumed.outPath, resumed.resumePath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal"), cut
	refuse(t, "no approval", resumed, "starting book")
	resumed.approvalPath = writeFirstRunApproval(t, "DU1234567")
	if err := startEngine(t, resumed)(); err != nil {
		t.Fatalf("the resumed first run: %v", err)
	}
	if entries := recoverFile(t, resumed.logPath).Entries; entries[1].Envelope.Type != event.OperatorRunApprovedEventType {
		t.Fatalf("the resumed log's second record is %s, want the approval", entries[1].Envelope.Type)
	}
}

// configurationOnlyLogOf installs dir/name as the configuration-only log a
// start of opts cut short before its approval leaves, modified at at: the
// first committed group of a run of opts started with a first run's
// approval in a directory of its own.
func configurationOnlyLogOf(t *testing.T, dir, name string, opts options, at time.Time) string {
	t.Helper()
	scratch := t.TempDir()
	whole := opts
	whole.logPath, whole.outPath = filepath.Join(scratch, "run.log"), filepath.Join(scratch, "run.journal")
	whole.socketPath = filepath.Join(shortSocketDir(t), "engine.sock")
	whole.approvalPath = writeFirstRunApproval(t, "DU1234567")
	if err := startEngine(t, whole)(); err != nil {
		t.Fatalf("the first run copied: %v", err)
	}
	return committedPrefix(t, whole.logPath, dir, name, 1, at)
}

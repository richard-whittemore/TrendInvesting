package restart_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

var (
	header      = journal.NewLogHeader("sha256:config", "boost/1.0.0+build")
	otherHeader = journal.NewLogHeader("sha256:config", "boost/1.0.0+other")
)

// record is a recorded entry of kind whose envelope is told apart by id.
func record(kind, id string) journal.Entry {
	return journal.Entry{Kind: kind, Envelope: event.Envelope{ID: id, Type: "test.record"}}
}

// configuration is the configuration input a run opens with.
var configuration = record(journal.KindInput, "configuration")

// logOf is a recovered log under h holding entries.
func logOf(h journal.LogHeader, entries ...journal.Entry) journal.Recovery {
	return journal.Recovery{Header: h, Entries: entries}
}

// TestALogDirectoryIsClassifiedIntoRunsAttemptsAndEmptyStarts: a log
// holding no committed record is an abandoned start; a log whose committed
// records are a prefix of an older log's under the same header is a resume
// of that log abandoned partway (ADR 0027 §6), attributed to the newest run
// it is a prefix of and never to another attempt; every other log is a run,
// a start cut short after its configuration included.
func TestALogDirectoryIsClassifiedIntoRunsAttemptsAndEmptyStarts(t *testing.T) {
	decision := record(journal.KindDecision, "decision")
	names := []string{"run-1.log", "empty.log", "run-2.log", "attempt-a.log", "attempt-b.log", "other-build.log", "kind.log", "envelope.log", "cut-short.log"}
	recovered := []journal.Recovery{
		logOf(header, configuration, record(journal.KindInput, "bar")),
		logOf(header),
		// A resume of run-1 that ran on: run-1's records, then more.
		logOf(header, configuration, record(journal.KindInput, "bar"), decision),
		// A prefix of run-1 and run-2: attributed to run-2, the newest.
		logOf(header, configuration, record(journal.KindInput, "bar")),
		// A prefix of attempt-a too, but attempt-a is no run.
		logOf(header, configuration),
		// The same records under another build's header copy nothing.
		logOf(otherHeader, configuration),
		// The same envelope under another kind copies nothing.
		logOf(header, record(journal.KindDecision, "configuration")),
		// Another envelope at the same position copies nothing.
		logOf(header, record(journal.KindInput, "another configuration")),
		// Longer than every older log it shares a start with.
		logOf(header, configuration, record(journal.KindInput, "bar"), decision, decision),
	}
	got := restart.Classify(names, recovered)
	if want := []string{"run-1.log", "run-2.log", "other-build.log", "kind.log", "envelope.log", "cut-short.log"}; !slices.Equal(got.Runs, want) {
		t.Errorf("Runs = %v, want %v", got.Runs, want)
	}
	if want := map[string]string{"attempt-a.log": "run-2.log", "attempt-b.log": "run-2.log"}; !maps.Equal(got.Abandoned, want) {
		t.Errorf("Abandoned = %v, want %v", got.Abandoned, want)
	}
	if want := []string{"empty.log"}; !slices.Equal(got.EmptyStarts, want) {
		t.Errorf("EmptyStarts = %v, want %v", got.EmptyStarts, want)
	}
	if latest, ok := got.Latest(); !ok || latest != "cut-short.log" {
		t.Errorf("Latest() = %q, %t, want cut-short.log", latest, ok)
	}
}

// TestAnEmptyDirectoryHasNoLatestRun: with no run, a start is a first run.
func TestAnEmptyDirectoryHasNoLatestRun(t *testing.T) {
	got := restart.Classify([]string{"empty.log"}, []journal.Recovery{logOf(header)})
	if latest, ok := got.Latest(); ok || latest != "" {
		t.Errorf("Latest() = %q, %t, want none", latest, ok)
	}
}

// TestAStartCutShortHoldsNothingPastItsConfiguration: a run records its
// configuration, then its approval, one input at a time, so a log holding
// at most one record decided nothing and records no state.
func TestAStartCutShortHoldsNothingPastItsConfiguration(t *testing.T) {
	for _, c := range []struct {
		name string
		log  journal.Recovery
		want bool
	}{
		{"empty", logOf(header), true},
		{"configuration only", logOf(header, configuration), true},
		{"configuration and approval", logOf(header, configuration, record(journal.KindInput, "approval")), false},
	} {
		if got := restart.CutShort(c.log); got != c.want {
			t.Errorf("%s: CutShort = %t, want %t", c.name, got, c.want)
		}
	}
}

// TestAnAttemptAtAStartIsALogOfThatStartHoldingNothingPastIt: recorded
// under the same configuration and build, opening with the same
// configuration input, if anything; and a log opens with a start when its
// first record is that configuration input under the same header.
func TestAnAttemptAtAStartIsALogOfThatStartHoldingNothingPastIt(t *testing.T) {
	for _, c := range []struct {
		name             string
		log              journal.Recovery
		attempt, opening bool
	}{
		{"empty", logOf(header), true, false},
		{"configuration only", logOf(header, configuration), true, true},
		{"another build", logOf(otherHeader, configuration), false, false},
		{"another configuration", logOf(header, record(journal.KindInput, "another")), false, false},
		{"past its configuration", logOf(header, configuration, record(journal.KindInput, "approval")), false, true},
	} {
		if got := restart.AttemptAtStart(c.log, header, configuration.Envelope); got != c.attempt {
			t.Errorf("%s: AttemptAtStart = %t, want %t", c.name, got, c.attempt)
		}
		if got := restart.OpensWith(c.log, header, configuration.Envelope); got != c.opening {
			t.Errorf("%s: OpensWith = %t, want %t", c.name, got, c.opening)
		}
	}
}

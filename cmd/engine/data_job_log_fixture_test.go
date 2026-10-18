package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

var updateDataJobLog = flag.Bool("update-data-job-log", false,
	"rewrite adapter/marketdata/tests/testdata/boost-fixture.log from the engine's own writer")

// dataJobLogFixture is the durable log the daily data job's reader is
// tested against (adapter/marketdata/tests/test_tidata.py): the file name,
// without ".log", is the run's name.
var dataJobLogFixture = filepath.Join("..", "..", "adapter", "marketdata", "tests", "testdata", "boost-fixture.log")

// memoryLog is the SyncWriter the fixture is written through.
type memoryLog struct{ bytes.Buffer }

func (*memoryLog) Sync() error { return nil }

// TestTheDataJobsEngineLogFixtureIsTheLogTheEngineWrites pins the durable
// log the daily data job reads its run's identity and configuration from
// (ADR 0027) to what this command writes at a Boost start: composed from the
// shipped Boost configuration, the header CreateLog writes and the
// configuration input as the first group. A change to the log's header,
// record or commit layout, or to the configuration input, fails here; the
// fixture is then regenerated with -update-data-job-log and the data job's
// tests read the new one, so the Python reader is never checked only against
// a log it fabricated itself.
func TestTheDataJobsEngineLogFixtureIsTheLogTheEngineWrites(t *testing.T) {
	comp, err := compose(filepath.Join("..", "..", "deploy", "etc", "engines", "boost.config"), "fixture",
		time.Date(2026, 3, 13, 21, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	out := &memoryLog{}
	log, err := journal.CreateLog(out, logHeader(comp))
	if err != nil {
		t.Fatalf("CreateLog: %v", err)
	}
	if err := log.Append([]journal.Entry{{Kind: journal.KindInput, Envelope: comp.configEnvelope}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if *updateDataJobLog {
		if err := os.WriteFile(dataJobLogFixture, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(dataJobLogFixture)
	if err != nil {
		t.Fatalf("read the fixture (write it with -update-data-job-log): %v", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("the engine writes\n%s\nwant the fixture's bytes (regenerate with -update-data-job-log)\n%s", out.Bytes(), want)
	}
	if _, err := journal.RecoverLog(bytes.NewReader(want)); err != nil {
		t.Fatalf("RecoverLog: %v", err)
	}
}

package registry_test

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
)

type researchStore struct {
	fs          fstest.MapFS
	failDir     string
	failRead    bool
	missingRoot bool
	listings    int
}

func (s *researchStore) ReadDir(dir string) ([]string, error) {
	s.listings++
	if dir == "." && s.missingRoot {
		return nil, fs.ErrNotExist
	}
	if dir == s.failDir || (s.failDir == "second" && s.listings == 3) {
		return nil, fs.ErrPermission
	}
	entries, err := fs.ReadDir(s.fs, dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}
func (s *researchStore) ReadFile(name string) ([]byte, error) {
	if s.failRead && strings.HasSuffix(name, ".opening") {
		return nil, fs.ErrPermission
	}
	return fs.ReadFile(s.fs, name)
}
func marshalResearch(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPriorOpeningsAcrossConfigurationsAndLegacyRuns(t *testing.T) {
	p := protocolForTest(t)
	run := completedRun("legacy")
	run.Variant = "v"
	entry := mustEntry(t, run)
	entryPath := mustPath(t, entry)
	run.RunID = "reserved"
	run.Configuration.MaxUnitsTotalLong++
	opening, err := p.Opening(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := registry.Dir(opening.ConfigurationHash)
	if err != nil {
		t.Fatal(err)
	}
	store := &researchStore{fs: fstest.MapFS{
		entryPath:                 &fstest.MapFile{Data: encoded(t, entry)},
		dir + "/reserved.opening": &fstest.MapFile{Data: marshalResearch(t, opening)},
		dir + "/note.txt":         &fstest.MapFile{Data: []byte("note")},
		"README":                  &fstest.MapFile{Data: []byte("note")},
	}}
	prior, err := p.PriorOpenings(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior) != 2 {
		t.Fatalf("prior %+v", prior)
	}
	run.RunID = "third"
	next, err := p.Opening(run, prior)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Repeat || len(next.Prior) != 2 {
		t.Fatalf("next %+v", next)
	}
	// A completed run and its reservation are one exposure, not two.
	prior = append(prior, prior[0])
	next, err = p.Opening(run, prior)
	if err != nil || len(next.Prior) != 2 {
		t.Fatalf("dedup %+v, %v", next, err)
	}
}

// TestPriorOpeningsSkipsRunsThisProtocolAlreadyReportedOn: a run this
// protocol reported on states its own exposure through its own .opening
// sidecar, if it reserved one at all -- never through the span-based guess
// legacy entries fall back to. A run recorded with a mixed span but a
// .report and no .opening (an in-sample run, a refused -fit, or any other
// attempt that reserved nothing) must not be re-guessed into exposure it
// never took (ADR 0012, Proposed amendment).
func TestPriorOpeningsSkipsRunsThisProtocolAlreadyReportedOn(t *testing.T) {
	p := protocolForTest(t)
	run := completedRun("reported")
	run.Variant = "v"
	entry := mustEntry(t, run)
	entryPath := mustPath(t, entry)
	dir, err := registry.Dir(entry.ConfigurationHash)
	if err != nil {
		t.Fatal(err)
	}
	store := &researchStore{fs: fstest.MapFS{
		entryPath:                &fstest.MapFile{Data: encoded(t, entry)},
		dir + "/reported.report": &fstest.MapFile{Data: []byte("{}")},
	}}
	prior, err := p.PriorOpenings(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior) != 0 {
		t.Fatalf("a run this protocol already reported on must not be re-guessed from its span: %+v", prior)
	}
}

func TestPriorOpeningsFailures(t *testing.T) {
	p := protocolForTest(t)
	run := completedRun("first")
	run.Variant = "v"
	o, err := p.Opening(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := registry.Dir(o.ConfigurationHash)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		store *researchStore
		bad   bool
	}{
		{"empty root", &researchStore{}, false},
		{"missing registry directory", &researchStore{missingRoot: true}, false},
		{"root unreadable", &researchStore{failDir: "."}, true},
		{"runs unreadable", &researchStore{fs: fstest.MapFS{dir + "/first.opening": &fstest.MapFile{}}, failDir: dir}, true},
		{"second listing fails", &researchStore{fs: fstest.MapFS{dir + "/first.opening": &fstest.MapFile{}}, failDir: "second"}, true},
		{"opening unreadable", &researchStore{fs: fstest.MapFS{dir + "/first.opening": &fstest.MapFile{}}, failRead: true}, true},
		{"opening malformed", &researchStore{fs: fstest.MapFS{dir + "/first.opening": &fstest.MapFile{Data: []byte("bad")}}}, true},
		{"opening misplaced", &researchStore{fs: fstest.MapFS{dir + "/wrong.opening": &fstest.MapFile{Data: marshalResearch(t, o)}}}, true},
		{"note", &researchStore{fs: fstest.MapFS{"note": &fstest.MapFile{}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior, err := p.PriorOpenings(tc.store)
			if (err != nil) != tc.bad {
				t.Fatalf("error %v", err)
			}
			// A registry root that has never been created is an empty
			// history, not a failure to read one (ADR 0012): no run has
			// executed there yet, so nothing can have been opened.
			if tc.store.missingRoot && (prior != nil || err != nil) {
				t.Fatalf("prior %+v err %v", prior, err)
			}
		})
	}
}

func TestEquityCurveUsesAsOfAndRefusesUnsupportedEvidence(t *testing.T) {
	at := date("2015-12-31T23:59:59Z")
	snap := event.AccountSnapshotPayload{AsOf: at, Equity: 100, AvailableCash: 100, Currency: "USD"}
	valid := journal.Entry{Kind: journal.KindInput, Envelope: event.Envelope{Type: event.AccountSnapshotEventType, SchemaVersion: event.AccountSnapshotSchemaVersion, EventTime: at.Add(24 * time.Hour), Payload: marshalResearch(t, snap)}}
	pts, err := registry.EquityCurve([]journal.Entry{{Kind: journal.KindDecision}, valid})
	if err != nil || len(pts) != 1 || pts[0].At != at {
		t.Fatalf("curve %+v error %v", pts, err)
	}
	for _, mutate := range []func(*journal.Entry){
		func(e *journal.Entry) { e.Envelope.Type = event.CashMovementEventType },
		func(e *journal.Entry) { e.Envelope.SchemaVersion++ },
		func(e *journal.Entry) { e.Envelope.Payload = []byte("bad") },
		func(e *journal.Entry) { snap.Equity = -1; e.Envelope.Payload = marshalResearch(t, snap) },
	} {
		e := valid
		mutate(&e)
		if _, err := registry.EquityCurve([]journal.Entry{e}); err == nil {
			t.Fatal("accepted unsupported curve")
		}
	}
	if pts, err := registry.EquityCurve(nil); err != nil || len(pts) != 0 {
		t.Fatal(pts, err)
	}
}

package maintenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 28, 19, 0, 0, 0, time.UTC)

// marker is a marker file's content as ti-maintenance writes it.
func marker(opened, until time.Time) []byte {
	return []byte(`{"id":"0123456789abcdef","opened":"` + opened.UTC().Format("2006-01-02T15:04:05Z") + `","until":"` + until.UTC().Format("2006-01-02T15:04:05Z") +
		`","opened_unix":` + strconv.FormatInt(opened.Unix(), 10) + `,"until_unix":` + strconv.FormatInt(until.Unix(), 10) + "}\n")
}

func TestAMarkerWithinItsExpiryReadsOpen(t *testing.T) {
	m, open := Read(marker(t0, t0.Add(15*time.Minute)), t0.Add(time.Minute))
	if !open || !m.Until.Equal(t0.Add(15*time.Minute)) || m.ID != "0123456789abcdef" {
		t.Fatalf("Read = %v, %v; want open until %v with its id", m, open, t0.Add(15*time.Minute))
	}
}

func TestAnExpiredMarkerReadsClosedWithoutAnyoneRemovingIt(t *testing.T) {
	raw := marker(t0, t0.Add(15*time.Minute))
	if _, open := Read(raw, t0.Add(15*time.Minute)); open {
		t.Error("a marker is open at its expiry instant; the window ends there")
	}
	if _, open := Read(raw, t0.Add(time.Hour)); open {
		t.Error("an expired marker reads open")
	}
}

func TestNoMarkerReadsClosed(t *testing.T) {
	if _, open := Read(nil, t0); open {
		t.Error("a missing marker reads open")
	}
}

// A marker nobody can trust never holds an alert: the window fails closed
// (ADR 0019, "Alerting": a fault must not silence the owner).
func TestAMarkerThatCannotBeTrustedReadsClosed(t *testing.T) {
	for name, raw := range map[string][]byte{
		"garbage":              []byte("not json"),
		"no expiry":            []byte(`{"id":"0123456789abcdef","opened":"2026-10-28T19:00:00Z"}`),
		"over the maximum":     marker(t0, t0.Add(Max+time.Second)),
		"far future":           marker(t0, t0.Add(48*time.Hour)),
		"ends before start":    marker(t0, t0.Add(-time.Minute)),
		"opened in the future": marker(t0.Add(time.Hour), t0.Add(time.Hour+time.Minute)),
	} {
		if _, open := Read(raw, t0.Add(time.Second)); open {
			t.Errorf("%s: the marker reads open", name)
		}
	}
}

func TestTheLongestWindowIsThirtyMinutesAndTheDefaultFifteen(t *testing.T) {
	if Max != 30*time.Minute || Default != 15*time.Minute {
		t.Errorf("Max, Default = %v, %v; want 30m, 15m", Max, Default)
	}
	if _, open := Read(marker(t0, t0.Add(Max)), t0.Add(time.Second)); !open {
		t.Error("a window of exactly the maximum reads closed")
	}
}

// The reader and ti-maintenance status judge the same files the same way:
// deploy/rehearsal/test-maintenance.sh runs the script over this folder too.
// A file named open-* is a window open at 2026-10-28T19:00:00Z, closed-* is not.
func TestTheSharedFixturesAreJudgedAsTheirNamesSay(t *testing.T) {
	files, err := filepath.Glob("testdata/markers/*.json")
	if err != nil || len(files) < 20 {
		t.Fatalf("fixtures: %v, %v", files, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		_, open := Read(raw, t0)
		if want := strings.HasPrefix(filepath.Base(f), "open-"); open != want {
			t.Errorf("%s: open = %v, want %v", filepath.Base(f), open, want)
		}
	}
}

// The writer is deploy/maintenance.sh: what it writes is what the reporter
// reads, and `end` closes it.
func TestWhatTheScriptWritesReadsOpenAndEndClosesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maintenance.json")
	read := func() []byte {
		raw, _ := os.ReadFile(path)
		return raw
	}
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("bash", append([]string{"../../deploy/maintenance.sh"}, args...)...)
		c.Env = append(os.Environ(), "PATH="+flockPath(), "TI_MAINTENANCE_FILE="+path, "TI_MAINTENANCE_NO_ROOT=1", "TI_MAINTENANCE_NOW="+strconv.FormatInt(t0.Unix(), 10))
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("ti-maintenance %v: %v\n%s", args, err, out)
		}
	}
	run("start", "--replace", "10m")
	m, open := Read(read(), t0.Add(time.Minute))
	if !open || !m.Until.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("Read = %v, %v; want open until %v", m, open, t0.Add(10*time.Minute))
	}
	if _, open := Read(read(), t0.Add(11*time.Minute)); open {
		t.Error("the script's marker is open past its expiry")
	}
	run("start", "--replace")
	if m, _ := Read(read(), t0); !m.Until.Equal(t0.Add(Default)) {
		t.Errorf("the default window ends %v, want %v", m.Until, t0.Add(Default))
	}
	run("end")
	if _, open := Read(read(), t0); open {
		t.Error("the marker is open after end")
	}
}

// flockPath is PATH with the stand-in flock added where the machine has none.
func flockPath() string {
	if _, err := exec.LookPath("flock"); err == nil {
		return os.Getenv("PATH")
	}
	return "../../deploy/rehearsal/stubs:" + os.Getenv("PATH")
}

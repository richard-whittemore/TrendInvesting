package maintenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 28, 19, 0, 0, 0, time.UTC)

// write returns body as the file's content, for Open to judge.
func write(_ *testing.T, body string) []byte { return []byte(body) }

// Open judges content at now.
func Open(raw []byte, now time.Time) (Marker, bool) {
	m, open, _ := Read(raw, now)
	return m, open
}

func marker(opened, until time.Time) string {
	return `{"opened":"` + opened.Format(time.RFC3339) + `","until":"` + until.Format(time.RFC3339) + `"}`
}

func TestAMarkerWithinItsExpiryReadsOpen(t *testing.T) {
	raw := write(t, marker(t0, t0.Add(15*time.Minute)))
	m, open := Open(raw, t0.Add(time.Minute))
	if !open || !m.Until.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("Open = %v, %v; want open until %v", m, open, t0.Add(15*time.Minute))
	}
}

func TestAnExpiredMarkerReadsClosedWithoutAnyoneRemovingIt(t *testing.T) {
	raw := write(t, marker(t0, t0.Add(15*time.Minute)))
	if _, open := Open(raw, t0.Add(15*time.Minute)); open {
		t.Error("a marker is open at its expiry instant; the window ends there")
	}
	if _, open := Open(raw, t0.Add(time.Hour)); open {
		t.Error("an expired marker reads open")
	}
}

// A marker past its expiry is dead whatever its span, so the caller can
// remove it.
func TestAMarkerPastItsExpiryIsReportedExpired(t *testing.T) {
	for name, body := range map[string]string{
		"within the maximum": marker(t0, t0.Add(15*time.Minute)),
		"over the maximum":   marker(t0, t0.Add(Max+time.Hour)),
	} {
		_, open, expired := Read(write(t, body), t0.Add(3*time.Hour))
		if open || !expired {
			t.Errorf("%s: open %v, expired %v; want closed and expired", name, open, expired)
		}
	}
	if _, _, expired := Read(write(t, marker(t0, t0.Add(15*time.Minute))), t0.Add(time.Minute)); expired {
		t.Error("a marker within its window is reported expired")
	}
	if _, _, expired := Read(write(t, "not json"), t0); expired {
		t.Error("garbage is reported expired")
	}
}

func TestNoMarkerReadsClosed(t *testing.T) {
	if _, open := Open(nil, t0); open {
		t.Error("a missing marker reads open")
	}
}

// A marker nobody can trust never holds an alert: the window fails closed
// (ADR 0019, "Alerting": a fault must not silence the owner).
func TestAMarkerThatCannotBeTrustedReadsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"garbage":              "not json",
		"empty":                "",
		"no expiry":            `{"opened":"2026-10-28T19:00:00Z"}`,
		"no opening":           `{"until":"2026-10-28T19:10:00Z"}`,
		"over the maximum":     marker(t0, t0.Add(Max+time.Second)),
		"far future":           marker(t0, t0.Add(48*time.Hour)),
		"ends before start":    marker(t0, t0.Add(-time.Minute)),
		"opened in the future": marker(t0.Add(time.Hour), t0.Add(time.Hour+time.Minute)),
	} {
		t.Run(name, func(t *testing.T) {
			raw := write(t, body)
			if _, open := Open(raw, t0.Add(time.Second)); open {
				t.Errorf("%s: the marker reads open", name)
			}
		})
	}
}

func TestTheLongestWindowIsThirtyMinutesAndTheDefaultFifteen(t *testing.T) {
	if Max != 30*time.Minute || Default != 15*time.Minute {
		t.Errorf("Max, Default = %v, %v; want 30m, 15m", Max, Default)
	}
	raw := write(t, marker(t0, t0.Add(Max)))
	if _, open := Open(raw, t0.Add(time.Second)); !open {
		t.Error("a window of exactly the maximum reads closed")
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
		c.Env = append(os.Environ(), "TI_MAINTENANCE_FILE="+path, "TI_MAINTENANCE_NO_ROOT=1", "TI_MAINTENANCE_NOW="+strconv.FormatInt(t0.Unix(), 10))
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("ti-maintenance %v: %v\n%s", args, err, out)
		}
	}
	run("start", "10m")
	m, open := Open(read(), t0.Add(time.Minute))
	if !open || !m.Until.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("Open = %v, %v; want open until %v", m, open, t0.Add(10*time.Minute))
	}
	if _, open := Open(read(), t0.Add(11*time.Minute)); open {
		t.Error("the script's marker is open past its expiry")
	}
	run("start")
	if m, _ := Open(read(), t0); !m.Until.Equal(t0.Add(Default)) {
		t.Errorf("the default window ends %v, want %v", m.Until, t0.Add(Default))
	}
	run("end")
	if _, open := Open(read(), t0); open {
		t.Error("the marker is open after end")
	}
}

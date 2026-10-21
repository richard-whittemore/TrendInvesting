package journalquery

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// entry is a directory entry whose Info answers err.
type entry struct {
	name string
	mode fs.FileMode
	err  error
}

func (e entry) Name() string               { return e.name }
func (e entry) IsDir() bool                { return e.mode.IsDir() }
func (e entry) Type() fs.FileMode          { return e.mode.Type() }
func (e entry) Info() (fs.FileInfo, error) { return nil, e.err }

// TestListLogsSkipsOnlyAVanishedLog: the engine's listing, shared with the
// dashboard, lists the regular files with the suffix oldest first, skips a
// log that vanished while listed and refuses one that cannot be examined
// for any other reason, so the latest run is never passed over.
func TestListLogsSkipsOnlyAVanishedLog(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	for i, name := range []string{"b.log", "a.log", "notes.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at.Add(time.Duration(-i)*time.Hour), at.Add(time.Duration(-i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "quarantine.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	logs, err := ListLogs(dir, ".log")
	if err != nil || len(logs) != 2 || logs[0].Name != "a.log" || logs[1].Name != "b.log" {
		t.Fatalf("logs = %v, err = %v; want a.log then b.log", logs, err)
	}
	if _, err := ListLogs(filepath.Join(dir, "none"), ".log"); err == nil {
		t.Fatal("a directory that cannot be listed must refuse")
	}
	if logs, err := listed(dir, ".log", []fs.DirEntry{entry{name: "gone.log", err: fs.ErrNotExist}}); err != nil || len(logs) != 0 {
		t.Fatalf("a vanished log: logs = %v, err = %v; want it skipped", logs, err)
	}
	if _, err := listed(dir, ".log", []fs.DirEntry{entry{name: "x.log", err: fs.ErrPermission}}); err == nil || !strings.Contains(err.Error(), "x.log") {
		t.Fatalf("a log that cannot be examined: err = %v, want a refusal naming it", err)
	}
}

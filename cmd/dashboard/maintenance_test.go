package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The page says "Maintenance until ..." while the host's maintenance window
// is open, and says nothing otherwise (internal/maintenance).

func TestTheMarkerPathIsTheReportsDirectoryBesideTheEnginesFolder(t *testing.T) {
	got := maintenanceFileFor([]string{"/var/lib/trend-investing/engines/boost"})
	if got != "/var/lib/trend-investing/reports/maintenance.json" {
		t.Errorf("marker path = %q", got)
	}
	if maintenanceFileFor(nil) != "" {
		t.Error("a dashboard with no engine directory has no marker")
	}
}

func TestMaintenanceNoteNamesTheEndOfAnOpenWindowOnly(t *testing.T) {
	at := time.Date(2026, 10, 28, 19, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "maintenance.json")
	if got := maintenanceNote(path, at); got != "" {
		t.Errorf("note with no marker = %q", got)
	}
	body := `{"opened":"2026-10-28T18:59:00Z","until":"2026-10-28T19:10:00Z","opened_unix":1793213940,"until_unix":1793214600}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := maintenanceNote(path, at); got != "2026-10-28 19:10Z" {
		t.Errorf("note = %q, want the window's end in UTC", got)
	}
	if got := maintenanceNote(path, at.Add(11*time.Minute)); got != "" {
		t.Errorf("note after the expiry = %q", got)
	}
}

func TestThePageShowsMaintenanceUntilOnlyDuringAWindow(t *testing.T) {
	var with, without strings.Builder
	if err := page.Execute(&with, pageData{Maintenance: "2026-10-28 19:10Z"}); err != nil {
		t.Fatal(err)
	}
	if err := page.Execute(&without, pageData{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with.String(), "Maintenance until 2026-10-28 19:10Z") {
		t.Errorf("the page lacks the maintenance line:\n%s", with.String())
	}
	if strings.Contains(without.String(), "Maintenance until") {
		t.Error("the page shows a window when none is open")
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discoverSource(t *testing.T, source string) (map[string]uint32, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "events.go"), []byte("package event\n"+source), 0600); err != nil {
		t.Fatal(err)
	}
	return discoverEventTypes(dir)
}

func TestFractionalVersionsAreDiscoveredSeparately(t *testing.T) {
	got, err := discoverSource(t, `
const FillEventType = "execution.fill"
const FillSchemaVersion = 4
const FillTargetWeightFractionalVersion = 6
const OrderSkippedEventType = "strategy.order.skipped"
const OrderSkippedSchemaVersion = 1
const OrderSkippedFractionalSchemaVersion = 2
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("discovered %d entries, want 4", len(got))
	}
	for key, version := range map[string]uint32{
		"execution.fill": 4, "execution.fill@6": 6,
		"strategy.order.skipped": 1, "strategy.order.skipped@2": 2,
	} {
		if got[key] != version {
			t.Errorf("discovered %s = %d, want %d", key, got[key], version)
		}
	}
}

func TestUnclassifiedFractionalVersionFailsClosed(t *testing.T) {
	_, err := discoverSource(t, `const UnknownFractionalVersion = 2`)
	if err == nil || !strings.Contains(err.Error(), "UnknownFractionalVersion") {
		t.Fatalf("want unclassified fractional version error, got %v", err)
	}
}

func TestFractionalVersionBumpNeedsNewExcuse(t *testing.T) {
	got, err := discoverSource(t, `const FillTargetWeightFractionalVersion = 99`)
	if err != nil {
		t.Fatal(err)
	}
	if got["execution.fill@99"] != 99 {
		t.Fatalf("new fractional schema was not discovered: %v", got)
	}
	for key := range got {
		if _, ok := notYetCrossed[key]; ok {
			t.Fatalf("new schema %s inherited an old excuse", key)
		}
		if _, ok := wireCovered[key]; ok {
			t.Fatalf("new schema %s inherited old coverage", key)
		}
	}
}

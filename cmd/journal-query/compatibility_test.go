package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWholeShareViewsMatchMainGolden(t *testing.T) {
	var output strings.Builder
	for _, path := range []string{tradingJournal(t), ordersJournal(t), positionsJournal(t), "../../internal/targetweight/testdata/held-one-lot-short.journal.jsonl"} {
		for _, view := range []string{"orders", "fills", "refusals", "operator", "positions", "totals", "reconcile"} {
			for _, format := range []string{"text", "csv", "json"} {
				out, err := query(t, view, "-format", format, path)
				out = strings.ReplaceAll(out, path, filepath.Base(path))
				fmt.Fprintf(&output, "=== %s %s %s ===\n%sERROR: %v\n", filepath.Base(path), view, format, out, strings.ReplaceAll(fmt.Sprint(err), path, filepath.Base(path)))
			}
		}
	}
	const path = "testdata/whole-share-views.golden.txt"
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != string(want) {
		t.Fatal("whole-share view output differs from main golden")
	}
}

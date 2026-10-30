package manualaudit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func journalQueryManual(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", "journal-query.md"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(raw)), " ")
}

// TestJournalQueryManualListsFractionalSchemas guards the reader's inventory
// against the fixed-point schema families introduced by ADR 0026 D7-F.
func TestJournalQueryManualListsFractionalSchemas(t *testing.T) {
	manual := journalQueryManual(t)
	for _, want := range []string{
		"6 (fractional target-weight)", "4 (fractional venue reports)", "5 (fractional synthetic reports)",
		"5 (fractional positions)", "6 (fractional positions and cash)",
		"planned orders of versions 1, 2 and 3", "releases of versions 1 through 4", "refusals of versions 1 through 5",
		"skipped trades of versions 1 and 2",
	} {
		if !strings.Contains(manual, want) {
			t.Errorf("journal-query manual omits supported schema: %s", want)
		}
	}
}

// TestJournalQueryManualExplainsScaleCutoverAndRefusals preserves the operator's
// account of checked legacy upcasts and invalid histories (ADR 0026 D7-F).
func TestJournalQueryManualExplainsScaleCutoverAndRefusals(t *testing.T) {
	manual := journalQueryManual(t)
	for _, want := range []string{
		"legacy whole-share records are upcast to ticks with overflow checked",
		"refuse negative legacy holdings during upcast",
		"fractional record followed by a whole-share record",
		"two different nonzero scales",
		"naming the account", "before applying filters",
	} {
		if !strings.Contains(manual, want) {
			t.Errorf("journal-query manual omits scale behavior: %s", want)
		}
	}
}

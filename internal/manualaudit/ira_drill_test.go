package manualaudit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIRAGateDocumentsOwnerDecisions keeps the operational gate tied to plan.md
// section 9 and its dated owner decision, rather than elapsed calendar months.
func TestIRAGateDocumentsOwnerDecisions(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "docs", "live-trading", "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	plan := string(raw)
	for _, want := range []string{"one real month-end decided cleanly", "one rehearsed month", "2026-10-28 owner decision"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan.md lacks %q", want)
		}
	}
	if strings.Contains(plan, "2 clean dry-run rebalances") {
		t.Error("plan.md retains superseded gate")
	}
	raw, err = os.ReadFile(filepath.Join(root, "docs", "runbooks", "ira-drill-checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	if problems := iraChecklistProblems(string(raw)); len(problems) != 0 {
		t.Fatal(problems)
	}
}

// iraChecklistProblems validates the sign-off seam of ira-drill-checklist.md:
// no absent, failed or undated step may become a passed gate (plan.md section 9).
func iraChecklistProblems(doc string) []string {
	ids := []string{"paper-free", "no-borrow", "fractional", "market-data", "same-day", "gateway-restart", "logged-out-page", "withheld-page", "restart-hold", "clearance", "missed-recovery", "real-month", "rehearsed-month", "owner-signoff"}
	var problems []string
	passed := false
	statuses := 0
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "Gate status:") {
			statuses++
			switch line {
			case "Gate status: PASS":
				passed = true
			case "Gate status: PENDING":
			default:
				problems = append(problems, "invalid gate status")
			}
		}
	}
	if statuses != 1 {
		problems = append(problems, "expected one gate status")
	}
	for _, id := range ids {
		found := false
		for _, line := range strings.Split(doc, "\n") {
			cells := strings.Split(line, "|")
			if len(cells) != 7 || strings.TrimSpace(cells[1]) != id {
				continue
			}
			if found {
				problems = append(problems, "duplicate step "+id)
			}
			found = true
			date, outcome := strings.TrimSpace(cells[3]), strings.TrimSpace(cells[4])
			_, dateErr := time.Parse(time.DateOnly, date)
			if passed && (dateErr != nil || outcome != "PASS" || strings.TrimSpace(cells[5]) == "") {
				problems = append(problems, "unpassed or undated step "+id)
			}
		}
		if !found {
			problems = append(problems, "missing step "+id)
		}
	}
	return problems
}

func TestIRAChecklistRefusesIncompleteSignoff(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "runbooks", "ira-drill-checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if len(iraChecklistProblems(strings.Replace(doc, "Gate status: PENDING", "Gate status: PASS", 1))) == 0 {
		t.Fatal("undated checklist passed")
	}
	if len(iraChecklistProblems(strings.Replace(doc, "| paper-free |", "| removed |", 1))) == 0 {
		t.Fatal("missing step passed")
	}
}

func TestIRAChecklistRejectsBadDatesAndMissingEvidence(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "runbooks", "ira-drill-checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(raw), "| | | |", "| 2026-10-30 | PASS | private evidence |")
	doc = strings.Replace(doc, "Gate status: PENDING", "Gate status: PASS", 1)
	if problems := iraChecklistProblems(doc); len(problems) != 0 {
		t.Fatal(problems)
	}
	for name, bad := range map[string]string{
		"invalid date":   strings.Replace(doc, "2026-10-30", "2026-99-99", 1),
		"no evidence":    strings.Replace(doc, "| private evidence |", "| |", 1),
		"failed row":     strings.Replace(doc, "| PASS |", "| FAIL |", 1),
		"unknown status": strings.Replace(doc, "Gate status: PASS", "Gate status: PASSED", 1),
	} {
		if len(iraChecklistProblems(bad)) == 0 {
			t.Errorf("%s passed", name)
		}
	}
}

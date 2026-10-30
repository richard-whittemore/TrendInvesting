package manualaudit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIRAGateDocumentsOwnerDecisions checks the dated owner decision and each
// installation's retained evidence (CONTEXT.md: "IRA dry-run gate").
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
	context, err := os.ReadFile(filepath.Join(root, "CONTEXT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(context), "**IRA dry-run gate**:") {
		t.Error("CONTEXT.md lacks the IRA dry-run gate rule cited by the checklist audit")
	}
	if problems := iraChecklistsProblems(filepath.Join(root, "docs", "runbooks")); len(problems) != 0 {
		t.Fatal(problems)
	}
}

// iraChecklistsProblems audits the template and each installation's retained
// checklist, as required by CONTEXT.md: "IRA dry-run gate".
func iraChecklistsProblems(folder string) []string {
	paths, err := filepath.Glob(filepath.Join(folder, "ira-drill-checklist*.md"))
	if err != nil || len(paths) == 0 {
		return []string{"missing IRA checklists"}
	}
	var problems []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, filepath.Base(path)+": cannot read checklist")
			continue
		}
		for _, problem := range iraChecklistProblems(string(raw)) {
			problems = append(problems, filepath.Base(path)+": "+problem)
		}
	}
	return problems
}

// iraChecklistProblems validates all dated attempts and the owner's sign-off
// (CONTEXT.md: "IRA dry-run gate"). Each latest attempt must pass, and sign-off
// must cover every latest attempt; earlier evidence remains in the checklist.
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
	latestDates := make(map[string]string, len(ids))
	for _, id := range ids {
		found := false
		lastDate, lastOutcome := "", ""
		for _, line := range strings.Split(doc, "\n") {
			attempt := strings.TrimPrefix(strings.TrimSpace(line), "|")
			if strings.TrimSpace(strings.SplitN(attempt, "|", 2)[0]) != id {
				continue
			}
			cells := strings.Split(line, "|")
			if len(cells) != 7 || strings.TrimSpace(cells[0]) != "" || strings.TrimSpace(cells[6]) != "" {
				problems = append(problems, "invalid attempt "+id)
				continue
			}
			date, outcome, evidence := strings.TrimSpace(cells[3]), strings.TrimSpace(cells[4]), strings.TrimSpace(cells[5])
			_, dateErr := time.Parse(time.DateOnly, date)
			blank := date == "" && outcome == "" && evidence == ""
			if !blank && (dateErr != nil || (outcome != "PASS" && outcome != "FAIL") || evidence == "") {
				problems = append(problems, "invalid attempt "+id)
			}
			if found && (blank || lastOutcome == "" || date < lastDate) {
				problems = append(problems, "undated or out-of-order attempt "+id)
			}
			found = true
			lastDate, lastOutcome = date, outcome
		}
		latestDates[id] = lastDate
		if !found {
			problems = append(problems, "missing step "+id)
		} else if passed && lastOutcome != "PASS" {
			problems = append(problems, "latest attempt has not passed "+id)
		}
	}
	if passed {
		for _, id := range ids {
			if latestDates["owner-signoff"] < latestDates[id] {
				problems = append(problems, "owner-signoff predates "+id)
			}
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

func TestIRAChecklistRetainsDatedAttempts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "runbooks", "ira-drill-checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(raw), "| | | |", "| 2026-10-30 | PASS | private evidence |")
	doc = strings.Replace(doc, "Gate status: PENDING", "Gate status: PASS", 1)
	failed := "| restart-hold | Earlier failed attempt | 2026-10-29 | FAIL | retained failure evidence |\n"
	doc = strings.Replace(doc, "| restart-hold |", failed+"| restart-hold |", 1)
	if problems := iraChecklistProblems(doc); len(problems) != 0 {
		t.Fatalf("failed attempt followed by dated pass must remain auditable: %v", problems)
	}
	for name, bad := range map[string]string{
		"latest failure":           doc + "\n| restart-hold | Retry failed | 2026-10-31 | FAIL | retained evidence |\n",
		"undated failure":          strings.Replace(doc, "| 2026-10-29 |", "| |", 1),
		"missing failure evidence": strings.Replace(doc, "retained failure evidence", "", 1),
		"out of order":             strings.Replace(doc, "| 2026-10-29 |", "| 2026-10-31 |", 1),
	} {
		if len(iraChecklistProblems(bad)) == 0 {
			t.Errorf("%s passed", name)
		}
	}
}

func TestIRAChecklistAuditsInstallationCopies(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "runbooks", "ira-drill-checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	for name, doc := range map[string]string{
		"ira-drill-checklist.md":          string(raw),
		"ira-drill-checklist-trad-ira.md": string(raw),
		"ira-drill-checklist-roth-ira.md": strings.Replace(string(raw), "Gate status: PENDING", "Gate status: PASS", 1),
	} {
		if err := os.WriteFile(filepath.Join(folder, name), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	problems := iraChecklistsProblems(folder)
	if len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), "ira-drill-checklist-roth-ira.md") {
		t.Fatalf("invalid second installation escaped audit: %v", problems)
	}
}

func TestIRAChecklistRejectsMalformedRetry(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "runbooks", "ira-drill-checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(raw), "| | | |", "| 2026-10-30 | PASS | private evidence |")
	doc = strings.Replace(doc, "Gate status: PENDING", "Gate status: PASS", 1)
	for name, retry := range map[string]string{
		"extra pipe":       "| rehearsed-month | Retry | 2026-10-31 | FAIL | journal output | tail |",
		"escaped pipe":     `| rehearsed-month | Retry | 2026-10-31 | FAIL | journal output \| tail |`,
		"no outer pipes":   "rehearsed-month | Retry | 2026-10-31 | FAIL | private evidence",
		"no trailing pipe": "| rehearsed-month | Retry | 2026-10-31 | FAIL | private evidence",
		"no leading pipe":  "rehearsed-month | Retry | 2026-10-31 | FAIL | private evidence |",
	} {
		t.Run(name, func(t *testing.T) {
			problems := iraChecklistProblems(doc + "\n" + retry + "\n")
			if !strings.Contains(strings.Join(problems, "\n"), "invalid attempt rehearsed-month") {
				t.Fatalf("malformed retry escaped audit: %v", problems)
			}
		})
	}
}

func TestIRAChecklistSignoffMustCoverLatestAttempts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "runbooks", "ira-drill-checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(raw), "| | | |", "| 2026-10-30 | PASS | private evidence |")
	doc = strings.Replace(doc, "Gate status: PENDING", "Gate status: PASS", 1)
	doc += "\n| rehearsed-month | Retry failed | 2026-10-31 | FAIL | retained failure |\n" +
		"| rehearsed-month | Retry passed | 2026-11-01 | PASS | retained success |\n"
	if problems := iraChecklistProblems(doc); !strings.Contains(strings.Join(problems, "\n"), "owner-signoff predates rehearsed-month") {
		t.Fatalf("sign-off predating retry escaped audit: %v", problems)
	}
	for _, date := range []string{"2026-11-01", "2026-11-02"} {
		current := doc + "| owner-signoff | Signed latest evidence | " + date + " | PASS | owner signature |\n"
		if problems := iraChecklistProblems(current); len(problems) != 0 {
			t.Fatalf("current sign-off rejected: %v", problems)
		}
	}
	if problems := iraChecklistProblems(strings.Replace(doc, "Gate status: PASS", "Gate status: PENDING", 1)); len(problems) != 0 {
		t.Fatalf("pending gate must retain earlier sign-off: %v", problems)
	}
}

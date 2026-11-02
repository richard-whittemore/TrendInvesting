package manualaudit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMomentumManualCashReadingMatchesIRAConfigs keeps the owner's buy-budget
// explanation tied to the shipped D5 choice (ADR 0026, point 2, "Budget").
// No Borrow Margin uses settled cash: unsettled sale proceeds are not spent.
func TestMomentumManualCashReadingMatchesIRAConfigs(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "docs", "manual", "momentum.md"))
	if err != nil {
		t.Fatal(err)
	}
	manual := strings.Join(strings.Fields(string(raw)), " ")
	_, budget, ok := strings.Cut(manual, "8. **The buy budget**")
	if !ok {
		t.Fatal("momentum manual lacks buy-budget step 8")
	}
	budget, _, ok = strings.Cut(budget, "9. **Each buy is costed")
	if !ok {
		t.Fatal("momentum manual lacks the end of buy-budget step 8")
	}
	for _, name := range []string{"trad-ira.config", "roth-ira.config.example"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, "deploy", "etc", "engines", name))
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Execution struct {
					CashRule string `json:"cash_rule"`
				} `json:"execution"`
			}
			if err := json.Unmarshal(raw, &config); err != nil {
				t.Fatal(err)
			}
			if config.Execution.CashRule == "" {
				t.Fatal("IRA config lacks execution.cash_rule")
			}
			want := fmt.Sprintf("`execution.cash_rule` is %#q", config.Execution.CashRule)
			if !strings.Contains(budget, want) {
				t.Errorf("buy-budget step must match %s: missing %q", name, want)
			}
		})
	}
	// Compare words, not line breaks, so rewrapping the step does not fail it.
	flat := strings.Join(strings.Fields(budget), " ")
	for _, want := range []string{
		"account's settled cash",
		"No Borrow Margin",
		"Unsettled sale proceeds are therefore not spent",
		"decision D5 chose `total-cash`",
		"no later buy phase spends them",
		"ADR 0026, point 2",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("buy-budget step lacks %q", want)
		}
	}
	if strings.Contains(budget, "account's total cash") {
		t.Error("buy-budget step still promises total cash for the shipped IRA configs")
	}
}

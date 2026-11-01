package manualaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPagingRuleCommentsCiteADR(t *testing.T) {
	for file, names := range map[string][]string{"internal/report/paging.go": {"GatewayResetWaitAlert"}, "cmd/reporter/gatewayjournal.go": {"resetNightEnd"}, "cmd/reporter/receipts.go": {"keepReceipt", "queueReceiptCancellations", "retryReceiptCancellations", "discardUnscopedCancellations"}} {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot(t), file), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && fn.Name.Name == name && !strings.Contains(fn.Doc.Text(), "ADR 0019") {
					t.Errorf("%s: %s lacks rule provenance ADR 0019", file, name)
				}
			}
		}
	}
}

func TestPagingManualSessionAndCancellationContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", "alerts-and-dashboard.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"a page only when it makes sense", "orchestrator acts on operational alerts first", "default `session`", "configured and available", "explicit maintenance hold", "only the receipts", "new incident"} {
		if !strings.Contains(string(raw), phrase) {
			t.Errorf("manual missing paging contract: %s", phrase)
		}
	}
	raw, err = os.ReadFile(filepath.Join(repoRoot(t), "deploy", "etc", "reporter.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "TI_ESCALATE_AFTER=session\n") {
		t.Error("shipped paging default is not session")
	}
}

func TestPagingServerManualUsesSessionDefault(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", "server.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "default 30m") || !strings.Contains(string(raw), "default `session`") {
		t.Error("server manual still promises duration default instead of session")
	}
}

func TestPagingManualReceiptClassAndChapterIndex(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", "alerts-and-dashboard.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, phrase := range []string{
		"On entry to a quiet IBKR window, the reporter cancels only outstanding Pushover emergencies that were operational when sent; on recovery, it cancels all outstanding emergencies for that incident.",
		"The class is recorded with each receipt at send time; a capital-safety receipt is never cancelled except on recovery.",
		"| `<host>: IB Gateway is logged out` (the server's name) | Class: operational. urgent, reminded hourly once escalated |",
		"| `<host>: <unit> failed` (the server's name and the failed unit's, such as `ti-adapter-boost.service`) | Class: operational; capital-safety for an engine, adapter or Gateway failure near a session. urgent |",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("manual missing receipt/class contract: %s", phrase)
		}
	}
}

func TestPagingServerManualDescribesFilteredResetEvidence(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", "server.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, obsolete := range []string{"The newest `gateway-start:` line being", "The newest gateway-start line records"} {
		if strings.Contains(text, obsolete) {
			t.Errorf("manual still uses unfiltered reset evidence: %s", obsolete)
		}
	}
	for _, want := range []string{
		"Only matching login, reset-wait and start-state evidence participates",
		"Unrelated `gateway-start:` diagnostics do not change the state",
		"strictly newer than the newest completed-login timestamp",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("manual missing filtered reset rule: %s", want)
		}
	}
}

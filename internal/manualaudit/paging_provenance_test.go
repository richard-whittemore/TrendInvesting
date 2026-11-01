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
	for file, names := range map[string][]string{"internal/report/paging.go": {"GatewayResetWaitAlert"}, "cmd/reporter/gatewayjournal.go": {"resetNightEnd"}, "cmd/reporter/receipts.go": {"queueReceiptCancellations", "retryReceiptCancellations", "discardUnscopedCancellations"}} {
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

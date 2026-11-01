package manualaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestPagingRuleCommentsCiteADR(t *testing.T) {
	for file, names := range map[string][]string{"internal/report/paging.go": {"IBKRNotice"}, "cmd/reporter/gatewayjournal.go": {"resetNightEnd", "adapterStart"}} {
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

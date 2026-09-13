package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// composition is what the composition root is allowed to reach for in
// internal/strategy: a constructor and the rules version it stamps into
// provenance. Anything else — a level, a size, an ordering, a channel — is a
// rule, and a rule in cmd/ is a rule outside the reducer that owns it
// (AGENTS.md rule 7).
var composition = map[string]bool{
	"NewReducer":   true,
	"RulesVersion": true,
}

// TestTheCompositionRootUsesNoStrategyRules walks every non-test file under
// cmd/ and checks which identifiers it reaches for in internal/strategy.
//
// depguard enforces the package-level half of this rule in .golangci.yml
// (cmd/ may not import internal/indicator or internal/sizing at all). It
// cannot express this half: cmd/ must import internal/strategy to construct
// the reducer, so the boundary is which identifiers are used, not whether
// the package is imported.
func TestTheCompositionRootUsesNoStrategyRules(t *testing.T) {
	t.Parallel()

	const strategyPath = `"github.com/richard-whittemore/TrendInvesting/internal/strategy"`

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("locate cmd/: %v", err)
	}

	var checked int
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		checked++

		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		name := ""
		for _, imported := range file.Imports {
			if imported.Path.Value != strategyPath {
				continue
			}
			name = "strategy"
			if imported.Name != nil {
				name = imported.Name.Name
			}
		}
		if name == "" {
			return nil
		}

		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Name != name {
				return true
			}
			if !composition[selector.Sel.Name] {
				t.Errorf("%s uses strategy.%s; cmd/ composes the reducer and contains no rules", path, selector.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/: %v", err)
	}
	if checked == 0 {
		t.Fatal("no files under cmd/ were checked; the walk found nothing")
	}
}

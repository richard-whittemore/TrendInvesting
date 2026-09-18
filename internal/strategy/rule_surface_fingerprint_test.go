package strategy_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// ruleIdentifierPattern matches this codebase's own naming convention for a
// rule's declared identity (docs/development.md's Source comment standard,
// principle 2): a Rule* constant naming the rule a decision payload's Rule
// field carries, or an ADR* constant naming the ADR it cites. internal/event
// declares roughly thirty such pairs (RuleEntryChannelBreakout,
// ADREntryChannelBreakout, RuleAddLadderHalfN, ...); a rule added anywhere
// else in the module is caught the same way, by the name it is given rather
// than by a maintained list of where rules live.
//
// [A-Z0-9] after the prefix excludes RulesVersion and RulesSurfaceFingerprint
// themselves (a lower-case letter follows "Rule" in both), which must not
// feed the fingerprint they are pinned beside.
var ruleIdentifierPattern = regexp.MustCompile(`^(?:Rule|ADR)[A-Z0-9]`)

// numericRuleConstantPackages are the packages RulesSurfaceFingerprint's own
// doc comment names as carrying a Baseline rule's numeric value rather than
// its name: internal/indicator (the Wilder period), internal/sizing (the
// Drawdown Step retained fraction), and internal/strategy (the Drawdown Step
// threshold fraction) alongside RulesVersion itself.
//
// internal/event is deliberately excluded from this second sweep: every
// numeric constant it declares outside the Rule*/ADR* pairs above is a
// schema version or an enumerated wire value, already governed by its own
// explicit versioning discipline (docs/development.md's schema-change
// principle), not a trading-rule value.
var numericRuleConstantPackages = map[string]bool{
	"internal/indicator": true,
	"internal/sizing":    true,
	"internal/strategy":  true,
}

// ruleConstant is one declared constant the rule surface sweep found,
// identified by its package directory and name; Value is its declaration's
// own source text, unevaluated, so that renaming a value's own dependency
// (rather than typing a new literal) still changes what is hashed.
type ruleConstant struct {
	Package string
	Name    string
	Value   string
}

// declaredRuleSurface parses every non-test .go file in the module and
// returns the constants that make up the declared rule surface: every
// Rule*/ADR* constant anywhere, plus every numeric (int or float) literal
// constant declared in numericRuleConstantPackages. It is a sweep, not a
// maintained list, precisely so that the next rule someone declares is
// caught by the name or the package it is given rather than by whether
// anyone remembered to add it here.
func declaredRuleSurface(t *testing.T) []ruleConstant {
	t.Helper()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	var constants []ruleConstant
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "runs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		pkgDir := filepath.ToSlash(filepath.Dir(rel))

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if name.Name == "_" || i >= len(vs.Values) {
						continue
					}
					value := vs.Values[i]
					switch {
					case ruleIdentifierPattern.MatchString(name.Name):
						constants = append(constants, ruleConstant{
							Package: pkgDir,
							Name:    name.Name,
							Value:   exprText(fset, value),
						})
					case numericRuleConstantPackages[pkgDir]:
						lit, ok := value.(*ast.BasicLit)
						if !ok || (lit.Kind != token.INT && lit.Kind != token.FLOAT) {
							continue
						}
						constants = append(constants, ruleConstant{
							Package: pkgDir,
							Name:    name.Name,
							Value:   lit.Value,
						})
					}
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}

	sort.Slice(constants, func(i, j int) bool {
		if constants[i].Package != constants[j].Package {
			return constants[i].Package < constants[j].Package
		}
		return constants[i].Name < constants[j].Name
	})
	return constants
}

// exprText renders a constant's value expression as source text. Every
// Rule*/ADR* constant today is a plain string literal, but this falls back
// to the expression's own printed form rather than silently dropping a
// future one declared some other way.
func exprText(fset *token.FileSet, e ast.Expr) string {
	if lit, ok := e.(*ast.BasicLit); ok {
		return lit.Value
	}
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, e); err != nil {
		return fmt.Sprintf("<unprintable: %v>", err)
	}
	return buf.String()
}

// ruleSurfaceFingerprint hashes the declared rule surface deterministically,
// the same "build the canonical bytes, then sha256.Sum256 them" shape
// event.ConfigurationHash uses (ADR 0016): sorted first, so the result
// depends only on which rules are declared and what they are declared to
// be, never on the order the file walk visits them or the order a const
// block lists them in.
func ruleSurfaceFingerprint(t *testing.T) string {
	t.Helper()
	var canonical strings.Builder
	for _, c := range declaredRuleSurface(t) {
		fmt.Fprintf(&canonical, "%s.%s=%s\n", c.Package, c.Name, c.Value)
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return hex.EncodeToString(sum[:])
}

// TestDeclaredRuleSurfaceMatchesItsPinnedFingerprint is the invariant
// strategy.RulesVersion's own doc comment has asked for since ADR 0016's
// version bump before this one: a fingerprint pinned over the declared rule
// surface, so that a rule change which moves a Rule*, ADR*, or declared
// numeric rule constant fails this test unless RulesVersion moves with it.
//
// It does not catch every way a rule can change. A validator's predicate
// changing behaviour with no constant renamed or retyped — the case that
// motivated this test, where a Campaign exiting at a raised Protective Stop
// stopped being refused with no Rule*, ADR*, or numeric rule constant
// touched — moves nothing this sweep reads. Closing that gap needs a
// different, and harder, check; this one is honest about not being it.
func TestDeclaredRuleSurfaceMatchesItsPinnedFingerprint(t *testing.T) {
	t.Parallel()

	got := ruleSurfaceFingerprint(t)
	if got != strategy.RulesSurfaceFingerprint {
		t.Fatalf("the declared rule surface's fingerprint is %s, but strategy.RulesSurfaceFingerprint is "+
			"pinned to %s: a Rule*, ADR*, or declared numeric rule constant's name or value changed. "+
			"Revert that change, or, if the rule itself changed, bump strategy.RulesVersion and re-pin "+
			"RulesSurfaceFingerprint to %s (ADR 0016).",
			got, strategy.RulesSurfaceFingerprint, got)
	}
}

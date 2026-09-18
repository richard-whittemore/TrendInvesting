package strategy_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// Drawdown Step retained fraction and StopKind's own values), and
// internal/strategy (the Drawdown Step threshold fraction) alongside
// RulesVersion itself.
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

// predeclaredNumericTypes names every predeclared Go type a constant
// expression can be converted to that is not a string or a bool — the
// closed set isNumericExpr checks a type-conversion CallExpr's function
// identifier against (e.g. float64(1 << 53)).
var predeclaredNumericTypes = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true, "byte": true, "rune": true,
	"float32": true, "float64": true,
	"complex64": true, "complex128": true,
}

// isNumericExpr reports whether a constant's value expression can only be
// numeric, judged from the expression's own shape rather than by evaluating
// it or resolving what any identifier inside it refers to.
//
// A literal decides itself. Beyond that, Go restricts most of the relevant
// operators to numeric operands: -, *, /, %, the shifts and the bitwise
// operators are undefined for a string or a bool, so a BinaryExpr using one
// of them is numeric whatever its operands are, even an identifier this
// function cannot otherwise classify — which is exactly
// notionalAccountUndefinedDrawdownFraction's shape, a formula over two other
// rule constants with no literal of its own at the top. + is the one
// exception (string concatenation), so it falls back to asking the same
// question of its own operands. Unary -, +, and ^ (bitwise complement) are
// numeric-only for the identical reason; unary ! is not, being boolean.
// A CallExpr converting to one of Go's predeclared numeric types (e.g.
// float64(1 << 53), maxExactWholeQuantity's own shape) is numeric; any other
// call — a cross-package conversion, a built-in — is left out rather than
// guessed at. iota is always an untyped integer constant by the language
// spec regardless of what type a ConstSpec declares alongside it, which is
// how sizing.StopKind's own iota-valued constants are reached without
// resolving StopKind's underlying type at all. A bare reference to some
// OTHER constant, by contrast, cannot be classified this way, so it is left
// out rather than guessed at.
func isNumericExpr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind != token.STRING
	case *ast.Ident:
		return v.Name == "iota"
	case *ast.ParenExpr:
		return isNumericExpr(v.X)
	case *ast.UnaryExpr:
		return v.Op != token.NOT
	case *ast.BinaryExpr:
		switch v.Op {
		case token.SUB, token.MUL, token.QUO, token.REM,
			token.SHL, token.SHR, token.AND, token.OR, token.XOR, token.AND_NOT:
			return true
		case token.ADD:
			return isNumericExpr(v.X) || isNumericExpr(v.Y)
		default:
			return false
		}
	case *ast.CallExpr:
		id, ok := v.Fun.(*ast.Ident)
		return ok && predeclaredNumericTypes[id.Name]
	default:
		return false
	}
}

// ruleConstant is one declared constant the rule surface sweep found,
// identified by its package directory and name; Value is its declaration's
// own source text, unevaluated, so that renaming a value's own dependency
// (rather than typing a new literal) still changes what is hashed. Numeric
// marks an entry found through numericRuleConstantPackages rather than
// through ruleIdentifierPattern — the only entries rule_surface_exceptions.json
// may ever name (see TestRuleSurfaceExceptionsAreCurrent).
type ruleConstant struct {
	Package string
	Name    string
	Value   string
	Numeric bool
}

func (c ruleConstant) key() string { return c.Package + "\x00" + c.Name }

// declaredRuleSurface parses every non-test .go file in the current module
// and returns the constants that make up the declared rule surface: every
// Rule*/ADR* constant anywhere, plus every numeric-shaped constant declared
// in numericRuleConstantPackages (see isNumericExpr). It is a sweep, not a
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
			case ".git", "runs", "testdata", "vendor":
				return filepath.SkipDir
			}
			// A directory other than the root that declares its own go.mod
			// is a separate module: parsing into it would mix a different
			// module's identifiers into this one's rule surface, and a
			// parse error there would fail this test for a reason that has
			// nothing to do with this module's rules.
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
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
			// A ConstSpec may omit its expression list entirely, in which
			// case Go's const semantics substitute the nearest PRECEDING
			// non-empty expression list, textually, within the same block
			// (https://go.dev/ref/spec#Constant_declarations) — the shape
			// every iota-valued enum in this module uses, sizing.StopKind
			// included. lastValues carries that expression list forward so
			// such a spec is not silently skipped for having none of its
			// own.
			var lastValues []ast.Expr
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				values := vs.Values
				if len(values) == 0 {
					values = lastValues
				} else {
					lastValues = values
				}
				for i, name := range vs.Names {
					if name.Name == "_" || i >= len(values) {
						continue
					}
					value := values[i]
					switch {
					case ruleIdentifierPattern.MatchString(name.Name):
						constants = append(constants, ruleConstant{
							Package: pkgDir,
							Name:    name.Name,
							Value:   exprText(fset, value),
						})
					case numericRuleConstantPackages[pkgDir] && isNumericExpr(value):
						constants = append(constants, ruleConstant{
							Package: pkgDir,
							Name:    name.Name,
							Value:   exprText(fset, value),
							Numeric: true,
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
// future one declared some other way — the same fallback that lets the
// numeric sweep hash a formula like notionalAccountUndefinedDrawdownFraction's
// own, rather than trying to evaluate it.
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

// ruleSurfaceExceptionsFile is the checked-in list of numeric constants the
// sweep finds that are declared to NOT be trading rules, modelled on
// internal/coverageaudit/exclusions.json: same shape, same demand for a
// named reason instead of a free-text excuse in a failure message.
const ruleSurfaceExceptionsFile = "rule_surface_exceptions.json"

// ruleSurfaceException is one entry in ruleSurfaceExceptionsFile.
type ruleSurfaceException struct {
	Package string `json:"package"`
	Name    string `json:"name"`
	Reason  string `json:"reason"`
}

func (e ruleSurfaceException) key() string { return e.Package + "\x00" + e.Name }

type ruleSurfaceExceptionList struct {
	Note       string                 `json:"note"`
	Exceptions []ruleSurfaceException `json:"exceptions"`
}

// readRuleSurfaceExceptions reads and decodes ruleSurfaceExceptionsFile.
func readRuleSurfaceExceptions(t *testing.T) []ruleSurfaceException {
	t.Helper()
	raw, err := os.ReadFile(ruleSurfaceExceptionsFile)
	if err != nil {
		t.Fatalf("read %s: %v", ruleSurfaceExceptionsFile, err)
	}
	var list ruleSurfaceExceptionList
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&list); err != nil {
		t.Fatalf("decode %s: %v", ruleSurfaceExceptionsFile, err)
	}
	return list.Exceptions
}

// withoutExceptions removes every candidate the exception list names from
// the declared rule surface, leaving the remainder that RulesSurfaceFingerprint
// hashes. It does not itself judge whether an exception is well-formed,
// current, or aimed at a Rule*/ADR* identifier — TestRuleSurfaceExceptionsAreCurrent
// is the check that a listed name is still a real, current, non-rule entry;
// this function only removes whatever it is told to.
func withoutExceptions(all []ruleConstant, exceptions []ruleSurfaceException) []ruleConstant {
	excluded := make(map[string]bool, len(exceptions))
	for _, e := range exceptions {
		excluded[e.key()] = true
	}
	kept := make([]ruleConstant, 0, len(all))
	for _, c := range all {
		if excluded[c.key()] {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// ruleSurfaceFingerprint hashes the declared rule surface, minus its listed
// exceptions, deterministically — the same "build the canonical bytes, then
// sha256.Sum256 them" shape event.ConfigurationHash uses (ADR 0016): sorted
// first, so the result depends only on which rules are declared and what
// they are declared to be, never on the order the file walk visits them or
// the order a const block lists them in.
func ruleSurfaceFingerprint(t *testing.T) string {
	t.Helper()
	kept := withoutExceptions(declaredRuleSurface(t), readRuleSurfaceExceptions(t))
	var canonical strings.Builder
	for _, c := range kept {
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
			"Revert that change, or, if a rule itself changed, bump strategy.RulesVersion and re-pin "+
			"RulesSurfaceFingerprint to %s (ADR 0016).",
			got, strategy.RulesSurfaceFingerprint, got)
	}
}

// TestRuleSurfaceExceptionsAreCurrent audits ruleSurfaceExceptionsFile itself,
// the same two directions internal/coverageaudit's own audit checks: every
// entry must still name a real, current, numeric candidate the sweep finds
// (a stale entry — one naming a constant that was renamed, removed, or is no
// longer numeric-shaped — is worse than none, since it reads as checked when
// nobody has re-checked it), it must carry an actual reason, it must not
// name the same constant twice, and it must never name a Rule*/ADR*
// identifier: those are rules by construction, and this list exists only for
// the numeric sweep's deliberate over-inclusiveness, never as a back door
// out of a real rule.
func TestRuleSurfaceExceptionsAreCurrent(t *testing.T) {
	t.Parallel()

	numericCandidates := make(map[string]bool)
	for _, c := range declaredRuleSurface(t) {
		if c.Numeric {
			numericCandidates[c.key()] = true
		}
	}

	seen := make(map[string]bool)
	for _, e := range readRuleSurfaceExceptions(t) {
		where := fmt.Sprintf("%s.%s", e.Package, e.Name)
		if ruleIdentifierPattern.MatchString(e.Name) {
			t.Errorf("%s: %s names a Rule*/ADR* identifier, which is a rule by construction and may never be listed as an exception", ruleSurfaceExceptionsFile, where)
		}
		if strings.TrimSpace(e.Reason) == "" {
			t.Errorf("%s: %s is listed with no reason; an unexplained exception is a hidden rule with extra steps", ruleSurfaceExceptionsFile, where)
		}
		if seen[e.key()] {
			t.Errorf("%s: %s is listed more than once", ruleSurfaceExceptionsFile, where)
		}
		seen[e.key()] = true
		if !numericCandidates[e.key()] {
			t.Errorf("%s: %s is listed but the sweep no longer finds it as a declared numeric rule constant "+
				"(renamed, removed, or no longer numeric-shaped); remove or correct the entry", ruleSurfaceExceptionsFile, where)
		}
	}
}

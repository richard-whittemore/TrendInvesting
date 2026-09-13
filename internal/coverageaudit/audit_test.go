package coverageaudit

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// modulePath is the import-path prefix a coverage profile stamps on every
// file it reports, stripped to recover the repository-relative path.
const modulePath = "github.com/richard-whittemore/TrendInvesting/"

// auditedPackages is the pattern the audit covers. internal/ is where every
// strategy, risk and reconciliation rule lives; cmd/ is composition, and is
// held to its own tests rather than to this list.
const auditedPackages = "./internal/..."

// exclusionsFile names the checked-in list of blocks that are intentionally
// never executed, each with the reason it cannot be.
const exclusionsFile = "exclusions.json"

// profileEnv supplies an already-generated count-mode profile, so a caller
// that has one (a CI job, or a developer iterating) does not pay for a
// second test run. When it is unset the test generates its own.
const profileEnv = "COVERAGE_AUDIT_PROFILE"

// childEnv marks the nested `go test` run this test starts when it has to
// generate its own profile. That run re-enters this package, so without the
// marker the test would spawn itself without end.
const childEnv = "COVERAGE_AUDIT_CHILD"

// dumpEnv names a file to write the current uncovered set to, in the exact
// shape exclusions.json takes. It is how the list is authored and re-authored
// after a refactor renames a function; it never affects the verdict.
const dumpEnv = "COVERAGE_AUDIT_DUMP"

// categories are the only dispositions an excluded block may carry. Every
// uncovered statement in internal/ is one of these two or it is a test's
// job, and there is no third answer: "we have not got round to it" is a
// missing test, not a category.
var categories = map[string]string{
	// A value this package itself has just built cannot fail its own
	// contract: json.Marshal of a struct of strings, numbers, slices of
	// those and time.Time, or Validate on a payload whose every field was
	// assigned from an already-validated source a few lines above.
	"unreachable-by-construction": "the value was built, and validated, by the code immediately above the guard",
	// A domain rule makes the state the branch tests for impossible. The
	// rule must be named in the reason, and asserted in the source where
	// that is cheap: a branch that exists "in case" is indistinguishable
	// from one that exists because something upstream is broken.
	"unreachable-by-invariant": "a named domain invariant makes the tested state impossible",
}

// block is one uncovered coverage block, identified by something more stable
// than a line number: the file, the function that encloses it, and the
// source text of the block itself. A line number moves whenever anything
// above it is edited, which would make the list rot within a week; this key
// survives everything but a rename or a rewrite of the block, both of which
// are exactly the changes that should force the reason to be re-read.
type block struct {
	File      string `json:"file"`
	Function  string `json:"function"`
	Statement string `json:"statement"`
	Category  string `json:"category,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Count is how many identical blocks the entry accounts for. Several
	// guards in one function are often word-for-word the same statement —
	// eight `return nil, err` propagations in applyCompletedBar, say — and
	// they share one reason. Absent means one.
	Count int `json:"count,omitempty"`

	// line is for the failure message only, and is deliberately not part of
	// the key or of the checked-in file.
	line int
}

func (b block) count() int {
	if b.Count == 0 {
		return 1
	}
	return b.Count
}

func (b block) key() string {
	return b.File + "\x00" + b.Function + "\x00" + b.Statement
}

func (b block) where() string {
	return fmt.Sprintf("%s:%d (%s)", b.File, b.line, b.Function)
}

type exclusionList struct {
	Note       string   `json:"note"`
	Categories []string `json:"categories"`
	Exclusions []block  `json:"exclusions"`
}

// TestEveryUncoveredStatementIsExcludedWithANamedReason is the audit: the set
// of statements no test executes must equal, exactly, the set of statements
// exclusions.json says cannot be executed.
//
// Both directions fail. A newly-uncovered statement is an untested branch, or
// a branch some change upstream has made unreachable — the failure mode that
// hid two defects at once behind a wrong entry level, where the coverage
// percentage barely moved. A listed block that is now covered, or that no
// longer exists, is a stale reason, and a stale reason is worse than none:
// it asserts an invariant nobody has checked since.
func TestEveryUncoveredStatementIsExcludedWithANamedReason(t *testing.T) {
	if os.Getenv(childEnv) != "" {
		t.Skip("nested coverage run: the audit itself is not re-audited")
	}
	t.Parallel()

	root := moduleRoot(t)
	uncovered := uncoveredBlocks(t, root, profile(t, root))

	if dump := os.Getenv(dumpEnv); dump != "" {
		writeDump(t, dump, uncovered)
	}

	listed := readExclusions(t)

	remaining := make(map[string][]block, len(uncovered))
	for _, b := range uncovered {
		remaining[b.key()] = append(remaining[b.key()], b)
	}

	var stale []block
	for _, e := range listed {
		if _, ok := categories[e.Category]; !ok {
			t.Errorf("%s: category %q is not one of the two dispositions this audit recognises", e.where(), e.Category)
		}
		if strings.TrimSpace(e.Reason) == "" {
			t.Errorf("%s: excluded with no reason; an unexplained exclusion is a hidden branch with extra steps", e.where())
		}
		matches := remaining[e.key()]
		if len(matches) < e.count() {
			stale = append(stale, e)
			if len(matches) == 0 {
				continue
			}
		}
		take := min(e.count(), len(matches))
		if remaining[e.key()] = matches[take:]; len(remaining[e.key()]) == 0 {
			delete(remaining, e.key())
		}
	}

	var unlisted []block
	for _, bs := range remaining {
		unlisted = append(unlisted, bs...)
	}
	sort.Slice(unlisted, func(i, j int) bool {
		if unlisted[i].File != unlisted[j].File {
			return unlisted[i].File < unlisted[j].File
		}
		return unlisted[i].line < unlisted[j].line
	})

	if len(unlisted) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%d statement(s) in internal/ are executed by no test and are not in %s.\n", len(unlisted), exclusionsFile)
		b.WriteString("Write a test for each, or add it to the list with the invariant that makes it unreachable:\n")
		for _, u := range unlisted {
			fmt.Fprintf(&b, "  %s\n      %s\n", u.where(), u.Statement)
		}
		fmt.Fprintf(&b, "Re-authoring the whole list: %s=/tmp/uncovered.json go test ./internal/coverageaudit/\n", dumpEnv)
		t.Error(b.String())
	}

	for _, s := range stale {
		t.Errorf("%s (%s) is listed in %s %d time(s) but fewer blocks than that are uncovered: it is now covered, or no longer exists. Correct the count or remove the entry — a reason nobody has re-checked is worse than none.\n      %s",
			s.File, s.Function, exclusionsFile, s.count(), s.Statement)
	}
}

// moduleRoot walks up from the test's own directory to the go.mod that
// defines this module.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// profile returns the path of a count-mode coverage profile for
// auditedPackages, generating one if the environment does not supply it.
func profile(t *testing.T, root string) string {
	t.Helper()
	if path := os.Getenv(profileEnv); path != "" {
		return path
	}
	path := filepath.Join(t.TempDir(), "audit.out")
	cmd := exec.Command("go", "test", "-covermode=count", "-coverprofile="+path, auditedPackages)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), childEnv+"=1", "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating a coverage profile failed; the audit cannot run without one:\n%s\n%v", out, err)
	}
	return path
}

// uncoveredBlocks parses a coverage profile and returns every block with a
// zero execution count, keyed by file, enclosing function and source text.
func uncoveredBlocks(t *testing.T, root, path string) []block {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read coverage profile %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "mode:") {
		t.Fatalf("%s is not a coverage profile", path)
	}

	sources := map[string]*sourceFile{}
	var blocks []block
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("coverage profile line %q has %d fields, want 3", line, len(fields))
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("coverage profile line %q: %v", line, err)
		}
		if count > 0 {
			continue
		}
		name, span, ok := strings.Cut(fields[0], ":")
		if !ok {
			t.Fatalf("coverage profile line %q has no span", line)
		}
		rel := strings.TrimPrefix(name, modulePath)
		src, ok := sources[rel]
		if !ok {
			src = readSource(t, filepath.Join(root, filepath.FromSlash(rel)))
			sources[rel] = src
		}
		startLine, startCol, endLine, endCol := parseSpan(t, span)
		start, end := src.offset(startLine, startCol), src.offset(endLine, endCol)
		blocks = append(blocks, block{
			File:      rel,
			Function:  src.enclosing(start),
			Statement: normalise(string(src.text[start:end])),
			line:      startLine,
		})
	}
	return blocks
}

// sourceFile is one Go file, parsed once, with what the audit needs from it:
// where its lines start and which function encloses a given byte offset.
type sourceFile struct {
	text       []byte
	lineStarts []int
	funcs      []funcSpan
}

type funcSpan struct {
	name       string
	start, end int
}

func readSource(t *testing.T, path string) *sourceFile {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := &sourceFile{text: text, lineStarts: []int{0}}
	for i, c := range text {
		if c == '\n' {
			src.lineStarts = append(src.lineStarts, i+1)
		}
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, text, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		src.funcs = append(src.funcs, funcSpan{
			name:  funcName(fn),
			start: fset.Position(fn.Pos()).Offset,
			end:   fset.Position(fn.End()).Offset,
		})
	}
	return src
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	var receiver string
	switch typ := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if ident, ok := typ.X.(*ast.Ident); ok {
			receiver = "*" + ident.Name
		}
	case *ast.Ident:
		receiver = typ.Name
	}
	return "(" + receiver + ")." + fn.Name.Name
}

// offset converts a 1-based line and byte column into a byte offset.
func (s *sourceFile) offset(line, col int) int {
	at := s.lineStarts[line-1] + col - 1
	if at > len(s.text) {
		return len(s.text)
	}
	return at
}

// enclosing names the top-level function a byte offset falls inside. A
// function literal reports its enclosing declaration, which together with the
// block's own text is enough to tell any two blocks apart.
func (s *sourceFile) enclosing(at int) string {
	for _, fn := range s.funcs {
		if at >= fn.start && at < fn.end {
			return fn.name
		}
	}
	return "(file scope)"
}

// normalise reduces a block's source text to the part that identifies it:
// its statements, with comments, indentation and the enclosing braces
// removed, so that reformatting or rewording a comment does not invalidate
// the list.
func normalise(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "{")
		line = strings.TrimSuffix(line, "}")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
}

func parseSpan(t *testing.T, span string) (startLine, startCol, endLine, endCol int) {
	t.Helper()
	start, end, ok := strings.Cut(span, ",")
	if !ok {
		t.Fatalf("coverage span %q has no end position", span)
	}
	return position(t, start, span), column(t, start, span), position(t, end, span), column(t, end, span)
}

func position(t *testing.T, pos, span string) int {
	t.Helper()
	line, _, ok := strings.Cut(pos, ".")
	if !ok {
		t.Fatalf("coverage span %q has a malformed position %q", span, pos)
	}
	n, err := strconv.Atoi(line)
	if err != nil {
		t.Fatalf("coverage span %q: %v", span, err)
	}
	return n
}

func column(t *testing.T, pos, span string) int {
	t.Helper()
	_, col, ok := strings.Cut(pos, ".")
	if !ok {
		t.Fatalf("coverage span %q has a malformed position %q", span, pos)
	}
	n, err := strconv.Atoi(col)
	if err != nil {
		t.Fatalf("coverage span %q: %v", span, err)
	}
	return n
}

func readExclusions(t *testing.T) []block {
	t.Helper()
	raw, err := os.ReadFile(exclusionsFile)
	if err != nil {
		t.Fatalf("read %s: %v", exclusionsFile, err)
	}
	var list exclusionList
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&list); err != nil {
		t.Fatalf("decode %s: %v", exclusionsFile, err)
	}
	return list.Exclusions
}

// writeDump records the current uncovered set in the shape exclusions.json
// takes, so the list can be re-authored wholesale after a rename rather than
// hand-edited entry by entry.
func writeDump(t *testing.T, path string, blocks []block) {
	t.Helper()
	sorted := append([]block(nil), blocks...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].File != sorted[j].File {
			return sorted[i].File < sorted[j].File
		}
		return sorted[i].line < sorted[j].line
	})
	grouped := make([]block, 0, len(sorted))
	seen := map[string]int{}
	for _, b := range sorted {
		if at, ok := seen[b.key()]; ok {
			grouped[at].Count = grouped[at].count() + 1
			continue
		}
		seen[b.key()] = len(grouped)
		grouped = append(grouped, b)
	}
	sorted = grouped
	encoded, err := json.MarshalIndent(exclusionList{
		Note:       "generated by COVERAGE_AUDIT_DUMP; every entry still needs a category and a reason",
		Categories: sortedCategories(),
		Exclusions: sorted,
	}, "", "  ")
	if err != nil {
		t.Fatalf("encode dump: %v", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write dump %s: %v", path, err)
	}
}

func sortedCategories() []string {
	names := make([]string, 0, len(categories))
	for name := range categories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

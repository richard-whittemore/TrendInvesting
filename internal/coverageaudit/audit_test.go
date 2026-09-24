package coverageaudit

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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

// profileEnv supplies an already-generated coverage profile, so a caller
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
	// contract: json.Marshal after validated-payload-json is established
	// (docs/development.md), or Validate on a payload whose every field was
	// assigned from an already-validated source a few lines above.
	"unreachable-by-construction": "the value was built, and validated, by the code immediately above the guard",
	// A domain rule makes the state the branch tests for impossible. The
	// rule must be named in the reason, and asserted in the source where
	// that is cheap: a branch that exists "in case" is indistinguishable
	// from one that exists because something upstream is broken.
	"unreachable-by-invariant": "a named domain invariant makes the tested state impossible",
}

// block identifies a span by file, enclosing function, text and occurrence.
// Occurrence is its 1-based ordinal among all blocks with identical text in
// that function, including covered blocks, in source order. The key survives
// edits outside the function and edits inside it that do not add, remove or
// reorder identical statements. TestIdenticalGuardsCannotExchangeCoverage
// requires the ordinal: text alone lets a newly uncovered guard consume an
// unrelated guard's exclusion.
type block struct {
	File       string `json:"file"`
	Function   string `json:"function"`
	Statement  string `json:"statement"`
	Occurrence int    `json:"occurrence,omitempty"`
	Category   string `json:"category,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// Count accepts only the legacy default or one; grouping distinct
	// occurrences is rejected. Negative values fail without panicking.
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

func (b block) occurrence() int {
	if b.Occurrence == 0 {
		return 1
	}
	return b.Occurrence
}

func (b block) key() string {
	return b.File + "\x00" + b.Function + "\x00" + b.Statement + "\x00" + strconv.Itoa(b.occurrence())
}

func (b block) where() string {
	return fmt.Sprintf("%s:%d (%s, occurrence %d)", b.File, b.line, b.Function, b.occurrence())
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

	checkExclusions(t, uncovered, readExclusions(t))
}

func checkExclusions(t *testing.T, uncovered, listed []block) {
	t.Helper()
	remaining := make(map[string][]block, len(uncovered))
	for _, b := range uncovered {
		remaining[b.key()] = append(remaining[b.key()], b)
	}

	var stale []block
	for _, e := range listed {
		if e.Count < 0 {
			t.Errorf("%s: count %d is negative", e.where(), e.Count)
			continue
		}
		if e.Count > 1 {
			t.Errorf("%s: count %d groups blocks; list each occurrence separately", e.where(), e.Count)
			continue
		}
		if e.Occurrence < 0 {
			t.Errorf("%s: occurrence must be positive", e.where())
			continue
		}
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
		t.Errorf("%s is listed in %s %d time(s) but fewer blocks than that are uncovered: it is now covered, changed, or no longer exists. Recheck the reason and update or remove the entry.\n      %s",
			s.where(), exclusionsFile, s.count(), s.Statement)
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

// resolveProfile turns whatever COVERAGE_AUDIT_PROFILE holds into a path this
// test can open. A relative one is resolved against the module root, not
// against this package's own directory, because the profile a caller already
// has is the one `make test` wrote — `coverage.out`, at the root — and Go
// runs every test in its own package directory.
func resolveProfile(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

// profile returns the path of a count-mode coverage profile for
// auditedPackages, generating one if the environment does not supply it.
func profile(t *testing.T, root string) string {
	t.Helper()
	if path := os.Getenv(profileEnv); path != "" {
		path = resolveProfile(root, path)
		checkProfileFreshness(t, root, path)
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

// checkProfileFreshness rejects repository inputs newer than a reused profile.
// TestSuppliedProfileRejectsNewerCoverageInputs covers test-only changes, whose
// spans still match. Timestamps cannot prove freshness: the reuse contract in
// docs/development.md also requires unchanged external inputs and test options.
func checkProfileFreshness(t *testing.T, root, path string) {
	t.Helper()
	profileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat coverage profile %s: %v", path, err)
	}
	err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".go") && entry.Name() != "go.mod" && entry.Name() != "go.sum" && !strings.Contains("/"+rel, "/testdata/") {
			return nil
		}
		info, err := os.Stat(name)
		if err != nil {
			return err
		}
		if info.ModTime().After(profileInfo.ModTime()) {
			return fmt.Errorf("coverage profile is older than %s; regenerate it", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cannot reuse %s: %v", path, err)
	}
}

// uncoveredBlocks parses a coverage profile and returns every block with a
// zero execution count, keyed by file, enclosing function, source text and
// occurrence among all matching blocks, including those executed by tests.
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
	type profileBlock struct {
		block
		start   int
		covered bool
	}
	var all []profileBlock
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("coverage profile line %q has %d fields, want 3", line, len(fields))
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("coverage profile line %q: %v", line, err)
		}
		name, span, ok := strings.Cut(fields[0], ":")
		if !ok {
			t.Fatalf("coverage profile line %q has no span", line)
		}
		rel, ok := strings.CutPrefix(name, modulePath)
		if !ok {
			t.Fatalf("coverage profile line %q names %s, which is outside module %s", line, name, strings.TrimSuffix(modulePath, "/"))
		}
		if !strings.HasPrefix(rel, "internal/") {
			continue
		}
		src, ok := sources[rel]
		if !ok {
			src = readSource(t, filepath.Join(root, filepath.FromSlash(rel)))
			sources[rel] = src
		}
		startLine, startCol, endLine, endCol := parseSpan(t, span)
		text, ok := src.slice(startLine, startCol, endLine, endCol)
		if !ok {
			t.Fatalf("%s reports a block at %s:%s that does not lie inside the file as it stands; the profile was taken against different source — regenerate it", path, rel, span)
		}
		start, _ := src.offset(startLine, startCol)
		all = append(all, profileBlock{
			block: block{
				File:      rel,
				Function:  src.enclosing(start),
				Statement: normalise(text),
				line:      startLine,
			},
			start:   start,
			covered: count > 0,
		})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].File != all[j].File {
			return all[i].File < all[j].File
		}
		return all[i].start < all[j].start
	})
	occurrences := make(map[string]int)
	var blocks []block
	for _, entry := range all {
		textKey := entry.key()
		occurrences[textKey]++
		entry.Occurrence = occurrences[textKey]
		if !entry.covered {
			blocks = append(blocks, entry.block)
		}
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

// lineEnd is the byte offset one past the last character of a 1-based line:
// the newline that ended it, or the end of the file for a last line without
// one. A coverage position may point AT that offset — a block ending in a
// closing brace at the end of a line does — but never past it.
func (s *sourceFile) lineEnd(line int) int {
	if line < len(s.lineStarts) {
		return s.lineStarts[line] - 1
	}
	return len(s.text)
}

// offset converts a 1-based line and byte column into a byte offset, and
// reports whether that position exists on THAT LINE.
//
// Bounding the column against its own line rather than against the whole file
// is the load-bearing part. A file long enough overall will happily accept a
// column from a line that has since been shortened, and the span extracted
// from it then runs past its line into the next — silently, since the result
// is still valid source text. Every block in this audit is keyed by its own
// source text, so a reinterpreted span does not fail: it matches the wrong
// entry, or reports an entry as changed when it has not. A guard that turns a
// loud panic into a quiet wrong answer is worse than the panic.
func (s *sourceFile) offset(line, col int) (int, bool) {
	if line < 1 || line > len(s.lineStarts) || col < 1 {
		return 0, false
	}
	at := s.lineStarts[line-1] + col - 1
	if at > s.lineEnd(line) {
		return 0, false
	}
	return at, true
}

// slice returns the source text of one coverage block, and reports whether
// the span lies inside the file as it stands. A profile taken against
// different source than the working tree holds produces spans that run past
// the end of a file, or backwards; saying so is the only useful answer,
// because every block read from that profile describes code that is no longer
// there.
func (s *sourceFile) slice(startLine, startCol, endLine, endCol int) (string, bool) {
	start, startOK := s.offset(startLine, startCol)
	end, endOK := s.offset(endLine, endCol)
	if !startOK || !endOK || start > end {
		return "", false
	}
	return string(s.text[start:end]), true
}

// enclosing names the top-level function a byte offset falls inside. A
// function literal reports its enclosing declaration; the block's occurrence
// distinguishes identical guards within that declaration.
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
// removed. Reformatting preserves this text identity; the separately keyed
// occurrence distinguishes identical text within a function.
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
	for i := range sorted {
		if sorted[i].Occurrence == 1 {
			sorted[i].Occurrence = 0
		}
	}
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

// TestAProfileSuppliedByTheEnvironmentResolvesAgainstTheModuleRoot covers the
// documented reuse workflow rather than assuming it. `make test` writes
// coverage.out at the module root and the guide says to pass it, but go test
// runs each test in its own package directory, so a relative path taken
// verbatim would be looked for inside internal/coverageaudit and the audit
// would report a profile it could not read instead of the one it was given.
func TestAProfileSuppliedByTheEnvironmentResolvesAgainstTheModuleRoot(t *testing.T) {
	t.Parallel()

	const root = "/module"

	tests := []struct {
		name  string
		given string
		want  string
	}{
		{
			name:  "the path make test writes",
			given: "coverage.out",
			want:  filepath.Join(root, "coverage.out"),
		},
		{
			name:  "a path relative to the module root",
			given: filepath.Join("build", "coverage.out"),
			want:  filepath.Join(root, "build", "coverage.out"),
		},
		{
			name:  "an absolute path passes through",
			given: filepath.Join(string(filepath.Separator), "tmp", "cov.out"),
			want:  filepath.Join(string(filepath.Separator), "tmp", "cov.out"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveProfile(root, tt.given); got != tt.want {
				t.Errorf("resolveProfile(%q, %q) = %q, want %q", root, tt.given, got, tt.want)
			}
		})
	}
}

// TestAProfileTakenAgainstDifferentSourceIsRefusedRatherThanMisread is the
// stale-profile case, which is easy to produce: edit a file, then reuse the
// profile from before the edit.
//
// The case that matters is the quiet one. A column recorded when its line was
// longer still lands inside a file that is long enough overall, and the span
// taken from it runs past its own line into the next — producing valid source
// text that belongs to different code. Nothing downstream can tell: every
// block in this audit is keyed by its own source text, so a reinterpreted
// span does not fail, it matches the wrong entry or reports an unchanged
// entry as changed. Bounding each coordinate against the whole file catches
// only the loud half of this; bounding it against its own line catches both.
func TestAProfileTakenAgainstDifferentSourceIsRefusedRatherThanMisread(t *testing.T) {
	t.Parallel()

	// Three lines of quite different lengths, so a column from one of them
	// can be inside the file and outside its own line. Offsets: line 1 at 0
	// with its newline at 9, line 2 ("x", the line that was shortened) at 10
	// with its newline at 11, line 3 at 12 with its newline at 23.
	src := &sourceFile{
		text:       []byte("package p\nx\nfunc f() {}\n"),
		lineStarts: []int{0, 10, 12, 24},
	}

	tests := []struct {
		name                                 string
		startLine, startCol, endLine, endCol int
		want                                 string
		wantOK                               bool
	}{
		{
			name:      "a span inside one line",
			startLine: 1, startCol: 1, endLine: 1, endCol: 8,
			want: "package", wantOK: true,
		},
		{
			// A block ending in a closing brace at the end of its line puts
			// the end column one past the last character, which is the
			// newline's own offset. That is a position, not an overrun.
			name:      "a span ending at its line's own newline",
			startLine: 3, startCol: 1, endLine: 3, endCol: 12,
			want: "func f() {}", wantOK: true,
		},
		{
			// The quiet case. Line 2 held eleven characters when the profile
			// was taken and holds one now. Offset 14 is comfortably inside a
			// 24-byte file, so a file-length bound accepts it and the span
			// comes back as "x\nfu" — line 2's content plus the start of
			// line 3, which is code the block never described.
			name:      "an end column from a line that has since been shortened",
			startLine: 2, startCol: 1, endLine: 2, endCol: 5,
		},
		{
			name:      "a start column from a line that has since been shortened",
			startLine: 2, startCol: 6, endLine: 3, endCol: 2,
		},
		{
			name:      "a line the file no longer has",
			startLine: 99, startCol: 1, endLine: 99, endCol: 2,
		},
		{
			name:      "an end line past the end of the file",
			startLine: 3, startCol: 1, endLine: 99, endCol: 1,
		},
		{
			name:      "a column past the end of the file",
			startLine: 3, startCol: 1, endLine: 3, endCol: 500,
		},
		{
			name:      "a span that runs backwards",
			startLine: 3, startCol: 9, endLine: 1, endCol: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := src.slice(tt.startLine, tt.startCol, tt.endLine, tt.endCol)
			if ok != tt.wantOK {
				t.Fatalf("slice() ok = %v, want %v (got %q)", ok, tt.wantOK, got)
			}
			if got != tt.want {
				t.Errorf("slice() = %q, want %q", got, tt.want)
			}
		})
	}
}

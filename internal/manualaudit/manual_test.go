// Package manualaudit contains no production code. It holds the owner's
// manual (docs/manual/) to the code: every reason the program records for a
// refused order, a withheld decision, a withheld sale, a skipped trade or a
// change of engine state, and every alert title the reporter sends, must
// appear in exactly one chapter's "What can go wrong" table, and every
// chapter's table must say what the owner sees, what the program does and
// what the owner does. A reason or alert the manual does not explain is one
// the owner meets for the first time when it matters; one explained in two
// places is one whose two explanations can drift apart.
//
// The reasons are read from internal/event's closed sets and the titles from
// internal/report's own format strings, so a new reason or alert fails this
// test until a chapter explains it.
package manualaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// reasonPrefixes are the names of internal/event's closed sets of reasons the
// owner can meet: a refusal by the risk gate, a withheld target decision, a
// withheld sale back to 98%, a skipped trade, and why the engine changed
// state. Each set's constants share its prefix.
var reasonPrefixes = []string{"Refused", "Withheld", "SaleWithheld", "OrderSkipped", "EngineStateReason"}

// notOwnerFacing are the constants under those prefixes that no account this
// manual covers can record: the Turtle Baseline's own halt reason. The
// target-weight accounts (Boost and momentum) hold no protective stop.
var notOwnerFacing = map[string]bool{
	"EngineStateReasonCampaignWithoutProtectiveStop": true,
}

// reasonValue is the shape of a reason: lower-case words joined by hyphens.
// It excludes the event-type and schema-version constants that share a
// prefix (OrderSkippedEventType is "strategy.order.skipped").
var reasonValue = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// wcgwHeader is the one header every chapter's "What can go wrong" table
// uses, so each row says the three things the owner needs.
const wcgwHeader = "| What you see | What the program does | What you do |"

// ownTableLayout lists chapters that have not yet had this audit's pass:
// they keep their own "What can go wrong" table layout, and their rows count
// towards a reason or a title only when no other chapter explains it (so a
// reason they alone explain is still explained, and one they repeat from its
// owning chapter is not counted twice). Remove a chapter from this list when
// its table adopts wcgwHeader and names only the reasons it owns.
var ownTableLayout = map[string]bool{
	"ibkr-adapter.md":  true,
	"market-data.md":   true,
	"paper-trading.md": true,
}

// notAChapter are the manual's pages that describe no program, so they have
// no "What can go wrong" section: the reading order and the glossary.
var notAChapter = map[string]bool{
	"README.md":   true,
	"glossary.md": true,
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func parseDir(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// ownerReasons returns every owner-facing reason value in internal/event,
// each once (one value can belong to two sets: stale-broker-snapshot is both
// a refusal and a withheld sale), mapped to the constants that carry it.
func ownerReasons(t *testing.T, root string) map[string][]string {
	t.Helper()
	reasons := map[string][]string{}
	for _, f := range parseDir(t, filepath.Join(root, "internal", "event")) {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) || notOwnerFacing[name.Name] || !hasReasonPrefix(name.Name) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil || !reasonValue.MatchString(v) {
						continue
					}
					reasons[v] = append(reasons[v], name.Name)
				}
			}
		}
	}
	return reasons
}

func hasReasonPrefix(name string) bool {
	for _, p := range reasonPrefixes {
		if strings.HasPrefix(name, p) && len(name) > len(p) {
			return true
		}
	}
	return false
}

// alertTitles returns the format string of every alert title in
// internal/report: each Title field or assignment set from fmt.Sprintf.
func alertTitles(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}
	var titles []string
	add := func(e ast.Expr) {
		call, ok := e.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Sprintf" {
			return
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil || seen[v] {
			return
		}
		seen[v] = true
		titles = append(titles, v)
	}
	for _, f := range parseDir(t, filepath.Join(root, "internal", "report")) {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && id.Name == "Title" {
					add(n.Value)
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Title" && i < len(n.Rhs) {
						add(n.Rhs[i])
					}
				}
			}
			return true
		})
	}
	return titles
}

// verb matches one fmt verb in a title's format string.
var verb = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z%]`)

// titlePattern turns a title's format string into the pattern the manual
// writes it as: each value the reporter fills in is a placeholder in angle
// brackets, so "%s: order refused (%s)" is written
// "<account>: order refused (<reason>)".
func titlePattern(format string) *regexp.Regexp {
	var b strings.Builder
	last := 0
	for _, loc := range verb.FindAllStringIndex(format, -1) {
		b.WriteString(regexp.QuoteMeta(format[last:loc[0]]))
		if format[loc[0]:loc[1]] == "%%" {
			b.WriteString("%")
		} else {
			b.WriteString(`<[^<>|\n]+>`)
		}
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(format[last:]))
	return regexp.MustCompile(b.String())
}

// chapter is one manual page's "What can go wrong" section.
type chapter struct {
	name    string
	found   bool     // the page has the section
	headers []string // the header line of each table in it
	rows    []string // every body row of every table in it
}

var heading = regexp.MustCompile(`^(#+)\s+(.*)$`)

// readChapter returns a page's "What can go wrong" section: from a heading
// that starts with those words to the next heading of the same or a higher
// level, outside code fences.
func readChapter(name, text string) chapter {
	c := chapter{name: name}
	level := 0
	fenced := false
	prevTable := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := heading.FindStringSubmatch(line); m != nil {
			l := len(m[1])
			if level > 0 && l <= level {
				level = 0
			}
			if level == 0 && strings.HasPrefix(m[2], "What can go wrong") {
				level = l
				c.found = true
			}
			prevTable = false
			continue
		}
		if level == 0 {
			continue
		}
		isTable := strings.HasPrefix(strings.TrimSpace(line), "|")
		switch {
		case isTable && !prevTable:
			c.headers = append(c.headers, strings.TrimSpace(line))
		case isTable && strings.Trim(strings.TrimSpace(line), "|-: ") == "":
			// the separator line under a header
		case isTable:
			c.rows = append(c.rows, line)
		}
		prevTable = isTable
	}
	return c
}

func readManual(t *testing.T, root string) []chapter {
	t.Helper()
	dir := filepath.Join(root, "docs", "manual")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var chapters []chapter
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		text, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		chapters = append(chapters, readChapter(e.Name(), string(text)))
	}
	return chapters
}

// chaptersMatching returns the chapters with a "What can go wrong" row that
// match reports true for. A chapter in ownTableLayout is returned only when
// no other chapter matches.
func chaptersMatching(chapters []chapter, match func(row string) bool) []string {
	var names, pending []string
	for _, c := range chapters {
		if !slices.ContainsFunc(c.rows, match) {
			continue
		}
		if ownTableLayout[c.name] {
			pending = append(pending, c.name)
		} else {
			names = append(names, c.name)
		}
	}
	if len(names) == 0 {
		return pending
	}
	return names
}

func TestEveryReasonIsExplainedInExactlyOneChapter(t *testing.T) {
	root := repoRoot(t)
	reasons := ownerReasons(t, root)
	if len(reasons) < 30 {
		t.Fatalf("found %d reasons in internal/event; the reader of its closed sets is broken", len(reasons))
	}
	chapters := readManual(t, root)
	values := make([]string, 0, len(reasons))
	for v := range reasons {
		values = append(values, v)
	}
	sort.Strings(values)
	for _, v := range values {
		code := "`" + v + "`"
		names := chaptersMatching(chapters, func(row string) bool { return strings.Contains(row, code) })
		if len(names) != 1 {
			t.Errorf("reason %s (%s) is in the \"What can go wrong\" table of %d chapters %v; want exactly one",
				code, strings.Join(reasons[v], ", "), len(names), names)
		}
	}
}

func TestEveryAlertTitleIsExplainedInExactlyOneChapter(t *testing.T) {
	root := repoRoot(t)
	titles := alertTitles(t, root)
	if len(titles) < 15 {
		t.Fatalf("found %d alert titles in internal/report; the reader of its titles is broken", len(titles))
	}
	chapters := readManual(t, root)
	for _, title := range titles {
		p := titlePattern(title)
		names := chaptersMatching(chapters, p.MatchString)
		if len(names) != 1 {
			t.Errorf("alert title %q (written as %s) is in the \"What can go wrong\" table of %d chapters %v; want exactly one",
				title, p, len(names), names)
		}
	}
}

func TestEveryChapterHasAWhatCanGoWrongTable(t *testing.T) {
	for _, c := range readManual(t, repoRoot(t)) {
		if notAChapter[c.name] {
			continue
		}
		if !c.found {
			t.Errorf("%s has no \"What can go wrong\" section", c.name)
			continue
		}
		if ownTableLayout[c.name] {
			continue
		}
		if len(c.headers) == 0 {
			t.Errorf("%s: its \"What can go wrong\" section has no table", c.name)
		}
		for _, h := range c.headers {
			if h != wcgwHeader {
				t.Errorf("%s: a \"What can go wrong\" table's header is %q; want %q", c.name, h, wcgwHeader)
			}
		}
	}
}

func TestTitlePatternReadsPlaceholders(t *testing.T) {
	p := titlePattern("%s: sale back to 98%% filled %d of %d (%s)")
	if !p.MatchString("| `<account>: sale back to 98% filled <n> of <wanted> (<instrument>)` | x |") {
		t.Errorf("%s does not match the placeholder form", p)
	}
	if p.MatchString("joint: sale back to 98% filled 3 of 5 (SPY)") {
		t.Errorf("%s matches a filled-in title; the manual writes placeholders", p)
	}
}

func TestAChapterAwaitingItsPassCountsOnlyWhenAloneInExplainingAReason(t *testing.T) {
	owner := chapter{name: "states-and-kill-switch.md", rows: []string{"| `a` | x | y |"}}
	pending := chapter{name: "ibkr-adapter.md", rows: []string{"| `a` | x | y |", "| `b` | x | y |"}}
	other := chapter{name: "momentum.md", rows: []string{"| `b` | x | y |"}}
	has := func(code string) func(string) bool {
		return func(row string) bool { return strings.Contains(row, "`"+code+"`") }
	}
	if got := chaptersMatching([]chapter{owner, pending}, has("a")); !slices.Equal(got, []string{"states-and-kill-switch.md"}) {
		t.Errorf("a: %v; want only the chapter that has had its pass", got)
	}
	if got := chaptersMatching([]chapter{pending}, has("b")); !slices.Equal(got, []string{"ibkr-adapter.md"}) {
		t.Errorf("b alone: %v; want the awaiting chapter", got)
	}
	if got := chaptersMatching([]chapter{owner, other}, has("b")); !slices.Equal(got, []string{"momentum.md"}) {
		t.Errorf("b: %v", got)
	}
	if got := chaptersMatching([]chapter{other, other}, has("b")); len(got) != 2 {
		t.Errorf("two chapters that have had their pass both count: %v", got)
	}
}

func TestReadChapterScopesToTheSection(t *testing.T) {
	text := strings.Join([]string{
		"# Title",
		"| `outside` | a | b |",
		"## What can go wrong, and what it does",
		wcgwHeader,
		"| --- | --- | --- |",
		"| `inside` | a | b |",
		"### A subsection",
		"| `nested` | a | b |",
		"```",
		"| `fenced` | a | b |",
		"```",
		"## What backs this",
		"| `after` | a | b |",
	}, "\n")
	c := readChapter("x.md", text)
	if !c.found || len(c.headers) != 2 || c.headers[0] != wcgwHeader {
		t.Fatalf("headers %v found %v", c.headers, c.found)
	}
	want := []string{"| `inside` | a | b |"}
	if !slices.Equal(c.rows, want) {
		t.Errorf("rows %v; want %v", c.rows, want)
	}
}

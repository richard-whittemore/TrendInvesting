// Package manualaudit contains no production code. It holds the owner's
// manual (docs/manual/) to the code: every reason the program records for a
// refused order, a withheld decision, a withheld sale, a skipped trade or a
// change of engine state, and every alert title the reporter sends, must be
// explained by its own row of one chapter's "What can go wrong" table, named
// in that row's first cell ("What you see"), and every row of every
// chapter's table must say what the owner sees, what the program does and
// what the owner does. A reason or alert the manual does not explain is one
// the owner meets for the first time when it matters; one explained in two
// places is one whose two explanations can drift apart. A mention in another
// cell, such as a cross-reference, explains nothing. A value two sets share
// means a different thing in each, so it has one row per set, each saying
// which set it explains (reasonRowProblems has the rule).
//
// The reasons are read from internal/event's closed sets and the titles from
// every way internal/report sets an alert's Title, so a new reason or alert
// fails this test until a chapter explains it, and a constant or title the
// reader cannot classify fails it too rather than being skipped.
package manualaudit

import (
	"fmt"
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

// notAReasonSuffixes name the constants that share a set's prefix but are
// the set's event type or schema version, not one of its reasons
// (OrderSkippedEventType is "strategy.order.skipped").
var notAReasonSuffixes = []string{"EventType", "SchemaVersion"}

// notOwnerFacing are the constants under those prefixes that no account this
// manual covers can record: the Turtle Baseline's own halt reason. The
// target-weight accounts (Boost and momentum) hold no protective stop.
var notOwnerFacing = map[string]bool{
	"EngineStateReasonCampaignWithoutProtectiveStop": true,
}

// reasonValue is the shape of a reason: lower-case words joined by hyphens.
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
var ownTableLayout = map[string]bool{}

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

// source is one Go package's non-test files, parsed.
type source struct {
	fset  *token.FileSet
	files []*ast.File
}

func parseDir(t *testing.T, dir string) source {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	src := source{fset: token.NewFileSet()}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(src.fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		src.files = append(src.files, f)
	}
	return src
}

// parseSource parses one file's text as x.go, for the reader's own tests.
func parseSource(t *testing.T, text string) source {
	t.Helper()
	src := source{fset: token.NewFileSet()}
	f, err := parser.ParseFile(src.fset, "x.go", text, 0)
	if err != nil {
		t.Fatal(err)
	}
	src.files = []*ast.File{f}
	return src
}

// constants is a package's constants, each with the expression that gives
// its value (nil for one written with no expression of its own, an iota
// repetition), and the names of the types it declares, which a constant's
// value may be converted to.
type constants struct {
	values map[string]ast.Expr
	types  map[string]bool
}

func (s source) consts() constants {
	out := constants{values: map[string]ast.Expr{}, types: map[string]bool{"string": true}}
	for _, f := range s.files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if ok && gd.Tok == token.TYPE {
				for _, spec := range gd.Specs {
					out.types[spec.(*ast.TypeSpec).Name.Name] = true
				}
			}
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					var v ast.Expr
					if i < len(vs.Values) {
						v = vs.Values[i]
					}
					out.values[name.Name] = v
				}
			}
		}
	}
	return out
}

// constString resolves a constant string expression: a literal of either
// quoting, in parentheses, converted to a type the package declares
// (Reason("x")) or to string, another constant, or a sum of these. ok is
// false for anything else, a call to a function included.
func constString(e ast.Expr, consts constants, depth int) (string, bool) {
	if depth > 16 {
		return "", false
	}
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(e.Value)
		return v, err == nil
	case *ast.ParenExpr:
		return constString(e.X, consts, depth+1)
	case *ast.Ident:
		v, ok := consts.values[e.Name]
		if !ok || v == nil {
			return "", false
		}
		return constString(v, consts, depth+1)
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); !ok || !consts.types[id.Name] || len(e.Args) != 1 {
			return "", false
		}
		return constString(e.Args[0], consts, depth+1)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		x, okx := constString(e.X, consts, depth+1)
		y, oky := constString(e.Y, consts, depth+1)
		return x + y, okx && oky
	}
	return "", false
}

// reasonsIn returns every owner-facing reason value in an event package,
// each once (one value can belong to two sets: stale-broker-snapshot is both
// a refusal and a withheld sale), mapped to the constants that carry it, and
// a problem for every constant under a reason prefix that is neither a
// set's event type or schema version nor a reason value it can read.
func reasonsIn(src source) (reasons map[string][]string, problems []string) {
	consts := src.consts()
	names := make([]string, 0, len(consts.values))
	for name := range consts.values {
		names = append(names, name)
	}
	sort.Strings(names)
	reasons = map[string][]string{}
	for _, name := range names {
		if !hasReasonPrefix(name) || notOwnerFacing[name] || hasNotAReasonSuffix(name) {
			continue
		}
		v, ok := constString(consts.values[name], consts, 0)
		if !ok || !reasonValue.MatchString(v) {
			problems = append(problems, fmt.Sprintf("constant %s is under a reason prefix but is not a reason value this audit can read (lower-case words joined by hyphens): name it as a set's event type or schema version, list it in notOwnerFacing, or teach constString its form", name))
			continue
		}
		reasons[v] = append(reasons[v], name)
	}
	return reasons, problems
}

func ownerReasons(t *testing.T, root string) map[string][]string {
	t.Helper()
	reasons, problems := reasonsIn(parseDir(t, filepath.Join(root, "internal", "event")))
	for _, p := range problems {
		t.Error(p)
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

func hasNotAReasonSuffix(name string) bool {
	for _, s := range notAReasonSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// titlesIn returns the format string of every alert title in a report
// package, from each Title field or assignment, whether it is set from a
// string literal, a constant, fmt.Sprintf with a literal or constant format,
// or a call to one of the package's own functions whose every return is one
// of these, and whether it is a keyed field, an assignment or the Title's
// place in an Alert literal that lists its fields by position; and a
// problem, naming its position, for every Title set any other way and every
// positional Alert literal whose Title it cannot place, so a title the reader
// cannot see fails the audit.
func titlesIn(src source) (titles, problems []string) {
	consts := src.consts()
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range src.files {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	seen := map[string]bool{}
	var formats func(e ast.Expr, depth int) ([]string, bool)
	formats = func(e ast.Expr, depth int) ([]string, bool) {
		if depth > 16 {
			return nil, false
		}
		if v, ok := constString(e, consts, 0); ok {
			return []string{v}, true
		}
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return nil, false
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" && sel.Sel.Name == "Sprintf" && len(call.Args) > 0 {
				return formats(call.Args[0], depth+1)
			}
			return nil, false
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || funcs[id.Name] == nil {
			return nil, false
		}
		var out []string
		readable := true
		ast.Inspect(funcs[id.Name].Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.ReturnStmt:
				if len(n.Results) != 1 {
					readable = false
					return false
				}
				vs, ok := formats(n.Results[0], depth+1)
				if !ok {
					readable = false
				}
				out = append(out, vs...)
			}
			return true
		})
		return out, readable && len(out) > 0
	}
	add := func(e ast.Expr) {
		vs, ok := formats(e, 0)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: an alert Title set from an expression this audit cannot read; set it from a literal, a constant, fmt.Sprintf with one of these as its format, or a function returning one of these", src.fset.Position(e.Pos())))
			return
		}
		for _, v := range vs {
			if !seen[v] {
				seen[v] = true
				titles = append(titles, v)
			}
		}
	}
	// titleField is Title's index among the fields of the package's Alert
	// struct, for reading a positional literal; -1 when the package declares
	// no Alert with a Title field.
	titleField := -1
	for _, f := range src.files {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Alert" {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return false
			}
			i := 0
			for _, field := range st.Fields.List {
				if len(field.Names) == 0 {
					i++
					continue
				}
				for _, name := range field.Names {
					if name.Name == "Title" {
						titleField = i
					}
					i++
				}
			}
			return false
		})
	}
	isAlert := func(e ast.Expr) bool {
		if st, ok := e.(*ast.StarExpr); ok {
			e = st.X
		}
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name == "Alert"
		case *ast.SelectorExpr:
			return e.Sel.Name == "Alert"
		}
		return false
	}
	// positional reads the Title of an Alert literal that lists its fields
	// by position; a keyed literal's Title is read as a KeyValueExpr.
	positional := func(lit *ast.CompositeLit) {
		if len(lit.Elts) == 0 {
			return
		}
		if _, keyed := lit.Elts[0].(*ast.KeyValueExpr); keyed {
			return
		}
		if titleField < 0 || titleField >= len(lit.Elts) {
			problems = append(problems, fmt.Sprintf("%s: an Alert literal listing its fields by position, whose Title this audit cannot place; key its fields", src.fset.Position(lit.Pos())))
			return
		}
		add(lit.Elts[titleField])
	}
	// declared maps each type the package declares to what it is declared as,
	// so a literal of a named slice or map type is read as the type it names.
	declared := map[string]ast.Expr{}
	for _, f := range src.files {
		for _, decl := range f.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok {
				for _, spec := range gd.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						declared[ts.Name.Name] = ts.Type
					}
				}
			}
		}
	}
	// resolve follows named types to the type they are declared as, stopping
	// at Alert itself.
	resolve := func(e ast.Expr) ast.Expr {
		for range 16 {
			if isAlert(e) {
				return e
			}
			id, ok := e.(*ast.Ident)
			if !ok || declared[id.Name] == nil {
				return e
			}
			e = declared[id.Name]
		}
		return e
	}
	// placed holds the elided literals a parent literal's type has read.
	placed := map[*ast.CompositeLit]bool{}
	// read reads a literal of type typ: an Alert is read positionally; a slice
	// or map of something has its elided element literals read as its element
	// type, however deeply nested.
	var read func(lit *ast.CompositeLit, typ ast.Expr, depth int)
	read = func(lit *ast.CompositeLit, typ ast.Expr, depth int) {
		typ = resolve(typ)
		if isAlert(typ) {
			positional(lit)
			return
		}
		var elem ast.Expr
		switch t := typ.(type) {
		case *ast.ArrayType:
			elem = t.Elt
		case *ast.MapType:
			elem = t.Value
		default:
			return
		}
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				e = kv.Value
			}
			if u, ok := e.(*ast.UnaryExpr); ok {
				e = u.X
			}
			if inner, ok := e.(*ast.CompositeLit); ok && inner.Type == nil && depth < 16 {
				placed[inner] = true
				read(inner, elem, depth+1)
			}
		}
	}
	for _, f := range src.files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if n.Type == nil {
					// An elided literal is read through its parent's type; one
					// no parent placed is a literal whose type this audit
					// cannot resolve, and may be an Alert.
					if !placed[n] && len(n.Elts) > 0 {
						if _, keyed := n.Elts[0].(*ast.KeyValueExpr); !keyed {
							problems = append(problems, fmt.Sprintf("%s: a literal listing its values by position, whose type this audit cannot resolve to see whether it is an Alert; name its type", src.fset.Position(n.Pos())))
						}
					}
					break
				}
				read(n, n.Type, 0)
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && id.Name == "Title" {
					add(n.Value)
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Title" {
						continue
					}
					if n.Tok != token.ASSIGN || len(n.Lhs) != len(n.Rhs) {
						problems = append(problems, fmt.Sprintf("%s: an alert Title set with %s, which this audit cannot read", src.fset.Position(n.Pos()), n.Tok))
						continue
					}
					add(n.Rhs[i])
				}
			}
			return true
		})
	}
	return titles, problems
}

func alertTitles(t *testing.T, root string) []string {
	t.Helper()
	titles, problems := titlesIn(parseDir(t, filepath.Join(root, "internal", "report")))
	for _, p := range problems {
		t.Error(p)
	}
	return titles
}

// verb matches one fmt verb in a title's format string.
var verb = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z%]`)

// titlePattern turns a title's format string into the pattern the manual
// writes it as: in backticks, each value the reporter fills in a placeholder
// in angle brackets, so "%s: order refused (%s)" is written
// "`<account>: order refused (<reason>)`". The backticks bound the title,
// so a title is never satisfied by a longer one that begins with it.
func titlePattern(format string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("`")
	last := 0
	for _, loc := range verb.FindAllStringIndex(format, -1) {
		b.WriteString(regexp.QuoteMeta(format[last:loc[0]]))
		if format[loc[0]:loc[1]] == "%%" {
			b.WriteString("%")
		} else {
			b.WriteString("<[^<>|`\n]+>")
		}
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(format[last:]))
	b.WriteString("`")
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

// cells splits a table row into its trimmed cells, on every pipe a
// backslash does not escape, as Markdown tables are read.
func cells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	if strings.HasSuffix(row, "|") && !strings.HasSuffix(row, `\|`) {
		row = row[:len(row)-1]
	}
	var out []string
	var cell strings.Builder
	for i := 0; i < len(row); i++ {
		switch {
		case row[i] == '\\' && i+1 < len(row) && row[i+1] == '|':
			cell.WriteString(`\|`)
			i++
		case row[i] == '|':
			out = append(out, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(row[i])
		}
	}
	return append(out, strings.TrimSpace(cell.String()))
}

// firstCell is a row's "What you see" cell.
func firstCell(row string) string { return cells(row)[0] }

// reasonCell reports whether a row's first cell names the reason, in
// backticks.
func reasonCell(reason string) func(row string) bool {
	code := "`" + reason + "`"
	return func(row string) bool { return strings.Contains(firstCell(row), code) }
}

// titleCell reports whether a row's first cell holds the title, written as
// titlePattern says.
func titleCell(format string) func(row string) bool {
	p := titlePattern(format)
	return func(row string) bool { return p.MatchString(firstCell(row)) }
}

// rowsMatching returns, by chapter name, every "What can go wrong" row that
// match reports true for. A chapter in ownTableLayout's rows are returned
// only when no other chapter has a row that matches.
func rowsMatching(chapters []chapter, match func(row string) bool) []string {
	var names []string
	for _, r := range matchedRows(chapters, match) {
		names = append(names, r.chapter)
	}
	return names
}

// matchedRow is a "What can go wrong" row and the chapter it is in.
type matchedRow struct{ chapter, row string }

// matchedRows is rowsMatching's rows, with their text.
func matchedRows(chapters []chapter, match func(row string) bool) []matchedRow {
	var rows, pending []matchedRow
	for _, c := range chapters {
		for _, row := range c.rows {
			if !match(row) {
				continue
			}
			if ownTableLayout[c.name] {
				pending = append(pending, matchedRow{c.name, row})
			} else {
				rows = append(rows, matchedRow{c.name, row})
			}
		}
	}
	if len(rows) == 0 {
		return pending
	}
	return rows
}

// rowProblems returns a problem for every row of a chapter's tables that
// does not have exactly the header's three cells, each said.
func rowProblems(c chapter) []string {
	var problems []string
	for _, row := range c.rows {
		cs := cells(row)
		if len(cs) != 3 {
			problems = append(problems, fmt.Sprintf("%s: a \"What can go wrong\" row has %d cells, not 3: %s", c.name, len(cs), row))
			continue
		}
		for i, cell := range cs {
			if cell == "" {
				problems = append(problems, fmt.Sprintf("%s: a \"What can go wrong\" row leaves %q empty: %s", c.name, strings.Split(strings.Trim(wcgwHeader, "| "), " | ")[i], row))
			}
		}
	}
	return problems
}

func TestEveryReasonIsExplainedInExactlyOneRow(t *testing.T) {
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
		for _, p := range reasonRowProblems(chapters, v, reasons[v]) {
			t.Error(p)
		}
	}
}

// reasonSet returns the closed set a reason constant belongs to: the longest
// reason prefix its name begins with.
func reasonSet(name string) string {
	set := ""
	for _, p := range reasonPrefixes {
		if strings.HasPrefix(name, p) && len(p) > len(set) {
			set = p
		}
	}
	return set
}

// setQualifiers are what a row's first cell writes straight after a reason
// value two or more sets share, to say which set's meaning the row explains:
// "`stale-broker-snapshot` (a refused sell)" and "`stale-broker-snapshot` (a
// withheld sale)". A value only one set records needs no qualifier.
var setQualifiers = map[string]string{
	"Refused":           "(a refused ",
	"Withheld":          "(a withheld decision",
	"SaleWithheld":      "(a withheld sale",
	"OrderSkipped":      "(a skipped trade",
	"EngineStateReason": "(a change of state",
}

// reasonRowProblems holds reason value v to the rule: it begins exactly one
// "What can go wrong" row for each set whose constants carry it. Sets are
// counted, not constants, so an alias constant in one set demands no second
// row. A value one set records needs one row naming it. A value two or more
// sets record needs, for each set, exactly one row whose first cell names
// the value followed by that set's qualifier, and no other row naming it, so
// each row is tied to the meaning it explains.
func reasonRowProblems(chapters []chapter, v string, names []string) []string {
	var sets []string
	for _, n := range names {
		if s := reasonSet(n); !slices.Contains(sets, s) {
			sets = append(sets, s)
		}
	}
	sort.Strings(sets)
	var problems []string
	if rows := rowsMatching(chapters, reasonCell(v)); len(rows) != len(sets) {
		problems = append(problems, fmt.Sprintf("reason %#q (%s) begins %d \"What can go wrong\" rows %v; want %d, one for each set that records it (%s)",
			v, strings.Join(names, ", "), len(rows), rows, len(sets), strings.Join(sets, ", ")))
	}
	if len(sets) < 2 {
		return problems
	}
	// Each row that names the value must begin with exactly one set's
	// qualified value, and carry no other set's, so the qualifier is tied to
	// the row it explains; the sets so found must be exactly the sets.
	found := map[string]int{}
	unqualified := false
	for _, s := range sets {
		if _, ok := setQualifiers[s]; !ok {
			unqualified = true
			problems = append(problems, fmt.Sprintf("reason %#q is recorded by the set %q, which has no entry in setQualifiers to tell its row from the other sets'; add one", v, s))
		}
	}
	if unqualified {
		return problems
	}
	for _, r := range matchedRows(chapters, reasonCell(v)) {
		cell := firstCell(r.row)
		var carried, leading []string
		for _, s := range sets {
			code := "`" + v + "` " + setQualifiers[s]
			if strings.Contains(cell, code) {
				carried = append(carried, s)
			}
			if strings.HasPrefix(cell, code) {
				leading = append(leading, s)
			}
		}
		if len(leading) != 1 || len(carried) != 1 {
			problems = append(problems, fmt.Sprintf("%s: reason %#q, which more than one set records, has a row whose first cell must begin with the value and exactly one set's qualifier (%s) and carry no other; it begins with %v and carries %v: %s",
				r.chapter, v, qualifierList(sets), leading, carried, cell))
			continue
		}
		found[leading[0]]++
	}
	for _, s := range sets {
		if found[s] != 1 {
			problems = append(problems, fmt.Sprintf("reason %#q, which more than one set records, has %d rows that begin with it written %q; want exactly one, explaining the %s set's meaning",
				v, found[s], "`"+v+"` "+setQualifiers[s]+"…)", s))
		}
	}
	return problems
}

func qualifierList(sets []string) string {
	var out []string
	for _, s := range sets {
		out = append(out, fmt.Sprintf("%q", setQualifiers[s]))
	}
	return strings.Join(out, ", ")
}

func TestEveryAlertTitleIsExplainedInExactlyOneRow(t *testing.T) {
	root := repoRoot(t)
	titles := alertTitles(t, root)
	if len(titles) < 15 {
		t.Fatalf("found %d alert titles in internal/report; the reader of its titles is broken", len(titles))
	}
	chapters := readManual(t, root)
	raw, err := os.ReadFile(filepath.Join(root, "docs", "manual", "alerts-and-dashboard.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, index, found := strings.Cut(string(raw), "## Alert class index\n")
	if !found {
		t.Fatal("missing alert class index")
	}
	index, _, _ = strings.Cut(index, "\n## ")
	classIndex := chapter{name: "alert class index"}
	for _, line := range strings.Split(index, "\n") {
		if strings.HasPrefix(line, "| `") {
			classIndex.rows = append(classIndex.rows, line)
			if !alertRowHasClass(line) {
				t.Errorf("alert row without a class: %s", line)
			}
		}
	}
	for _, title := range titles {
		rows := matchedRows([]chapter{classIndex}, titleCell(title))
		if len(rows) != 1 {
			t.Errorf("alert %q needs exactly one class-index row; got %d", title, len(rows))
		}
		for _, row := range rows {
			if !alertRowHasClass(row.row) {
				t.Errorf("alert row without a class: %s", row.row)
			}
		}
		if rows := rowsMatching(chapters, titleCell(title)); len(rows) != 1 {
			t.Errorf("alert title %q (written as %s) begins %d \"What can go wrong\" rows %v; want exactly one",
				title, titlePattern(title), len(rows), rows)
		}
	}
}

func TestEveryRowSaysAllThreeThings(t *testing.T) {
	for _, c := range readManual(t, repoRoot(t)) {
		if notAChapter[c.name] || ownTableLayout[c.name] {
			continue
		}
		for _, p := range rowProblems(c) {
			t.Error(p)
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
	ownTableLayout["awaiting.md"] = true
	defer delete(ownTableLayout, "awaiting.md")
	owner := chapter{name: "states-and-kill-switch.md", rows: []string{"| `a` | x | y |"}}
	pending := chapter{name: "awaiting.md", rows: []string{"| `a` | x | y |", "| `b` | x | y |"}}
	other := chapter{name: "momentum.md", rows: []string{"| `b` | x | y |"}}
	if got := rowsMatching([]chapter{owner, pending}, reasonCell("a")); !slices.Equal(got, []string{"states-and-kill-switch.md"}) {
		t.Errorf("a: %v; want only the chapter that has had its pass", got)
	}
	if got := rowsMatching([]chapter{pending}, reasonCell("b")); !slices.Equal(got, []string{"awaiting.md"}) {
		t.Errorf("b alone: %v; want the awaiting chapter", got)
	}
	if got := rowsMatching([]chapter{owner, other}, reasonCell("b")); !slices.Equal(got, []string{"momentum.md"}) {
		t.Errorf("b: %v", got)
	}
	if got := rowsMatching([]chapter{other, other}, reasonCell("b")); len(got) != 2 {
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

func TestAReasonCountsOnlyInTheFirstCellOfARow(t *testing.T) {
	c := chapter{name: "states-and-kill-switch.md", rows: []string{
		"| The account is Halted | Refused (`engine-halted` above) | Recover |",
	}}
	if got := rowsMatching([]chapter{c}, reasonCell("engine-halted")); len(got) != 0 {
		t.Errorf("a reason named only in another cell counted as explained: %v", got)
	}
	c.rows = append(c.rows, "| `engine-halted`. Alert: x | Refuses | Recover |")
	if got := rowsMatching([]chapter{c}, reasonCell("engine-halted")); len(got) != 1 {
		t.Errorf("the row that explains the reason in its first cell: %v; want one row", got)
	}
}

func TestCellsSplitOnUnescapedPipes(t *testing.T) {
	got := cells("| `a\\|b` | x |  |")
	want := []string{"`a\\|b`", "x", ""}
	if !slices.Equal(got, want) {
		t.Errorf("cells %q; want %q", got, want)
	}
}

func TestEveryRowNeedsThreeNonEmptyCells(t *testing.T) {
	c := chapter{name: "x.md", headers: []string{wcgwHeader}, rows: []string{
		"| `a` | does | do |",
		"| `b` |  | do |",
		"| `c` | does |",
	}}
	if got := rowProblems(c); len(got) != 2 {
		t.Errorf("problems %q; want one for the empty cell and one for the missing cell", got)
	}
}

func TestTitlePatternIsAnchoredToItsBackticks(t *testing.T) {
	p := titlePattern("%s: sale of %s withheld")
	if p.MatchString("| `<account>: sale of <instrument> withheld (<reason>)` | x | y |") {
		t.Errorf("%s matches a longer title that begins with it", p)
	}
	if !p.MatchString("| `<account>: sale of <instrument> withheld`, urgent | x | y |") {
		t.Errorf("%s does not match its own backticked form", p)
	}
}

const titleSource = `package report

import "fmt"

const heartbeatTitle = "%s: no engine heartbeat"

const plain = "the reporter stopped"

func lateTitle(late bool) string {
	if late {
		return "%s: late"
	}
	return "%s: on time"
}

func alerts(account string) []Alert {
	a := Alert{Title: fmt.Sprintf("%s: order refused (%s)", account, "x")}
	a.Title = plain
	b := Alert{Title: fmt.Sprintf(heartbeatTitle, account)}
	c := Alert{Title: fmt.Sprintf(lateTitle(true), account)}
	return []Alert{a, b, c}
}
`

func TestAlertTitlesReadsEveryWayATitleIsSet(t *testing.T) {
	titles, problems := titlesIn(parseSource(t, titleSource))
	if len(problems) != 0 {
		t.Fatalf("problems %q", problems)
	}
	want := []string{"%s: late", "%s: no engine heartbeat", "%s: on time", "%s: order refused (%s)", "the reporter stopped"}
	sort.Strings(titles)
	if !slices.Equal(titles, want) {
		t.Errorf("titles %q; want %q", titles, want)
	}
}

func TestAlertTitlesFailsOnATitleItCannotRead(t *testing.T) {
	src := `package report

func alerts(account string, kind string) Alert {
	return Alert{Title: account + ": " + kind}
}
`
	if _, problems := titlesIn(parseSource(t, src)); len(problems) != 1 || !strings.Contains(problems[0], "x.go:4") {
		t.Errorf("problems %q; want one naming the title's position", problems)
	}
}

const reasonSource = "package event\n" +
	"type Reason string\n" +
	"const OrderSkippedEventType = \"strategy.order.skipped\"\n" +
	"const OrderSkippedSchemaVersion uint32 = 1\n" +
	"const (\n" +
	"\tRefusedPlain = \"plain-reason\"\n" +
	"\tRefusedRaw = `raw-reason`\n" +
	"\tRefusedTyped Reason = \"typed-reason\"\n" +
	"\tRefusedWrapped = Reason(\"wrapped-reason\")\n" +
	"\tRefusedParens = (\"parens-reason\")\n" +
	"\tRefusedAlias = RefusedPlain\n" +
	")\n"

func TestOwnerReasonsReadsEverySpellingAndWrapper(t *testing.T) {
	reasons, problems := reasonsIn(parseSource(t, reasonSource))
	if len(problems) != 0 {
		t.Fatalf("problems %q", problems)
	}
	want := map[string][]string{
		"plain-reason":   {"RefusedAlias", "RefusedPlain"},
		"raw-reason":     {"RefusedRaw"},
		"typed-reason":   {"RefusedTyped"},
		"wrapped-reason": {"RefusedWrapped"},
		"parens-reason":  {"RefusedParens"},
	}
	for v := range reasons {
		sort.Strings(reasons[v])
	}
	if len(reasons) != len(want) {
		t.Fatalf("reasons %v; want %v", reasons, want)
	}
	for v, names := range want {
		if !slices.Equal(reasons[v], names) {
			t.Errorf("%s: %v; want %v", v, reasons[v], names)
		}
	}
}

func TestOwnerReasonsFailsOnAConstantItCannotClassify(t *testing.T) {
	src := "package event\n" +
		"const (\n" +
		"\tRefusedFirst = \"first-reason\"\n" +
		"\tRefusedUpper = \"Not A Reason\"\n" +
		"\tRefusedNumber = 3\n" +
		")\n" +
		"const (\n" +
		"\tWithheldA = iota\n" +
		"\tWithheldB\n" +
		")\n"
	if _, problems := reasonsIn(parseSource(t, src)); len(problems) != 4 {
		t.Errorf("problems %q; want one each for RefusedUpper, RefusedNumber, WithheldA and WithheldB", problems)
	}
}

func TestAnAliasInOneSetNeedsOneRow(t *testing.T) {
	c := chapter{name: "states-and-kill-switch.md", rows: []string{"| `plain-reason` | x | y |"}}
	if got := reasonRowProblems([]chapter{c}, "plain-reason", []string{"RefusedAlias", "RefusedPlain"}); len(got) != 0 {
		t.Errorf("two constants of one set carrying one value demand one row: %q", got)
	}
	c.rows = append(c.rows, "| `plain-reason` again | x | y |")
	if got := reasonRowProblems([]chapter{c}, "plain-reason", []string{"RefusedAlias", "RefusedPlain"}); len(got) == 0 {
		t.Error("a second row for one set's value passed")
	}
}

func TestASharedValueNeedsOneQualifiedRowPerSet(t *testing.T) {
	names := []string{"RefusedStale", "SaleWithheldStale"}
	both := chapter{name: "states-and-kill-switch.md", rows: []string{
		"| `shared` (a refused sell). Alert: x | x | y |",
		"| `shared` (a withheld sale). Alert: x | x | y |",
	}}
	if got := reasonRowProblems([]chapter{both}, "shared", names); len(got) != 0 {
		t.Errorf("one qualified row per set: %q", got)
	}
	twice := chapter{name: "states-and-kill-switch.md", rows: []string{
		"| `shared` (a refused sell) | x | y |",
		"| `shared` (a refused order) | x | y |",
	}}
	if got := reasonRowProblems([]chapter{twice}, "shared", names); len(got) == 0 {
		t.Error("two rows both explaining the refusal passed with the withheld sale unexplained")
	}
	bare := chapter{name: "states-and-kill-switch.md", rows: []string{
		"| `shared` (a refused sell) | x | y |",
		"| `shared` | x | y |",
	}}
	if got := reasonRowProblems([]chapter{bare}, "shared", names); len(got) == 0 {
		t.Error("a row that names no set passed as the withheld sale's row")
	}
}

func TestAQualifierInAnotherRowDoesNotTieARowToItsSet(t *testing.T) {
	names := []string{"RefusedStale", "SaleWithheldStale"}
	crossReference := chapter{name: "states-and-kill-switch.md", rows: []string{
		"| `shared` (a refused sell). Alert: x. Not `shared` (a withheld sale), below | x | y |",
		"| `shared` (a withheld sale). Alert: y | x | y |",
	}}
	if got := reasonRowProblems([]chapter{crossReference}, "shared", names); len(got) == 0 {
		t.Error("a withheld-sale qualifier written in the refused row's cross-reference passed")
	}
	twoQualifiers := chapter{name: "states-and-kill-switch.md", rows: []string{
		"| `shared` (a refused sell) and `shared` (a withheld sale) | x | y |",
		"| `shared` (a withheld sale) | x | y |",
	}}
	if got := reasonRowProblems([]chapter{twoQualifiers}, "shared", names); len(got) == 0 {
		t.Error("a row carrying both sets' qualifiers passed")
	}
}

func TestASetWithNoQualifierFailsTheAudit(t *testing.T) {
	c := chapter{name: "states-and-kill-switch.md", rows: []string{
		"| `shared` (a refused sell) | x | y |",
		"| `shared`. Alert: x | x | y |",
	}}
	got := reasonRowProblems([]chapter{c}, "shared", []string{"RefusedStale", "NewSetStale"})
	if len(got) == 0 {
		t.Error("a set reasonPrefixes lists without a setQualifiers entry fell back to the empty qualifier and passed")
	}
}

func TestAlertTitlesReadsAPositionalLiteral(t *testing.T) {
	src := `package report

import "fmt"

type Alert struct {
	Key   string
	Title string
	Body  string
}

func alerts(account string) []Alert {
	a := Alert{"k", fmt.Sprintf("%s: positional", account), "body"}
	b := &Alert{"k", "%s: by pointer", "body"}
	return []Alert{a, *b, {"k", "%s: elided", "body"}}
}
`
	titles, problems := titlesIn(parseSource(t, src))
	if len(problems) != 0 {
		t.Fatalf("problems %q", problems)
	}
	want := []string{"%s: by pointer", "%s: elided", "%s: positional"}
	sort.Strings(titles)
	if !slices.Equal(titles, want) {
		t.Errorf("titles %q; want %q", titles, want)
	}
}

func TestAlertTitlesReadsAPositionalLiteralInANamedOrNestedType(t *testing.T) {
	src := `package report

type Alert struct {
	Key   string
	Title string
	Body  string
}

type Alerts []Alert

type ByAccount map[string]Alerts

func named() Alerts { return Alerts{{"k", "%s: named", "body"}} }

func nested() map[string][]Alert {
	return map[string][]Alert{"x": {{"k", "%s: nested", "body"}}}
}

func chained() ByAccount { return ByAccount{"x": {{"k", "%s: chained", "body"}}} }
`
	titles, problems := titlesIn(parseSource(t, src))
	if len(problems) != 0 {
		t.Fatalf("problems %q", problems)
	}
	want := []string{"%s: chained", "%s: named", "%s: nested"}
	sort.Strings(titles)
	if !slices.Equal(titles, want) {
		t.Errorf("titles %q; want %q", titles, want)
	}
}

func TestAlertTitlesFailsOnAnElidedLiteralOfAnUnresolvableType(t *testing.T) {
	src := `package report

func alerts() other.Alerts {
	return other.Alerts{{"k", "%s: lost", "body"}}
}
`
	if _, problems := titlesIn(parseSource(t, src)); len(problems) != 1 {
		t.Errorf("problems %q; want one for the elided literal whose type cannot be resolved", problems)
	}
}

func TestAlertTitlesFailsOnAPositionalLiteralItCannotPlace(t *testing.T) {
	src := `package report

func alerts(account string) Alert {
	return Alert{"k", account, "body"}
}
`
	if _, problems := titlesIn(parseSource(t, src)); len(problems) != 1 || !strings.Contains(problems[0], "x.go:4") {
		t.Errorf("problems %q; want one naming the literal's position", problems)
	}
}

// alertRowHasClass enforces the paging invariant in alerts-and-dashboard.md.
// The class must be attached to the alert row, not inherited from a heading.
func alertRowHasClass(row string) bool {
	return strings.Contains(row, "Class: capital-safety") || strings.Contains(row, "Class: operational")
}
func TestAnAlertRowWithoutAClassFails(t *testing.T) {
	if alertRowHasClass("| `alert` | does something | read the capital-safety chapter |") {
		t.Fatal("accepted a row without a class")
	}
	for _, class := range []string{"capital-safety", "operational"} {
		if !alertRowHasClass("| `alert` | Class: " + class + ". Does something | Act |") {
			t.Fatal("rejected class", class)
		}
	}
}

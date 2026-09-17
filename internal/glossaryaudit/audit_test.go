// Package glossaryaudit enforces docs/development.md's Source comment
// standard, principle 2: a doc comment that cites CONTEXT.md for a specific
// quoted term must name a term the glossary actually defines. It is the
// same "assert the absence of a known failure class" discipline
// internal/coverageaudit applies to an unexplained uncovered branch and
// internal/event's marshal invariant test applies to an unregistered
// payload type, applied here to citation rot: a citation pointing at a
// definition that is not there is worse than no citation, since it looks
// checkable and fails only when someone tries.
package glossaryaudit

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// headerPattern matches one of CONTEXT.md's own glossary headings: a bold
// term at the start of a line, immediately followed by a colon — the exact
// shape every entry in the file uses (**Term**:).
var headerPattern = regexp.MustCompile(`(?m)^\*\*([^*]+)\*\*:`)

// citationPattern matches a source comment citing CONTEXT.md for a specific
// quoted term, in either form this codebase uses: a colon then a quoted
// term (CONTEXT.md: "N"), or a possessive apostrophe-s then a quoted term
// (CONTEXT.md's "N"). A short run of ordinary prose is allowed between the
// citation and its first quote, since some comments read "a Unit in that
// state is" between the colon and its quoted term rather than quoting
// immediately — bounded to 40 characters, well past the longest existing
// case and far short of a second, unrelated citation such as an ADR's own
// quoted rule.
var citationPattern = regexp.MustCompile(`CONTEXT\.md(?:'s|:)[^"'\n]{0,40}(["'])([^"']+)['"]`)

// continuationPattern matches a second (or later) quotation chained onto a
// citation citationPattern already matched: a compound citation such as
// CONTEXT.md: "Campaign" — "not a Unit, is what gets entered, added to,
// stopped out, and exited" names one header and then separately quotes that
// header's own definition verbatim, and BOTH quotations are a claim this
// audit must check — the fabricated instance this package exists to catch
// took exactly this shape, with a genuine term first and the fabricated
// text second. The gap before the next quote is restricted to whitespace,
// dashes and list punctuation only — no letters, digits or colon — so a
// genuinely new attribution (the "; ADR 0009: " that introduces an
// unrelated ADR quotation) can never be mistaken for a continuation of this
// citation: reaching a letter before a quote stops the chain.
var continuationPattern = regexp.MustCompile(`^[\s—\-,;]{0,10}(["'])([^"']+)['"]`)

// glossaryTerms returns CONTEXT.md's own header terms and the concatenated
// prose of each entry's own definition — never the file's raw text, so a
// citation's body quote can only resolve against what a term's own entry
// actually says (see resolves), not against a heading elsewhere, the
// file's intro paragraph, or another entry's "_Avoid_" synonym note. Each
// entry's span runs from its own "**Term**:" heading to the next heading (or
// EOF); the "_Avoid_" line that convention always places last in that span,
// when present, is stripped before the span is kept, since a citation
// resolving means the definition says this, not the note warning readers off
// a near-synonym does.
func glossaryTerms(contextMD string) (headers map[string]bool, body string) {
	headers = make(map[string]bool)
	locs := headerPattern.FindAllStringSubmatchIndex(contextMD, -1)
	defs := make([]string, 0, len(locs))
	for i, loc := range locs {
		headers[contextMD[loc[2]:loc[3]]] = true
		start, end := loc[1], len(contextMD)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		def := contextMD[start:end]
		if avoid := strings.Index(def, "_Avoid_"); avoid >= 0 {
			def = def[:avoid]
		}
		defs = append(defs, def)
	}
	return headers, strings.Join(defs, "\n")
}

// citedTerms extracts every term a comment's text cites CONTEXT.md for,
// including every quotation chained onto a compound citation, not only its
// first (see continuationPattern). An *ast.CommentGroup's own Text() strips
// "//" markers but keeps each source line's own newline, so a quoted term
// gofmt wrapped across two comment lines (e.g. "Delisting\nExit") would
// otherwise read as two words neither side can resolve; collapsing all
// whitespace to single spaces first reads it the way a person reading the
// rendered comment would.
func citedTerms(text string) []string {
	flat := strings.Join(strings.Fields(text), " ")
	var terms []string
	for _, m := range citationPattern.FindAllStringSubmatchIndex(flat, -1) {
		terms = append(terms, flat[m[4]:m[5]])
		pos := m[1]
		for {
			cont := continuationPattern.FindStringSubmatchIndex(flat[pos:])
			if cont == nil {
				break
			}
			terms = append(terms, flat[pos+cont[4]:pos+cont[5]])
			pos += cont[1]
		}
	}
	return terms
}

// resolves reports whether term is something CONTEXT.md actually defines:
// one of its own glossary headers, or text quoted verbatim from its body.
func resolves(term string, headers map[string]bool, body string) bool {
	return headers[term] || strings.Contains(body, term)
}

// TestCitedTermsFindsOnlyGenuineCitations pins citedTerms's own behaviour
// before it is trusted to sweep the whole module: it must recognise both
// quote styles and the intervening-prose shape a handful of real comments
// use, and it must not mistake an unrelated nearby quotation (an ADR's own,
// or one that never followed a CONTEXT.md citation at all) for one.
func TestCitedTermsFindsOnlyGenuineCitations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "double-quoted term immediately after the colon",
			text: `a wide bar shrinks its own Unit (CONTEXT.md: "Completed bar").`,
			want: []string{"Completed bar"},
		},
		{
			name: "single-quoted term after a colon citation",
			text: `bar-count warm-up (CONTEXT.md: 'Completed bar') is complete.`,
			want: []string{"Completed bar"},
		},
		{
			name: "double-quoted term after a possessive citation",
			text: `CONTEXT.md's "Notional Account" entry exists precisely because the two are not the same figure.`,
			want: []string{"Notional Account"},
		},
		{
			name: "compound citation checks both the header and its quoted body prose",
			text: `the Baseline has no partial exit (CONTEXT.md: "Campaign" — "not a Unit, is what gets entered, added to, stopped out, and exited").`,
			want: []string{"Campaign", "not a Unit, is what gets entered, added to, stopped out, and exited"},
		},
		{
			name: "term after intervening prose",
			text: `a stop at or above entry is a legitimate break-even or profit-protecting level (CONTEXT.md: a Unit in that state is "risk-free"), not a corrupted one.`,
			want: []string{"risk-free"},
		},
		{
			name: "term cited then a separate, unrelated ADR quotation is ignored",
			text: `a delisting is a Campaign's life ending (CONTEXT.md: "Delisting Exit"; ADR 0009: "a delisting is a forced exit at the last available price").`,
			want: []string{"Delisting Exit"},
		},
		{
			name: "a bare mention of CONTEXT.md with no colon or possessive is not a citation",
			text: `this function's own doc comment on CONTEXT.md "a field present on one side only").`,
			want: nil,
		},
		{
			name: "CONTEXT.md referenced without any quoted term",
			text: `CONTEXT.md defines a Setup as an Eligible instrument not in a Campaign.`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := citedTerms(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("citedTerms(%q) = %v, want %v", tt.text, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("citedTerms(%q) = %v, want %v", tt.text, got, tt.want)
				}
			}
		})
	}
}

// TestResolvesAcceptsHeadersAndVerbatimBody pins resolves's own two ways a
// citation can be genuine, and confirms an invented one is refused.
func TestResolvesAcceptsHeadersAndVerbatimBody(t *testing.T) {
	t.Parallel()
	const fixture = "**Protective Stop**:\nThe price at which a Campaign's Units are exited to cap loss. Every open Campaign has one at all times.\n"
	headers, body := glossaryTerms(fixture)

	if !resolves("Protective Stop", headers, body) {
		t.Error(`resolves("Protective Stop") = false, want true (a glossary header)`)
	}
	if !resolves("Every open Campaign has one at all times", headers, body) {
		t.Error(`resolves("Every open Campaign has one at all times") = false, want true (verbatim body text)`)
	}
	if resolves("risk-free", headers, body) {
		t.Error(`resolves("risk-free") = true, want false (not defined anywhere in the fixture)`)
	}
}

// TestGlossaryTermsScopesBodyToDefinitionProse pins the fix for a citation
// that could "resolve" against prose outside any definition: the file's
// intro paragraph, a section heading, or another entry's "_Avoid_" synonym
// note. Resolving must mean the term's own definition says this, not these
// words appear somewhere in the file.
func TestGlossaryTermsScopesBodyToDefinitionProse(t *testing.T) {
	t.Parallel()
	const fixture = "This glossary is deliberately opinionated about strict definitions.\n\n" +
		"### Positions\n\n" +
		"**Campaign**:\n" +
		"The complete life of a position in one instrument.\n" +
		"_Avoid_: trade, position (both ambiguous between a Unit and the whole Campaign).\n\n" +
		"**Unit**:\nOne indivisible increment of a position.\n"
	headers, body := glossaryTerms(fixture)

	if resolves("deliberately opinionated about strict definitions", headers, body) {
		t.Error(`resolves(intro prose) = true, want false: the file's intro is not any term's own definition`)
	}
	if resolves("Positions", headers, body) {
		t.Error(`resolves(a section heading) = true, want false: a section heading is not a definition`)
	}
	if resolves("ambiguous between a Unit and the whole Campaign", headers, body) {
		t.Error(`resolves(an _Avoid_ note) = true, want false: a synonym warning is not the definition itself`)
	}
	if !resolves("The complete life of a position in one instrument", headers, body) {
		t.Error(`resolves(Campaign's own definition) = false, want true`)
	}
}

// TestCompoundCitationCatchesAFabricatedSecondQuotation demonstrates the
// fix for the defect this package exists to close: a compound citation that
// names a header and then separately quotes that header's own body prose
// must have EVERY quotation checked, not only the first. A real term named
// first must never let a fabricated quotation following it escape — which
// is exactly the shape the one real fabrication this PR fixed (in
// exit_proposal.go) took.
func TestCompoundCitationCatchesAFabricatedSecondQuotation(t *testing.T) {
	t.Parallel()
	const fixture = "**Campaign**:\nThe complete life of a position in one instrument, from the first Unit's entry to the exit of the last. A Campaign, not a Unit, is what gets entered, added to, stopped out, and exited.\n"
	headers, body := glossaryTerms(fixture)

	genuine := citedTerms(`the Baseline has no partial exit (CONTEXT.md: "Campaign" — "not a Unit, is what gets entered, added to, stopped out, and exited").`)
	if len(genuine) != 2 {
		t.Fatalf("citedTerms found %d terms in the genuine compound citation, want 2: %v", len(genuine), genuine)
	}
	for _, term := range genuine {
		if !resolves(term, headers, body) {
			t.Errorf("resolves(%q) = false, want true: both quotations in a genuine compound citation should resolve", term)
		}
	}

	// Same shape, but the second quotation is fabricated: a real term
	// ("Campaign") still comes first, exactly as it did in the escaped
	// defect, and only the body text after the dash is invented.
	fabricated := citedTerms(`the Baseline has no partial exit (CONTEXT.md: "Campaign" — "not a Unit, but the whole position").`)
	if len(fabricated) != 2 {
		t.Fatalf("citedTerms found %d terms in the fabricated compound citation, want 2: %v", len(fabricated), fabricated)
	}
	if !resolves(fabricated[0], headers, body) {
		t.Errorf("resolves(%q) = false, want true: the first quotation names a real header", fabricated[0])
	}
	if resolves(fabricated[1], headers, body) {
		t.Errorf("resolves(%q) = true, want false: this is the fabricated quotation a compound citation must still catch", fabricated[1])
	}
}

// TestEveryContextMDCitationNamesADefinedTerm sweeps every .go file in the
// module for a CONTEXT.md citation and fails on the first one whose quoted
// term is not something CONTEXT.md actually defines. This is the same class
// of bug as an uncovered branch with no recorded reason (coverageaudit) or a
// payload type missing from the marshal-invariant fixture list
// (internal/event): a comment asserting a fact — here, that a definition
// exists at a named place — that a mechanical check can verify, so review
// does not have to catch it by hand every time.
func TestEveryContextMDCitationNamesADefinedTerm(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	contextMD, err := os.ReadFile(filepath.Join(root, "CONTEXT.md"))
	if err != nil {
		t.Fatal(err)
	}
	headers, body := glossaryTerms(string(contextMD))
	if len(headers) == 0 {
		t.Fatal("found no glossary headers in CONTEXT.md; headerPattern is out of sync with the file's own format")
	}

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
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, group := range file.Comments {
			for _, term := range citedTerms(group.Text()) {
				if resolves(term, headers, body) {
					continue
				}
				pos := fset.Position(group.Pos())
				t.Errorf("%s:%d: cites CONTEXT.md: %q, which CONTEXT.md does not define", rel, pos.Line, term)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

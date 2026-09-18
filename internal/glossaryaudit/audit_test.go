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
var citationPattern = regexp.MustCompile(`CONTEXT\.md(?:'s|:)(?:'s|[^"'\n]){0,40}(?:"([^"\n]+)"|'([^'\n]+)')`)

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
var continuationPattern = regexp.MustCompile(`^([\s—\-,;]{0,10})(?:"([^"\n]+)"|'([^'\n]+)')`)

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
func glossaryTerms(contextMD string) (headers map[string]bool, defs map[string]string, body string) {
	headers = make(map[string]bool)
	defs = make(map[string]string)
	locs := headerPattern.FindAllStringSubmatchIndex(contextMD, -1)
	spans := make([]string, 0, len(locs))
	for i, loc := range locs {
		term := contextMD[loc[2]:loc[3]]
		headers[term] = true
		start, end := loc[1], len(contextMD)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		def := contextMD[start:end]
		if avoid := strings.Index(def, "_Avoid_"); avoid >= 0 {
			def = def[:avoid]
		}
		def = definitionProse(def)
		defs[term] = def
		spans = append(spans, def)
	}
	// Joined on newlines, which no normalised span contains, so a quotation
	// can only ever match inside one entry.
	return headers, defs, strings.Join(spans, "\n")
}

// definitionProse reduces one entry's span to the prose that entry states.
//
// A Markdown section heading sits between two entries, so it falls inside
// the preceding entry's span and would otherwise read as part of that
// definition: with "### Positions" between them, a comment could cite
// CONTEXT.md for "Positions" and resolve against Breakout's entry. A
// heading organises the file; it defines nothing.
//
// Whitespace is then collapsed the same way citedTerms collapses a comment's,
// because CONTEXT.md wraps its definitions and a quotation is compared
// against them as one line. Without this a citation quoting a sentence that
// happens to cross a line break in CONTEXT.md fails an audit it should pass
// — a false accusation of fabrication, which is worse here than a miss.
func definitionProse(span string) string {
	kept := make([]string, 0, 8)
	for _, line := range strings.Split(span, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
}

// citation is one quoted claim a source comment makes about CONTEXT.md.
// term is the quoted text. entry names the glossary entry the quotation is
// attributed to, and is set only when the comment's own punctuation says it
// is an attribution — a dash — rather than a term cited alongside another.
//
// Comments use two shapes that chain a second quotation, and the separator
// is the only thing that tells them apart:
//
//	(CONTEXT.md: "Notional Account", "Drawdown Step")
//	(CONTEXT.md: "Campaign" — "not a Unit, is what gets entered, added to, stopped out, and exited")
//
// The first cites two terms together. The second cites one term and then
// quotes what that term's own entry says. (Both examples above are real
// citations, and this audit checks them like any other: an illustration
// that would not survive the rule it illustrates has no business being
// here.)
//
// The first claims nothing about either entry's prose. The second claims
// Campaign's entry says this, and that claim is what the audit must check
// against Campaign's entry alone. Reading both alike and recovering the
// difference from whether the quotation happens to name a header would
// accept "Campaign" — "Unit" without ever asking whether Campaign's entry
// mentions a Unit, so the separator is kept rather than inferred.
type citation struct {
	term  string
	entry string
}

// attributes reports whether the punctuation between two chained
// quotations makes the second an attribution to the first rather than a
// term cited alongside it. A comma or semicolon separates items in a list;
// a dash introduces what the preceding term's entry says. Whitespace alone
// is read as an attribution, the stricter of the two, so a shape nobody has
// written yet has to prove itself against a named entry rather than being
// waved through on the chance that it was a list.
func attributes(separator string) bool {
	return !strings.ContainsAny(separator, ",;")
}

// citedTerms extracts every term a comment's text cites CONTEXT.md for,
// including every quotation chained onto a compound citation, not only its
// first (see continuationPattern). An *ast.CommentGroup's own Text() strips
// "//" markers but keeps each source line's own newline, so a quoted term
// gofmt wrapped across two comment lines (e.g. "Delisting\nExit") would
// otherwise read as two words neither side can resolve; collapsing all
// whitespace to single spaces first reads it the way a person reading the
// rendered comment would.
func citedTerms(text string) []citation {
	flat := strings.Join(strings.Fields(text), " ")
	var cites []citation
	for _, m := range citationPattern.FindAllStringSubmatchIndex(flat, -1) {
		// A term is attributed to the MOST RECENT term cited, not to the
		// one that opened the citation. In "Unit", "Campaign" — "prose",
		// the prose is Campaign's; reading it as Unit's would reject a
		// perfectly good citation. Only a co-citation moves the target, so
		// a second attribution still belongs to the same entry.
		target := quoted(flat, m, 1, 2)
		cites = append(cites, citation{term: target})
		pos := m[1]
		for {
			cont := continuationPattern.FindStringSubmatchIndex(flat[pos:])
			if cont == nil {
				break
			}
			term := quoted(flat[pos:], cont, 2, 3)
			if attributes(flat[pos+cont[2] : pos+cont[3]]) {
				cites = append(cites, citation{term: term, entry: target})
			} else {
				cites = append(cites, citation{term: term})
				target = term
			}
			pos += cont[1]
		}
	}
	return cites
}

// quoted returns whichever of the given alternation groups matched. A
// citation's term may be double- or single-quoted, and the two are separate
// groups so that each delimiter must be closed by its own kind: accepting
// any closing quote let an apostrophe inside a possessive open a quotation,
// and "the Campaign's \"Unit\" rung" was read as citing a term named "s ".
func quoted(s string, m []int, groups ...int) string {
	for _, g := range groups {
		if m[2*g] >= 0 {
			return s[m[2*g]:m[2*g+1]]
		}
	}
	return ""
}

// resolves reports whether a citation is something CONTEXT.md actually
// says.
//
// A quotation that is not an attribution — the one that opens a citation,
// or a term cited alongside it — may be any glossary header or any text
// quoted verbatim from a definition.
//
// An attributed quotation is a narrower claim: that the entry named before
// the dash says this. It is checked against that entry's definition alone,
// because resolving it against the whole file accepts a citation that names
// one entry and quotes a different entry's definition. Every quotation in
// such a citation is genuine glossary text and the citation is still false,
// since the entry it names does not say what it is credited with saying.
//
// An attribution whose named entry is not a glossary header at all — the
// head quotation was itself prose — is refused outright. There is no entry
// whose prose could confirm it, and falling back to the whole file would
// restore exactly the hole this scoping closes, with a genuine prose head
// resolving happily and raising no failure to report the citation by.
func resolves(c citation, headers map[string]bool, defs map[string]string, body string) bool {
	if c.entry == "" {
		return headers[c.term] || strings.Contains(body, c.term)
	}
	def, named := defs[c.entry]
	return named && strings.Contains(def, c.term)
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
		want []citation
	}{
		{
			name: "double-quoted term immediately after the colon",
			text: `a wide bar shrinks its own Unit (CONTEXT.md: "Completed bar").`,
			want: []citation{{term: "Completed bar"}},
		},
		{
			name: "single-quoted term after a colon citation",
			text: `bar-count warm-up (CONTEXT.md: 'Completed bar') is complete.`,
			want: []citation{{term: "Completed bar"}},
		},
		{
			name: "double-quoted term after a possessive citation",
			text: `CONTEXT.md's "Notional Account" entry exists precisely because the two are not the same figure.`,
			want: []citation{{term: "Notional Account"}},
		},
		{
			name: "compound citation checks both the header and its quoted body prose",
			text: `the Baseline has no partial exit (CONTEXT.md: "Campaign" — "not a Unit, is what gets entered, added to, stopped out, and exited").`,
			want: []citation{{term: "Campaign"}, {term: "not a Unit, is what gets entered, added to, stopped out, and exited", entry: "Campaign"}},
		},
		{
			name: "term after intervening prose",
			text: `a stop at or above entry is a legitimate break-even or profit-protecting level (CONTEXT.md: a Unit in that state is "risk-free"), not a corrupted one.`,
			want: []citation{{term: "risk-free"}},
		},
		{
			name: "term cited then a separate, unrelated ADR quotation is ignored",
			text: `a delisting is a Campaign's life ending (CONTEXT.md: "Delisting Exit"; ADR 0009: "a delisting is a forced exit at the last available price").`,
			want: []citation{{term: "Delisting Exit"}},
		},
		{
			name: "possessive prose before the quoted term is not itself a quote",
			text: `the rung is set (CONTEXT.md: the Campaign's "Unit" size) at entry.`,
			want: []citation{{term: "Unit"}},
		},
		{
			name: "a quotation whose delimiters do not match is not a citation",
			text: `an unbalanced (CONTEXT.md: "Unit' rung) proves nothing.`,
			want: nil,
		},
		{
			name: "a term attributed after a co-citation belongs to the nearest term, not the first",
			text: `both apply (CONTEXT.md: "Unit", "Campaign" — "The complete life of a position").`,
			want: []citation{{term: "Unit"}, {term: "Campaign"}, {term: "The complete life of a position", entry: "Campaign"}},
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
	headers, defs, body := glossaryTerms(fixture)

	if !resolves(citation{term: "Protective Stop"}, headers, defs, body) {
		t.Error(`resolves("Protective Stop") = false, want true (a glossary header)`)
	}
	if !resolves(citation{term: "Every open Campaign has one at all times"}, headers, defs, body) {
		t.Error(`resolves("Every open Campaign has one at all times") = false, want true (verbatim body text)`)
	}
	if resolves(citation{term: "risk-free"}, headers, defs, body) {
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
	headers, defs, body := glossaryTerms(fixture)

	if resolves(citation{term: "deliberately opinionated about strict definitions"}, headers, defs, body) {
		t.Error(`resolves(intro prose) = true, want false: the file's intro is not any term's own definition`)
	}
	if resolves(citation{term: "Positions"}, headers, defs, body) {
		t.Error(`resolves(a section heading) = true, want false: a section heading is not a definition`)
	}
	if resolves(citation{term: "ambiguous between a Unit and the whole Campaign"}, headers, defs, body) {
		t.Error(`resolves(an _Avoid_ note) = true, want false: a synonym warning is not the definition itself`)
	}
	if !resolves(citation{term: "The complete life of a position in one instrument"}, headers, defs, body) {
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
	headers, defs, body := glossaryTerms(fixture)

	genuine := citedTerms(`the Baseline has no partial exit (CONTEXT.md: "Campaign" — "not a Unit, is what gets entered, added to, stopped out, and exited").`)
	if len(genuine) != 2 {
		t.Fatalf("citedTerms found %d terms in the genuine compound citation, want 2: %v", len(genuine), genuine)
	}
	for _, cite := range genuine {
		if !resolves(cite, headers, defs, body) {
			t.Errorf("resolves(%q) = false, want true: both quotations in a genuine compound citation should resolve", cite.term)
		}
	}

	// Same shape, but the second quotation is fabricated: a real term
	// ("Campaign") still comes first, exactly as it did in the escaped
	// defect, and only the body text after the dash is invented.
	fabricated := citedTerms(`the Baseline has no partial exit (CONTEXT.md: "Campaign" — "not a Unit, but the whole position").`)
	if len(fabricated) != 2 {
		t.Fatalf("citedTerms found %d terms in the fabricated compound citation, want 2: %v", len(fabricated), fabricated)
	}
	if !resolves(fabricated[0], headers, defs, body) {
		t.Errorf("resolves(%q) = false, want true: the first quotation names a real header", fabricated[0].term)
	}
	if resolves(fabricated[1], headers, defs, body) {
		t.Errorf("resolves(%q) = true, want false: this is the fabricated quotation a compound citation must still catch", fabricated[1].term)
	}
}

// TestCompoundCitationIsScopedToTheEntryItNames closes the gap the previous
// test left open. Checking every quotation is not enough on its own: if a
// chained quotation resolves against the whole file, a citation can name one
// entry and then quote a DIFFERENT entry's definition, and every quotation in
// it is genuine CONTEXT.md text. The citation is still false — Campaign's
// entry does not say what Unit's entry says — and only scoping the chained
// quotation to the entry the citation named can tell the two apart.
func TestCompoundCitationIsScopedToTheEntryItNames(t *testing.T) {
	t.Parallel()
	const fixture = "**Unit**:\nOne indivisible increment of a position.\n\n" +
		"**Campaign**:\nThe complete life of a position in one instrument.\n"
	headers, defs, body := glossaryTerms(fixture)

	crossed := citedTerms(`a Campaign is (CONTEXT.md: "Campaign" — "One indivisible increment of a position").`)
	if len(crossed) != 2 {
		t.Fatalf("citedTerms found %d quotations, want 2: %v", len(crossed), crossed)
	}
	if crossed[1].entry != "Campaign" {
		t.Fatalf("chained quotation carries entry %q, want %q", crossed[1].entry, "Campaign")
	}
	if !resolves(crossed[0], headers, defs, body) {
		t.Errorf("resolves(%q) = false, want true: Campaign is a real header", crossed[0].term)
	}
	if resolves(crossed[1], headers, defs, body) {
		t.Errorf("resolves(%q) = true, want false: that is Unit's definition, not Campaign's, "+
			"and a citation naming Campaign may not quote it", crossed[1].term)
	}

	// The same quotation attributed to the entry that does say it still
	// resolves, so the scoping refuses a false attribution rather than
	// refusing chained quotations in general.
	honest := citedTerms(`a Unit is (CONTEXT.md: "Unit" — "One indivisible increment of a position").`)
	if len(honest) != 2 {
		t.Fatalf("citedTerms found %d quotations, want 2: %v", len(honest), honest)
	}
	for _, cite := range honest {
		if !resolves(cite, headers, defs, body) {
			t.Errorf("resolves(%q) = false, want true: attributed to the entry that does define it", cite.term)
		}
	}

	// A pair of headers cited together is the other shape a chained
	// quotation takes, and it is not an attribution at all: neither entry
	// is being credited with the other's prose. Scoping must not refuse it.
	paired := citedTerms(`this drives both (CONTEXT.md: "Unit", "Campaign").`)
	if len(paired) != 2 {
		t.Fatalf("citedTerms found %d quotations, want 2: %v", len(paired), paired)
	}
	for _, cite := range paired {
		if !resolves(cite, headers, defs, body) {
			t.Errorf("resolves(%q) = false, want true: a co-cited glossary header, not attributed prose", cite.term)
		}
	}

	for _, cite := range paired {
		if cite.entry != "" {
			t.Errorf("co-cited %q carries entry %q, want none: a comma separates items in a list, "+
				"it does not attribute one to the other", cite.term, cite.entry)
		}
	}

	// The separator, not the shape of the quotation, is what decides. A
	// term name after a dash is still an attribution, and must be something
	// the named entry actually says — being a glossary header of its own
	// does not excuse it from that.
	namedTerm := citedTerms(`a Campaign is (CONTEXT.md: "Campaign" — "Unit").`)
	if len(namedTerm) != 2 {
		t.Fatalf("citedTerms found %d quotations, want 2: %v", len(namedTerm), namedTerm)
	}
	if resolves(namedTerm[1], headers, defs, body) {
		t.Errorf(`resolves("Unit" attributed to Campaign) = true, want false: Campaign's entry in this ` +
			"fixture does not mention a Unit, and Unit being a header elsewhere is not the claim made")
	}

	// An attribution whose head is prose rather than a header has no entry
	// to check against. Refusing it is the point: the head resolves on its
	// own, so nothing else would report the citation, and a whole-file
	// fallback would let the chained quotation come from any entry at all.
	proseHead := citedTerms(`(CONTEXT.md: "One indivisible increment of a position" — "The complete life of a position in one instrument").`)
	if len(proseHead) != 2 {
		t.Fatalf("citedTerms found %d quotations, want 2: %v", len(proseHead), proseHead)
	}
	if !resolves(proseHead[0], headers, defs, body) {
		t.Errorf("resolves(%q) = false, want true: the head is genuine prose from Unit's entry", proseHead[0].term)
	}
	if resolves(proseHead[1], headers, defs, body) {
		t.Errorf("resolves(%q) = true, want false: attributed to a head that names no entry, so nothing "+
			"can confirm it — and the head itself resolves, so no other failure would report this citation", proseHead[1].term)
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
	headers, defs, body := glossaryTerms(string(contextMD))
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
			for _, cite := range citedTerms(group.Text()) {
				if resolves(cite, headers, defs, body) {
					continue
				}
				pos := fset.Position(group.Pos())
				if cite.entry != "" {
					t.Errorf("%s:%d: cites CONTEXT.md: %q — %q, but %[3]q's own entry does not say that",
						rel, pos.Line, cite.entry, cite.term)
					continue
				}
				t.Errorf("%s:%d: cites CONTEXT.md: %q, which CONTEXT.md does not define", rel, pos.Line, cite.term)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

// TestDefinitionProseIsWhatTheEntryStates pins the two ways a raw span is
// not yet a definition. A Markdown section heading falls inside the
// preceding entry's span, so without stripping it a comment could cite
// CONTEXT.md for "Positions" and resolve against Breakout's entry. And
// CONTEXT.md wraps its definitions, so a citation quoting a sentence that
// crosses a line break would fail an audit it should pass — accusing a
// correct citation of fabrication, which is the worse of the two errors
// this package can make.
func TestDefinitionProseIsWhatTheEntryStates(t *testing.T) {
	t.Parallel()
	const fixture = "**Breakout**:\nA price exceeding the channel.\n\n" +
		"### Positions\n\n" +
		"**Unit**:\nOne indivisible\nincrement of a position.\n"
	headers, defs, body := glossaryTerms(fixture)

	if got := defs["Breakout"]; got != "A price exceeding the channel." {
		t.Errorf("Breakout's definition = %q, want the prose alone with no heading and no stray whitespace", got)
	}
	if resolves(citation{term: "### Positions"}, headers, defs, body) {
		t.Error(`resolves("### Positions") = true, want false: a heading organises the file, it defines nothing`)
	}
	if resolves(citation{term: "Positions"}, headers, defs, body) {
		t.Error(`resolves("Positions") = true, want false: it is a heading inside Breakout's span, not part of its definition`)
	}
	if !resolves(citation{term: "One indivisible increment of a position"}, headers, defs, body) {
		t.Error("resolves(a definition wrapped across two lines) = false, want true: " +
			"CONTEXT.md wraps its prose and a comment quotes it as one line")
	}
	if resolves(citation{term: "the channel. One indivisible"}, headers, defs, body) {
		t.Error("resolves(text spanning two entries) = true, want false: entries are joined on a newline " +
			"precisely so a quotation cannot straddle them")
	}
}

package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// Redacted replaces whatever a Redactor takes out of text.
const Redacted = "[redacted]"

// The patterns of what never goes into text bound for a place the owner does
// not control the readers of (docs/live-trading/plan.md section 7: the public
// text carries no balance; docs/manual/alerts-and-dashboard.md): an IBKR
// account id (U or DU and digits, as the broker issues them), a GitHub token,
// and a Tailscale host name or address (the 100.64.0.0/10 range), which name
// the account or reach it; and a dollar figure (dollarFigure).
var identifierPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:DU|U)\d{4,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bgh[opsur]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9.-]*\.ts\.net\b`),
	regexp.MustCompile(`\b100\.(?:6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.\d{1,3}\.\d{1,3}\b`),
}

// dollarFigure is a dollar amount, removed whole before the other figures so
// its sign and grouping go with it.
var dollarFigure = regexp.MustCompile(`[-+]?\$\s?\d[\d,]*(?:\.\d+)?`)

// withheldParts are the parts of a field's or a query column's name that
// mark its value as a figure of the account or a name of it: a balance, a
// size, a cost, a quantity, a price, a limit, a fill, a holding or a P&L
// (docs/live-trading/plan.md section 7: the public text carries no figure),
// or a secret. One rule serves a journal record's fields, wherever they sit,
// and the journal-query columns, so the two never disagree.
var withheldParts = []string{
	"cash", "equity", "liquid", "balance", "margin", "notional", "value", "amount", "account",
	"starting", "book", "residual", "expected", "observed", "figures", "commission", "fee",
	"cost", "realised", "realized", "result", "exposure", "buying", "token", "secret", "password", "key",
	"quantity", "qty", "price", "limit", "fill", "shares", "position", "size", "pnl", "profit", "loss",
	"weight", "fraction", "bought", "sold", "snapshot", "change", "journal",
}

// Withheld reports whether a field or column named name holds a figure or a
// name of the account, so its value is never shown in a public place.
func Withheld(name string) bool {
	name = strings.ToLower(name)
	for _, part := range withheldParts {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

// minLiteral is the shortest literal value a Redactor removes wherever it
// appears; a shorter one, such as a short host name, is removed where it
// stands as a word, so it never mangles the ordinary words that contain it.
const minLiteral = 6

// Redactor takes secrets, account identifiers and figures out of text and
// journal records before they leave the server.
type Redactor struct{ literals, words []string }

// NewRedactor returns a Redactor that also removes each of the given literal
// values, such as the configured secrets and this host's names, in its own
// form and in its JSON-escaped forms, since a record's text arrives escaped.
// Only an empty value is ignored: none is skipped for being short.
func NewRedactor(literals []string) Redactor {
	var r Redactor
	seen := map[string]bool{}
	for _, l := range literals {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		for _, form := range []string{l, jsonEscaped(l, true), jsonEscaped(l, false)} {
			if seen[form] {
				continue
			}
			seen[form] = true
			if len(form) >= minLiteral {
				r.literals = append(r.literals, form)
			} else {
				r.words = append(r.words, form)
			}
		}
	}
	// The longest first, so a value that contains another is removed whole.
	for _, list := range [][]string{r.literals, r.words} {
		sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	}
	return r
}

// jsonEscaped is s as it reads inside a JSON string, with or without the
// HTML escaping Go's encoder applies by default.
func jsonEscaped(s string, html bool) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(html)
	_ = enc.Encode(s) // a string always encodes
	quoted := strings.TrimSuffix(b.String(), "\n")
	return quoted[1 : len(quoted)-1]
}

// Text returns s with every literal value, every pattern and every figure
// removed.
func (r Redactor) Text(s string) string {
	return removeFigures(dollarFigure.ReplaceAllString(r.Identifiers(s), Redacted))
}

// Identifiers returns s with every literal value and every identifier
// pattern removed, as Text removes them, and its figures kept: the check of
// code bound for a public repository, where numbers are the code's own
// (the triage job's pre-push check, whose input already never held a
// figure: the job hands Claude only text through Text). Prose bound for a
// public place goes through Text.
func (r Redactor) Identifiers(s string) string {
	for _, l := range r.literals {
		s = strings.ReplaceAll(s, l, Redacted)
	}
	for _, w := range r.words {
		s = replaceWord(s, w)
	}
	for _, p := range identifierPatterns {
		s = p.ReplaceAllString(s, Redacted)
	}
	return s
}

// replaceWord replaces w in s wherever it stands alone: not next to a letter
// or a digit.
func replaceWord(s, w string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, w)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(w)
		if (i > 0 && isAlnum(s[i-1])) || (end < len(s) && isAlnum(s[end])) {
			b.WriteString(s[:i+1])
			s = s[i+1:]
			continue
		}
		b.WriteString(s[:i] + Redacted)
		s = s[end:]
	}
}

func isAlnum(c byte) bool { return isDigit(c) || isLetter(c) }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
}

// The allow-list of free text, the only tokens whose digits are kept, each
// matched as a whole token: an ISO date, or a date and a time after a T, on
// the calendar (a real month and day, in a year of 19xx or 20xx) and the
// 24-hour clock, to the second and with no fraction of it (a date followed
// by a bare T is neither); and an ADR's number written "ADR nnnn". A time of
// day alone is not on it. Every other number, of Unicode category N (decimal
// digits of any script, letter numbers such as roman numerals, and other
// numbers such as superscripts, fractions and circled numbers), is a figure
// and is removed, whatever stands beside it, an identifier's and a
// duration's too: a public text carries no figure, and fails closed
// (docs/live-trading/plan.md section 7).
var (
	isoDate = regexp.MustCompile(`^((?:19|20)\d\d)-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])(?:T(.+))?$`)
	isoTime = regexp.MustCompile(`^(?:[01]\d|2[0-3]):[0-5]\d(?::[0-5]\d)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)?$`)
	// adrNumber is a decision record's number as it follows "ADR ".
	adrNumber = regexp.MustCompile(`^\d{4}$`)
)

// keptToken reports whether core, a whole token less its closing
// punctuation, is on the allow-list (isoDate, adrNumber); afterADR
// is whether "ADR " stands before it.
func keptToken(core string, afterADR bool) bool {
	if afterADR && adrNumber.MatchString(core) {
		return true
	}
	m := isoDate.FindStringSubmatch(core)
	if m == nil {
		return false
	}
	if m[4] != "" && !isoTime.MatchString(m[4]) {
		return false
	}
	// A day the month does not have (2026-02-31) is not a date.
	_, err := time.Parse("2006-01-02", m[1]+"-"+m[2]+"-"+m[3])
	return err == nil
}

// isTokenEdge reports whether c ends a token of free text: ASCII space or
// a bracket, quote or other mark that never joins a number to its
// neighbours. A dash, a colon, a dot, a '#', a sign, a letter and every
// non-ASCII byte join the token, so a figure beside them is judged with them.
func isTokenEdge(c byte) bool {
	return strings.IndexByte(" \t\n\r\f\v()[]{}<>\"'`;=|*", c) >= 0
}

// removeFigures replaces every number in s, of Unicode category N (digits of any script, letter numbers, other numbers), that is
// not a token on the allow-list (keptToken): a quantity, a price, a share
// count, an identifier's number, a duration or any other figure, signed,
// grouped, decimal, in exponent form or a percentage, is removed rather than
// judged, since a public text carries no figure (docs/live-trading/plan.md
// section 7).
func removeFigures(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if isTokenEdge(s[i]) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && !isTokenEdge(s[j]) {
			j++
		}
		b.WriteString(tokenWithoutFigures(s, i, j))
		i = j
	}
	return b.String()
}

// tokenWithoutFigures is the token s[i:j] kept whole when it is on the
// allow-list, less any closing punctuation, and otherwise with each of its
// numbers replaced.
func tokenWithoutFigures(s string, i, j int) string {
	tok := s[i:j]
	if strings.IndexFunc(tok, unicode.IsNumber) < 0 {
		return tok
	}
	if keptToken(strings.TrimRight(tok, ".,:!?"), strings.HasSuffix(s[:i], "ADR ")) {
		return tok
	}
	rs := []rune(tok)
	var b []rune
	for k := 0; k < len(rs); {
		if !unicode.IsNumber(rs[k]) {
			b = append(b, rs[k])
			k++
			continue
		}
		e := k
		for e < len(rs) && (unicode.IsNumber(rs[e]) || (rs[e] == ',' || rs[e] == '.') && e+1 < len(rs) && unicode.IsNumber(rs[e+1])) {
			e++
		}
		// An exponent goes with its mantissa.
		if e+1 < len(rs) && (rs[e] == 'e' || rs[e] == 'E') {
			x := e + 1
			if rs[x] == '+' || rs[x] == '-' {
				x++
			}
			if x < len(rs) && unicode.IsNumber(rs[x]) {
				for x < len(rs) && unicode.IsNumber(rs[x]) {
					x++
				}
				e = x
			}
		}
		// A sign that stands before the figure goes with it.
		if n := len(b); n > 0 && (b[n-1] == '-' || b[n-1] == '+') && (n == 1 || !isAlnumRune(b[n-2])) {
			b = b[:n-1]
		}
		b = append(b, []rune(Redacted)...)
		if e < len(rs) && rs[e] == '%' {
			e++
		}
		k = e
	}
	return string(b)
}

// isAlnumRune reports whether c is a letter, a number of Unicode category N (digits of any script, letter numbers, other numbers), or '_'.
func isAlnumRune(c rune) bool { return unicode.IsLetter(c) || unicode.IsNumber(c) || c == '_' }

// Record renders one journal record on one line: its type and event time,
// then its payload with the value of every withheld field and
// every number replaced, wherever it sits, and every string and field name
// redacted as text in its decoded form; the line is redacted once more as
// written, so a secret is removed escaped or not.
func (r Redactor) Record(e event.Envelope) string {
	line := fmt.Sprintf("%s %s", e.Type, e.EventTime.UTC().Format("2006-01-02T15:04:05Z07:00"))
	if len(e.Payload) == 0 {
		return r.Text(line)
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(e.Payload))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return r.Text(line + " (payload unreadable)")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(r.scrub(v)) // a value decoded from JSON encodes
	return r.Text(line + " " + strings.TrimSpace(out.String()))
}

// scrub replaces, anywhere in v, the value of every withheld field and every
// number, and redacts every string and field name.
func (r Redactor) scrub(v any) any {
	switch t := v.(type) {
	case map[string]any:
		// Names that redact alike are each kept, told apart by a run of
		// "+" (a count would be redacted as a figure), in
		// the names' sorted order, so no value is lost and map iteration never
		// decides the output (.greptile/rules.md).
		names := make([]string, 0, len(t))
		for k := range t {
			names = append(names, k)
		}
		sort.Strings(names)
		out := make(map[string]any, len(t))
		for _, k := range names {
			name := r.Text(k)
			for {
				if _, taken := out[name]; !taken {
					break
				}
				name += "+"
			}
			if Withheld(k) {
				out[name] = Redacted
				continue
			}
			out[name] = r.scrub(t[k])
		}
		return out
	case []any:
		for i := range t {
			t[i] = r.scrub(t[i])
		}
		return t
	case json.Number:
		return Redacted
	case string:
		return r.Text(t)
	}
	return v
}

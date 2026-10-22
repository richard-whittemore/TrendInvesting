package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// Redacted replaces whatever a Redactor takes out of text.
const Redacted = "[redacted]"

// The patterns of what never goes into text bound for a place the owner does
// not control the readers of (docs/live-trading/plan.md section 7: the public
// text carries no balance; docs/manual/alerts-and-dashboard.md): an IBKR
// account id (U or DU and digits, as the broker issues them), a GitHub token,
// a Tailscale host name or address (the 100.64.0.0/10 range), and a dollar
// figure.
var redactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:DU|U)\d{4,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bgh[opsur]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9.-]*\.ts\.net\b`),
	regexp.MustCompile(`\b100\.(?:6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.\d{1,3}\.\d{1,3}\b`),
	regexp.MustCompile(`[-+]?\$\s?\d[\d,]*(?:\.\d+)?`),
}

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
	for _, l := range r.literals {
		s = strings.ReplaceAll(s, l, Redacted)
	}
	for _, w := range r.words {
		s = replaceWord(s, w)
	}
	for _, p := range redactPatterns {
		s = p.ReplaceAllString(s, Redacted)
	}
	return removeFigures(s)
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

// removeFigures replaces every number in s that is not part of a date, a
// time, a word, a record number (#4) or an ADR's number: a quantity, a price, a share count
// or any other figure, signed, grouped, decimal or a percentage, is removed
// rather than judged, since a public text carries no figure
// (docs/live-trading/plan.md section 7).
func removeFigures(s string) string {
	var b []byte
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			b = append(b, s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && (isDigit(s[j]) || (s[j] == ',' || s[j] == '.') && j+1 < len(s) && isDigit(s[j+1])) {
			j++
		}
		if keptNumber(s, i, j) {
			b = append(b, s[i:j]...)
		} else {
			// A sign that stands before the figure goes with it.
			if n := len(b); n > 0 && (b[n-1] == '-' || b[n-1] == '+') && (n == 1 || !isAlnum(b[n-2])) {
				b = b[:n-1]
			}
			b = append(b, Redacted...)
			if j < len(s) && s[j] == '%' {
				j++
			}
		}
		i = j
	}
	return string(b)
}

// keptNumber reports whether the number s[i:j] is part of a date, a time, a
// word, a record number or an ADR's number rather than a figure.
func keptNumber(s string, i, j int) bool {
	if i > 0 {
		before := s[i-1]
		switch {
		case isLetter(before), before == '#':
			return true
		case strings.HasSuffix(s[:i], "ADR "):
			return true // a decision record's number, such as ADR 0019
		case before == '-' && i > 1 && isLetter(s[i-2]):
			return true // an identifier such as tw-17
		case (before == '-' || before == ':' || before == '+' || before == '.') && i > 1 && isDigit(s[i-2]):
			return true // a date's, a time's or a zone's later part
		}
	}
	if j < len(s) {
		after := s[j]
		switch {
		case isLetter(after):
			return true // a word such as 30m, or a date's T
		case (after == '-' || after == ':') && j+1 < len(s) && isDigit(s[j+1]):
			return true // a date's or a time's first part
		}
	}
	return false
}

// Record renders one journal record on one line: its sequence, type and
// event time, then its payload with the value of every withheld field and
// every number replaced, wherever it sits, and every string and field name
// redacted as text in its decoded form; the line is redacted once more as
// written, so a secret is removed escaped or not.
func (r Redactor) Record(e event.Envelope) string {
	line := fmt.Sprintf("#%d %s %s", e.Sequence, e.Type, e.EventTime.UTC().Format("2006-01-02T15:04:05Z07:00"))
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
		out := make(map[string]any, len(t))
		for k, child := range t {
			if Withheld(k) {
				out[r.Text(k)] = Redacted
				continue
			}
			out[r.Text(k)] = r.scrub(child)
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

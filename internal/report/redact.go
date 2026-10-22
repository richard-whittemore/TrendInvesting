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

// balanceKeys are the parts of a payload field's name that mark its value as
// a balance, a figure of the account's size or a name of the account: the
// field's value is replaced wherever it sits in the record.
var balanceKeys = []string{
	"cash", "equity", "liquid", "balance", "margin", "notional", "value", "amount", "account",
	"starting", "book", "residual", "expected", "observed", "figures", "commission", "fee",
	"cost", "realised", "result", "exposure", "buying", "token", "secret", "password", "key",
}

// minLiteral is the shortest literal value a Redactor removes.
const minLiteral = 6

// Redactor takes secrets and account identifiers out of text and journal
// records before they leave the server.
type Redactor struct{ literals []string }

// NewRedactor returns a Redactor that also removes each of the given literal
// values, such as the configured secrets and this host's name. Empty values
// and values shorter than minLiteral are ignored: a short one is no secret
// and would mangle ordinary words.
func NewRedactor(literals []string) Redactor {
	var kept []string
	for _, l := range literals {
		if l = strings.TrimSpace(l); len(l) >= minLiteral {
			kept = append(kept, l)
		}
	}
	// The longest first, so a value that contains another is removed whole.
	sort.Slice(kept, func(i, j int) bool { return len(kept[i]) > len(kept[j]) })
	return Redactor{literals: kept}
}

// Text returns s with every literal value and every pattern removed.
func (r Redactor) Text(s string) string {
	for _, l := range r.literals {
		s = strings.ReplaceAll(s, l, Redacted)
	}
	for _, p := range redactPatterns {
		s = p.ReplaceAllString(s, Redacted)
	}
	return s
}

// Record renders one journal record on one line: its sequence, type and
// event time, then its payload with the value of every balance-bearing or
// account-naming field replaced and the rest redacted as text.
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
	out, _ := json.Marshal(scrub(v)) // a value decoded from JSON marshals
	return r.Text(line + " " + string(out))
}

// scrub replaces, anywhere in v, the value of every field whose name marks
// it as balance-bearing.
func scrub(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if balanceKey(k) {
				t[k] = Redacted
				continue
			}
			t[k] = scrub(child)
		}
		return t
	case []any:
		for i := range t {
			t[i] = scrub(t[i])
		}
		return t
	}
	return v
}

func balanceKey(name string) bool {
	name = strings.ToLower(name)
	for _, part := range balanceKeys {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

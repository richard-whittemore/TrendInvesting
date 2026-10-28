package report_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// TestRedactionKeepsSecretsAccountsAndHostsOutOfText: text bound for a
// public place carries no secret value, no IBKR account id, no token, no
// Tailscale host name or address and no dollar figure (docs/live-trading/plan.md
// section 7: the public text carries no balance).
func TestRedactionKeepsSecretsAccountsAndHostsOutOfText(t *testing.T) {
	r := report.NewRedactor([]string{"s3cret-topic-value", "myhost"})
	leaks := []string{
		"s3cret-topic-value", "DU1234567", "U7654321", "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"github_pat_11ABCDEFG0abcdefghijklmnopqrstu_vwxyz0123456789", "lightsail.tail1a2b3.ts.net",
		"100.101.102.103", "$12,345.67", "myhost",
	}
	in := "topic s3cret-topic-value account DU1234567 and U7654321 tokens ghp_abcdefghijklmnopqrstuvwxyz0123456789 " +
		"github_pat_11ABCDEFG0abcdefghijklmnopqrstu_vwxyz0123456789 host lightsail.tail1a2b3.ts.net at 100.101.102.103 " +
		"cash $12,345.67 on myhost"
	got := r.Text(in)
	for _, leak := range leaks {
		if strings.Contains(got, leak) {
			t.Errorf("redacted text still holds %q:\n%s", leak, got)
		}
	}
	for _, kept := range []string{"topic", "account", "tokens", "cash"} {
		if !strings.Contains(got, kept) {
			t.Errorf("redaction removed ordinary word %q:\n%s", kept, got)
		}
	}
}

func TestRedactionLeavesOrdinaryTextAlone(t *testing.T) {
	r := report.NewRedactor(nil)
	in := "joint: order refused (order-size) for SPY at 2024-01-02T21:00:00Z; docs/runbooks/reconciliation.md"
	if got := r.Text(in); got != in {
		t.Fatalf("ordinary text changed:\n%s", got)
	}
}

// TestRecordRedactionDropsBalanceBearingFields: a journal record is shown by
// its sequence, type, time and payload, with the value of every
// balance-bearing or account-naming field replaced.
func TestRecordRedactionDropsBalanceBearingFields(t *testing.T) {
	payload, err := json.Marshal(event.AccountPositionsSnapshotPayload{AccountID: "DU1234567", AsOf: time.Date(2024, 1, 2, 21, 0, 0, 0, time.UTC), Currency: "USD", NetLiquidation: 987654.32})
	if err != nil {
		t.Fatal(err)
	}
	e := event.Envelope{ID: "snap", Type: event.AccountSnapshotEventType, Sequence: 7, EventTime: time.Date(2024, 1, 2, 21, 0, 0, 0, time.UTC), Payload: payload}
	got := report.NewRedactor(nil).Record(e)
	for _, leak := range []string{"DU1234567", "987654", "987,654"} {
		if strings.Contains(got, leak) {
			t.Errorf("record still holds %q:\n%s", leak, got)
		}
	}
	for _, want := range []string{event.AccountSnapshotEventType, "2024-01-02T21:00:00Z", "USD"} {
		if !strings.Contains(got, want) {
			t.Errorf("record lacks %q:\n%s", want, got)
		}
	}
}

func TestRecordRedactionRedactsNestedFieldsAndTextValues(t *testing.T) {
	payload := `{"reason":"order-size","figures":{"net_liquidation":4000,"order_fraction":0.48},"detail":"DU1234567 holds $4,000.00","orders":[{"instrument_id":"SPY","available_cash":12.5}]}`
	e := event.Envelope{Type: event.OrderRefusedEventType, Sequence: 4, Payload: json.RawMessage(payload)}
	got := report.NewRedactor(nil).Record(e)
	for _, leak := range []string{"DU1234567", "4000", "4,000", "12.5", "0.48"} {
		if strings.Contains(got, leak) {
			t.Errorf("record still holds %q:\n%s", leak, got)
		}
	}
	for _, want := range []string{"order-size", "SPY"} {
		if !strings.Contains(got, want) {
			t.Errorf("record lacks %q:\n%s", want, got)
		}
	}
}

func TestRecordRedactionHandlesNoPayloadAndAnUnreadableOne(t *testing.T) {
	r := report.NewRedactor(nil)
	for name, payload := range map[string]string{"none": "", "unreadable": "{not json DU1234567"} {
		e := event.Envelope{Type: "x.y", Sequence: 1, Payload: json.RawMessage(payload)}
		got := r.Record(e)
		if !strings.HasPrefix(got, "x.y ") || strings.Contains(got, "DU1234567") {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// TestRecordWithholdsQuantitiesPricesAndPositionsAtAnyDepth: a public record
// carries no quantity, price, limit, fill, share count, position, size or
// P&L, however deep the field sits, and no number at all from its payload
// (docs/live-trading/plan.md section 7).
func TestRecordWithholdsQuantitiesPricesAndPositionsAtAnyDepth(t *testing.T) {
	payload := `{"instrument_id":"SPY","quantity":731911,"limit_price":826417.5,"order":{"fill_price":554433.2,"shares":918273,"size":664422},` +
		`"positions":[{"instrument_id":"QQQ","qty":445566}],"risk":{"unrealized_pnl":-337711.25,"pnl":228844},"note":"bought 776655 at 112233.44",` +
		`"orders_in_decision":918811,"tag":"qty:747474","final":true,"none":null}`
	got := report.NewRedactor(nil).Record(event.Envelope{Type: event.FillEventType, Sequence: 9, Payload: json.RawMessage(payload)})
	for _, leak := range []string{"731911", "826417", "554433", "918273", "664422", "445566", "337711", "228844", "776655", "112233", "918811", "747474"} {
		if strings.Contains(got, leak) {
			t.Errorf("record still holds %q:\n%s", leak, got)
		}
	}
	for _, want := range []string{event.FillEventType, "SPY", "bought", `"final":true`, `"none":null`} {
		if !strings.Contains(got, want) {
			t.Errorf("record lacks %q:\n%s", want, got)
		}
	}
}

// TestTextRemovesEveryBareFigure: free text bound for a public place keeps
// its words, times and record numbers and loses every figure, signed,
// decimal, grouped or a percentage.
func TestTextRemovesEveryBareFigure(t *testing.T) {
	in := "buy 7391 SPY at 8264.17 (-0.48, +12%) of 1,234,567 at 2024-01-02T21:00:00Z, record #4, qty:39 and 39/100 (ADR 0019), order tw-17, every 30m."
	got := report.NewRedactor(nil).Text(in)
	for _, leak := range []string{"7391", "8264", "0.48", "12%", "1,234", "567", "qty:39", " 39/", "100", "#4", "tw-17", "30m"} {
		if strings.Contains(got, leak) {
			t.Errorf("text still holds %q:\n%s", leak, got)
		}
	}
	for _, want := range []string{"buy", "SPY", "2024-01-02T21:00:00Z", "ADR 0019"} {
		if !strings.Contains(got, want) {
			t.Errorf("text lacks %q:\n%s", want, got)
		}
	}
}

// TestAnEscapedSecretIsRedacted: a secret is removed in its decoded form and
// in its JSON-escaped forms, from text and from a record's strings and names.
func TestAnEscapedSecretIsRedacted(t *testing.T) {
	r := report.NewRedactor([]string{`abc&def"123`})
	for _, in := range []string{`saw abc\u0026def\"123`, `saw abc&def\"123`, `saw abc&def"123`} {
		if got := r.Text(in); strings.Contains(got, "abc") {
			t.Errorf("Text(%q) = %q", in, got)
		}
	}
	got := r.Record(event.Envelope{Type: "x.y", Sequence: 1, Payload: json.RawMessage(`{"detail":"saw abc\u0026def\"123","abc\u0026def\"123":"name"}`)})
	if strings.Contains(got, "abc") {
		t.Errorf("record still holds the secret:\n%s", got)
	}
}

// TestShortSecretsAndHostNamesAreRedactedAsWords: a short secret or host name
// is never skipped; it is removed where it stands as a word, so ordinary
// words that merely contain it are kept.
func TestShortSecretsAndHostNamesAreRedactedAsWords(t *testing.T) {
	r := report.NewRedactor([]string{"zq7", "trade", "a", "", "  "})
	got := r.Text("host trade said zq7; the trader saw zq7x on trade.")
	for _, leak := range []string{"host trade", " zq7;", "on trade"} {
		if strings.Contains(got, leak) {
			t.Errorf("text still holds %q:\n%s", leak, got)
		}
	}
	for _, want := range []string{"trader", "saw zq", "said"} {
		if !strings.Contains(got, want) {
			t.Errorf("text lacks %q:\n%s", want, got)
		}
	}
}

// TestWithheldNamesTheFigureFieldsAndColumns: one rule decides both a
// record's withheld fields and the query columns left out.
func TestWithheldNamesTheFigureFieldsAndColumns(t *testing.T) {
	for _, name := range []string{"quantity", "price", "limit_price", "fill_price", "fills", "shares", "position", "positions", "size", "pnl", "unrealized_pnl", "account", "commission", "snapshot", "change", "journal", "net_liquidation"} {
		if !report.Withheld(name) {
			t.Errorf("%s is not withheld", name)
		}
	}
	for _, name := range []string{"seq", "time", "instrument", "instrument_id", "event", "side", "note", "reason", "detail", "kind", "status", "input", "action", "by"} {
		if report.Withheld(name) {
			t.Errorf("%s is withheld", name)
		}
	}
}

// TestTextRedactsAFigureWhateverItsNeighbours: a number is a figure whatever
// stands beside it, a letter, a currency sign, a non-ASCII character, an
// exponent, a dash, a colon or a '#' (docs/live-trading/plan.md section 7:
// the public text carries no figure); no digit of any of these survives.
func TestTextRedactsAFigureWhateverItsNeighbours(t *testing.T) {
	r := report.NewRedactor(nil)
	for _, in := range []string{
		"412.35€", "412.35USD", "USD412.35", "£400", "400×", "\u2212400", "100-200", "100\u2014200", "100\u2013200",
		"412.35-415.00", "5:412.35", "123-45-67", "sell-400", "qty-412.35", "limit-412.35", "#400", "#4", "1.234567e+06",
		"1.234567E-06", "100\u00a0shares", "x400", "400x", "tw-1234", "2024-13-45", "25:99", "ADR 26", "ADR-0026",
		"ADR 00261", "999m", "1.5s", "30mm", "30m5", "2024-01-02T25:00:00Z", "v2", "sha256",
		// No identifier or duration is kept: a public issue needs neither, and
		// either can carry a small figure.
		"tw-17", "qty-50", "fill-3", "x-42", "5m", "99s", "250ms", "1h30m",
		// A date or time is kept only when it is on the calendar and the
		// 24-hour clock, in a year of 19xx or 20xx, and with no fraction of a
		// second, whose digits could carry a figure.
		"2026-02-31", "2026-04-31", "2025-02-29", "5000-10-10", "0412-12-31", "2024-01-02T21:00:00.123456789Z", "21:00:00.5",
		// Digits of any script are digits.
		"\u0664\u0660\u0660", "\uff14\uff10\uff10", "\u00b2", "400\u00b2", "\u00bd", "\u2460", "\u2167", "x\u2462\u2463", "ADR \u2460\u2461\u2462\u2463",
		// A time of day stands alone is a figure; a date followed by a bare T,
		// with no time, is neither a date nor a timestamp.
		"21:00", "21:00:00", "00:00", "23:59:59Z", "2026-10-22T", "2026-10-22T.", "\u096a\u0966\u0966", "x\u0664\u0662", "sell-\u0664\u0660", "2024-01-0\u0662", "ADR \u0660\u0660\u0661\u0669",
	} {
		got := r.Text("saw " + in + " here")
		if anyDigit.MatchString(got) {
			t.Errorf("Text(%q) = %q: a digit survived", in, got)
		}
		if !strings.Contains(got, report.Redacted) {
			t.Errorf("Text(%q) = %q: nothing was redacted", in, got)
		}
	}
}

// TestTextKeepsOnlyTheAllowedShapes: the only numbers kept are ISO dates and
// times that are on the calendar and the 24-hour clock, and ADR numbers
// written "ADR 0026", each matched as a whole token.
func TestTextKeepsOnlyTheAllowedShapes(t *testing.T) {
	r := report.NewRedactor(nil)
	for _, in := range []string{
		"2024-01-02", "2024-02-29", "1998-12-31", "2024-01-02T21:00:00Z", "2024-01-02T21:00:00+02:00", "2024-01-02T23:59:59-05:00",
		"2024-01-02T21:00Z", "(ADR 0026)", "ADR 0019,",
	} {
		if got := r.Text("at " + in + " now"); got != "at "+in+" now" {
			t.Errorf("Text(%q) = %q, want it kept", in, got)
		}
	}
}

// TestNoRandomFigureSurvivesBesideAnything: a random figure of one to seven
// digits, whole, decimal, grouped, signed or in exponent form, in ASCII
// digits or another script's, placed beside letters, signs, dashes, colons,
// '#' and non-ASCII characters, leaves no digit of any length but what the
// allow-list keeps ("ADR nnnn").
func TestNoRandomFigureSurvivesBesideAnything(t *testing.T) {
	rng := rand.New(rand.NewSource(20261022))
	r := report.NewRedactor(nil)
	sides := []string{"", " ", "x", "USD", "qty", "qty-", "sell-", "tw-", "-", "\u2014", "\u2013", "\u2212", "\u20ac", "\u00a3", "\u00d7",
		"\u00a0", ":", "#", "+", "e", "_", "/", ".", ",", "(", "\"", "%", "$", "\u00e9", "\u4e2d", "ADR ", "m", "s", "ms", "h"}
	scripts := []rune{'0', '\u0660', '\u0966', '\uff10'}
	for n := 0; n < 4000; n++ {
		digits := 1 + rng.Intn(7)
		lo := int64(1)
		for range digits - 1 {
			lo *= 10
		}
		whole := lo + rng.Int63n(9*lo)
		if digits == 1 {
			whole = rng.Int63n(10)
		}
		var figure string
		switch rng.Intn(5) {
		case 0:
			figure = strconv.FormatInt(whole, 10)
		case 1:
			figure = strconv.FormatFloat(float64(whole)+float64(rng.Intn(99)+1)/100, 'f', 2, 64)
		case 2:
			figure = strconv.FormatFloat(float64(whole)*1.5, 'g', -1, 64)
		case 3:
			figure = fmt.Sprintf("%v", float64(whole)*1000)
		default:
			figure = fmt.Sprintf("%d,%03d", whole/1000, whole%1000)
		}
		if zero := scripts[rng.Intn(len(scripts))]; zero != '0' {
			figure = strings.Map(func(c rune) rune {
				if c >= '0' && c <= '9' {
					return zero + c - '0'
				}
				return c
			}, figure)
		}
		in := sides[rng.Intn(len(sides))] + figure + sides[rng.Intn(len(sides))]
		got := r.Text("saw " + in + " here")
		if rest := allowed.ReplaceAllString(got, ""); anyDigit.MatchString(rest) {
			t.Fatalf("Text(%q) = %q: a digit survived outside the allow-list", in, got)
		}
	}
}

// anyDigit is a number of any Unicode category N (Nd, Nl, No); allowed is the one allow-listed shape
// a random figure beside random text can make, an ADR's number.
var (
	anyDigit = regexp.MustCompile(`\p{N}`)
	allowed  = regexp.MustCompile(`ADR \d{4}`)
)

// TestFieldNamesThatRedactAlikeAreAllKept: two field names that redact to
// the same text both keep their values, in an order that never depends on
// map iteration (.greptile/rules.md: map iteration never affects output).
func TestFieldNamesThatRedactAlikeAreAllKept(t *testing.T) {
	r := report.NewRedactor([]string{"alpha1x", "alpha2x", "alpha3x"})
	e := event.Envelope{Type: "x.y", Payload: json.RawMessage(`{"alpha1x":"accepted","alpha2x":"refused","alpha3x":"held"}`)}
	first := r.Record(e)
	for _, want := range []string{"accepted", "refused", "held"} {
		if !strings.Contains(first, want) {
			t.Errorf("record lost %q:\n%s", want, first)
		}
	}
	for range 50 {
		if got := r.Record(e); got != first {
			t.Fatalf("record changed between renderings:\n%s\n%s", first, got)
		}
	}
}

// TestIdentifiersTakesOutWhatNamesTheAccountButKeepsFigures: code bound
// for a public repository, whose numbers are its own, loses every secret,
// account id, token and Tailscale host or address, exactly as Text does,
// and keeps its numbers.
func TestIdentifiersTakesOutWhatNamesTheAccountButKeepsFigures(t *testing.T) {
	r := report.NewRedactor([]string{"s3cret-topic-value", "myhost"})
	in := "topic s3cret-topic-value account DU1234567 and U7654321 tokens ghp_abcdefghijklmnopqrstuvwxyz0123456789 " +
		"github_pat_11ABCDEFG0abcdefghijklmnopqrstu_vwxyz0123456789 host lightsail.tail1a2b3.ts.net at 100.101.102.103 on myhost; " +
		"bought 39 SPY at 101.25, cash $12,345.67"
	got := r.Identifiers(in)
	for _, leak := range []string{"s3cret-topic-value", "DU1234567", "U7654321", "ghp_", "github_pat_", "ts.net", "100.101.102.103", "myhost"} {
		if strings.Contains(got, leak) {
			t.Errorf("identifiers left %q:\n%s", leak, got)
		}
	}
	for _, kept := range []string{"bought 39 SPY at 101.25", "$12,345.67"} {
		if !strings.Contains(got, kept) {
			t.Errorf("identifiers removed the figure %q:\n%s", kept, got)
		}
	}
	if r.Identifiers("plain text 42") != "plain text 42" {
		t.Error("text with no identifier changed")
	}
}

// TestRedactionCoversPaperAccountsWithLetters: an IBKR paper account
// number can carry letters after DU (DUX1234567), as the adapter's masking
// already reads it (adapter/ibkr/masking.py), so the IRAs' paper accounts
// never reach a public place either (Boost lesson L12); a word of capitals
// with no digit, such as USD or UTILITIES, is not one. The whole number
// goes, so none of its letters is left beside a redacted figure.
func TestRedactionCoversPaperAccountsWithLetters(t *testing.T) {
	r := report.NewRedactor(nil)
	for _, account := range []string{"DUX1234567", "DUAB12C34", "DU12AB345"} {
		if got, want := r.Text("account "+account+" decided"), "account "+report.Redacted+" decided"; got != want {
			t.Errorf("%s is not redacted whole: %s, want %s", account, got, want)
		}
	}
	for _, word := range []string{"USD", "UTILITIES", "DURATION"} {
		if got := r.Text("held " + word); got != "held "+word {
			t.Errorf("%s was redacted: %s", word, got)
		}
	}
}

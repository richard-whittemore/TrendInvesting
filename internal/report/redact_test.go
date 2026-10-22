package report_test

import (
	"encoding/json"
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
	for _, want := range []string{"7", event.AccountSnapshotEventType, "2024-01-02T21:00:00Z", "USD"} {
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
		if !strings.Contains(got, "#1 x.y") || strings.Contains(got, "DU1234567") {
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
	for _, want := range []string{"#9", event.FillEventType, "SPY", "bought", `"final":true`, `"none":null`} {
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
	for _, leak := range []string{"7391", "8264", "0.48", "12%", "1,234", "567", "qty:39", " 39/", "100"} {
		if strings.Contains(got, leak) {
			t.Errorf("text still holds %q:\n%s", leak, got)
		}
	}
	for _, want := range []string{"buy", "SPY", "2024-01-02T21:00:00Z", "#4", "ADR 0019", "tw-17", "30m"} {
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
	for _, want := range []string{"trader", "zq7x", "said"} {
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

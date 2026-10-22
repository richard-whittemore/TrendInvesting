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

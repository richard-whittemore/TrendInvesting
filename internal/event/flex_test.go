package event_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// ledgerLine is one line as the Flex reader writes it (adapter/ibkr/flex.py
// _record): a synthetic deposit of $1,000 on a made-up day.
const ledgerLine = `{"event_time": "2026-06-02T04:00:00Z", "id": "ibkr-flex:T1", "payload": {"amount": 1000.0, "as_of": "2026-06-02T04:00:00Z", "currency": "USD", "equity_before": 20000.0, "transaction_id": "T1"}, "schema_version": 2, "source": "ibkr-flex", "type": "account.cash-movement"}`

func TestTheFlexLedgerIsReadLineByLineAsTheReaderWritesIt(t *testing.T) {
	t.Parallel()

	noted := strings.Replace(ledgerLine, `"type"`, `"note": "an earlier transfer in the same interval was written first; its equity_before omits this one and is not revised", "type"`, 1)
	if _, err := event.DecodeFlexLedger([]byte(noted + "\n")); err != nil {
		t.Errorf("a record with the reader's note: %v", err)
	}
	second := strings.NewReplacer(`T1"`, `T2"`, `1000.0`, `-250.5`, `"equity_before": 20000.0`, `"equity_before": 21000.0, "equity_before_basis": "same-day-snapshot-less-amount"`).Replace(ledgerLine)
	records, err := event.DecodeFlexLedger([]byte(ledgerLine + "\n" + second + "\n"))
	if err != nil {
		t.Fatalf("DecodeFlexLedger: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	first := records[0]
	if first.ID != "ibkr-flex:T1" || !first.Payload.AsOf.Equal(time.Date(2026, 6, 2, 4, 0, 0, 0, time.UTC)) || first.Payload.Amount != 1000 || first.Payload.TransactionID != "T1" {
		t.Errorf("first record: %+v", first)
	}
	if records[1].Payload.Amount != -250.5 || records[1].Payload.EquityBeforeBasis != event.EquityBeforeSameDaySnapshotLessAmount {
		t.Errorf("second record: %+v", records[1])
	}
	if empty, err := event.DecodeFlexLedger(nil); err != nil || len(empty) != 0 {
		t.Errorf("an empty ledger holds nothing: %v %v", empty, err)
	}
}

func TestALedgerLineNotAsTheReaderWritesItRefusesTheLedger(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, raw, want string }{
		{"no final newline", ledgerLine, "whole line"},
		{"not json", "nope\n", "line 1"},
		{"another type", strings.Replace(ledgerLine, `"account.cash-movement"`, `"account.snapshot"`, 1) + "\n", "line 1"},
		{"version 1", strings.Replace(ledgerLine, `"schema_version": 2`, `"schema_version": 1`, 1) + "\n", "line 1"},
		{"another source", strings.Replace(ledgerLine, `"ibkr-flex"`, `"fixture"`, 1) + "\n", "line 1"},
		{"an id not the transaction's", strings.Replace(ledgerLine, `"ibkr-flex:T1"`, `"ibkr-flex:T9"`, 1) + "\n", "line 1"},
		{"an event time not the as of", strings.Replace(ledgerLine, `"event_time": "2026-06-02T04:00:00Z"`, `"event_time": "2026-06-03T04:00:00Z"`, 1) + "\n", "line 1"},
		{"an unknown field", strings.Replace(ledgerLine, `"type"`, `"extra": 1, "type"`, 1) + "\n", "line 1"},
		{"an unknown payload field", strings.Replace(ledgerLine, `"amount"`, `"account_id": "x", "amount"`, 1) + "\n", "line 1"},
		{"a zero amount", strings.Replace(ledgerLine, `1000.0`, `0`, 1) + "\n", "line 1"},
		{"a line twice", ledgerLine + "\n" + ledgerLine + "\n", "line 2"},
		{"a note the reader does not write", strings.Replace(ledgerLine, `"type"`, `"note": "edited by hand", "type"`, 1) + "\n", "line 1"},
		{"two values on a line", ledgerLine + " " + ledgerLine + "\n", "line 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := event.DecodeFlexLedger([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DecodeFlexLedger = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

func TestTheFlexStatusIsReadWithItsCoverage(t *testing.T) {
	t.Parallel()

	read, err := event.DecodeFlexStatus([]byte(`{"checked_at": "2026-06-02T01:30:00Z", "covered_to": "20260601", "postings": 3, "state": "read", "written": 1}`+"\n"), "ira")
	if err != nil {
		t.Fatalf("DecodeFlexStatus: %v", err)
	}
	want := event.FlexStatusPayload{AccountID: "ira", State: event.FlexStateRead, CheckedAt: time.Date(2026, 6, 2, 1, 30, 0, 0, time.UTC), CoveredTo: "20260601"}
	if read != want {
		t.Fatalf("got %+v, want %+v", read, want)
	}
	// The day of a New York time: 20260601 is covered, 20260602 is not.
	if !read.Covers(time.Date(2026, 6, 2, 3, 59, 0, 0, time.UTC)) || read.Covers(time.Date(2026, 6, 2, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("covered to 20260601 covers 1 June in New York and not 2 June")
	}
	aside, err := event.DecodeFlexStatus([]byte(`{"checked_at": "2026-06-02T01:30:00Z", "code": "1019", "reason": "busy", "state": "set-aside"}`), "ira")
	if err != nil || aside.State != event.FlexStateSetAside || aside.CoveredTo != "" {
		t.Fatalf("a set-aside status: %+v %v", aside, err)
	}
	if aside.Covers(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a status that is not read covers no day")
	}
}

func TestAFlexStatusNotAsTheReaderWritesItIsRefused(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`nope`,
		`{"checked_at": "2026-06-02T01:30:00Z", "state": "read"}`,
		`{"checked_at": "2026-06-02T01:30:00Z", "covered_to": "2026-06-01", "state": "read"}`,
		`{"checked_at": "2026-06-02T01:30:00Z", "covered_to": "20260601", "state": "running"}`,
		`{"checked_at": "2026-06-02T01:30:00Z", "state": "asleep"}`,
		`{"state": "running"}`,
	} {
		if _, err := event.DecodeFlexStatus([]byte(raw), "ira"); err == nil {
			t.Errorf("DecodeFlexStatus(%s) was accepted", raw)
		}
	}
	if err := (event.FlexStatusPayload{State: event.FlexStateRunning, CheckedAt: time.Now()}).Validate(); err == nil {
		t.Errorf("a status names its account")
	}
}

func validFlexStatus() event.FlexStatusPayload {
	return event.FlexStatusPayload{AccountID: "ira", State: event.FlexStateRead, CheckedAt: time.Date(2026, 6, 2, 1, 30, 0, 0, time.UTC), CoveredTo: "20260601"}
}

// pendingResult is a reconciliation that found a cash difference only, as
// Classify states one.
func pendingResult() event.ReconciliationResultPayload {
	return event.ReconciliationResultPayload{
		AccountID: "ira", ReconciliationID: "r1", InputID: "in1", AnchorID: "s1",
		Classification: event.ReconciliationPendingCashRead, Action: event.ReconciliationActionAlert,
		CashCompared: true, Currency: "USD",
		Components: []event.ReconciliationComponent{
			{Component: event.ReconciliationComponentCash, Book: 100, Expected: 100, Observed: 200, Residual: 100, Explanations: []string{}},
			{Component: event.ReconciliationComponentPosition, InstrumentID: "SPY", Book: 1, Expected: 1, Observed: 1, Explanations: []string{}, Explained: true},
		},
		Detail: "pending-cash-read: total cash",
	}
}

func TestAPendingCashReadIsACashOnlyDifferenceThatAlertsAndScopesNothing(t *testing.T) {
	t.Parallel()

	if err := pendingResult().Validate(); err != nil {
		t.Fatalf("a pending cash read: %v", err)
	}
	for name, mutate := range map[string]func(*event.ReconciliationResultPayload){
		"degrades": func(p *event.ReconciliationResultPayload) { p.Action = event.ReconciliationActionDegrade },
		"scopes cash": func(p *event.ReconciliationResultPayload) {
			p.ScopeCashComponents = []string{event.ReconciliationComponentCash}
		},
		"cash explained": func(p *event.ReconciliationResultPayload) {
			p.Components[0].Observed, p.Components[0].Residual, p.Components[0].Explained = 100, 0, true
		},
		"a holding differs": func(p *event.ReconciliationResultPayload) {
			p.Components[1].Observed, p.Components[1].Residual, p.Components[1].Explained = 2, 1, false
		},
		"no cash compared": func(p *event.ReconciliationResultPayload) {
			p.CashCompared, p.Currency, p.Components = false, "", p.Components[1:]
		},
	} {
		p := pendingResult()
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: a pending cash read that %s was accepted", name, name)
		}
	}
	alert := pendingResult()
	alert.Classification = event.ReconciliationUnexplained
	if err := alert.Validate(); err == nil {
		t.Errorf("only a pending cash read alerts")
	}
}

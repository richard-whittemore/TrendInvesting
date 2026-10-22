package event_test

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validOperatorRunApproved() event.OperatorRunApprovedPayload {
	return event.OperatorRunApprovedPayload{
		AccountID:            "joint",
		PreviousLog:          "run-2026-10-16.log",
		PreviousLogFinalHash: "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		IncidentID:           "",
		ApprovedBy:           "owner",
		ApprovedAt:           time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
		Boost:                event.BoostStateInForce{Latched: new(true)},
	}
}

func validOperatorRunApprovedInIncident() event.OperatorRunApprovedPayload {
	p := validOperatorRunApproved()
	p.IncidentInForce = &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(true), SaleClientOrderID: "joint-sale-1"}
	return p
}

// TestOperatorRunApprovedValidate: an approval names the account, the
// previous run's durable log by file name and the hash of its last committed
// record, who approved and when; the incident is optional.
func TestOperatorRunApprovedValidate(t *testing.T) {
	v := validOperatorRunApproved()
	if err := v.Validate(); err != nil {
		t.Fatalf("the fixture is invalid: %v", err)
	}
	withIncident := v
	withIncident.IncidentID = "incident-2026-10-16-reconciliation"
	if err := withIncident.Validate(); err != nil {
		t.Errorf("an approval naming an incident: %v", err)
	}
	type P = event.OperatorRunApprovedPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "previous log", v, func(p *P) { p.PreviousLog = "" }, "previous log")
	expectInvalid(t, "previous log path", v, func(p *P) { p.PreviousLog = "../engines/run.log" }, "file name")
	expectInvalid(t, "previous log dot", v, func(p *P) { p.PreviousLog = ".." }, "file name")
	expectInvalid(t, "hash", v, func(p *P) { p.PreviousLogFinalHash = "" }, "final hash")
	expectInvalid(t, "hash prefix", v, func(p *P) {
		p.PreviousLogFinalHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	}, "final hash")
	expectInvalid(t, "hash short", v, func(p *P) { p.PreviousLogFinalHash = "sha256:0123" }, "final hash")
	expectInvalid(t, "hash upper", v, func(p *P) {
		p.PreviousLogFinalHash = "sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef"
	}, "final hash")
	expectInvalid(t, "approved by", v, func(p *P) { p.ApprovedBy = "" }, "approved by")
	expectInvalid(t, "approved at", v, func(p *P) { p.ApprovedAt = time.Time{} }, "approved at")
}

// TestAnApprovalStatesBoostsLatchExplicitly: the latch is state a fresh run
// cannot recompute from bars (ADR 0026, point 5), so an approval that does
// not state it is refused; a missing latch is never read as "not latched",
// which would take a boosted account from 1.47 to 0.98 unasked.
func TestAnApprovalStatesBoostsLatchExplicitly(t *testing.T) {
	type P = event.OperatorRunApprovedPayload
	expectInvalid(t, "latch", validOperatorRunApproved(), func(p *P) { p.Boost.Latched = nil }, "latched")
	unlatched := validOperatorRunApproved()
	unlatched.Boost.Latched = new(false)
	if err := unlatched.Validate(); err != nil {
		t.Errorf("an approval stating an unlatched boost: %v", err)
	}
}

// TestAnApprovalCarriesTheIncidentSaleAttempt: a fresh run that continues an
// incident carries the incident's ID and whether its one sale back to 98%
// was attempted (ADR 0019, amended 2026-10-15: one attempt per incident),
// and the attempt's client order ID when one was sent, so the fresh run never
// attempts that sale twice.
func TestAnApprovalCarriesTheIncidentSaleAttempt(t *testing.T) {
	type P = event.OperatorRunApprovedPayload
	v := validOperatorRunApproved()
	for name, in := range map[string]event.IncidentInForce{
		"not attempted":       {IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(false)},
		"attempted, withheld": {IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(true)},
		"attempted, sent":     {IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(true), SaleClientOrderID: "joint-sale-1"},
	} {
		ok := v
		ok.IncidentInForce = &in
		if err := ok.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	in := func(mutate func(*event.IncidentInForce)) func(*P) {
		return func(p *P) {
			i := event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(true), SaleClientOrderID: "joint-sale-1"}
			mutate(&i)
			p.IncidentInForce = &i
		}
	}
	expectInvalid(t, "incident id", v, in(func(i *event.IncidentInForce) { i.IncidentID = "" }), "incident in force")
	expectInvalid(t, "sale attempted", v, in(func(i *event.IncidentInForce) { i.SaleAttempted = nil }), "sale attempted")
	expectInvalid(t, "order without attempt", v, in(func(i *event.IncidentInForce) { i.SaleAttempted = new(false) }), "client order")
	expectInvalid(t, "cleared and in force", v, func(p *P) {
		in(func(*event.IncidentInForce) {})(p)
		p.IncidentID = p.IncidentInForce.IncidentID
	}, "clears")
}

// TestAnApprovalStatesTheIncidentsRestrictions: an incident in force carries
// the engine state and scope the previous run ended in, so the fresh run
// resumes in them: "a crash or restart while Degraded or Halted resumes in
// that same state and scope" and leaving either is never automatic (ADR
// 0019, amended 2026-09-24, "The state survives a restart"). Degraded names
// its instruments, strictly ascending; Halted covers the whole account and
// names none; a missing state is refused, never read as Normal.
func TestAnApprovalStatesTheIncidentsRestrictions(t *testing.T) {
	type P = event.OperatorRunApprovedPayload
	v := validOperatorRunApproved()
	for name, in := range map[string]event.IncidentInForce{
		"halted":   {IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(false)},
		"degraded": {IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeInstrumentIDs: []string{"SPY"}, SaleAttempted: new(false)},
	} {
		ok := v
		ok.IncidentInForce = &in
		if err := ok.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	in := func(mutate func(*event.IncidentInForce)) func(*P) {
		return func(p *P) {
			i := event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeInstrumentIDs: []string{"SPY"}, SaleAttempted: new(false)}
			mutate(&i)
			p.IncidentInForce = &i
		}
	}
	expectInvalid(t, "no state", v, in(func(i *event.IncidentInForce) { i.State, i.ScopeInstrumentIDs = "", nil }), "state")
	expectInvalid(t, "normal", v, in(func(i *event.IncidentInForce) { i.State, i.ScopeInstrumentIDs = "normal", nil }), "state")
	expectInvalid(t, "degraded without scope", v, in(func(i *event.IncidentInForce) { i.ScopeInstrumentIDs = nil }), "scope")
	expectInvalid(t, "halted with scope", v, in(func(i *event.IncidentInForce) { i.State = event.EngineStateHalted }), "scope")
	expectInvalid(t, "scope out of order", v, in(func(i *event.IncidentInForce) { i.ScopeInstrumentIDs = []string{"TLT", "SPY"} }), "ascending")
}

// TestDecodeOperatorRunApprovedRequiresEveryKey: the approval file is
// written outside the engine, so a key it leaves out, or one this build does
// not know, is refused rather than read as a zero value: no Boost state, no
// latch, or no statement of the incident in force (null states that there is
// none) is never a default (ADR 0026, point 5; ADR 0019, amended 2026-10-15).
func TestDecodeOperatorRunApprovedRequiresEveryKey(t *testing.T) {
	base := `"account_id":"joint","previous_log":"run-1.log","previous_log_final_hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","incident_id":"","approved_by":"owner","approved_at":"2026-10-17T13:00:00Z","starting_book":null`
	accept := []string{
		`{` + base + `,"boost":{"latched":true},"incident_in_force":null}`,
		`{` + base + `,"boost":{"latched":false},"incident_in_force":{"incident_id":"engine-state:joint:1","state":"halted","scope_instrument_ids":[],"scope_cash_components":[],"sale_attempted":false}}`,
		`{` + base + `,"boost":{"latched":false},"incident_in_force":{"incident_id":"engine-state:joint:1","state":"degraded","scope_instrument_ids":["SPY"],"scope_cash_components":[],"sale_attempted":false}}`,
	}
	for _, raw := range accept {
		if _, err := event.DecodeOperatorRunApproved([]byte(raw)); err != nil {
			t.Errorf("DecodeOperatorRunApproved(%s) = %v", raw, err)
		}
	}
	refuse := map[string]string{
		`{` + base + `,"incident_in_force":null}`:                                                                                          "boost",
		`{` + base + `,"boost":{},"incident_in_force":null}`:                                                                               "latched",
		`{` + base + `,"boost":{"latched":null},"incident_in_force":null}`:                                                                 "latched",
		`{` + base + `,"boost":null,"incident_in_force":null}`:                                                                             "boost",
		`{` + base + `,"boost":{"latched":true}}`:                                                                                          "incident_in_force",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":{"incident_id":"x"}}`:                                                  "incident_in_force.sale_attempted",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":{"incident_id":"x","scope_instrument_ids":[],"sale_attempted":false}}`: "incident_in_force.state",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":{"incident_id":"x","state":"degraded","sale_attempted":false}}`:        "incident_in_force.scope_instrument_ids",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":null,"extra":1}`:                                                       "extra",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":5}`:                                                                    "incident_in_force",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":null} {}`:                                                              "one JSON value",
	}
	for raw, want := range refuse {
		if _, err := event.DecodeOperatorRunApproved([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DecodeOperatorRunApproved(%s) = %v, want a refusal mentioning %q", raw, err, want)
		}
	}
}

// TestAnApprovalCarriesTheIncidentsCashScope: a Degraded incident's scope can
// cover cash (a reconciliation's unexplained cash difference, ADR 0019,
// amended 2026-09-24), and a fresh run resumes in that same scope, so the
// approval states the cash components beside the instruments, at schema
// version 2: Degraded names instruments or cash components or both, strictly
// ascending; Halted names neither.
func TestAnApprovalCarriesTheIncidentsCashScope(t *testing.T) {
	if event.OperatorRunApprovedSchemaVersion < 2 {
		t.Errorf("OperatorRunApprovedSchemaVersion = %d, want at least 2 once an incident in force states its cash scope", event.OperatorRunApprovedSchemaVersion)
	}
	type P = event.OperatorRunApprovedPayload
	v := validOperatorRunApproved()
	cash := []string{event.ReconciliationComponentCash}
	for name, in := range map[string]event.IncidentInForce{
		"cash alone":          {IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeCashComponents: cash, SaleAttempted: new(false)},
		"instrument and cash": {IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeInstrumentIDs: []string{"SPY"}, ScopeCashComponents: cash, SaleAttempted: new(false)},
	} {
		ok := v
		ok.IncidentInForce = &in
		if err := ok.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	in := func(mutate func(*event.IncidentInForce)) func(*P) {
		return func(p *P) {
			i := event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeCashComponents: cash, SaleAttempted: new(false)}
			mutate(&i)
			p.IncidentInForce = &i
		}
	}
	expectInvalid(t, "halted with cash", v, in(func(i *event.IncidentInForce) { i.State = event.EngineStateHalted }), "scope")
	expectInvalid(t, "cash out of order", v, in(func(i *event.IncidentInForce) { i.ScopeCashComponents = []string{"total-cash", "dividends"} }), "ascending")

	base := `"account_id":"joint","previous_log":"run-1.log","previous_log_final_hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","incident_id":"","approved_by":"owner","approved_at":"2026-10-17T13:00:00Z","boost":{"latched":false}`
	cashOnly := `{` + base + `,"incident_in_force":{"incident_id":"engine-state:joint:1","state":"degraded","scope_instrument_ids":[],"scope_cash_components":["total-cash"],"sale_attempted":false}}`
	// The file states every key of the current version, the starting book
	// (version 3) included.
	current := func(raw string) []byte {
		return []byte(strings.Replace(raw, `"boost"`, `"starting_book":null,"boost"`, 1))
	}
	p, err := event.DecodeOperatorRunApproved(current(cashOnly))
	if err != nil || p.IncidentInForce == nil || len(p.IncidentInForce.ScopeCashComponents) != 1 {
		t.Errorf("DecodeOperatorRunApproved(cash alone) = %+v, %v", p.IncidentInForce, err)
	}
	unstated := `{` + base + `,"incident_in_force":{"incident_id":"engine-state:joint:1","state":"degraded","scope_instrument_ids":["SPY"],"sale_attempted":false}}`
	if _, err := event.DecodeOperatorRunApproved(current(unstated)); err == nil || !strings.Contains(err.Error(), "incident_in_force.scope_cash_components") {
		t.Errorf("an incident in force that does not state its cash scope: %v", err)
	}

	// A version 1 approval recorded by an earlier build could not state a
	// cash scope, so it is read with none; it may not name one.
	if r, err := event.DecodeRecordedOperatorRunApproved([]byte(unstated), 1); err != nil || len(r.IncidentInForce.ScopeCashComponents) != 0 {
		t.Errorf("a version 1 approval: %+v, %v", r.IncidentInForce, err)
	}
	if _, err := event.DecodeRecordedOperatorRunApproved([]byte(cashOnly), 1); err == nil {
		t.Error("a version 1 approval naming a cash scope was read")
	}
	if _, err := event.DecodeRecordedOperatorRunApproved([]byte(cashOnly), 2); err != nil {
		t.Errorf("a version 2 approval: %v", err)
	}
	if _, err := event.DecodeRecordedOperatorRunApproved([]byte(cashOnly), 5); err == nil || !strings.Contains(err.Error(), "schema version 5") {
		t.Errorf("an approval at an unknown schema version: %v", err)
	}
}

// TestOperatorRunApprovedValidateSchema: an approval is checked against the
// schema version it was recorded at (ADR 0015): version 1 could not state an
// incident's cash scope, so a payload naming one is refused under it; version
// 2 holds it; any other version is refused.
func TestOperatorRunApprovedValidateSchema(t *testing.T) {
	plain := validOperatorRunApprovedInIncident()
	withCash := validOperatorRunApproved()
	withCash.IncidentInForce = &event.IncidentInForce{
		IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded,
		ScopeCashComponents: []string{event.ReconciliationComponentCash}, SaleAttempted: new(false),
	}
	for _, v := range []uint32{1, 2} {
		if err := plain.ValidateSchema(v); err != nil {
			t.Errorf("version %d, no cash scope: %v", v, err)
		}
	}
	if err := withCash.ValidateSchema(2); err != nil {
		t.Errorf("version 2 with a cash scope: %v", err)
	}
	if err := withCash.ValidateSchema(1); err == nil || !strings.Contains(err.Error(), "cash") {
		t.Errorf("version 1 naming a cash scope: %v", err)
	}
	for _, v := range []uint32{0, 5} {
		if err := plain.ValidateSchema(v); err == nil || !strings.Contains(err.Error(), "schema version") {
			t.Errorf("version %d: %v", v, err)
		}
	}
	bad := plain
	bad.AccountID = ""
	if err := bad.ValidateSchema(2); err == nil {
		t.Error("an invalid payload passed ValidateSchema")
	}
}

func firstRunApproval() event.OperatorRunApprovedPayload {
	p := validOperatorRunApproved()
	p.PreviousLog, p.PreviousLogFinalHash, p.Boost.Latched = "", "", new(false)
	p.StartingBook = &event.StartingBook{
		Positions: []event.AccountPosition{{InstrumentID: "IEF", Quantity: 3}, {InstrumentID: "SPY", Quantity: 147}},
		TotalCash: -4211.37, Currency: "USD",
	}
	return p
}

// TestAFirstRunsApprovalStatesTheStartingBook: an account's first run has no
// earlier run whose last account snapshot its first reading can be compared
// with, so its approval states the holdings and total cash the owner confirms
// it starts from (schema version 3), and the adapter compares its first
// reading with that, never adopting IBKR's book unchecked (ADR 0019, amended
// 2026-10-18; ADR 0027). A fresh run's approval may state one too, the book
// the owner confirms after a change no fill explains (a transfer, a deposit).
func TestAFirstRunsApprovalStatesTheStartingBook(t *testing.T) {
	if event.OperatorRunApprovedSchemaVersion < 3 {
		t.Fatalf("OperatorRunApprovedSchemaVersion = %d, want 3 or later once an approval states a starting book", event.OperatorRunApprovedSchemaVersion)
	}
	first := firstRunApproval()
	if err := first.Validate(); err != nil {
		t.Fatalf("a first run's approval: %v", err)
	}
	if !first.FirstRun() || validOperatorRunApproved().FirstRun() {
		t.Error("FirstRun names the approval with no previous log")
	}
	stated := validOperatorRunApproved()
	stated.StartingBook = first.StartingBook
	if err := stated.Validate(); err != nil {
		t.Errorf("a fresh run's approval stating the book the owner confirms: %v", err)
	}
	type P = event.OperatorRunApprovedPayload
	book := func(mutate func(*event.StartingBook)) func(*P) {
		return func(p *P) {
			b := *p.StartingBook
			b.Positions = slices.Clone(b.Positions)
			mutate(&b)
			p.StartingBook = &b
		}
	}
	expectInvalid(t, "first run, no book", first, func(p *P) { p.StartingBook = nil }, "previous log is required")
	expectInvalid(t, "first run, a hash", first, func(p *P) { p.PreviousLogFinalHash = validOperatorRunApproved().PreviousLogFinalHash }, "first run")
	expectInvalid(t, "first run, latched", first, func(p *P) { p.Boost.Latched = new(true) }, "latch")
	expectInvalid(t, "first run, an incident cleared", first, func(p *P) { p.IncidentID = "engine-state:joint:1" }, "incident")
	expectInvalid(t, "first run, an incident in force", first, func(p *P) { p.IncidentInForce = validOperatorRunApprovedInIncident().IncidentInForce }, "incident")
	expectInvalid(t, "positions out of order", first, book(func(b *event.StartingBook) { slices.Reverse(b.Positions) }), "ascending")
	expectInvalid(t, "a zero holding", first, book(func(b *event.StartingBook) { b.Positions[0].Quantity = 0 }), "quantity")
	expectInvalid(t, "an unnamed holding", first, book(func(b *event.StartingBook) { b.Positions[0].InstrumentID = "" }), "instrument")
	expectInvalid(t, "cash not finite", first, book(func(b *event.StartingBook) { b.TotalCash = math.NaN() }), "total cash")
	expectInvalid(t, "no currency", first, book(func(b *event.StartingBook) { b.Currency = "" }), "currency")
	expectInvalid(t, "positions null", first, book(func(b *event.StartingBook) { b.Positions = nil }), "positions")

	if err := first.ValidateSchema(2); err == nil || !strings.Contains(err.Error(), "starting book") {
		t.Errorf("version 2 stating a starting book: %v", err)
	}
	if err := first.ValidateSchema(3); err != nil {
		t.Errorf("version 3: %v", err)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	back, err := event.DecodeOperatorRunApproved(raw)
	if err != nil || back.StartingBook == nil || back.StartingBook.TotalCash != -4211.37 || len(back.StartingBook.Positions) != 2 {
		t.Fatalf("round trip = %+v, %v from %s", back.StartingBook, err, raw)
	}
	// A starting book that does not state its cash is refused, never read
	// as an approved zero balance: an unavailable balance is not zero (ADR
	// 0019), and the book is the owner's to state (ADR 0027, amended
	// 2026-10-18).
	for name, cash := range map[string]string{"omitted": "", "null": `"total_cash":null,`} {
		book := strings.Replace(string(raw), `"total_cash":-4211.37,`, cash, 1)
		if book == string(raw) {
			t.Fatalf("setup: %s holds no total_cash to replace", raw)
		}
		if _, err := event.DecodeOperatorRunApproved([]byte(book)); err == nil || !strings.Contains(err.Error(), "total_cash") {
			t.Errorf("a starting book with its cash %s: %v", name, err)
		}
	}
	var asObject map[string]json.RawMessage
	_ = json.Unmarshal(raw, &asObject)
	asObject["starting_book"] = json.RawMessage(`[]`)
	notABook, _ := json.Marshal(asObject)
	if _, err := event.DecodeOperatorRunApproved(notABook); err == nil || !strings.Contains(err.Error(), "object or null") {
		t.Errorf("a starting book that is no object: %v", err)
	}
	chained, _ := json.Marshal(validOperatorRunApproved())
	if !strings.Contains(string(chained), `"starting_book":null`) {
		t.Errorf("an approval stating no book does not say so: %s", chained)
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(chained, &keys)
	delete(keys, "starting_book")
	unstated, _ := json.Marshal(keys)
	if _, err := event.DecodeOperatorRunApproved(unstated); err == nil || !strings.Contains(err.Error(), "starting_book") {
		t.Errorf("an approval file that does not state starting_book: %v", err)
	}
	if _, err := event.DecodeRecordedOperatorRunApproved(unstated, 2); err != nil {
		t.Errorf("a version 2 approval, which has no starting book: %v", err)
	}
	if _, err := event.DecodeRecordedOperatorRunApproved(chained, 2); err == nil || !strings.Contains(err.Error(), "starting_book") {
		t.Errorf("a version 2 approval stating starting_book: %v", err)
	}
}

// TestAnApprovalCarriesTheIncidentsRetiredSaleAttempts: from schema version
// 4 an incident in force states how many of its sales were retired as never
// sent, never fewer than none, so the fresh run's next attempt has a client
// order ID of its own; an earlier version cannot state it, and a decoded
// approval may leave it out, as none (ADR 0026, point 2, "Order identity";
// ADR 0015).
func TestAnApprovalCarriesTheIncidentsRetiredSaleAttempts(t *testing.T) {
	if event.OperatorRunApprovedSchemaVersion != 4 {
		t.Fatalf("OperatorRunApprovedSchemaVersion = %d, want 4 once an approval states the retired sale attempts", event.OperatorRunApprovedSchemaVersion)
	}
	p := validOperatorRunApprovedInIncident()
	p.IncidentInForce.SaleRetiredAttempts = 2
	if err := p.ValidateSchema(4); err != nil {
		t.Errorf("version 4 stating two retired attempts: %v", err)
	}
	for _, v := range []uint32{2, 3} {
		if err := p.ValidateSchema(v); err == nil || !strings.Contains(err.Error(), "retired") {
			t.Errorf("version %d stating retired attempts: %v", v, err)
		}
	}
	p.IncidentInForce.SaleRetiredAttempts = -1
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Errorf("a negative count of retired attempts: %v", err)
	}
	p.IncidentInForce.SaleRetiredAttempts = 0
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sale_retired_attempts") {
		t.Errorf("none retired is left out: %s", raw)
	}
	if _, err := event.DecodeRecordedOperatorRunApproved(raw, 4); err != nil {
		t.Errorf("a version 4 approval leaving the retired attempts out: %v", err)
	}
}

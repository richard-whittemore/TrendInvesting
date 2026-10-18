package targetweight_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// dryRun runs the momentum IRA in dry-run, as a dry-run adapter does: no
// order reaches a venue, so the venue never reports on one (ADR 0026, point
// 3, "Mode"). The IRA holds FFF, which no rebalance targets, so January's
// rebalance sells it.
func dryRun(mode string) momentumOptions {
	return momentumOptions{
		mutate: func(c *event.TargetWeightConfigurationPayload) {
			c.Mode = mode
			if mode == event.TradingModeLive {
				c.AccountID = "U1234567" // a live account: not a paper "DU" one
			}
		},
		silentVenue: true,
		held:        map[string]int64{"FFF": 100},
	}
}

// syntheticReports decodes each lifecycle decision as the reader sees it,
// by client order ID.
func syntheticReports(t *testing.T, es []event.Envelope) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, e := range ofType(es, event.OrderLifecycleEventType) {
		var p map[string]any
		decode(t, e, &p)
		out[p["client_order_id"].(string)] = p
	}
	return out
}

func TestADryRunRebalanceThatSellsPlansItsBuysInTheSameRun(t *testing.T) {
	r := simulateMomentum(t, dryRun(event.TradingModeDryRun))
	const januaryClose = "session:2025-01-31"
	var sells, buys []event.OrderPlannedPayload
	for _, entry := range r.entries {
		e := entry.Envelope
		if e.Type != event.OrderPlannedEventType || e.CausationID != januaryClose {
			continue
		}
		var p event.OrderPlannedPayload
		decode(t, e, &p)
		if p.Side == event.OrderSideSell {
			sells = append(sells, p)
		} else {
			buys = append(buys, p)
		}
	}
	if len(sells) == 0 || len(buys) == 0 {
		t.Fatalf("January's rebalance planned %d sells and %d buys at its close; a dry-run must reach the buy phase", len(sells), len(buys))
	}
	reports := syntheticReports(t, r.decisions)
	for _, o := range released(t, r.decisions) {
		if o.Send {
			t.Errorf("dry-run released %s to send", o.ClientOrderID)
		}
		p, ok := reports[o.ClientOrderID]
		if !ok {
			t.Errorf("released %s has no synthetic terminal report", o.ClientOrderID)
			continue
		}
		if p["synthetic"] != true || p["status"] != event.OrderLifecycleStatusCanceled || p["order_id"] != "" {
			t.Errorf("report on %s is not marked synthetic: %v", o.ClientOrderID, p)
		}
	}
	if got, want := withheldReasons(t, r), []string{"decided:", "decided:"}; !equalStrings(got, want) {
		t.Fatalf("decisions %v, want %v: February must not wait on January's unsent orders", got, want)
	}
}

func TestOnlyADryRunRecordsSyntheticReports(t *testing.T) {
	for _, mode := range []string{event.TradingModePaper, event.TradingModeLive} {
		t.Run(mode, func(t *testing.T) {
			r := simulateMomentum(t, dryRun(mode))
			if n := len(ofType(r.decisions, event.OrderLifecycleEventType)); n != 0 {
				t.Fatalf("%s recorded %d lifecycle reports of its own", mode, n)
			}
			if got, want := withheldReasons(t, r), []string{"decided:", "withheld:rebalance-in-progress"}; !equalStrings(got, want) {
				t.Fatalf("decisions %v, want %v: a sent order waits for the venue", got, want)
			}
		})
	}
}

// A synthetic report is no execution: under the total-cash rule the dry-run
// sale of FFF adds no proceeds, so the buys fit the snapshot's cash alone.
func TestADryRunCountsNoProceedsOrCommissionFromASyntheticReport(t *testing.T) {
	opts := dryRun(event.TradingModeDryRun)
	opts.mutate = func(c *event.TargetWeightConfigurationPayload) {
		c.Mode = event.TradingModeDryRun
		c.Execution.CashRule = event.CashRuleTotal
	}
	r := simulateMomentum(t, opts)
	if n := len(ofType(r.decisions, event.FillEventType)); n != 0 {
		t.Fatalf("%d fills recorded in a dry-run", n)
	}
	var cost float64
	for _, o := range released(t, r.decisions) {
		if o.Side == event.OrderSideBuy && o.Tag == "targets-decided:DUIRA:2025-01-31T21:00:00Z" {
			cost += float64(float64(o.Quantity)*o.LimitPrice) + 1 // the configured fee allowance
		}
	}
	if cost == 0 || cost > 10000 {
		t.Fatalf("January's buys may cost %v; the IRA has 10000 in cash and the unsent sale realised nothing", cost)
	}
}

// The synthetic reports are decisions the handler derives, so a dry-run
// journal replays to the same records.
func TestADryRunJournalReplays(t *testing.T) {
	opts := dryRun(event.TradingModeDryRun)
	r := simulateMomentum(t, opts)
	cfg := momentumConfiguration()
	opts.mutate(&cfg)
	inputs, decisions, err := journal.Split(records(t, r))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := replay.New(handler)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if d := replay.Equivalent(decisions, replayed); d != nil {
		t.Fatalf("replay diverged: %+v", d)
	}
	if len(ofType(decisions, event.OrderLifecycleEventType)) == 0 {
		t.Fatal("the journal holds no synthetic report to replay")
	}
}

// records is r's journal as written and read back.
func records(t *testing.T, r momentumResult) []journal.Record {
	t.Helper()
	header, err := r.recorder.Header(r.hash, strategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := journal.Write(&buf, header, r.entries); err != nil {
		t.Fatal(err)
	}
	_, out, err := journal.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

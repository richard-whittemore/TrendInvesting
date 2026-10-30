package journalquery_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
)

func scaledSnapshot(account string, quantity, scale int64, when time.Time) rec {
	r := cashSnap(when)
	p := r.payload.(event.AccountCashSnapshotPayload)
	p.AccountID, p.QuantityScale = account, scale
	if quantity != 0 {
		p.Positions = []event.AccountPosition{{InstrumentID: "AAA", Quantity: quantity}}
	}
	r.payload, r.schema = p, p.SchemaVersion()
	return r
}

func scaledFill(account, id, side string, quantity, scale int64, when time.Time) rec {
	r := limitFill(id, side, quantity, 0.1, when)
	p := r.payload.(event.TargetWeightFillPayload)
	p.AccountID, p.QuantityScale = account, scale
	r.payload, r.schema = p, p.SchemaVersion()
	return r
}

var aggregationViews = map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
	"positions": journalquery.Positions, "totals": journalquery.Totals, "reconcile": journalquery.Reconcile,
}

func TestFractionalAggregationsUseExactTicks(t *testing.T) {
	d := at(time.March, 2)
	// ADR 0026 D7-F: the one-tick difference above 2^53 cannot survive a float conversion.
	const baseline int64 = 9007199254740992
	js := []journalquery.Journal{load(t, "fractional", build(t,
		scaledSnapshot("synthetic", baseline, 10000, d),
		scaledFill("synthetic", "buy", "buy", 3, 10000, d),
		scaledFill("synthetic", "sell", "sell", 2, 10000, d),
		scaledSnapshot("synthetic", baseline+1, 10000, d),
		scaledFill("synthetic", "last", "buy", 1, 10000, d),
		scaledSnapshot("synthetic", baseline+1, 10000, d),
	))}
	for name, fn := range aggregationViews {
		t.Run(name, func(t *testing.T) {
			tab, err := fn(js, journalquery.Filter{})
			if name == "reconcile" {
				var disagreement *journalquery.DisagreementError
				if !errors.As(err, &disagreement) || disagreement.Count != 1 {
					t.Fatalf("reconcile: %v", err)
				}
				if len(tab.Rows) != 2 || cell(tab, 0, "fills") != "900719925474.0993" || cell(tab, 0, "status") != "ok" || cell(tab, 1, "fills") != "900719925474.0994" || cell(tab, 1, "snapshot") != "900719925474.0993" || cell(tab, 1, "status") != "DISAGREES" {
					t.Fatalf("tick comparison: %+v", tab)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if name == "positions" {
				if len(tab.Rows) != 3 || cell(tab, 0, "position") != "900719925474.0995" || cell(tab, 1, "position") != "900719925474.0993" || cell(tab, 1, "change") != "-0.0002" {
					t.Fatalf("positions: %+v", tab)
				}
			} else if cell(tab, 0, "bought") != "0.0004" || cell(tab, 0, "sold") != "0.0002" || cell(tab, 1, "bought") != "0.0004" {
				t.Fatalf("totals: %+v", tab)
			}
		})
	}
}

func TestDifferentAccountScalesAggregateSeparately(t *testing.T) {
	d := at(time.March, 2)
	records := []rec{
		scaledSnapshot("fractional", 72500, 10000, d), scaledSnapshot("whole", 7, 0, d),
		scaledFill("fractional", "fractional-sell", "sell", 12500, 10000, d), scaledFill("whole", "whole-buy", "buy", 2, 0, d),
		scaledSnapshot("fractional", 60000, 10000, d), scaledSnapshot("whole", 9, 0, d),
	}
	for _, split := range []bool{false, true} {
		var js []journalquery.Journal
		if split {
			js = []journalquery.Journal{load(t, "f", build(t, records[0], records[2], records[4])), load(t, "w", build(t, records[1], records[3], records[5]))}
		} else {
			js = []journalquery.Journal{load(t, "both", build(t, records...))}
		}
		for name, fn := range aggregationViews {
			t.Run(name+map[bool]string{false: "/one-journal", true: "/two-journals"}[split], func(t *testing.T) {
				tab, err := fn(js, journalquery.Filter{})
				if err != nil {
					t.Fatal(err)
				}
				if name == "totals" {
					if len(tab.Rows) != 4 {
						t.Fatalf("per-account groups and totals: %+v", tab)
					}
					for i, row := range tab.Rows {
						account := cell(tab, i, "account")
						if account == "fractional" {
							if cell(tab, i, "sold") != "1.25" || cell(tab, i, "bought") != "0" {
								t.Fatal(row)
							}
						} else if account == "whole" {
							if cell(tab, i, "bought") != "2" || cell(tab, i, "sold") != "0" {
								t.Fatal(row)
							}
						} else {
							t.Fatalf("missing account: %+v", tab)
						}
					}
				} else {
					column := "position"
					if name == "reconcile" {
						column = "fills"
					}
					if len(tab.Rows) != 2 || cell(tab, 0, column) != "6" || cell(tab, 1, column) != "9" {
						t.Fatalf("separate books: %+v", tab)
					}
				}
			})
		}
	}
}

func TestMixedScalesWithinAnAccountAreRefusedByName(t *testing.T) {
	d := at(time.March, 2)
	whole, fractional := scaledSnapshot("synthetic", 1, 0, d), scaledFill("synthetic", "fractional", "buy", 1, 10000, d)
	life := limitLifecycle(d)
	p := life.payload.(event.LimitOrderLifecyclePayload)
	p.AccountID, p.QuantityScale = "synthetic", 10000
	life.payload, life.schema = p, p.SchemaVersion()
	for _, conflict := range []rec{fractional, life} {
		for _, split := range []bool{false, true} {
			js := []journalquery.Journal{load(t, "mixed", build(t, whole, conflict))}
			if split {
				js = []journalquery.Journal{load(t, "whole", build(t, whole)), load(t, "fractional", build(t, conflict))}
			}
			for name, fn := range aggregationViews {
				if _, err := fn(js, journalquery.Filter{Account: "elsewhere"}); err == nil || !strings.Contains(err.Error(), "mixed quantity scales") || !strings.Contains(err.Error(), "synthetic") || !strings.Contains(err.Error(), "10000") {
					t.Errorf("%s: mixed account scales unnamed: %v", name, err)
				}
			}
		}
	}
}

func TestFractionalTotalsRefuseOverflow(t *testing.T) {
	d := at(time.March, 2)
	for _, side := range []string{"buy", "sell"} {
		js := []journalquery.Journal{load(t, "overflow", build(t, scaledFill("synthetic", "large", side, math.MaxInt64, 10000, d), scaledFill("synthetic", "tick", side, 1, 10000, d)))}
		if _, err := journalquery.Totals(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "overflow") {
			t.Fatalf("%s totals: %v", side, err)
		}
	}
}

func TestFractionalTotalsRemainExactAcrossGroups(t *testing.T) {
	d := at(time.March, 2)
	const large int64 = 9007199254740992
	js := []journalquery.Journal{load(t, "exact", build(t,
		scaledFill("synthetic", "large", "buy", large, 10000, d),
		scaledFill("synthetic", "tick", "buy", 1, 10000, d.AddDate(1, 0, 0)),
	))}
	tab, err := journalquery.Totals(js, journalquery.Filter{})
	if err != nil || len(tab.Rows) != 3 || cell(tab, 2, "bought") != "900719925474.0993" {
		t.Fatalf("exact per-account total: %+v: %v", tab, err)
	}
	js = []journalquery.Journal{load(t, "overflow", build(t,
		scaledFill("synthetic", "large", "buy", math.MaxInt64, 10000, d),
		scaledFill("synthetic", "tick", "buy", 1, 10000, d.AddDate(1, 0, 0)),
	))}
	if _, err := journalquery.Totals(js, journalquery.Filter{}); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("total overflow across year groups: %v", err)
	}
}

func TestFractionalSkippedOrderDeclaresTheAccountScale(t *testing.T) {
	d := at(time.March, 2)
	skipped := event.OrderSkippedPayload{AccountID: "synthetic", Tag: "t", ClientOrderID: "skip", InstrumentID: "AAA", Side: event.OrderSideBuy,
		Phase: event.OrderSideBuy, Reason: event.OrderSkippedBelowIncrement, TargetWeight: 0.5, NetLiquidation: 1, ReferencePrice: 3,
		QuantityScale: event.FractionalQuantityScale, RequestedValue: "0.0010", FractionalIncrement: "0.0001"}
	r := dec(event.OrderSkippedEventType, skipped.SchemaVersion(), d, skipped)
	for _, scale := range []int64{0, 10000} {
		js := []journalquery.Journal{load(t, "skipped", build(t, scaledSnapshot("synthetic", 1, scale, d), r, scaledSnapshot("synthetic", 1, scale, d)))}
		for name, fn := range aggregationViews {
			tab, err := fn(js, journalquery.Filter{})
			if scale == 0 {
				if err == nil || !strings.Contains(err.Error(), "mixed quantity scales") {
					t.Errorf("%s accepted conflicting skip: %v", name, err)
				}
			} else if err != nil {
				t.Errorf("%s refused fractional skip: %+v: %v", name, tab, err)
			}
		}
	}
}

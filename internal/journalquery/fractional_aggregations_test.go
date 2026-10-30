package journalquery_test

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
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

func TestAggregationPreflightRefusesInvalidNumericScales(t *testing.T) {
	d := at(time.March, 2)
	for _, typ := range []string{"other.thing", event.AccountSnapshotEventType} {
		for _, scale := range []int64{-10000, -1, 1, 1000, 10000} {
			if typ == "other.thing" && scale == event.FractionalQuantityScale {
				continue
			}
			t.Run(fmt.Sprintf("%s/%d", typ, scale), func(t *testing.T) {
				bad := rec{journal.KindInput, typ, event.AccountSnapshotSchemaVersion, d, map[string]any{"quantity_scale": scale}}
				js := []journalquery.Journal{load(t, "numeric-scale", build(t,
					scaledSnapshot("synthetic", 0, 0, d), bad, scaledSnapshot("synthetic", 0, 0, d),
				))}
				for name, fn := range aggregationViews {
					if _, err := fn(js, journalquery.Filter{Account: "elsewhere"}); err == nil || !strings.Contains(err.Error(), "record 2") || !strings.Contains(err.Error(), "quantity") {
						t.Errorf("%s accepted invalid numeric scale %d or omitted context: %v", name, scale, err)
					}
				}
			})
		}
	}
}

func TestWholeShareTotalsSelectionIgnoresExcludedFractionalRecords(t *testing.T) {
	d := at(time.March, 2)
	whole := []rec{scaledFill("whole-a", "whole-a", "buy", 2, 0, d), scaledFill("whole-b", "whole-b", "buy", 3, 0, d)}
	for _, selection := range []struct {
		name, account, instrument, from, to string
		fractional                          rec
	}{
		{"account", "whole-a", "", "", "", scaledSnapshot("fractional", 72500, 10000, d)},
		{"date", "", "", "2026-03-01", "2026-03-31", scaledFill("fractional", "fractional", "buy", 1, 10000, at(time.April, 2))},
		{"instrument", "", "AAA", "", "", func() rec {
			r := scaledFill("fractional", "fractional", "buy", 1, 10000, d)
			p := r.payload.(event.TargetWeightFillPayload)
			p.InstrumentID = "BBB"
			r.payload = p
			return r
		}()},
	} {
		t.Run(selection.name, func(t *testing.T) {
			f, err := journalquery.NewFilter(selection.account, selection.instrument, selection.from, selection.to)
			if err != nil {
				t.Fatal(err)
			}
			want, err := journalquery.Totals([]journalquery.Journal{load(t, "whole", build(t, whole...))}, f)
			if err != nil {
				t.Fatal(err)
			}
			js := []journalquery.Journal{load(t, "mixed", build(t, append(append([]rec{}, whole...), selection.fractional)...))}
			got, err := journalquery.Totals(js, f)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("excluded fractional record changed whole-share totals: got %+v (%v), want %+v", got, err, want)
			}
		})
	}
}

func TestFractionalTotalsIncludeSelectedAccountsWithoutFills(t *testing.T) {
	d := at(time.March, 2)
	skipped := event.OrderSkippedPayload{AccountID: "synthetic", Tag: "t", ClientOrderID: "skip", InstrumentID: "AAA", Side: event.OrderSideBuy,
		Phase: event.OrderSideBuy, Reason: event.OrderSkippedBelowIncrement, TargetWeight: 0.5, NetLiquidation: 1, ReferencePrice: 3,
		QuantityScale: event.FractionalQuantityScale, RequestedValue: "0.0010", FractionalIncrement: "0.0001"}
	for _, selection := range []struct {
		name, instrument, from, to string
		records                    []rec
	}{
		{"snapshots-only", "", "", "", []rec{scaledSnapshot("synthetic", 72500, 10000, d)}},
		{"skipped-only", "", "", "", []rec{{journal.KindInput, "other.thing", 1, d, 0}, dec(event.OrderSkippedEventType, skipped.SchemaVersion(), d, skipped)}},
		{"no-fill-in-date-range", "", "2026-03-01", "2026-03-31", []rec{
			scaledSnapshot("synthetic", 72500, 10000, d), scaledFill("synthetic", "later", "buy", 1, 10000, at(time.April, 2)),
		}},
		{"no-fill-in-instrument", "BBB", "", "", []rec{
			scaledSnapshot("synthetic", 72500, 10000, d), scaledFill("synthetic", "other", "buy", 1, 10000, d),
		}},
	} {
		t.Run(selection.name, func(t *testing.T) {
			f, err := journalquery.NewFilter("synthetic", selection.instrument, selection.from, selection.to)
			if err != nil {
				t.Fatal(err)
			}
			tab, err := journalquery.Totals([]journalquery.Journal{load(t, "zero", build(t, selection.records...))}, f)
			if err != nil || len(tab.Rows) != 1 || strings.Join(tab.Rows[0], " ") != "synthetic TOTAL all 0 0 0 0.00" {
				t.Fatalf("missing zero fractional TOTAL: %+v: %v", tab, err)
			}
		})
	}
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
						switch cell(tab, i, "account") {
						case "fractional":
							if cell(tab, i, "sold") != "1.25" || cell(tab, i, "bought") != "0" {
								t.Fatal(row)
							}
						case "whole":
							if cell(tab, i, "bought") != "2" || cell(tab, i, "sold") != "0" {
								t.Fatal(row)
							}
						default:
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

func TestFractionalThenWholeScalesWithinAnAccountAreRefusedByName(t *testing.T) {
	d := at(time.March, 2)
	whole, fractional := scaledSnapshot("synthetic", 1, 0, d), scaledFill("synthetic", "fractional", "buy", 1, 10000, d)
	life := limitLifecycle(d)
	p := life.payload.(event.LimitOrderLifecyclePayload)
	p.AccountID, p.QuantityScale = "synthetic", 10000
	life.payload, life.schema = p, p.SchemaVersion()
	for _, conflict := range []rec{fractional, life} {
		for _, split := range []bool{false, true} {
			js := []journalquery.Journal{load(t, "mixed", build(t, conflict, whole))}
			if split {
				// Two runs' journals: each is forward-only on its own, so the whole-share run is
				// upcast to the account's declared scale rather than refused (ADR 0026, D7-F).
				js = []journalquery.Journal{load(t, "fractional", build(t, conflict)), load(t, "whole", build(t, whole))}
				for name, fn := range aggregationViews {
					if _, err := fn(js, journalquery.Filter{Account: "elsewhere"}); err != nil && strings.Contains(err.Error(), "mixed quantity scales") {
						t.Errorf("%s: two runs refused as one history: %v", name, err)
					}
				}
				continue
			}
			for name, fn := range aggregationViews {
				if _, err := fn(js, journalquery.Filter{Account: "elsewhere"}); err == nil || !strings.Contains(err.Error(), "mixed quantity scales") || !strings.Contains(err.Error(), "synthetic") || !strings.Contains(err.Error(), "10000") {
					t.Errorf("%s: mixed account scales unnamed: %v", name, err)
				}
			}
		}
	}
}

func TestDifferentNonzeroScalesWithinAnAccountAreRefusedByName(t *testing.T) {
	d := at(time.March, 2)
	js := []journalquery.Journal{load(t, "mixed", build(t,
		scaledSnapshot("synthetic", 10000, 10000, d), scaledSnapshot("synthetic", 1000, 1000, d),
	))}
	for name, fn := range aggregationViews {
		if _, err := fn(js, journalquery.Filter{Account: "elsewhere"}); err == nil || !strings.Contains(err.Error(), "synthetic") || !strings.Contains(err.Error(), "1000") {
			t.Errorf("%s: nonzero scale conflict omitted account: %v", name, err)
		}
	}
}

func TestLegacyThenFractionalCutoverUpcastsAllAggregationViews(t *testing.T) {
	legacy, fractional := at(time.March, 2), at(time.April, 2)
	records := []rec{
		scaledSnapshot("synthetic", 7, 0, legacy),
		scaledFill("synthetic", "legacy", "buy", 2, 0, legacy),
		scaledSnapshot("synthetic", 9, 0, legacy),
		scaledSnapshot("synthetic", 90000, 10000, fractional),
		scaledFill("synthetic", "fractional", "buy", 5000, 10000, fractional),
		scaledSnapshot("synthetic", 95000, 10000, fractional),
	}
	for _, split := range []bool{false, true} {
		js := []journalquery.Journal{load(t, "cutover", build(t, records...))}
		if split {
			js = []journalquery.Journal{load(t, "legacy", build(t, records[:3]...)), load(t, "fractional", build(t, records[3:]...))}
		}
		for name, fn := range aggregationViews {
			t.Run(fmt.Sprintf("%s/split=%t", name, split), func(t *testing.T) {
				for _, dates := range [][2]string{{"", ""}, {"2026-04-01", ""}, {"", "2026-03-31"}} {
					if split && dates != [2]string{} {
						continue // Reconcile requires a selected comparison in every journal (ADR 0017).
					}
					f, err := journalquery.NewFilter("", "", dates[0], dates[1])
					if err != nil {
						t.Fatal(err)
					}
					tab, err := fn(js, f)
					if err != nil {
						t.Fatalf("valid cutover refused: %v", err)
					}
					switch name {
					case "positions":
						want := []string{"9", "9.5"}
						if dates[0] != "" {
							want = want[1:]
						} else if dates[1] != "" {
							want = want[:1]
						}
						if len(tab.Rows) != len(want) {
							t.Fatalf("positions: %+v", tab)
						}
						for i, q := range want {
							if cell(tab, i, "position") != q {
								t.Fatalf("position was not upcast exactly: %+v", tab)
							}
						}
					case "reconcile":
						for i := range tab.Rows {
							if cell(tab, i, "status") != "ok" || cell(tab, i, "fills") != cell(tab, i, "snapshot") {
								t.Fatalf("cutover comparison: %+v", tab)
							}
						}
					case "totals":
						want := "synthetic TOTAL all 2 2.5 0 0.20"
						if dates[0] != "" {
							want = "synthetic TOTAL all 1 0.5 0 0.10"
						} else if dates[1] != "" {
							want = "TOTAL all 1 2 0 0.10"
						}
						if got := strings.Join(tab.Rows[len(tab.Rows)-1], " "); got != want {
							t.Fatalf("cutover total = %q, want %q", got, want)
						}
					}
				}
			})
		}
	}
}

func TestLegacyUpcastRefusesOverflowAndNegativeHoldings(t *testing.T) {
	d := at(time.March, 2)
	for _, legacy := range []rec{
		scaledSnapshot("synthetic", -1, 0, d),
		scaledSnapshot("synthetic", math.MaxInt64/event.FractionalQuantityScale+1, 0, d),
		scaledFill("synthetic", "overflow", "buy", math.MaxInt64/event.FractionalQuantityScale+1, 0, d),
	} {
		js := []journalquery.Journal{load(t, "upcast", build(t, legacy, scaledSnapshot("synthetic", 10000, 10000, d)))}
		for name, fn := range aggregationViews {
			if _, err := fn(js, journalquery.Filter{Account: "elsewhere"}); err == nil || !strings.Contains(err.Error(), "cannot upcast") || !strings.Contains(err.Error(), "synthetic") || !strings.Contains(err.Error(), "record 1") {
				t.Errorf("%s: missing named upcast refusal: %v", name, err)
			}
		}
	}
}

func TestLegacyCutoverPreservesTheLargestUpcastableFillAndItsDeduplication(t *testing.T) {
	d := at(time.March, 2)
	const quantity = math.MaxInt64 / event.FractionalQuantityScale
	life := limitLifecycle(d)
	p := life.payload.(event.LimitOrderLifecyclePayload)
	p.AccountID = "synthetic"
	life.payload = p
	legacy := scaledFill("synthetic", "duplicate", "buy", quantity, 0, d)
	js := []journalquery.Journal{load(t, "boundary", build(t,
		life, legacy, legacy, scaledSnapshot("synthetic", quantity, 0, d),
		scaledSnapshot("synthetic", quantity*event.FractionalQuantityScale, 10000, d),
	))}
	for name, fn := range aggregationViews {
		tab, err := fn(js, journalquery.Filter{})
		if err != nil {
			t.Fatalf("%s: largest representable legacy quantity refused: %v", name, err)
		}
		switch name {
		case "positions":
			if len(tab.Rows) != 1 || cell(tab, 0, "change") != "922337203685477" || cell(tab, 0, "position") != "922337203685477" {
				t.Fatalf("upcast fill before baseline or duplicate changed position: %+v", tab)
			}
		case "totals":
			if len(tab.Rows) != 2 || cell(tab, 1, "fills") != "1" || cell(tab, 1, "bought") != "922337203685477" {
				t.Fatalf("upcast duplicate changed total: %+v", tab)
			}
		case "reconcile":
			if len(tab.Rows) != 2 || cell(tab, 0, "fills") != "922337203685477" || cell(tab, 1, "fills") != "922337203685477" {
				t.Fatalf("upcast comparisons: %+v", tab)
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
			switch {
			case scale == 0:
				if err == nil || !strings.Contains(err.Error(), "mixed quantity scales") {
					t.Errorf("%s accepted conflicting skip: %v", name, err)
				}
			case err != nil:
				t.Errorf("%s refused fractional skip: %+v: %v", name, tab, err)
			case name == "totals" && (len(tab.Rows) != 1 || strings.Join(tab.Rows[0], " ") != "synthetic TOTAL all 0 0 0 0.00"):
				t.Errorf("missing zero TOTAL for skipped order: %+v", tab)
			}
		}
	}
}

func TestTwoRunsOfOneAccountEachCuttingOverAggregateInEitherOrder(t *testing.T) {
	// Each run's journal starts with a whole-share baseline and cuts over to fractional;
	// the order is judged per journal, so naming the runs in either order aggregates.
	run := func(name string, d time.Time, held int64) journalquery.Journal {
		return load(t, name, build(t, scaledSnapshot("synthetic", held, 0, d), scaledSnapshot("synthetic", held, event.FractionalQuantityScale, d.Add(time.Hour))))
	}
	// Two distinct months, so the runs are not read as copies of one history.
	first, second := run("run-1", at(time.March, 2), 1), run("run-2", at(time.April, 2), 2)
	for _, js := range [][]journalquery.Journal{{first, second}, {second, first}} {
		for name, fn := range aggregationViews {
			// Reconcile may well disagree on this synthetic pair (no fills explain the
			// change in holdings); what must not happen is a refusal of the scales.
			if _, err := fn(js, journalquery.Filter{}); err != nil && strings.Contains(err.Error(), "mixed quantity scales") {
				t.Errorf("%s refused two cut-over runs: %v", name, err)
			}
		}
	}
}

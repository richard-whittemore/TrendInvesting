package strategy

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestCloneCoversEveryReferenceTypedField keeps a transition's isolation
// complete as the state grows. A transition mutates candidate state and
// publishes it only on success (docs/development.md: reducer transactions); a
// map, slice or pointer shared with published state would let a rejected
// transition leak its mutation through it. This walks every type reachable
// from Reducer and lists each reference-typed path with the mechanism that
// isolates it, so a new path fails here until one of them handles it:
//
//   - eager: Reducer.begin copies it for every transaction;
//   - overlay: handlers reach it only through the transition's accessors,
//     which buffer changes until commit;
//   - on first access: instrumentState.clone deep-copies it when the
//     instrument accessor first hands the instrument to the transaction;
//   - immutable: an accepted fill is never mutated once recorded, so the
//     published history is shared rather than copied.
func TestCloneCoversEveryReferenceTypedField(t *testing.T) {
	t.Parallel()

	var got []string
	referencePaths(reflect.TypeOf(Reducer{}), "Reducer", map[reflect.Type]bool{}, &got)
	slices.Sort(got)

	const (
		eager       = "eager"
		overlay     = "overlay"
		firstAccess = "on first access"
		immutable   = "immutable"
	)
	isolation := map[string]string{
		"Reducer.acceptedFills (map)":                                   overlay,
		"Reducer.acceptedFills[v].unitIDs (slice)":                      immutable,
		"Reducer.classifications (map)":                                 eager,
		"Reducer.classifications[v].pending (slice)":                    eager,
		"Reducer.delisted (map)":                                        eager,
		"Reducer.fillDebits (slice)":                                    eager,
		"Reducer.holds (slice)":                                         eager,
		"Reducer.instruments (map)":                                     overlay,
		"Reducer.instruments[v] (pointer)":                              firstAccess,
		"Reducer.instruments[v]->.campaign (pointer)":                   firstAccess,
		"Reducer.instruments[v]->.campaign->.units (slice)":             firstAccess,
		"Reducer.instruments[v]->.entryChannel (pointer)":               firstAccess,
		"Reducer.instruments[v]->.entryChannel->.values (slice)":        firstAccess,
		"Reducer.instruments[v]->.exitChannel (pointer)":                firstAccess,
		"Reducer.instruments[v]->.exitChannel->.values (slice)":         firstAccess,
		"Reducer.instruments[v]->.n (pointer)":                          firstAccess,
		"Reducer.instruments[v]->.n->.seed (slice)":                     firstAccess,
		"Reducer.instruments[v]->.pendingAddProposal (pointer)":         firstAccess,
		"Reducer.instruments[v]->.pendingExitProposal (pointer)":        firstAccess,
		"Reducer.instruments[v]->.pendingProposal (pointer)":            firstAccess,
		"Reducer.instruments[v]->.pendingSignal (pointer)":              firstAccess,
		"Reducer.instruments[v]->.rawCloses (pointer)":                  firstAccess,
		"Reducer.instruments[v]->.rawCloses->.values (slice)":           firstAccess,
		"Reducer.instruments[v]->.rawVolumes (pointer)":                 firstAccess,
		"Reducer.instruments[v]->.rawVolumes->.values (slice)":          firstAccess,
		"Reducer.instruments[v]->.splitAdjustedCloses (pointer)":        firstAccess,
		"Reducer.instruments[v]->.splitAdjustedCloses->.values (slice)": firstAccess,
		"Reducer.notionalAccount (pointer)":                             eager,
		"Reducer.renamed (map)":                                         eager,
		"Reducer.sessionDelistedBars (slice)":                           eager,
	}
	want := slices.Sorted(maps.Keys(isolation))
	if !slices.Equal(got, want) {
		t.Fatalf("the reference-typed paths reachable from Reducer changed; isolate any new one (Reducer.begin, a transition accessor, or instrumentState.clone), then record it in this map.\ngot:  %q\nwant: %q", got, want)
	}

	// Each "on first access" path is checked against a real copy below, so
	// the list cannot claim more than instrumentState.clone does.
	var instrumentPaths []string
	for path, how := range isolation {
		if how == firstAccess && path != "Reducer.instruments[v] (pointer)" {
			instrumentPaths = append(instrumentPaths, strings.TrimPrefix(path, "Reducer.instruments[v]->"))
		}
	}
	slices.Sort(instrumentPaths)
	r, _ := transitionFixture(t, "partial-stop")
	s := r.instruments["AAPL"]
	s.pendingProposal = &pendingProposalState{proposalID: "entry"}
	s.pendingExitProposal = &pendingExitProposalState{proposalID: "exit"}
	s.pendingSignal = &pendingSignalState{signalID: "signal"}
	var checked []string
	assertNoSharedReferences(t, reflect.ValueOf(s).Elem(), reflect.ValueOf(s.clone()).Elem(), "", &checked)
	slices.Sort(checked)
	if !slices.Equal(checked, instrumentPaths) {
		t.Fatalf("instrumentState.clone was checked on %q, want every on-first-access path %q; populate the fixture so each is non-empty", checked, instrumentPaths)
	}

	// The eager paths are fresh in every transaction.
	r.sessionDelistedBars = []string{"DELISTED"}
	r.fillDebits = []fillDebit{{cost: 1}}
	r.holds = []hold{{proposalID: "held", cost: 1}}
	tx := r.begin()
	if tx.notionalAccount == r.notionalAccount || reflect.ValueOf(tx.delisted).Pointer() == reflect.ValueOf(r.delisted).Pointer() ||
		reflect.ValueOf(tx.sessionDelistedBars).Pointer() == reflect.ValueOf(r.sessionDelistedBars).Pointer() {
		t.Fatal("Reducer.begin shares an eagerly copied field with published state")
	}
	// A shared backing array would let a rejected transaction's change to a
	// debit reach the published ledger (ADR 0020).
	tx.fillDebits[0].cost = 2
	if r.fillDebits[0].cost != 1 {
		t.Fatal("Reducer.begin shares fillDebits' backing array with published state")
	}
	// Likewise for holds: a rejected transaction's reservation, or its
	// release, must never reach the published ledger (ADR 0020, as amended
	// 2026-09-24).
	tx.holds[0].cost = 2
	if r.holds[0].cost != 1 {
		t.Fatal("Reducer.begin shares holds' backing array with published state")
	}
	if tx.instruments != nil || tx.acceptedFills != nil {
		t.Fatal("a transition exposes the published instruments or accepted fills directly; handlers must use the accessors")
	}
}

// assertNoSharedReferences fails if any non-empty pointer or slice reachable
// from original is shared by copied, and records each path it compared.
func assertNoSharedReferences(t *testing.T, original, copied reflect.Value, path string, checked *[]string) {
	t.Helper()
	switch original.Kind() {
	case reflect.Pointer:
		if original.IsNil() {
			return
		}
		*checked = append(*checked, path+" (pointer)")
		if original.Pointer() == copied.Pointer() {
			t.Errorf("%s is shared with the copy", path)
			return
		}
		assertNoSharedReferences(t, original.Elem(), copied.Elem(), path+"->", checked)
	case reflect.Slice:
		if original.Len() == 0 {
			return
		}
		*checked = append(*checked, path+" (slice)")
		if original.Pointer() == copied.Pointer() {
			t.Errorf("%s is shared with the copy", path)
		}
	case reflect.Struct:
		if original.Type().PkgPath() == "time" {
			return
		}
		for i := range original.NumField() {
			assertNoSharedReferences(t, original.Field(i), copied.Field(i), path+"."+original.Type().Field(i).Name, checked)
		}
	}
}

// referencePaths appends every pointer, map, slice, interface, func and chan
// path reachable from t. time.Time is skipped: its location pointer is
// immutable and shared by design.
//
// seen tracks only the types on the CURRENT path from the root, not every
// type visited anywhere in the walk: it is unmarked again after each pointer
// recursion returns. That is enough to stop a genuinely self-referential type
// from recursing forever, and no more — two sibling fields of the identical
// pointer type (instrumentState's splitAdjustedCloses, rawCloses and
// rawVolumes, all *indicator.RollingWindow) are independent occurrences, not
// a cycle, and each must still contribute its own nested paths.
func referencePaths(t reflect.Type, prefix string, seen map[reflect.Type]bool, out *[]string) {
	switch t.Kind() {
	case reflect.Pointer:
		*out = append(*out, prefix+" (pointer)")
		if !seen[t.Elem()] {
			seen[t.Elem()] = true
			referencePaths(t.Elem(), prefix+"->", seen, out)
			delete(seen, t.Elem())
		}
	case reflect.Map:
		*out = append(*out, prefix+" (map)")
		referencePaths(t.Elem(), prefix+"[v]", seen, out)
	case reflect.Slice:
		*out = append(*out, prefix+" (slice)")
		referencePaths(t.Elem(), prefix+"[i]", seen, out)
	case reflect.Interface, reflect.Func, reflect.Chan:
		*out = append(*out, fmt.Sprintf("%s (%s)", prefix, t.Kind()))
	case reflect.Struct:
		if t.PkgPath() == "time" {
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			referencePaths(f.Type, prefix+"."+f.Name, seen, out)
		}
	}
}

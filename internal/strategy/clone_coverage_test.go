package strategy

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// TestCloneCoversEveryReferenceTypedField keeps Reducer.clone complete as the
// state grows. A transition mutates a candidate copy and publishes it only
// on success (docs/development.md: reducer transactions); a map, slice or
// pointer that clone copies shallowly would be shared with the published
// state, and a rejected transition would leak its mutation through it. This
// walks every type reachable from Reducer and lists each reference-typed
// path. clone must deep-copy every one of them, so a new path fails here
// until clone handles it and the list below records that it does.
func TestCloneCoversEveryReferenceTypedField(t *testing.T) {
	t.Parallel()

	var got []string
	referencePaths(reflect.TypeOf(Reducer{}), "Reducer", map[reflect.Type]bool{}, &got)
	slices.Sort(got)

	// Each path below is deep-copied by Reducer.clone.
	want := []string{
		"Reducer.acceptedFills (map)",
		"Reducer.acceptedFills[v].unitIDs (slice)",
		"Reducer.delisted (map)",
		"Reducer.instruments (map)",
		"Reducer.instruments[v] (pointer)",
		"Reducer.instruments[v]->.campaign (pointer)",
		"Reducer.instruments[v]->.campaign->.units (slice)",
		"Reducer.instruments[v]->.entryChannel (pointer)",
		"Reducer.instruments[v]->.entryChannel->.values (slice)",
		"Reducer.instruments[v]->.exitChannel (pointer)",
		"Reducer.instruments[v]->.exitChannel->.values (slice)",
		"Reducer.instruments[v]->.n (pointer)",
		"Reducer.instruments[v]->.n->.seed (slice)",
		"Reducer.instruments[v]->.pendingAddProposal (pointer)",
		"Reducer.instruments[v]->.pendingExitProposal (pointer)",
		"Reducer.instruments[v]->.pendingProposal (pointer)",
		"Reducer.notionalAccount (pointer)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the reference-typed paths reachable from Reducer changed; make Reducer.clone deep-copy any new one, then update this list.\ngot:  %q\nwant: %q", got, want)
	}
}

// referencePaths appends every pointer, map, slice, interface, func and chan
// path reachable from t. time.Time is skipped: its location pointer is
// immutable and shared by design.
func referencePaths(t reflect.Type, prefix string, seen map[reflect.Type]bool, out *[]string) {
	switch t.Kind() {
	case reflect.Pointer:
		*out = append(*out, prefix+" (pointer)")
		if !seen[t.Elem()] {
			seen[t.Elem()] = true
			referencePaths(t.Elem(), prefix+"->", seen, out)
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

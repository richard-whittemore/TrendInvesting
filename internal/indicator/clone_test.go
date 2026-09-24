package indicator

import (
	"reflect"
	"testing"
)

// Copying a completed-bar accumulator must isolate its entire backing storage
// so a rejected reducer transition cannot advance the next decision's inputs
// (CONTEXT.md: "Completed bar").
func TestClonesOwnTheirBuffers(t *testing.T) {
	t.Run("entry", func(t *testing.T) {
		original := &EntryChannel{length: 2, values: []float64{100, 101}, count: 2}
		want := &EntryChannel{length: 2, values: []float64{100, 101}, count: 2}
		copy := original.Clone()
		if !reflect.DeepEqual(copy, original) {
			t.Fatal("copy changed state")
		}
		copy.Add(200)
		if !reflect.DeepEqual(original, want) {
			t.Fatal("copy aliases original")
		}
		if (*EntryChannel)(nil).Clone() != nil {
			t.Fatal("nil clone")
		}
	})
	t.Run("exit", func(t *testing.T) {
		original := &ExitChannel{length: 2, values: []float64{100, 101}, count: 2}
		want := &ExitChannel{length: 2, values: []float64{100, 101}, count: 2}
		copy := original.Clone()
		if !reflect.DeepEqual(copy, original) {
			t.Fatal("copy changed state")
		}
		copy.Add(50)
		if !reflect.DeepEqual(original, want) {
			t.Fatal("copy aliases original")
		}
		if (*ExitChannel)(nil).Clone() != nil {
			t.Fatal("nil clone")
		}
	})
	t.Run("wilder", func(t *testing.T) {
		original := &WilderAverage{period: 20, seed: []float64{1, 2}, count: 2}
		want := &WilderAverage{period: 20, seed: []float64{1, 2}, count: 2}
		copy := original.Clone()
		if !reflect.DeepEqual(copy, original) {
			t.Fatal("copy changed state")
		}
		copy.seed[0] = 99
		copy.Add(7)
		if !reflect.DeepEqual(original, want) {
			t.Fatal("copy aliases original")
		}
		if (*WilderAverage)(nil).Clone() != nil {
			t.Fatal("nil clone")
		}
	})
}

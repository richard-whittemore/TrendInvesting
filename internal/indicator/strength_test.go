package indicator

import "testing"

// --- RollingWindow -----------------------------------------------------

func TestRollingWindowRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()
	if _, err := NewRollingWindow(0); err == nil {
		t.Fatal("NewRollingWindow(0) succeeded, want an error")
	}
	if _, err := NewRollingWindow(-1); err == nil {
		t.Fatal("NewRollingWindow(-1) succeeded, want an error")
	}
}

// TestRollingWindowValuesAreChronologicalOldestFirst is the arithmetic seam
// for the shared history buffer Strength's 64-close window and the 20-Session
// dollar-volume window both use: fewer than capacity values before it fills,
// and once full, the oldest value is evicted as a new one arrives, always
// reporting the window oldest-first.
func TestRollingWindowValuesAreChronologicalOldestFirst(t *testing.T) {
	t.Parallel()

	w, err := NewRollingWindow(3)
	if err != nil {
		t.Fatalf("NewRollingWindow() error = %v", err)
	}
	if got := w.Values(); len(got) != 0 {
		t.Fatalf("Values() before any Add = %v, want empty", got)
	}
	if w.Full() {
		t.Fatal("Full() = true before any Add")
	}

	w.Add(1)
	if got, want := w.Values(), []float64{1}; !equalFloats(got, want) {
		t.Fatalf("Values() = %v, want %v", got, want)
	}
	if w.Full() {
		t.Fatal("Full() = true with 1 of 3 Added")
	}

	w.Add(2)
	w.Add(3)
	if !w.Full() {
		t.Fatal("Full() = false with 3 of 3 Added")
	}
	if got, want := w.Values(), []float64{1, 2, 3}; !equalFloats(got, want) {
		t.Fatalf("Values() = %v, want %v", got, want)
	}

	// A fourth Add evicts the oldest value (1).
	w.Add(4)
	if got, want := w.Values(), []float64{2, 3, 4}; !equalFloats(got, want) {
		t.Fatalf("Values() after eviction = %v, want %v", got, want)
	}

	w.Add(5)
	w.Add(6)
	if got, want := w.Values(), []float64{4, 5, 6}; !equalFloats(got, want) {
		t.Fatalf("Values() after two more evictions = %v, want %v", got, want)
	}
}

func TestRollingWindowValuesReturnsACopy(t *testing.T) {
	t.Parallel()
	w, err := NewRollingWindow(2)
	if err != nil {
		t.Fatalf("NewRollingWindow() error = %v", err)
	}
	w.Add(1)
	w.Add(2)
	got := w.Values()
	got[0] = 99
	if again := w.Values(); again[0] != 1 {
		t.Fatalf("mutating a returned Values() slice changed the window: got %v", again)
	}
}

// TestRollingWindowCloneIsolatesItsBuffer mirrors
// internal/indicator's existing clone_test.go: a transaction's copy must not
// alias the published window's backing array (docs/development.md: reducer
// transactions).
func TestRollingWindowCloneIsolatesItsBuffer(t *testing.T) {
	t.Parallel()
	w, err := NewRollingWindow(2)
	if err != nil {
		t.Fatalf("NewRollingWindow() error = %v", err)
	}
	w.Add(1)
	w.Add(2)
	cloned := w.Clone()
	cloned.Add(3)
	if got, want := w.Values(), []float64{1, 2}; !equalFloats(got, want) {
		t.Fatalf("Clone aliases the original: original Values() = %v, want %v", got, want)
	}
	if (*RollingWindow)(nil).Clone() != nil {
		t.Fatal("nil clone")
	}
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Strength ------------------------------------------------------------

// TestStrengthIsThePriceChangeOverTheLookbackDividedByN is the arithmetic
// seam for Faith's ranking measure (CONTEXT.md: "Strength"; The Turtle Rules
// p.29, ADR 0010, as amended by the owner's decision of 2026-09-25): a hand
// built series of exactly StrengthLookbackBars+1 closes.
func TestStrengthIsThePriceChangeOverTheLookbackDividedByN(t *testing.T) {
	t.Parallel()

	closes := make([]float64, StrengthLookbackBars+1)
	for i := range closes {
		closes[i] = 100 + float64(i) // close(d-63)=100, close(d)=163
	}
	got, ready := Strength(closes, 5)
	if !ready {
		t.Fatal("ready = false, want true with exactly StrengthLookbackBars+1 closes")
	}
	want := (163.0 - 100.0) / 5.0
	if got != want {
		t.Fatalf("Strength() = %v, want %v", got, want)
	}
}

// TestStrengthDeclinesInsufficientHistory is the hand-built insufficient-
// history case the owner's decision requires (fewer than 64 closes): the
// instrument cannot be ranked, so Strength must say so rather than compute
// from a shorter window.
func TestStrengthDeclinesInsufficientHistory(t *testing.T) {
	t.Parallel()

	closes := make([]float64, StrengthLookbackBars) // one short of 64
	for i := range closes {
		closes[i] = 100
	}
	if _, ready := Strength(closes, 5); ready {
		t.Fatal("ready = true with one fewer than StrengthLookbackBars+1 closes, want false")
	}

	if _, ready := Strength(nil, 5); ready {
		t.Fatal("ready = true with no closes at all, want false")
	}
}

// TestStrengthUsesOnlyTheLatestLookbackWindow: extra history beyond the
// window must not change the result, since Strength reads exactly
// close(d) and close(d-StrengthLookbackBars) — the two most recent points 63
// apart, not the series' own start.
func TestStrengthUsesOnlyTheLatestLookbackWindow(t *testing.T) {
	t.Parallel()

	closes := make([]float64, StrengthLookbackBars+1+10) // ten extra, older points
	for i := range closes {
		closes[i] = float64(i)
	}
	got, ready := Strength(closes, 2)
	if !ready {
		t.Fatal("ready = false, want true")
	}
	last := len(closes) - 1
	want := (closes[last] - closes[last-StrengthLookbackBars]) / 2
	if got != want {
		t.Fatalf("Strength() = %v, want %v (must ignore history older than the window)", got, want)
	}
}

// --- MedianDollarVolume ----------------------------------------------------

// TestMedianDollarVolumeIsTheMedianOfCloseTimesVolume is the arithmetic seam
// for the one definition ADR 0009's $5M eligibility test and ADR 0010's
// ranking tie-break both read (ADR 0004: the raw view; ADR 0010, as amended
// 2026-09-25): DollarVolumeWindow is always even, so this also exercises the
// even-length median (the mean of the two middle values) with a hand-built,
// deliberately smaller even-length series to make the calculation
// hand-checkable.
func TestMedianDollarVolumeIsTheMedianOfCloseTimesVolume(t *testing.T) {
	t.Parallel()

	// Four dollar volumes once sorted: 100, 200, 300, 400 — median (200+300)/2.
	closes := []float64{10, 20, 40, 30}
	volumes := []float64{10, 10, 10, 10}
	// dollar volumes: 100, 200, 400, 300 — sorted 100,200,300,400.
	got, ready := medianDollarVolumeOfWindow(closes, volumes)
	if !ready {
		t.Fatal("ready = false, want true (helper always uses a full, even-length window)")
	}
	want := (200.0 + 300.0) / 2.0
	if got != want {
		t.Fatalf("median = %v, want %v", got, want)
	}
}

// medianDollarVolumeOfWindow calls MedianDollarVolume at the real,
// fixed DollarVolumeWindow length the production code always uses, built by
// repeating the four hand-chosen (close, volume) pairs until the window is
// full. DollarVolumeWindow is a multiple of four, so the repetition preserves
// the four dollar volumes' relative proportions exactly (five of each),
// leaving the same hand-checkable median this test asserts.
// TestMedianDollarVolumeRequiresTheFullWindow exercises the window-size gate
// on its own, with a window one short of DollarVolumeWindow.
func medianDollarVolumeOfWindow(closes, volumes []float64) (float64, bool) {
	fullCloses := make([]float64, 0, DollarVolumeWindow)
	fullVolumes := make([]float64, 0, DollarVolumeWindow)
	for len(fullCloses) < DollarVolumeWindow {
		fullCloses = append(fullCloses, closes...)
		fullVolumes = append(fullVolumes, volumes...)
	}
	fullCloses = fullCloses[:DollarVolumeWindow]
	fullVolumes = fullVolumes[:DollarVolumeWindow]
	return MedianDollarVolume(fullCloses, fullVolumes)
}

// TestMedianDollarVolumeRequiresTheFullWindow is the hand-built insufficient-
// history case for the tie-break: fewer than DollarVolumeWindow raw
// closes/volumes cannot be ranked (the owner's decision of 2026-09-25).
func TestMedianDollarVolumeRequiresTheFullWindow(t *testing.T) {
	t.Parallel()

	closes := make([]float64, DollarVolumeWindow-1)
	volumes := make([]float64, DollarVolumeWindow-1)
	for i := range closes {
		closes[i], volumes[i] = 10, 1_000_000
	}
	if _, ready := MedianDollarVolume(closes, volumes); ready {
		t.Fatal("ready = true with one fewer than DollarVolumeWindow elements, want false")
	}
}

// TestMedianDollarVolumePanicsOnMismatchedLengths: the two windows are always
// fed in lockstep by the caller (one raw close and one raw volume per
// completed Session), so a length mismatch is a caller defect, not a data
// condition worth reporting as merely "not ready".
func TestMedianDollarVolumePanicsOnMismatchedLengths(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("MedianDollarVolume did not panic on mismatched lengths")
		}
	}()
	MedianDollarVolume(make([]float64, DollarVolumeWindow), make([]float64, DollarVolumeWindow-1))
}

// TestMedianDollarVolumeOddLength documents medianOf's odd-length branch,
// exercised here directly since DollarVolumeWindow itself is always even.
func TestMedianDollarVolumeOddLength(t *testing.T) {
	t.Parallel()
	if got, want := medianOf([]float64{3, 1, 2}), 2.0; got != want {
		t.Fatalf("medianOf(odd) = %v, want %v", got, want)
	}
}

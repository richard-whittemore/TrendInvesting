package replay

import (
	"bytes"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// Divergence names the first place two decision streams differ, by
// position. A nil *Divergence from Equivalent means the streams are
// byte-identical.
type Divergence struct {
	Index int
	// Want and Got are the compared envelopes at Index, or nil when the
	// streams differ only in length: Want is nil when got holds a decision
	// beyond the end of want, Got is nil when want holds one beyond the end
	// of got.
	Want *event.Envelope
	Got  *event.Envelope
}

// Equivalent reports the first point at which got diverges from want,
// decision for decision, in order — or nil if none.
//
// Envelopes are compared by event.CanonicalEnvelopeBytes rather than by the
// decoded Go structs: two time.Time values can represent the same recorded
// instant and still fail reflect.DeepEqual (a different Location pointer, a
// monotonic reading picked up from wherever the value was constructed),
// which would report a divergence that is not really there. Canonical bytes
// are this project's one definition of "the same envelope" (ADR 0016) — the
// same definition the journal's own hash chain hashes over — so "byte
// identical replay" means here exactly what it means when a journal is
// hashed.
func Equivalent(want, got []event.Envelope) *Divergence {
	for i := 0; i < len(want) || i < len(got); i++ {
		switch {
		case i >= len(got):
			w := want[i]
			return &Divergence{Index: i, Want: &w}
		case i >= len(want):
			g := got[i]
			return &Divergence{Index: i, Got: &g}
		default:
			w, g := want[i], got[i]
			if !bytes.Equal(event.CanonicalEnvelopeBytes(w), event.CanonicalEnvelopeBytes(g)) {
				return &Divergence{Index: i, Want: &w, Got: &g}
			}
		}
	}
	return nil
}

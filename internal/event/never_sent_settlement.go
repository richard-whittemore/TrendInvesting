package event

import (
	"errors"
	"fmt"
	"strings"
)

// The closed set of evidence an adapter's settlement of a never-sent order
// carries: what its own write-ahead record says of the order (the relay's
// hand-over record, adapter/ibkr/relay.py). An order the record marks
// handed over, sent, or allowed to be sent again may have reached IBKR, so
// it is never settled this way (fail closed, AGENTS.md).
const (
	// NeverSentUnmarked: the order's input awaited its reply when the run
	// ended and the record holds no mark for it, so the adapter never wrote
	// "about to send" for it and never handed it over (write-ahead).
	NeverSentUnmarked = "unmarked"
	// NeverSentRefused: the adapter recorded that it refused the order for
	// good before any mark (its refusal record), so it never handed it over.
	NeverSentRefused = "refused"
	// NeverSentMarked: the record marks the order never sent, or, for an
	// input settled before the run ended, the last decision the adapter
	// recorded for it settles it so: the owner settled it so, or the adapter
	// retired it.
	NeverSentMarked = "never-sent"
)

// NeverSentSettlement is an IBKR adapter's settlement, written once the run
// that released the order has ended, of one order that run released and the
// adapter never sent: the run's durable log by file name and the run's
// identity, the input whose committed reply released the order, the
// adapter's evidence that it never sent it, and the retirement it amounts
// to, retired by the owner who ran the settlement, as the relay's retire
// command journals one (OrderRetiredByOwnerReason). It is never journalled
// for the run that released the order, which has ended: the owner's
// approval of the fresh run carries it, and the restart reading applies it
// to the approved log by the engine's retirement rule
// (targetweight.RetirementVerdict), so a sale back to 98% it settles is no
// attempt (ADR 0019, amended 2026-10-17, reading 4, as the owner's decision
// of 2026-10-22 reads it).
type NeverSentSettlement struct {
	Log        string              `json:"log"`
	Run        string              `json:"run"`
	InputID    string              `json:"input_id"`
	Evidence   string              `json:"evidence"`
	Retirement OrderRetiredPayload `json:"retirement"`
}

// Validate checks that the settlement names a log by file name, a run and
// the releasing input, carries evidence of the closed set, and retires its
// order as the owner's retirement, valid as any retirement.
func (s NeverSentSettlement) Validate() error {
	var errs []error
	switch {
	case s.Log == "":
		errs = append(errs, errors.New("the settlement names no log"))
	case strings.ContainsAny(s.Log, `/\`) || s.Log == "." || s.Log == "..":
		errs = append(errs, fmt.Errorf("the settlement's log %q must be a file name, not a path", s.Log))
	}
	if s.Run == "" {
		errs = append(errs, errors.New("the settlement names no run: the adapter names the run that released the order"))
	}
	if s.InputID == "" {
		errs = append(errs, errors.New("the settlement names no input: the adapter names the input whose reply released the order"))
	}
	switch s.Evidence {
	case NeverSentUnmarked, NeverSentRefused, NeverSentMarked:
	default:
		errs = append(errs, fmt.Errorf("the settlement's evidence %q is none of %q, %q or %q", s.Evidence, NeverSentUnmarked, NeverSentRefused, NeverSentMarked))
	}
	if s.Retirement.Reason != OrderRetiredByOwnerReason {
		errs = append(errs, fmt.Errorf("the settlement retires its order for %q; the owner settles an ended run's never-sent order, as %q", s.Retirement.Reason, OrderRetiredByOwnerReason))
	}
	if err := s.Retirement.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid never-sent settlement of order %q: %w", s.Retirement.ClientOrderID, err)
	}
	return nil
}

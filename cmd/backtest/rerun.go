package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// rerunInputs contains only independent facts supplied to drive (ADR 0005).
// The Variant's trading settings live in cfg; the registry-only Variant label
// is not a pipeline input (ADR 0012). Recorded fills must never enter drive.
type rerunInputs struct {
	cfg     event.ConfigurationPayload
	bars    []event.CompletedBarPayload
	actions []event.CorporateActionPayload
	// openingCash is the cash the simulated account opened with. The
	// account's statements are simulator output, derived like the fills;
	// only the first, stated before any fill, still says what it opened
	// with.
	openingCash float64
}

// pipelineEquivalence regenerates a completed backtest through drive and
// compares the entire ordered journal (ADR 0005, ADR 0017). Every header field,
// record sequence, kind, chain hash and canonical envelope byte is compared;
// payload bytes, timestamps and the original build suffix are retained. No
// run-specific field is excluded: inputEnvelope uses event time, not a clock.
// Outer JSON layout and equivalent timestamp zones are representation only,
// normalized by journal.Read and ADR 0016's canonical envelope comparison.
// This is record equivalence, not literal file-byte equality.
//
// A missing completion marker is refused: failed/abandoned runs may lack bars
// for fills already recorded, and their stopping condition is not journalled.
// The regenerated recorder is bounded at the recorded length plus one input
// boundary, enough to expose extra output without guessing the original limit.
func pipelineEquivalence(ctx context.Context, r io.Reader) error {
	header, records, err := journal.Read(contextReader{ctx, r})
	if err != nil {
		return err
	}
	if err := journal.CheckIdentity(header, records); err != nil {
		return err
	}
	if err := journal.CheckSpan(header, records); err != nil {
		return err
	}
	inputs, _, err := journal.Split(records)
	if err != nil {
		return err
	}
	cfg, err := journalConfiguration(header, inputs)
	if err != nil {
		return err
	}
	facts, err := reconstructRun(cfg, inputs)
	if err != nil {
		return err
	}
	reducer, err := strategy.NewReducer(header.StrategyVersion, cfg)
	if err != nil {
		return err
	}
	simulator, err := fills.New(cfg, header.StrategyVersion, header.ConfigurationHash)
	if err != nil {
		return err
	}
	if err := simulator.OpenAccount(facts.openingCash); err != nil {
		return err
	}
	recorder := journal.NewBoundedRecorder(reducer, len(records)+1)
	runErr := drive(ctx, simulator, recorder, facts.cfg, header.StrategyVersion, facts.bars, facts.actions)
	entries := recorder.Entries()
	if ctxErr := ctx.Err(); runErr != nil && ctxErr != nil && errors.Is(runErr, ctxErr) {
		// A re-run stopped by the operator's own cancellation ends short of
		// the journal. Comparing that partial output would report a
		// divergence the pipeline never made. Only a failure that IS the
		// cancellation is treated this way: an independent pipeline failure
		// that happens to coincide with it still falls through below, so the
		// audit never hides the defect it exists to find.
		return fmt.Errorf("backtest: rerun stopped before completing: %w", runErr)
	}
	if runErr != nil {
		// A failed new pipeline may have a shorter span. Compare its prefix
		// first so an altered fill is reported before the resulting failure.
		return errors.Join(comparePipelineRecords(header, records, entries),
			fmt.Errorf("backtest: pipeline execution failed: %w", runErr))
	}
	regenerated, err := recorder.Header(header.ConfigurationHash, header.StrategyVersion)
	if err != nil {
		return err
	}
	header.SpanStart, header.SpanEnd = header.SpanStart.UTC(), header.SpanEnd.UTC()
	regenerated.SpanStart, regenerated.SpanEnd = regenerated.SpanStart.UTC(), regenerated.SpanEnd.UTC()
	if header != regenerated {
		return fmt.Errorf("backtest: pipeline divergence in journal header: recorded %+v, regenerated %+v", header, regenerated)
	}
	if err := comparePipelineRecords(regenerated, records, entries); err != nil {
		return err
	}
	return stoppedBy(ctx)
}

// reconstructRun refuses missing, repeated and unsupported independent inputs
// rather than defaulting them (docs/development.md principle 4, ADR 0015).
// Configuration and completion each occur exactly once in drive's protocol.
// The simulated account states every Session's close, so its snapshots are
// derived output like the fills; the opening cash is read from the first,
// and only when no fill precedes it, since a later statement is not the
// opening cash.
func reconstructRun(cfg event.ConfigurationPayload, inputs []event.Envelope) (rerunInputs, error) {
	facts := rerunInputs{cfg: cfg}
	counts := make(map[string]int)
	for i, input := range inputs {
		if err := input.Validate(); err != nil {
			return facts, fmt.Errorf("backtest: rerun input %d: %w", i+1, err)
		}
		counts[input.Type]++
		var err error
		switch input.Type {
		case event.ConfigurationEventType:
			if i != 0 || counts[input.Type] != 1 {
				return facts, errors.New("backtest: rerun requires exactly one configuration as the first input")
			}
			err = decodeRerunInput(input, event.ConfigurationSchemaVersion, &facts.cfg)
		case event.AccountSnapshotEventType:
			if counts[input.Type] != 1 {
				// Derived from the bars and the opening cash: compared
				// later, never replayed into the pipeline.
				break
			}
			if counts[event.FillEventType] != 0 {
				return facts, errors.New("backtest: rerun cannot read the opening cash from an account.snapshot that follows a fill")
			}
			var opening event.AccountSnapshotPayload
			err = decodeRerunInput(input, event.AccountSnapshotSchemaVersion, &opening)
			facts.openingCash = opening.AvailableCash
		case event.CompletedBarEventType:
			var bar event.CompletedBarPayload
			err = decodeRerunInput(input, event.CompletedBarSchemaVersion, &bar)
			facts.bars = append(facts.bars, bar)
		case event.MarketCorporateActionEventType:
			var action event.CorporateActionPayload
			err = decodeRerunInput(input, event.MarketCorporateActionSchemaVersion, &action)
			facts.actions = append(facts.actions, action)
		case event.RunCompletedEventType:
			if i != len(inputs)-1 || counts[input.Type] != 1 {
				return facts, errors.New("backtest: rerun requires exactly one run.completed as the last input")
			}
			err = decodeRerunInput(input, event.RunCompletedSchemaVersion, &event.RunCompletedPayload{})
		case event.FillEventType, event.SessionClosedEventType:
			// Simulator and driver output, derived from the bars: compared
			// later, never replayed into the pipeline.
		default:
			return facts, fmt.Errorf("backtest: rerun cannot reconstruct unsupported input %q", input.Type)
		}
		if err != nil {
			return facts, err
		}
	}
	for _, required := range []string{event.ConfigurationEventType, event.AccountSnapshotEventType, event.CompletedBarEventType, event.RunCompletedEventType} {
		if counts[required] == 0 {
			return facts, fmt.Errorf("backtest: rerun missing required %s input", required)
		}
	}
	return facts, nil
}

// decodeRerunInput validates the recorded schema before decoding: regeneration
// must not silently upcast an independent input (ADR 0015).
func decodeRerunInput(input event.Envelope, version uint32, payload interface{ Validate() error }) error {
	if input.SchemaVersion != version {
		return fmt.Errorf("backtest: rerun %s schema %d is not supported; require %d", input.Type, input.SchemaVersion, version)
	}
	if err := json.Unmarshal(input.Payload, payload); err != nil {
		return fmt.Errorf("backtest: rerun decode %s: %w", input.Type, err)
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("backtest: rerun invalid %s: %w", input.Type, err)
	}
	return nil
}

// comparePipelineRecords reports the first record difference, including
// simulator inputs, reducer decisions and their interleaving (ADR 0017).
// Equivalent supplies the project's canonical envelope comparison; record
// metadata is compared separately because a Divergence holds only envelopes.
func comparePipelineRecords(header journal.Header, want []journal.Record, got []journal.Entry) error {
	chain := journal.NewChain(header)
	var sequence uint64
	for i := 0; i < len(want) || i < len(got); i++ {
		sequence++
		prefix := fmt.Sprintf("backtest: pipeline divergence at record %d", i+1)
		if i >= len(got) {
			return fmt.Errorf("%s: recorded %s, regenerated nothing", prefix, describeDivergent(&want[i].Envelope))
		}
		if i >= len(want) {
			return fmt.Errorf("%s: recorded nothing, regenerated %s", prefix, describeDivergent(&got[i].Envelope))
		}
		w, g := want[i], got[i]
		if w.Sequence != sequence {
			return fmt.Errorf("%s: recorded sequence %d, regenerated %d", prefix, w.Sequence, i+1)
		}
		if w.Kind != g.Kind {
			return fmt.Errorf("%s: recorded kind %s, regenerated %s", prefix, w.Kind, g.Kind)
		}
		if d := replay.Equivalent([]event.Envelope{w.Envelope}, []event.Envelope{g.Envelope}); d != nil {
			return fmt.Errorf("%s: recorded %s [payload_hash=%s], regenerated %s [payload_hash=%s]; canonical envelope bytes differ", prefix, describeDivergent(d.Want), d.Want.PayloadHash, describeDivergent(d.Got), d.Got.PayloadHash)
		}
		if hash := chain.Next(g.Kind, g.Envelope); w.RecordHash != hash {
			return fmt.Errorf("%s: recorded record_hash %s, regenerated %s", prefix, w.RecordHash, hash)
		}
	}
	return nil
}

// doRerun reports whole-pipeline equivalence separately from chain verification
// and reducer replay (ADR 0005, ADR 0017); it never installs new evidence.
func doRerun(ctx context.Context, path string, out io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("backtest: open the journal to rerun: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := pipelineEquivalence(ctx, file); err != nil {
		return fmt.Errorf("backtest: %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(out, "journal %s reruns with no pipeline divergence (all records byte-identical under canonical comparison)\n", path); err != nil {
		return fmt.Errorf("backtest: report the pipeline rerun: %w", err)
	}
	return nil
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// perturbFill changes the simulator's first fill before the recorder and
// reducer see it, modelling the output instability ADR 0005 must avoid.
// Only tests can install this handler; the recorded journal stays untouched.
type perturbFill struct {
	handler replay.Handler
	changed *bool
	t       *testing.T
}

func (p perturbFill) Apply(ctx context.Context, e event.Envelope) ([]event.Envelope, error) {
	if e.Type == event.FillEventType && !*p.changed {
		var fill event.FillPayload
		if err := json.Unmarshal(e.Payload, &fill); err != nil {
			p.t.Fatal(err)
		}
		fill.Price = math.Nextafter(fill.Price, math.Inf(1))
		payload, err := json.Marshal(fill)
		if err != nil {
			p.t.Fatal(err)
		}
		e.Payload, e.PayloadHash = payload, event.HashPayload(payload)
		*p.changed = true
	}
	return p.handler.Apply(ctx, e)
}

// TestRerunDetectsSimulatorDriftThatReplayCannot proves ADR 0017 reducer
// replay can pass while regeneration of ADR 0005 simulator output diverges.
func TestRerunDetectsSimulatorDriftThatReplayCannot(t *testing.T) {
	original := runSession
	t.Cleanup(func() { runSession = original })
	changed := false
	runSession = func(ctx context.Context, sim *fills.Simulator, handler replay.Handler, bars []event.Envelope) (fills.Result, error) {
		return original(ctx, sim, perturbFill{handler: handler, changed: &changed, t: t}, bars)
	}
	var out bytes.Buffer
	err := run(context.Background(), []string{"-rerun", goldenJournal}, &out)
	if err == nil || !strings.Contains(err.Error(), "pipeline divergence at record") || !strings.Contains(err.Error(), event.FillEventType) {
		t.Fatalf("-rerun must detect the changed simulator fill as the first pipeline divergence; got error %v, output %q", err, out.String())
	}
	if !changed {
		t.Fatal("the simulator never produced the perturbed fill")
	}
	t.Logf("-rerun: %v", err)
	out.Reset()
	if err := run(context.Background(), []string{"-replay", goldenJournal}, &out); err != nil {
		t.Fatal(err)
	}
	t.Logf("-replay: %s", strings.TrimSpace(out.String()))
}

// TestRerunGoldens reads committed evidence without regenerating the reference
// side (ADR 0012, ADR 0017), including a distinct declared Variant.
func TestRerunGoldens(t *testing.T) {
	for _, path := range []string{goldenJournal, "testdata/variants/profit-protecting-stop/journal.golden.jsonl", "testdata/variants/uncapped/journal.golden.jsonl"} {
		var out bytes.Buffer
		if err := run(context.Background(), []string{"-rerun", path}, &out); err != nil {
			t.Fatal(err)
		}
		t.Log(strings.TrimSpace(out.String()))
	}
}

// TestRerunReconstructsIndependentInputs exercises the recorded ADR 0010 cash
// figure and the corporate-action interleaving already implemented by drive.
func TestRerunReconstructsIndependentInputs(t *testing.T) {
	for _, cash := range []string{"0", "1000", "100000"} {
		_, path := runWithCashFlags(t, "-available-cash", cash)
		if err := doRerun(context.Background(), path, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []struct{ bars, actions string }{
		{barsDelistingFixture, corporateActionsDelistingFixture},
		{barsFixture, corporateActionsUntradedFixture},
		{barsGroupedInstrumentsFixture, corporateActionsGroupedInstrumentsFixture},
		{barsFixture, corporateActionsUnorderedFixture},
	} {
		path := filepath.Join(t.TempDir(), "journal.jsonl")
		err := backtest(context.Background(), options{configPath: configurationFixture, barsPath: fixture.bars, corporateActionsPath: fixture.actions, outPath: path, build: "another-build"}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if err := doRerun(context.Background(), path, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRerunMissingInputsFailClosed removes independent facts while repairing
// the chain: a refusal must name the missing fact, not guess it (ADR 0017).
func TestRerunMissingInputsFailClosed(t *testing.T) {
	for _, missing := range []string{event.ConfigurationEventType, event.AccountSnapshotEventType, event.CompletedBarEventType, event.RunCompletedEventType} {
		t.Run(missing, func(t *testing.T) {
			path := rewriteJournal(t, goldenJournal, func(_ *journal.Header, entries []journal.Entry) []journal.Entry {
				var kept []journal.Entry
				for _, e := range entries {
					if e.Envelope.Type != missing {
						kept = append(kept, e)
					}
				}
				return kept
			})
			err := doRerun(context.Background(), path, io.Discard)
			if err == nil {
				t.Fatalf("accepted missing %s", missing)
			}
			// Remove the path before matching: t.TempDir includes the subtest
			// name, so matching it would not prove the refusal named the input.
			message := strings.ReplaceAll(err.Error(), path, "<journal>")
			want := "missing required " + missing
			if missing == event.ConfigurationEventType {
				want = "no configuration event"
			}
			if !strings.Contains(message, want) {
				t.Fatalf("missing %s: %s", missing, message)
			}
			t.Log(message)
		})
	}
}

// encodeRerunEvidence retains deliberately malformed evidence for refusal
// tests, bypassing Write's producer validation without changing any fixture.
func encodeRerunEvidence(t *testing.T, header journal.Header, records []journal.Record) []byte {
	t.Helper()
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	if err := enc.Encode(header); err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := enc.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	return raw.Bytes()
}

// TestRerunIdentityRefusals requires the same identity checks as reducer
// replay, including headers whose records agree with the false claim.
func TestRerunIdentityRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mutate     func(*journal.Header, []journal.Record)
	}{
		{"uncomposed", "strategy version", func(h *journal.Header, rs []journal.Record) {
			h.StrategyVersion = "uncomposed"
			for i := range rs {
				rs[i].Envelope.StrategyVersion = h.StrategyVersion
			}
		}},
		{"rules", "rules version", func(h *journal.Header, rs []journal.Record) {
			h.StrategyVersion = event.ComposeStrategyVersion("turtle", "9.9.9", "test")
			for i := range rs {
				rs[i].Envelope.StrategyVersion = h.StrategyVersion
			}
		}},
		{"strategy", "configuration it records declares", func(h *journal.Header, rs []journal.Record) {
			_, rules, build, err := event.DecomposeStrategyVersion(h.StrategyVersion)
			if err != nil {
				t.Fatal(err)
			}
			h.StrategyVersion = event.ComposeStrategyVersion("other", rules, build)
			for i := range rs {
				rs[i].Envelope.StrategyVersion = h.StrategyVersion
			}
		}},
		{"hash", "hashes to", func(h *journal.Header, rs []journal.Record) {
			h.ConfigurationHash = "sha256:wrong"
			for i := range rs {
				rs[i].Envelope.ConfigurationHash = h.ConfigurationHash
			}
		}},
		{"mixed identity", "record 1 states", func(_ *journal.Header, rs []journal.Record) { rs[0].Envelope.ConfigurationHash = "other" }},
		{"span", "header states the span", func(h *journal.Header, _ []journal.Record) { h.SpanEnd = h.SpanEnd.AddDate(0, 0, 1) }},
		{"kind", "neither", func(_ *journal.Header, rs []journal.Record) { rs[0].Kind = "unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header, records := readJournalFile(t, goldenJournal)
			tc.mutate(&header, records)
			err := pipelineEquivalence(context.Background(), bytes.NewReader(encodeRerunEvidence(t, header, records)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRerunRefusesUnsupportedIndependentInputs(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mutate     func([]event.Envelope) []event.Envelope
	}{
		{"opening cash after a fill", "follows a fill", func(es []event.Envelope) []event.Envelope {
			fill, snapshot := firstOfType(t, es, event.FillEventType), firstOfType(t, es, event.AccountSnapshotEventType)
			moved := append([]event.Envelope{}, es[:snapshot]...)
			moved = append(moved, es[fill])
			return append(moved, es[snapshot:]...)
		}},
		{"duplicate configuration", "exactly one configuration", func(es []event.Envelope) []event.Envelope { return append(es[:len(es)-1], es[0], es[len(es)-1]) }},
		{"early completion", "run.completed as the last", func(es []event.Envelope) []event.Envelope { return append(es, es[len(es)-1]) }},
		{"unknown input", "unsupported input", func(es []event.Envelope) []event.Envelope { es[2].Type = "unknown"; return es }},
		{"old schema", "schema 1", func(es []event.Envelope) []event.Envelope {
			es[firstOfType(t, es, event.AccountSnapshotEventType)].SchemaVersion = 1
			return es
		}},
		{"invalid envelope", "payload hash", func(es []event.Envelope) []event.Envelope {
			es[firstOfType(t, es, event.AccountSnapshotEventType)].PayloadHash = "wrong"
			return es
		}},
		{"missing available cash", "available cash is required", func(es []event.Envelope) []event.Envelope {
			var fields map[string]json.RawMessage
			opening := firstOfType(t, es, event.AccountSnapshotEventType)
			if err := json.Unmarshal(es[opening].Payload, &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, "available_cash")
			p, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			es[opening].Payload = p
			es[opening].PayloadHash = event.HashPayload(p)
			return es
		}},
		{"malformed bar", "decode market.bar.completed", func(es []event.Envelope) []event.Envelope {
			bar := firstOfType(t, es, event.CompletedBarEventType)
			es[bar].Payload = []byte(`"wrong"`)
			es[bar].PayloadHash = event.HashPayload(es[bar].Payload)
			return es
		}},
		{"invalid bar", "invalid market.bar.completed", func(es []event.Envelope) []event.Envelope {
			bar := firstOfType(t, es, event.CompletedBarEventType)
			es[bar].Payload = []byte(`{}`)
			es[bar].PayloadHash = event.HashPayload(es[bar].Payload)
			return es
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header, records := readJournalFile(t, goldenJournal)
			inputs, _, err := journal.Split(records)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := journalConfiguration(header, inputs)
			if err != nil {
				t.Fatal(err)
			}
			_, err = reconstructRun(cfg, tc.mutate(inputs))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// TestRerunComparesEveryRecordField pins the comparison boundary: a change in
// any record field is a pipeline divergence even if decisions still replay.
func TestRerunComparesEveryRecordField(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mutate     func([]journal.Record) []journal.Record
	}{
		{"sequence", "record 3: recorded sequence", func(rs []journal.Record) []journal.Record { rs[2].Sequence++; return rs }},
		{"kind", "record 2: recorded kind", func(rs []journal.Record) []journal.Record { rs[1].Kind = journal.KindDecision; return rs }},
		{"chain", "record 3: recorded record_hash", func(rs []journal.Record) []journal.Record { rs[2].RecordHash = "wrong"; return rs }},
		{"envelope", "record 3: recorded", func(rs []journal.Record) []journal.Record {
			rs[2].Envelope.RecordedAt = rs[2].Envelope.RecordedAt.AddDate(0, 0, 1)
			return rs
		}},
		{"shorter recording", "recorded nothing", func(rs []journal.Record) []journal.Record { return rs[:len(rs)-1] }},
		{"longer recording", "regenerated nothing", func(rs []journal.Record) []journal.Record { return append(rs, rs[len(rs)-1]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, rs := readJournalFile(t, goldenJournal)
			es := make([]journal.Entry, len(rs))
			for i, r := range rs {
				es[i] = journal.Entry{Kind: r.Kind, Envelope: r.Envelope}
			}
			err := comparePipelineRecords(h, tc.mutate(rs), es)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRerunCLIRefusals(t *testing.T) {
	for _, args := range [][]string{
		{"-rerun", goldenJournal, "-replay", goldenJournal},
		{"-rerun", goldenJournal, "-verify", goldenJournal},
		{"-rerun", goldenJournal, "-available-cash", "0"},
		{"-rerun", goldenJournal, "-bars", barsFixture},
		{"-rerun", goldenJournal, "-variant", "other"},
		{"-rerun", goldenJournal, "-max-records", "5"},
	} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Fatalf("accepted incompatible arguments %v", args)
		}
	}
	if err := doRerun(context.Background(), filepath.Join(t.TempDir(), "absent"), io.Discard); err == nil {
		t.Fatal("accepted absent file")
	}
	if err := pipelineEquivalence(context.Background(), strings.NewReader("bad")); err == nil {
		t.Fatal("accepted malformed journal")
	}
	if err := doRerun(context.Background(), goldenJournal, rerunFailedWriter{}); err == nil || !strings.Contains(err.Error(), "report the pipeline rerun") {
		t.Fatalf("writer failure: %v", err)
	}
	// Reads preserve the original evidence and never create outputs.
	before, err := os.ReadFile(goldenJournal)
	if err != nil {
		t.Fatal(err)
	}
	if err := doRerun(context.Background(), goldenJournal, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(goldenJournal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rerun changed recorded evidence")
	}
}

type rerunFailedWriter struct{}

func (rerunFailedWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("output closed") }

// TestRerunComparesTheGeneratedHeaderBeforeItsRecords includes the format
// metadata ADR 0017 seeds into the chain, independent of identity and span.
func TestRerunComparesTheGeneratedHeaderBeforeItsRecords(t *testing.T) {
	header, records := readJournalFile(t, goldenJournal)
	header.ChainAlgorithm = "changed algorithm"
	err := pipelineEquivalence(context.Background(), bytes.NewReader(encodeRerunEvidence(t, header, records)))
	if err == nil || !strings.Contains(err.Error(), "pipeline divergence in journal header") {
		t.Fatalf("want header divergence first, got %v", err)
	}
}

// TestRerunNormalizesEnvelopeTimeZones covers ADR 0016 canonical comparison
// without normalizing the payload bytes that the envelope hash attests.
func TestRerunNormalizesEnvelopeTimeZones(t *testing.T) {
	header, records := readJournalFile(t, goldenJournal)
	zone := time.FixedZone("fixture-offset", -5*60*60)
	header.SpanStart, header.SpanEnd = header.SpanStart.In(zone), header.SpanEnd.In(zone)
	for i := range records {
		e := &records[i].Envelope
		e.EventTime, e.RecordedAt = e.EventTime.In(zone), e.RecordedAt.In(zone)
	}
	if err := pipelineEquivalence(context.Background(), bytes.NewReader(encodeRerunEvidence(t, header, records))); err != nil {
		t.Fatal(err)
	}
}

// TestRerunReportsTheFirstMissingRecordWhenThePipelineStops checks that a
// new simulator failure cannot hide the changed output prefix (ADR 0017).
func TestRerunReportsTheFirstMissingRecordWhenThePipelineStops(t *testing.T) {
	original := runSession
	t.Cleanup(func() { runSession = original })
	runSession = func(context.Context, *fills.Simulator, replay.Handler, []event.Envelope) (fills.Result, error) {
		return fills.Result{}, fmt.Errorf("simulator stopped")
	}
	err := doRerun(context.Background(), goldenJournal, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "pipeline divergence at record 2") || !strings.Contains(err.Error(), "regenerated nothing") || !strings.Contains(err.Error(), "simulator stopped") {
		t.Fatalf("first missing record: %v", err)
	}
}

// TestRerunStopsWhenItsInvocationIsCancelled pins that -rerun honours the
// command's own interrupt handling: main cancels the invocation context on
// SIGINT or SIGTERM, and a re-run over a long journal must stop on it rather
// than absorb the signal and keep regenerating fills.
func TestRerunStopsWhenItsInvocationIsCancelled(t *testing.T) {
	journalBytes, err := os.ReadFile(goldenJournal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = pipelineEquivalence(ctx, bytes.NewReader(journalBytes))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pipelineEquivalence(cancelled) error = %v, want it to wrap context.Canceled", err)
	}
	// The golden journal is valid, so a cancelled re-run of it must not
	// report the partial output as a divergence.
	if strings.Contains(err.Error(), "divergence") {
		t.Fatalf("pipelineEquivalence(cancelled) error = %v, want no divergence reported for a valid journal", err)
	}
}

// TestRerunStillReportsAPipelineFailureThatCoincidesWithCancellation pins the
// other side of the cancellation rule: only a failure that IS the
// cancellation is reported as a stopped re-run. A simulator failure that
// happens while the context is also cancelled is still the audit's finding.
func TestRerunStillReportsAPipelineFailureThatCoincidesWithCancellation(t *testing.T) {
	journalBytes, err := os.ReadFile(goldenJournal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failure := errors.New("simulator failed independently")
	original := runSession
	t.Cleanup(func() { runSession = original })
	runSession = func(context.Context, *fills.Simulator, replay.Handler, []event.Envelope) (fills.Result, error) {
		cancel()
		return fills.Result{}, failure
	}
	err = pipelineEquivalence(ctx, bytes.NewReader(journalBytes))
	if !errors.Is(err, failure) {
		t.Fatalf("pipelineEquivalence error = %v, want it to report the independent simulator failure", err)
	}
	if strings.Contains(err.Error(), "rerun stopped before completing") {
		t.Fatalf("pipelineEquivalence error = %v, want the failure reported, not a cancellation", err)
	}
}

// firstOfType is the index of the first envelope of eventType in es.
func firstOfType(t *testing.T, es []event.Envelope, eventType string) int {
	t.Helper()
	for i, e := range es {
		if e.Type == eventType {
			return i
		}
	}
	t.Fatalf("no %s input", eventType)
	return -1
}

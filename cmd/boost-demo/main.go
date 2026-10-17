// Command boost-demo runs Boost end to end over an invented SPY series: bars,
// account snapshots and marks through the target-weight handler, every
// decision journalled, the journal written, read back and replayed, and the
// decision log printed. The series is synthetic, never market data. It is
// composition only: every rule lives in internal/targetweight,
// internal/orderplan and internal/risklimit.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/buildinfo"
	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// configuration is the backtested Boost (plan.md section 2) for a dry-run
// joint account. MaxOrderFraction is 1: the first decision from cash buys 98%
// of net liquidation in one order.
func configuration() event.TargetWeightConfigurationPayload {
	return event.TargetWeightConfigurationPayload{
		StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR,
		Boost: event.BoostConfiguration{
			InstrumentID: "SPY", InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 10,
			TrendDays: 200, ExitSMA: 5, WarmUpStart: "2024-01-02",
		},
		Limits: event.MarginLimits{MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 1, PriceBand: 0.05},
	}
}

// syntheticClose is an invented series: each 20-session cycle rises 0.4% a
// session for 17 sessions, falls 2% twice, and recovers 3%.
func syntheticCloses(n int) []float64 {
	closes := make([]float64, n)
	price := 400.0
	for i := range closes {
		closes[i] = price
		factor := 1.004
		switch i % 20 {
		case 17, 18:
			factor = 0.98
		case 19:
			factor = 1.03
		}
		price = float64(price * factor)
	}
	return closes
}

// sessions returns n weekday session closes from 2024-01-02, 21:00 UTC.
func sessions(n int) []time.Time {
	out := make([]time.Time, 0, n)
	for day := time.Date(2024, time.January, 2, 21, 0, 0, 0, time.UTC); len(out) < n; day = day.AddDate(0, 0, 1) {
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
			out = append(out, day)
		}
	}
	return out
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("boost-demo", flag.ContinueOnError)
	flags.SetOutput(out)
	count := flags.Int("sessions", 320, "number of synthetic sessions")
	outPath := flags.String("out", "", "path to write the run's journal to (optional)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *count < 1 {
		return errors.New("boost-demo: at least one session is required")
	}
	cfg := configuration()
	version := event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, buildinfo.Version)
	days := sessions(*count)
	opts := targetweight.Options{DecideFrom: days[0]}
	handler, err := targetweight.New(version, cfg, opts)
	if err != nil {
		return err
	}
	recorder := journal.NewRecorder(handler)
	feed := &feeder{hash: handler.ConfigurationHash(), version: version, recorder: recorder, account: account{cash: 4000}}
	if err := feed.apply(event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", days[0].Add(-time.Hour), cfg); err != nil {
		return err
	}
	for i, price := range syntheticCloses(*count) {
		if err := feed.session(i, days[i], price); err != nil {
			return err
		}
	}
	for _, line := range feed.log {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}

	header, err := recorder.Header(feed.hash, version)
	if err != nil {
		return err
	}
	var written bytes.Buffer
	if err := journal.Write(&written, header, recorder.Entries()); err != nil {
		return err
	}
	if *outPath != "" {
		if err := writeNew(*outPath, written.Bytes()); err != nil {
			return err
		}
	}
	return replayJournal(written.Bytes(), version, cfg, opts, out)
}

// writeNew writes data to a file that must not exist yet: the exclusive
// create refuses an occupied path, so a rerun never replaces an earlier run's
// journal (AGENTS.md rule 6). The file is synced before it is closed.
func writeNew(path string, data []byte) error {
	return writeNewWith(path, data, func(f *os.File, b []byte) error {
		_, err := f.Write(b)
		return err
	})
}

// writeNewWith is writeNew with the write step injectable, so a test can force
// a failure after the file exists.
func writeNewWith(path string, data []byte, write func(*os.File, []byte) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	// The path was created by this call, so removing it on failure never
	// touches an earlier journal.
	if err := write(f, data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// replayJournal reads the journal back, verifies its chain and identity, and
// replays its inputs through a fresh handler against its recorded decisions.
func replayJournal(raw []byte, version string, cfg event.TargetWeightConfigurationPayload, opts targetweight.Options, out io.Writer) error {
	if _, err := journal.Verify(bytes.NewReader(raw)); err != nil {
		return err
	}
	header, records, err := journal.Read(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := journal.CheckIdentity(header, records); err != nil {
		return err
	}
	inputs, decisions, err := journal.Split(records)
	if err != nil {
		return err
	}
	handler, err := targetweight.New(version, cfg, opts)
	if err != nil {
		return err
	}
	engine, err := replay.New(handler)
	if err != nil {
		return err
	}
	replayed, err := engine.Run(context.Background(), inputs)
	if err != nil {
		return err
	}
	if d := replay.Equivalent(decisions, replayed); d != nil {
		return fmt.Errorf("boost-demo: replay diverged: %+v", d)
	}
	_, err = fmt.Fprintf(out, "replayed: journal decisions match (%d inputs, %d decisions)\n", len(inputs), len(decisions))
	return err
}

// account is the simulated joint account: a released order fills at the next
// session's close.
type account struct {
	cash    float64
	held    int64
	pending []event.OrderReleasedPayload
}

type feeder struct {
	hash     string
	version  string
	sequence uint64
	recorder *journal.Recorder
	account  account
	log      []string
	emitted  []event.Envelope
}

func (f *feeder) apply(eventType string, version uint32, id string, at time.Time, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	f.sequence++
	in := event.Envelope{
		ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: version,
		EventTime: at, RecordedAt: at, Sequence: f.sequence, Source: "boost-demo",
		StrategyVersion: f.version, ConfigurationHash: f.hash, PayloadHash: event.HashPayload(raw), Payload: raw,
	}
	out, err := f.recorder.Apply(context.Background(), in)
	f.emitted = out
	return err
}

func (f *feeder) session(i int, at time.Time, price float64) error {
	a := &f.account
	for _, o := range a.pending {
		quantity := o.Quantity
		if o.Side == event.OrderSideSell {
			quantity = -quantity
		}
		a.held += quantity
		a.cash -= float64(float64(quantity) * price)
	}
	a.pending = nil
	view := func(label string) event.PriceView {
		return event.PriceView{View: label, Open: price, High: price, Low: price, Close: price, Volume: 1e6}
	}
	snapshot := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: at, Currency: "USD", NetLiquidation: a.cash + float64(float64(a.held)*price)}
	if a.held != 0 {
		snapshot.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: a.held}}
	}
	steps := []struct {
		eventType string
		version   uint32
		id        string
		payload   any
	}{
		{event.CompletedBarEventType, event.CompletedBarSchemaVersion, fmt.Sprintf("bar:%d", i),
			event.CompletedBarPayload{InstrumentID: "SPY", PeriodEnd: at, SplitAdjusted: view(event.ViewSplitAdjusted), Raw: view(event.ViewRaw)}},
		{event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, fmt.Sprintf("snapshot:%d", i), snapshot},
		{event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, fmt.Sprintf("mark:%d", i),
			event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: price, Source: event.AdapterIBKR}},
		{event.SessionClosedEventType, event.SessionClosedSchemaVersion, fmt.Sprintf("session:%d", i),
			event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}},
	}
	for _, s := range steps {
		if err := f.apply(s.eventType, s.version, s.id, at, s.payload); err != nil {
			return err
		}
	}
	return f.describe(at)
}

// describe renders the session's emissions as one decision-log line.
func (f *feeder) describe(at time.Time) error {
	line := at.Format("2006-01-02")
	for _, e := range f.emitted {
		switch e.Type {
		case event.TargetsDecidedEventType:
			var p event.TargetsDecidedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			b := p.Boost
			line += fmt.Sprintf(" %s close %.2f rsi2 %s sma200 %s sma5 %s latched %v->%v", p.Outcome, b.Close,
				reading(b.RSI, b.RSIReady), reading(b.TrendMean, b.TrendMeanReady), reading(b.ExitMean, b.ExitMeanReady), b.LatchedBefore, b.LatchedAfter)
			if len(p.Targets) == 1 {
				line += fmt.Sprintf(" target %.2f", p.Targets[0].Weight)
			}
		case event.OrderPlannedEventType:
			var p event.OrderPlannedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			line += fmt.Sprintf(" | %s %d @ %.2f", p.Side, p.Quantity, p.LimitPrice)
		case event.OrderReleasedEventType:
			var p event.OrderReleasedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			line += fmt.Sprintf(" released (send %v)", p.Send)
			f.account.pending = append(f.account.pending, p)
		case event.OrderRefusedEventType:
			var p event.OrderRefusedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			line += " refused: " + p.Reason
		case event.EngineStateEventType:
			line += " | engine state changed"
		}
	}
	f.log = append(f.log, line)
	return nil
}

func reading(v float64, ready bool) string {
	if !ready {
		return "-"
	}
	return fmt.Sprintf("%.2f", v)
}

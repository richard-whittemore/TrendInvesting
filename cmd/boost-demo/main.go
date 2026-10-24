// Command boost-demo plays the broker adapter for a dry-run Boost engine: it
// connects to cmd/engine's socket and sends an invented SPY series, session
// by session (the bar, the account snapshot, the mark, the session close),
// with a simulated account, and prints the decision log from the engine's
// replies. A dry-run sends nothing, so nothing fills: the account holds what
// it started with, 14 SPY bought on margin at the first close (net
// liquidation $4,000), and every order is sized from that holding. Each
// released order is followed by the engine's synthetic cancel of it, since
// it was never sent (ADR 0026, point 3, "Mode", amended 2026-10-17 and 2026-10-22).
// The series is synthetic, never market data. It composes no strategy: every
// decision is the engine's, so there is one composition root, cmd/engine.
//
//	boost-demo -socket <engine.sock> -log <the engine's durable log> -config <boost.config> [-sessions 320] [-first-sequence 2]
//
// The run's identity (configuration hash and strategy version) is read from
// the engine's durable log header, so the envelopes carry the build the
// engine was stamped with. `make demo-boost-dry-run` runs it against the
// real engine and then replays the durable log byte for byte.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
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

// sessions returns n weekday session closes from 2025-01-02, 21:00 UTC.
func sessions(n int) []time.Time {
	out := make([]time.Time, 0, n)
	for day := time.Date(2025, time.January, 2, 21, 0, 0, 0, time.UTC); len(out) < n; day = day.AddDate(0, 0, 1) {
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
			out = append(out, day)
		}
	}
	return out
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("boost-demo", flag.ContinueOnError)
	flags.SetOutput(out)
	socket := flags.String("socket", "", "the engine's socket")
	logPath := flags.String("log", "", "the engine's durable log, whose header names the run's identity")
	configPath := flags.String("config", "", "the Boost configuration the engine was started with")
	count := flags.Int("sessions", 320, "number of synthetic sessions")
	first := flags.Uint64("first-sequence", 2, "the sequence of the first input: 2 after the configuration, 3 after an approval")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *count < 1 {
		return errors.New("boost-demo: at least one session is required")
	}
	if *socket == "" || *logPath == "" || *configPath == "" {
		return errors.New("boost-demo: -socket, -log and -config are required")
	}
	header, err := readLogHeader(*logPath)
	if err != nil {
		return err
	}
	cfg, err := readConfiguration(*configPath)
	if err != nil {
		return err
	}
	client, err := transport.Dial(*socket)
	if err != nil {
		return fmt.Errorf("boost-demo: %w", err)
	}
	defer func() { _ = client.Close() }()
	feed := &feeder{
		client: client, hash: header.ConfigurationHash, version: header.StrategyVersion, sequence: *first,
		accountID: cfg.AccountID, instrument: cfg.Boost.InstrumentID, account: startingAccount(),
	}
	days := sessions(*count)
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
	return nil
}

// readLogHeader reads the durable log's first line (ADR 0027).
func readLogHeader(path string) (journal.LogHeader, error) {
	file, err := os.Open(path)
	if err != nil {
		return journal.LogHeader{}, fmt.Errorf("boost-demo: open the engine's durable log: %w", err)
	}
	defer func() { _ = file.Close() }()
	line, err := bufio.NewReader(file).ReadBytes('\n')
	if err != nil {
		return journal.LogHeader{}, fmt.Errorf("boost-demo: read the durable log header: %w", err)
	}
	var header journal.LogHeader
	if err := json.Unmarshal(line, &header); err != nil {
		return journal.LogHeader{}, fmt.Errorf("boost-demo: decode the durable log header: %w", err)
	}
	return header, nil
}

func readConfiguration(path string) (event.TargetWeightConfigurationPayload, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return event.TargetWeightConfigurationPayload{}, fmt.Errorf("boost-demo: %w", err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return event.TargetWeightConfigurationPayload{}, fmt.Errorf("boost-demo: decode %s: %w", path, err)
	}
	return cfg, cfg.Validate()
}

// account is the simulated joint account. No order a dry-run releases is
// sent, so none fills and the account never changes.
type account struct {
	cash float64
	held int64
}

// startingAccount holds 14 SPY bought on margin at the first close, $400:
// net liquidation $4,000, 140% invested, so the first decision, at 98%,
// sells, and every Boost exit has shares to sell.
func startingAccount() account {
	const held, price = 14, 400.0
	return account{cash: float64(4000 - float64(held*price)), held: held}
}

type feeder struct {
	client     *transport.Client
	hash       string
	version    string
	sequence   uint64
	accountID  string
	instrument string
	account    account
	log        []string
	emitted    []event.Envelope
}

// decisions is the engine's reply: the decisions one input caused, in
// emission order (cmd/engine's decisionsPayload).
type decisions struct {
	Decisions []event.Envelope `json:"decisions"`
}

func (f *feeder) send(eventType string, version uint32, id string, at time.Time, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	in := event.Envelope{
		ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: version,
		EventTime: at, RecordedAt: at, Sequence: f.sequence, Source: "boost-demo",
		StrategyVersion: f.version, ConfigurationHash: f.hash, PayloadHash: event.HashPayload(raw), Payload: raw,
	}
	f.sequence++
	reply, err := f.client.Decide(context.Background(), in)
	if err != nil {
		return fmt.Errorf("boost-demo: %s: %w", id, err)
	}
	var d decisions
	if err := json.Unmarshal(reply.Payload, &d); err != nil {
		return fmt.Errorf("boost-demo: decode the decisions for %s: %w", id, err)
	}
	f.emitted = append(f.emitted, d.Decisions...)
	return nil
}

func (f *feeder) session(i int, at time.Time, price float64) error {
	a := &f.account
	view := func(label string) event.PriceView {
		return event.PriceView{View: label, Open: price, High: price, Low: price, Close: price, Volume: 1e6}
	}
	snapshot := event.AccountPositionsSnapshotPayload{AccountID: f.accountID, AsOf: at, Currency: "USD", NetLiquidation: a.cash + float64(float64(a.held)*price)}
	if a.held != 0 {
		snapshot.Positions = []event.AccountPosition{{InstrumentID: f.instrument, Quantity: a.held}}
	}
	steps := []struct {
		eventType string
		version   uint32
		id        string
		payload   any
	}{
		{event.CompletedBarEventType, event.CompletedBarAdjustedVersion, fmt.Sprintf("bar:%d", i),
			event.CompletedBarAdjustedPayload{
				CompletedBarPayload: event.CompletedBarPayload{InstrumentID: f.instrument, PeriodEnd: at, SplitAdjusted: view(event.ViewSplitAdjusted), Raw: view(event.ViewRaw)},
				Adjusted:            view(event.ViewAdjusted), AdjustedBase: "2025-01-02",
			}},
		{event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, fmt.Sprintf("snapshot:%d", i), snapshot},
		{event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, fmt.Sprintf("mark:%d", i),
			event.MarkRecordedPayload{InstrumentID: f.instrument, AsOf: at, Price: price, Source: event.AdapterIBKR}},
		{event.SessionClosedEventType, event.SessionClosedSchemaVersion, fmt.Sprintf("session:%d", i),
			event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{f.instrument}}},
	}
	f.emitted = nil
	for _, s := range steps {
		if err := f.send(s.eventType, s.version, s.id, at, s.payload); err != nil {
			return err
		}
	}
	return f.describe(at)
}

// describe renders the session's decisions as one decision-log line.
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
		case event.OrderLifecycleEventType:
			var p event.LimitOrderLifecyclePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			if p.Synthetic {
				line += fmt.Sprintf(" %s (synthetic, never sent)", p.Status)
			}
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

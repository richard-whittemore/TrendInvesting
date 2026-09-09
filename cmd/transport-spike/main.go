// Command transport-spike runs the Go side of the LEAN transport spike
// (issue #26): a decision engine on a Unix-domain socket, and a Go client that
// measures round trips against it.
//
// It is a measurement tool, not part of the trading path. See
// docs/adr/0014-lean-go-transport.md and adapter/lean/spike/README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/transport"
	"github.com/richard-whittemore/TrendInvesting/transport/spike"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "transport-spike:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: transport-spike serve|bench [flags]")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], out)
	case "bench":
		return bench(args[1:], out)
	default:
		return fmt.Errorf("unknown command %q; want serve or bench", args[0])
	}
}

func serve(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	socket := flags.String("socket", "/tmp/trend-spike.sock", "Unix-domain socket path")
	maxFrame := flags.Int("max-frame", transport.DefaultMaxFrameBytes, "maximum frame size in bytes")
	timeout := flags.Duration("decision-timeout", 5*time.Second, "per-decision timeout; 0 disables")
	stall := flags.Duration("stall", 0, "delay every decision by this much, to model a slow engine")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse serve flags: %w", err)
	}

	decide := spike.Decider(time.Now)
	if *stall > 0 {
		decide = stalling(decide, *stall)
	}
	server, err := transport.Listen(*socket, decide, transport.ServerConfig{
		MaxFrameBytes:   *maxFrame,
		DecisionTimeout: *timeout,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "listening on %s (max-frame=%d decision-timeout=%s)\n", server.Path(), *maxFrame, *timeout); err != nil {
		return fmt.Errorf("report readiness: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.ServeContext(ctx)
}

func stalling(decide transport.Decider, delay time.Duration) transport.Decider {
	return func(ctx context.Context, bar event.Envelope) (event.Envelope, error) {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return event.Envelope{}, ctx.Err()
		}
		return decide(ctx, bar)
	}
}

func bench(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("bench", flag.ContinueOnError)
	socket := flags.String("socket", "/tmp/trend-spike.sock", "Unix-domain socket path")
	rounds := flags.Int("rounds", 500, "measured round trips")
	warmup := flags.Int("warmup", 50, "discarded round trips before measuring")
	universe := flags.Int("universe", 1000, "instruments per bar")
	label := flags.String("label", "go-client", "label for the result row")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse bench flags: %w", err)
	}

	client, err := transport.Dial(*socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	stats, err := spike.Run(context.Background(), client, *rounds, *universe, *warmup, time.Now)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "%s universe=%d %s\n", *label, *universe, stats); err != nil {
		return fmt.Errorf("report result: %w", err)
	}
	return nil
}

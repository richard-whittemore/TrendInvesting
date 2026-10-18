package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/buildinfo"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// main wires the run to an interrupt that cancels it rather than ending it,
// mirroring cmd/backtest's own main: SIGINT/SIGTERM reach the run as a
// cancelled context, never as process death, so the journal this run has
// accumulated is written on the way out (see engine.go's run, "decision 3")
// instead of being lost.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := parseAndRun(ctx, os.Args[1:], os.Stdout)
	// Released here rather than deferred: os.Exit below would skip a defer,
	// and the handler has to outlive the whole run, journal write included.
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// parseAndRun parses the invocation and performs it. It exists separately
// from main so the command is testable as a function rather than as a
// process (mirrors cmd/backtest's run).
func parseAndRun(ctx context.Context, args []string, out io.Writer) error {
	return parseAndRunWithBuild(ctx, args, out, buildinfo.Version)
}

// parseAndRunWithBuild is parseAndRun with the build identity given, so a
// test can run a parsed invocation as a stamped build would.
func parseAndRunWithBuild(ctx context.Context, args []string, out io.Writer, build string) error {
	flags := flag.NewFlagSet("engine", flag.ContinueOnError)
	flags.SetOutput(out)
	socketPath := flags.String("socket", "", "Unix-domain socket path to listen on (ADR 0014)")
	configPath := flags.String("config", "", "path to the JSON strategy configuration this run decides under")
	outPath := flags.String("out", "", "path to write the run's journal to when this process stops")
	logPath := flags.String("log", "", "path to stream this process's durable log to, each input flushed before its decisions are returned (ADR 0027); must not exist")
	resumePath := flags.String("resume", "", "path of a previous process's durable log to rebuild state from before listening; requires -log")
	approvalPath := flags.String("approval", "", "path of the owner's operator.run.approved file: required for a fresh run when the -log directory already holds an earlier run's durable log; never with -resume")
	operatorDir := flags.String("operator", "", "path of the operator inbox, where the operator tool records the owner's kill switch and rebalance clearances; never with -mode backtest")
	asOf := flags.String("as-of", "", "required nonzero run start as an RFC 3339 time; warm-up bars may precede it (ADR 0012)")
	mode := flags.String("mode", "", "required: what the adapter does with decisions: backtest, dry-run, paper or live; every mode but backtest requires -log (ADR 0027)")
	maxFrame := flags.Int("max-frame", transport.DefaultMaxFrameBytes, "maximum frame size in bytes")
	decisionTimeout := flags.Duration("decision-timeout", 5*time.Second, "per-decision timeout; 0 disables")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return run(ctx, options{
		socketPath:      *socketPath,
		configPath:      *configPath,
		outPath:         *outPath,
		logPath:         *logPath,
		resumePath:      *resumePath,
		approvalPath:    *approvalPath,
		operatorDir:     *operatorDir,
		asOf:            *asOf,
		mode:            *mode,
		build:           build,
		maxFrameBytes:   *maxFrame,
		decisionTimeout: *decisionTimeout,
	}, out)
}

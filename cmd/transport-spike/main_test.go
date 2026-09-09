package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/transport"
	"github.com/richard-whittemore/TrendInvesting/transport/spike"
)

func TestRunRejectsUnusableInvocations(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"no command":      {},
		"unknown command": {"explode"},
		"bad serve flag":  {"serve", "-nope"},
		"bad bench flag":  {"bench", "-nope"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := run(args, io.Discard); err == nil {
				t.Fatalf("run(%q) succeeded; want an error", args)
			}
		})
	}
}

func TestBenchReportsAnAbsentEngine(t *testing.T) {
	t.Parallel()
	err := run([]string{"bench", "-socket", filepath.Join(t.TempDir(), "absent.sock")}, io.Discard)
	if !errors.Is(err, transport.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestServeRefusesAnUnbindableSocket(t *testing.T) {
	t.Parallel()
	overlong := "/tmp/" + strings.Repeat("x", transport.MaxSocketPathBytes)
	if err := run([]string{"serve", "-socket", overlong}, io.Discard); err == nil {
		t.Fatal("expected serve to refuse an unbindable socket path")
	}
}

func TestBenchMeasuresRoundTripsAgainstAServer(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "cli-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")

	server, err := transport.Listen(socket, spike.Decider(time.Now), transport.ServerConfig{})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.Serve(); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		_ = server.Close()
		wg.Wait()
	})

	var out strings.Builder
	args := []string{
		"bench", "-socket", socket,
		"-rounds", "5", "-warmup", "1", "-universe", "10", "-label", "unit",
	}
	if err := run(args, &out); err != nil {
		t.Fatalf("bench: %v", err)
	}
	for _, want := range []string{"unit", "universe=10", "n=5", "median="} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not contain %q", out.String(), want)
		}
	}
}

func TestStallingDecisionRespectsCancellation(t *testing.T) {
	t.Parallel()
	slow := stalling(spike.Decider(time.Now), time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := slow(ctx, spike.NewBarEnvelope(1, 1, time.Now)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/buildinfo"
	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

const testAsOf = "2026-01-01T00:00:00Z"

// ADRs 0012 and 0017 require repeatable run evidence, including the header
// that seeds the chain. Wall-clock time must not enter the configuration input.
func TestAsOfMakesJournalDeterministic(t *testing.T) {
	for _, tc := range []struct {
		name string
		asOf string
		bars int
	}{
		{"no-bars", testAsOf, 0},
		{"before-first-bar", testAsOf, 2},
		{"at-first-bar", "2026-01-02T00:00:00Z", 2},
		{"warmup-before-start", "2026-01-03T00:00:00Z", 2},
		{"offset", "2026-01-01T01:00:00+01:00", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asOf, err := time.Parse(time.RFC3339, tc.asOf)
			if err != nil {
				t.Fatal(err)
			}
			cfg := testConfiguration(t)
			payload, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			hash := event.ConfigurationHash(cfg)
			version := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, buildinfo.Version)
			var previous journal.Verification
			var previousBytes []byte
			for attempt := range 2 {
				path := filepath.Join(t.TempDir(), "journal.jsonl")
				socket := filepath.Join(shortSocketDir(t), "engine.sock")
				stop := startEngineInvocation(t, func(ctx context.Context, out io.Writer) error {
					return parseAndRun(ctx, []string{"-socket", socket, "-config", testConfigPath,
						"-out", path, "-as-of", tc.asOf}, out)
				})
				if tc.bars > 0 {
					client, err := transport.Dial(socket)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = client.Close() })
					for day := range tc.bars {
						bar := flatBar("TEST", day)
						sequence := configurationSequence + 1 + 2*uint64(day)
						for _, input := range []event.Envelope{
							barEnvelope(t, bar, sequence, version, hash),
							sessionClosedEnvelope(t, bar, sequence+1, version, hash),
						} {
							if _, err := client.Decide(context.Background(), input); err != nil {
								t.Fatal(err)
							}
						}
					}
					if err := client.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if err := stop(); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				verified, err := journal.Verify(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				if attempt > 0 {
					if !reflect.DeepEqual(verified.Header, previous.Header) {
						t.Errorf("identical runs have different headers: %+v versus %+v", previous.Header, verified.Header)
					}
					if verified.FinalRecordHash != previous.FinalRecordHash {
						t.Errorf("identical runs have different final hashes: %s versus %s", previous.FinalRecordHash, verified.FinalRecordHash)
					}
					if !bytes.Equal(raw, previousBytes) {
						t.Error("identical runs have different journal bytes")
					}
				}
				previous, previousBytes = verified, raw
				header, records, err := journal.Read(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				if err := journal.CheckIdentity(header, records); err != nil {
					t.Fatal(err)
				}
				if err := journal.CheckSpan(header, records); err != nil {
					t.Fatal(err)
				}
				inputs, decisions, err := journal.Split(records)
				if err != nil {
					t.Fatal(err)
				}
				if len(inputs) != 1+2*tc.bars || len(decisions) != tc.bars {
					t.Fatalf("unexpected inputs/decisions: %d/%d", len(inputs), len(decisions))
				}
				configuration := inputs[0]
				if configuration.Type != event.ConfigurationEventType ||
					!configuration.EventTime.Equal(asOf) || !configuration.RecordedAt.Equal(asOf) {
					t.Errorf("configuration must carry declared start %s in both timestamps: %+v", asOf, configuration)
				}
				if header.ConfigurationHash != hash || !bytes.Equal(configuration.Payload, payload) {
					t.Error("declared start changed configuration identity or payload")
				}
				wantStart, wantEnd := asOf, asOf
				if tc.bars > 0 {
					if first := flatBar("TEST", 0).PeriodEnd; first.Before(wantStart) {
						wantStart = first
					}
					if last := flatBar("TEST", tc.bars-1).PeriodEnd; last.After(wantEnd) {
						wantEnd = last
					}
				}
				if !header.SpanStart.Equal(wantStart) || !header.SpanEnd.Equal(wantEnd) {
					t.Errorf("span = %s to %s, want %s to %s", header.SpanStart, header.SpanEnd, wantStart, wantEnd)
				}
				assertJournalReplays(t, header, inputs, decisions)
			}
		})
	}
}

// Startup must fail before socket readiness when the declared start cannot
// produce a nonzero journal span (ADRs 0014 and 0017).
func TestAsOfRefusesInvalidStartup(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing", nil, "-as-of is required"},
		{"invalid", []string{"-as-of", "not-a-time"}, "-as-of must be an RFC 3339 time"},
		{"date-only", []string{"-as-of", "2026-01-01"}, "-as-of must be an RFC 3339 time"},
		{"comma fraction", []string{"-as-of", "2026-01-01T00:00:00,5Z"}, "-as-of must be an RFC 3339 time"},
		{"lowercase separator", []string{"-as-of", "2026-01-01t00:00:00Z"}, "-as-of must be an RFC 3339 time"},
		{"zero", []string{"-as-of", "0001-01-01T00:00:00Z"}, "-as-of must be nonzero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := filepath.Join(shortSocketDir(t), "engine.sock")
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			args := append([]string{"-socket", socket, "-config", testConfigPath, "-out", path}, tc.args...)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var out bytes.Buffer
			err := parseAndRun(ctx, args, &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("startup error = %v, want %q", err, tc.want)
			}
			if strings.Contains(out.String(), "listening on") {
				t.Error("invalid start reached socket readiness")
			}
			for _, target := range []string{socket, path} {
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("invalid start created %s (stat: %v)", target, err)
				}
			}
		})
	}
}

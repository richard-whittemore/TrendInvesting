package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

func runWithCashFlags(t *testing.T, cashFlags ...string) ([]byte, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	args := append([]string{"-config", configurationFixture, "-bars", barsFixture, "-out", path}, cashFlags...)
	var log bytes.Buffer
	if err := run(context.Background(), args, &log); err != nil {
		t.Fatalf("run(%v): %v\n%s", args, err, log.String())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw, path
}

// ADR 0010's existing cash refusal must be reachable through a journalled
// command run, with its cash input preserved for ADR 0017 replay.
func TestAvailableCashReachesTheJournalAndDeclinesUnaffordableUnits(t *testing.T) {
	for _, cash := range []float64{1000, 0} {
		t.Run(strconv.FormatFloat(cash, 'f', -1, 64), func(t *testing.T) {
			flags := []string{"-available-cash", strconv.FormatFloat(cash, 'f', -1, 64)}
			raw, path := runWithCashFlags(t, flags...)
			_, records, err := journal.Read(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			snapshots, declines := 0, 0
			for _, record := range records {
				switch record.Envelope.Type {
				case event.AccountSnapshotEventType:
					snapshots++
					var snapshot event.AccountSnapshotPayload
					if err := json.Unmarshal(record.Envelope.Payload, &snapshot); err != nil {
						t.Fatal(err)
					}
					if snapshot.AvailableCash != cash || snapshot.Equity != fixtureConfiguration(t).NotionalAccount.StartingEquity {
						t.Fatalf("snapshot = %+v, want available cash %v and unchanged equity", snapshot, cash)
					}
				case event.ProposalDeclinedEventType:
					var decline event.ProposalDeclinedPayload
					if err := json.Unmarshal(record.Envelope.Payload, &decline); err != nil {
						t.Fatal(err)
					}
					if decline.Reason == event.DeclineReasonInsufficientCash {
						declines++
						if decline.AvailableCash != cash || decline.RequiredCash <= cash {
							t.Fatalf("cash decline = %+v, want available %v below required cash", decline, cash)
						}
					}
				case event.FillEventType:
					t.Fatal("a cash-refused fixture must produce no fill")
				}
			}
			if snapshots != 1 || declines == 0 {
				t.Fatalf("got %d snapshots and %d insufficient-cash declines; want one snapshot and at least one decline", snapshots, declines)
			}
			if divergence, err := replayJournalFile(t, path); err != nil || divergence != nil {
				t.Fatalf("cash-constrained journal replay: divergence=%+v error=%v", divergence, err)
			}
			second, _ := runWithCashFlags(t, flags...)
			if !bytes.Equal(raw, second) {
				t.Fatal("repeating the same cash-constrained run changed the journal")
			}
		})
	}
}

func TestAvailableCashEqualToStartingEquityPreservesTheDefaultJournal(t *testing.T) {
	defaultRaw, _ := runWithCashFlags(t)
	cash := strconv.FormatFloat(fixtureConfiguration(t).NotionalAccount.StartingEquity, 'f', -1, 64)
	explicitRaw, _ := runWithCashFlags(t, "-available-cash", cash)
	if !bytes.Equal(defaultRaw, explicitRaw) {
		t.Fatal("explicit starting equity as available cash changed the default journal")
	}
}

func TestAvailableCashRejectsInvalidFigures(t *testing.T) {
	for _, cash := range []string{"-1", "NaN", "+Inf", "-Inf", "1e999", "dollars", ""} {
		t.Run(cash, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			var log bytes.Buffer
			err := run(context.Background(), []string{"-config", configurationFixture, "-bars", barsFixture, "-out", path, "-available-cash=" + cash}, &log)
			if err == nil || !strings.Contains(err.Error(), "-available-cash") || strings.Contains(err.Error(), "flag provided but not defined") {
				t.Fatalf("invalid cash %q: error=%v, want a cash-input validation failure", cash, err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid cash left a journal: %v", err)
			}
		})
	}
}

func TestAvailableCashCannotBeIgnoredByAuditOperations(t *testing.T) {
	for _, args := range [][]string{
		{"-verify", goldenJournal},
		{"-replay", goldenJournal},
		{"-decisions", goldenJournal},
		{"-diff-want", goldenJournal, "-diff-got", goldenJournal},
		{"-runs", "configuration-hash", "-registry", t.TempDir()},
	} {
		for _, cash := range []string{"0", ""} {
			t.Run(args[0]+"/"+cash, func(t *testing.T) {
				var log bytes.Buffer
				err := run(context.Background(), append(args, "-available-cash="+cash), &log)
				if err == nil || !strings.Contains(err.Error(), "would ignore -available-cash") {
					t.Fatalf("error=%v, want an explicit run/audit flag conflict", err)
				}
			})
		}
	}
}

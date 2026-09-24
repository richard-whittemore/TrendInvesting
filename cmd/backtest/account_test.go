package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// The command's account: internal/fills keeps it and states each Session's
// close, so exit proceeds return to the cash the next Session's Units are
// checked against (ADR 0010: exits in bar t free capital for bar t+1; ADR
// 0020: credits wait for the next previous close).

// runJournal runs opts and returns the records of the journal it wrote.
func runJournal(t *testing.T, opts options) []journal.Record {
	t.Helper()
	opts.outPath = filepath.Join(t.TempDir(), "journal.jsonl")
	opts.build = testBuild
	var log bytes.Buffer
	if err := backtest(context.Background(), opts, &log); err != nil {
		t.Fatalf("backtest(%+v) error = %v\n%s", opts, err, log.String())
	}
	raw, err := os.ReadFile(opts.outPath)
	if err != nil {
		t.Fatal(err)
	}
	_, records, err := journal.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func decodeRecord(t *testing.T, record journal.Record, into any) {
	t.Helper()
	if err := json.Unmarshal(record.Envelope.Payload, into); err != nil {
		t.Fatalf("decode %s: %v", record.Envelope.Type, err)
	}
}

// fillCost is what a fill moves the account's cash by: a buy's cost with its
// commission, negative, or a sell's proceeds less its commission (ADR 0013).
func fillCost(fill event.FillPayload) float64 {
	value := float64(float64(fill.Quantity) * fill.Price)
	if fill.Kind == event.FillKindEntry || fill.Kind == event.FillKindAdd {
		return -(value + fill.Commission)
	}
	return value - fill.Commission
}

// TestTheSecondCampaignIsFundedByTheFirstCampaignsExitProceeds: under the
// declared narrow-stop Variant, at the default account (1,000,000 of cash,
// the starting equity), the golden bars open a Campaign on 2026-01-22 that
// its 0.1N stop closes the same day, and signal again on 2026-01-23. The
// second entry costs more than the cash the first entry left, so only the
// first Campaign's exit proceeds, returned by the snapshot of the 2026-01-22
// close, can fund it. Falsified by leaving the sell out of the account's
// ledger (the entry is declined for insufficient cash), and by a single
// opening snapshot (the same).
func TestTheSecondCampaignIsFundedByTheFirstCampaignsExitProceeds(t *testing.T) {
	records := runJournal(t, options{
		configPath: filepath.Join("testdata", "variants", narrowStopVariant, "configuration.json"),
		barsPath:   barsFixture,
	})
	opening := fixtureConfiguration(t).NotionalAccount.StartingEquity

	var fillsSoFar []event.FillPayload
	var basis event.AccountSnapshotPayload
	var campaigns []event.CampaignOpenedPayload
	var checked bool
	for _, record := range records {
		switch record.Envelope.Type {
		case event.FillEventType:
			var fill event.FillPayload
			decodeRecord(t, record, &fill)
			fillsSoFar = append(fillsSoFar, fill)
		case event.AccountSnapshotEventType:
			decodeRecord(t, record, &basis)
		case event.ProposalDeclinedEventType:
			var decline event.ProposalDeclinedPayload
			decodeRecord(t, record, &decline)
			if decline.Kind == event.ProposalDeclinedKindEntry {
				t.Fatalf("entry declined: %+v", decline)
			}
		case event.TradeProposalEventType:
			if len(campaigns) != 1 || checked {
				continue
			}
			// The second entry, proposed at the 2026-01-23 close against
			// the snapshot of the 2026-01-22 close: the first Campaign has
			// bought and sold, and that snapshot states both.
			var proposal event.TradeProposalPayload
			decodeRecord(t, record, &proposal)
			first := campaigns[0].CampaignID
			ledger, leftByEntry := opening, opening
			var sold bool
			for _, fill := range fillsSoFar {
				if fill.FilledAt.After(basis.AsOf) {
					t.Fatalf("fill %s postdates the basis %s", fill.FillID, basis.AsOf)
				}
				ledger += fillCost(fill)
				if fill.Kind == event.FillKindEntry {
					leftByEntry += fillCost(fill)
				}
				sold = sold || fill.CampaignID == first
			}
			cost := float64(proposal.Quantity) * proposal.EntryLevel
			if !sold || math.Abs(basis.AvailableCash-ledger) >= 0.005 {
				t.Fatalf("the basis of the second entry states %.6f, want the first Campaign's buy and sell, %.6f", basis.AvailableCash, ledger)
			}
			if cost <= leftByEntry || cost > basis.AvailableCash {
				t.Fatalf("the second entry costs %v: the fixture must make the exit proceeds decisive (%v without them, %v with them)", cost, leftByEntry, basis.AvailableCash)
			}
			checked = true
		case event.CampaignOpenedEventType:
			var opened event.CampaignOpenedPayload
			decodeRecord(t, record, &opened)
			campaigns = append(campaigns, opened)
		}
	}
	if !checked || len(campaigns) < 2 {
		t.Fatalf("%d Campaign(s) opened, want a second one funded by the first one's exit", len(campaigns))
	}
}

// TestTheCommandStatesEverySessionsClose: one account.snapshot per Session,
// as of its close, stated by the simulator after that Session's own fills
// and before the next Session's bar, and the last one before the end of the
// stream (ADR 0021, as amended).
func TestTheCommandStatesEverySessionsClose(t *testing.T) {
	records := runJournal(t, options{configPath: configurationFixture, barsPath: barsFixture})
	bars, err := readBars(barsFixture)
	if err != nil {
		t.Fatal(err)
	}
	var stated []time.Time
	for i, record := range records {
		if record.Envelope.Type != event.AccountSnapshotEventType {
			continue
		}
		var snapshot event.AccountSnapshotPayload
		decodeRecord(t, record, &snapshot)
		if record.Envelope.Source != fills.Source || !record.Envelope.EventTime.Equal(snapshot.AsOf) {
			t.Fatalf("snapshot %+v from %q at %s, want the simulator's, at its own as-of", snapshot, record.Envelope.Source, record.Envelope.EventTime)
		}
		// What follows the statement is the next Session's bar, or the end
		// of the stream.
		next := records[i+1].Envelope
		switch {
		case next.Type == event.CompletedBarEventType && next.EventTime.After(snapshot.AsOf):
		case next.Type == event.RunCompletedEventType:
		default:
			t.Fatalf("the snapshot of %s is followed by %s %s, want the next Session's bar or the end of the stream", snapshot.AsOf, next.Type, next.ID)
		}
		stated = append(stated, snapshot.AsOf)
	}
	if len(stated) != len(bars) {
		t.Fatalf("%d snapshots over %d Sessions, want one per Session", len(stated), len(bars))
	}
	for i, bar := range bars {
		if !stated[i].Equal(bar.PeriodEnd) {
			t.Fatalf("snapshot %d is as of %s, want the Session's close %s", i+1, stated[i], bar.PeriodEnd)
		}
	}
}

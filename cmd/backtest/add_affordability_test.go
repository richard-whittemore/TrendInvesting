package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// ADR 0020: the cash available to a Unit is the previous close's figure less
// what fills since that close have already spent, and a Unit that cannot be
// funded is declined with insufficient-cash (ADR 0010: no borrowing). The
// command's own fixture opens with $1,000,000 and sizes each Unit at about
// $640,000, so Unit 1 fits and Unit 2 does not once Unit 1 has filled.
//
// Each Session's snapshot states that close's balance, which already
// reflects every fill up to it, so a fill is debited until the snapshot of
// its own close arrives and never after (ADR 0020: "A fill debits the ledger
// exactly once"). Every decline therefore carries exactly the latest
// snapshot's cash less the fills stamped after it: on 2026-01-22 the entry's
// fill, and on every later Session nothing. Falsified by a snapshot that does
// not drop the debits it reflects (the later declines would carry the entry
// twice), and by a single opening snapshot (they would carry it once, from a
// basis that never moves).
func TestTheBacktestNeverBuysMoreThanItsCashAndDeclinesTheAddItCannotFund(t *testing.T) {
	raw, _ := runWithCashFlags(t)
	_, records, err := journal.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	opening := fixtureConfiguration(t).NotionalAccount.StartingEquity
	var basis event.AccountSnapshotPayload
	var spent float64
	var sinceBasis []event.FillPayload
	addDeclines, laterDeclines := 0, 0
	// A declined Unit is skipped before any proposal is built (ADR 0010), so
	// no Add is proposed for the Campaign and bar a decline answers.
	type rung struct {
		campaignID string
		periodEnd  string
	}
	proposed, declined := map[rung]bool{}, map[rung]bool{}
	for _, record := range records {
		switch record.Envelope.Type {
		case event.AddProposalEventType:
			var add event.AddProposalPayload
			if err := json.Unmarshal(record.Envelope.Payload, &add); err != nil {
				t.Fatal(err)
			}
			proposed[rung{add.CampaignID, add.PeriodEnd.UTC().String()}] = true
		case event.AccountSnapshotEventType:
			if err := json.Unmarshal(record.Envelope.Payload, &basis); err != nil {
				t.Fatal(err)
			}
			var kept []event.FillPayload
			for _, fill := range sinceBasis {
				if fill.FilledAt.After(basis.AsOf) {
					kept = append(kept, fill)
				}
			}
			sinceBasis = kept
		case event.FillEventType:
			var fill event.FillPayload
			if err := json.Unmarshal(record.Envelope.Payload, &fill); err != nil {
				t.Fatal(err)
			}
			if fill.Kind == event.FillKindEntry || fill.Kind == event.FillKindAdd {
				spent += float64(float64(fill.Quantity)*fill.Price) + fill.Commission
				sinceBasis = append(sinceBasis, fill)
			}
		case event.ProposalDeclinedEventType:
			var decline event.ProposalDeclinedPayload
			if err := json.Unmarshal(record.Envelope.Payload, &decline); err != nil {
				t.Fatal(err)
			}
			if decline.Kind == event.ProposalDeclinedKindAdd && decline.Reason == event.DeclineReasonInsufficientCash {
				addDeclines++
				declined[rung{decline.CampaignID, decline.PeriodEnd.UTC().String()}] = true
				want := basis.AvailableCash
				for _, fill := range sinceBasis {
					want -= float64(float64(fill.Quantity)*fill.Price) + fill.Commission
				}
				if decline.AvailableCash != want || decline.AvailableCash >= decline.RequiredCash {
					t.Errorf("decline = %+v, want the snapshot's %v less the fills since it, %v, below the Unit's cost", decline, basis.AvailableCash, want)
				}
				if len(sinceBasis) == 0 {
					laterDeclines++
				}
			}
		}
	}
	if spent == 0 || spent > opening {
		t.Errorf("buy fills cost %v against opening cash %v; want at least one fill and none unfunded", spent, opening)
	}
	if addDeclines == 0 || laterDeclines == 0 {
		t.Errorf("%d Add(s) declined for insufficient cash, %d of them against a snapshot that reflects Unit 1; want both", addDeclines, laterDeclines)
	}
	for r := range declined {
		if proposed[r] {
			t.Errorf("Campaign %q was both declined for insufficient cash and proposed an Add on the bar ending %s", r.campaignID, r.periodEnd)
		}
	}
}

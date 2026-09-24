package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// ADR 0020: the cash available to a Unit is the previous close's figure less
// what earlier fills have already spent, and a Unit that cannot be funded is
// declined with insufficient-cash (ADR 0010: no borrowing). The command's own
// fixture opens with $1,000,000 and sizes each Unit at about $640,000, so
// Unit 1 fits and Unit 2 does not once Unit 1 has filled. Every buy the run
// fills must be funded by the opening snapshot, which is the only cash
// figure this command states.
func TestTheBacktestNeverBuysMoreThanItsCashAndDeclinesTheAddItCannotFund(t *testing.T) {
	raw, _ := runWithCashFlags(t)
	_, records, err := journal.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var cash, spent float64
	addDeclines := 0
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
			var snapshot event.AccountSnapshotPayload
			if err := json.Unmarshal(record.Envelope.Payload, &snapshot); err != nil {
				t.Fatal(err)
			}
			cash = snapshot.AvailableCash
		case event.FillEventType:
			var fill event.FillPayload
			if err := json.Unmarshal(record.Envelope.Payload, &fill); err != nil {
				t.Fatal(err)
			}
			if fill.Kind == event.FillKindEntry || fill.Kind == event.FillKindAdd {
				spent += float64(float64(fill.Quantity)*fill.Price) + fill.Commission
			}
		case event.ProposalDeclinedEventType:
			var decline event.ProposalDeclinedPayload
			if err := json.Unmarshal(record.Envelope.Payload, &decline); err != nil {
				t.Fatal(err)
			}
			if decline.Kind == event.ProposalDeclinedKindAdd && decline.Reason == event.DeclineReasonInsufficientCash {
				addDeclines++
				declined[rung{decline.CampaignID, decline.PeriodEnd.UTC().String()}] = true
				if decline.AvailableCash >= decline.RequiredCash || decline.AvailableCash >= cash {
					t.Errorf("decline = %+v, want the balance left after earlier fills, below both the Unit's cost and the opening %v", decline, cash)
				}
			}
		}
	}
	if spent == 0 || spent > cash {
		t.Errorf("buy fills cost %v against opening cash %v; want at least one fill and none unfunded", spent, cash)
	}
	if addDeclines == 0 {
		t.Error("no Add was declined for insufficient cash; want Unit 2 declined once Unit 1 has spent the cash")
	}
	for r := range declined {
		if proposed[r] {
			t.Errorf("Campaign %q was both declined for insufficient cash and proposed an Add on the bar ending %s", r.campaignID, r.periodEnd)
		}
	}
}

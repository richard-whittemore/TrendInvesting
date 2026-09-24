package strategy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// ADR 0020's cash-movement amendment constrains spendable cash without
// advancing the snapshot timestamp. These are synthetic boundary fixtures,
// not source-derived strategy goldens; ADR 0007 still scales the Unit size.
func TestCashMovementSpendable(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cash         float64
		amounts      []float64
		wantCash     float64
		wantProposal bool
	}{
		{"withdrawal-during-decision-bar", 21_000, []float64{-20_000}, 1_000, false},
		{"deposit-during-decision-bar", 1_000, []float64{30_000}, 1_000, false},
		{"cumulative-withdrawals", 21_000, []float64{-10_000, -10_000}, 1_000, false},
		{"deposit-does-not-offset-withdrawal", 21_000, []float64{-20_000, 30_000}, 1_000, false},
		{"withdrawal-exhausts-cash", 21_000, []float64{-21_000}, 0, false},
		{"withdrawal-exceeds-cash", 21_000, []float64{-30_000}, 0, false},
		{"affordable-withdrawal-does-not-invalidate-basis", 30_000, []float64{-100}, 29_900, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfigurationPayload()
			bars := breakoutBars("AAPL")
			s := newStream(t, cfg).snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), tc.cash)).bars(bars[:len(bars)-1])
			equity := cfg.NotionalAccount.StartingEquity
			for i, amount := range tc.amounts {
				s.movement(cashMovementPayload(day(55).Add(time.Duration(i+1)*time.Hour), amount, equity))
				equity += amount
			}
			s.bar(bars[len(bars)-1])
			emitted := s.mustRun()
			proposals := envelopesOfType(emitted, event.TradeProposalEventType)
			declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
			if tc.wantProposal {
				if len(proposals) != 1 || len(declines) != 0 {
					t.Fatalf("proposals=%d declines=%d, want 1 and 0", len(proposals), len(declines))
				}
			} else {
				if len(proposals) != 0 || len(declines) != 1 {
					t.Fatalf("proposals=%d declines=%d, want 0 and 1", len(proposals), len(declines))
				}
				p := decodeProposalDeclined(t, declines[0])
				if p.Reason != event.DeclineReasonInsufficientCash || p.AvailableCash != tc.wantCash || p.RequiredCash <= p.AvailableCash {
					t.Fatalf("decline=%+v, want insufficient-cash with available=%v", p, tc.wantCash)
				}
				if declines[0].SchemaVersion != 4 {
					t.Fatalf("decline schema=%d, want 4 for spendable cash", declines[0].SchemaVersion)
				}
			}
			verifyMovementJournal(t, s, emitted)
		})
	}
}

func (s *stream) movement(p event.CashMovementPayload) *stream {
	s.seq++
	s.envelopes = append(s.envelopes, cashMovementEnvelopeFor(s.t, s.cfg, s.seq, p))
	return s
}

// verifyMovementJournal writes and verifies ADR 0017's hash chain, then
// replays the recorded inputs and compares canonical decision bytes.
func verifyMovementJournal(t *testing.T, s *stream, emitted []event.Envelope) {
	t.Helper()
	var entries []journal.Entry
	for _, input := range s.envelopes {
		entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: input})
		for _, decision := range emitted {
			if decision.CausationID == input.ID {
				entries = append(entries, journal.Entry{Kind: journal.KindDecision, Envelope: decision})
			}
		}
	}
	var buf bytes.Buffer
	first, last := s.envelopes[0], s.envelopes[len(s.envelopes)-1]
	if err := journal.Write(&buf, journal.NewHeader(first.ConfigurationHash, first.StrategyVersion, first.EventTime, last.EventTime), entries); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Verify(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	_, records, err := journal.Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var inputs, decisions []event.Envelope
	for _, record := range records {
		if record.Kind == journal.KindInput {
			inputs = append(inputs, record.Envelope)
		} else {
			decisions = append(decisions, record.Envelope)
		}
		if record.Envelope.Type == event.CashMovementEventType || record.Envelope.Type == event.ProposalDeclinedEventType {
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("journal: %s", raw)
		}
	}
	reducer, err := strategy.NewReducer(testStrategyVersion, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatal(err)
	}
	got, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if d := replay.Equivalent(emitted, decisions); d != nil {
		t.Fatalf("journal omitted decisions: %+v", d)
	}
	if d := replay.Equivalent(decisions, got); d != nil {
		t.Fatalf("replay divergence: %+v", d)
	}
	t.Log("journal chain verified; replay decisions byte-identical")
}

// TestASnapshotAfterAWithdrawalReplacesRatherThanDeductsAgain pins the one
// sentence of ADR 0020's cash-movement amendment that nothing else here
// checks: "A subsequent snapshot replaces the constrained figure outright;
// earlier withdrawals are already reflected in that balance and must not be
// deducted again."
//
// The failure it rules out is double-counting, which is invisible in every
// other case in this file because they all end with the withdrawal. Here a
// 20,000 withdrawal is followed by a snapshot of 30,000 that already reflects
// it. Deducting again would leave 10,000 — under the Unit's cost — and turn
// an affordable entry into an insufficient-cash decline.
func TestASnapshotAfterAWithdrawalReplacesRatherThanDeductsAgain(t *testing.T) {
	cfg := validConfigurationPayload()
	bars := breakoutBars("AAPL")
	equity := cfg.NotionalAccount.StartingEquity

	s := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), 21_000)).
		bars(bars[:len(bars)-1])
	// Both account events are stamped before the decision bar's previous
	// close (day 55, 00:00), so the replacing snapshot is eligible to be
	// spent on that bar. A snapshot stamped later fails closed under ADR
	// 0010 instead, which is a different rule and has its own tests.
	s.movement(cashMovementPayload(day(54).Add(time.Hour), -20_000, equity))
	s.snapshot(cashSnapshot(cfg, day(54).Add(2*time.Hour), 30_000))
	s.bar(bars[len(bars)-1])

	emitted := s.mustRun()
	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(proposals) != 1 || len(declines) != 0 {
		detail := ""
		if len(declines) == 1 {
			detail = decodeProposalDeclined(t, declines[0]).Detail
		}
		t.Fatalf("proposals=%d declines=%d, want 1 and 0: the 30,000 snapshot already reflects the withdrawal, so deducting it again would leave 10,000 — %s",
			len(proposals), len(declines), detail)
	}
}

// TestAWithdrawalReducesTheCashAnAddIsCheckedAgainst is the Add-path
// counterpart of the cases above, every one of which ends before a Campaign
// opens and so only ever reaches the new-entry check. ADR 0020's
// cash-movement amendment says both existing checks read the reduced
// figure; this pins the second one. After the opening fill's debit (ADR
// 0020) the snapshot covers Unit 2's rung with 5,000 to spare, a 10,000
// withdrawal lands after the Campaign opens, and the rung is then declined
// against exactly what is left.
func TestAWithdrawalReducesTheCashAnAddIsCheckedAgainst(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 2) error = %v", err)
	}
	cost2 := float64(cashSkipCampaignUnitQuantity) * rung2 * cfg.DollarsPerPoint
	openingCost := fillCost(cfg, openingFill("AAPL"))
	initialCash := openingCost + cost2 + 5_000
	const withdrawal = 10_000.0

	emitted := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), initialCash)).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		movement(cashMovementPayload(day(56).Add(time.Hour), -withdrawal, cfg.NotionalAccount.StartingEquity)).
		bar(addOpportunityBar("AAPL", day(57), rung2+5)).
		mustRun()

	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1: Unit 2's rung, unaffordable only because of the withdrawal", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Kind != event.ProposalDeclinedKindAdd || decline.Reason != event.DeclineReasonInsufficientCash {
		t.Fatalf("decline = %+v, want an insufficient-cash Add decline", decline)
	}
	if want := initialCash - withdrawal - openingCost; decline.AvailableCash != want {
		t.Errorf("AvailableCash = %v, want %v: the snapshot's %v less the %v withdrawal and the %v opening fill", decline.AvailableCash, want, initialCash, withdrawal, openingCost)
	}
	if decline.RequiredCash != cost2 {
		t.Errorf("RequiredCash = %v, want Unit 2's cost %v", decline.RequiredCash, cost2)
	}
}

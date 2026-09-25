package strategy_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds the tests for a dividend (ADR 0004, ADR 0024): the
// dividend kind of market.corporate-action, and the
// strategy.campaign.dividend decision the reducer records for it.
//
// ADR 0004's rule is that a dividend is "credited as cash events when they
// occur", never folded into a price series: it must change no channel level,
// no N, and no price the rules see. The tests below pin that by comparing a
// run WITH a dividend against the identical run without one: every decision
// other than the dividend itself, including every later Setup evaluation,
// Signal, proposal and Exit Order, must be byte-for-byte identical, which is
// only possible if the dividend touched none of the state those decisions
// are computed from.

func dividendAction(instrumentID string, cashAmount float64, when time.Time) event.CorporateActionPayload {
	return event.CorporateActionPayload{
		InstrumentID: instrumentID,
		Kind:         event.CorporateActionKindDividend,
		EffectiveAt:  when,
		CashAmount:   cashAmount,
		Currency:     "USD",
	}
}

func decodeCampaignDividend(t *testing.T, envelope event.Envelope) event.CampaignDividendPayload {
	t.Helper()
	if envelope.Type != event.CampaignDividendEventType || envelope.SchemaVersion != event.CampaignDividendSchemaVersion {
		t.Fatalf("envelope = %s/%d, want %s/%d", envelope.Type, envelope.SchemaVersion, event.CampaignDividendEventType, event.CampaignDividendSchemaVersion)
	}
	var payload event.CampaignDividendPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("emitted campaign dividend payload fails its own Validate(): %v", err)
	}
	return payload
}

// dividendAt mirrors splitAt/symbolChangeAt: the moment between day(56)'s
// Session and day(57)'s.
var dividendAt = day(56).Add(12 * time.Hour)

// TestADividendCreditsCashDuringAnOpenCampaignWithoutChangingLevels is the
// headline reducer-seam case (issue #38's "a dividend during an open
// Campaign: cash up, levels and N unchanged"). It runs the identical
// fixture with and without the dividend and requires every decision after
// it — the next bar's Setup evaluation, the exit proposal and the Campaign's
// eventual exit, all of which depend on the Entry/Exit Channel and N — to be
// byte-for-byte identical, proving the dividend changed none of them.
func TestADividendCreditsCashDuringAnOpenCampaignWithoutChangingLevels(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))

	buildTail := func(s *stream) []event.Envelope {
		breachAt := day(58)
		exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, day(59))
		return s.bar(addOpportunityBar("AAPL", day(57), 200)).
			bar(postEntryBar("AAPL", breachAt, 99)).
			fill(exitFill).
			mustRun()
	}

	withDividend := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		corporateAction(dividendAction("AAPL", 184.0, dividendAt))
	dividendID := withDividend.envelopes[len(withDividend.envelopes)-1].ID

	dividendRun := withDividend.mustRun()
	fromDividend := causedBy(dividendRun, dividendID)
	if len(fromDividend) != 1 {
		t.Fatalf("the dividend caused %d decision(s), want 1: %v", len(fromDividend), fromDividend)
	}
	got := decodeCampaignDividend(t, fromDividend[0])
	want := event.CampaignDividendPayload{
		CampaignID:        campaignID,
		InstrumentID:      "AAPL",
		CorporateActionID: dividendID,
		EffectiveAt:       dividendAt,
		CashAmount:        184.0,
		Currency:          "USD",
		Rule:              event.RuleDividendCreditedAsCash,
		ADR:               event.ADRDividendCreditedAsCash,
	}
	if got != want {
		t.Fatalf("dividend = %+v, want %+v", got, want)
	}

	// buildTail runs the WHOLE stream from its configuration event, so the
	// "with dividend" side carries every decision the "without" side does,
	// plus the one dividend decision itself: filtering that one type out is
	// what makes "every OTHER decision is identical" the actual comparison,
	// rather than "the run has one more decision" (true of any new decision
	// kind, and not what this test is about).
	var withTail []event.Envelope
	for _, e := range buildTail(withDividend) {
		if e.Type != event.CampaignDividendEventType {
			withTail = append(withTail, e)
		}
	}
	withoutTail := buildTail(newStream(t, cfg).bars(breakoutBars("AAPL")).fill(openingFill("AAPL")))

	if len(withTail) != len(withoutTail) {
		t.Fatalf("%d decision(s) after the dividend, want %d (the dividend must change nothing about the Campaign's later decisions)", len(withTail), len(withoutTail))
	}
	for i := range withTail {
		if withTail[i].Type != withoutTail[i].Type || string(withTail[i].Payload) != string(withoutTail[i].Payload) {
			t.Fatalf("decision %d differs with a dividend applied:\n  with:    %s %s\n  without: %s %s",
				i, withTail[i].Type, withTail[i].Payload, withoutTail[i].Type, withoutTail[i].Payload)
		}
	}
}

// TestADividendWithNoOpenCampaignFailsClosed pins ADR 0019's "explained"
// standard: a payment for shares this engine holds none of is unexplained.
func TestADividendWithNoOpenCampaignFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		bar(addOpportunityBar("AAPL", day(1), 200)).
		corporateAction(dividendAction("AAPL", 100, day(1).Add(12*time.Hour))).
		wantRunError("no open Campaign")
}

// TestADividendOnAnUnknownInstrumentFailsClosed: unlike a delisting notice,
// a dividend payment names an instrument this engine must actually hold
// shares in, so an entirely unknown instrument cannot explain it either.
func TestADividendOnAnUnknownInstrumentFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		corporateAction(dividendAction("AAPL", 100, day(1))).
		wantRunError("no open Campaign")
}

// TestADividendRedeliveredAtTheSameInstantFailsClosed: each dividend applies
// once, so a redelivered one cannot credit its cash twice.
func TestADividendRedeliveredAtTheSameInstantFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		corporateAction(dividendAction("AAPL", 184.0, dividendAt)).
		corporateAction(dividendAction("AAPL", 184.0, dividendAt)).
		wantRunError("not after the dividend already applied")
}

// TestASecondLaterDividendAppliesOnItsOwnTerms: unlike a split, a Campaign
// may legitimately be paid several dividends over its life.
func TestASecondLaterDividendAppliesOnItsOwnTerms(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	second := dividendAt.Add(24 * time.Hour)
	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		corporateAction(dividendAction("AAPL", 184.0, dividendAt)).
		corporateAction(dividendAction("AAPL", 200.0, second)).
		mustRun()

	var dividends []event.CampaignDividendPayload
	for _, e := range emitted {
		if e.Type == event.CampaignDividendEventType {
			dividends = append(dividends, decodeCampaignDividend(t, e))
		}
	}
	if len(dividends) != 2 {
		t.Fatalf("got %d dividend decision(s), want 2", len(dividends))
	}
	if dividends[0].CashAmount != 184.0 || dividends[1].CashAmount != 200.0 {
		t.Fatalf("dividends = %+v, want 184.0 then 200.0", dividends)
	}
}

// TestADividendInAWrongCurrencyFailsClosed: the account is kept in one
// currency, and a dividend stated in another is out of scope.
func TestADividendInAWrongCurrencyFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	action := dividendAction("AAPL", 184.0, dividendAt)
	action.Currency = "EUR"
	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		corporateAction(action).
		wantRunError("multi-currency accounts are out of scope")
}

// TestADividendBeforeItsLastBarFailsClosed pins the chronology every
// corporate action shares (ADR 0023/0024): it cannot predate the last
// completed bar this reducer holds for the instrument.
func TestADividendBeforeItsLastBarFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		corporateAction(dividendAction("AAPL", 184.0, day(55))).
		wantRunError("predates the last completed bar")
}

// TestADividendBeforeALaterFillFailsClosed: a dividend cannot predate a fill
// the Campaign has already accepted, even one dated after the last bar this
// reducer holds (a fill-chained Add's own FilledAt, here, mirroring how a
// closing fill's FilledAt can follow its own breach bar in this package's
// fixtures).
func TestADividendBeforeALaterFillFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	late := day(57).Add(time.Hour)
	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addOpportunityBar("AAPL", day(57), rung2+5)).
		fill(addFill("AAPL", testDecisionID("campaign", "AAPL", day(56)), 2, day(57), "sim-fill-add-2", rung2, 133, late)).
		corporateAction(dividendAction("AAPL", 184.0, day(57).Add(30*time.Minute))).
		wantRunError("predates the fill")
}

// TestADividendOnADelistedInstrumentFailsClosed: a delisted instrument holds
// nothing, so it cannot have earned a dividend (ADR 0009).
func TestADividendOnADelistedInstrumentFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		corporateAction(delistingAction("AAPL", day(1))).
		corporateAction(dividendAction("AAPL", 100, day(2))).
		wantRunError("already delisted")
}

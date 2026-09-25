package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// decisionLine explains recorded values without re-running strategy rules
// (docs/development.md principle 3). Event time, in UTC, dates each decision.
func decisionLine(e event.Envelope) (line, instrument string, err error) {
	if err := e.Validate(); err != nil {
		return "", "", err
	}
	sentence, err := decisionSentence(e)
	if err != nil {
		return "", "", err
	}
	var meta struct {
		Instrument string `json:"instrument_id"`
		Rule       string `json:"rule"`
		ADR        string `json:"adr"`
	}
	if err := json.Unmarshal(e.Payload, &meta); err != nil {
		return "", "", err
	}
	if meta.Rule == "" {
		meta.Rule = "not recorded"
	}
	if meta.ADR == "" {
		meta.ADR = "not recorded"
	}
	subject := logText(meta.Instrument)
	if subject == "" {
		subject = "account"
	}
	return fmt.Sprintf("%s [decision %d] %s: %s (rule %s; ADR %s).", e.EventTime.UTC().Format(time.RFC3339Nano), e.Sequence, subject, sentence, logText(meta.Rule), logText(meta.ADR)), meta.Instrument, nil
}

// decisionNumber delegates float rendering to the canonical encoder (ADR 0016).
// The single-field wrapper has no whitespace and is removed without reformatting.
func decisionNumber(v float64) string {
	const prefix = `{"v":`
	raw := event.CanonicalBytes(map[string]any{"v": v})
	return string(raw[len(prefix) : len(raw)-1])
}

// logText keeps recorded text from lying about the shape of the line it is
// written on. A journal's text fields are unrestricted Unicode — neither the
// envelope nor the chain narrows them — so besides control characters they
// can carry a line or paragraph separator (U+2028, U+2029) that splits the
// line in a viewer, or a bidi override (U+202E) that reverses the order it
// is read in. Every non-graphic rune is escaped; printable text, in any
// alphabet, is left readable.
func logText(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		return strconv.QuoteToGraphic(s)
	}
	return s
}

// exitCause names what ended the Campaign, reading
// CampaignExitedPayload.FillID as that payload defines it for the recorded
// Reason. A stop or Exit-Channel exit is a fill's own report, so the fill
// confirms it. A Delisting Exit has no order and no execution behind it
// (ADR 0009 forces the close at the last available price), so FillID is the
// corporate-action envelope instead — and a log that called it a fill would
// send a reconciliation hunting a broker record that never existed.
func exitCause(p event.CampaignExitedPayload) string {
	if p.Reason == event.ExitReasonDelisting {
		return fmt.Sprintf("forced by corporate action %q; no fill was recorded for this exit", p.FillID)
	}
	return fmt.Sprintf("confirmed by fill %q", p.FillID)
}

// exitPriceLabel names CampaignExitedPayload.ExitPrice for the same reason:
// it is what the closing fills averaged, except under ADR 0009, where it is
// the last completed bar's close and no trade happened at it.
func exitPriceLabel(reason string) string {
	if reason == event.ExitReasonDelisting {
		return "last available price"
	}
	return "average price"
}

func renderDecision[T interface{ Validate() error }](e event.Envelope, version uint32, render func(T) string) (string, error) {
	if e.SchemaVersion != version {
		return "", fmt.Errorf("%s schema %d is not supported; expected %d", e.Type, e.SchemaVersion, version)
	}
	var p T
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return "", err
	}
	if err := p.Validate(); err != nil {
		return "", err
	}
	return render(p), nil
}

// decisionSentence renders the declared payload schema and its recorded reasons.
// Unknown schemas fail closed (ADR 0015); no trading condition is recomputed.
func decisionSentence(e event.Envelope) (string, error) {
	switch e.Type {
	case event.SignalEventType:
		return renderDecision(e, event.SignalSchemaVersion, func(p event.SignalPayload) string {
			return fmt.Sprintf("Signal %s because high %s exceeded Entry Channel %s; N was %s", logText(p.Direction), decisionNumber(p.BreakoutHigh), decisionNumber(p.EntryChannelHigh), decisionNumber(p.N))
		})
	case event.TradeProposalEventType:
		return renderDecision(e, event.TradeProposalSchemaVersion, func(p event.TradeProposalPayload) string {
			return fmt.Sprintf("proposed a %s entry Unit of %d shares at %s for Signal %q, sized by %s from Notional Account %s and N %s; Protective Stop intent %s", logText(p.Direction), p.Quantity, decisionNumber(p.EntryLevel), p.SignalID, logText(string(p.SizingMode)), decisionNumber(p.NotionalAccount), decisionNumber(p.N), decisionNumber(p.ProtectiveStopIntent))
		})
	case event.CampaignOpenedEventType:
		return renderDecision(e, event.CampaignOpenedSchemaVersion, func(p event.CampaignOpenedPayload) string {
			return fmt.Sprintf("opened %s Campaign %q with %d Unit(s), %d shares at %s, because fill %q was recorded; Protective Stop %s", logText(p.Direction), p.CampaignID, p.Units, p.FilledQuantity, decisionNumber(p.EntryPrice), p.FillID, decisionNumber(p.ProtectiveStop))
		})
	case event.AddProposalEventType:
		return renderDecision(e, event.AddProposalSchemaVersion, func(p event.AddProposalPayload) string {
			return fmt.Sprintf("proposed Add of Unit %d to Campaign %q: %d shares at Add Ladder level %s, following Unit fill %s with Campaign N %s", p.UnitIndex, p.CampaignID, p.Quantity, decisionNumber(p.Level), decisionNumber(p.PreviousUnitFill), decisionNumber(p.CampaignN))
		})
	case event.CampaignUnitAddedEventType:
		return renderDecision(e, event.CampaignUnitAddedSchemaVersion, func(p event.CampaignUnitAddedPayload) string {
			return fmt.Sprintf("completed Add of Unit %d to Campaign %q: %d shares at %s because fill %q was recorded; now %s with the added Unit's Protective Stop at %s", p.UnitIndex, p.CampaignID, p.Quantity, decisionNumber(p.FillPrice), p.FillID, unitCount(p.Units), decisionNumber(p.ProtectiveStop))
		})
	case event.ProtectiveStopSetEventType:
		return renderDecision(e, event.ProtectiveStopSetSchemaVersion, func(p event.ProtectiveStopSetPayload) string {
			return fmt.Sprintf("set Protective Stop for Unit %d of Campaign %q to %s because %s; previous level %s, entry price %s, Campaign N %s", p.UnitIndex, p.CampaignID, decisionNumber(p.Level), logText(p.Reason), decisionNumber(p.PreviousLevel), decisionNumber(p.EntryPrice), decisionNumber(p.CampaignN))
		})
	case event.ExitOrderSetEventType:
		return renderDecision(e, event.ExitOrderSetSchemaVersion, func(p event.ExitOrderSetPayload) string {
			exit := "no Exit-Channel exit proposed"
			if p.ExitChannelLevel > 0 {
				exit = "Exit Channel level " + decisionNumber(p.ExitChannelLevel)
			}
			return fmt.Sprintf("set Exit Order for Unit %d of Campaign %q to %d shares at %s because %s governs; Protective Stop %s, %s", p.UnitIndex, p.CampaignID, p.Quantity, decisionNumber(p.Level), logText(p.Source), decisionNumber(p.ProtectiveStop), exit)
		})
	case event.ExitProposalEventType:
		return renderDecision(e, event.ExitProposalSchemaVersion, func(p event.ExitProposalPayload) string {
			return fmt.Sprintf("proposed exit of Campaign %q: %d shares at %s because %s", p.CampaignID, p.Quantity, decisionNumber(p.Level), logText(p.Reason))
		})
	case event.CampaignExitedEventType:
		return renderDecision(e, event.CampaignExitedSchemaVersion, func(p event.CampaignExitedPayload) string {
			return fmt.Sprintf("exited Campaign %q because %s, %s; %s and %d shares closed at %s %s; realised result %s", p.CampaignID, logText(p.Reason), exitCause(p), unitCount(p.Units), p.Quantity, exitPriceLabel(p.Reason), decisionNumber(p.ExitPrice), decisionNumber(p.RealisedResult))
		})
	case event.CampaignUnitsStoppedEventType:
		return renderDecision(e, event.CampaignUnitsStoppedSchemaVersion, func(p event.CampaignUnitsStoppedPayload) string {
			return fmt.Sprintf("Protective Stop closed Units %v of Campaign %q: %d shares at %s because fill %q was recorded; %s remain; realised result %s", p.UnitIndexes, p.CampaignID, p.QuantityClosed, decisionNumber(p.FillPrice), p.FillID, unitCount(p.RemainingUnits), decisionNumber(p.RealisedResult))
		})
	case event.CampaignCashInLieuEventType:
		return renderDecision(e, event.CampaignCashInLieuSchemaVersion, func(p event.CampaignCashInLieuPayload) string {
			if len(p.Reductions) == 0 {
				return fmt.Sprintf("recorded %s %s cash in lieu for Campaign %q from the %d-for-%d split of corporate action %q, which lost no whole share; the Campaign still holds %d shares", decisionNumber(p.CashInLieu), logText(p.Currency), p.CampaignID, p.NewShares, p.OldShares, p.CorporateActionID, p.QuantityAfter)
			}
			units := make([]int, len(p.Reductions))
			for i, r := range p.Reductions {
				units[i] = r.UnitIndex
			}
			return fmt.Sprintf("took one raw share (%s) off Units %v of Campaign %q, most recent first, because the %d-for-%d split of corporate action %q lost %d raw share(s) and paid %s %s cash in lieu; the Campaign now holds %d shares, %d before", engineShares(p.EngineSharesPerRawShare), units, p.CampaignID, p.NewShares, p.OldShares, p.CorporateActionID, p.RawSharesLost, decisionNumber(p.CashInLieu), logText(p.Currency), p.QuantityAfter, p.QuantityBefore)
		})
	case event.ProposalExpiredEventType:
		return renderDecision(e, event.ProposalExpiredSchemaVersion, func(p event.ProposalExpiredPayload) string {
			return fmt.Sprintf("expired %s proposal %q for %d shares at %s because %s; no fill was recorded for this proposal", logText(p.Kind), p.ProposalID, p.Quantity, decisionNumber(p.Level), logText(p.Reason))
		})
	case event.ProposalDeclinedEventType:
		return renderDecision(e, event.ProposalDeclinedSchemaVersion, func(p event.ProposalDeclinedPayload) string {
			text := fmt.Sprintf("declined %s proposal because %s: %s", logText(p.Kind), logText(p.Reason), logText(p.Detail))
			if p.Kind == event.ProposalDeclinedKindAdd {
				text += fmt.Sprintf("; Campaign %q", p.CampaignID)
			} else {
				text += fmt.Sprintf("; Signal %q", p.SignalID)
			}
			if p.Reason == event.DeclineReasonInsufficientCash {
				text += fmt.Sprintf("; required cash %s exceeded available cash %s", decisionNumber(p.RequiredCash), decisionNumber(p.AvailableCash))
			}
			return text
		})
	case event.SetupEvaluatedEventType:
		return renderDecision(e, event.SetupEvaluatedSchemaVersion, func(p event.SetupEvaluatedPayload) string {
			if !p.NReady || !p.EntryChannelReady {
				return fmt.Sprintf("Setup could not be evaluated because inputs were not ready (N ready: %t; Entry Channel ready: %t)", p.NReady, p.EntryChannelReady)
			}
			tier := "outside Tier A and Tier B"
			if p.Tier != "" {
				tier = "in Tier " + p.Tier
			}
			return fmt.Sprintf("Setup was %s, with distance to Entry Channel %s N; Entry Channel %s and N %s", tier, decisionNumber(p.DistanceToEntryInN), decisionNumber(p.EntryChannelHigh), decisionNumber(p.N))
		})
	case event.CampaignEvaluatedEventType:
		return renderDecision(e, event.CampaignEvaluatedSchemaVersion, func(p event.CampaignEvaluatedPayload) string {
			exit := "Exit Channel was not ready"
			if p.ExitChannelReady {
				exit = fmt.Sprintf("Exit Channel %s; exit condition met: %t", decisionNumber(p.ExitChannelLow), p.ExitConditionMet)
			}
			return fmt.Sprintf("evaluated Campaign %q with %s; %s; aggregate open risk %s against Notional Account %s", p.CampaignID, unitCount(len(p.Units)), exit, decisionNumber(p.AggregateOpenRisk), decisionNumber(p.NotionalAccount))
		})
	case event.DrawdownStepAppliedEventType:
		return renderDecision(e, event.DrawdownStepAppliedSchemaVersion, func(p event.DrawdownStepAppliedPayload) string {
			return fmt.Sprintf("applied Drawdown Step %d, reducing Notional Account from %s to %s because actual equity %s was at or below threshold %s", p.StepNumber, decisionNumber(p.NotionalBefore), decisionNumber(p.NotionalAfter), decisionNumber(p.Equity), decisionNumber(p.Threshold))
		})
	case event.NotionalAccountRecoveredEventType:
		return renderDecision(e, event.NotionalAccountRecoveredSchemaVersion, func(p event.NotionalAccountRecoveredPayload) string {
			return fmt.Sprintf("restored Notional Account from %s to %s and cleared %d Drawdown Steps because actual equity %s regained the yearly starting figure", decisionNumber(p.NotionalBefore), decisionNumber(p.StartingFigure), p.StepsCleared, decisionNumber(p.Equity))
		})
	case event.NotionalAccountRebasedEventType:
		return renderDecision(e, event.NotionalAccountRebasedSchemaVersion, func(p event.NotionalAccountRebasedPayload) string {
			return fmt.Sprintf("rebased Notional Account to actual equity %s for the yearly re-basing; yearly starting figure changed from %s to %s", decisionNumber(p.Equity), decisionNumber(p.PreviousStartingFigure), decisionNumber(p.NewStartingFigure))
		})
	case event.NotionalAccountCashAdjustedEventType:
		return renderDecision(e, event.NotionalAccountCashAdjustedSchemaVersion, func(p event.NotionalAccountCashAdjustedPayload) string {
			return fmt.Sprintf("adjusted Notional Account from %s to %s because cash movement %s changed actual equity from %s to %s", decisionNumber(p.NotionalBefore), decisionNumber(p.NotionalAfter), decisionNumber(p.Amount), decisionNumber(p.EquityBefore), decisionNumber(p.EquityAfter))
		})
	case event.EngineStateEventType:
		return renderDecision(e, event.EngineStateSchemaVersion, func(p event.EngineStatePayload) string {
			return fmt.Sprintf("engine became %s because %s: %s", logText(p.State), logText(p.Reason), logText(p.Detail))
		})
	default:
		return "", fmt.Errorf("unsupported decision type %q", e.Type)
	}
}

// engineShares states a number of the engine's split-adjusted shares in
// English: "1 engine share", otherwise "n engine shares".
func engineShares(n int64) string {
	if n == 1 {
		return "1 engine share"
	}
	return fmt.Sprintf("%d engine shares", n)
}

// unitCount states a number of Units in English: "1 Unit", otherwise
// "n Units".
func unitCount(n int) string {
	if n == 1 {
		return "1 Unit"
	}
	return fmt.Sprintf("%d Units", n)
}

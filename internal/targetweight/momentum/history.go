package momentum

import (
	"fmt"
	"math"
	"time"
)

// Status is what the rule did with one universe member. It is recorded for
// every ranked name (ADR 0026 point 1, "Output").
type Status string

// The closed set of Status values.
const (
	// StatusScored: a full window with a nonzero volatility; SelectBook
	// replaces it with what it did with the name.
	StatusScored Status = "scored"
	// StatusPicked: in the book.
	StatusPicked Status = "picked"
	// StatusSectorCapped: skipped because its sector already held
	// SectorCap picks (defensive_book, defensive_v2.py:70-71).
	StatusSectorCapped Status = "sector-capped"
	// StatusBeyondTopN: ranked after the book was full.
	StatusBeyondTopN Status = "beyond-top-n"
	// StatusNoSector: no sector label from any source; left out of the
	// ranking, never uncapped (plan.md section 4, "Sectors", "Chosen").
	StatusNoSector Status = "no-sector"
	// StatusZeroVolatility: every return in the volatility window was the
	// same, so it has no inverse-volatility weight; left out.
	StatusZeroVolatility Status = "zero-volatility"
	// StatusShortHistory: fewer than ScoreLookback+1 closes in all
	// (defensive_v2.py:287-288).
	StatusShortHistory Status = "short-history"
	// StatusGap: enough closes, but one session of its window has none
	// (ADR 0026 point 5, "Missing bars fail closed").
	StatusGap Status = "gap"
	// StatusUnaffordable: D7-L skipped it, since one share at its previous
	// raw close exceeds its target value; the next eligible ranked name
	// takes its place (ADR 0026, point 2, "D7-L"; plan.md lines 380-383).
	StatusUnaffordable Status = "unaffordable"
)

// History is one name's score and volatility, or why it has none.
type History struct {
	Status Status
	// Closes counts the name's closes up to the last session.
	Closes     int
	Score      float64
	Volatility float64
}

// Score computes a name's 12-1 score and volatility from its split- and
// dividend-adjusted closes (session -> close) over the calendar sessions,
// whose last is the close session.
//
// The score is close[-(ScoreSkip+1)] / close[-(ScoreLookback+1)] - 1, in
// sessions (defensive_v2.py:289). The volatility is the population
// standard deviation of the last VolatilityReturns daily returns
// (np.std, defensive_v2.py:290-291). A name with fewer than
// ScoreLookback+1 closes is StatusShortHistory; a name missing any session
// of its window is StatusGap.
//
// The backtest dropped missing closes and took the last 253 it had, so its
// window shifted across a gap (defensive_v2.py:286); live leaves the name
// out (ADR 0026 points 5 and 8).
//
// Every close must be dated on a calendar session, finite and positive:
// a close after the last session would be look-ahead, and anything else is
// a malformed input, so each is an error rather than a skipped name, as
// is a configuration that fails Validate.
func Score(cfg Config, sessions []string, closes map[string]float64) (History, error) {
	if err := cfg.Validate(); err != nil {
		return History{}, err
	}
	if err := checkInputs(sessions, closes); err != nil {
		return History{}, err
	}
	need := cfg.ScoreLookback + 1
	h := History{Closes: len(closes)}
	if h.Closes < need {
		h.Status = StatusShortHistory
		return h, nil
	}
	window, ok := lastCloses(sessions, closes, need)
	if !ok {
		h.Status = StatusGap
		return h, nil
	}
	h.Score = window[need-1-cfg.ScoreSkip]/window[0] - 1
	h.Volatility = populationStd(window[need-1-cfg.VolatilityReturns:])
	h.Status = StatusScored
	if h.Volatility == 0 {
		h.Status = StatusZeroVolatility
	}
	return h, nil
}

// populationStd is the population standard deviation of the simple daily
// returns of prices, oldest first. Each squared deviation is rounded
// before it is summed (docs/development.md, "never leave a multiply-add
// fusible").
func populationStd(prices []float64) float64 {
	n := len(prices) - 1
	returns := make([]float64, n)
	sum := 0.0
	for i := range returns {
		returns[i] = prices[i+1]/prices[i] - 1
		sum += returns[i]
	}
	mean := sum / float64(n)
	squares := 0.0
	for _, r := range returns {
		d := r - mean
		squares += float64(d * d)
	}
	return math.Sqrt(squares / float64(n))
}

// Trend is the risk-off test's inputs and result, recorded with every
// decision (ADR 0026 point 1, "momentum's risk-off inputs").
type Trend struct {
	// Closes counts the trend instrument's closes up to the last session.
	Closes  int
	Close   float64
	Mean    float64
	RiskOff bool
}

// EvaluateTrend decides risk-off: the trend instrument's last close is
// strictly below the mean of its last TrendCloses closes, that close
// included (defensive_v2.py:279-280; plan.md section 2, "Risk-off").
//
// Fewer than TrendCloses closes gives ReasonInsufficientTrendHistory, and
// a missing close in the window, the last included, gives
// ReasonMissingBars. The backtest treated the first as risk-on and
// dropped missing closes; live withholds the decision instead (ADR 0026
// point 5). A configuration that fails Validate is an error.
func EvaluateTrend(cfg Config, sessions []string, closes map[string]float64) (Trend, WithheldReason, error) {
	if err := cfg.Validate(); err != nil {
		return Trend{}, "", err
	}
	if err := checkInputs(sessions, closes); err != nil {
		return Trend{}, "", err
	}
	tr := Trend{Closes: len(closes)}
	if tr.Closes < cfg.TrendCloses {
		return tr, ReasonInsufficientTrendHistory, nil
	}
	window, ok := lastCloses(sessions, closes, cfg.TrendCloses)
	if !ok {
		return tr, ReasonMissingBars, nil
	}
	sum := 0.0
	for _, p := range window {
		sum += p
	}
	tr.Close = window[len(window)-1]
	tr.Mean = sum / float64(len(window))
	tr.RiskOff = tr.Close < tr.Mean
	return tr, "", nil
}

// lastCloses returns the closes of the last n sessions, oldest first, and
// false when any of those sessions has no close. Callers check first that
// there are at least n closes, which checkCloses puts on n distinct
// calendar sessions, so the calendar is never shorter than n.
func lastCloses(sessions []string, closes map[string]float64, n int) ([]float64, bool) {
	out := make([]float64, n)
	for i, s := range sessions[len(sessions)-n:] {
		p, ok := closes[s]
		if !ok {
			return nil, false
		}
		out[i] = p
	}
	return out, true
}

// checkInputs refuses a malformed calendar (checkCalendar) or closes
// (checkCloses).
func checkInputs(sessions []string, closes map[string]float64) error {
	if err := checkCalendar(sessions); err != nil {
		return err
	}
	return checkCloses(sessions, closes)
}

// checkCloses refuses a close that is not finite and positive, or that is
// dated on no calendar session.
func checkCloses(sessions []string, closes map[string]float64) error {
	on := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		on[s] = true
	}
	for s, p := range closes {
		if !on[s] {
			return fmt.Errorf("momentum: close dated %q is on no calendar session", s)
		}
		if !finite(p) || p <= 0 {
			return fmt.Errorf("momentum: close %v on %s", p, s)
		}
	}
	return nil
}

// checkCalendar refuses an empty calendar, a session that is not a
// YYYY-MM-DD date, and sessions out of strictly ascending order.
func checkCalendar(sessions []string) error {
	if len(sessions) == 0 {
		return fmt.Errorf("momentum: empty session calendar")
	}
	for i, s := range sessions {
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			return fmt.Errorf("momentum: session %q: %w", s, err)
		}
		if i > 0 && s <= sessions[i-1] {
			return fmt.Errorf("momentum: session %s does not follow %s", s, sessions[i-1])
		}
	}
	return nil
}

// IsDecisionSession reports whether session, following previous, is the
// first session of a month: the momentum decision time, using closes to
// previous (plan.md section 2, "On the first session of each month";
// defensive_v2.py:167).
func IsDecisionSession(previous, session string) (bool, error) {
	p, err := time.Parse(time.DateOnly, previous)
	if err != nil {
		return false, fmt.Errorf("momentum: session %q: %w", previous, err)
	}
	s, err := time.Parse(time.DateOnly, session)
	if err != nil {
		return false, fmt.Errorf("momentum: session %q: %w", session, err)
	}
	if !s.After(p) {
		return false, fmt.Errorf("momentum: session %s does not follow %s", session, previous)
	}
	return s.Year() != p.Year() || s.Month() != p.Month(), nil
}

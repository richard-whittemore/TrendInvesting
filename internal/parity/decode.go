package parity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// The logging templates' payload formats
// (research/edge-search/momentum/parity_log.py and
// research/edge-search/boost/parity_log.py). Numbers are written with
// Python's repr, the shortest string that round-trips, so they decode to
// the very float64 QuantConnect held.

// The required scalars are pointers, so a field that is missing or null is
// refused rather than read as zero.
type momentumPayload struct {
	Month     *string               `json:"month"`
	RiskOff   *bool                 `json:"risk_off"`
	SPYClose  *float64              `json:"spy_close"`
	SPYMean   *float64              `json:"spy_mean"`
	SPYCloses *int                  `json:"spy_n"`
	Members   *int                  `json:"members"`
	Scored    *int                  `json:"scored"`
	Index     []string              `json:"index"`
	Ranked    [][]json.RawMessage   `json:"ranked"`
	Weights   map[string]float64    `json:"weights"`
	Short     map[string]int        `json:"short"`
	Closes    map[string][]*float64 `json:"closes"`
}

// DecodeMomentum decodes one logged month, refusing an unknown field, a
// required field missing or null, a ranked row that is not [ticker, score,
// volatility, sector or null, mark], an empty ticker and an unknown mark.
func DecodeMomentum(raw []byte) (MomentumMonth, error) {
	var p momentumPayload
	if err := strictDecode(raw, &p); err != nil {
		return MomentumMonth{}, err
	}
	if p.Month == nil || *p.Month == "" {
		return MomentumMonth{}, errors.New("parity: momentum payload has no month")
	}
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"risk_off", p.RiskOff != nil}, {"spy_close", p.SPYClose != nil}, {"spy_mean", p.SPYMean != nil},
		{"spy_n", p.SPYCloses != nil}, {"members", p.Members != nil}, {"scored", p.Scored != nil},
	} {
		if !f.present {
			return MomentumMonth{}, fmt.Errorf("parity: %s: %s missing or null", *p.Month, f.name)
		}
	}
	m := MomentumMonth{Month: *p.Month, RiskOff: *p.RiskOff, SPYClose: *p.SPYClose, SPYMean: *p.SPYMean,
		SPYCloses: *p.SPYCloses, Members: *p.Members, Scored: *p.Scored, Index: p.Index,
		Weights: p.Weights, Short: p.Short, Closes: p.Closes}
	for i, row := range p.Ranked {
		e, err := rankedEntry(row)
		if err != nil {
			return MomentumMonth{}, fmt.Errorf("parity: %s ranked row %d: %w", *p.Month, i+1, err)
		}
		m.Ranked = append(m.Ranked, e)
	}
	return m, nil
}

func rankedEntry(row []json.RawMessage) (RankedEntry, error) {
	var e RankedEntry
	if len(row) != 5 {
		return e, fmt.Errorf("%d fields, want 5", len(row))
	}
	var ticker, mark *string
	var score, vol, sector *float64
	for i, dst := range []any{&ticker, &score, &vol, &sector, &mark} {
		if err := json.Unmarshal(row[i], dst); err != nil {
			return e, err
		}
	}
	switch {
	case ticker == nil || *ticker == "":
		return e, errors.New("no ticker")
	case score == nil || vol == nil:
		return e, fmt.Errorf("%s: a null score or volatility", *ticker)
	case mark == nil:
		return e, fmt.Errorf("%s: a null mark", *ticker)
	}
	e.Ticker, e.Score, e.Volatility = *ticker, *score, *vol
	if sector != nil {
		// A Morningstar sector code is a whole number (sector-labels-2026-10.md).
		if *sector != math.Trunc(*sector) || math.Abs(*sector) > 1e9 {
			return e, fmt.Errorf("sector %v is not a whole code", *sector)
		}
		e.Sector = strconv.FormatInt(int64(*sector), 10)
	}
	switch e.Mark = Mark(*mark); e.Mark {
	case MarkPicked, MarkCapped, MarkBeyond:
		return e, nil
	}
	return e, fmt.Errorf("mark %q", *mark)
}

type boostPayload struct {
	Rows [][]json.RawMessage `json:"rows"`
}

// DecodeBoost decodes one chunk of logged sessions, each
// [date, close, RSI, trend mean, exit mean, state, target], an indicator
// not ready and both state and target during warm-up being null.
func DecodeBoost(raw []byte) ([]BoostSession, error) {
	var p boostPayload
	if err := strictDecode(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Rows) == 0 {
		return nil, errors.New("parity: boost payload has no rows")
	}
	out := make([]BoostSession, 0, len(p.Rows))
	for i, row := range p.Rows {
		s, err := boostSession(row)
		if err != nil {
			return nil, fmt.Errorf("parity: boost row %d: %w", i+1, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func boostSession(row []json.RawMessage) (BoostSession, error) {
	var s BoostSession
	if len(row) != 7 {
		return s, fmt.Errorf("%d fields, want 7", len(row))
	}
	var date *string
	var closePx *float64
	var state *int
	var target *float64
	for i, dst := range []any{&date, &closePx, &s.RSI, &s.TrendMean, &s.ExitMean, &state, &target} {
		if err := json.Unmarshal(row[i], dst); err != nil {
			return s, err
		}
	}
	if date == nil || closePx == nil {
		return s, errors.New("a null date or close")
	}
	s.Date, s.Close = *date, *closePx
	switch {
	case state == nil && target == nil:
		s.Warmup = true
	case state == nil || target == nil:
		return s, errors.New("a state without a target, or a target without a state")
	case *state != 0 && *state != 1:
		return s, fmt.Errorf("state %d", *state)
	default:
		s.Latched, s.Target = *state == 1, *target
	}
	return s, nil
}

// strictDecode decodes one JSON value, refusing unknown fields and
// trailing data.
func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parity: %w", err)
	}
	if dec.More() {
		return errors.New("parity: trailing data after the payload")
	}
	return nil
}

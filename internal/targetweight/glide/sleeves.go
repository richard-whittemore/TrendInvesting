package glide

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// Sleeve names recorded against each share of a combined target.
const (
	SleeveMomentum = "momentum"
	SleeveBond     = "bond"
)

// Invested is the fraction of the account a sleeve holds in positions: 98%,
// the research's no-leverage cap (glide_path.py:80 with_bond_sleeve).
const Invested = 0.98

// Weight is a symbol and its target weight as a fraction of the account.
type Weight struct {
	Symbol string
	Weight float64
}

// Share is the part of a combined target contributed by one sleeve.
type Share struct {
	Sleeve string
	Weight float64
}

// Target is one symbol's combined target weight and the share each sleeve
// contributed. Shares are in a fixed order, momentum then bond, and sum to
// Weight.
type Target struct {
	Symbol string
	Weight float64
	Shares []Share
}

// DefaultBondFunds is decision D2: the bond sleeve is half IEF, half SHY
// (glide_path.py:177).
func DefaultBondFunds() []Weight { return []Weight{{"IEF", 0.5}, {"SHY", 0.5}} }

// Combine scales the momentum book (weights summing to at most 98%,
// including its own risk-off IEF) to 1 - b, adds the bond sleeve of 98% of b
// split over funds, and sums a symbol held by both sleeves into one target
// with each sleeve's share recorded (plan.md section 2; glide_path.py:80-88).
// The planner's band applies to the returned sums. Targets are sorted by
// symbol. With b = 0 the bond sleeve is empty and the momentum book is
// returned unscaled. Funds' fractions must be positive and sum to 1.
func Combine(momentum []Weight, b float64, funds []Weight) ([]Target, error) {
	if math.IsNaN(b) || b < 0 || b > 1 {
		return nil, fmt.Errorf("glide: bond share %v is outside [0, 1]", b)
	}
	if err := checkWeights("momentum", momentum, true); err != nil {
		return nil, err
	}
	if err := checkWeights("bond fund", funds, false); err != nil {
		return nil, err
	}
	var fundTotal, momTotal float64
	for _, f := range funds {
		fundTotal += f.Weight
	}
	if math.Abs(fundTotal-1) > 1e-9 {
		return nil, errors.New("glide: bond fund fractions must sum to 1")
	}
	for _, w := range momentum {
		momTotal += w.Weight
	}
	if momTotal > Invested+1e-9 {
		return nil, fmt.Errorf("glide: momentum weights sum to %v, above %v", momTotal, Invested)
	}

	bySymbol := make(map[string]*Target)
	add := func(symbol, sleeve string, weight float64) {
		t := bySymbol[symbol]
		if t == nil {
			t = &Target{Symbol: symbol}
			bySymbol[symbol] = t
		}
		t.Weight += weight
		t.Shares = append(t.Shares, Share{Sleeve: sleeve, Weight: weight})
	}
	scale := 1 - b
	for _, w := range momentum {
		add(w.Symbol, SleeveMomentum, float64(w.Weight*scale))
	}
	if b > 0 {
		bondTotal := float64(Invested * b)
		for _, f := range funds {
			add(f.Symbol, SleeveBond, float64(bondTotal*f.Weight))
		}
	}
	out := make([]Target, 0, len(bySymbol))
	for _, t := range bySymbol {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

func checkWeights(what string, ws []Weight, allowZero bool) error {
	seen := make(map[string]bool, len(ws))
	for _, w := range ws {
		switch {
		case w.Symbol == "":
			return fmt.Errorf("glide: %s has an empty symbol", what)
		case seen[w.Symbol]:
			return fmt.Errorf("glide: %s lists %s twice", what, w.Symbol)
		case math.IsNaN(w.Weight) || math.IsInf(w.Weight, 0) || w.Weight < 0 || (!allowZero && w.Weight == 0):
			return fmt.Errorf("glide: %s weight for %s is %v", what, w.Symbol, w.Weight)
		}
		seen[w.Symbol] = true
	}
	return nil
}

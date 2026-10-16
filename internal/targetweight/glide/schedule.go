// Package glide computes the bond share of an IRA from the owner's age and
// combines the momentum and bond sleeves into one set of target weights
// (docs/live-trading/plan.md section 2, decision D2; ADR 0026 section 6).
//
// It is pure: plain inputs, plain outputs, no clock, no I/O. The birth value
// is a year and month only, configured on the server and never stored in the
// repository.
package glide

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Anchor is one corner of the schedule: the bond share Share at owner's age
// AgeYears whole years.
type Anchor struct {
	AgeYears int
	Share    float64
}

// DefaultAnchors are the plan.md section 2 table and
// research/edge-search/momentum/glide_path.py:59-71: 0% at 38, 30.8% at 53,
// 50% at 65, 70% at 72, flat at 70% after. Share is linear in age in months
// between anchors and 0 before the first.
func DefaultAnchors() []Anchor {
	return []Anchor{{38, 0}, {53, 0.308}, {65, 0.5}, {72, 0.7}}
}

// Plausible owner ages in whole years on the rebalance date; a birth value
// outside them is refused as a likely typo.
const (
	minOwnerAgeYears = 18
	maxOwnerAgeYears = 110
)

// ParseBirth reads a configured birth value "YYYY-MM" into a year and month.
// A day, a missing zero, or any other shape is refused.
func ParseBirth(s string) (year int, month time.Month, err error) {
	if len(s) != 7 || s[4] != '-' {
		return 0, 0, fmt.Errorf("glide: birth value must be YYYY-MM, got %d characters", len(s))
	}
	y, err := strconv.Atoi(s[:4])
	if err != nil || s[0] == '+' || s[0] == '-' {
		return 0, 0, errors.New("glide: birth year is not four digits")
	}
	m, err := strconv.Atoi(s[5:])
	if err != nil || s[5] == '+' || s[5] == '-' {
		return 0, 0, errors.New("glide: birth month is not two digits")
	}
	if m < 1 || m > 12 {
		return 0, 0, fmt.Errorf("glide: birth month %d is not 01-12", m)
	}
	return y, time.Month(m), nil
}

// AgeMonths is the owner's age in whole months on the rebalance date: the
// months from the birth month to the rebalance month. With no birth day the
// age changes on the first day of each month, which is the day-of-month rule
// of glide_path.py:74-77 (age_on) with the birth day taken as 1. It refuses
// a birth value that is malformed or implies an age outside 18 to 110 years.
func AgeMonths(birth string, rebalance time.Time) (int, error) {
	by, bm, err := ParseBirth(birth)
	if err != nil {
		return 0, err
	}
	months := (rebalance.Year()-by)*12 + int(rebalance.Month()) - int(bm)
	if months < minOwnerAgeYears*12 || months > maxOwnerAgeYears*12 {
		return 0, fmt.Errorf("glide: birth value implies an age of %d months, outside %d to %d years",
			months, minOwnerAgeYears, maxOwnerAgeYears)
	}
	return months, nil
}

// BondShare is the bond share b at an age in whole months under the anchors
// (glide_path.py:59-71 glide_share, with age = months/12): 0 at or before the
// first anchor, linear between anchors, the last anchor's share after it.
// Anchors must have strictly increasing ages and shares in [0, 1], else it
// fails closed.
func BondShare(anchors []Anchor, ageMonths int) (float64, error) {
	if len(anchors) < 2 {
		return 0, errors.New("glide: need at least two anchors")
	}
	for i, a := range anchors {
		if math.IsNaN(a.Share) || a.Share < 0 || a.Share > 1 {
			return 0, fmt.Errorf("glide: anchor %d share is outside [0, 1]", i)
		}
		if i > 0 && a.AgeYears <= anchors[i-1].AgeYears {
			return 0, fmt.Errorf("glide: anchor %d age does not increase", i)
		}
	}
	if ageMonths <= anchors[0].AgeYears*12 {
		return 0, nil
	}
	for i := 1; i < len(anchors); i++ {
		lo, hi := anchors[i-1], anchors[i]
		end := hi.AgeYears * 12
		if ageMonths > end {
			continue
		}
		if ageMonths == end {
			return hi.Share, nil
		}
		start := lo.AgeYears * 12
		rise := float64((hi.Share - lo.Share) * float64(ageMonths-start))
		return lo.Share + float64(rise/float64(end-start)), nil
	}
	return anchors[len(anchors)-1].Share, nil
}

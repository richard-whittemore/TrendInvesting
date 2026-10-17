// Package boost is the Boost rule core: SPY held at 98%, raised to 147% while
// RSI(2) is below 10 with the close above its 200-day mean, until a close
// above the 5-day mean (docs/live-trading/plan.md, section 2, "Boost";
// research/edge-search/boost/template.py:33-37,52-55,89-100). It is pure
// arithmetic over adjusted closes and knows nothing of events, accounts or
// orders, and shares nothing with internal/strategy (AGENTS.md rule 4).
package boost

import "fmt"

// Parameters are Boost's backtested parameters (template.py:33-37).
type Parameters struct {
	InvestedFraction float64
	BoostSize        float64
	RSIEntry         float64
	RSIPeriod        int
	TrendDays        int
	ExitSMA          int
}

// Readings are one session's adjusted close and the indicators after it.
// An indicator not ready carries 0.
type Readings struct {
	Close          float64
	RSI            float64
	RSIReady       bool
	TrendMean      float64
	TrendMeanReady bool
	ExitMean       float64
	ExitMeanReady  bool
}

// Next returns the latched boost state after one session (template.py:89-94):
// unlatched, it latches when the trend mean is ready with the close strictly
// above it and RSI is ready and strictly below RSIEntry; latched, it unlatches
// only on a close strictly above the exit mean. A latched boost survives a
// close below the trend mean.
func (p Parameters) Next(latched bool, r Readings) bool {
	if !latched {
		trendOK := r.TrendMeanReady && r.Close > r.TrendMean
		return trendOK && r.RSIReady && r.RSI < p.RSIEntry
	}
	return !(r.ExitMeanReady && r.Close > r.ExitMean)
}

// Target returns the SPY weight for a latched state: InvestedFraction, times
// BoostSize while latched (template.py:96, want = 0.98 x (BOOST_SIZE if sig
// else 1)).
func (p Parameters) Target(latched bool) float64 {
	if latched {
		return p.InvestedFraction * p.BoostSize
	}
	return p.InvestedFraction
}

// Indicators are Boost's three indicators over adjusted closes, each updated
// with a session's close before that session is decided, as QuantConnect
// updates them before OnData (template.py:52-54, 73-94).
//
// Every close must be one session after the last: the indicators never span a
// gap (plan.md section 2, "Boost"). The caller withholds instead of adding.
type Indicators struct {
	rsi   rsi
	trend sma
	exit  sma
}

// NewIndicators returns empty indicators for p's lengths.
func NewIndicators(p Parameters) (*Indicators, error) {
	if p.RSIPeriod < 1 || p.TrendDays < 1 || p.ExitSMA < 1 {
		return nil, fmt.Errorf("boost: indicator lengths must be at least 1 (rsi %d, trend %d, exit %d)", p.RSIPeriod, p.TrendDays, p.ExitSMA)
	}
	return &Indicators{
		rsi:   newRSI(p.RSIPeriod),
		trend: sma{window: make([]float64, 0, p.TrendDays), length: p.TrendDays},
		exit:  sma{window: make([]float64, 0, p.ExitSMA), length: p.ExitSMA},
	}, nil
}

// Add updates every indicator with close and returns the readings after it.
func (i *Indicators) Add(price float64) Readings {
	i.rsi.add(price)
	i.trend.add(price)
	i.exit.add(price)
	r := Readings{Close: price}
	r.RSI, r.RSIReady = i.rsi.value()
	r.TrendMean, r.TrendMeanReady = i.trend.value()
	r.ExitMean, r.ExitMeanReady = i.exit.value()
	return r
}

// sma is a simple mean of the last length closes, the current one included
// (QuantConnect's SimpleMovingAverage, template.py:52-53). Ready once length
// closes have been added.
type sma struct {
	window []float64
	length int
}

func (s *sma) add(price float64) {
	if len(s.window) == s.length {
		copy(s.window, s.window[1:])
		s.window = s.window[:s.length-1]
	}
	s.window = append(s.window, price)
}

func (s *sma) value() (float64, bool) {
	if len(s.window) < s.length {
		return 0, false
	}
	var sum float64
	for _, v := range s.window {
		sum += v
	}
	return sum / float64(s.length), true
}

// rsi is QuantConnect's RelativeStrengthIndex with Wilder averages
// (template.py:54, MovingAverageType.Wilders): each close after the first adds
// its rise to the gain average and its fall to the loss average (an unchanged
// close adds 0 to both); RSI = 100 - 100 / (1 + gain/loss), and 100 when the
// loss average rounds to 0 at ten decimals. Ready once period changes, so
// period + 1 closes, have been added.
type rsi struct {
	gain, loss wilder
	previous   float64
	hasPrev    bool
}

func newRSI(period int) rsi {
	return rsi{gain: newWilder(period), loss: newWilder(period)}
}

func (r *rsi) add(price float64) {
	if r.hasPrev {
		if price >= r.previous {
			r.gain.add(price - r.previous)
			r.loss.add(0)
		} else {
			r.gain.add(0)
			r.loss.add(r.previous - price)
		}
	}
	r.previous = price
	r.hasPrev = true
}

func (r *rsi) value() (float64, bool) {
	if !r.gain.ready() {
		return 0, false
	}
	// QuantConnect rounds the loss average to ten decimals before testing
	// for zero; half of 1e-10 rounds to even, which is 0.
	if r.loss.value <= 5e-11 {
		return 100, true
	}
	return 100 - 100/(1+r.gain.value/r.loss.value), true
}

// wilder is QuantConnect's WilderMovingAverage: the simple mean of the values
// so far until period values, then value x k + previous x (1 - k) with
// k = 1/period. At the period-th value both forms give the simple mean.
type wilder struct {
	period  int
	k       float64
	samples int
	sum     float64
	value   float64
}

func newWilder(period int) wilder {
	return wilder{period: period, k: 1 / float64(period)}
}

func (w *wilder) add(x float64) {
	w.samples++
	if w.samples < w.period {
		w.sum += x
		w.value = w.sum / float64(w.samples)
		return
	}
	// Each product is rounded before the sum (docs/development.md,
	// "never leave a multiply-add fusible").
	w.value = float64(x*w.k) + float64(w.value*(1-w.k))
}

func (w *wilder) ready() bool { return w.samples >= w.period }

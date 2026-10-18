package parity_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
)

// A synthetic payload in the logging templates' format
// (research/edge-search/momentum/parity_log.py, parity_record).
const momentumPayload = `{"month":"2009-06","risk_off":false,"spy_close":101.5,"spy_mean":99.25,"spy_n":3,` +
	`"members":3,"scored":2,"index":["2009-05-27","2009-05-28","2009-05-29"],` +
	`"ranked":[["AAA",0.5,0.012345678901234568,311,"P"],["BBB",0.25,0.02,null,"-"]],` +
	`"weights":{"AAA":0.98},"short":{"NEW":2},` +
	`"closes":{"SPY":[100.0,101.0,101.5],"NEW":[null,10.5,11.0]}}`

func TestAMomentumPayloadDecodesExactly(t *testing.T) {
	m, err := parity.DecodeMomentum([]byte(momentumPayload))
	if err != nil {
		t.Fatal(err)
	}
	if m.Month != "2009-06" || m.RiskOff || m.SPYClose != 101.5 || m.SPYMean != 99.25 || m.SPYCloses != 3 ||
		m.Members != 3 || m.Scored != 2 || len(m.Index) != 3 || m.Index[2] != "2009-05-29" {
		t.Errorf("header %+v", m)
	}
	want := []parity.RankedEntry{{"AAA", 0.5, 0.012345678901234568, "311", parity.MarkPicked}, {"BBB", 0.25, 0.02, "", parity.MarkBeyond}}
	if len(m.Ranked) != 2 || m.Ranked[0] != want[0] || m.Ranked[1] != want[1] {
		t.Errorf("ranked %+v", m.Ranked)
	}
	if m.Weights["AAA"] != 0.98 || m.Short["NEW"] != 2 {
		t.Errorf("weights %v short %v", m.Weights, m.Short)
	}
	if c := m.Closes["NEW"]; c[0] != nil || *c[1] != 10.5 || *c[2] != 11.0 {
		t.Errorf("closes %v", c)
	}
}

func TestAMalformedMomentumPayloadIsRefused(t *testing.T) {
	for name, payload := range map[string]string{
		"not JSON":                "{",
		"an unknown field":        strings.Replace(momentumPayload, `"members"`, `"memberz"`, 1),
		"a short ranked row":      strings.Replace(momentumPayload, `["BBB",0.25,0.02,null,"-"]`, `["BBB",0.25,0.02,null]`, 1),
		"an unknown mark":         strings.Replace(momentumPayload, `"-"]`, `"X"]`, 1),
		"a text score":            strings.Replace(momentumPayload, `["BBB",0.25`, `["BBB","0.25"`, 1),
		"a text sector":           strings.Replace(momentumPayload, `311,"P"`, `"311","P"`, 1),
		"a fractional sector":     strings.Replace(momentumPayload, `311,"P"`, `311.5,"P"`, 1),
		"a numeric ticker":        strings.Replace(momentumPayload, `["AAA"`, `[7`, 1),
		"a numeric mark":          strings.Replace(momentumPayload, `"P"]`, `1]`, 1),
		"a text volatility":       strings.Replace(momentumPayload, `0.012345678901234568`, `"v"`, 1),
		"no month":                strings.Replace(momentumPayload, `"month":"2009-06",`, ``, 1),
		"trailing data":           momentumPayload + "{}",
		"a ranked row not a list": strings.Replace(momentumPayload, `["BBB",0.25,0.02,null,"-"]`, `{}`, 1),
		"a null ticker":           strings.Replace(momentumPayload, `["AAA"`, `[null`, 1),
		"an empty ticker":         strings.Replace(momentumPayload, `["AAA"`, `[""`, 1),
		"a null score":            strings.Replace(momentumPayload, `["BBB",0.25`, `["BBB",null`, 1),
		"a null volatility":       strings.Replace(momentumPayload, `0.012345678901234568`, `null`, 1),
		"a null mark":             strings.Replace(momentumPayload, `"P"]`, `null]`, 1),
		"a null month":            strings.Replace(momentumPayload, `"month":"2009-06"`, `"month":null`, 1),
		"a null risk-off flag":    strings.Replace(momentumPayload, `"risk_off":false`, `"risk_off":null`, 1),
		"a null SPY close":        strings.Replace(momentumPayload, `"spy_close":101.5`, `"spy_close":null`, 1),
		"a null SPY mean":         strings.Replace(momentumPayload, `"spy_mean":99.25`, `"spy_mean":null`, 1),
		"a null SPY close count":  strings.Replace(momentumPayload, `"spy_n":3`, `"spy_n":null`, 1),
		"a null member count":     strings.Replace(momentumPayload, `"members":3`, `"members":null`, 1),
		"a null scored count":     strings.Replace(momentumPayload, `"scored":2`, `"scored":null`, 1),
		"no SPY mean":             strings.Replace(momentumPayload, `"spy_mean":99.25,`, ``, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parity.DecodeMomentum([]byte(payload)); err == nil {
				t.Errorf("%s must be refused", name)
			}
		})
	}
}

// A synthetic payload: round made-up closes, not market data.
const boostPayload = `{"rows":[["2001-01-02",10.0,null,null,null,null,null],` +
	`["2001-01-03",10.5,50.0,null,10.25,0,0.98],["2001-01-04",11.0,60.5,40.0,10.75,1,1.47]]}`

func TestABoostPayloadDecodesExactly(t *testing.T) {
	s, err := parity.DecodeBoost([]byte(boostPayload))
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 3 {
		t.Fatalf("sessions %+v", s)
	}
	if !s[0].Warmup || s[0].RSI != nil || s[0].Close != 10.0 || s[0].Date != "2001-01-02" {
		t.Errorf("warm-up session %+v", s[0])
	}
	if s[1].Warmup || s[1].Latched || s[1].Target != 0.98 || *s[1].RSI != 50 || s[1].TrendMean != nil || *s[1].ExitMean != 10.25 {
		t.Errorf("session %+v", s[1])
	}
	if !s[2].Latched || s[2].Target != 1.47 || *s[2].TrendMean != 40 {
		t.Errorf("session %+v", s[2])
	}
}

func TestAMalformedBoostPayloadIsRefused(t *testing.T) {
	for name, payload := range map[string]string{
		"not JSON":               "[",
		"a short row":            strings.Replace(boostPayload, `10.5,50.0,null,10.25,0,0.98]`, `10.5,50.0,null,10.25,0]`, 1),
		"a state of 2":           strings.Replace(boostPayload, `10.25,0,0.98`, `10.25,2,0.98`, 1),
		"a state with no target": strings.Replace(boostPayload, `10.25,0,0.98`, `10.25,0,null`, 1),
		"a text close":           strings.Replace(boostPayload, `10.0,null`, `"x",null`, 1),
		"a numeric date":         strings.Replace(boostPayload, `"2001-01-02"`, `20010102`, 1),
		"a null date":            strings.Replace(boostPayload, `"2001-01-02"`, `null`, 1),
		"a null close":           strings.Replace(boostPayload, `10.0,null`, `null,null`, 1),
		"a text RSI":             strings.Replace(boostPayload, `50.0`, `"x"`, 1),
		"a text state":           strings.Replace(boostPayload, `10.25,0,`, `10.25,"0",`, 1),
		"a text target":          strings.Replace(boostPayload, `0.98]`, `"x"]`, 1),
		"an unknown field":       strings.Replace(boostPayload, `"rows"`, `"rowz"`, 1),
		"no rows":                `{"rows":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parity.DecodeBoost([]byte(payload)); err == nil {
				t.Errorf("%s must be refused", name)
			}
		})
	}
}

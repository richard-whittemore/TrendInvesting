// Command parity compares the Go target-weight modules with logged
// QuantConnect runs and prints the pass/fail report (ADR 0026 point 7,
// item 2; docs/research/parity-2026-10.md).
//
// Each input is a run's orders, as a JSON list of QuantConnect order
// objects; the logging templates carry their export in order tags
// (research/edge-search/momentum/parity_log.py and boost/parity_log.py),
// since the Free plan's log holds 10 KB and its object store cannot be
// written. A momentum run names its months, chosen before any comparison:
//
//	parity -momentum FU_PARITY=orders.json -months FU_PARITY=1999-01,1999-09 \
//	       -boost BX_PARITY=boost-orders.json \
//	       -sessions BX_PARITY=research/edge-search/boost/parity_plan.json
//
// and a Boost run names its plan file, {"first":"1998-01-02","last":
// "2026-06-30","sessions":7166}, committed before the run, which its export
// must cover.
//
// The remaining parity runs (docs/research/parity-2026-10.md, "Remaining
// scope, plan, 2026-10-19") are compared the same way, each kind with its
// own planned months:
//
//	parity -glide G_IS_P38_PARITY=orders.json -glide-months G_IS_P38_PARITY=2002-08..2015-12 \
//	       -lot D7L_PARITY=orders.json -lot-months D7L_PARITY=1999-03,1999-11 \
//	       -ira IRA_PARITY=orders.json -ira-months IRA_PARITY=1999-03,1999-11
//
// A months list is comma separated; an item YYYY-MM..YYYY-MM is every month
// from the first to the last, both included. Every planned month must be
// exported; a glide, D7-L or IRA run may also export months beyond them,
// which are compared too.
//
// It exits 0 when every check passes, 1 when any fails, and 2 when an
// input cannot be read or compared at all.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/glide"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// The tag prefixes of the logging templates' payloads.
const (
	momentumPrefix = "PARITY1 "
	boostPrefix    = "BPARITY1 "
	glidePrefix    = "GLIDE1 "
	lotPrefix      = "LOTPARITY1 "
	iraPrefix      = "IRAPARITY1 "
)

func main() {
	pass, err := run(os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "parity:", err)
		os.Exit(2)
	case !pass:
		os.Exit(1)
	}
}

// named collects repeated NAME=VALUE flags in order.
type named struct {
	names  []string
	values map[string]string
}

func (n *named) set(s string) error {
	name, value, ok := strings.Cut(s, "=")
	if !ok || name == "" || value == "" {
		return fmt.Errorf("%q is not NAME=VALUE", s)
	}
	if n.values == nil {
		n.values = map[string]string{}
	}
	if _, dup := n.values[name]; dup {
		return fmt.Errorf("%s named twice", name)
	}
	n.names = append(n.names, name)
	n.values[name] = value
	return nil
}

func run(args []string, stdout, stderr io.Writer) (bool, error) {
	var mom, months, bst, sess named
	kinds := []*kind{
		{flag: "glide", what: "a glide-path run", prefix: glidePrefix, compare: compareGlide},
		{flag: "lot", what: "a D7-L run", prefix: lotPrefix, compare: compareLot},
		{flag: "ira", what: "an IRA run", prefix: iraPrefix, compare: compareIRA},
	}
	flags := flag.NewFlagSet("parity", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Func("momentum", "NAME=FILE: a momentum run's orders (repeatable)", mom.set)
	flags.Func("months", "NAME=YYYY-MM,...: that run's planned months (repeatable)", months.set)
	flags.Func("boost", "NAME=FILE: a Boost run's orders (repeatable)", bst.set)
	flags.Func("sessions", "NAME=FILE: the JSON plan file of that Boost run: first, last, sessions (repeatable)", sess.set)
	for _, k := range kinds {
		flags.Func(k.flag, "NAME=FILE: "+k.what+"'s orders (repeatable)", k.runs.set)
		flags.Func(k.flag+"-months", "NAME=YYYY-MM,...: that run's planned months (repeatable)", k.months.set)
	}
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	count := len(mom.names) + len(bst.names)
	for _, k := range kinds {
		count += len(k.runs.names)
	}
	if count == 0 {
		return false, errors.New("nothing to compare: name a -momentum, -boost, -glide, -lot or -ira run")
	}
	pairs := []struct{ plans, runs *named }{{&months, &mom}, {&sess, &bst}}
	for _, k := range kinds {
		pairs = append(pairs, struct{ plans, runs *named }{&k.months, &k.runs})
	}
	for i, p := range pairs {
		for _, name := range p.plans.names {
			if _, ok := p.runs.values[name]; !ok {
				what := "months"
				if i == 1 {
					what = "sessions"
				}
				return false, fmt.Errorf("%s has %s but no export", name, what)
			}
		}
	}
	var reports []parity.Report
	var inputs []string
	for _, name := range mom.names {
		spec, ok := months.values[name]
		if !ok {
			return false, fmt.Errorf("%s has no planned months", name)
		}
		plan, err := expandMonths(spec)
		if err != nil {
			return false, fmt.Errorf("%s: months: %w", name, err)
		}
		tags, total, err := tagged(mom.values[name], momentumPrefix)
		if err != nil {
			return false, fmt.Errorf("%s: %w", name, err)
		}
		var ms []parity.MomentumMonth
		for _, tg := range tags {
			m, err := parity.DecodeMomentum([]byte(tg.payload))
			if err != nil {
				return false, fmt.Errorf("%s: order %d: %w", name, tg.id, err)
			}
			ms = append(ms, m)
		}
		inputs = append(inputs, fmt.Sprintf("- %s: %d logged months from %d tagged orders of %d", name, len(ms), len(tags), total))
		reports = append(reports, parity.CompareMomentum(momentum.FCombo(), name, plan, ms))
	}
	for _, name := range bst.names {
		spec, ok := sess.values[name]
		if !ok {
			return false, fmt.Errorf("%s has no planned sessions", name)
		}
		sp, err := readPlan(spec)
		if err != nil {
			return false, fmt.Errorf("%s: sessions plan %s: %w", name, spec, err)
		}
		tags, total, err := tagged(bst.values[name], boostPrefix)
		if err != nil {
			return false, fmt.Errorf("%s: %w", name, err)
		}
		var sessions []parity.BoostSession
		for _, tg := range tags {
			s, err := parity.DecodeBoost([]byte(tg.payload))
			if err != nil {
				return false, fmt.Errorf("%s: order %d: %w", name, tg.id, err)
			}
			sessions = append(sessions, s...)
		}
		inputs = append(inputs, fmt.Sprintf("- %s: %d sessions from %d tagged orders of %d", name, len(sessions), len(tags), total))
		reports = append(reports, parity.CompareBoost(parity.BacktestBoost(), name, sp, sessions))
	}
	for _, k := range kinds {
		for _, name := range k.runs.names {
			line, report, err := k.run(name)
			if err != nil {
				return false, fmt.Errorf("%s: %w", name, err)
			}
			inputs = append(inputs, line)
			reports = append(reports, report)
		}
	}
	r := parity.Merge(reports...)
	if _, err := fmt.Fprintf(stdout, "%s\n\n%s", strings.Join(inputs, "\n"), r.Markdown()); err != nil {
		return false, err
	}
	return r.Pass(), nil
}

// readPlan reads and validates a Boost run's plan file.
func readPlan(path string) (parity.BoostPlan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return parity.BoostPlan{}, err
	}
	return parity.DecodeBoostPlan(raw)
}

type tag struct {
	id      int
	payload string
}

// tagged reads a run's orders and returns the payloads of the tags with
// prefix, in order ID order, which is the order the run wrote them, and
// the number of orders read.
func tagged(path, prefix string) ([]tag, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	return taggedIn(raw, path, prefix)
}

// taggedIn is tagged on the orders already read from path.
func taggedIn(raw []byte, path, prefix string) ([]tag, int, error) {
	var orders []struct {
		ID  int    `json:"id"`
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(raw, &orders); err != nil {
		return nil, 0, err
	}
	var out []tag
	for _, o := range orders {
		if payload, ok := strings.CutPrefix(o.Tag, prefix); ok {
			out = append(out, tag{o.ID, payload})
		}
	}
	if len(out) == 0 {
		return nil, 0, fmt.Errorf("no tagged order in %d orders of %s", len(orders), path)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, len(orders), nil
}

// kind is one of the remaining parity exports: its flags, its tag prefix,
// and how its decoded months are compared.
type kind struct {
	flag, what, prefix string
	runs, months       named
	compare            func(name string, raw []byte, plan []string, payloads []tag) (parity.Report, int, error)
}

// run compares one named run of the kind and returns its input line.
func (k *kind) run(name string) (string, parity.Report, error) {
	spec, ok := k.months.values[name]
	if !ok {
		return "", parity.Report{}, errors.New("has no planned months")
	}
	plan, err := expandMonths(spec)
	if err != nil {
		return "", parity.Report{}, fmt.Errorf("months: %w", err)
	}
	path := k.runs.values[name]
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", parity.Report{}, err
	}
	tags, total, err := taggedIn(raw, path, k.prefix)
	if err != nil {
		return "", parity.Report{}, err
	}
	report, n, err := k.compare(name, raw, plan, tags)
	if err != nil {
		return "", parity.Report{}, err
	}
	line := fmt.Sprintf("- %s: %d logged months from %d tagged orders of %d (planned %d)", name, n, len(tags), total, len(plan))
	return line, report, nil
}

// decodeAll decodes each tag's payload with decode.
func decodeAll[T any](tags []tag, decode func([]byte) (T, error)) ([]T, error) {
	out := make([]T, 0, len(tags))
	for _, tg := range tags {
		m, err := decode([]byte(tg.payload))
		if err != nil {
			return nil, fmt.Errorf("order %d: %w", tg.id, err)
		}
		out = append(out, m)
	}
	return out, nil
}

func compareGlide(name string, _ []byte, plan []string, tags []tag) (parity.Report, int, error) {
	ms, err := decodeAll(tags, parity.DecodeGlide)
	if err != nil {
		return parity.Report{}, 0, err
	}
	return parity.CompareGlide(glide.DefaultAnchors(), name, plan, ms), len(ms), nil
}

func compareLot(name string, _ []byte, plan []string, tags []tag) (parity.Report, int, error) {
	ms, err := decodeAll(tags, parity.DecodeLot)
	if err != nil {
		return parity.Report{}, 0, err
	}
	return parity.CompareMinimumLot(momentum.FCombo(), name, plan, ms), len(ms), nil
}

func compareIRA(name string, raw []byte, plan []string, tags []tag) (parity.Report, int, error) {
	ms, err := decodeAll(tags, parity.DecodeIRA)
	if err != nil {
		return parity.Report{}, 0, err
	}
	orders, err := parity.DecodeOrders(raw)
	if err != nil {
		return parity.Report{}, 0, fmt.Errorf("orders: %w", err)
	}
	return parity.CompareIRA(name, plan, ms, orders), len(ms), nil
}

// expandMonths reads a months list: YYYY-MM items, comma separated, and
// YYYY-MM..YYYY-MM ranges, both ends included.
func expandMonths(spec string) ([]string, error) {
	var out []string
	for _, item := range strings.Split(spec, ",") {
		first, last, isRange := strings.Cut(item, "..")
		a, err := time.Parse("2006-01", first)
		if err != nil || a.Format("2006-01") != first {
			return nil, fmt.Errorf("%q is not a YYYY-MM month", first)
		}
		if !isRange {
			out = append(out, first)
			continue
		}
		b, err := time.Parse("2006-01", last)
		if err != nil || b.Format("2006-01") != last || b.Before(a) {
			return nil, fmt.Errorf("%q is not a month on or after %s", last, first)
		}
		for m := a; !m.After(b); m = m.AddDate(0, 1, 0) {
			out = append(out, m.Format("2006-01"))
		}
	}
	return out, nil
}

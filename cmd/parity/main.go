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
//	       -boost BX_PARITY=boost-orders.json
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

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// The tag prefixes of the logging templates' payloads.
const (
	momentumPrefix = "PARITY1 "
	boostPrefix    = "BPARITY1 "
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
	var mom, months, bst named
	flags := flag.NewFlagSet("parity", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Func("momentum", "NAME=FILE: a momentum run's orders (repeatable)", mom.set)
	flags.Func("months", "NAME=YYYY-MM,...: that run's planned months (repeatable)", months.set)
	flags.Func("boost", "NAME=FILE: a Boost run's orders (repeatable)", bst.set)
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if len(mom.names)+len(bst.names) == 0 {
		return false, errors.New("nothing to compare: name a -momentum or -boost run")
	}
	for _, name := range months.names {
		if _, ok := mom.values[name]; !ok {
			return false, fmt.Errorf("%s has months but no export", name)
		}
	}
	var reports []parity.Report
	var inputs []string
	for _, name := range mom.names {
		plan, ok := months.values[name]
		if !ok {
			return false, fmt.Errorf("%s has no planned months", name)
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
		reports = append(reports, parity.CompareMomentum(momentum.FCombo(), name, strings.Split(plan, ","), ms))
	}
	for _, name := range bst.names {
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
		reports = append(reports, parity.CompareBoost(parity.BacktestBoost(), name, sessions))
	}
	r := parity.Merge(reports...)
	if _, err := fmt.Fprintf(stdout, "%s\n\n%s", strings.Join(inputs, "\n"), r.Markdown()); err != nil {
		return false, err
	}
	return r.Pass(), nil
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

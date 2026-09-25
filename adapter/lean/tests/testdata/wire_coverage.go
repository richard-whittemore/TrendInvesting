// Command wire_coverage fails if internal/event defines an event type (at any
// schema version) that this file's own lists do not account for, in either
// direction: a new type with no adapter fixture, or a stale list entry
// internal/event no longer defines. It is issue #31's "test that fails if a
// Go event type or schema version exists that the fixtures don't cover":
// internal/event has no runtime registry of its own event types, so this
// program is the explicit list ADR 0015 and the ticket ask for, and it stays
// in sync with the real constants by reading them out of source (below)
// rather than restating them by hand.
//
// discoverEventTypes parses every internal/event/*.go file (excluding
// _test.go) for the two constant shapes every event type in that package
// declares — `const XxxEventType = "..."` and
// `const XxxSchemaVersion uint32 = N` — and pairs them by their shared Xxx
// prefix. That is a parse of the actual source, not a second, independently
// maintained inventory: bump a schema version or add a new event type and
// this program's own view of "what internal/event defines" moves with it
// automatically. What still has to be maintained by hand is classification —
// which of those types the LEAN adapter's fixtures are meant to cover, and
// which deliberately do not cross the boundary yet — and that is exactly the
// two lists below.
package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// eventTypeConstRe matches `const XxxEventType = "yyy"` (every event type in
// internal/event declares its type string this way; see the package's own
// files for the convention).
var eventTypeConstRe = regexp.MustCompile(`(?m)^const (\w+)EventType\s*=\s*"([^"]+)"`)

// schemaVersionConstRe matches `const XxxSchemaVersion uint32 = N`.
var schemaVersionConstRe = regexp.MustCompile(`(?m)^const (\w+)SchemaVersion uint32 = (\d+)`)

// wireCovered is every event type this build's LEAN adapter fixtures are
// meant to exercise — sent by the adapter, acted on when received, or
// received and deliberately ignored (the bookkeeping and diagnostic
// decisions orders.py's OrderDesk.act does not act on) — mapped to what
// covers it. Adding an event type to internal/event, or bumping one's schema
// version, requires adding or updating its entry here and adding the fixture
// it names; discoverEventTypes fails closed otherwise.
var wireCovered = map[string]string{
	// Inputs the adapter sends.
	"account.snapshot":          "snapshot_contract.go (test_publisher.py)",
	"adapter.run.stopped":       "run_stopped_contract.go (test_publisher.py)",
	"market.bar.completed":      "bar_contract.go (test_publisher.py)",
	"market.session.closed":     "session_contract.go (test_publisher.py)",
	"execution.fill":            "execution_contract.go (test_orders.py InputContractTests)",
	"execution.order.lifecycle": "execution_contract.go (test_orders.py InputContractTests)",
	"replay.run.completed":      "run_completed_contract.go (test_publisher.py)",
	// Decisions orders.py acts on (SCHEMA_VERSIONS).
	"strategy.trade.proposed":         "order_decisions_contract.go (test_orders.py FixtureContractTests)",
	"strategy.add.proposed":           "order_decisions_contract.go (test_orders.py FixtureContractTests)",
	"strategy.proposal.expired":       "order_decisions_contract.go (test_orders.py FixtureContractTests)",
	"strategy.campaign.opened":        "order_decisions_contract.go (test_orders.py FixtureContractTests)",
	"strategy.exit-order.set":         "order_decisions_contract.go (test_orders.py FixtureContractTests)",
	"strategy.exit.proposed":          "order_decisions_contract.go (test_orders.py FixtureContractTests)",
	"strategy.campaign.unit-added":    "order_decisions_contract.go (test_orders.py FixtureContractTests)",
	"strategy.campaign.units-stopped": "order_decisions_contract.go (test_orders.py FixtureContractTests)",
	"strategy.campaign.exited":        "order_decisions_contract.go (test_orders.py FixtureContractTests)",
	// Decisions the adapter receives but never acts on (README.md's decision
	// table; ADR 0022's discipline applied to the adapter's own boundary).
	"strategy.campaign.evaluated":             "order_decisions_contract.go (test_orders.py IgnoredDecisionTests)",
	"strategy.drawdown-step.applied":          "order_decisions_contract.go (test_orders.py IgnoredDecisionTests)",
	"strategy.notional-account.cash-adjusted": "order_decisions_contract.go (test_orders.py IgnoredDecisionTests)",
	"strategy.notional-account.rebased":       "order_decisions_contract.go (test_orders.py IgnoredDecisionTests)",
	"strategy.notional-account.recovered":     "order_decisions_contract.go (test_orders.py IgnoredDecisionTests)",
	"strategy.proposal.declined":              "order_decisions_contract.go (test_orders.py IgnoredDecisionTests)",
	"strategy.engine.state":                   "order_decisions_contract.go (test_orders.py IgnoredDecisionTests)",
	"strategy.setup.evaluated":                "order_decisions_contract.go (test_orders.py IgnoredDecisionTests)",
	"strategy.signal":                         "order_decisions_contract.go (test_orders.py IgnoredDecisionTests)",
	"strategy.protective-stop.set":            "order_decisions_contract.go (test_orders.py IgnoredDecisionTests)",
}

// notYetCrossed is every event type internal/event defines that deliberately
// does not cross the LEAN adapter boundary yet, with the reason a fixture
// would be fiction rather than evidence. Each entry needs an adapter fixture,
// moved out of this list, the day the cited work lands.
var notYetCrossed = map[string]string{
	// cmd/engine composes and delivers its own configuration input before the
	// socket opens (cmd/engine/engine.go's configurationEnvelope); the
	// adapter never sends or receives one.
	"strategy.configuration": "cmd/engine/engine.go's configurationEnvelope, not the adapter",
	// adapter/lean/README.md's own "will still need to" list.
	"account.cash-movement":   "adapter/lean/README.md: \"send account.cash-movement events\" (outside #158)",
	"market.corporate-action": "adapter/lean/README.md: \"normalize... corporate actions... into versioned messages\"",
}

func main() {
	discovered, err := discoverEventTypes("internal/event")
	must(err)

	var problems []string
	for eventType := range discovered {
		_, covered := wireCovered[eventType]
		_, excused := notYetCrossed[eventType]
		if !covered && !excused {
			problems = append(problems, fmt.Sprintf(
				"internal/event defines %q (schema %d) but no LEAN adapter fixture covers it and it is not "+
					"in wire_coverage.go's notYetCrossed list; add a fixture (extend an existing "+
					"tests/testdata/*_contract.go where the shape fits, per adapter/lean/tests/testdata's own "+
					"pattern) and add it to wireCovered, or add it to notYetCrossed with why not",
				eventType, discovered[eventType]))
		}
		if covered && excused {
			problems = append(problems, fmt.Sprintf(
				"%q is listed in both wireCovered and notYetCrossed; it can only be one", eventType))
		}
	}
	for eventType, coverage := range wireCovered {
		if _, ok := discovered[eventType]; !ok {
			problems = append(problems, fmt.Sprintf(
				"wireCovered names %q (%s), which internal/event no longer defines; remove it", eventType, coverage))
		}
	}
	for eventType, reason := range notYetCrossed {
		if _, ok := discovered[eventType]; !ok {
			problems = append(problems, fmt.Sprintf(
				"notYetCrossed names %q (%s), which internal/event no longer defines; remove it", eventType, reason))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "wire_coverage: "+p)
		}
		os.Exit(1)
	}
	fmt.Printf("wire_coverage: %d event types, all covered or excused\n", len(discovered))
}

// discoverEventTypes reads every *.go file directly in dir, except *_test.go,
// and returns every type-string -> schema-version pair it can pair up by a
// shared constant-name prefix. It fails closed if a file cannot be read, an
// EventType constant has no matching SchemaVersion constant or vice versa
// (internal/event's own convention, unbroken as of this writing), or two
// constants claim the same event type string.
func discoverEventTypes(dir string) (map[string]uint32, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wire_coverage: read %s: %w", dir, err)
	}

	types := map[string]string{}    // prefix -> event type string
	versions := map[string]uint32{} // prefix -> schema version
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		contents, err := os.ReadFile(dir + "/" + name)
		if err != nil {
			return nil, fmt.Errorf("wire_coverage: read %s: %w", name, err)
		}
		for _, m := range eventTypeConstRe.FindAllStringSubmatch(string(contents), -1) {
			types[m[1]] = m[2]
		}
		for _, m := range schemaVersionConstRe.FindAllStringSubmatch(string(contents), -1) {
			version, err := strconv.ParseUint(m[2], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("wire_coverage: %s: schema version %q for %sSchemaVersion: %w", name, m[2], m[1], err)
			}
			versions[m[1]] = uint32(version)
		}
	}

	discovered := map[string]uint32{}
	for prefix, eventType := range types {
		version, ok := versions[prefix]
		if !ok {
			return nil, fmt.Errorf("wire_coverage: %sEventType = %q has no matching %sSchemaVersion constant", prefix, eventType, prefix)
		}
		if existing, dup := discovered[eventType]; dup {
			return nil, fmt.Errorf("wire_coverage: event type %q is declared by more than one constant (schema versions %d and %d)", eventType, existing, version)
		}
		discovered[eventType] = version
	}
	for prefix := range versions {
		if _, ok := types[prefix]; !ok {
			return nil, fmt.Errorf("wire_coverage: %sSchemaVersion has no matching %sEventType constant", prefix, prefix)
		}
	}
	return discovered, nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

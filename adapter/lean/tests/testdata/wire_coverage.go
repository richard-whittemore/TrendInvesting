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
// _test.go) with go/parser and go/ast for the two declaration shapes every
// event type in that package declares — `XxxEventType` and
// `XxxSchemaVersion`, const or var, singly or grouped inside `const ( ... )`
// — and pairs them by their shared Xxx prefix. That is a parse of the
// actual source, not a second, independently maintained inventory: bump a
// schema version or add a new event type and this program's own view of
// "what internal/event defines" moves with it automatically. What still has
// to be maintained by hand is classification — which of those types the
// LEAN adapter's fixtures are meant to cover, and which deliberately do not
// cross the boundary yet — and that is exactly the two lists below.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
)

// minimumDiscoveredEventTypes is a floor on how many event types
// discoverEventTypes should ever find in internal/event. Below it almost
// certainly means the parser regressed — it skipped a file, a grouped
// const/var block, or otherwise missed a declaration it should have read —
// rather than that internal/event genuinely shrank to fewer types than it
// has ever had. It is the count as of this writing (see wire_coverage's own
// "N event types" success line); raise it if internal/event genuinely grows,
// but a value observed to DROP below it is a parser bug to fix first, not a
// classification list to edit.
const minimumDiscoveredEventTypes = 32

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
	"market.corporate-action":   "corporate_action_contract.go (test_orders.py CashInLieuTests)",
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
	"strategy.campaign.cash-in-lieu":  "order_decisions_contract.go (test_orders.py FixtureContractTests, CashInLieuTests)",
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
	"account.cash-movement": "adapter/lean/README.md: \"send account.cash-movement events\" (outside #158)",
	// ADR 0009's universe port is fixture-backed only so far: a
	// provider-backed implementation, which would need the LEAN adapter to
	// actually send this fact, is separate, later work. A fixture here would
	// be fiction rather than evidence of anything the adapter does today.
	"market.instrument-classification": "no provider-backed universe port yet; the adapter never sends this fact",
	// The adapter never acts on this decision (SCHEMA_VERSIONS omits it, like
	// every other decision orders.py ignores), but a fixture proving that
	// would need the same provider-backed wiring the classification fact
	// above does, so it stays here alongside it rather than moving to
	// wireCovered's "ignored decision" group on its own.
	"strategy.universe.eligibility": "no provider-backed universe port yet; the adapter never receives this decision",
}

func main() {
	discovered, err := discoverEventTypes("internal/event")
	must(err)

	var problems []string
	if len(discovered) < minimumDiscoveredEventTypes {
		problems = append(problems, fmt.Sprintf(
			"found only %d event type(s) in internal/event, fewer than the %d this build expects "+
				"(minimumDiscoveredEventTypes); that almost always means discoverEventTypes itself "+
				"regressed -- missed a file, a grouped const/var block, or a declaration shape -- "+
				"not that internal/event shrank; fix the parser before touching the coverage lists below",
			len(discovered), minimumDiscoveredEventTypes))
	}
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

// discoverEventTypes parses every *.go file directly in dir, except
// *_test.go, with go/parser and go/ast, and returns every type-string ->
// schema-version pair it can pair up by a shared declaration-name prefix.
//
// It walks each file's top-level const and var declarations
// (ast.GenDecl.Specs holds one *ast.ValueSpec per name=value line whether or
// not the declaration groups several such lines inside `const ( ... )` or
// `var ( ... )`, so this one loop reads `const Foo = "x"` and
// `const ( Foo = "x"; Bar uint32 = 1 )` identically), and for every declared
// name ending in "EventType" or "SchemaVersion" reads the literal string or
// integer it is assigned — a name's own declared type (a typed string
// constant, say) is irrelevant here; only the literal value is. It fails
// closed if a file cannot be parsed, a name in that shape is assigned
// something other than a literal (this package computes no event type or
// schema version from an expression, as of this writing, so failing rather
// than guessing is correct), an EventType has no matching SchemaVersion or
// vice versa, or two constants claim the same event type string.
func discoverEventTypes(dir string) (map[string]uint32, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wire_coverage: read %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	types := map[string]string{}    // prefix -> event type string
	versions := map[string]uint32{} // prefix -> schema version

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := dir + "/" + name
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("wire_coverage: parse %s: %w", path, err)
		}
		if err := collectEventDecls(path, file, types, versions); err != nil {
			return nil, err
		}
	}

	discovered := map[string]uint32{}
	for prefix, eventType := range types {
		version, ok := versions[prefix]
		if !ok {
			return nil, fmt.Errorf("wire_coverage: %sEventType = %q has no matching %sSchemaVersion declaration", prefix, eventType, prefix)
		}
		if existing, dup := discovered[eventType]; dup {
			return nil, fmt.Errorf("wire_coverage: event type %q is declared by more than one name (schema versions %d and %d)", eventType, existing, version)
		}
		discovered[eventType] = version
	}
	for prefix := range versions {
		if _, ok := types[prefix]; !ok {
			return nil, fmt.Errorf("wire_coverage: %sSchemaVersion has no matching %sEventType declaration", prefix, prefix)
		}
	}
	return discovered, nil
}

// collectEventDecls walks file's top-level const and var declarations and
// records every XxxEventType's string literal into types, and every
// XxxSchemaVersion's integer literal into versions, keyed by the shared
// prefix Xxx. Grouped and single declarations are the same shape in the AST
// (see discoverEventTypes's own doc comment), so one loop over
// genDecl.Specs handles both.
func collectEventDecls(path string, file *ast.File, types map[string]string, versions map[string]uint32) error {
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || (genDecl.Tok != token.CONST && genDecl.Tok != token.VAR) {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range valueSpec.Names {
				var prefix, kind string
				switch {
				case strings.HasSuffix(ident.Name, "EventType"):
					prefix, kind = strings.TrimSuffix(ident.Name, "EventType"), "EventType"
				case strings.HasSuffix(ident.Name, "SchemaVersion"):
					prefix, kind = strings.TrimSuffix(ident.Name, "SchemaVersion"), "SchemaVersion"
				default:
					continue
				}
				// A name repeating an earlier ValueSpec's value inside an
				// iota-style block (const A = iota; B; C, where B and C
				// carry no Values of their own) has nothing to read here.
				// No EventType or SchemaVersion is declared that way as of
				// this writing, so such a name is refused with an error
				// (fail closed) rather than skipped or indexed out of range:
				// an iota-style event constant is never silently accepted.
				if i >= len(valueSpec.Values) {
					return fmt.Errorf("wire_coverage: %s: %s%s has no literal value of its own (an iota-style repeated value?); this package expects every event type and schema version to be its own literal", path, prefix, kind)
				}
				lit, ok := valueSpec.Values[i].(*ast.BasicLit)
				if !ok {
					return fmt.Errorf("wire_coverage: %s: %s%s is not a literal (%T); this package expects every event type and schema version to be a literal string or integer, not a computed expression", path, prefix, kind, valueSpec.Values[i])
				}
				switch kind {
				case "EventType":
					if lit.Kind != token.STRING {
						return fmt.Errorf("wire_coverage: %s: %sEventType is not a string literal: %s", path, prefix, lit.Value)
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						return fmt.Errorf("wire_coverage: %s: %sEventType %s: %w", path, prefix, lit.Value, err)
					}
					types[prefix] = value
				case "SchemaVersion":
					if lit.Kind != token.INT {
						return fmt.Errorf("wire_coverage: %s: %sSchemaVersion is not an integer literal: %s", path, prefix, lit.Value)
					}
					version, err := strconv.ParseUint(lit.Value, 10, 32)
					if err != nil {
						return fmt.Errorf("wire_coverage: %s: %sSchemaVersion %s: %w", path, prefix, lit.Value, err)
					}
					versions[prefix] = uint32(version)
				}
			}
		}
	}
	return nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Command configuration_identity prints the run identity cmd/engine derives
// for a configuration: its hash (ADR 0016), then the strategy version for the
// build named by the second argument. The end-to-end test uses it to write
// run.json settings the engine will accept.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

func main() {
	raw, err := os.ReadFile(os.Args[1])
	must(err)
	var cfg event.ConfigurationPayload
	must(json.Unmarshal(raw, &cfg))
	must(cfg.Validate())
	fmt.Println(event.ConfigurationHash(cfg))
	fmt.Println(event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, os.Args[2]))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

package restart

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// settlementKeys and retirementKeys are the keys every line of the adapter's
// settlement file states, none of them left out: a key left out is refused
// rather than read as its zero value (fail closed, AGENTS.md).
var (
	settlementKeys = []string{"evidence", "input_id", "log", "retirement", "run"}
	retirementKeys = []string{"account_id", "client_order_id", "detail", "instrument_id", "limit_price", "quantity", "reason", "retired_at", "retired_by", "side"}
)

// ReadSettlements reads the IBKR adapter's settlements of orders an ended
// run released and the adapter never sent (event.NeverSentSettlement), one
// JSON object a line as the relay's settle-ended-run command writes them;
// blank lines are skipped. A line that is not one object stating every key,
// none unknown, refuses, naming fileName and the line, rather than being
// read as no settlement (fail closed, AGENTS.md). Each line's own checks,
// and its match against the approved log, are ReadEndState's.
func ReadSettlements(raw []byte, fileName string) ([]event.NeverSentSettlement, error) {
	var settlements []event.NeverSentSettlement
	for n, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		s, err := readSettlement(line)
		if err != nil {
			return nil, fmt.Errorf("restart: %s line %d is not a settlement as the adapter writes one: %w", fileName, n+1, err)
		}
		settlements = append(settlements, s)
	}
	return settlements, nil
}

// readSettlement decodes one line strictly: one JSON object (json.Unmarshal
// refuses anything after it), every key stated, none unknown.
func readSettlement(line []byte) (event.NeverSentSettlement, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(line, &keys); err != nil {
		return event.NeverSentSettlement{}, err
	}
	if err := stated(keys, settlementKeys, ""); err != nil {
		return event.NeverSentSettlement{}, err
	}
	var retirement map[string]json.RawMessage
	if err := json.Unmarshal(keys["retirement"], &retirement); err != nil || retirement == nil {
		return event.NeverSentSettlement{}, fmt.Errorf("its retirement is not an object")
	}
	if err := stated(retirement, retirementKeys, "retirement."); err != nil {
		return event.NeverSentSettlement{}, err
	}
	var s event.NeverSentSettlement
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return event.NeverSentSettlement{}, err
	}
	return s, nil
}

// stated refuses keys that leave out any of want, or state one it does not
// name, prefix naming where they sit.
func stated(keys map[string]json.RawMessage, want []string, prefix string) error {
	var missing, unknown []string
	for _, key := range want {
		if _, ok := keys[key]; !ok {
			missing = append(missing, prefix+key)
		}
	}
	for key := range keys {
		if !slices.Contains(want, key) {
			unknown = append(unknown, prefix+key)
		}
	}
	slices.Sort(unknown)
	switch {
	case len(missing) > 0:
		return fmt.Errorf("it states no %s; every key is required", strings.Join(missing, ", "))
	case len(unknown) > 0:
		return fmt.Errorf("it states %s, which a settlement has not", strings.Join(unknown, ", "))
	}
	return nil
}

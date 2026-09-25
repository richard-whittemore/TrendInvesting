package registry

import (
	"fmt"
	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// WiderTotalLongCap declares ADR 0008's two total-long cap ablations under
// stable ADR 0012 identities. Apart from StrategyID, only MaxUnitsTotalLong
// changes; the caller supplies the common control, including fixture settings.
func WiderTotalLongCap(baseline event.ConfigurationPayload, variant string) (event.ConfigurationPayload, error) {
	switch variant {
	case "total-long-cap-24":
		baseline.MaxUnitsTotalLong = 24
	case "total-long-cap-36":
		baseline.MaxUnitsTotalLong = 36
	default:
		return event.ConfigurationPayload{}, fmt.Errorf("registry: undeclared total-long cap Variant %q", variant)
	}
	baseline.StrategyID = variant
	return baseline, nil
}

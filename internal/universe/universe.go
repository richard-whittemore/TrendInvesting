// Package universe implements the Baseline universe (CONTEXT.md: "Universe",
// "Eligible"; ADR 0009): the set of instruments the strategy may open a
// Campaign in on a given date, decided from declared, point-in-time
// criteria. It depends only on internal/indicator, matching that package's
// own "pure, side-effect-free" discipline (docs/development.md); it knows
// nothing of events, replay, or the reducer.
//
// Port is the seam a market-data provider implements; FixtureUniverse is
// today's synthetic-data implementation (a provider-backed one is separate,
// later work). Evaluate is the pure decision ADR 0009 states, reusing
// indicator.MedianDollarVolume — the one definition this criterion and ADR
// 0010's ranking tie-break both read — rather than a second implementation
// of the same measure.
package universe

// SecurityType classifies an instrument's own listing (ADR 0009: only common
// stock is eligible; an ETF, an ADR, or a SPAC never is, regardless of price
// or volume).
type SecurityType string

// The four SecurityType values ADR 0009's classification criterion
// distinguishes. Only SecurityTypeCommonStock can ever be eligible; the
// other three name the excluded instrument classes explicitly, rather than
// leaving "not common stock" as a single unnamed catch-all, so a fixture (or
// a future provider) states which exclusion applies and a reader of an
// eligibility decision can see why.
const (
	SecurityTypeCommonStock SecurityType = "common_stock"
	SecurityTypeETF         SecurityType = "etf"
	SecurityTypeADR         SecurityType = "adr"
	SecurityTypeSPAC        SecurityType = "spac"
)

// Classification is the point-in-time, declared fact about an instrument's
// own listing that ADR 0009's first criterion reads: whether it is common
// stock, and whether its primary listing is a US exchange. Neither figure is
// derived from price or volume history (see Input's own fields for those).
//
// The zero value is never a legitimate classification — SecurityType is
// empty, which CommonStockOnUSPrimaryExchange reports as ineligible — so an
// instrument this run's Port has simply never classified fails this
// criterion by construction, rather than needing a separate "unknown" case.
type Classification struct {
	SecurityType      SecurityType
	USPrimaryExchange bool
}

// CommonStockOnUSPrimaryExchange reports whether c satisfies ADR 0009's
// classification criterion: common stock on a US primary exchange, no ETFs,
// ADRs, or SPACs.
func (c Classification) CommonStockOnUSPrimaryExchange() bool {
	return c.SecurityType == SecurityTypeCommonStock && c.USPrimaryExchange
}

// Port supplies an instrument's point-in-time Classification: the seam a
// market-data provider implements. FixtureUniverse is the Baseline's
// synthetic-data implementation, requiring no purchased data; a
// provider-backed implementation is separate, later work and can replace it
// without any caller of Port changing.
type Port interface {
	// Classify reports instrumentID's declared Classification, and whether
	// this Port has ever classified it at all. A false ok means "not
	// declared," never "declared ineligible" — the caller decides what that
	// means (ADR 0009: an unclassified instrument is not evaluated as a
	// universe candidate at all, rather than being asserted ineligible).
	Classify(instrumentID string) (classification Classification, ok bool)
}

// FixtureUniverse is a Port backed by a fixed, in-memory table of
// classifications: the fixture-backed implementation ADR 0009 asks for,
// with no network access and no purchased data (AGENTS.md rule 5; this
// ticket's own scope). It never changes after construction, so a classified
// instrument's Classification is the same for every Classify call across a
// run — appropriate for synthetic test data, where a security's own type and
// listing are declared once and do not move mid-fixture.
type FixtureUniverse struct {
	classifications map[string]Classification
}

// NewFixtureUniverse returns a FixtureUniverse over a copy of
// classifications, keyed by instrument ID. Copying means the caller's map
// can be reused or mutated afterwards without affecting what this Port
// reports.
func NewFixtureUniverse(classifications map[string]Classification) *FixtureUniverse {
	table := make(map[string]Classification, len(classifications))
	for id, c := range classifications {
		table[id] = c
	}
	return &FixtureUniverse{classifications: table}
}

// Classify implements Port.
func (f *FixtureUniverse) Classify(instrumentID string) (Classification, bool) {
	c, ok := f.classifications[instrumentID]
	return c, ok
}

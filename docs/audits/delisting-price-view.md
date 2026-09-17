# Delisting fixture price basis

## Findings

The original differing-view fixture changed raw / split-adjusted OHLC from
1 at entry to 2 at exit without a corporate action. It did not establish a
consistent Campaign basis. The corrected fixture holds this ratio at 2 for
every bar, including warm-up and entry, and asserts that invariant.

The entry fill remains 133 shares at 201.25 in a fixed adjusted basis. Under
[ADR 0004](../adr/0004-split-adjusted-signals-raw-accounting.md), the expected
exit is 155 and the result is `133 × (155 − 201.25) = −6151.25`. Reading the
raw close of 310 instead yields `+14463.75`. These are view-selection checks,
not evidence of a simulated split or a vendor's adjustment anchor.

A split event that reconciles an open Campaign's price and quantity is not
expressible today:
`CorporateActionPayload.Validate` accepts only `delisting`, with no split
ratio or position-reconciliation event. Price-view labels alone do not
establish a stable normalization basis. Corporate-action handling and the
adapter contract remain tracked in issues
[#38](https://github.com/richard-whittemore/TrendInvesting/issues/38) and
[#28](https://github.com/richard-whittemore/TrendInvesting/issues/28).

## Work log and testing evidence

- Added the OHLC factor invariant first; the original first warm-up bar failed.
- Applied factor 2 throughout the fixture; the focused test passed.
- Mutating `previousClose` to raw throughout also broke entry reconciliation.
  Restricting that mutation to the open Campaign isolated the exit assertions:
  both failed with price 310 and result +14463.75. Mutations were removed.
- `make check` passed: both audits, race tests, 95.0% coverage, lint,
  vulnerability scan (no affected code) and build. Caches were redirected
  into the worktree; transport tests required sandbox access to Unix sockets.
- Production code and all golden journals are unchanged.

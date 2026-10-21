package risklimit

import "github.com/richard-whittemore/TrendInvesting/internal/reconcile"

// ClassifiedStandsFor exposes classifiedStandsFor so a test can pin the
// standing window itself, not only the journalling it usually leads to.
func (g *Gate) ClassifiedStandsFor(b reconcile.Book) bool { return g.classifiedStandsFor(b) }

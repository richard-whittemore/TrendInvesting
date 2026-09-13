// Package coverageaudit contains no production code. It exists for one
// test, which holds every statement in internal/ to a single rule: it is
// either executed by a test, or listed in exclusions.json with a named
// reason.
//
// The rule exists because a coverage percentage cannot detect a branch that
// has become unreachable. Dead code raises the denominator and moves the
// score by a fraction of a point, so a defect that makes a path impossible
// to execute — and with it any second defect sitting on that path — passes
// a coverage floor unremarked. Comparing the SET of uncovered blocks against
// a list that a person had to write a reason into is the check that does
// notice.
package coverageaudit

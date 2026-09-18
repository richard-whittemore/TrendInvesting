#!/usr/bin/env bash
#
# Refuse an edit to a rule-surface row that a released RulesVersion already
# owns.
#
# strategy.RuleSurfaceFingerprints records the declared rule surface each
# RulesVersion was built against. The Go test beside it compares today's
# source against the row for today's version, which catches a rule change
# that forgot to move the version. It cannot catch the other direction: a
# row for a version already on main can be rewritten, and the test then
# passes with the version unmoved.
#
# No value checked into the tree can prevent its own rewriting, so the check
# is made against history instead of against the tree. A row is evidence of
# what a released version decided, and ADRs 0017 and 0018 already say
# recorded evidence is appended, never overwritten. This is that rule applied
# to the one file where it was still only a convention.
#
# Appending a row is always allowed. Removing or altering one that main
# already carries is not.
set -euo pipefail

FILE="internal/strategy/rules_version.go"
BASE="${1:-origin/main}"

rows() {
	# One "version<TAB>fingerprint" line per recorded row. gofmt keeps these
	# on their own lines, so a line-wise read is stable; anything it cannot
	# parse simply does not appear, and a row that disappears is reported
	# below rather than passing quietly.
	sed -n 's/^[[:space:]]*"\([0-9][^"]*\)":[[:space:]]*"\([0-9a-f]\{64\}\)",\{0,1\}[[:space:]]*$/\1\t\2/p'
}

if ! git cat-file -e "$BASE:$FILE" 2>/dev/null; then
	echo "rule-surface-append-only: $BASE has no $FILE yet; nothing to compare against."
	exit 0
fi

before="$(git show "$BASE:$FILE" | rows)"
after="$(rows < "$FILE")"

status=0
while IFS=$'\t' read -r version fingerprint; do
	[ -n "$version" ] || continue
	now="$(printf '%s\n' "$after" | awk -F'\t' -v v="$version" '$1 == v { print $2 }')"
	if [ -z "$now" ]; then
		echo "rule-surface-append-only: $FILE no longer records a rule surface for RulesVersion $version."
		echo "  It is on $BASE and has been removed. A released version's recorded surface is evidence of"
		echo "  what that version decided; restore the row and append a new one instead (ADR 0016, ADR 0018)."
		status=1
	elif [ "$now" != "$fingerprint" ]; then
		echo "rule-surface-append-only: the rule surface recorded for RulesVersion $version has been changed."
		echo "  on $BASE: $fingerprint"
		echo "  here:    $now"
		echo "  That row states what the rule surface WAS when $version was released, so rewriting it makes"
		echo "  two different sets of rules claim one version. If a rule changed, bump RulesVersion and"
		echo "  APPEND a row for the new version, leaving this one as it stands (ADR 0016, ADR 0018)."
		status=1
	fi
done <<< "$before"

if [ "$status" -eq 0 ]; then
	echo "rule-surface-append-only: every rule surface $BASE records is unchanged here."
fi
exit "$status"

#!/bin/sh

set -eu

profile=${1:-coverage.out}
minimum=${COVERAGE_MIN:-80.0}

if ! test -s "$profile"; then
  echo "coverage profile is missing or empty: $profile" >&2
  exit 1
fi

total=$(go tool cover -func="$profile" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')

if test -z "$total"; then
  echo "could not determine total coverage from: $profile" >&2
  exit 1
fi

echo "total coverage: ${total}% (minimum: ${minimum}%)"

awk -v total="$total" -v minimum="$minimum" 'BEGIN {
  if ((total + 0) < (minimum + 0)) {
    printf "coverage %.1f%% is below required %.1f%%\n", total, minimum > "/dev/stderr"
    exit 1
  }
}'

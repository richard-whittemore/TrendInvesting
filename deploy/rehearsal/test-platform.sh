#!/bin/bash
# Tests for deploy/rehearsal/platform.sh, the mapping from the container's
# platform to the architecture the rehearsal builds its Go tools for.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=/dev/null
. "$here/platform.sh"
# The caller's environment must not decide any case: set only inside the
# precedence cases below.
unset TI_REHEARSAL_PLATFORM DOCKER_DEFAULT_PLATFORM

failures=0
expect() { # description expected actual
  if test "$2" != "$3"; then
    echo "FAIL: $1: want '$2', got '$3'" >&2
    failures=$((failures + 1))
  fi
}
# A stand-in for `docker info --format {{.Architecture}}`.
docker_arch() { echo "${FAKE_DOCKER_ARCH:-aarch64}"; }

expect "amd64 maps to amd64" amd64 "$(rehearsal_goarch linux/amd64)"
expect "arm64 maps to arm64" arm64 "$(rehearsal_goarch linux/arm64)"
expect "variant is ignored" arm64 "$(rehearsal_goarch linux/arm64/v8)"
expect "x86_64 maps to amd64" amd64 "$(rehearsal_goarch linux/x86_64)"
if rehearsal_goarch linux/riscv64 2>/dev/null; then
  expect "unknown platform is refused" refused accepted
fi

expect "explicit platform wins" linux/amd64 \
  "$(TI_REHEARSAL_PLATFORM=linux/amd64 DOCKER_DEFAULT_PLATFORM=linux/arm64 rehearsal_platform)"
expect "DOCKER_DEFAULT_PLATFORM is honoured" linux/amd64 \
  "$(DOCKER_DEFAULT_PLATFORM=linux/amd64 rehearsal_platform)"
expect "daemon architecture is the fallback" arm64 \
  "$(rehearsal_goarch "$(FAKE_DOCKER_ARCH=aarch64 rehearsal_platform)")"
expect "daemon x86_64 is the fallback" amd64 \
  "$(rehearsal_goarch "$(FAKE_DOCKER_ARCH=x86_64 rehearsal_platform)")"

# The check that the tools and the container agree.
rehearsal_arch_matches amd64 linux/amd64 || expect "matching arch accepted" ok refused
if rehearsal_arch_matches arm64 amd64 2>/dev/null; then
  expect "mismatched arch refused" refused accepted
fi
rehearsal_arch_matches arm64 arm64 || expect "image arch arm64 accepted" ok refused

test "$failures" -eq 0 || exit 1
echo "platform tests ok"

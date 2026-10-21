#!/bin/bash
# Tests for ensure_operator_user in deploy/provision.sh: ti-operator, the user
# that writes the owner's instructions, has the engine's group, ti-trader, as
# its primary group, whether the step creates it or finds it (ADR 0026,
# amendment of 2026-10-18, "the operator inbox has a writer of its own").
# id, useradd and usermod are stand-ins on PATH.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for v in $(compgen -e | grep -E '^FAKE_' || true); do unset "$v"; done
failures=0
expect() { # description expected actual
  if test "$2" != "$3"; then
    echo "FAIL: $1: want '$2', got '$3'" >&2
    failures=$((failures + 1))
  fi
}

stubs="$work/bin"
mkdir -p "$stubs"
# The user's primary group is in $FAKE_STATE (no file: no such user).
cat >"$stubs/id" <<'STUB'
#!/bin/bash
test -f "$FAKE_STATE" || exit 1
case $1 in -gn) cat "$FAKE_STATE" ;; *) echo "uid=999(ti-operator)" ;; esac
STUB
cat >"$stubs/useradd" <<'STUB'
#!/bin/bash
echo "useradd $*" >>"$FAKE_LOG"
while test $# -gt 0; do case $1 in --gid | -g) echo "$2" >"$FAKE_STATE"; shift ;; esac; shift; done
STUB
# usermod -g GROUP USER: fails when FAKE_USERMOD_FAIL=1.
cat >"$stubs/usermod" <<'STUB'
#!/bin/bash
echo "usermod $*" >>"$FAKE_LOG"
test "${FAKE_USERMOD_FAIL:-0}" != 1 || exit 1
test "$1" = -g && echo "$2" >"$FAKE_STATE"
STUB
chmod +x "$stubs"/*

fn=$(awk '/^ensure_operator_user\(\)/,/^}/' "$here/../provision.sh")
run() { # [group of an existing ti-operator] [env...]; prints the exit status
  local group=$1; shift
  rm -f "$work/state"; : >"$work/log"
  test -z "$group" || echo "$group" >"$work/state"
  local st=0
  # shellcheck disable=SC2016 # the function body is expanded by the inner shell.
  env PATH="$stubs:$PATH" FAKE_STATE="$work/state" FAKE_LOG="$work/log" "$@" bash -c '
    log() { printf "provision: %s\n" "$*"; }
    die() { printf "provision: %s\n" "$*" >&2; exit 1; }
    eval "$1"; ensure_operator_user' _ "$fn" >"$work/out" 2>"$work/err" || st=$?
  echo "$st"
}
group() { cat "$work/state" 2>/dev/null || echo none; }

expect "no ti-operator: it is created" 0 "$(run '')"
expect "no ti-operator: created in ti-trader" ti-trader "$(group)"
expect "ti-operator in ti-trader: accepted" 0 "$(run ti-trader)"
grep -q usermod "$work/log" && expect "ti-operator in ti-trader: left as it is" no yes
expect "ti-operator in another group: corrected" 0 "$(run ti-other)"
expect "ti-operator in another group: its primary group becomes ti-trader" ti-trader "$(group)"
expect "a ti-operator whose group cannot be corrected is refused" 1 "$(run ti-other FAKE_USERMOD_FAIL=1)"
grep -q 'ti-other' "$work/err" || expect "the refusal names the group found" yes no
grep -q 'ti-trader' "$work/err" || expect "the refusal names the group wanted" yes no

test "$failures" -eq 0 || exit 1
echo "operator user tests ok"

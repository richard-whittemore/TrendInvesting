#!/bin/bash
# Tests for install_ibc_tree and fix_ibc_modes in deploy/provision.sh: the
# IBC tree is root's and readable by ti-gateway, whatever modes the zip or the
# umask gave its files; the gateway step repairs an existing tree without
# downloading again (ADR 0028: the template holds no secrets).
# Unprivileged: chown is a stand-in that records its arguments; the stand-in
# zip's tree holds a 0600 config.ini, as the real one came out on the server.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'chmod -R u+rwX "$work" 2>/dev/null || true; rm -rf "$work"' EXIT
failures=0
expect() { # description expected actual
  if test "$2" != "$3"; then
    echo "FAIL: $1: want '$2', got '$3'" >&2
    failures=$((failures + 1))
  fi
}
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

fns=$(awk '/^(fix_ibc_modes|install_ibc_tree)\(\)/,/^}/' "$here/../provision.sh")
test -n "$fns" || { echo "FAIL: provision.sh has no install_ibc_tree" >&2; exit 1; }

src="$work/src"
mkdir -p "$src/scripts" "$src/sub"
echo 'LogToConsole=yes' >"$src/config.ini"
echo '#!/bin/sh' >"$src/scripts/ibcstart.sh"
echo '#!/bin/sh' >"$src/gatewaystart.sh"
echo 'x' >"$src/sub/data.txt"
chmod 0600 "$src/config.ini" "$src/scripts/ibcstart.sh" "$src/gatewaystart.sh" "$src/sub/data.txt"
chmod 0700 "$src/sub" "$src/scripts"

run() { # DEST FUNCTION ARGS...; chown is recorded, not run
  local dest=$1; shift
  bash -c '
    chown() { echo "chown $*" >>"$CHOWN_LOG"; }
    eval "$1"; shift; "$@"' _ "$fns" "$@" >"$work/out" 2>&1 || { cat "$work/out" >&2; return 1; }
}
dest="$work/ibc"
export CHOWN_LOG="$work/chown"
: >"$CHOWN_LOG"

( umask 077; run "$dest" install_ibc_tree "$src" "$dest" )
expect "config.ini is 0644" 644 "$(mode "$dest/config.ini")"
expect "a subfolder is 0755" 755 "$(mode "$dest/sub")"
expect "scripts folder is 0755" 755 "$(mode "$dest/scripts")"
expect "a plain file is 0644" 644 "$(mode "$dest/sub/data.txt")"
expect "a script is 0755" 755 "$(mode "$dest/scripts/ibcstart.sh")"
expect "the start script is 0755" 755 "$(mode "$dest/gatewaystart.sh")"
grep -q 'chown -R root:root' "$CHOWN_LOG" || expect "the tree is given to root" yes no
if test "$(id -u)" -ne 0; then
  # A user other than the owner can read it: the owner here stands for root,
  # so the check is on the "other" bits.
  expect "others can read config.ini" 4 "$(( $(mode "$dest/config.ini") % 10 & 4 ))"
fi

# A rerun repairs a tree that is already there, from the files in place.
chmod 0600 "$dest/config.ini"
chmod 0700 "$dest/sub"
run "$dest" fix_ibc_modes "$dest"
expect "a rerun repairs config.ini" 644 "$(mode "$dest/config.ini")"
expect "a rerun repairs a folder" 755 "$(mode "$dest/sub")"

test "$failures" -eq 0 || exit 1
echo "ibc modes tests ok"

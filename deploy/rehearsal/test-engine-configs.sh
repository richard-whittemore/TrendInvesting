#!/bin/bash
# Tests for deploy/provision.sh's install of the engines' settings
# (install_engine_settings): each account's configuration is installed only
# when the server has none, so an account ID the owner substituted on the
# server survives every later `provision.sh services`, byte for byte; the
# shipped file goes beside it as <name>.config.example, and the settings
# files (*.env), which carry no account, are refreshed (docs/live-trading/lessons-2026-10.md L7: the
# provisioner once reset Boost's account to the shipped placeholder). Sources
# a copy of the provisioner; no root, no Docker and no network needed.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

unset TI_TARGET TI_REHEARSAL
src=$(cd "$here/../etc/engines" && pwd)
# Sourced, the provisioner only defines its steps (and sets its own $here).
# shellcheck disable=SC1091
. "$here/../provision.sh"
dest=$work/etc/engines
mkdir -p "$dest"

install_engine_settings "$src" "$dest" >"$work/out"
for f in "$src"/*.env "$src"/*.config; do
  cmp -s "$f" "$dest/$(basename "$f")" || fail "the first install did not install $(basename "$f")"
done
test -e "$dest/trad-ira.config" || fail "no trad-ira.config shipped"

# The owner names the account on the server, as the runbooks do.
sed -i.bak 's/"account_id": "trad-ira"/"account_id": "DUEDITED1"/' "$dest/trad-ira.config"
sed -i.bak 's/"account_id": "joint"/"account_id": "DUEDITED2"/' "$dest/boost.config"
rm -f "$dest"/*.bak
# Each edit must have taken, or the checks below would pass on an unedited file.
grep -q '"account_id": "DUEDITED1"' "$dest/trad-ira.config" || fail "the trad-ira.config edit did not apply"
grep -q '"account_id": "DUEDITED2"' "$dest/boost.config" || fail "the boost.config edit did not apply"
cp "$dest/trad-ira.config" "$work/trad-ira.edited"
cp "$dest/boost.config" "$work/boost.edited"
echo "TI_STALE=1" >>"$dest/trad-ira.env"

install_engine_settings "$src" "$dest" >"$work/out"
cmp -s "$work/trad-ira.edited" "$dest/trad-ira.config" || fail "a second install changed the edited trad-ira.config"
cmp -s "$work/boost.edited" "$dest/boost.config" || fail "a second install changed the edited boost.config"
grep -q DUEDITED1 "$dest/trad-ira.config" || fail "the edited trad-ira account ID is gone"
grep -q DUEDITED2 "$dest/boost.config" || fail "the edited boost account ID is gone"
cmp -s "$src/trad-ira.config" "$dest/trad-ira.config.example" || fail "the shipped trad-ira.config is not beside it as trad-ira.config.example"
cmp -s "$src/trad-ira.env" "$dest/trad-ira.env" || fail "the settings file trad-ira.env was not refreshed"
grep -q "kept $dest/trad-ira.config" "$work/out" || fail "the install does not say it kept trad-ira.config: $(cat "$work/out")"

if test "$failures" -gt 0; then
  echo "$failures failure(s)" >&2
  exit 1
fi
echo "test-engine-configs: ok"

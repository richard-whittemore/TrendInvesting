#!/bin/bash
# Tests for deploy/provision.sh's install of the engines' settings
# (install_engine_settings): each account's configuration is installed only
# when the server has none, so an account ID the owner substituted on the
# server survives every later `provision.sh services`, byte for byte; the
# shipped file goes beside it as <name>.config.example, and so is each
# settings file (*.env): one already on the server is kept byte for byte, an
# edit by hand included, and the shipped one goes beside it as
# <name>.env.example (docs/live-trading/lessons-2026-10.md L7: the
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

initial=$work/initial
mkdir -p "$initial"
cp "$src"/*.env "$src"/*.config "$initial/"
rm -f "$initial/roth-ira.env" "$initial/roth-ira.config"
install_engine_settings "$initial" "$dest" >"$work/out"
for f in "$initial"/*.env "$initial"/*.config; do
  cmp -s "$f" "$dest/$(basename "$f")" || fail "the first install did not install $(basename "$f")"
done
test -e "$dest/trad-ira.config" || fail "no trad-ira.config shipped"

# Adding the second IRA preserves every existing account's settings (docs/live-trading/lessons-2026-10.md L7), including the second IRA's own settings on a later run.
for f in "$dest"/*.config "$dest"/*.env; do
  cp "$f" "$work/$(basename "$f").before"
done
install_engine_settings "$src" "$dest" >"$work/out"
for f in "$work"/*.before; do
  name=$(basename "${f%.before}")
  cmp -s "$f" "$dest/$name" || fail "adding roth-ira changed $name"
done
test -e "$dest/roth-ira.config" || fail "no roth-ira.config installed from its example"

# Adapters are also created once, including the Roth's; the host substitution
# is target-specific and never rewrites an existing owner's file (ADR 0026).
adapters=$work/adapters
mkdir -p "$adapters"
printf 'owner-boost\n' >"$adapters/boost.json"
printf 'owner-trad\n' >"$adapters/trad-ira.json"
install_adapter_settings "$here/etc/adapters" "$adapters" >"$work/out"
grep -qx owner-boost "$adapters/boost.json" || fail "adapter install changed Boost"
grep -qx owner-trad "$adapters/trad-ira.json" || fail "adapter install changed Traditional IRA"
cmp -s "$here/etc/adapters/roth-ira.json.example" "$adapters/roth-ira.json" || fail "Roth adapter not created"
printf 'owner-roth\n' >"$adapters/roth-ira.json"
install_adapter_settings "$here/etc/adapters" "$adapters" >"$work/out"
grep -qx owner-roth "$adapters/roth-ira.json" || fail "adapter install changed Roth"
rm "$adapters/roth-ira.json"
target=mac-mini install_adapter_settings "$here/etc/adapters" "$adapters" >"$work/out"
grep -q '"host": "host.docker.internal"' "$adapters/roth-ira.json" || fail "Roth adapter on Mac mini uses Linux Gateway"

# The owner names the account on the server, as the runbooks do.
sed -i.bak 's/"account_id": "trad-ira"/"account_id": "DUEDITED1"/' "$dest/trad-ira.config"
sed -i.bak 's/"account_id": "joint"/"account_id": "DUEDITED2"/' "$dest/boost.config"
rm -f "$dest"/*.bak
# Each edit must have taken, or the checks below would pass on an unedited file.
grep -q '"account_id": "DUEDITED1"' "$dest/trad-ira.config" || fail "the trad-ira.config edit did not apply"
grep -q '"account_id": "DUEDITED2"' "$dest/boost.config" || fail "the boost.config edit did not apply"
cp "$dest/trad-ira.config" "$work/trad-ira.edited"
cp "$dest/boost.config" "$work/boost.edited"
echo "TI_OWNER_MARKER=1" >>"$dest/trad-ira.env"
cp "$dest/trad-ira.env" "$work/trad-ira.env.edited"

install_engine_settings "$src" "$dest" >"$work/out"
cmp -s "$work/trad-ira.edited" "$dest/trad-ira.config" || fail "a second install changed the edited trad-ira.config"
cmp -s "$work/boost.edited" "$dest/boost.config" || fail "a second install changed the edited boost.config"
grep -q DUEDITED1 "$dest/trad-ira.config" || fail "the edited trad-ira account ID is gone"
grep -q DUEDITED2 "$dest/boost.config" || fail "the edited boost account ID is gone"
cmp -s "$src/trad-ira.config" "$dest/trad-ira.config.example" || fail "the shipped trad-ira.config is not beside it as trad-ira.config.example"
cmp -s "$work/trad-ira.env.edited" "$dest/trad-ira.env" || fail "a second install changed the server's trad-ira.env (its marker line is gone)"
cmp -s "$src/trad-ira.env" "$dest/trad-ira.env.example" || fail "the shipped trad-ira.env is not beside it as trad-ira.env.example"
grep -q "kept $dest/trad-ira.env" "$work/out" || fail "the install does not say it kept trad-ira.env: $(cat "$work/out")"
grep -q "kept $dest/trad-ira.config" "$work/out" || fail "the install does not say it kept trad-ira.config: $(cat "$work/out")"

# A first install interrupted mid-copy never leaves a partial settings file
# under its own name, which every later run would keep as the owner's: the
# file is written beside it under a temporary name and renamed into place.
fresh=$work/etc/fresh
mkdir -p "$fresh"
# provision.sh runs with errexit, so the failed copy ends the step there (a
# subshell in a condition or a || list would run with errexit off).
set +e
(
  set -e
  # A stand-in for install(1) that dies part-way through trad-ira.env's
  # temporary file, after writing its first line.
  # shellcheck disable=SC2329 # called by install_engine_settings in this subshell
  install() {
    local last=${*: -1}
    case $last in
      */.trad-ira.env.new | */.trad-ira.config.new) head -n 1 "${*: -2:1}" >"$last"; return 1 ;;
      *) command install "$@" ;;
    esac
  }
  install_engine_settings "$src" "$fresh" >/dev/null 2>&1
)
set -e
test ! -e "$fresh/trad-ira.env" || fail "an interrupted first install left a partial trad-ira.env under its own name"
# The same for a configuration, which names the account and is hashed into the run: its copy cut short (the
# .env copies above now succeeding) never stands under its own name either.
rm -rf "$fresh" && mkdir -p "$fresh"
set +e
(
  set -e
  # shellcheck disable=SC2329 # called by install_engine_settings in this subshell
  install() {
    local last=${*: -1}
    case $last in
      */.trad-ira.config.new) head -n 1 "${*: -2:1}" >"$last"; return 1 ;;
      *) command install "$@" ;;
    esac
  }
  install_engine_settings "$src" "$fresh" >/dev/null 2>&1
)
set -e
test ! -e "$fresh/trad-ira.config" || fail "an interrupted first install left a partial trad-ira.config under its own name"
install_engine_settings "$src" "$fresh" >"$work/out"
cmp -s "$src/trad-ira.env" "$fresh/trad-ira.env" || fail "the install after an interrupted one did not install the whole trad-ira.env"
cmp -s "$src/trad-ira.config" "$fresh/trad-ira.config" || fail "the install after an interrupted one did not install the whole trad-ira.config"
test -z "$(find "$fresh" -name '.*.new')" || fail "a temporary settings file is left behind: $(find "$fresh" -name '.*.new')"

if test "$failures" -gt 0; then
  echo "$failures failure(s)" >&2
  exit 1
fi
echo "test-engine-configs: ok"

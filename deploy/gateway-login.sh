#!/bin/bash
# Selects the Gateway login without touching secrets or restarting services
# (docs/runbooks/gateway-login.md). The selector is root-owned and non-secret.
set -euo pipefail
umask 022
selector=/etc/trend-investing/gateway-login
die() { echo "ti-gateway-login: $*" >&2; exit 64; }
test ! -d "$selector" || die "$selector is a directory; expected a selector file"
test $# -le 1 || die 'usage: sudo ti-gateway-login [joint|trad-ira|roth-ira|live]'
if test $# -eq 0; then
  test -r "$selector" || die "missing or unreadable $selector; run sudo deploy/provision.sh services"
  selection=$(cat "$selector" && printf '.') || exit 64
  selection=${selection%.}
  selection=${selection%$'\n'}
else
  test "$(id -u)" -eq 0 || die 'run as root (sudo ti-gateway-login ...)'
  selection=$1
fi
case $selection in
  joint|trad-ira|roth-ira|live) ;;
  *) die "$selector must contain one word: joint | trad-ira | roth-ira | live" ;;
esac
if test $# -eq 0; then
  # Reject bytes that Bash drops when reading the selector (the one-word
  # invariant in docs/runbooks/gateway-login.md).
  if ! cmp -s "$selector" <(printf '%s' "$selection") && ! cmp -s "$selector" <(printf '%s\n' "$selection"); then
    die "invalid bytes in $selector; expected one login word with an optional final newline"
  fi
  printf '%s\n' "$selection"
  exit 0
fi
tmp=$(mktemp "${selector}.tmp.XXXXXX")
trap 'rm -f -- "$tmp"' EXIT
printf '%s\n' "$selection" >"$tmp"
chmod 0644 "$tmp"
mv -f -- "$tmp" "$selector"
echo "Gateway login selected: $selection. A Gateway restart applies it: sudo systemctl restart ti-gateway"

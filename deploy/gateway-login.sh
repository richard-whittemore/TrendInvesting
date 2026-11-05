#!/bin/bash
# Selects the Gateway login without touching secrets or restarting services
# (docs/runbooks/gateway-login.md). The selector is root-owned and non-secret.
set -euo pipefail
umask 022
selector=/etc/trend-investing/gateway-login
die() { echo "ti-gateway-login: $*" >&2; exit 64; }
test ! -d "$selector" || die "$selector is a directory; expected a selector file"
test $# -le 1 || die 'usage: sudo ti-gateway-login [joint|live]'
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
  joint|live) ;;
  trad-ira|roth-ira) die "$selector: use ti-gateway@$selection.service for this login" ;;
  *) die "$selector must contain one word: joint | live" ;;
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
login="${selector%/*}/ibkr-login-$selection.env.gpg"
test -f "$login" || die "missing regular file $login; create it with sudo ti-secrets edit ibkr-login-$selection.env"
# Acknowledged selections survive power loss (ADR 0028;
# docs/runbooks/gateway-login.md). Sync both the contents and the rename.
python3 -I - "$selector" "$selection" <<'PY'
import os
from pathlib import Path
import sys
import tempfile

selector = Path(sys.argv[1])
fd, temporary = tempfile.mkstemp(prefix=selector.name + ".tmp.", dir=selector.parent)
try:
    with os.fdopen(fd, "w") as output:
        output.write(sys.argv[2] + "\n")
        output.flush()
        os.fchmod(output.fileno(), 0o644)
        os.fsync(output.fileno())
    os.replace(temporary, selector)
    directory = os.open(selector.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)
finally:
    Path(temporary).unlink(missing_ok=True)
PY
echo "Gateway login selected: $selection. Applies at the Gateway's next start of any kind (owner restart, healer, ti-unlock, systemd restart)."
if test "$selection" = live; then
  echo "For live, restart at once with sudo systemctl restart ti-gateway; switch back to joint afterwards."
else
  echo "Select only immediately before a planned restart: sudo systemctl restart ti-gateway"
fi

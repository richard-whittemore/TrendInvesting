#!/bin/bash
# ti-maintenance: open, close or show the host's maintenance window, the
# bounded quiet period in which an install's own noise pages no one.
#
#   sudo ti-maintenance start [DURATION]   open a window: 15 minutes by default,
#                                          30 at most (10m, 90s, or a bare
#                                          number of minutes)
#   sudo ti-maintenance end                close it
#   ti-maintenance status                  say whether one is open, and until when
#
# While a window is open the reporter still fires and still pings the outside
# heartbeat; it RECORDS failed units, a logged-out Gateway and a reconciliation
# alert without paging them, and pages whatever is still wrong at the first
# firing after the window closes. An order refused, a halt, a Degraded state
# and the other capital-safety alerts are never held (internal/report,
# maintenance.go; ADR 0019, "Alerting"). `ti-release` opens and closes a window
# itself (docs/manual/server.md, "Installing a release").
#
# The marker is /var/lib/trend-investing/reports/maintenance.json, in the
# reports directory: the reporter and the OnFailure hook already read and write
# only there, so no unit file and no permission on any other directory changes,
# and it survives a reboot, which a window of at most 30 minutes may not
# outlive (its expiry is in the file, so an old one reads as closed whenever it
# is read). It is written by a shell script, not a Go program: ti-release is a
# shell script that must work while the Go programs are being replaced, and a
# seventh program would join the release's checksum, build and attestation
# lists for a dozen lines of work.
#
# Settings for tests (the server needs none): TI_MAINTENANCE_FILE,
# TI_MAINTENANCE_NOW (epoch seconds), TI_MAINTENANCE_NO_ROOT.
set -euo pipefail

file=${TI_MAINTENANCE_FILE:-/var/lib/trend-investing/reports/maintenance.json}
max_s=1800
default_s=900

die() { printf 'ti-maintenance: %s\n' "$*" >&2; exit 1; }
now() { printf '%s' "${TI_MAINTENANCE_NOW:-$(date +%s)}"; }
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }
need_root() {
  if test "${TI_MAINTENANCE_NO_ROOT:-0}" != 1 && test "$(id -u)" -ne 0; then
    die "run as root (sudo ti-maintenance ...)"
  fi
}

# seconds DURATION: the duration in seconds, or nothing if it is not one.
seconds() {
  [[ $1 =~ ^[0-9]+[ms]?$ ]] || return 1
  case $1 in
    *m) printf '%s' "$(( 10#${1%m} * 60 ))" ;;
    *s) printf '%s' "$(( 10#${1%s} ))" ;;
    *) printf '%s' "$(( 10#$1 * 60 ))" ;;
  esac
}

# until_unix: the open marker's expiry in epoch seconds, or nothing.
until_unix() { sed -n 's/.*"until_unix":\([0-9][0-9]*\).*/\1/p' "$file" 2>/dev/null | head -n 1; }

cmd=${1:-}
case $cmd in
  start)
    need_root
    test $# -le 2 || die "usage: ti-maintenance start [DURATION]"
    d=${2-}
    if test $# -lt 2; then
      secs=$default_s
    else
      secs=$(seconds "$d") || die "'$d' is not a duration (10m, 90s, or a number of minutes)"
    fi
    test "$secs" -ge 1 || die "the window must be at least a second long"
    test "$secs" -le "$max_s" || die "a window is at most 30 minutes; '$d' is longer, so none was opened"
    t=$(now)
    end=$((t + secs))
    dir=$(dirname "$file")
    test -d "$dir" || die "$dir does not exist"
    tmp=$(mktemp "$dir/.maintenance.XXXXXX")
    printf '{"opened":"%s","until":"%s","opened_unix":%s,"until_unix":%s}\n' "$(iso "$t")" "$(iso "$end")" "$t" "$end" >"$tmp" || { rm -f "$tmp"; die "could not write the marker"; }
    chmod 0644 "$tmp"
    # The reporter's user owns the reports directory; the file is its to
    # remove when it is past its expiry.
    if test "${TI_MAINTENANCE_NO_ROOT:-0}" != 1; then chown ti-trader:ti-trader "$tmp" 2>/dev/null || true; fi
    # Durable before the rename, and the rename before this returns.
    sync "$tmp" 2>/dev/null || sync
    mv -f "$tmp" "$file" || { rm -f "$tmp"; die "could not write the marker"; }
    sync "$dir" 2>/dev/null || sync
    printf 'ti-maintenance: maintenance window open until %s (%s seconds); capital-safety alerts still page\n' "$(iso "$end")" "$secs"
    ;;
  end)
    need_root
    rm -f "$file"
    echo "ti-maintenance: maintenance window closed; whatever is still broken pages at the reporter's next firing, within 5 minutes"
    ;;
  status)
    u=$(until_unix)
    if test -z "$u" || test "$(now)" -ge "$u"; then
      # A marker past its expiry is closed; removing it is a courtesy.
      if test -e "$file" && { test "${TI_MAINTENANCE_NO_ROOT:-0}" = 1 || test "$(id -u)" -eq 0; }; then rm -f "$file"; fi
      echo "no maintenance window"
    else
      printf 'maintenance window open until %s (%s seconds left)\n' "$(iso "$u")" "$((u - $(now)))"
    fi
    ;;
  *)
    echo "usage: ti-maintenance start [DURATION] | end | status" >&2
    exit 1
    ;;
esac

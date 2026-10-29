#!/bin/bash
# ti-maintenance: open, close or show the host's maintenance window, the
# bounded quiet period in which an install's own noise pages no one.
#
#   sudo ti-maintenance start [DURATION]   open a window: 15 minutes by default,
#                                          30 at most (10m, 90s, or a bare
#                                          number of minutes)
#   sudo ti-maintenance end                close it
#   ti-maintenance status                  say whether one is open, and until when
#   ti-maintenance is-open                 exit 0 when one is open, 1 when not
#                                          (what ti-release asks, so it never
#                                          overwrites or ends a window you opened)
#
# While a window is open the reporter still fires and still pings the outside
# heartbeat; it RECORDS failed units and a logged-out Gateway
# without paging them, and pages whatever is still wrong at the first
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
# A marker is judged here exactly as internal/maintenance.Read judges it (one
# set of fixture files, internal/maintenance/testdata/markers, is run through
# both): both ends in both forms and agreeing, no more than 30 minutes long,
# and now from its opening to before its expiry; anything else is closed. No
# reader removes a marker, expired or not: it reads closed whoever sees it, and
# the next start replaces it, so a start in flight is never undone.
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

# seconds DURATION: the duration in seconds, or nothing if it is not one. At
# most five digits are read, so the arithmetic below cannot overflow.
seconds() {
  [[ $1 =~ ^[0-9]{1,5}[ms]?$ ]] || return 1
  case $1 in
    *m) printf '%s' "$(( 10#${1%m} * 60 ))" ;;
    *s) printf '%s' "$(( 10#${1%s} ))" ;;
    *) printf '%s' "$(( 10#$1 * 60 ))" ;;
  esac
}

# field NAME: a marker field's value (a number, or a string without quotes).
field() { sed -n 's/.*"'"$1"'": *"\{0,1\}\([^",}]*\)"\{0,1\}.*/\1/p' "$file" 2>/dev/null | head -n 1; }

# until_unix: the expiry, in epoch seconds, of the window open now; nothing
# when there is none or the marker cannot be trusted.
until_unix() {
  local o u os us t
  test -r "$file" || return 0
  o=$(field opened_unix); u=$(field until_unix); os=$(field opened); us=$(field until); t=$(now)
  [[ $o =~ ^[0-9]{1,12}$ && $u =~ ^[0-9]{1,12}$ && -n $os && -n $us ]] || return 0
  test "$u" -ge "$o" && test $((u - o)) -le "$max_s" || return 0
  test "$t" -ge "$o" && test "$t" -lt "$u" || return 0
  test "$(iso "$o")" = "$os" && test "$(iso "$u")" = "$us" || return 0
  printf '%s' "$u"
}

cmd=${1:-}
case $cmd in
  start)
    need_root
    test $# -le 2 || die "usage: ti-maintenance start [DURATION]"
    d=${2-}
    if test $# -lt 2; then
      secs=$default_s
    else
      secs=$(seconds "$d") || die "'$d' is not a duration (10m, 90s, or a number of minutes up to 30)"
    fi
    test "$secs" -ge 1 || die "the window must be at least a second long"
    test "$secs" -le "$max_s" || die "a window is at most 30 minutes; '$d' is longer, so none was opened"
    t=$(now)
    end=$((t + secs))
    dir=$(dirname "$file")
    test -d "$dir" || die "$dir does not exist"
    # Prepared in a folder only the writer can reach (root, on the server),
    # then published into the reports folder, which the reporter's user owns,
    # under a name no one can guess; no path in that folder is written,
    # chmodded or chowned after it is created, so root follows none that the
    # reporter's user could swap. The file stays the writer's: the reporter's
    # user only reads it.
    work=$(mktemp -d) || die "could not make a private folder"
    trap 'rm -rf "$work"' EXIT
    printf '{"opened":"%s","until":"%s","opened_unix":%s,"until_unix":%s}\n' "$(iso "$t")" "$(iso "$end")" "$t" "$end" >"$work/marker" || die "could not write the marker"
    chmod 0644 "$work/marker"
    pub=$(mktemp -u "$dir/.maintenance.XXXXXXXXXX")
    install -m 0644 "$work/marker" "$pub" || { rm -f "$pub"; die "could not write the marker"; }
    # Durable before the rename, and the rename before this returns.
    sync "$pub" 2>/dev/null || sync
    mv -f "$pub" "$file" || { rm -f "$pub"; die "could not write the marker"; }
    sync "$dir" 2>/dev/null || sync
    printf 'ti-maintenance: maintenance window open until %s (%s seconds); capital-safety alerts still page\n' "$(iso "$end")" "$secs"
    ;;
  end)
    need_root
    rm -f "$file"
    # The removal is on disk before success is reported: a power cut must not
    # bring an earlier marker back.
    sync "$(dirname "$file")" 2>/dev/null || sync || die "could not make the close durable"
    echo "ti-maintenance: maintenance window closed; whatever is still broken pages at the reporter's next firing, within 5 minutes"
    ;;
  status)
    u=$(until_unix)
    if test -z "$u"; then
      echo "maintenance window closed"
    else
      printf 'maintenance window open until %s (%s seconds left)\n' "$(iso "$u")" "$((u - $(now)))"
    fi
    ;;
  is-open)
    test -n "$(until_unix)"
    ;;
  *)
    echo "usage: ti-maintenance start [DURATION] | end | status | is-open" >&2
    exit 1
    ;;
esac

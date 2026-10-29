#!/bin/bash
# ti-maintenance: open, close or show the host's maintenance window, the
# bounded quiet period in which an install's own noise pages no one.
#
#   sudo ti-maintenance start [DURATION]   open a window: 15 minutes by default,
#                                          30 at most (10m, 90s, or a bare
#                                          number of minutes); prints the
#                                          window's id on a line "id <hex>"
#   sudo ti-maintenance end [--id ID]      close it; with --id, only the window
#                                          that has that id (ti-release closes
#                                          only the window it opened)
#   ti-maintenance status                  say whether one is open, and until when
#   ti-maintenance is-open                 exit 0 when one is open, 1 when not
#                                          (what ti-release asks, so it never
#                                          overwrites or ends a window you opened)
#
# While a window is open the reporter still fires and still pings the outside
# heartbeat; it RECORDS failed units and a logged-out Gateway without paging
# them, and pages whatever is still wrong at the first firing after the window
# closes. An order refused, a halt, a Degraded state, a reconciliation that did
# not match and the other capital-safety alerts are never held (internal/report,
# maintenance.go; ADR 0019, "Alerting"). `ti-release` opens and closes a window
# itself (docs/manual/server.md, "Installing a release").
#
# The marker is /run/ti-maintenance/maintenance.json. The folder is made by
# root and writable by root alone, so no other user can replace, swap or link
# anything in it; the reporter, the failed-unit hook and the dashboard only
# read it (they run as another user and see /run read-only). It is on /run, so
# a reboot ends a window, which one of at most 30 minutes may not outlive
# anyway. The file is created exclusively, with the mode root's umask gives
# (0644), under a name no one can guess, and renamed into place; no path is
# chmodded or chowned. It is written by a shell script, not a Go program:
# ti-release is a shell script that must work while the Go programs are being
# replaced, and a seventh program would join the release's checksum, build and
# attestation lists for a dozen lines of work.
#
# The marker has ONE layout, written only here and accepted only in this form
# by this script and by internal/maintenance (fixtures:
# internal/maintenance/testdata/markers, run through both): lower-case keys in
# a fixed order and no spaces, UTC times to the second ending in Z, the same
# times as seconds since the epoch, one object and one newline. Anything else,
# or a window of more than 30 minutes, or one not yet begun, reads as no
# window. The file is read once and every field is taken from that one read. No
# reader removes a marker, expired or not: it reads closed whoever sees it,
# and the next start replaces it, so a start in flight is never undone.
#
# Settings for tests (the server needs none): TI_MAINTENANCE_FILE,
# TI_MAINTENANCE_NOW (epoch seconds), TI_MAINTENANCE_NO_ROOT.
set -euo pipefail

file=${TI_MAINTENANCE_FILE:-/run/ti-maintenance/maintenance.json}
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

layout='^\{"id":"([0-9a-f]{16})","opened":"([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)","until":"([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)","opened_unix":([0-9]{1,12}),"until_unix":([0-9]{1,12})\}$'

# read_marker: reads the marker once and sets marker_id and marker_until to the
# window open now (empty when there is none or the marker cannot be trusted).
marker_id=
marker_until=
read_marker() {
  local raw o u t
  marker_id=
  marker_until=
  test -r "$file" || return 0
  raw=$(cat "$file" 2>/dev/null) || return 0
  # One object on one line: a second line, even blank, is not the layout.
  [[ $(wc -l <"$file" | tr -d ' ') -le 1 ]] || return 0
  [[ $raw =~ $layout ]] || return 0
  o=${BASH_REMATCH[4]}
  u=${BASH_REMATCH[5]}
  t=$(now)
  test "$u" -ge "$o" && test $((u - o)) -le "$max_s" || return 0
  test "$t" -ge "$o" && test "$t" -lt "$u" || return 0
  test "$(iso "$o")" = "${BASH_REMATCH[2]}" && test "$(iso "$u")" = "${BASH_REMATCH[3]}" || return 0
  marker_id=${BASH_REMATCH[1]}
  marker_until=$u
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
    ( umask 022; mkdir -p "$dir" ) || die "could not make $dir"
    id=$(od -An -tx1 -N8 /dev/urandom | tr -d ' \n')
    pub=$(mktemp -u "$dir/.maintenance.XXXXXXXXXX")
    # Created exclusively with the umask's mode (0644), then renamed over the
    # marker: nothing is ever chmodded or chowned by name.
    ( umask 022; set -o noclobber
      printf '{"id":"%s","opened":"%s","until":"%s","opened_unix":%s,"until_unix":%s}\n' "$id" "$(iso "$t")" "$(iso "$end")" "$t" "$end" >"$pub" ) || { rm -f "$pub"; die "could not write the marker"; }
    # Durable before the rename, and the rename before this returns.
    sync "$pub" 2>/dev/null || sync
    mv -f "$pub" "$file" || { rm -f "$pub"; die "could not write the marker"; }
    sync "$dir" 2>/dev/null || sync
    printf 'id %s\n' "$id"
    printf 'ti-maintenance: maintenance window open until %s (%s seconds); capital-safety alerts still page\n' "$(iso "$end")" "$secs"
    ;;
  end)
    need_root
    if test "${2-}" = --id; then
      test $# -eq 3 && [[ $3 =~ ^[0-9a-f]{16}$ ]] || die "usage: ti-maintenance end [--id ID]"
      # Only the window with that id: one opened since, by anyone else, stays.
      raw=$(cat "$file" 2>/dev/null || true)
      if [[ $raw =~ $layout ]] && test "${BASH_REMATCH[1]}" = "$3"; then
        rm -f "$file"
      else
        echo "ti-maintenance: the open window (if any) is not window $3; left as it is"
        exit 0
      fi
    else
      test $# -eq 1 || die "usage: ti-maintenance end [--id ID]"
      rm -f "$file"
    fi
    # The removal is on disk before success is reported: a power cut must not
    # bring an earlier marker back.
    sync "$(dirname "$file")" 2>/dev/null || sync || die "could not make the close durable"
    echo "ti-maintenance: maintenance window closed; whatever is still broken pages at the reporter's next firing, within 5 minutes"
    ;;
  status)
    read_marker
    if test -z "$marker_until"; then
      echo "maintenance window closed"
    else
      printf 'maintenance window open until %s (%s seconds left, id %s)\n' "$(iso "$marker_until")" "$((marker_until - $(now)))" "$marker_id"
    fi
    ;;
  is-open)
    read_marker
    test -n "$marker_until"
    ;;
  *)
    echo "usage: ti-maintenance start [DURATION] | end [--id ID] | status | is-open" >&2
    exit 1
    ;;
esac

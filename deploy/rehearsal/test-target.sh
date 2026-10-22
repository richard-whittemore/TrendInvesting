#!/bin/bash
# Tests for deploy/provision.sh's two targets (TI_TARGET): the Lightsail
# server, the default, and the Mac mini's rehearsal container (mac-mini). Each
# case sources a copy of the provisioner, replaces every command that would
# change the machine with a stand-in that records its arguments, runs `main`
# with one step, and checks what was called. No root, no Docker and no network
# needed.
#
# The test must never change the machine it runs on, whatever permissions it
# has. Commands that only change the machine (install, systemctl, useradd and
# the rest) are recorded and never run. The file commands the provisioner
# also uses on its own temporary files (rm, cp, mv, mkdir, touch, dd, sed in
# place...) run only when every path they are given, resolved against the
# case's folder inside the test's tree as `realpath -m` would, is inside
# that tree; any other path is refused, recorded as REFUSED, and ends the
# case at once with status 97, which every case is checked for. Where a step must
# stop part way (the Gateway's unpacking), its stand-in stops it explicitly
# (status 75) rather than relying on `set -e`, which Bash ignores inside a
# subshell tested with `||`. A redirection cannot be intercepted, so the
# provisioner redirects only to /dev/null or /dev/stderr, or under one of the
# path variables each case sets inside the tree ($redirected_vars; checked
# below).
#
# What the Mac mini target does differently is in docs/manual/mac-mini.md:
# arm64 accepted only with it; no Tailscale, firewall or Linux Gateway; the
# Gateway unit masked rather than enabled; a persistent journal; an editor;
# the clock checked against a reference rather than synced, at provisioning
# and every 15 minutes; the service users at fixed IDs.
# The stand-ins are called by the sourced provisioner, and the variables they
# set are read by it, which shellcheck cannot see.
# shellcheck disable=SC2034,SC2329
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# The tree's own path with every link resolved, as the guard below resolves
# each path it is given (macOS's temporary folder is reached through a link).
work=$(cd "$(mktemp -d)" && pwd -P)
trap 'rm -rf "$work"' EXIT
# The caller's environment must not decide any case.
unset TI_TARGET TI_REHEARSAL TI_REHEARSAL_ANY_ARCH TI_REHEARSAL_SKIP_INSTALLER TI_CLOCK_VERIFY

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

# The provisioner's path variables each case sets under $work, so that a
# redirection to one of them stays inside the test's tree.
redirected_vars='etc unit_dir dl_cache twsapi_root adapter_venv adapter_root'
# redirect_leaks FILE: prints, as "LINE: TEXT;", each line with a redirection of output
# in FILE (outside a comment line) to a literal absolute path other than
# /dev/null or /dev/stderr (bare, double- or single-quoted), to a path
# starting with a variable not in $redirected_vars, or to one starting with
# "~" or a command substitution ($(...) or backticks), whose place the test cannot know. A redirection cannot be intercepted by a stand-in, so
# these are the only ones the provisioner may use.
redirect_leaks() {
  awk -v allowed=" $redirected_vars " '
    /^[[:space:]]*#/ { next }
    {
      line = $0
      bad = 0
      while (match(line, />>?[|]?[[:space:]]*["\047]?(\$\{?[A-Za-z_][A-Za-z_0-9]*|\$\(|`|~|\/[^[:space:];|&)"\047]*)/)) {
        t = substr(line, RSTART, RLENGTH)
        line = substr(line, RSTART + RLENGTH)
        sub(/^>>?[|]?[[:space:]]*["\047]?/, "", t)
        if (t ~ /^\$/) {
          v = t; sub(/^\$\{?/, "", v)
          if (index(allowed, " " v " ")) continue
        } else if (t == "/dev/null" || t == "/dev/stderr") continue
        bad = 1
      }
      if (bad) { text = $0; sub(/^[[:space:]]+/, "", text); printf "%d: %s; ", NR, text }
    }' "$1"
}

# A copy of deploy/, whose clock scripts record that they ran (and the clock
# check fails when FAKE_CLOCK_BAD=1).
cp -R "$here/.." "$work/deploy"
cat >"$work/deploy/libexec/time-sync.sh" <<'STUB'
#!/bin/bash
echo "time-sync.sh" >>"$TEST_CALLS"
STUB
cat >"$work/deploy/libexec/clock-check.sh" <<'STUB'
#!/bin/bash
echo "clock-check.sh TI_CLOCK_VERIFY=${TI_CLOCK_VERIFY:-}" >>"$TEST_CALLS"
test "${FAKE_CLOCK_BAD:-0}" != 1
STUB
chmod +x "$work/deploy/libexec/"*.sh

# scenario TARGET ARCH STEP: runs `provision.sh STEP` in a subshell with
# TI_TARGET=TARGET ("" for unset) on a machine reporting ARCH. Its output is
# in $work/out, the commands it called in $work/calls, one per line; its
# exit status is returned. Set FAKE_SYSTEMD=1 for a running systemd,
# TEST_REHEARSAL=1 for TI_REHEARSAL=1, FAKE_PASSWD to the stand-in user
# database's lines ("name:x:uid:gid::home:shell"; by default both service
# users exist) and TEST_LEAK to a command run before `main` (the guard's own
# test). A command refused by the guard fails the case unless TEST_LEAK is set.
# resolve PATH: PATH made absolute against $PWD, with every link followed and
# every "." and ".." taken out, whether or not the rest exists: what GNU
# `realpath -m` prints, which macOS's realpath cannot (it has no -m). A link
# is followed before a ".." after it is applied, as the kernel does.
resolve() {
  local p=$1 out='' c target depth=${2:-0}
  test "$depth" -lt 40 || { echo /resolve-loop; return; }
  case $p in /*) ;; *) p=$PWD/$p ;; esac
  # Split on "/" with pathname expansion off, so that a component holding *,
  # ? or [...] is taken as it is written, never matched against files; the
  # caller's setting is restored.
  local IFS=/ noglob=0
  case $- in *f*) noglob=1 ;; esac
  set -f
  # shellcheck disable=SC2086
  set -- $p
  test "$noglob" = 1 || set +f
  for c in "$@"; do
    case $c in
      '' | .) ;;
      ..) out=${out%/*} ;;
      *)
        if test -L "$out/$c"; then
          target=$(readlink "$out/$c")
          case $target in /*) ;; *) target=$out/$target ;; esac
          out=$(resolve "$target" $((depth + 1)))
          test "$out" = / && out=
        else
          out=$out/$c
        fi
        ;;
    esac
  done
  printf '%s\n' "${out:-/}"
}
default_passwd='ti-gateway:x:2001:2001::/var/lib/ti-gateway:/usr/sbin/nologin
ti-trader:x:2002:2002::/var/lib/trend-investing:/usr/sbin/nologin
ti-operator:x:2003:2002::/nonexistent:/usr/sbin/nologin'
scenario() {
  local target=$1 arch=$2 step=$3
  : >"$work/calls"
  rm -rf "${work:?}/etc" "${work:?}/units" "${work:?}/tmp" "${work:?}/cwd"
  mkdir -p "$work/etc/adapters" "$work/units" "$work/tmp" "$work/cwd"
  # The owner's checkout, which the service users cannot enter.
  chmod 0700 "$work/cwd"
  printf '%s\n' "${FAKE_PASSWD-$default_passwd}" | sed '/^$/d' >"$work/passwd"
  # Each user's primary group, named after the first user that has it (ti-operator
  # shares ti-trader's).
  awk -F: '!seen[$4]++ {print $1 ":x:" $4 ":"}' "$work/passwd" >"$work/group"
  # FAKE_GROUP_EXTRA: group lines with no user (a run cut off between
  # groupadd and useradd).
  if test -n "${FAKE_GROUP_EXTRA:-}"; then printf '%s\n' "$FAKE_GROUP_EXTRA" >>"$work/group"; fi
  # The volumes' roots, kept across cases (a case's files are set up first).
  mkdir -p "$work/vol/state" "$work/vol/opt"
  if test -n "${FAKE_BOOST_JSON:-}"; then printf '%s\n' "$FAKE_BOOST_JSON" >"$work/etc/adapters/boost.json"; fi
  local status=0
  (
    cd "$work/cwd"
    export TMPDIR="$work/tmp"
    export TEST_CALLS="$work/calls"
    if test -n "$target"; then export TI_TARGET=$target; fi
    if test "${TEST_REHEARSAL:-0}" = 1; then export TI_REHEARSAL=1; fi
    # shellcheck source=/dev/null
    . "$work/deploy/provision.sh"
    # The machine's identity.
    uname() { echo "$arch"; }
    read_os_release() { ID=ubuntu VERSION_ID=24.04; }
    systemd_running() { test "${FAKE_SYSTEMD:-0}" = 1; }
    record() { echo "$*" >>"$TEST_CALLS"; }
    # The user database is a stand-in: $work/passwd and $work/group.
    id() {
      case $# in
        0) echo "uid=0(root)" ;;
        2) if test "$1" = -gn; then
             local g
             g=$(awk -F: -v n="$2" '$1 == n {print $4; found = 1} END {exit !found}' "$work/passwd") || return 1
             awk -F: -v g="$g" '$3 == g {print $1; found = 1} END {if (!found) print g}' "$work/group"
             return
           fi
           local field=3; test "$1" = -g && field=4
           awk -F: -v n="$2" -v f="$field" '$1 == n {print $f; found = 1} END {exit !found}' "$work/passwd" ;;
        1) if test "$1" = -u; then echo 0; else awk -F: -v n="$1" '$1 == n {f = 1} END {exit !f}' "$work/passwd"; fi ;;
        *) local field=3; test "$1" = -g && field=4
           awk -F: -v n="$2" -v f="$field" '$1 == n {print $f; found = 1} END {exit !found}' "$work/passwd" ;;
      esac
    }
    getent() {
      local db=$work/$1
      awk -F: -v k="$2" '$1 == k || $3 == k {print; f = 1} END {exit f ? 0 : 2}' "$db"
    }
    useradd() {
      record useradd "$@"
      # Without --uid, the next free system ID, as useradd picks one.
      local uid gid name usergroup=0
      uid=$((900 + $(wc -l <"$work/passwd")))
      gid=$uid
      while test $# -gt 1; do
        case $1 in --uid | -u) uid=$2; shift ;; --gid | -g) gid=$2; shift ;; --user-group | -U) usergroup=1 ;; esac
        shift
      done
      name=$1
      case $gid in *[!0-9]*) gid=$(awk -F: -v g="$gid" '$1 == g {print $3}' "$work/group") ;; esac
      if test "$usergroup" = 1; then echo "$name:x:$gid:" >>"$work/group"; fi
      echo "$name:x:$uid:$gid::/nonexistent:/usr/sbin/nologin" >>"$work/passwd"
    }
    groupadd() {
      record groupadd "$@"
      local gid=99
      while test $# -gt 1; do
        case $1 in --gid | -g) gid=$2; shift ;; esac
        shift
      done
      echo "$1:x:$gid:" >>"$work/group"
    }
    # Every command that only changes the machine is recorded, never run.
    install() { record install "$@"; }
    systemctl() { record systemctl "$@"; }
    apt-get() { record apt-get "$@"; }
    usermod() { record usermod "$@"; return 1; }
    userdel() { record userdel "$@"; return 1; }
    groupdel() { record groupdel "$@"; return 1; }
    chmod() { record chmod "$@"; }
    chown() { record chown "$@"; }
    chgrp() { record chgrp "$@"; }
    ln() { record ln "$@"; }
    tar() { record tar "$@"; }
    systemd-tmpfiles() { record systemd-tmpfiles "$@"; }
    fetch() { record fetch "$@"; }
    ufw() { record ufw "$@"; }
    tailscale() { record tailscale "$@"; return 1; }
    ip() { record ip "$@"; return 1; }
    mount() { record mount "$@"; return 1; }
    umount() { record umount "$@"; return 1; }
    curl() { record curl "$@"; return 1; }
    python3() { record python3 "$@"; return 1; }
    sudo() { record sudo "$@"; return 1; }
    docker() { record docker "$@"; return 1; }
    timedatectl() { record timedatectl "$@"; return 1; }
    # Unpacking IBC, or running a vendor installer, is where the Gateway case
    # stops, explicitly.
    unzip() { record unzip "$@"; record "STOPPED at unzip"; exit 75; }
    # The install4j installer cds back to the folder it started in; where that
    # folder is closed to the service user (the owner's checkout, mode 0700),
    # the real one prints this line, so the stand-in does too.
    runuser() {
      record runuser "$@"
      case $(stat -c %a . 2>/dev/null || stat -f %Lp .) in
        *[1357]) ;;
        *) echo "installer.sh: 820: cd: can't cd to $PWD" >&2 ;;
      esac
      record "STOPPED at runuser"
      exit 75
    }
    # Temporary folders are made inside the test's tree (macOS's mktemp may
    # not follow TMPDIR).
    mktemp() {
      if test "${1:-}" = -d; then command mktemp -d "$work/tmp/tmp.XXXXXX"; else command mktemp "$work/tmp/tmp.XXXXXX"; fi
    }
    # File commands run only inside the test's tree: every argument that is
    # not an option (each one after "--"), an option's "=value", the value of
    # a short option in $path_opts given attached (cp -t/dir, or in a
    # cluster, -at/dir), and dd's if=/of= operands is resolved as
    # `realpath -m` would, against the case's folder, and must be $work or
    # under it. A mode (0644) resolves as a relative name inside the tree, so
    # it passes. A sed script is not a path: sed's stand-in below leaves it
    # out, so one beginning with an address ("/^$/d") is not refused.
    inside_work() {
      local a p ends=0 rest c
      for a in "$@"; do
        p=$a
        if test "$ends" = 0; then
          case $p in
            --) ends=1; continue ;;
            --*=*) p=${p#*=} ;;
            -?*)
              # A cluster of short options: the rest after one in $path_opts
              # is its path; after one in $value_opts, a value that is not.
              rest=${p#-} p=
              while test -n "$rest"; do
                c=${rest:0:1} rest=${rest:1}
                case $c in
                  [$value_opts]) break ;;
                  [$path_opts]) p=$rest; break ;;
                esac
              done
              test -n "$p" || continue
              ;;
            if=* | of=*) p=${p#*=} ;;
          esac
        fi
        case $p in /dev/null | /dev/zero | /dev/stdin | /dev/stdout | /dev/stderr) continue ;; esac
        p=$(resolve "$p")
        case $p in "$work" | "$work"/*) ;; *) return 1 ;; esac
      done
    }
    # refuse CMD ARGS...: records the refusal and ends the case.
    refuse() {
      record "REFUSED $*"
      echo "test-target: refused '$*': outside the test's tree" >&2
      exit 97
    }
    guarded() {
      local cmd=$1 path_opts=' ' value_opts=' '
      shift
      # GNU cp and mv: -t DIR is the target directory, -S SUFFIX the
      # backups' suffix. (A space keeps each bracket expression valid.)
      case $cmd in cp | mv) path_opts=t value_opts=S ;; esac
      inside_work "$@" || refuse "$cmd" "$@"
      command "$cmd" "$@"
    }
    rm() { guarded rm "$@"; }
    rmdir() { guarded rmdir "$@"; }
    cp() { guarded cp "$@"; }
    mv() { guarded mv "$@"; }
    mkdir() { guarded mkdir "$@"; }
    touch() { guarded touch "$@"; }
    tee() { guarded tee "$@"; }
    truncate() { guarded truncate "$@"; }
    dd() { guarded dd "$@"; }
    # sed changes a file only in place: -i alone, in a cluster (-ni, -Ei,
    # -i.bak) or as --in-place[=SUFFIX] or any unambiguous prefix of it (--i, --in, ...). Then every file it is given must be
    # inside the tree: each operand and each -f/--file script file, but not
    # the script itself (an -e/--expression value, or else the first
    # operand), which is not a path even when it begins with "/".
    sed() {
      local a inplace=0
      for a in "$@"; do
        case $a in --i* | -i* | -[!-]*i*) inplace=1 ;; esac
      done
      if test "$inplace" = 0; then command sed "$@"; return; fi
      local paths=() operands=() ends=0 want='' scripted=0 rest c
      for a in "$@"; do
        case $want in
          e) want=''; continue ;;
          f) want=''; paths+=("$a"); continue ;;
        esac
        if test "$ends" = 0; then
          case $a in
            --) ends=1; continue ;;
            # GNU sed takes any unambiguous prefix of a long option, so
            # --e* is --expression and --fi* is --file. Any other long
            # option but the known ones that take no value is not understood,
            # and so refused rather than skipped past its operands.
            --e*=*) scripted=1; continue ;;
            --e*) scripted=1; want=e; continue ;;
            --fi*=*) scripted=1; paths+=("${a#*=}"); continue ;;
            --fi*) scripted=1; want=f; continue ;;
            --i* | --quiet | --silent | --regexp-extended | --follow-symlinks | --null-data \
              | --zero-terminated | --separate | --sandbox | --debug | --posix | --unbuffered | --binary \
              | --line-length=*) continue ;;
            --*) refuse sed "$@" ;;
            -?*)
              # A cluster: -i's suffix, -e's script and -f's file are
              # attached, or else -e's and -f's are the next argument.
              rest=${a#-}
              while test -n "$rest"; do
                c=${rest:0:1} rest=${rest:1}
                case $c in
                  i | l) break ;;
                  e) scripted=1; test -n "$rest" || want=e; break ;;
                  f) scripted=1; if test -n "$rest"; then paths+=("$rest"); else want=f; fi; break ;;
                esac
              done
              continue
              ;;
          esac
        fi
        operands+=("$a")
      done
      if test "$scripted" = 0 && test "${#operands[@]}" -gt 0; then operands=("${operands[@]:1}"); fi
      paths+=(${operands[@]+"${operands[@]}"})
      inside_work -- ${paths[@]+"${paths[@]}"} || refuse sed "$@"
      command sed "$@"
    }
    etc=$work/etc
    unit_dir=$work/units
    dl_cache=$work/cache
    twsapi_root=$work/opt/twsapi
    adapter_venv=$work/opt/adapter-venv
    adapter_root=$work/opt/adapter
    volume_roots=("$etc" "$work/vol/state" "$work/vol/opt")
    # FAKE_FIND_FAILS=1: find lists what it finds, then fails (an unreadable
    # folder part way through a volume).
    # The stand-in files belong to whoever runs the test, so find is asked for
    # the stand-in old owner (1998:1998, never root's 0) as that real owner,
    # and the cases pass for any caller, root included.
    find() {
      local a=() x prev=''
      for x in "$@"; do
        if test "$prev" = -uid && test "$x" = 1998; then x=$(command id -u); fi
        if test "$prev" = -gid && test "$x" = 1998; then x=$(command id -g); fi
        a+=("$x")
        prev=$x
      done
      command find "${a[@]}"
    }
    if test "${FAKE_FIND_FAILS:-0}" = 1; then
      eval "owned_find() $(declare -f find | tail -n +2)"
      find() { owned_find "$@"; echo "find: a folder could not be read" >&2; return 1; }
    fi
    if test -n "${TEST_LEAK:-}"; then eval "$TEST_LEAK"; fi
    # STEP may carry the step's arguments (reown NAME UID GID).
    # shellcheck disable=SC2086
    main $step
  ) >"$work/out" 2>&1 || status=$?
  if test -z "${TEST_LEAK:-}" && grep -q '^REFUSED' "$work/calls"; then
    fail "provision.sh $step (TI_TARGET=${target:-unset}) ran a file command outside the test's tree: $(grep '^REFUSED' "$work/calls")"
  fi
  return "$status"
}
called() { grep -qF -- "$1" "$work/calls"; }
said() { grep -qF -- "$1" "$work/out"; }

# --- The guard itself: a file command outside the test's tree is refused,
# ends the case, and changes nothing. The canary is outside $work.
canary=$(mktemp -d)
trap 'rm -rf "$work" "$canary"' EXIT
echo keep >"$canary/keep"
# The case runs in $work/cwd, so a relative path is resolved against it: up
# climbs from there to /, whatever the depth of the temporary folder.
up=$(printf '../%.0s' $(seq 1 40))
for leak in "rm -rf $canary/keep" "cp /dev/null $canary/keep" "mv $canary/keep $canary/moved" \
  "mkdir $canary/made" "touch $canary/touched" "sed -i s/keep/gone/ $canary/keep" \
  "rm -rf --one-file-system /opt/ibc-test-target-canary" \
  "rm -f ${up%/}$canary/keep" "cp /dev/null ${up%/}$canary/keep" "touch $work/${up%/}$canary/touched" \
  "mkdir -p -- ${up%/}$canary/made" "rm -f -- $canary/keep" \
  "dd if=/dev/zero of=$canary/keep count=1" "dd if=/dev/zero of=${up%/}$canary/keep count=1" \
  "sed -ni s/keep/gone/p $canary/keep" "sed -Ei s/keep/gone/ $canary/keep" \
  "sed --in-place s/keep/gone/ $canary/keep" "sed --in-place=.bak s/keep/gone/ $canary/keep" \
  "sed -e s/keep/gone/ -i $canary/keep" "cp -t$canary /dev/null" "mv -t$canary $canary/keep" \
  "cp -at$canary /dev/null" "sed -i -f /dev/null $canary/keep" "sed -i -e /^keep/d $canary/keep" \
  "sed -i --expr=s/keep/gone/ $canary/keep" "sed -i --exp s/keep/gone/ $canary/keep" \
  "sed -i --fi=/dev/null $canary/keep" "sed --in-pl s/keep/gone/ $canary/keep" "sed --i s/keep/gone/ $canary/keep" "sed --i=.bak s/keep/gone/ $canary/keep" \
  "sed -i --some-future-option s/keep/gone/ $canary/keep"; do
  status=0
  TEST_LEAK=$leak scenario mac-mini aarch64 services || status=$?
  test "$status" = 97 || fail "the guard did not end the case on '$leak' (status $status)"
  called "REFUSED ${leak%% *}" || fail "the guard did not record '$leak' as refused: $(cat "$work/calls")"
  if called "systemctl"; then fail "the case went on after '$leak' was refused"; fi
done
test "$(cat "$canary/keep" 2>/dev/null)" = keep || fail "a refused command changed the canary"
test ! -e "$canary/moved" && test ! -e "$canary/made" && test ! -e "$canary/touched" && test ! -e "$canary/null" || fail "a refused command created a file outside the test's tree: $(ls "$canary")"
# A path inside the test's tree is allowed, given absolute or relative to
# the case's folder, and as dd's operand.
TEST_LEAK="mkdir -p $work/tmp/allowed" scenario mac-mini aarch64 services || fail "the guard refused a path inside the test's tree: $(cat "$work/out")"
TEST_LEAK="mkdir -p ../tmp/relative" scenario mac-mini aarch64 services || fail "the guard refused a relative path inside the test's tree: $(cat "$work/out")"
test -d "$work/tmp/relative" || fail "the guard did not run an allowed relative mkdir"
TEST_LEAK="dd if=/dev/zero of=$work/tmp/dd count=1" scenario mac-mini aarch64 services || fail "the guard refused dd inside the test's tree: $(cat "$work/out")"
# A sed script is not a path, even one that begins with an address (and so
# with "/"): given alone, with -e, or with --expression, it is not resolved,
# and the file it edits inside the tree is. So is cp's target directory given
# attached to -t. Only the guard's decision is checked: the Mac's own sed and
# cp (BSD) read some of these options differently from the server's (GNU).
for allowed in "sed -i /^drop/d $work/tmp/x" "sed -i -e /^drop/d $work/tmp/x" "sed -i --expression=/^drop/d $work/tmp/x" \
  "sed -i --expr=/^drop/d $work/tmp/x" "sed -i --quiet --regexp-extended /^drop/d $work/tmp/x" \
  "sed -ni /^keep/p $work/tmp/x" "sed -i -- /^drop/d $work/tmp/x" "sed -i -f $work/tmp/script $work/tmp/x" \
  "cp -t$work/tmp /dev/null" "cp -at$work/tmp /dev/null"; do
  status=0
  TEST_LEAK="$allowed || true" scenario mac-mini aarch64 services || status=$?
  if test "$status" = 97 || called "REFUSED"; then fail "the guard refused '$allowed' inside the test's tree: $(cat "$work/out")"; fi
done
# resolve takes every component literally: one holding *, ? or [...] is never
# expanded against the folder's files.
globbed=$(cd "$work/cwd" && : >xfile && resolve '[x]file' && resolve '*' && resolve 'x?ile/..')
expected="$work/cwd/[x]file
$work/cwd/*
$work/cwd"
test "$globbed" = "$expected" || fail "resolve expanded a component as a pattern: $globbed"
rm -f "$work/cwd/xfile"
# No redirection to a path the test does not redirect: a literal absolute
# path other than /dev/null, or a variable other than those set under $work
# above. A redirection cannot be intercepted by a stand-in.
leaks=$(redirect_leaks "$here/../provision.sh")
if test -n "$leaks"; then
  fail "provision.sh writes by redirection to a path the test does not redirect: $leaks. Write under one of the variables the test sets under its own tree ($redirected_vars)"
fi
# The check itself: it flags each shape, and passes the redirected ones.
cat >"$work/redirects" <<'SHAPES'
printf x >/etc/flagged-literal
printf x >> "/var/flagged-quoted"
printf x >"$somewhere/flagged-variable"
printf x 2>${elsewhere}/flagged-braced
printf x >'/etc/flagged-single-quoted'
printf x >~/flagged-tilde
printf x >"$(pick)/flagged-substituted"
printf x >$(pick)-flagged-bare-substituted
printf x >"`pick`/flagged-backtick"
printf x >`pick`-flagged-bare-backtick
printf x >"$etc/fine" 2>/dev/null
printf x >"$unit_dir/fine" >&2
# printf x >/etc/a-comment
SHAPES
got=$(redirect_leaks "$work/redirects")
for shape in flagged-literal flagged-quoted flagged-variable flagged-braced flagged-single-quoted flagged-tilde flagged-substituted flagged-bare-substituted flagged-backtick flagged-bare-backtick; do
  case $got in *"$shape"*) ;; *) fail "the redirection check missed '$shape': $got" ;; esac
done
case $got in *fine* | *a-comment*) fail "the redirection check flagged an allowed line: $got" ;; esac

# --- An unknown target is refused before anything is done.
if scenario nowhere x86_64 services; then fail "an unknown TI_TARGET was accepted"; fi
said "TI_TARGET" || fail "the refusal of an unknown target does not name TI_TARGET: $(cat "$work/out")"
test ! -s "$work/calls" || fail "an unknown target changed something: $(cat "$work/calls")"

# --- Lightsail (the default): unchanged.
if scenario "" aarch64 services; then fail "Lightsail accepted arm64"; fi
said "x86_64 only" || fail "Lightsail's arm64 refusal changed: $(cat "$work/out")"
test ! -s "$work/calls" || fail "Lightsail's arm64 refusal changed something first"
if scenario lightsail aarch64 services; then fail "TI_TARGET=lightsail accepted arm64"; fi

TEST_REHEARSAL=1 scenario "" x86_64 base || fail "Lightsail base failed: $(cat "$work/out")"
called "fetch https://pkgs.tailscale.com" || fail "Lightsail base no longer fetches Tailscale"
called "ufw" || fail "Lightsail base no longer installs ufw"
if called "journald.conf.d"; then fail "Lightsail base changed the journal's settings"; fi

scenario "" x86_64 services || fail "Lightsail services failed: $(cat "$work/out")"
called "systemctl enable ti-gateway.service" || fail "Lightsail no longer enables the Gateway unit"
if called "systemctl mask"; then fail "Lightsail masks a unit"; fi
called "/systemd/ti-gateway.service" || fail "Lightsail no longer installs the Gateway unit"
called "systemctl enable --now ti-data.timer ti-reporter@boost.timer" || fail "Lightsail no longer starts the timers"
grep -q '"host": "127.0.0.1"' "$work/etc/adapters/boost.json" || fail "Lightsail's adapter configuration does not point at 127.0.0.1"

scenario "" x86_64 firewall || fail "Lightsail firewall failed: $(cat "$work/out")"
said "Tailscale is not signed in" || fail "Lightsail's firewall step no longer checks Tailscale: $(cat "$work/out")"

FAKE_SYSTEMD=1 scenario "" x86_64 clock || fail "Lightsail clock failed: $(cat "$work/out")"
called "time-sync.sh" || fail "Lightsail's clock step no longer syncs time where systemd runs"
called "clock-check.sh TI_CLOCK_VERIFY=" || fail "Lightsail's clock step no longer checks the clock"
if called "clock-check.sh TI_CLOCK_VERIFY=1"; then fail "Lightsail's clock step forces a reference check"; fi

status=0
scenario "" x86_64 gateway || status=$?
test "$status" = 75 || fail "Lightsail's gateway step did not stop at its stand-in (status $status): $(cat "$work/out")"
called "fetch https://github.com/IbcAlpha" || fail "Lightsail's gateway step no longer fetches IBC: $(cat "$work/calls")"
# The same step with IBC unpacked, so that it reaches the Gateway installer:
# the installer must start in a folder its user can enter, not the owner's
# checkout (the real one prints "can't cd" there, after a successful install).
status=0
TEST_LEAK='unzip() { record unzip "$@"; }; rm() { record rm "$@"; }; cp() { record cp "$@"; }; touch() { record touch "$@"; }' \
  scenario "" x86_64 gateway || status=$?
# The stand-in's exit ends only the installer's own subshell, so the step goes on
# to its missing-binary refusal.
test "$status" != 0 || fail "Lightsail's gateway step finished although the installer stand-in made nothing: $(cat "$work/out")"
said "ibgateway is missing" || fail "Lightsail's gateway step did not end with its missing-binary refusal: $(cat "$work/out")"
called "STOPPED at runuser" || fail "Lightsail's gateway step never ran the installer: $(cat "$work/calls")"
if grep -q "can't cd" "$work/out"; then fail "Lightsail's gateway installer starts in a folder its user cannot enter: $(cat "$work/out")"; fi

# The download folder is made absolute where it is read, because the installers
# start from "/": a relative TI_DOWNLOAD_CACHE would point somewhere else
# there. An empty one is refused.
rel_cache=$(cd "$work/cwd" && TI_DOWNLOAD_CACHE=rel/cache bash -c '. "$1/deploy/provision.sh"; printf %s "$dl_cache"' _ "$work") || fail "a relative TI_DOWNLOAD_CACHE was refused"
test "$rel_cache" = "$work/cwd/rel/cache" || fail "a relative TI_DOWNLOAD_CACHE is not made absolute (got '$rel_cache')"
if (cd "$work/cwd" && TI_DOWNLOAD_CACHE='' bash -c '. "$1/deploy/provision.sh"' _ "$work") >"$work/out" 2>&1; then fail "an empty TI_DOWNLOAD_CACHE was accepted"; fi
said "TI_DOWNLOAD_CACHE" || fail "the empty TI_DOWNLOAD_CACHE refusal does not name it: $(cat "$work/out")"

# The owner's own ntfy server: fetched against its pin on Lightsail; with no
# Tailscale address it is installed and not configured; an address outside the
# tailnet's range is refused; the Mac mini has none.
scenario "" x86_64 ntfy || fail "Lightsail ntfy failed: $(cat "$work/out")"
called "fetch https://github.com/binwiederhier/ntfy" || fail "Lightsail's ntfy step no longer fetches the pinned server: $(cat "$work/calls")"
said "SKIPPED the ntfy server's configuration" || fail "the ntfy step does not say it waits for Tailscale: $(cat "$work/out")"
if called "ti-ntfy.service"; then fail "the ntfy step installed its unit with no Tailscale address"; fi
for bad in 0.0.0.0 127.0.0.1 203.0.113.5 100.128.0.1 "100.64.0.1:2586" 100.64.999.1 100.64.0.256 100.64.256.0; do
  if TI_NTFY_ADDRESS=$bad scenario "" x86_64 ntfy; then fail "the ntfy step accepted the address '$bad'"; fi
  said "not a Tailscale address" || fail "the ntfy step's refusal of '$bad' does not say why: $(cat "$work/out")"
  if called "ti-ntfy.service" || called "systemctl enable ti-ntfy"; then fail "the ntfy step set the server up for '$bad'"; fi
done
TI_NTFY_ADDRESS=100.100.1.2 scenario "" x86_64 ntfy || fail "the ntfy step refused a Tailscale address: $(cat "$work/out")"
called "/systemd/ti-ntfy.service" || fail "the ntfy step did not install its unit: $(cat "$work/calls")"
called "systemctl enable ti-ntfy.service" || fail "the ntfy step did not enable its unit"
said "http://100.100.1.2:2586" || fail "the ntfy step does not print the server URL for secrets.env: $(cat "$work/out")"
scenario mac-mini aarch64 ntfy || fail "Mac mini ntfy failed: $(cat "$work/out")"
test ! -s "$work/calls" || fail "the Mac mini's ntfy step changed something: $(cat "$work/calls")"

# --- The Mac mini.
if scenario mac-mini x86_64 services; then fail "the Mac mini target accepted x86_64"; fi
said "arm64" || fail "the Mac mini's x86_64 refusal does not say arm64: $(cat "$work/out")"
test ! -s "$work/calls" || fail "the Mac mini's x86_64 refusal changed something first"

FAKE_SYSTEMD=1 scenario mac-mini aarch64 base || fail "Mac mini base failed: $(cat "$work/out")"
if called "tailscale"; then fail "Mac mini base fetches Tailscale"; fi
if called "ufw"; then fail "Mac mini base installs ufw"; fi
called "nano" || fail "Mac mini base installs no editor"
called "journald.conf.d" || fail "Mac mini base does not make the journal persistent"
called "systemd-tmpfiles --create --prefix /var/log/journal" || fail "Mac mini base does not prepare /var/log/journal"
called "systemctl restart systemd-journald.service" || fail "Mac mini base does not restart journald with its new setting"
grep -q 'Storage=persistent' "$work/deploy/provision.sh" || fail "the provisioner never sets Storage=persistent"

scenario mac-mini aarch64 services || fail "Mac mini services failed: $(cat "$work/out")"
called "systemctl mask ti-gateway.service" || fail "Mac mini does not mask the Gateway unit"
if called "systemctl enable ti-gateway.service"; then fail "Mac mini enables the Gateway unit"; fi
if called "/systemd/ti-gateway.service"; then fail "Mac mini installs the Gateway unit, which masking cannot replace"; fi
called "systemctl enable --now ti-data.timer ti-reporter@boost.timer" || fail "Mac mini does not start the timers"
called "systemctl enable ti-dashboard.service" || fail "Mac mini does not enable the dashboard"
called "systemctl disable ti-engine@boost.service" || fail "Mac mini does not keep the engines disabled"
called "systemctl disable ti-adapter-boost.service" || fail "Mac mini does not keep the adapter disabled"
grep -q '"host": "host.docker.internal"' "$work/etc/adapters/boost.json" || fail "Mac mini's adapter configuration does not point at host.docker.internal: $(cat "$work/etc/adapters/boost.json")"
grep -q '"port": 4002' "$work/etc/adapters/boost.json" || fail "Mac mini's adapter configuration lost the paper port"

owner='{"host": "owner-set", "port": 4002}'
FAKE_BOOST_JSON=$owner scenario mac-mini aarch64 services || fail "Mac mini services with an owner's file failed"
test "$(cat "$work/etc/adapters/boost.json")" = "$owner" || fail "Mac mini rewrote the owner's adapter configuration"

scenario mac-mini aarch64 firewall || fail "Mac mini firewall failed: $(cat "$work/out")"
if called "ufw" || called "ip " || called "tailscale"; then fail "Mac mini's firewall step touched the firewall or Tailscale: $(cat "$work/calls")"; fi
said "Mac mini" || fail "Mac mini's firewall step does not say why it skips"

scenario mac-mini aarch64 gateway || fail "Mac mini gateway failed: $(cat "$work/out")"
test ! -s "$work/calls" || fail "Mac mini's gateway step installed something: $(cat "$work/calls")"

FAKE_SYSTEMD=1 scenario mac-mini aarch64 clock || fail "Mac mini clock failed: $(cat "$work/out")"
if called "time-sync.sh"; then fail "Mac mini syncs time in the container, against the Mac's clock"; fi
called "clock-check.sh TI_CLOCK_VERIFY=1" || fail "Mac mini does not check the clock against a reference"
if FAKE_SYSTEMD=1 FAKE_CLOCK_BAD=1 scenario mac-mini aarch64 clock; then fail "Mac mini's clock step finished with a bad clock"; fi

FAKE_SYSTEMD=1 scenario mac-mini aarch64 all || fail "Mac mini all failed: $(cat "$work/out")"
if called "fetch"; then fail "Mac mini all downloaded something (Tailscale or the Gateway): $(grep fetch "$work/calls")"; fi
called "clock-check.sh TI_CLOCK_VERIFY=1" || fail "Mac mini all does not run the clock step"
called "systemctl mask ti-gateway.service" || fail "Mac mini all does not run the services step"

# --- The Mac mini's service users have fixed IDs. /etc/passwd is in the
# container's own disk, not a volume, so a container created again from a
# changed image creates them again; the IDs must be the ones that own the
# files in the volumes, whatever the image's own system users took.
FAKE_PASSWD='' FAKE_SYSTEMD=1 scenario mac-mini aarch64 base || fail "Mac mini base with no service users failed: $(cat "$work/out")"
called "groupadd --system --gid 2002 ti-trader" || fail "Mac mini does not create ti-trader's group at GID 2002: $(grep -E 'useradd|groupadd' "$work/calls")"
called "useradd --system --uid 2002 --gid 2002" || fail "Mac mini does not create ti-trader at UID 2002: $(grep -E 'useradd|groupadd' "$work/calls")"
called "useradd --system --uid 2001 --gid 2001" || fail "Mac mini does not create ti-gateway at UID 2001: $(grep -E 'useradd|groupadd' "$work/calls")"
grep -q '^ti-trader:x:2002:2002:' "$work/passwd" || fail "ti-trader is not 2002:2002 after a Mac mini base: $(cat "$work/passwd")"
# A changed image whose own system user already holds 2002: refused, naming
# it, before any user is created.
shifted='ti-shift:x:2002:2002::/nonexistent:/usr/sbin/nologin'
if FAKE_PASSWD=$shifted FAKE_SYSTEMD=1 scenario mac-mini aarch64 base; then fail "Mac mini accepted UID 2002 held by another account"; fi
said "ti-shift" || fail "the refusal of a taken UID does not name its holder: $(cat "$work/out")"
if called "useradd --system --uid 2002"; then fail "Mac mini created ti-trader over another account's UID"; fi
# ti-operator, the inbox's writer, at 2003 in ti-trader's group, at the base
# step and at the services step that ensures it too.
FAKE_PASSWD='' FAKE_SYSTEMD=1 scenario mac-mini aarch64 base || fail "Mac mini base with no service users failed: $(cat "$work/out")"
called "useradd --system --uid 2003 --gid ti-trader" || fail "Mac mini base does not create ti-operator at UID 2003: $(grep -E 'useradd|groupadd' "$work/calls")"
grep -q '^ti-operator:x:2003:2002:' "$work/passwd" || fail "ti-operator is not 2003 in ti-trader's group after a Mac mini base: $(cat "$work/passwd")"
nooperator='ti-gateway:x:2001:2001::/var/lib/ti-gateway:/usr/sbin/nologin
ti-trader:x:2002:2002::/var/lib/trend-investing:/usr/sbin/nologin'
FAKE_PASSWD=$nooperator scenario mac-mini aarch64 services || fail "Mac mini services without ti-operator failed: $(cat "$work/out")"
called "useradd --system --uid 2003 --gid ti-trader" || fail "Mac mini services does not create ti-operator at UID 2003: $(grep useradd "$work/calls")"
# UID 2003 held by another account: refused, naming it.
taken="$nooperator
ti-other:x:2003:2003::/nonexistent:/usr/sbin/nologin"
if FAKE_PASSWD=$taken FAKE_SYSTEMD=1 scenario mac-mini aarch64 base; then fail "Mac mini accepted UID 2003 held by another account"; fi
said "ti-other" || fail "the refusal of a taken UID 2003 does not name its holder: $(cat "$work/out")"
if called "useradd --system --uid 2003"; then fail "Mac mini created ti-operator over another account's UID"; fi
# ti-operator already there at another UID: refused, with the re-owning pass.
oldop="$nooperator
ti-operator:x:997:2002::/nonexistent:/usr/sbin/nologin"
if FAKE_PASSWD=$oldop FAKE_SYSTEMD=1 scenario mac-mini aarch64 base; then fail "Mac mini accepted ti-operator at UID 997"; fi
said "provision.sh reown ti-operator 997" || fail "the refusal of ti-operator at another UID does not name the re-owning step: $(cat "$work/out")"
# A run cut off between groupadd and useradd left ti-trader's group at 2002
# and no user: the next run reuses that group and creates the user, rather
# than refusing (the provisioner is safe to run again after any partial
# failure). A group of another name at 2002 is still refused.
halfway='ti-gateway:x:2001:2001::/var/lib/ti-gateway:/usr/sbin/nologin'
FAKE_PASSWD=$halfway FAKE_GROUP_EXTRA='ti-trader:x:2002:' FAKE_SYSTEMD=1 scenario mac-mini aarch64 base || fail "Mac mini base after a run cut off between groupadd and useradd failed: $(cat "$work/out")"
called "useradd --system --uid 2002 --gid 2002" || fail "Mac mini did not create ti-trader in its existing group: $(grep -E 'useradd|groupadd' "$work/calls")"
if called "groupadd --system --gid 2002"; then fail "Mac mini created ti-trader's group a second time"; fi
if FAKE_PASSWD=$halfway FAKE_GROUP_EXTRA='ti-other:x:2002:' FAKE_SYSTEMD=1 scenario mac-mini aarch64 base; then fail "Mac mini accepted GID 2002 held by another group"; fi
said "ti-other" || fail "the refusal of a taken GID does not name its holder: $(cat "$work/out")"
# A ti-trader already there at another UID: refused, since the volumes' files
# are owned by 2002.
moved='ti-gateway:x:2001:2001::/var/lib/ti-gateway:/usr/sbin/nologin
ti-trader:x:998:998::/var/lib/trend-investing:/usr/sbin/nologin'
if FAKE_PASSWD=$moved FAKE_SYSTEMD=1 scenario mac-mini aarch64 base; then fail "Mac mini accepted ti-trader at UID 998"; fi
said "998" || fail "the refusal of ti-trader at another UID does not say which: $(cat "$work/out")"
# Its remedy: re-creating the container leaves the volumes' files at the old
# IDs, so the refusal says so and names the re-owning pass.
said "does not re-own" || fail "the refusal of ti-trader at another UID does not say a re-creation leaves the volumes' files: $(cat "$work/out")"
said "provision.sh reown ti-trader 998 998" || fail "the refusal of ti-trader at another UID does not name the re-owning step: $(cat "$work/out")"
# The re-owning step, after the re-creation: every file in the volumes
# owned by the old UID goes to the fixed one and every file of the old GID to
# the fixed group, links not followed; nothing else is touched.
me=1998
mygid=1998
rm -rf "${work:?}/vol"
mkdir -p "$work/vol/state/engines/boost"
: >"$work/vol/state/engines/boost/durable.log"
scenario mac-mini aarch64 "reown ti-trader $me $mygid" || fail "the reown step failed: $(cat "$work/out")"
called "chown -h 2002 $work/vol/state/engines/boost/durable.log" || fail "the reown step did not re-own a file of UID $me: $(cat "$work/calls")"
called "chgrp -h 2002 $work/vol/state/engines/boost/durable.log" || fail "the reown step did not re-group a file of GID $mygid: $(cat "$work/calls")"
said "re-owned" || fail "the reown step does not say what it changed: $(cat "$work/out")"
# Refused: before the re-creation (ti-trader not yet at its fixed ID), on
# Lightsail, for another user name, and with an ID that is not a number.
if FAKE_PASSWD=$moved scenario mac-mini aarch64 "reown ti-trader 998 998"; then fail "reown ran with ti-trader not yet at 2002"; fi
said "up.sh --recreate" || fail "reown before the re-creation does not say to re-create first: $(cat "$work/out")"
if called "chown"; then fail "reown before the re-creation changed a file"; fi
if scenario "" x86_64 "reown ti-trader $me $mygid"; then fail "reown ran on Lightsail"; fi
if called "chown"; then fail "reown on Lightsail changed a file"; fi
if scenario mac-mini aarch64 "reown root 0 0"; then fail "reown accepted root"; fi
if scenario mac-mini aarch64 "reown ti-trader 99x 998"; then fail "reown accepted a UID that is not a number"; fi
# Refused: an old ID that an account holds now, whose files are its own:
# root's 0, a service user's fixed ID, or any other existing account's or
# group's; given as a number however written. Nothing is changed.
withother="$default_passwd
ti-other:x:1500:1600::/nonexistent:/usr/sbin/nologin"
for ids in "0" "0 $mygid" "00" "2001" "2002" "2003" "02003" "$me 0" "$me 2001" "$me 2002" "$me 002002" "99999999999999999999" "0 --force-id" "2002 --force-id" "$me 2001 --force-id"; do
  if FAKE_PASSWD=$withother scenario mac-mini aarch64 "reown ti-trader $ids"; then fail "reown accepted the old ID(s) $ids"; fi
  if called "chown" || called "chgrp"; then fail "reown with the old ID(s) $ids changed a file: $(cat "$work/calls")"; fi
done
# An old ID that merely collides with another account on the new image (a
# system account the image added) is refused with the account named and the
# way forward given; with --force-id it is accepted and the account named.
for ids in "1500" "$me 1600"; do
  if FAKE_PASSWD=$withother scenario mac-mini aarch64 "reown ti-trader $ids"; then fail "reown accepted the colliding old ID(s) $ids without --force-id"; fi
  said "ti-other" || fail "the refusal of the colliding ID(s) $ids does not name the account: $(cat "$work/out")"
  said "--force-id" || fail "the refusal of the colliding ID(s) $ids does not give the way forward: $(cat "$work/out")"
  if called "chown" || called "chgrp"; then fail "reown with the colliding ID(s) $ids changed a file without --force-id"; fi
  FAKE_PASSWD=$withother scenario mac-mini aarch64 "reown ti-trader $ids --force-id" || fail "reown refused the colliding ID(s) $ids with --force-id: $(cat "$work/out")"
  said "ti-other" || fail "reown --force-id does not name the account whose ID it took: $(cat "$work/out")"
  said "re-owned" || fail "reown --force-id did not run: $(cat "$work/out")"
done
if scenario mac-mini aarch64 "reown ti-operator 0"; then fail "reown accepted ti-operator from UID 0"; fi
# A listing that fails is an error, never "re-owned 0 file(s)".
if FAKE_FIND_FAILS=1 scenario mac-mini aarch64 "reown ti-trader $me $mygid"; then fail "reown succeeded although find failed"; fi
if said "re-owned"; then fail "reown reported a count although find failed: $(cat "$work/out")"; fi
said "could not list" || fail "reown does not say the listing failed: $(cat "$work/out")"
# Lightsail is unchanged: the users are created as before, at no fixed ID.
FAKE_PASSWD='' TEST_REHEARSAL=1 scenario "" x86_64 base || fail "Lightsail base with no service users failed: $(cat "$work/out")"
called "useradd --system --user-group --home-dir /var/lib/trend-investing" || fail "Lightsail no longer creates ti-trader as before: $(grep useradd "$work/calls")"
if called "--uid"; then fail "Lightsail now creates a service user at a fixed ID"; fi
called "useradd --system --gid ti-trader --no-create-home" || fail "Lightsail no longer creates ti-operator as before: $(grep useradd "$work/calls")"

# --- The Mac mini's clock is checked every 15 minutes, not only when
# provisioning runs (ADR 0019: a clock behind IBKR's withholds every sell).
scenario mac-mini aarch64 services || fail "Mac mini services failed: $(cat "$work/out")"
called "/mac-mini/systemd/ti-clock-check.service" || fail "Mac mini does not install the clock check's unit"
called "/mac-mini/systemd/ti-clock-check.timer" || fail "Mac mini does not install the clock check's timer"
called "systemctl enable --now ti-clock-check.timer" || fail "Mac mini does not start the clock check's timer"
grep -q '^Environment=TI_CLOCK_VERIFY=1' "$work/deploy/mac-mini/systemd/ti-clock-check.service" || fail "the clock check's unit does not compare with a reference"
grep -q '^OnUnitActiveSec=15min' "$work/deploy/mac-mini/systemd/ti-clock-check.timer" || fail "the clock check's timer is not every 15 minutes"
scenario "" x86_64 services || fail "Lightsail services failed: $(cat "$work/out")"
if called "ti-clock-check"; then fail "Lightsail installs the Mac mini's clock check"; fi

test "$failures" -eq 0 || exit 1
echo "target tests ok"

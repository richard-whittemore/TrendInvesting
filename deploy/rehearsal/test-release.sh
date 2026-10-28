#!/bin/bash
# Tests for deploy/release.sh (the server's `ti-release`): the checksum,
# Linux-program and attestation checks that gate an install, the attestation
# bound to this repository and to the tag's commit, the refusal without gh,
# the refusal while an engine unit is mid-restart (checked again just before
# the swap), the download limits, the install that restarts the dashboard and
# never the engine, an install step that fails partway, the previous build
# kept and re-verified before it is put back, the dashboard that must stay
# active, and --rollback. The release is a local fake release folder reached
# through file://, the tag's commit comes from a fake API folder, and the
# server is a temporary prefix with stand-ins for systemctl, gh, curl and mv
# that record their calls; no network, no root, no systemd. The fake programs
# are ELF headers for this machine followed by text, never executed.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
script=$repo/deploy/release.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
unset TI_RELEASE_URL TI_RELEASE_API TI_PREFIX TI_LINK_DIR TI_SYSTEMCTL TI_GH TI_RELEASE_MAX_BYTES TI_RELEASE_SETTLE TI_REHEARSAL

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
expect() { # description expected actual
  if test "$2" != "$3"; then fail "$1: want '$2', got '$3'"; fi
}
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
str_sha() { printf '%s' "$1" | { if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi; } | cut -c1-40; }

programs="ti-engine ti-reporter ti-dashboard operator journal-query ti-gateway-start"
slug=richard-whittemore/TrendInvesting

# This machine's ELF e_machine as an octal byte, as release.sh expects it.
case $(uname -m) in
  x86_64) elf_byte='\076' other_byte='\267' ;;
  aarch64 | arm64) elf_byte='\267' other_byte='\076' ;;
  *) echo "test-release: no ELF machine for $(uname -m)" >&2; exit 1 ;;
esac
# fake_program FILE TEXT [BYTE]: an ELF header (e_machine BYTE, this machine's
# by default) followed by "TEXT" as the last line.
fake_program() { printf "\177ELF              ${3:-$elf_byte}\000\n#!/bin/sh\n%s\n" "$2" >"$1"; }

# reseal TAG: checksums.txt and the fake attestation bundle (the tag's commit,
# its ref, and the digest of each program) for the release folder as it is.
reseal() {
  local tag=$1 d=$work/rel/$1 p
  : >"$d/checksums.txt"
  for p in $programs; do printf '%s  %s\n' "$(sha "$d/$p")" "$p" >>"$d/checksums.txt"; done
  { printf 'commit %s\nref refs/tags/%s\n' "$(str_sha "commit-$tag")" "$tag"
    for p in $programs; do printf 'digest %s\n' "$(sha "$d/$p")"; done; } >"$d/attestation.sigstore.json"
}
# set_compare TAG BODY: the API's answer to "is the tag's commit on main".
main_sha=$(str_sha main-head)
set_compare() { printf '%s\n' "$2" >"$work/api/repos/$slug/compare/$main_sha...$(str_sha "commit-$1")"; }
# make_release TAG [DASHBOARD_BODY]: a fake release folder with correct
# checksums and bundle, and the API's record of its annotated tag (on main).
make_release() {
  local tag=$1 dash=${2:-} d=$work/rel/$1 p api=$work/api/repos/$slug/git
  mkdir -p "$d" "$api/ref/tags" "$api/ref/heads" "$api/tags" "$work/api/repos/$slug/compare"
  printf '{"ref":"refs/heads/main","object":{"sha":"%s","type":"commit"}}\n' "$main_sha" >"$api/ref/heads/main"
  set_compare "$tag" '{"status":"behind"}'
  for p in $programs; do fake_program "$d/$p" "echo $p $tag"; done
  if test -n "$dash"; then fake_program "$d/ti-dashboard" "echo $dash"; fi
  reseal "$tag"
  printf '{"ref":"refs/tags/%s","object":{"sha":"%s","type":"tag"}}\n' "$tag" "$(str_sha "tagobj-$tag")" >"$api/ref/tags/$tag"
  printf '{"sha":"%s","object":{"sha":"%s","type":"commit"}}\n' "$(str_sha "tagobj-$tag")" "$(str_sha "commit-$tag")" >"$api/tags/$(str_sha "tagobj-$tag")"
}
make_release v1.0.0
make_release v1.1.0
make_release v1.2.0 BAD
make_release v1.3.0
make_release v1.4.0 FLAP

# The stand-in systemctl. Engine units are listed as mid-restart from the
# FAKE_ENGINE_BUSY_FROM-th list-units call on (1 when FAKE_ENGINE_BUSY is
# set); list-units fails when FAKE_LIST_FAIL is set. The dashboard is active
# unless the installed dashboard program says BAD; one that says FLAP is
# active once after each restart and then stops.
mkdir -p "$work/stub"
cat >"$work/stub/systemctl" <<'STUB'
#!/bin/bash
echo "$*" >>"$FAKE_CTL_LOG"
case $1 in
  list-units)
    test -z "${FAKE_LIST_FAIL:-}" || { echo "Failed to connect to bus" >&2; exit 1; }
    n=$(($(cat "$FAKE_CTL_LOG.lists" 2>/dev/null || echo 0) + 1)); echo "$n" >"$FAKE_CTL_LOG.lists"
    from=${FAKE_ENGINE_BUSY_FROM:-}
    if test -n "${FAKE_ENGINE_BUSY:-}"; then from=1; fi
    if test -n "$from" && test "$n" -ge "$from"; then echo "ti-engine@boost.service loaded deactivating stop"; fi ;;
  is-active)
    if grep -q BAD "$TI_PREFIX/bin/ti-dashboard"; then exit 3; fi
    if grep -q FLAP "$TI_PREFIX/bin/ti-dashboard"; then
      n=$(($(cat "$FAKE_CTL_LOG.active" 2>/dev/null || echo 0) + 1)); echo "$n" >"$FAKE_CTL_LOG.active"
      test "$n" -le 1
    fi ;;
  restart) rm -f "$FAKE_CTL_LOG.active" ;;
  daemon-reload) ;;
  *) echo "unexpected systemctl $*" >&2; exit 9 ;;
esac
STUB
# The stand-in gh: only `attestation verify FILE` with this repository's
# release workflow as the signer, the bundle, the tag's ref and commit, and no
# self-hosted runners; it passes when the bundle names that commit and ref and
# holds FILE's digest, as the real attestation does. Any other call fails.
cat >"$work/stub/gh" <<'STUB'
#!/bin/bash
echo "$*" >>"$FAKE_GH_LOG"
test -z "${FAKE_GH_FAIL:-}" || exit 1
test -z "${FAKE_GH_STDERR:-}" || { echo "$FAKE_GH_STDERR" >&2; exit 1; }
test "$1 $2" = "attestation verify" || { echo "unexpected gh $*" >&2; exit 9; }
file=$3; shift 3
repo= workflow= bundle= ref= digest= deny=
while test $# -gt 0; do
  case $1 in
    --repo) repo=$2; shift 2 ;;
    --signer-workflow) workflow=$2; shift 2 ;;
    --bundle) bundle=$2; shift 2 ;;
    --source-ref) ref=$2; shift 2 ;;
    --source-digest) digest=$2; shift 2 ;;
    --deny-self-hosted-runners) deny=1; shift ;;
    *) echo "unexpected gh flag $1" >&2; exit 9 ;;
  esac
done
slug=richard-whittemore/TrendInvesting
test "$repo" = "$slug" || { echo "gh: --repo is '$repo'" >&2; exit 8; }
test "$workflow" = "$slug/.github/workflows/release.yml" || { echo "gh: --signer-workflow is '$workflow'" >&2; exit 8; }
test -n "$deny" || { echo "gh: self-hosted runners not denied" >&2; exit 8; }
test -f "$bundle" || { echo "gh: no bundle" >&2; exit 8; }
grep -qx "commit $digest" "$bundle" || { echo "gh: source digest $digest does not match" >&2; exit 1; }
grep -qx "ref $ref" "$bundle" || { echo "gh: source ref $ref does not match" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then d=$(sha256sum "$file" | cut -d' ' -f1); else d=$(shasum -a 256 "$file" | cut -d' ' -f1); fi
grep -qx "digest $d" "$bundle" || { echo "gh: $file is not attested" >&2; exit 1; }
echo "verify $(basename "$file")" >>"$FAKE_GH_LOG.verified"
STUB
# The stand-in curl and mv: each records its arguments and runs the real one;
# mv fails the first FAKE_MV_FAIL_N (default 1) times it would rename onto the
# path in FAKE_MV_FAIL; every rename onto a path starting with FAKE_MV_FAIL2
# fails.
real_curl=$(command -v curl)
real_mv=$(command -v mv)
mkdir -p "$work/path"
cat >"$work/path/curl" <<STUB
#!/bin/bash
echo "\$*" >>"\$FAKE_CURL_LOG"
exec "$real_curl" "\$@"
STUB
cat >"$work/path/mv" <<STUB
#!/bin/bash
last=\${*: -1}
if test -n "\${FAKE_MV_FAIL2:-}" && [[ "\$last" == "\$FAKE_MV_FAIL2"* ]]; then
  echo "mv: No space left on device" >&2
  exit 1
fi
if test -n "\${FAKE_MV_FAIL:-}" && test "\$last" = "\$FAKE_MV_FAIL" && test "\$(cat "\$FAKE_MV_FAIL.failed-once" 2>/dev/null || echo 0)" -lt "\${FAKE_MV_FAIL_N:-1}"; then
  echo \$((\$(cat "\$FAKE_MV_FAIL.failed-once" 2>/dev/null || echo 0) + 1)) >"\$FAKE_MV_FAIL.failed-once"
  echo "mv: No space left on device" >&2
  exit 1
fi
exec "$real_mv" "\$@"
STUB
real_rm=$(command -v rm)
cat >"$work/path/rm" <<STUB
#!/bin/bash
for a in "\$@"; do
  if test -n "\${FAKE_RM_FAIL:-}" && [[ "\$a" == \$FAKE_RM_FAIL ]]; then
    echo "rm: cannot remove: Operation not permitted" >&2
    exit 1
  fi
done
exec "$real_rm" "\$@"
STUB
chmod +x "$work/stub/systemctl" "$work/stub/gh" "$work/path/curl" "$work/path/mv" "$work/path/rm"
export PATH="$work/path:$PATH"

# fresh_server: a prefix with an earlier hand-installed build, like a server
# provisioned before the first release.
fresh_server() {
  export TI_PREFIX=$work/server TI_LINK_DIR=$work/server/usr-local-bin
  rm -rf "$TI_PREFIX"
  mkdir -p "$TI_PREFIX/bin" "$TI_LINK_DIR"
  local p
  for p in $programs; do fake_program "$TI_PREFIX/bin/$p" "echo $p hand"; chmod 0755 "$TI_PREFIX/bin/$p"; done
  : >"$work/ctl.log"
  rm -f "$work/ctl.log.lists" "$work/ctl.log.active"
  : >"$work/gh.log"
  : >"$work/gh.log.verified"
  : >"$work/curl.log"
}
export TI_RELEASE_URL=file://$work/rel TI_RELEASE_API=file://$work/api TI_RELEASE_NO_ROOT=1 TI_RELEASE_ANY_ARCH=1
export TI_SYSTEMCTL=$work/stub/systemctl TI_GH=$work/stub/gh TI_RELEASE_SETTLE=0
export FAKE_CTL_LOG=$work/ctl.log FAKE_GH_LOG=$work/gh.log FAKE_CURL_LOG=$work/curl.log

# run ARGS...: the script's status; its output is in $work/out.
run() { local s=0; rm -f "$work/ctl.log.lists"; "$script" "$@" >"$work/out" 2>&1 || s=$?; echo "$s"; }
installed() { tail -1 "$TI_PREFIX/bin/$1"; }
state() { cat "$TI_PREFIX/releases/$1" 2>/dev/null || echo none; }
nonzero() { test "$1" -ne 0 && echo nonzero || echo "$1"; }
not_current() { if test "$(state current)" = "$2"; then fail "$1: current is $2"; fi; }
all_are() { # description build: every program in bin is that build's
  local p
  for p in $programs; do expect "$1: $p" "echo $p $2" "$(installed "$p")"; done
}

fresh_server
expect "no argument is refused" nonzero "$(nonzero "$(run)")"
for bad in ../x main v1/../../x "v1 0" "" --unverified; do
  expect "tag '$bad' is refused" nonzero "$(nonzero "$(run "$bad")")"
done
expect "nothing downloaded for a bad tag" none "$(state current)"

# An engine unit mid-restart: refused, nothing installed or restarted.
expect "an engine mid-restart is refused" nonzero "$(nonzero "$(FAKE_ENGINE_BUSY=1 run v1.0.0)")"
grep -qi "mid-restart" "$work/out" || fail "the refusal does not say why: $(cat "$work/out")"
grep -q "ti-engine@boost" "$work/out" || fail "the refusal does not name the unit"
expect "no program installed" "echo ti-dashboard hand" "$(installed ti-dashboard)"
if grep -q '^restart' "$work/ctl.log"; then fail "something was restarted despite the refusal"; fi

# systemd that cannot be asked counts as busy (fail closed).
expect "a failing systemctl list-units is refused" nonzero "$(nonzero "$(FAKE_LIST_FAIL=1 run v1.0.0)")"
expect "nothing installed when systemd cannot be asked" "echo ti-dashboard hand" "$(installed ti-dashboard)"
expect "no release recorded when systemd cannot be asked" none "$(state current)"

# An engine that starts restarting during the download is caught by the check
# just before the swap.
expect "an engine that goes mid-restart during the download is refused" nonzero "$(nonzero "$(FAKE_ENGINE_BUSY_FROM=2 run v1.0.0)")"
grep -qi "mid-restart" "$work/out" || fail "the late refusal does not say why: $(cat "$work/out")"
expect "nothing installed after the late refusal" "echo ti-dashboard hand" "$(installed ti-dashboard)"
not_current "no release recorded after the late refusal" v1.0.0
if grep -q '^restart' "$work/ctl.log"; then fail "something was restarted despite the late refusal"; fi

# A corrupted program: checksum mismatch, bin untouched.
make_release v0.9.0
printf 'tampered\n' >>"$work/rel/v0.9.0/operator"
expect "a checksum mismatch is refused" nonzero "$(nonzero "$(run v0.9.0)")"
grep -q "operator" "$work/out" || fail "the mismatch does not name the program"
expect "bin untouched after a mismatch" "echo operator hand" "$(installed operator)"
not_current "no release recorded after a mismatch" v0.9.0

# A program missing from checksums.txt.
make_release v0.9.1
grep -v journal-query "$work/rel/v0.9.1/checksums.txt" >"$work/c" && mv "$work/c" "$work/rel/v0.9.1/checksums.txt"
expect "an unlisted program is refused" nonzero "$(nonzero "$(run v0.9.1)")"

# A program that is not a Linux program for this machine, with matching
# checksums and attestation: refused the way provision.sh services refuses it.
make_release v0.9.2
fake_program "$work/rel/v0.9.2/ti-engine" "echo ti-engine other-arch" "$other_byte"
reseal v0.9.2
expect "a program for another machine is refused" nonzero "$(nonzero "$(run v0.9.2)")"
grep -q "ti-engine" "$work/out" || fail "the architecture refusal does not name the program: $(cat "$work/out")"
expect "bin untouched after an architecture refusal" "echo ti-engine hand" "$(installed ti-engine)"
make_release v0.9.3
printf '#!/bin/sh\necho not elf\n' >"$work/rel/v0.9.3/operator"
reseal v0.9.3
expect "a program that is not ELF is refused" nonzero "$(nonzero "$(run v0.9.3)")"
expect "bin untouched after a non-ELF refusal" "echo operator hand" "$(installed operator)"
grep -q 'elf_machine()' "$script" || fail "release.sh has no elf_machine check"
expect "release.sh's ELF check is provision.sh's" \
  "$(sed -n '/^elf_machine() {/,/^}/p' "$repo/deploy/provision.sh")" \
  "$(sed -n '/^elf_machine() {/,/^}/p' "$script")"

# An attestation that does not verify.
expect "a failed attestation is refused" nonzero "$(nonzero "$(FAKE_GH_FAIL=1 run v1.0.0)")"
expect "bin untouched after a failed attestation" "echo ti-dashboard hand" "$(installed ti-dashboard)"

# A consistent tamper: a program and its checksum line both replaced. The
# checksums pass; the attestation does not hold its digest.
make_release v0.9.4
fake_program "$work/rel/v0.9.4/operator" "echo operator evil"
awk -v s="$(sha "$work/rel/v0.9.4/operator")" '$2 == "operator" { $1 = s } { print $1 "  " $2 }' "$work/rel/v0.9.4/checksums.txt" >"$work/c" && mv "$work/c" "$work/rel/v0.9.4/checksums.txt"
expect "a consistent tamper is refused" nonzero "$(nonzero "$(run v0.9.4)")"
expect "bin untouched after a consistent tamper" "echo operator hand" "$(installed operator)"

# An older release's programs and bundle served under a newer tag: the
# attestation names the older tag's commit, not this tag's, so it is refused.
make_release v0.9.5
cp "$work/rel/v1.0.0/"* "$work/rel/v0.9.5/"
expect "an older release substituted under a newer tag is refused" nonzero "$(nonzero "$(run v0.9.5)")"
expect "bin untouched after a substitution" "echo ti-engine hand" "$(installed ti-engine)"
not_current "no release recorded after a substitution" v0.9.5

# Without gh: refused, nothing installed, saying how to install it.
expect "without gh the install is refused" nonzero "$(nonzero "$(TI_GH=$work/stub/no-such-gh run v1.0.0)")"
grep -q "gh" "$work/out" || fail "the refusal without gh does not name gh: $(cat "$work/out")"
expect "bin untouched without gh" "echo ti-engine hand" "$(installed ti-engine)"
not_current "no release recorded without gh" v1.0.0

# A tag moved to a commit that is not on main (the workflow at that commit is
# the attacker's, so only the server can check): refused unless the API says
# the commit is identical to or behind main, and on any answer it cannot read.
make_release v0.9.6
for st in ahead diverged; do
  set_compare v0.9.6 "{\"status\":\"$st\"}"
  expect "a tag whose commit is $st of main is refused" nonzero "$(nonzero "$(run v0.9.6)")"
  grep -q "main" "$work/out" || fail "the $st refusal does not say main: $(cat "$work/out")"
  expect "bin untouched when the commit is $st of main" "echo ti-engine hand" "$(installed ti-engine)"
  not_current "no release recorded when the commit is $st of main" v0.9.6
done
set_compare v0.9.6 '{"status":"identical"}'
expect "a tag at main's head installs" 0 "$(run v0.9.6)"
fresh_server
for body in 'not json' '[]' '{}' '{"status":["behind"]}' '{"status":"behindish"}' ''; do
  set_compare v0.9.6 "$body"
  expect "a compare answer '$body' is refused" nonzero "$(nonzero "$(run v0.9.6)")"
  expect "bin untouched after the compare answer '$body'" "echo ti-engine hand" "$(installed ti-engine)"
done
# The branch lookup is checked as strictly: a tag object, a short or non-hex
# sha, missing fields and a non-JSON answer are all refused as unreadable.
good_main=$(cat "$work/api/repos/$slug/git/ref/heads/main")
set_compare v0.9.6 '{"status":"identical"}'
for body in 'not json' '{}' '[]' '' '{"object":{"sha":"'"$main_sha"'","type":"tag"}}' '{"object":{"sha":"abc123","type":"commit"}}' '{"object":{"sha":"'"$main_sha"'0","type":"commit"}}' '{"object":{"sha":"'"${main_sha:0:39}"'","type":"commit"}}' '{"object":{"sha":"'"$(echo "$main_sha" | tr a-f A-F)"'","type":"commit"}}' '{"object":{"type":"commit"}}' '{"object":{"sha":"'"$main_sha"'"}}' '{"object":"x"}'; do
  printf '%s\n' "$body" >"$work/api/repos/$slug/git/ref/heads/main"
  expect "a main lookup answer '$body' is refused" nonzero "$(nonzero "$(run v0.9.6)")"
  grep -q "whether .* is on main.*try again later" "$work/out" || fail "the main lookup answer '$body' does not say to try again later: $(cat "$work/out")"
  expect "bin untouched after the main lookup answer '$body'" "echo ti-engine hand" "$(installed ti-engine)"
done
printf '%s\n' "$good_main" >"$work/api/repos/$slug/git/ref/heads/main"
rm -f "$work/api/repos/$slug/compare/$main_sha...$(str_sha commit-v0.9.6)"
expect "no compare answer is refused" nonzero "$(nonzero "$(run v0.9.6)")"
not_current "no release recorded without a compare answer" v0.9.6

# A tag named main must not stand in for the branch: the comparison is made
# against the branch's own commit, so a tag called main that makes the bare
# name compare as behind does not let an off-main commit through.
make_release v0.9.8
set_compare v0.9.8 '{"status":"diverged"}'
printf '{"status":"behind"}\n' >"$work/api/repos/$slug/compare/main...$(str_sha commit-v0.9.8)"
fresh_server
expect "a tag called main does not vouch for an off-main commit" nonzero "$(nonzero "$(run v0.9.8)")"
expect "bin untouched under a shadowing main tag" "echo ti-engine hand" "$(installed ti-engine)"
rm -f "$work/api/repos/$slug/compare/main...$(str_sha commit-v0.9.8)"

# A rate-limited or unreachable API is told apart from a commit that is not on
# main: the first says to try again later, the second says what the status is.
make_release v0.9.9
set_compare v0.9.9 '{"status":"diverged"}'
fresh_server
expect "a commit off main is refused" nonzero "$(nonzero "$(run v0.9.9)")"
grep -q "not on main" "$work/out" || fail "the off-main refusal does not say so: $(cat "$work/out")"
if grep -qi "try again" "$work/out"; then fail "the off-main refusal suggests retrying: $(cat "$work/out")"; fi
rm -f "$work/api/repos/$slug/compare/$main_sha...$(str_sha commit-v0.9.9)"
expect "an unreadable compare answer is refused" nonzero "$(nonzero "$(run v0.9.9)")"
grep -qi "try again later" "$work/out" || fail "the unreachable-API refusal has no retry hint: $(cat "$work/out")"
if grep -q "not on main" "$work/out"; then fail "the unreachable-API refusal claims the commit is off main: $(cat "$work/out")"; fi
set_compare v0.9.9 '{"status":"behind"}'

# A malformed tag answer never reaches gh as a source digest.
api=$work/api/repos/$slug/git
make_release v0.9.7
for body in '[]' 'not json' '{"object":{"type":"commit","sha":"../../x"}}' '{"object":{"type":"commit","sha":"abc123"}}' \
  "{\"object\":{\"type\":\"tag\",\"sha\":\"$(str_sha tagself)\"}}"; do
  printf '%s\n' "$body" >"$api/ref/tags/v0.9.7"
  printf '%s\n' "$body" >"$api/tags/$(str_sha tagself)"
  : >"$work/gh.log"
  expect "a malformed tag answer '$body' is refused" nonzero "$(nonzero "$(run v0.9.7)")"
  expect "gh not called for the tag answer '$body'" 0 "$(wc -l <"$work/gh.log" | tr -d ' ')"
  expect "bin untouched after the tag answer '$body'" "echo ti-engine hand" "$(installed ti-engine)"
  not_current "no release recorded after the tag answer '$body'" v0.9.7
done

# gh's own message reaches the operator (an old gh, a login prompt, no trust
# root) so a tooling fault is not reported as a tampered release; a token in
# it never does.
expect "a gh that fails with a message is refused" nonzero "$(nonzero "$(FAKE_GH_STDERR='unknown flag: --source-ref token ghp_abcdefghijklmnopqrstuvwxyz0123456789ab' run v1.0.0)")"
grep -q "unknown flag: --source-ref" "$work/out" || fail "gh's message is not shown: $(cat "$work/out")"
if grep -q "ghp_abcdefghijklmnop" "$work/out"; then fail "a token in gh's message was printed"; fi
expect "bin untouched after a gh failure" "echo ti-engine hand" "$(installed ti-engine)"

: >"$work/curl.log"
expect "a download over the size cap is refused" nonzero "$(nonzero "$(TI_RELEASE_MAX_BYTES=20 run v1.0.0)")"
expect "bin untouched after an oversized download" "echo ti-engine hand" "$(installed ti-engine)"
test -s "$work/curl.log" || fail "curl was not called"
if grep -v -- '--max-time' "$work/curl.log"; then fail "a download has no --max-time"; fi
if grep -v -- '--max-filesize' "$work/curl.log"; then fail "a download has no --max-filesize"; fi

# An install step that fails partway (a rename onto bin/ti-reporter): the
# install stops, what was swapped is put back, the release is not recorded.
expect "an install whose rename fails is nonzero" nonzero "$(nonzero "$(FAKE_MV_FAIL=$TI_PREFIX/bin/ti-reporter run v1.0.0)")"
all_are "a failed rename puts the hand build back" hand
not_current "no release recorded after a failed rename" v1.0.0
rm -f "$TI_PREFIX/bin/ti-reporter.failed-once"

# The save step after a successful install fails (a rename onto the kept
# build's folder): the old build is put back and the release is not recorded,
# so asking for the tag again installs it.
fresh_server
expect "a release whose save fails is nonzero" nonzero "$(nonzero "$(FAKE_MV_FAIL=$TI_PREFIX/releases/v1.0.0 run v1.0.0)")"
all_are "a failed save puts the hand build back" hand
not_current "no release recorded after a failed save" v1.0.0
rm -f "$TI_PREFIX/releases/v1.0.0.failed-once"
expect "the tag installs when asked again" 0 "$(run v1.0.0)"
all_are "the tag after a failed save" v1.0.0

# A save that fails, with the kept build moved back: the TAG note beside it is
# removed from its new place, and a failing removal never decides the outcome.
fresh_server
expect "v1.0.0 installs for the TAG case" 0 "$(run v1.0.0)"
expect "v1.1.0 installs for the TAG case" 0 "$(run v1.1.0)"
expect "a failed save is nonzero (TAG removed)" nonzero "$(nonzero "$(FAKE_MV_FAIL=$TI_PREFIX/releases/v1.0.0 run v1.0.0)")"
test -f "$TI_PREFIX/releases/v1.0.0/checksums.txt" || fail "the kept build was not moved back"
test ! -e "$TI_PREFIX/releases/v1.0.0/TAG" || fail "a stray TAG is left in the moved-back build"
rm -f "$TI_PREFIX/releases/v1.0.0.failed-once"
fresh_server
expect "v1.0.0 installs for the TAG rm case" 0 "$(run v1.0.0)"
expect "v1.1.0 installs for the TAG rm case" 0 "$(run v1.1.0)"
expect "a failed save with a failing TAG removal is nonzero" nonzero "$(nonzero "$(FAKE_RM_FAIL="$TI_PREFIX/releases/*/TAG" FAKE_MV_FAIL=$TI_PREFIX/releases/v1.0.0 run v1.0.0)")"
test -f "$TI_PREFIX/releases/v1.0.0/checksums.txt" || fail "a failing TAG removal lost the only copy of the build: $(cat "$work/out")"
test ! -e "$TI_PREFIX/releases/.replaced" || fail "the build is still at .replaced after a successful move back"
rm -f "$TI_PREFIX/releases/v1.0.0.failed-once"

# The save fails and the kept build cannot be moved back either (the folder of
# the tag being released again held the previous build): that build is kept
# under a .rescue name, an ALERT names it, and the exit is non-zero.
fresh_server
expect "v1.0.0 installs" 0 "$(run v1.0.0)"
expect "v1.1.0 installs" 0 "$(run v1.1.0)"
expect "previous is v1.0.0" v1.0.0 "$(state previous)"
expect "a failed save with a failed restore is nonzero" nonzero "$(nonzero "$(FAKE_MV_FAIL=$TI_PREFIX/releases/v1.0.0 FAKE_MV_FAIL_N=2 run v1.0.0)")"
grep -q "ALERT" "$work/out" || fail "no ALERT for the lost build: $(cat "$work/out")"
grep -q "$TI_PREFIX/releases/.rescue-v1.0.0" "$work/out" || fail "the ALERT does not name the rescued build: $(cat "$work/out")"
test -f "$TI_PREFIX/releases/.rescue-v1.0.0/checksums.txt" || fail "the kept build was deleted"
rm -f "$TI_PREFIX/releases/v1.0.0.failed-once"

# An earlier incident left a .rescue folder for the tag: the rescue goes to a
# new name, never inside the old folder.
fresh_server
expect "v1.0.0 installs again" 0 "$(run v1.0.0)"
expect "v1.1.0 installs again" 0 "$(run v1.1.0)"
mkdir -p "$TI_PREFIX/releases/.rescue-v1.0.0"
echo old >"$TI_PREFIX/releases/.rescue-v1.0.0/marker"
expect "a failed save beside an old rescue is nonzero" nonzero "$(nonzero "$(FAKE_MV_FAIL=$TI_PREFIX/releases/v1.0.0 FAKE_MV_FAIL_N=2 run v1.0.0)")"
test -f "$TI_PREFIX/releases/.rescue-v1.0.0/marker" || fail "the old rescue folder was disturbed"
test ! -e "$TI_PREFIX/releases/.rescue-v1.0.0/.replaced" || fail "the kept build was moved inside the old rescue folder"
rescued=$(find "$TI_PREFIX/releases" -maxdepth 1 -name ".rescue-v1.0.0.*" | head -1)
test -n "$rescued" && test -f "$rescued/checksums.txt" || fail "the kept build is not under a new rescue name"
grep -q "$rescued" "$work/out" || fail "the ALERT does not name $rescued: $(cat "$work/out")"
rm -f "$TI_PREFIX/releases/v1.0.0.failed-once"
rm -rf "$TI_PREFIX"/releases/.rescue-v1.0.0*

# The rescue rename fails too: the kept build stays at .replaced, the ALERT
# names it, and the next release refuses to start until it is moved aside.
fresh_server
expect "v1.0.0 installs a third time" 0 "$(run v1.0.0)"
expect "v1.1.0 installs a third time" 0 "$(run v1.1.0)"
expect "a failed save with no rescue is nonzero" nonzero "$(nonzero "$(FAKE_MV_FAIL=$TI_PREFIX/releases/v1.0.0 FAKE_MV_FAIL_N=2 FAKE_MV_FAIL2=$TI_PREFIX/releases/.rescue run v1.0.0)")"
grep -q "ALERT" "$work/out" || fail "no ALERT when the rescue also failed: $(cat "$work/out")"
grep -q "$TI_PREFIX/releases/.replaced" "$work/out" || fail "the ALERT does not name .replaced: $(cat "$work/out")"
test -f "$TI_PREFIX/releases/.replaced/checksums.txt" || fail "the kept build did not survive the exit trap"
rm -f "$TI_PREFIX/releases/v1.0.0.failed-once"
expect "a later release refuses while .replaced exists" nonzero "$(nonzero "$(run v1.3.0)")"
if ! grep -q "ALERT" "$work/out" || ! grep -q "$TI_PREFIX/releases/.replaced" "$work/out"; then fail "the refusal does not name .replaced: $(cat "$work/out")"; fi
test -f "$TI_PREFIX/releases/.replaced/checksums.txt" || fail "a later release deleted the kept build"
expect "bin untouched by the refused release" "echo ti-engine v1.1.0" "$(installed ti-engine)"
# The tag of the kept build is recorded beside it, and the refusal names it.
expect "the kept build's tag is recorded beside it" v1.0.0 "$(cat "$TI_PREFIX/releases/.replaced/TAG" 2>/dev/null || echo none)"
grep -q "(tag v1.0.0)" "$work/out" || fail "the refusal does not name the kept build's tag: $(cat "$work/out")"

# --status is read-only: it prints the tags and what is kept, takes no lock
# and works while an engine unit is mid-restart.
expect "--status succeeds with a kept build" 0 "$(FAKE_ENGINE_BUSY=1 run --status)"
grep -q "current: v1.1.0" "$work/out" || fail "--status does not print the current tag: $(cat "$work/out")"
grep -q "previous: v1.0.0" "$work/out" || fail "--status does not print the previous tag: $(cat "$work/out")"
grep -q "\.replaced: kept build v1.0.0" "$work/out" || fail "--status does not report .replaced and its tag: $(cat "$work/out")"
mkdir -p "$TI_PREFIX/releases/.rescue-v9.9.9"
expect "--status succeeds with a rescue" 0 "$(run --status)"
grep -q "\.rescue-v9.9.9" "$work/out" || fail "--status does not list the rescue folder: $(cat "$work/out")"
mkdir -p "$TI_PREFIX/releases/.aside-v9.9.8"
expect "--status succeeds with a parked copy" 0 "$(run --status)"
grep -q "\.aside-v9.9.8" "$work/out" || fail "--status does not list the parked copy: $(cat "$work/out")"
rmdir "$TI_PREFIX/releases/.aside-v9.9.8"
mkdir "$TI_PREFIX/releases/.lock"
echo 999999 >"$TI_PREFIX/releases/.lock/pid"
# A stand-in /proc: a plain user cannot signal a root release, so liveness is
# the pid's entry here and what its command line is.
export TI_PROC=$work/proc
rm -rf "$TI_PROC"; mkdir -p "$TI_PROC"
expect "--status with a lock held" 0 "$(run --status)"
grep -q "lock: held at $TI_PREFIX/releases/.lock" "$work/out" || fail "--status does not report the lock: $(cat "$work/out")"
grep -q "pid 999999" "$work/out" || fail "--status does not name the lock's pid: $(cat "$work/out")"
grep -q "not running" "$work/out" || fail "--status does not say the lock is stale: $(cat "$work/out")"
if grep -q "belongs to the running release" "$work/out"; then fail "--status calls .replaced a running release's when the lock is stale: $(cat "$work/out")"; fi
grep -q "left by a run that died" "$work/out" || fail "--status does not say .replaced was left by a dead run when the pid is absent: $(cat "$work/out")"
# A live release owned by another user (a pid we cannot signal) is running.
mkdir -p "$TI_PROC/999999"
printf '/bin/bash\0/usr/local/sbin/ti-release\0v1.2.3\0' >"$TI_PROC/999999/cmdline"
expect "--status with a live lock" 0 "$(run --status)"
grep -q "pid 999999 is running" "$work/out" || fail "--status does not say a live release is running: $(cat "$work/out")"
if grep -q "stale" "$work/out"; then fail "--status calls a live lock stale: $(cat "$work/out")"; fi
expect "--status with a live lock and .replaced" 0 "$(run --status)"
grep -q "belongs to the running release" "$work/out" || fail "--status does not say .replaced belongs to the running release: $(cat "$work/out")"
# A reused pid (some other program) is a stale lock.
printf 'sleep\0100\0' >"$TI_PROC/999999/cmdline"
expect "--status with a reused pid" 0 "$(run --status)"
grep -q "pid reused; the lock is stale" "$work/out" || fail "--status does not report a reused pid: $(cat "$work/out")"
expect "--status with a stale lock and .replaced" 0 "$(run --status)"
grep -q "left by a run that died" "$work/out" || fail "--status does not say .replaced was left by a dead run: $(cat "$work/out")"
# A reused pid whose command line merely contains the script's name in an
# argument (an editor opened on it) is not the release.
for words in 'vim\0/usr/local/sbin/ti-release\0' 'less\0deploy/release.sh\0' 'journalctl\0-t\0ti-release\0'; do
  printf '%b' "$words" >"$TI_PROC/999999/cmdline"
  expect "--status with a pid reused by a program naming the script" 0 "$(run --status)"
  grep -q "pid reused; the lock is stale" "$work/out" || fail "--status takes a program that only names the script for the release ($words): $(cat "$work/out")"
done
# bash running the script, with the script as argv[1], is the release.
printf '/bin/bash\0deploy/release.sh\0v1.2.3\0' >"$TI_PROC/999999/cmdline"
expect "--status with the script as argv[1]" 0 "$(run --status)"
grep -q "pid 999999 is running" "$work/out" || fail "--status misses a release run as bash release.sh: $(cat "$work/out")"
# A shell option before the script (bash -x) does not hide the release.
printf '/bin/bash\0-x\0/usr/local/sbin/ti-release\0v1.2.3\0' >"$TI_PROC/999999/cmdline"
expect "--status with a shell option before the script" 0 "$(run --status)"
grep -q "pid 999999 is running" "$work/out" || fail "--status misses a release run as bash -x ti-release: $(cat "$work/out")"
printf '/bin/bash\0-e\0-x\0deploy/release.sh\0v1.2.3\0' >"$TI_PROC/999999/cmdline"
expect "--status with two shell options before the script" 0 "$(run --status)"
grep -q "pid 999999 is running" "$work/out" || fail "--status misses a release run as bash -e -x release.sh: $(cat "$work/out")"
# A shell option that takes an argument (bash -o pipefail) does not make that
# argument the script: the release is the word after it.
printf '/bin/bash\0-o\0pipefail\0/usr/local/sbin/ti-release\0v1\0' >"$TI_PROC/999999/cmdline"
expect "--status with a shell option that takes an argument" 0 "$(run --status)"
grep -q "pid 999999 is running" "$work/out" || fail "--status misses a release run as bash -o pipefail ti-release: $(cat "$work/out")"
# A short-option cluster ending in o or O, and the +o/+O forms, take the next
# word as their argument exactly as bare -o/-O do; a cluster not ending in o
# does not.
for words in '/bin/bash\0-euo\0pipefail\0/usr/local/sbin/ti-release\0v1\0' \
  '/bin/bash\0+o\0errexit\0/usr/local/sbin/ti-release\0' \
  '/bin/bash\0-xO\0extglob\0/usr/local/sbin/ti-release\0' \
  '/bin/bash\0-eu\0/usr/local/sbin/ti-release\0'; do
  printf '%b' "$words" >"$TI_PROC/999999/cmdline"
  expect "--status with an option cluster before the script" 0 "$(run --status)"
  grep -q "pid 999999 is running" "$work/out" || fail "--status misses a release run as a shell with options ($words): $(cat "$work/out")"
done
# A shell with only options and no script is another program; the option-only
# command line must not trip set -u.
printf '/bin/bash\0-x\0' >"$TI_PROC/999999/cmdline"
expect "--status with a shell that has only options" 0 "$(run --status)"
grep -q "pid reused; the lock is stale" "$work/out" || fail "--status takes an option-only shell for the release: $(cat "$work/out")"
# A -- word before the script does not hide the release.
printf '/bin/bash\0--\0/usr/local/sbin/ti-release\0v1\0' >"$TI_PROC/999999/cmdline"
expect "--status with -- before the script" 0 "$(run --status)"
grep -q "pid 999999 is running" "$work/out" || fail "--status misses a release run as bash -- ti-release: $(cat "$work/out")"
# A shell running some other script is another program, not the release.
printf '/bin/bash\0/tmp/other.sh\0' >"$TI_PROC/999999/cmdline"
expect "--status with a shell running another script" 0 "$(run --status)"
grep -q "pid reused; the lock is stale" "$work/out" || fail "--status takes a shell running another script for the release: $(cat "$work/out")"
# A command line whose last word has no trailing NUL (a process that rewrote
# its argv) still names the release: the final word is read, not dropped.
printf '/bin/bash\0/usr/local/sbin/ti-release' >"$TI_PROC/999999/cmdline"
expect "--status with a final word that has no trailing NUL" 0 "$(run --status)"
grep -q "pid 999999 is running" "$work/out" || fail "--status drops a final cmdline word with no trailing NUL: $(cat "$work/out")"
# Only the first word after the shell's options is the script; a later word
# that names the release does not make another script the release, and -c
# runs a command string, not a script.
for words in '/bin/bash\0/tmp/check.sh\0/usr/local/sbin/ti-release\0' '/bin/bash\0-c\0ti-release\0' '/bin/bash\0-o\0pipefail\0/tmp/check.sh\0ti-release\0'; do
  printf '%b' "$words" >"$TI_PROC/999999/cmdline"
  expect "--status with a shell whose later argument names the script" 0 "$(run --status)"
  grep -q "pid reused; the lock is stale" "$work/out" || fail "--status takes a later argument for the release ($words): $(cat "$work/out")"
done
# An empty command line (not readable under hidepid) is a running lock.
: >"$TI_PROC/999999/cmdline"
expect "--status with an unreadable command line" 0 "$(run --status)"
grep -q "its command line cannot be read" "$work/out" || fail "--status does not say the command line cannot be read: $(cat "$work/out")"
grep -q "belongs to the running release" "$work/out" || fail "--status calls a lock with an unreadable command line stale: $(cat "$work/out")"
unset TI_PROC
rm -rf "$TI_PREFIX/releases/.lock"
expect "--status with no lock" 0 "$(run --status)"
grep -q "lock: none" "$work/out" || fail "--status does not say the lock is clear: $(cat "$work/out")"
expect "--status refuses extra arguments" nonzero "$(nonzero "$(run --status v1.0.0)")"
test ! -e "$TI_PREFIX/releases/.lock" || fail "--status left a lock"

# A .replaced without its TAG note (the run died between the rename and the
# write): the tag is worked out as the previous build whose folder is missing.
mv "$TI_PREFIX/releases/.replaced" "$TI_PREFIX/releases/aside"
mkdir -p "$TI_PREFIX/releases/.replaced"
echo "v1.0.0" >"$TI_PREFIX/releases/previous"
rm -rf "$TI_PREFIX/releases/v1.0.0"
expect "a release refuses over a .replaced with no TAG" nonzero "$(nonzero "$(run v1.3.0)")"
grep -q "probably v1.0.0, whose folder is missing" "$work/out" || fail "the refusal does not name the probable tag: $(cat "$work/out")"
expect "--status with a .replaced with no TAG" 0 "$(run --status)"
grep -q "probably v1.0.0, whose folder is missing" "$work/out" || fail "--status does not name the probable tag: $(cat "$work/out")"
rmdir "$TI_PREFIX/releases/.replaced"
mv "$TI_PREFIX/releases/aside" "$TI_PREFIX/releases/.replaced"

# A rescue folder never blocks a run; only .replaced does.
mv "$TI_PREFIX/releases/.replaced" "$TI_PREFIX/releases/aside"
expect "the release goes ahead once .replaced is moved aside, a rescue present" 0 "$(run v1.3.0)"
test -d "$TI_PREFIX/releases/.rescue-v9.9.9" || fail "a release removed the rescue folder"
expect "--status with nothing kept" 0 "$(run --status)"
grep -q "\.replaced: none" "$work/out" || fail "--status does not say .replaced is clear: $(cat "$work/out")"
rm -rf "$TI_PREFIX/releases/.rescue-v9.9.9"

# A second run during a legitimate swap (the lock is held and .replaced is
# there) says another release is running, not a false ALERT, and touches
# neither.
mkdir -p "$TI_PREFIX/releases/.lock" "$TI_PREFIX/releases/.replaced"
expect "a run while another holds the lock is refused" nonzero "$(nonzero "$(run v1.4.0)")"
grep -q "another ti-release is running" "$work/out" || fail "the lock message is missing: $(cat "$work/out")"
if grep -q "ALERT" "$work/out"; then fail "a false ALERT during a legitimate swap: $(cat "$work/out")"; fi
test -d "$TI_PREFIX/releases/.replaced" && test -d "$TI_PREFIX/releases/.lock" || fail "the refused run disturbed the lock or .replaced"
rm -rf "$TI_PREFIX/releases/.lock" "$TI_PREFIX/releases/.replaced"

# The first release.
fresh_server
expect "the first release installs" 0 "$(run v1.0.0)"
all_are "the first release" v1.0.0
test -x "$TI_PREFIX/bin/ti-engine" || fail "ti-engine is not executable"
expect "current is v1.0.0" v1.0.0 "$(state current)"
expect "the hand-installed build is the previous" initial "$(state previous)"
expect "the hand-installed build is kept" "echo ti-dashboard hand" "$(tail -1 "$TI_PREFIX/releases/initial/ti-dashboard")"
test -L "$TI_LINK_DIR/operator" && test -L "$TI_LINK_DIR/journal-query" || fail "operator and journal-query are not linked"
grep -q '^restart ti-dashboard' "$work/ctl.log" || fail "the dashboard was not restarted"
if grep -E '^(restart|stop|start|kill).*ti-engine' "$work/ctl.log"; then fail "the engine was touched"; fi
if grep -E '^(restart|stop|start).*ti-reporter' "$work/ctl.log"; then fail "the reporter was restarted"; fi
expect "gh verified each program once" "$(for p in $programs; do echo "verify $p"; done | sort)" "$(sort "$work/gh.log.verified")"
expect "gh was called once per program" 6 "$(grep -c '^attestation verify' "$work/gh.log")"
grep -q "next fresh run" "$work/out" || fail "the output does not say the engine takes the build at the next fresh run"
grep -q "v1.0.0" "$work/out" || fail "the output does not name the new tag"
grep -qi "changed" "$work/out" || fail "the output does not say what changed"

# The same tag again changes nothing.
: >"$work/ctl.log"
expect "the same tag again succeeds" 0 "$(run v1.0.0)"
grep -qi "already" "$work/out" || fail "the repeat does not say it is already installed"
if grep -q '^restart' "$work/ctl.log"; then fail "a repeat restarted the dashboard"; fi

# --unverified (the rehearsal container only): installs without gh, warning
# loudly that the attestation was not checked.
fresh_server
expect "--unverified is refused on a server (no rehearsal marker)" nonzero "$(nonzero "$(TI_GH=$work/stub/no-such-gh run --unverified v1.0.0)")"
grep -q "rehearsal" "$work/out" || fail "the --unverified refusal does not say why: $(cat "$work/out")"
expect "bin untouched by a refused --unverified" "echo ti-engine hand" "$(installed ti-engine)"
not_current "no release recorded by a refused --unverified" v1.0.0
if "$script" 2>&1 | grep -q -- '--unverified'; then fail "the usage line advertises --unverified"; fi
expect "--unverified installs without gh in the rehearsal" 0 "$(TI_REHEARSAL=1 TI_GH=$work/stub/no-such-gh run --unverified v1.0.0)"
grep -q "WARNING" "$work/out" || fail "--unverified does not warn: $(cat "$work/out")"
grep -qi "attestation not verified" "$work/out" || fail "--unverified does not say the attestation was not verified"
all_are "--unverified" v1.0.0

# A second release keeps the first as the previous build; the hand-installed
# one is dropped.
expect "the second release installs" 0 "$(run v1.1.0)"
expect "current is v1.1.0" v1.1.0 "$(state current)"
expect "previous is v1.0.0" v1.0.0 "$(state previous)"
test ! -e "$TI_PREFIX/releases/initial" || fail "the older build was not pruned"

# A release whose dashboard does not come back: reported, and the previous
# build is put back, so nothing is left half-installed.
: >"$work/ctl.log"
expect "a failed verify is nonzero" nonzero "$(nonzero "$(run v1.2.0)")"
grep -qi "dashboard" "$work/out" || fail "the verify failure does not name the dashboard"
all_are "a failed dashboard puts v1.1.0 back" v1.1.0
expect "current is still v1.1.0" v1.1.0 "$(state current)"
expect "previous is still v1.0.0" v1.0.0 "$(state previous)"
expect "the dashboard was restarted twice" 2 "$(grep -c '^restart ti-dashboard' "$work/ctl.log")"

# A dashboard that comes up and then stops within the settle is a failure.
expect "a dashboard that does not stay active is nonzero" nonzero "$(nonzero "$(run v1.4.0)")"
all_are "a flapping dashboard puts v1.1.0 back" v1.1.0
expect "current is still v1.1.0 after a flap" v1.1.0 "$(state current)"

# The kept copy of the current build is checked before a release starts: if
# it is damaged it could not be put back, so nothing is installed.
cp "$TI_PREFIX/releases/v1.1.0/ti-reporter" "$work/keep"
printf 'x\n' >>"$TI_PREFIX/releases/v1.1.0/ti-reporter"
expect "a release over a damaged kept build is refused" nonzero "$(nonzero "$(run v1.3.0)")"
all_are "bin untouched over a damaged kept build" v1.1.0
mv "$work/keep" "$TI_PREFIX/releases/v1.1.0/ti-reporter"

# Rollback.
: >"$work/ctl.log"
expect "rollback succeeds" 0 "$(run --rollback)"
all_are "rollback" v1.0.0
expect "current is v1.0.0 after rollback" v1.0.0 "$(state current)"
expect "previous is v1.1.0 after rollback" v1.1.0 "$(state previous)"
grep -q '^restart ti-dashboard' "$work/ctl.log" || fail "rollback did not restart the dashboard"
if grep -E '^(restart|stop|start|kill).*ti-engine' "$work/ctl.log"; then fail "rollback touched the engine"; fi
grep -q "next fresh run" "$work/out" || fail "rollback does not say the engine keeps its build until the next fresh run"

# Rollback refuses a damaged previous build, and while an engine is mid-restart.
printf 'x\n' >>"$TI_PREFIX/releases/v1.1.0/ti-reporter"
expect "rollback to a damaged build is refused" nonzero "$(nonzero "$(run --rollback)")"
expect "bin untouched after the refused rollback" "echo ti-reporter v1.0.0" "$(installed ti-reporter)"
expect "an engine mid-restart blocks rollback" nonzero "$(nonzero "$(FAKE_ENGINE_BUSY=1 run --rollback)")"
expect "a late engine restart blocks rollback" nonzero "$(nonzero "$(FAKE_ENGINE_BUSY_FROM=2 run --rollback)")"

# A saved tagged build must list all five programs: only the hand-installed
# build may lack one, so a lost checksum line cannot mix two builds.
fresh_server
run v1.0.0 >/dev/null
run v1.1.0 >/dev/null
cp "$TI_PREFIX/releases/v1.0.0/checksums.txt" "$work/keep"
grep -v journal-query "$work/keep" >"$TI_PREFIX/releases/v1.0.0/checksums.txt"
expect "rollback to a build missing a checksum line is refused" nonzero "$(nonzero "$(run --rollback)")"
all_are "bin untouched after a refusal for a missing checksum line" v1.1.0
mv "$work/keep" "$TI_PREFIX/releases/v1.0.0/checksums.txt"

# Rollback checks the kept copy of the current build first, like a release:
# if it is damaged it could not be put back, so nothing changes.
printf 'x\n' >>"$TI_PREFIX/releases/v1.1.0/ti-reporter"
: >"$work/ctl.log"
expect "rollback over a damaged current build is refused" nonzero "$(nonzero "$(run --rollback)")"
all_are "bin untouched over a damaged current build" v1.1.0
if grep -q '^restart' "$work/ctl.log"; then fail "rollback restarted the dashboard over a damaged current build"; fi
grep -q "v1.1.0" "$work/out" || fail "the refusal does not name the current build: $(cat "$work/out")"

# Rollback with no previous build.
fresh_server
expect "rollback with nothing to roll back to is refused" nonzero "$(nonzero "$(run --rollback)")"

# Rollback to a hand-installed build that lacked a program: the programs it
# had are put back, and the one it lacked is left as it is (nothing removed).
fresh_server
rm "$TI_PREFIX/bin/journal-query"
expect "a release over a hand build lacking a program installs" 0 "$(run v1.0.0)"
expect "rollback to a hand build lacking a program succeeds" 0 "$(run --rollback)"
for p in ti-engine ti-reporter ti-dashboard operator; do expect "$p back to the hand build" "echo $p hand" "$(installed "$p")"; done
expect "the program the hand build lacked is left" "echo journal-query v1.0.0" "$(installed journal-query)"

# A rollback whose dashboard does not come back puts the current build back
# and records nothing, so the server still holds the build state names.
fresh_server
run v1.2.0 >/dev/null || true
run v1.1.0 >/dev/null
run v1.2.0 >/dev/null || true
# current is v1.1.0, previous is the hand build; make the previous bad.
fake_program "$TI_PREFIX/releases/initial/ti-dashboard" "echo BAD"
(cd "$TI_PREFIX/releases/initial" && : >checksums.txt && for p in $programs; do printf '%s  %s\n' "$(sha "$p")" "$p" >>checksums.txt; done)
expect "a rollback whose dashboard does not return fails" nonzero "$(nonzero "$(run --rollback)")"
grep -qi "dashboard" "$work/out" || fail "the rollback failure does not name the dashboard"
all_are "a failed rollback puts the current build back" v1.1.0
expect "current is still v1.1.0 after a failed rollback" v1.1.0 "$(state current)"
expect "previous is still initial after a failed rollback" initial "$(state previous)"

# Releasing the previous build's tag again, when its dashboard then fails,
# keeps that build's folder, so --rollback still works.
fresh_server
run v1.3.0 >/dev/null
run v1.1.0 >/dev/null
expect "previous is v1.3.0" v1.3.0 "$(state previous)"
fake_program "$work/rel/v1.3.0/ti-dashboard" "echo BAD"
reseal v1.3.0
expect "re-releasing the previous tag with a bad dashboard fails" nonzero "$(nonzero "$(run v1.3.0)")"
all_are "the failed re-release puts v1.1.0 back" v1.1.0
test -d "$TI_PREFIX/releases/v1.3.0" || fail "the previous build's folder was removed by a failed release"
expect "rollback after the failed re-release succeeds" 0 "$(run --rollback)"
all_are "rollback to the kept v1.3.0" v1.3.0
make_release v1.3.0

# gh is installed pinned, like every other download the server trusts: the key
# by its published fingerprints, the package by version, both in versions.env.
book=$repo/docs/runbooks/lightsail-deployment.md
# shellcheck source=/dev/null
( . "$repo/deploy/versions.env"
  [[ ${GH_VERSION:-} =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 1
  [[ ${GH_KEY_FINGERPRINTS:-} =~ ^[0-9A-F]{40}\ [0-9A-F]{40}$ ]] ) || fail "versions.env does not pin gh's version and key fingerprints"
# shellcheck disable=SC2016  # the runbook holds the literal $GH_VERSION
grep -Fq 'apt install gh="$GH_VERSION"' "$book" || fail "the runbook does not install the pinned gh version"
grep -Fq 'apt-mark hold gh' "$book" || fail "the runbook does not hold gh at the pinned version"
grep -Fq 'apt-mark unhold gh' "$book" || fail "the runbook does not say how to upgrade the held gh"
if grep -Eq 'apt install gh([^=]|$)' "$book"; then fail "the runbook installs an unpinned gh"; fi
grep -Fq 'GH_KEY_FINGERPRINTS' "$book" || fail "the runbook does not check gh's key fingerprints"
server_manual=$repo/docs/manual/server.md
for doc in "$book" "$server_manual"; do
  grep -Fq 'ti-release --status' "$doc" || fail "$doc does not point to ti-release --status"
  if grep -Fq 'ti-release <tag>` for the tag you expect' "$doc"; then fail "$doc still probes the installed build with ti-release <tag>"; fi
  grep -Fq '.rescue-<tag>.1' "$doc" || [ "$doc" = "$server_manual" ] || fail "$doc names the wrong first extra rescue folder"
done
# shellcheck disable=SC2016  # the runbook holds a literal backtick
if grep -Fq '(or `.rescue-<tag>.2`' "$book"; then fail "the runbook names .rescue-<tag>.2 as the first extra rescue"; fi
if grep -Fq 'rename the copy to something else' "$book"; then fail "the runbook advises a rename that a later release deletes"; fi
grep -Fq '.aside-<tag>' "$book" || fail "the runbook does not name a dot-prefixed place to move the copy aside"
if grep -Fq 'refuses to start until that copy is dealt with' "$book" "$server_manual"; then fail "a doc says a rescue folder blocks a run"; fi
if grep -Fq -- '--unverified' "$book" && grep -F -- '--unverified' "$book" | grep -Fq 'sudo ti-release'; then fail "the runbook offers --unverified on the server"; fi

# The release workflow: tags only (no deploy on a merge), every action pinned
# by commit SHA, the attestation step present, no build cache (the attested
# programs are built from source), and valid to actionlint when installed.
wf=$repo/.github/workflows/release.yml
grep -Eq '^on:' "$wf" || fail "the release workflow has no trigger"
if grep -Eq 'branches:|pull_request|workflow_dispatch|schedule:' "$wf"; then fail "the release workflow runs on more than a version tag"; fi
grep -Eq 'tags: \["v\*"\]' "$wf" || fail "the release workflow is not triggered by a v* tag"
grep -q 'actions/attest-build-provenance@' "$wf" || fail "the release workflow does not attest the build"
if grep -E '^\s*(- )?uses:' "$wf" | grep -Ev '@[0-9a-f]{40} #'; then fail "an action in the release workflow is not pinned by SHA"; fi
grep -Eq '^\s+cache: false' "$wf" || fail "the release workflow's setup-go does not turn its cache off"
if grep -Eq 'cache: true|actions/cache@' "$wf"; then fail "the release workflow uses a build cache"; fi
if command -v actionlint >/dev/null 2>&1; then
  (cd "$repo" && actionlint .github/workflows/release.yml) || fail "actionlint rejects the release workflow"
fi

if test "$failures" -ne 0; then echo "$failures failure(s)" >&2; exit 1; fi
echo "test-release: ok"

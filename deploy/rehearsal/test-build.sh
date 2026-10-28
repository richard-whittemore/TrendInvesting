#!/bin/bash
# Tests for deploy/build-engine.sh's target architecture: linux/amd64 by
# default (the Lightsail server), linux/arm64 on request (the Mac mini's
# container), the same stamp for both, a dirty tree and an unknown
# architecture refused. Each binary's architecture is read from the file
# itself (its ELF header and the build settings Go records in it), not from
# what the script prints.
#
# The script under test is committed into a fresh clone of this checkout's
# HEAD, so the build runs in a clean tree whatever state this one is in.
# Each run has a directory of its own, so two runs at once (checks in two
# worktrees) never remove or read each other's clone.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
tmp=${TMPDIR:-/tmp}
work=$(mktemp -d "${tmp%/}/ti-test-build.XXXXXX")
work=$(cd "$work" && pwd -P)
trap 'rm -rf "$work"' EXIT
unset TI_ALLOW_DIRTY GOOS GOARCH

failures=0
expect() { # description expected actual
  if test "$2" != "$3"; then
    echo "FAIL: $1: want '$2', got '$3'" >&2
    failures=$((failures + 1))
  fi
}

clone=$work/repo
git clone -q --shared "$repo" "$clone"
cp "$repo/deploy/build-engine.sh" "$clone/deploy/build-engine.sh"
git -C "$clone" -c user.name=test -c user.email=test@invalid commit -q --allow-empty -am "the build script under test"
stamp=$(cd "$clone" && scripts/build-stamp.sh)
case $stamp in *dirty*) expect "the clone is clean" clean "$stamp" ;; esac

programs="ti-engine ti-reporter ti-dashboard operator journal-query universe-rank"

# ELF e_machine, the two bytes at offset 18, little-endian: 62 is x86-64,
# 183 is AArch64.
elf_machine() { od -An -tu2 -j18 -N2 "$1" | tr -d ' '; }
# A Go build setting recorded in the binary (`go version -m`).
# Its lines are tab-separated: "", "build", "KEY=VALUE".
build_setting() { go version -m "$1" | awk -F'\t' -v k="$2" '$2 == "build" && index($3, k "=") == 1 { print substr($3, length(k) + 2) }'; }

# check_build DIR GOARCH MACHINE: every program in DIR is a linux/GOARCH
# binary (by its ELF header and its recorded GOOS and GOARCH) stamped $stamp.
check_build() {
  local dir=$1 goarch=$2 machine=$3 p
  for p in $programs; do
    expect "$p is an ELF file for $goarch" "$machine" "$(elf_machine "$dir/$p")"
    expect "$p records GOOS linux" linux "$(build_setting "$dir/$p" GOOS)"
    expect "$p records GOARCH $goarch" "$goarch" "$(build_setting "$dir/$p" GOARCH)"
    case $(build_setting "$dir/$p" -ldflags) in
      *"buildinfo.Version=$stamp"*) ;;
      *) expect "$p carries the stamp" "$stamp" "$(build_setting "$dir/$p" -ldflags)" ;;
    esac
  done
}

# run_build NAME ARGS...: run the script in the clone; keep dist/ as NAME
# and its output as NAME.out; print its exit status.
run_build() {
  local name=$1 status=0
  shift
  rm -rf "$clone/dist"
  (cd "$clone" && deploy/build-engine.sh "$@") >"$work/$name.out" 2>&1 || status=$?
  if test -d "$clone/dist"; then cp -R "$clone/dist" "$work/$name"; fi
  echo "$status"
}

expect "the default build succeeds" 0 "$(run_build default)"
check_build "$work/default" amd64 62
grep -q "$stamp" "$work/default.out" || expect "the default build prints its stamp" "$stamp" "$(cat "$work/default.out")"
grep -q "linux/amd64" "$work/default.out" || expect "the default build names its platform" linux/amd64 "$(cat "$work/default.out")"

expect "an arm64 build succeeds" 0 "$(run_build arm64 --arch arm64)"
check_build "$work/arm64" arm64 183
grep -q "linux/arm64" "$work/arm64.out" || expect "the arm64 build names its platform" linux/arm64 "$(cat "$work/arm64.out")"

# Asking for amd64 is the default build, byte for byte.
expect "an explicit amd64 build succeeds" 0 "$(run_build amd64 --arch amd64)"
for p in $programs; do
  cmp -s "$work/default/$p" "$work/amd64/$p" || expect "$p from --arch amd64 is the default build" same different
done

# --out DIR writes the five there and leaves dist/ alone: the Mac-side arm64
# rehearsal never replaces the build the Lightsail runbook copies.
mkdir -p "$clone/dist"
echo lightsail >"$clone/dist/ti-engine"
status=0
(cd "$clone" && deploy/build-engine.sh --arch arm64 --out "$work/elsewhere") >"$work/out.out" 2>&1 || status=$?
expect "an arm64 build into --out succeeds" 0 "$status"
check_build "$work/elsewhere" arm64 183
expect "a build into --out leaves dist/ as it was" lightsail "$(cat "$clone/dist/ti-engine")"
grep -q "$work/elsewhere" "$work/out.out" || expect "the build names where it wrote" "$work/elsewhere" "$(cat "$work/out.out")"
expect "a missing --out directory is refused" 1 "$(run_build noout --out)"

expect "an unknown architecture is refused" 1 "$(run_build riscv --arch riscv64)"
test ! -e "$work/riscv" || expect "nothing is built for an unknown architecture" absent present
expect "a missing architecture is refused" 1 "$(run_build noarch --arch)"

touch "$clone/untracked.go"
expect "a dirty tree is refused for arm64" 1 "$(run_build dirty --arch arm64)"
grep -q "dirty" "$work/dirty.out" || expect "the refusal says the tree is dirty" dirty "$(cat "$work/dirty.out")"
test ! -e "$work/dirty" || expect "nothing is built from a dirty tree" absent present

test "$failures" -eq 0 || exit 1
echo "build tests ok"

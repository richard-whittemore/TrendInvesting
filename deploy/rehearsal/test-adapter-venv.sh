#!/bin/bash
# Tests for verify_adapter_venv in deploy/provision.sh, the twsapi step's last
# gate before it records INSTALLED: the venv's google.protobuf is the pinned
# version and both it and ibapi resolve under that venv, not from a system
# site-packages that shadows it (docs/dependency-policy.md, protobuf entry;
# ADR 0025). A real venv (no pip) with stand-in packages.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
expect() { # description expected actual
  if test "$2" != "$3"; then
    echo "FAIL: $1: want '$2', got '$3'" >&2
    failures=$((failures + 1))
  fi
}

fn=$(awk '/^verify_adapter_venv\(\)/,/^}/' "$here/../provision.sh")
test -n "$fn" || { echo "FAIL: provision.sh has no verify_adapter_venv" >&2; exit 1; }

# mkpkgs DIR PROTOBUF_VERSION: stand-in google.protobuf and ibapi packages.
# google is a namespace package (no __init__.py), as in the real protobuf
# wheel; a regular google/__init__.py elsewhere on the path therefore wins.
mkpkgs() {
  mkdir -p "$1/google/protobuf" "$1/ibapi"
  printf '__version__ = "%s"\n' "$2" >"$1/google/protobuf/__init__.py"
  : >"$1/ibapi/__init__.py"
  for m in client contract order wrapper; do : >"$1/ibapi/$m.py"; done
}
# mkvenv NAME: a venv; prints its site-packages.
mkvenv() {
  python3 -m venv --without-pip "$work/$1"
  "$work/$1/bin/python3" -c 'import sysconfig; print(sysconfig.get_paths()["purelib"])'
}
run() { # venv pinned; prints the exit status, output in $work/err
  local st=0
  bash -c '
    log() { printf "provision: %s\n" "$*"; }
    die() { printf "provision: %s\n" "$*" >&2; exit 1; }
    eval "$1"; verify_adapter_venv "$2" "$3"' _ "$fn" "$work/$1" "$2" >"$work/out" 2>"$work/err" || st=$?
  echo "$st"
}

sp=$(mkvenv good); mkpkgs "$sp" 5.29.5
expect "pinned protobuf and ibapi under the venv: accepted" 0 "$(run good 5.29.5)"

expect "a protobuf of another version is refused" 1 "$(run good 5.29.4)"
grep -q 'protobuf 5.29.5 .*pinned 5.29.4' "$work/err" || expect "the version refusal names both versions" yes no

sp=$(mkvenv shadow); mkpkgs "$sp" 5.29.5
mkdir -p "$work/system"; mkpkgs "$work/system" 5.29.5
rm -rf "$sp/google"
echo "$work/system" >"$sp/outside.pth"
expect "a protobuf resolving outside the venv is refused" 1 "$(run shadow 5.29.5)"
grep -q 'google.protobuf resolves to .*system.*outside' "$work/err" || expect "the shadow refusal names protobuf and where it resolved" yes no
grep -q 'protobuf 5' "$work/err" && expect "the shadow refusal is not the version refusal" no yes

# Ubuntu's python3-protobuf: a REGULAR google/__init__.py in a system dir that
# the venv's namespace google loses to. The location is the cause, so the
# refusal names it, not the (also wrong) version.
sp=$(mkvenv regular); mkpkgs "$sp" 5.29.5
mkdir -p "$work/dist/google/protobuf" "$work/dist/ibapi"
: >"$work/dist/google/__init__.py"
printf '__version__ = "3.21.12"\n' >"$work/dist/google/protobuf/__init__.py"
echo "$work/dist" >"$sp/dist.pth"
expect "a system regular google package shadowing the venv is refused" 1 "$(run regular 5.29.5)"
grep -q 'google.protobuf resolves to .*dist.*outside' "$work/err" || expect "the regular-package shadow is reported by location" yes no
grep -q 'protobuf 3' "$work/err" && expect "the regular-package shadow is not reported as a version mismatch" no yes

# A stand-in interpreter that reports fixed values, for what a real venv
# cannot be made to report: PREFIX BASE_PREFIX VERSION PB IB, one per line.
mkstub() { # name prefix base_prefix version pb ib
  mkdir -p "$work/$1/bin"
  printf '#!/bin/bash\ncase "$*" in *ibapi.client*) exit 0 ;; esac\nprintf "%%s\\n" %s %s %s %s %s\n' \
    "'$2'" "'$3'" "'$4'" "'$5'" "'$6'" >"$work/$1/bin/python3"
  chmod +x "$work/$1/bin/python3"
}
real=$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$work")
mkstub notvenv "$real/notvenv" "$real/notvenv" 5.29.5 "$real/notvenv/g" "$real/notvenv/i"
expect "an interpreter that is not a venv is refused" 1 "$(run notvenv 5.29.5)"
grep -q 'not a venv' "$work/err" || expect "the refusal says it is not a venv" yes no
mkstub elsewhere "$real/elsewhere" /usr "5.29.5" "$real/elsewhere/g" "$real/elsewhere/i"
mkdir -p "$work/venvarg"; ln -s "$work/elsewhere/bin" "$work/venvarg/bin"
expect "modules under sys.prefix but outside the VENV argument are refused" 1 "$(run venvarg 5.29.5)"
grep -q 'google.protobuf resolves to .*outside' "$work/err" || expect "measured against the venv argument's realpath" yes no

sp=$(mkvenv ibshadow); mkpkgs "$sp" 5.29.5
rm -rf "$sp/ibapi"
echo "$work/system" >"$sp/outside.pth"
expect "an ibapi resolving outside the venv is refused" 1 "$(run ibshadow 5.29.5)"
grep -q 'ibapi resolves to .*system.*outside' "$work/err" || expect "the ibapi refusal names ibapi and where it resolved" yes no

# The step runs the gate before it writes the record.
step=$(awk '/^step_twsapi\(\)/,/^}/' "$here/../provision.sh")
gate_line=$(printf '%s\n' "$step" | grep -n 'verify_adapter_venv' | head -n1 | cut -d: -f1)
write_line=$(printf '%s\n' "$step" | grep -n 'INSTALLED.tmp' | head -n1 | cut -d: -f1)
test -n "$gate_line" -a -n "$write_line" && test "$gate_line" -lt "$write_line" || expect "twsapi verifies the venv before writing INSTALLED" yes no

test "$failures" -eq 0 || exit 1
echo "adapter venv tests ok"

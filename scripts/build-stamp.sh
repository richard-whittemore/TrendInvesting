#!/bin/bash
# Prints the build stamp every envelope's strategy version carries (ADR
# 0016): `git describe --always --dirty`, and for a dirty tree, one with an
# untracked file that is not ignored included, the hash of its whole working
# tree, untracked files included, after "-dirty-". Two
# different uncommitted changes at one commit are then two builds, and a run
# never resumes across builds (ADR 0027, owner decision 2026-10-16). The
# index and working tree are left as they were; the tree's objects are
# written to the repository's object store. Outside a git checkout it
# prints "dev", the unstamped build, which every mode but backtest refuses.
set -euo pipefail

if ! stamp=$(git describe --always --dirty 2>/dev/null); then
  echo dev
  exit 0
fi
# `git describe --dirty` ignores untracked files, but `go build` compiles an
# untracked Go file, so an untracked file that is not ignored makes the tree
# dirty too.
if test "${stamp%-dirty}" = "$stamp" && test -n "$(git ls-files --others --exclude-standard)"; then
  stamp=$stamp-dirty
fi
case $stamp in
  *-dirty)
    index=$(mktemp)
    trap 'rm -f "$index"' EXIT
    GIT_INDEX_FILE=$index git read-tree HEAD
    GIT_INDEX_FILE=$index git add -A
    tree=$(GIT_INDEX_FILE=$index git write-tree)
    stamp=$stamp-${tree:0:12}
    ;;
esac
echo "$stamp"

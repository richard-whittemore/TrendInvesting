#!/usr/bin/env bash
# Run the Sublime research algorithm on local LEAN in fixed-universe mode
# (README.md, "Local smoke run"). Uses only the pinned image and the data
# already in the LEAN workspace: it never pulls an image or fetches data.
#
# Usage: run_local.sh <lean-workspace> [project-dir]
# The project directory defaults to <lean-workspace>/sublime-smoke; LEAN writes
# its backtest under <project-dir>/backtests/. A project outside the workspace
# also runs, but the CLI then exits 1 with a path error after writing its
# results.
set -euo pipefail

workspace=${1:?lean workspace directory (holds lean.json, data/ and .venv/)}
project=${2:-$workspace/sublime-smoke}
image=quantconnect/lean@sha256:9b8e69ec49e49f0ee207c27c6b0f3e2e6b35cfd7a241f31aa16577c6debb890d
here=$(cd "$(dirname "$0")" && pwd)

mkdir -p "$project"
cp "$here/lean_main.py" "$project/main.py"
cp "$here/../main.py" "$project/research_main.py"
cp "$here/../rules.py" "$project/rules.py"
if [ ! -f "$project/config.json" ]; then
  printf '{\n  "algorithm-language": "Python",\n  "parameters": {},\n  "description": "sublime smoke"\n}\n' \
    > "$project/config.json"
fi

"$workspace/.venv/bin/lean" backtest "$project" --lean-config "$workspace/lean.json" \
  --image "$image" --no-update
echo "backtest written under $project/backtests"

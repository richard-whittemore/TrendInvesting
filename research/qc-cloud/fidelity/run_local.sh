#!/usr/bin/env bash
# Run the research algorithm on local LEAN in its fixed-universe mode, then
# compare its fills with a Go engine journal (README.md, "Checking fidelity
# against the Go engine"). Uses only the image and data already on this
# machine: it never pulls an image or fetches market data.
#
# Usage: run_local.sh <lean-workspace> <go-journal.jsonl> [project-name]
set -euo pipefail

workspace=${1:?lean workspace directory}
journal=${2:?Go engine journal.jsonl}
project=${3:-research-fidelity}
image=quantconnect/lean@sha256:9b8e69ec49e49f0ee207c27c6b0f3e2e6b35cfd7a241f31aa16577c6debb890d
here=$(cd "$(dirname "$0")" && pwd)

mkdir -p "$workspace/$project"
cp "$here/lean_main.py" "$workspace/$project/main.py"
cp "$here/../main.py" "$workspace/$project/research_main.py"
cp "$here/../rules.py" "$workspace/$project/rules.py"
if [ ! -f "$workspace/$project/config.json" ]; then
  printf '{\n  "algorithm-language": "Python",\n  "parameters": {},\n  "description": "research fidelity"\n}\n' \
    > "$workspace/$project/config.json"
fi

(cd "$workspace" && .venv/bin/lean backtest "$project" --image "$image" --no-update)

latest=$(ls -td "$workspace/$project"/backtests/*/ | head -n 1)
python3 "$here/compare_fills.py" "$latest" "$journal"

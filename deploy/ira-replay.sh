#!/bin/bash
# A private dry-run replay for plan.md section 9's rehearsed-month gate.
# ADR 0026 and AGENTS.md rule 5: no adapter, no broker connection, never sends.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
exec python3 "$here/ira-replay.py" "$@"

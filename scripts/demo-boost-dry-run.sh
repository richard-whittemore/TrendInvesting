#!/bin/bash
# Runs the one engine (cmd/engine, stamped by `make engine`) in dry-run over
# an invented SPY series, played by cmd/boost-demo as the adapter, and prints
# the decision log. Then it checks the durable log the way a restart reads it:
# a second engine resumes it (-resume replays every committed input through a
# fresh handler and requires each group to be reproduced record for record,
# ADR 0027) into a new log, and the two logs and the two journals must be
# identical byte for byte. Synthetic data only; nothing reaches a broker.
#
#   make demo-boost-dry-run
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo"
config=deploy/etc/engines/boost.config
as_of=2024-01-02T21:00:00Z

make --no-print-directory engine
go build -o bin/boost-demo ./cmd/boost-demo

work=$(mktemp -d)
sockets=$(mktemp -d /tmp/ti-demo-XXXXXX)
engine_pid=
cleanup() {
  if test -n "$engine_pid"; then kill "$engine_pid" 2>/dev/null || true; fi
  rm -rf "$sockets"
}
trap cleanup EXIT

# start_engine NAME [ARGS...]: start the engine, wait until it listens.
start_engine() {
  local name=$1
  shift
  bin/ti-engine -mode dry-run -config "$config" -as-of "$as_of" -socket "$sockets/engine.sock" \
    -log "$work/$name.log" -out "$work/$name.journal" "$@" >"$work/$name.out" 2>&1 &
  engine_pid=$!
  for _ in $(seq 100); do
    if grep -q "listening on" "$work/$name.out"; then return 0; fi
    if ! kill -0 "$engine_pid" 2>/dev/null; then cat "$work/$name.out" >&2; exit 1; fi
    sleep 0.1
  done
  echo "demo: the engine did not start" >&2
  exit 1
}

stop_engine() {
  kill -TERM "$engine_pid"
  wait "$engine_pid"
  engine_pid=
}

echo "== decision log (engine build $(git describe --always --dirty), dry-run, synthetic SPY)"
start_engine run
bin/boost-demo -socket "$sockets/engine.sock" -log "$work/run.log" -config "$config" -sessions 320
stop_engine
cat "$work/run.out"

echo "== replay: resume the durable log into a new engine"
start_engine replay -resume "$work/run.log"
stop_engine
cat "$work/replay.out"
cmp "$work/run.log" "$work/replay.log"
cmp "$work/run.journal" "$work/replay.journal"
released=$(grep -c '"type":"risk.order.released"' "$work/run.log" || true)
sending=$(grep -c '"send":true' "$work/run.log" || true)
held=$(grep -c '"send":false' "$work/run.log" || true)
test "$held" = "$released"
echo "replayed byte for byte: $(wc -c <"$work/run.log" | tr -d ' ') bytes of durable log, $released released orders, $sending marked to send"
test "$sending" = 0
echo "files: $work"

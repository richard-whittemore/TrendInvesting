# Transport spike — Python side

Throwaway measurement code for issue #26. **This is not the adapter.** It
exists to produce the numbers in [ADR 0014](../../../docs/adr/0014-lean-go-transport.md)
and can be deleted once the real adapter exists.

| File | What it is |
|---|---|
| `client.py` | Unix-socket newline-JSON client mirroring the Go package `transport` |
| `bench.py` | Round-trip latency at a chosen universe size |
| `faults.py` | The failure matrix: engine absent, killed, restarted, oversized frame, slow engine, startup ordering |
| `algorithm.py` | A `QCAlgorithm` that exchanges a bar and a decision on every bar |

Everything is Python 3.9 syntax, so one copy runs on the host and inside the
LEAN container unchanged.

## The one thing to know first

**A Unix socket on a macOS bind mount cannot be connected to from inside the
container.** The socket file appears in the mount and `connect()` fails with
`ECONNREFUSED`. Put the socket in a Docker **named volume** (or in the
container's own filesystem). This is a Docker-Desktop-on-macOS property, not a
property of the transport; on a Linux host a bind mount works.

## Running it

Build the Go engine:

```sh
go build -o /tmp/transport-spike ./cmd/transport-spike
# for the container (Apple Silicon):
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/transport-spike-linux ./cmd/transport-spike
```

### On the host

```sh
/tmp/transport-spike serve --socket /tmp/trend-spike.sock &
python3 bench.py --socket /tmp/trend-spike.sock --rounds 500 --universe 1000
python3 faults.py --engine /tmp/transport-spike
```

`bench.py` prints one row: `min/median/p95/p99/max/mean`, plus
`encode_median`, the time Python's JSON encoder alone takes on the same
envelope. At a 1,000-symbol universe that is about 40% of the round trip, so
most of the cost is Python's encoder rather than the socket.

### Inside the LEAN container

```sh
mkdir -p /tmp/leanstage
cp /tmp/transport-spike-linux /tmp/leanstage/transport-spike
cp *.py /tmp/leanstage/

docker run --rm -v /tmp/leanstage:/spike --entrypoint /bin/bash \
  quantconnect/lean:latest -lc '
    mkdir -p /run/spike
    /spike/transport-spike serve --socket /run/spike/s.sock &
    sleep 1
    cd /spike
    python bench.py --socket /run/spike/s.sock --rounds 500 --universe 1000
    python faults.py --engine /spike/transport-spike
  '
```

### Engine and LEAN in separate containers

This is the shape a real deployment has, and the one that works on macOS.

```sh
docker volume create spikesock
docker run -d --name spike-engine \
  -v spikesock:/run/spike -v /tmp/leanstage:/spike \
  --entrypoint /spike/transport-spike quantconnect/lean:latest \
  serve --socket /run/spike/s.sock

docker run --rm -v spikesock:/run/spike -v /tmp/leanstage:/spike \
  --entrypoint /bin/bash quantconnect/lean:latest \
  -lc 'cd /spike && python bench.py --socket /run/spike/s.sock --universe 1000'
```

### Under a real backtest

`algorithm.py` is the load-bearing case: LEAN runs Python under pythonnet
inside the .NET process, so only this shows whether a blocking socket call per
bar is survivable where the adapter will actually live.

Create a LEAN workspace (a `lean.json` and a `data/` tree), put a project in it
whose `main.py` is `algorithm.py` and which also contains `client.py`, then:

```sh
lean backtest spike-transport \
  --extra-docker-config '{"volumes": {"spikesock": {"bind": "/run/spike", "mode": "rw"}}}'
```

With the engine container running, the log shows one round trip per bar. With
no engine, the algorithm calls `Quit()` in `Initialize` and LEAN stops before
processing a single data point — the fail-closed behaviour required by
`docs/architecture.md`.

## Three traps this spike walked into

1. **`from AlgorithmImports import *` shadows the standard library.** It
   exports `datetime.time`, so `time.perf_counter()` raises
   `AttributeError: type object 'datetime.time' has no attribute 'perf_counter'`
   at the first bar. Bind what you need by name *after* the star import
   (`from time import perf_counter`).
2. **The payload hash attests bytes, not structure.** Python must hash exactly
   the bytes it embeds. `client.py` dumps with `separators=(",", ":")`
   everywhere, which makes the payload dumped alone byte-identical to the
   substring the envelope dump produces. Change the separators in one place
   only and every envelope is rejected as `invalid_envelope`.
3. **`envelope_version` is required and is not inferred.** ADR 0015 versions
   the envelope struct itself. An envelope without the field decodes as
   version 0 on the Go side and is rejected — deliberately, because this build
   has no upcaster. `client.py`'s `ENVELOPE_VERSION` must equal
   `event.CurrentEnvelopeVersion` in `internal/event/envelope.go`; nothing
   checks that for you except the round trip.

## The rejected alternative

The gRPC half is not kept in the tree: it was measured from a separate Go
module at commit `634e2cb` (`transport/grpcspike/` on the #26 branch), which
remains the reproducible reference. See ADR 0014 for why it was not chosen and
why the module was not merged.

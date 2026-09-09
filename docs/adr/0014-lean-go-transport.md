# ADR 0014: The LEAN adapter talks to Go over a Unix-domain socket with newline-delimited JSON

- Status: Accepted
- Date: 2026-09-09

## Context

`docs/architecture.md` left the local transport deliberately unfrozen: "exact
local transport (Unix-domain gRPC is preferred but not frozen)". The preference
was an assumption, and it sat under every other decision — if a Python
`QCAlgorithm` inside the LEAN Docker container could not exchange a
completed-bar envelope and a decision envelope with a Go process, per bar,
reliably enough for a daily-bar strategy, the whole Go-owns-strategy
architecture would need rethinking. Issue #26 exists to replace the assumption
with measurements.

Two candidates were built and measured end to end:

- **A. Unix-domain socket, newline-delimited JSON.** Zero new Go dependencies.
- **B. Local gRPC over a Unix-domain socket.** Requires `google.golang.org/grpc`.

Both carry the identical `internal/event.Envelope` JSON, including the four
provenance fields from #5. Candidate B carries it in an opaque protobuf
`BytesValue` precisely so the comparison is between two *transports* and not
between two payload encodings.

## Measurements

Apple Silicon macOS 15 (Darwin 25.6.0), Go 1.24.4, host Python 3.9.6,
Docker Desktop 29.1.3, `quantconnect/lean:latest` (Python 3.11.9, aarch64).
500 measured round trips after 50 discarded warm-up round trips, sequential,
one exchange at a time. Every figure includes encoding the request and decoding
the reply, because that is what the adapter actually pays per bar.

A 1,000-instrument universe is the realistic size; 2,000 is included to show
the slope.

### Round-trip latency — Python inside the LEAN container

This is the number that matters: it is the adapter's real environment.

| Transport | Universe | Request | Median | p95 | p99 | Max |
|---|---:|---:|---:|---:|---:|---:|
| A. UDS + NDJSON | 1 | 497 B | **0.076 ms** | 0.100 ms | 0.119 ms | 0.142 ms |
| A. UDS + NDJSON | 100 | 9.5 KB | **0.411 ms** | 0.448 ms | 0.576 ms | 1.697 ms |
| A. UDS + NDJSON | 1,000 | 91.2 KB | **3.150 ms** | 3.686 ms | 4.630 ms | 5.535 ms |
| A. UDS + NDJSON | 2,000 | 182.1 KB | **6.532 ms** | 8.110 ms | 10.891 ms | 42.732 ms |
| B. gRPC | 1 | 503 B | 0.116 ms | 0.140 ms | 0.164 ms | 0.356 ms |
| B. gRPC | 100 | 9.5 KB | 0.472 ms | 0.512 ms | 0.743 ms | 1.685 ms |
| B. gRPC | 1,000 | 91.2 KB | 3.251 ms | 4.579 ms | 5.720 ms | 6.991 ms |
| B. gRPC | 2,000 | 182.1 KB | 6.356 ms | 8.469 ms | 9.202 ms | 10.826 ms |

At the size that matters the two are indistinguishable: 3.15 ms against
3.25 ms median, and gRPC's p99 is worse (5.72 ms against 4.63 ms). Candidate B
is better only in the far tail at 2,000 instruments.

### Round-trip latency — Go client on the host

The floor, with Python removed entirely.

| Transport | Universe | Request | Median | p99 |
|---|---:|---:|---:|---:|
| A. UDS + NDJSON | 1 | 507 B | 51 µs | 87 µs |
| A. UDS + NDJSON | 100 | 9.5 KB | 280 µs | 419 µs |
| A. UDS + NDJSON | 1,000 | 91.2 KB | 2.382 ms | 2.794 ms |
| A. UDS + NDJSON | 2,000 | 181.9 KB | 4.588 ms | 5.154 ms |
| B. gRPC | 1 | 511 B | 71 µs | 122 µs |
| B. gRPC | 100 | 9.5 KB | 325 µs | 504 µs |
| B. gRPC | 1,000 | 91.2 KB | 2.538 ms | 2.963 ms |
| B. gRPC | 2,000 | 181.9 KB | 4.805 ms | 5.346 ms |

### Where the time actually goes

`bench.py` times Python's JSON encoder on the same envelope separately:

| Universe | Round trip (median) | Python encode alone (median) | Encode share |
|---:|---:|---:|---:|
| 100 | 0.411 ms | 0.129 ms | 31% |
| 1,000 | 3.150 ms | 1.282 ms | 41% |
| 2,000 | 6.532 ms | 2.961 ms | 45% |

Nearly half the round trip at a realistic universe is `json.dumps` in CPython.
The socket is not the bottleneck and neither is gRPC — **the encoding is**.
This is the single most useful number in the spike: it says that if
per-bar latency ever needs to fall, the lever is the payload encoding, not the
transport, and that swapping transports would buy nothing.

### Deployment topology

| Arrangement | Works? | Median @ 1,000 | Note |
|---|---|---:|---|
| Socket on a macOS **bind mount**, Python in container | **No** | — | Socket file is visible; `connect()` returns `ECONNREFUSED` |
| Engine and Python in the **same container** | Yes | 3.150 ms | |
| Engine container + LEAN container, **shared named volume** | Yes | 3.160 ms | The realistic deployment shape |
| Host Python to host engine | Yes | 3.200 ms | |

The bind-mount failure is a property of Docker Desktop's macOS file sharing,
not of the transport: a Unix socket is a kernel object, and the macOS-to-VM
file share carries the inode but not the endpoint. Sharing the socket through a
Docker **named volume** — which lives inside the Linux VM — works at full
speed. On a Linux host, where LEAN and the engine share one kernel, the
question does not arise.

### Inside a real backtest

Both candidates were run under `lean backtest` with the algorithm calling the
engine on every daily bar, through a shared named volume. LEAN hosts Python
under pythonnet inside the .NET process, holding the GIL across the call; a
blocking socket call per bar proved unremarkable there.

| Transport | Bars | Median | First bar | Result |
|---|---:|---:|---:|---|
| A. UDS + NDJSON | 5 | 0.167 ms | 2.258 ms | Bar and decision exchanged on every bar |
| B. gRPC (grpcio 1.76.0) | 5 | 0.341 ms | 4.174 ms | Bar and decision exchanged on every bar |

The first bar is slower on both: connection setup and first-touch allocation.
An adapter should exchange one synthetic warm-up envelope during `Initialize`.

### Failure behaviour

All seven cases were run on the host and again inside the LEAN container, with
identical results (`adapter/lean/spike/faults.py`).

| Case | Observed | Adapter's correct response |
|---|---|---|
| Engine absent at startup | `connect()` fails, `ECONNREFUSED` | Refuse to trade |
| Adapter starts before the engine | Connect fails until the engine binds; a retry loop then connects | Bounded retry, then fail closed |
| Engine `SIGKILL`ed mid-run | Next exchange fails (`EPIPE` / truncated read) | Safe mode |
| Engine container killed mid-run | Same, within one bar | Safe mode |
| Engine restarted | The killed process leaves its socket file; the new engine reclaims it only because nothing is listening on it, then a new connection works | Reconnect |
| Oversized frame (2,000 instruments against a 64 KB limit) | Rejected as `oversized`; **the session survives** and the next bar succeeds | Drop the bar, keep the session |
| Slow engine, adapter's deadline first | Adapter's socket times out; the connection is abandoned and every later call fails fast | Safe mode |
| Slow engine, engine's own deadline first | Rejected as `timeout`; the session survives | Drop the bar |

Two of these are design decisions rather than observations, and both matter:

- **An oversized frame costs one bar, not the session.** The reader discards to
  the next newline within a bounded budget and answers with an error, so one
  malformed message from a still-healthy adapter does not end the trading day.
- **A timed-out connection is abandoned, not reused.** Nothing on the wire
  pairs a request with a reply beyond ordering. If the adapter gave up on bar
  *n* and reused the connection, bar *n*'s late reply would be read as the
  answer to bar *n+1* — a decision applied to the wrong bar, silently. The
  client checks that every reply's `causation_id` is the bar it sent, and marks
  the connection dead whenever an exchange does not complete.

### What gRPC costs

| | Candidate A | Candidate B |
|---|---:|---:|
| Modules added to the build list | 0 | 37 |
| Non-stdlib packages linked | 0 | 122 |
| Engine binary | 4.0 MB | 14 MB |
| Median @ 1,000, in container | 3.150 ms | 3.251 ms |

The 37 modules include OpenTelemetry, Envoy `go-control-plane`, CEL, and
`cloud.google.com/go/compute/metadata` — a load balancing and observability
stack this system does not use and would have to keep patched.

## Decision

**The LEAN adapter and the Go decision engine exchange envelopes over a
Unix-domain socket using newline-delimited JSON, one request and one reply at a
time per connection.** The wire contract is the Go package `transport`; the
Python side that proved it is `adapter/lean/spike/`.

**Local gRPC is rejected.** It is not faster at the universe size this system
runs, it is worse at p99 there, and it costs 37 modules and 122 linked packages
whose security and licence surface would have to be carried for the life of the
project. `docs/dependency-policy.md` asks what capability a dependency provides
that the standard library does not; at this boundary — one process, one peer,
one sequential stream, on one host (ADR 0001) — gRPC's real capabilities
(streaming, deadlines propagated across services, load balancing, wire
compatibility across many languages) are all things this boundary does not
need. `net` and `encoding/json` do need it.

The gRPC implementation is not kept in the tree. It was measured from a
separate Go module at commit `634e2cb` (`transport/grpcspike/`, on the #26
branch), and that commit is the reproducible reference. Keeping a second
module in the repository would have left a `go.sum` carrying 37 modules that
neither Dependabot nor `govulncheck` covers — an unscanned dependency surface
is what `docs/dependency-policy.md` exists to prevent, and it would have been
paid for a candidate this ADR rejects.

`docs/architecture.md`'s deferred decision "exact local transport" is now
closed by this ADR.

## Consequences

Constraints this puts on #27–#31:

1. **The socket must not be a macOS bind mount.** In development on macOS, put
   it in a Docker named volume shared between the LEAN container and the engine
   container, or run both in one container. Production on Linux is unconstrained.
2. **The socket path must be at most 103 bytes.** `sockaddr_un.sun_path` is 104
   bytes on Darwin and 108 on Linux, both including the NUL. `transport.Listen`
   checks this and says so, rather than letting `bind(2)` return a bare
   "invalid argument". A path under a macOS `TMPDIR` (`/var/folders/...`) can
   approach the limit on its own.
3. **The adapter must check causation on every reply and abandon a connection
   whose exchange did not complete.** This is not defensive coding; it is the
   only thing standing between a client timeout and a decision being applied to
   the wrong bar.
4. **Startup ordering is: engine binds, then adapter connects.** The adapter
   retries with a bound during `Initialize` and fails closed — calls `Quit()` —
   when the bound is exhausted. This was verified: with no engine listening,
   LEAN stopped before processing a single data point and placed no orders.
5. **A restarting engine must not displace a live one.** `transport.Listen`
   removes a socket file only after confirming nothing answers on it, and
   refuses to bind over a live server. This is what keeps the
   "only one active executor may submit orders" invariant true across a restart.
6. **1 MiB is the frame limit.** A 2,000-instrument universe is 182 KB, so the
   limit is roughly a 10,000-instrument universe. It is a configured value, and
   exceeding it costs one bar rather than the session.
7. **The adapter must exchange a warm-up envelope in `Initialize`.** The first
   bar costs 10-20× the median on both transports.
8. **Latency work belongs in the payload encoding, not the transport.** 41% of
   the round trip at 1,000 instruments is CPython's `json.dumps`. There is no
   transport change that recovers it.
9. **`from AlgorithmImports import *` shadows standard-library names.** It
   exports `datetime.time`, so `time.perf_counter()` fails at the first bar.
   The adapter must bind what it needs by name after the star import. This cost
   a backtest run to find and will cost the next person one too.
10. **The payload hash attests bytes, not structure.** The Python side must
    serialise with fixed separators so that hashing the payload alone yields
    the same bytes the envelope embeds. Changing the separators in one place
    makes every envelope fail `invalid_envelope`.

11. **Use `net.DialUnix` and `net.ListenUnix`, not `net.Dial` and
    `net.Listen`.** The generic pair parses an address string and reaches
    `net.LookupPort`, which is meaningless for a socket path and is the subject
    of GO-2026-4971 (a Windows-only panic, fixed in Go 1.25.10). With the
    generic calls, `govulncheck` reports the vulnerability as *reachable* and
    `make check` fails against the pinned Go 1.24.4 toolchain. Naming the
    address family removes the call path, so no toolchain bump and no
    time-bounded exception under `docs/dependency-policy.md` were needed.

Not addressed here, and deliberately out of scope:

- **The socket has no access control beyond filesystem permissions**, and
  `net.Listen("unix", ...)` creates it with the process umask. Anything that
  can open the file can inject decision requests. This needs an explicit mode
  and a decision about ownership before paper trading.
- The spike client is not the adapter. Reconnection with backoff, safe-mode
  entry and exit, and idempotent replay of an unacknowledged decision are #27
  onwards.
- Throughput under concurrent connections was not measured: the boundary is
  sequential by design and by LEAN's threading model.

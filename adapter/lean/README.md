# LEAN adapter

The deliberately thin Python boundary to QuantConnect LEAN.

Built so far: `algorithm.py` is a `QCAlgorithm` that publishes one
`market.bar.completed` envelope per completed daily bar, carrying both the
split-adjusted and raw price views (ADR 0004), continuing the Go engine's own
input sequence (`cmd/engine/engine.go`'s package doc, "Wire contract: the
adapter's first bar must carry Sequence 2"). Warm-up is counted in bars, not
calendar days, and no bar is sent to the engine while warming up, so it
decides nothing during that period. `client.py` is the transport, reused from
the measured ADR 0014 spike (`spike/`); `publisher.py` maps a LEAN bar and its
raw counterpart to the wire payload without any methodology.

The adapter will still need to:

- normalize universe changes, corporate actions, connection changes, and brokerage events into versioned messages;
- validate returned trade proposals against current LEAN state;
- submit approved orders through LEAN;
- return acknowledgements, rejections, cancellations, updates, and fills to Go; and
- enter safe mode and submit no new orders when Go is unavailable or state is uncertain.

It contains no methodology, position-sizing, pyramid, drawdown, or portfolio-risk rules — those stay in Go.

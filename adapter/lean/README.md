# LEAN adapter

This directory is reserved for the deliberately thin Python boundary to QuantConnect LEAN.

The adapter will:

- normalize completed market bars, universe changes, corporate actions, connection changes, and brokerage events into versioned messages;
- send those messages to the Go decision engine;
- validate returned trade proposals against current LEAN state;
- submit approved orders through LEAN;
- return acknowledgements, rejections, cancellations, updates, and fills to Go; and
- enter safe mode and submit no new orders when Go is unavailable or state is uncertain.

It will not contain methodology, position-sizing, pyramid, drawdown, or portfolio-risk rules. Executable adapter code should begin only after the message contract and first integration spike are approved.

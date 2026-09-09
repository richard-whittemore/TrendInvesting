package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// ErrUnavailable reports that the decision engine could not be reached, or
// stopped answering part-way through an exchange.
//
// This is the error the adapter must treat as "enter safe mode and submit no
// new orders" (docs/architecture.md). It is deliberately distinct from a
// [ProtocolError], which means the engine is alive and rejected one bar.
var ErrUnavailable = errors.New("transport: decision engine unavailable")

// ErrOutOfOrder reports a reply that does not answer the request just sent.
//
// Nothing on the wire pairs a request with a reply beyond ordering, so a
// client that abandons a request and reuses the connection would read the
// abandoned reply as the answer to the next bar. Checking causation makes that
// desynchronisation loud instead of silent, and it is why the client closes
// the connection whenever an exchange does not complete.
var ErrOutOfOrder = errors.New("transport: reply does not answer the request")

// ClientConfig configures a Client. The zero value is usable.
type ClientConfig struct {
	// MaxFrameBytes bounds one request or reply. Zero selects
	// DefaultMaxFrameBytes. It should match the server's limit.
	MaxFrameBytes int
}

func (c ClientConfig) maxFrameBytes() int {
	if c.MaxFrameBytes <= 0 {
		return DefaultMaxFrameBytes
	}
	return c.MaxFrameBytes
}

// Client is a sequential request/response client. It is not safe for
// concurrent use: a connection carries one exchange at a time, matching the
// single-threaded QCAlgorithm it exists to serve.
type Client struct {
	conn   net.Conn
	reader *frameReader
	max    int
	broken bool
}

// Dial connects to a decision engine listening at path.
//
// A dial failure is the "Go side absent at startup" case and is reported as
// [ErrUnavailable]; the adapter's correct response is to refuse to trade, not
// to retry silently.
func Dial(path string) (*Client, error) {
	return DialConfig(path, ClientConfig{})
}

// DialConfig is Dial with an explicit configuration.
func DialConfig(path string, cfg ClientConfig) (*Client, error) {
	conn, err := net.DialUnix("unix", nil, unixAddr(path))
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %w", ErrUnavailable, path, err)
	}
	maxFrame := cfg.maxFrameBytes()
	return &Client{
		conn:   conn,
		reader: newFrameReader(conn, maxFrame),
		max:    maxFrame,
	}, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("transport: close: %w", err)
	}
	return nil
}

// Decide sends one completed-bar envelope and returns the decision envelope.
//
// A deadline on ctx bounds the whole exchange. If the exchange does not
// complete — deadline, dead peer, desynchronised reply — the connection is
// marked broken and every later call fails fast, because a half-finished
// exchange leaves an unread reply that would be misread as the answer to the
// next bar.
func (c *Client) Decide(ctx context.Context, bar event.Envelope) (event.Envelope, error) {
	if c.broken {
		return event.Envelope{}, fmt.Errorf("%w: connection abandoned by an earlier failure", ErrUnavailable)
	}
	if err := c.applyDeadline(ctx); err != nil {
		return event.Envelope{}, err
	}
	decision, err := c.exchange(bar)
	if err != nil {
		if recoverable(err) {
			return event.Envelope{}, err
		}
		c.broken = true
		_ = c.conn.Close()
		return event.Envelope{}, err
	}
	return decision, nil
}

// recoverable reports whether an exchange failed without leaving the
// connection desynchronised. A rejection by a live engine and a request too
// large to have been sent both leave the stream exactly where it was.
func recoverable(err error) bool {
	if errors.Is(err, ErrUnavailable) {
		return false
	}
	var protocolErr *ProtocolError
	if errors.As(err, &protocolErr) {
		return true
	}
	return errors.Is(err, ErrFrameTooLarge)
}

func (c *Client) applyDeadline(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		// Clear any deadline left by a previous call rather than inheriting it.
		deadline = time.Time{}
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("%w: set deadline: %w", ErrUnavailable, err)
	}
	return nil
}

func (c *Client) exchange(bar event.Envelope) (event.Envelope, error) {
	if err := writeFrame(c.conn, bar, c.max); err != nil {
		if errors.Is(err, ErrFrameTooLarge) {
			// A request too large to send never reaches the engine, so the
			// engine's own oversize handling is not involved.
			return event.Envelope{}, err
		}
		return event.Envelope{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	frame, err := c.reader.next()
	if err != nil {
		return event.Envelope{}, fmt.Errorf("%w: read reply: %w", ErrUnavailable, err)
	}
	var reply Response
	if err := json.Unmarshal(frame, &reply); err != nil {
		return event.Envelope{}, fmt.Errorf("%w: decode reply: %w", ErrUnavailable, err)
	}
	if reply.Error != nil {
		if reply.Error.CausationID != "" && reply.Error.CausationID != bar.ID {
			return event.Envelope{}, fmt.Errorf("%w: error names %q, sent %q",
				ErrOutOfOrder, reply.Error.CausationID, bar.ID)
		}
		if reply.Error.Code == CodeUnavailable {
			return event.Envelope{}, fmt.Errorf("%w: %w", ErrUnavailable, reply.Error)
		}
		return event.Envelope{}, reply.Error
	}
	if reply.Envelope == nil {
		return event.Envelope{}, fmt.Errorf("%w: reply carries neither a decision nor an error", ErrUnavailable)
	}
	if reply.Envelope.CausationID != bar.ID {
		return event.Envelope{}, fmt.Errorf("%w: decision cites %q, sent %q",
			ErrOutOfOrder, reply.Envelope.CausationID, bar.ID)
	}
	return *reply.Envelope, nil
}

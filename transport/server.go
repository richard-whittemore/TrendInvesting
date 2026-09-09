package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// MaxSocketPathBytes is the longest Unix-domain socket path this package will
// accept.
//
// The limit is the platform's, not ours: sockaddr_un.sun_path is 104 bytes on
// Darwin and 108 on Linux, both including the terminating NUL. The smaller of
// the two is used unconditionally so that a path which binds on a developer's
// Mac also binds inside the LEAN container, and so that the failure is a
// legible error here rather than a bare "invalid argument" from bind(2).
const MaxSocketPathBytes = 103

// Decider turns a completed-bar envelope into a decision envelope. It is the
// whole of the Go side's contract with the adapter.
//
// A Decider must honour ctx: the server abandons a decision whose deadline
// passes, and a Decider that ignores cancellation keeps a goroutine alive
// until it returns on its own.
type Decider func(ctx context.Context, bar event.Envelope) (event.Envelope, error)

// ServerConfig configures a Server. The zero value is usable.
type ServerConfig struct {
	// MaxFrameBytes bounds one request or reply. Zero selects
	// DefaultMaxFrameBytes.
	MaxFrameBytes int
	// DecisionTimeout bounds one call to the Decider. Zero means no bound,
	// in which case a stuck Decider stalls its connection for ever.
	DecisionTimeout time.Duration
}

func (c ServerConfig) maxFrameBytes() int {
	if c.MaxFrameBytes <= 0 {
		return DefaultMaxFrameBytes
	}
	return c.MaxFrameBytes
}

// Server answers bar envelopes with decision envelopes over a Unix-domain
// socket.
type Server struct {
	listener net.Listener
	path     string
	decide   Decider
	cfg      ServerConfig

	// lifetime is cancelled by Close and is the parent of every decision
	// context, so that shutdown is bounded even when a Decider is stuck.
	// Without it, Close would wait for a handler that is itself waiting for a
	// decision that never arrives.
	lifetime context.Context
	stop     context.CancelFunc

	wg sync.WaitGroup

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Listen binds a Unix-domain socket at path and prepares to serve.
//
// A socket file left behind by a killed process is removed if nothing is
// listening on it. A socket that still answers is left alone and reported as
// an error, because two decision engines answering the same adapter would
// violate the single-active-executor invariant in docs/architecture.md.
func Listen(path string, decide Decider, cfg ServerConfig) (*Server, error) {
	if decide == nil {
		return nil, errors.New("transport: a decider is required")
	}
	if len(path) > MaxSocketPathBytes {
		return nil, fmt.Errorf(
			"transport: socket path is %d bytes, limit is %d: %s",
			len(path), MaxSocketPathBytes, path,
		)
	}
	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", unixAddr(path))
	if err != nil {
		return nil, fmt.Errorf("transport: listen on %s: %w", path, err)
	}
	lifetime, stop := context.WithCancel(context.Background())
	return &Server{
		listener: listener,
		path:     path,
		decide:   decide,
		cfg:      cfg,
		lifetime: lifetime,
		stop:     stop,
		conns:    make(map[net.Conn]struct{}),
	}, nil
}

// unixAddr names a Unix-domain endpoint.
//
// The address-family-specific net.DialUnix and net.ListenUnix are used rather
// than net.Dial and net.Listen: those parse an address string and reach
// net.LookupPort, which is both meaningless for a socket path and the subject
// of GO-2026-4971. Naming the family removes that path entirely.
func unixAddr(path string) *net.UnixAddr {
	return &net.UnixAddr{Name: path, Net: "unix"}
}

// clearStaleSocket removes a socket file that no process is listening on.
//
// It probes by connecting rather than by stat: the file's existence says
// nothing about whether anyone is behind it, and a killed engine always leaves
// its socket file on disk.
func clearStaleSocket(path string) error {
	conn, dialErr := net.DialUnix("unix", nil, unixAddr(path))
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("transport: a server is already listening on %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("transport: remove stale socket %s: %w", path, err)
	}
	return nil
}

// Path reports the socket path the server is bound to.
func (s *Server) Path() string { return s.path }

// Serve accepts connections until Close is called. It returns nil on an
// orderly shutdown.
func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.isClosed() {
				return nil
			}
			return fmt.Errorf("transport: accept: %w", err)
		}
		if !s.track(conn) {
			_ = conn.Close()
			return nil
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			s.handle(conn)
		}()
	}
}

// Close stops accepting, drops every live connection, and waits for the
// handlers to finish. It models the decision engine going away: an adapter
// blocked on a reply sees its connection end rather than hanging.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.conns = make(map[net.Conn]struct{})
	s.mu.Unlock()

	s.stop()
	err := s.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
	s.wg.Wait()
	if err != nil {
		return fmt.Errorf("transport: close listener: %w", err)
	}
	return nil
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
	_ = conn.Close()
}

func (s *Server) handle(conn net.Conn) {
	maxFrame := s.cfg.maxFrameBytes()
	reader := newFrameReader(conn, maxFrame)
	for {
		frame, err := reader.next()
		switch {
		case errors.Is(err, ErrFrameTooLarge):
			oversized := errorResponse(
				CodeOversized,
				fmt.Sprintf("frame exceeds %d bytes", maxFrame),
				"",
			)
			if writeFrame(conn, oversized, maxFrame) != nil {
				return
			}
			continue
		case err != nil:
			// io.EOF is an adapter that closed cleanly; anything else is a
			// connection that failed. Neither is recoverable here.
			return
		}
		if writeFrame(conn, s.respond(frame), maxFrame) != nil {
			return
		}
	}
}

func (s *Server) respond(frame []byte) Response {
	var bar event.Envelope
	if err := json.Unmarshal(frame, &bar); err != nil {
		return errorResponse(CodeInvalidFrame, err.Error(), "")
	}
	if err := bar.Validate(); err != nil {
		return errorResponse(CodeInvalidEnvelope, err.Error(), bar.ID)
	}
	decision, err := s.decideWithTimeout(bar)
	if err != nil {
		return errorResponse(decisionErrorCode(err), err.Error(), bar.ID)
	}
	return Response{Envelope: &decision, Error: nil}
}

func decisionErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return CodeTimeout
	case errors.Is(err, context.Canceled):
		// Only the server's own shutdown cancels a decision context.
		return CodeUnavailable
	default:
		return CodeDeciderFailed
	}
}

type decisionResult struct {
	decision event.Envelope
	err      error
}

func (s *Server) decideWithTimeout(bar event.Envelope) (event.Envelope, error) {
	ctx := s.lifetime
	if s.cfg.DecisionTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.DecisionTimeout)
		defer cancel()
	}
	// The result channel is buffered so that a Decider which outlives its
	// deadline can still finish and be collected instead of blocking for ever.
	results := make(chan decisionResult, 1)
	go func() {
		decision, err := s.decide(ctx, bar)
		results <- decisionResult{decision: decision, err: err}
	}()
	select {
	case result := <-results:
		return result.decision, result.err
	case <-ctx.Done():
		return event.Envelope{}, fmt.Errorf("transport: decision abandoned: %w", ctx.Err())
	}
}

// ServeContext runs Serve and shuts the server down when ctx is cancelled.
func (s *Server) ServeContext(ctx context.Context) error {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		_ = s.Close()
	}()
	err := s.Serve()
	_ = s.Close()
	<-stopped
	return err
}

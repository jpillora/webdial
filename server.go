package webdial

import (
	"compress/flate"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/eventsource"
)

const defaultMaxPostBytes int64 = 1 << 20 // 1 MiB

// ErrServerClosed is returned by Accept and Push after the server is closed.
var ErrServerClosed = errors.New("webdial: server closed")

type Server struct {
	// KeepAlive is the interval between keep-alive pings.
	// Zero means 25 seconds. Negative means disabled.
	KeepAlive time.Duration
	// CheckOrigin, when non-nil, validates the Origin header of WebSocket
	// upgrade requests. A nil function uses Gorilla WebSocket's secure default:
	// requests without an Origin are accepted, while requests with an Origin
	// must have an Origin host matching the request Host. CheckOrigin may be
	// called concurrently and must be safe for concurrent use.
	CheckOrigin func(*http.Request) bool
	// MaxPostBytes limits the body of each SSE data POST. Zero means 1 MiB.
	// Negative disables the limit. Oversized requests receive HTTP 413.
	MaxPostBytes int64
	// CompressionLevel is the flate level for per-message WS compression
	// (1-9, or flate.DefaultCompression). Zero means flate.BestSpeed.
	CompressionLevel int
	// WriteTimeout bounds a single WS frame write, so a peer that stops
	// reading cannot pin the connection forever. Zero means 30 seconds.
	// Negative means unbounded. Ignored once the caller sets its own write
	// deadline via the net.Conn interface.
	WriteTimeout time.Duration
	acceptCh     chan net.Conn
	sessions     sync.Map // map[string]*sseSession
	closed       chan struct{}
	closeOnce    sync.Once
}

func NewServer() *Server {
	return &Server{
		acceptCh: make(chan net.Conn, 16),
		closed:   make(chan struct{}),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "" {
		s.handleWS(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.handlePost(w, r)
		return
	}
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handleSSE(w, r)
		return
	}
	http.Error(w, "webdial: unsupported request", http.StatusBadRequest)
}

func (s *Server) Accept() (net.Conn, error) {
	select {
	case conn := <-s.acceptCh:
		return conn, nil
	case <-s.closed:
		return nil, ErrServerClosed
	}
}

// Push hands an externally established connection to a caller of Accept, so a
// single Accept loop can serve transports implemented outside this package.
//
// It blocks until the connection is accepted, ctx is done, or the server is
// closed. In the latter two cases Push closes conn and returns a non-nil
// error: a caller must never be left holding a connection nothing will read.
func (s *Server) Push(ctx context.Context, conn net.Conn) error {
	// A select chooses uniformly among ready cases, so a closed server with a
	// free queue slot would still swallow roughly half of all pushes. Settle
	// both terminal states first to make the documented guarantee hold.
	select {
	case <-s.closed:
		conn.Close()
		return ErrServerClosed
	default:
	}
	select {
	case <-ctx.Done():
		conn.Close()
		return ctx.Err()
	default:
	}
	select {
	case s.acceptCh <- conn:
		return nil
	case <-ctx.Done():
		conn.Close()
		return ctx.Err()
	case <-s.closed:
		conn.Close()
		return ErrServerClosed
	}
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.sessions.Range(func(key, value any) bool {
			sess := value.(*sseSession)
			sess.conn.Close()
			s.sessions.Delete(key)
			return true
		})
		// Connections queued but never accepted can no longer reach a caller,
		// so closing the server has to release them too.
		for {
			select {
			case conn := <-s.acceptCh:
				conn.Close()
			default:
				return
			}
		}
	})
	return nil
}

// KeepAliveInterval reports the effective keep-alive interval: KeepAlive, or 25
// seconds when it is zero. A negative value means keep-alives are disabled.
// Transports implemented in other packages use it to match this server's
// heartbeat pacing.
func (s *Server) KeepAliveInterval() time.Duration {
	if s.KeepAlive == 0 {
		return 25 * time.Second
	}
	return s.KeepAlive
}

func (s *Server) compressionLevel() int {
	if s.CompressionLevel == 0 {
		return flate.BestSpeed
	}
	return s.CompressionLevel
}

func (s *Server) writeTimeout() time.Duration {
	if s.WriteTimeout == 0 {
		return 30 * time.Second
	}
	if s.WriteTimeout < 0 {
		return 0
	}
	return s.WriteTimeout
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// The upgrader is request-local so different Server instances can safely
	// use different origin policies. Leaving CheckOrigin nil deliberately uses
	// Gorilla's same-origin default.
	upgrader := websocket.Upgrader{
		CheckOrigin:       s.CheckOrigin,
		EnableCompression: true, // per-message deflate (RFC 7692); browser decompresses natively
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn := newWSConn(ws, s.KeepAliveInterval(), s.compressionLevel(), s.writeTimeout())
	select {
	case s.acceptCh <- conn:
	case <-s.closed:
		conn.Close()
	}
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	sid := generateSessionID()
	conn := &sseServerConn{
		sessionID:     sid,
		w:             w,
		response:      http.NewResponseController(w),
		inbound:       make(chan []byte),
		readDeadline:  newConnDeadline(),
		writeDeadline: newConnDeadline(),
		closeCh:       make(chan struct{}),
		localAddr:     addr{transport: "sse", url: "server"},
		remoteAddr:    addr{transport: "sse", url: r.RemoteAddr},
	}
	s.sessions.Store(sid, newSSESession(conn))
	defer func() {
		conn.finishResponse()
		s.sessions.Delete(sid)
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if err := conn.writeEvent(eventsource.Event{
		Type: "sid",
		Data: []byte(sid),
	}); err != nil {
		conn.Close()
		return
	}
	select {
	case s.acceptCh <- conn:
	case <-s.closed:
		conn.Close()
		return
	}
	ka := s.KeepAliveInterval()
	if ka < 0 {
		select {
		case <-r.Context().Done():
		case <-conn.closeCh:
		case <-s.closed:
		}
		return
	}
	ticker := time.NewTicker(ka)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := conn.writeHeartbeat(); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		case <-conn.closeCh:
			return
		case <-s.closed:
			return
		}
	}
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("s")
	if sid == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	val, ok := s.sessions.Load(sid)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	sess := val.(*sseSession)
	if r.URL.Query().Get("close") == "1" {
		// A close POST is the peer ending the connection, not a local Close.
		// Reads observe it as a clean EOF, exactly like an orderly remote
		// stream teardown.
		sess.conn.shutdown(io.EOF)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if ping := r.URL.Query().Get("ping"); ping != "" {
		if err := sess.conn.writePong([]byte(ping)); err != nil {
			http.Error(w, "session closed", http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.handlePostData(w, r, sess)
}

func (s *Server) maxPostBytes() int64 {
	if s.MaxPostBytes == 0 {
		return defaultMaxPostBytes
	}
	return s.MaxPostBytes
}

func (s *Server) handlePostData(w http.ResponseWriter, r *http.Request, sess *sseSession) {
	limit := s.maxPostBytes()
	if limit >= 0 && r.ContentLength > limit {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	if err := sess.acquirePost(r.Context()); err != nil {
		writePostDeliveryError(w, r, err)
		return
	}
	defer sess.releasePost()

	var delivered bool
	var err error
	if limit >= 0 {
		// Buffer bounded bodies before delivery. Besides enforcing the limit for
		// chunked or dishonest requests, this prevents a rejected request from
		// contaminating the connection with a successfully delivered prefix.
		var buffered []byte
		buffered, err = io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			writePostReadError(w, err)
			return
		}
		delivered, err = deliverWholeBody(r.Context(), sess.conn, buffered)
	} else {
		delivered, err = deliverPostBody(r.Context(), sess.conn, r.Body)
	}
	if err == nil {
		err = r.Context().Err()
	}
	if err != nil {
		// A streamed unlimited POST may already have delivered a prefix. In that
		// case terminate the session so a retry cannot silently duplicate bytes.
		if delivered {
			sess.conn.shutdown(net.ErrClosed)
		}
		writePostDeliveryError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deliverWholeBody hands the peer one message per POST.
//
// A POST is one message on the wire, and the boundary is the sender's. Reading
// it out in fixed slices would hand the peer read boundaries it never chose,
// which silently breaks any framing layered on top — a type byte at the front
// of each message, a length prefix — because every piece after the first begins
// with payload where the header belongs. The websocket transport already keeps
// a message whole per Read (see wsConn.fill); this is the same guarantee for
// the SSE fallback.
func deliverWholeBody(ctx context.Context, conn *sseServerConn, body []byte) (bool, error) {
	if len(body) == 0 {
		return false, nil
	}
	if err := conn.deliver(ctx, body); err != nil {
		return false, err
	}
	return true, nil
}

// deliverPostBody streams an unbounded body, which cannot preserve the sender's
// message boundary: nothing has read it all, so there is no way to know where it
// ends. Only a server that has opted out of MaxPostBytes takes this path, and
// such a server is by definition treating the connection as a byte stream.
func deliverPostBody(ctx context.Context, conn *sseServerConn, body io.Reader) (bool, error) {
	delivered := false
	for {
		// Each successful send owns its backing array. The receiving Read may not
		// copy from the slice until after deliver returns, so reusing this buffer
		// would race with the next body read and could corrupt the byte stream.
		chunk := make([]byte, 32<<10)
		n, readErr := body.Read(chunk)
		if n > 0 {
			if err := conn.deliver(ctx, chunk[:n]); err != nil {
				return delivered, err
			}
			delivered = true
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return delivered, nil
			}
			return delivered, readErr
		}
		if n == 0 {
			select {
			case <-ctx.Done():
				return delivered, ctx.Err()
			default:
			}
		}
	}
}

func writePostReadError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "read error", http.StatusBadRequest)
}

func writePostDeliveryError(w http.ResponseWriter, r *http.Request, err error) {
	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr):
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
	case r.Context().Err() != nil:
		http.Error(w, "request canceled", http.StatusRequestTimeout)
	case errors.Is(err, io.ErrClosedPipe), errors.Is(err, net.ErrClosed):
		http.Error(w, "session closed", http.StatusGone)
	default:
		http.Error(w, "delivery error", http.StatusInternalServerError)
	}
}

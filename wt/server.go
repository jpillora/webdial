package wt

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/jpillora/webdial"
)

// Default WebTransport session limits. The transport uses a single
// bidirectional stream, so the stream allowances only need to be non-zero:
// left at zero the settings are not sent at all and the peer may not open
// anything. The data allowance is the initial flow-control window.
const (
	defaultMaxIncomingStreams = 16
	defaultMaxIncomingData    = 1 << 20 // 1 MiB
)

// Server serves webdial connections over WebTransport. Accepted connections go
// to the core webdial.Server, so one Accept loop serves every transport.
//
// WebTransport needs an HTTP/3 listener, which is separate from the TCP
// listener serving the WebSocket and SSE transports. A Server owns that
// listener: WebTransport sessions are tracked per QUIC connection, so the
// accept loop cannot be delegated to a plain http3.Server. Callers with their
// own QUIC listener can drive ServeQUICConn directly.
//
// The zero value is not usable; call NewServer. Exported fields must be set
// before the first call to ListenAndServe, Serve, ServeQUICConn or ServeHTTP.
type Server struct {
	// Addr is the UDP address ListenAndServe listens on. Empty means ":443".
	Addr string
	// TLSConfig is the server certificate. WebTransport is HTTPS only. The h3
	// ALPN is added automatically when absent.
	TLSConfig *tls.Config
	// Handler is served over HTTP/3. Nil serves s, which answers WebTransport
	// itself and delegates every other request to the core server, so a single
	// endpoint also serves the SSE fallback over HTTP/3.
	Handler http.Handler
	// CheckOrigin mirrors webdial.Server.CheckOrigin. Nil uses the same secure
	// default: a request whose Origin host does not match Host is rejected,
	// while a request without an Origin is accepted.
	CheckOrigin func(*http.Request) bool
	// KeepAlive overrides the QUIC keep-alive period. Zero inherits the core
	// server's KeepAliveInterval. Negative disables QUIC keep-alives.
	//
	// Unlike SSE, WebTransport has no webdial-level server heartbeat: QUIC
	// PING frames refresh the UDP path, which is what a keep-alive is for
	// here. This matches the WebSocket transport, whose heartbeat is also a
	// protocol-level ping invisible to the application.
	KeepAlive time.Duration
	// QUICConfig, when non-nil, is the base QUIC configuration. The extensions
	// WebTransport requires are applied to a copy, so the caller's value is
	// left untouched.
	QUICConfig *quic.Config

	core      *webdial.Server
	initOnce  sync.Once
	wt        *webtransport.Server
	closeOnce sync.Once
	closeErr  error
}

func NewServer(core *webdial.Server) *Server {
	return &Server{core: core}
}

func (s *Server) keepAlive() time.Duration {
	if s.KeepAlive != 0 {
		return s.KeepAlive
	}
	return s.core.KeepAliveInterval()
}

// upgrader builds the underlying WebTransport server on first use, so every
// exported field can be set after NewServer and before serving.
func (s *Server) upgrader() *webtransport.Server {
	s.initOnce.Do(func() {
		handler := s.Handler
		if handler == nil {
			handler = s
		}
		addr := s.Addr
		if addr == "" {
			addr = ":443"
		}
		s.wt = &webtransport.Server{
			H3: &http3.Server{
				Addr:       addr,
				Handler:    handler,
				TLSConfig:  withH3ALPN(s.TLSConfig),
				QUICConfig: quicConfig(s.QUICConfig, s.keepAlive()),
			},
			CheckOrigin: s.CheckOrigin,
			Config: &webtransport.Config{
				MaxIncomingStreams:    defaultMaxIncomingStreams,
				MaxIncomingUniStreams: defaultMaxIncomingStreams,
				MaxIncomingData:       defaultMaxIncomingData,
			},
		}
	})
	return s.wt
}

// ServeHTTP upgrades WebTransport CONNECT requests and pushes the resulting
// connections to the core server. Every other request is delegated to the core
// server unchanged.
//
// Routing keys on the method alone. The extended-CONNECT protocol token
// changed between drafts, so validating it here would pin webdial to one of
// them; Upgrade checks the method, the token and the origin itself.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		s.core.ServeHTTP(w, r)
		return
	}
	sess, err := s.upgrader().Upgrade(w, r)
	if err != nil {
		http.Error(w, "webdial: webtransport upgrade failed", http.StatusBadRequest)
		return
	}
	// The client opens the connection's only stream immediately after the
	// handshake. A peer that never does would otherwise park this goroutine
	// for the lifetime of the session.
	ctx, cancel := context.WithTimeout(sess.Context(), acceptStreamTimeout)
	stream, err := sess.AcceptStream(ctx)
	cancel()
	if err != nil {
		sess.CloseWithError(0, "")
		return
	}
	if err := s.core.Push(sess.Context(), newWTConn(sess, stream)); err != nil {
		// Push closed the connection already.
		return
	}
	// The session lives only as long as the CONNECT stream, and returning from
	// a handler finishes its response. Unlike the WebSocket handler, which can
	// return the moment the socket is hijacked, this one has to stay.
	<-sess.Context().Done()
}

// ListenAndServe listens for QUIC on Addr and serves HTTP/3.
func (s *Server) ListenAndServe() error {
	return s.upgrader().ListenAndServe()
}

// Serve serves HTTP/3 on an existing packet connection, which is how callers
// that need an ephemeral port get an address up front.
func (s *Server) Serve(conn net.PacketConn) error {
	return s.upgrader().Serve(conn)
}

// ServeQUICConn serves a single QUIC connection accepted elsewhere, for
// callers running their own QUIC listener.
func (s *Server) ServeQUICConn(conn *quic.Conn) error {
	return s.upgrader().ServeQUICConn(conn)
}

// Close stops the listener and closes established sessions. It does not close
// the core webdial.Server.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.upgrader().Close()
	})
	return s.closeErr
}

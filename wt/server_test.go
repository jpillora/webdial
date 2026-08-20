package wt

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
	"github.com/stretchr/testify/require"
)

// dialWithOrigin attempts a session carrying an explicit Origin header, which
// is what a browser sends and what the origin policy is there to filter.
func dialWithOrigin(t *testing.T, base string, pool *x509.CertPool, origin string) error {
	t.Helper()
	tr := &webtransport.Transport{
		TLSClientConfig: withH3ALPN(&tls.Config{RootCAs: pool}),
		QUICConfig:      quicConfig(nil, 0),
	}
	defer tr.Close()
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wtTestTimeout)
	defer cancel()
	_, sess, err := tr.Dial(ctx, base, hdr)
	if err == nil {
		sess.CloseWithError(0, "")
	}
	return err
}

func TestWTDefaultOriginPolicy(t *testing.T) {
	_, base, pool, core := newWTTestServer(t)
	go func() {
		for {
			conn, err := core.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()

	host := strings.TrimSuffix(strings.TrimPrefix(base, "https://"), "/wd/")

	t.Run("same origin allowed", func(t *testing.T) {
		require.NoError(t, dialWithOrigin(t, base, pool, "https://"+host))
	})
	t.Run("absent origin allowed", func(t *testing.T) {
		require.NoError(t, dialWithOrigin(t, base, pool, ""))
	})
	t.Run("foreign origin rejected", func(t *testing.T) {
		err := dialWithOrigin(t, base, pool, "https://evil.example")
		require.Error(t, err, "a cross-origin CONNECT must not establish a session")
	})
}

func TestWTOriginPolicyOverride(t *testing.T) {
	_, base, pool, core := newWTTestServer(t, func(s *Server) {
		s.CheckOrigin = func(r *http.Request) bool {
			return r.Header.Get("Origin") == "https://allowed.example"
		}
	})
	go func() {
		for {
			conn, err := core.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()

	require.NoError(t, dialWithOrigin(t, base, pool, "https://allowed.example"))
	require.Error(t, dialWithOrigin(t, base, pool, "https://denied.example"))
}

// TestWTDialPreservesMountedPath mirrors the root package's equivalent: the
// endpoint URL is used exactly as supplied, mount path and query included.
func TestWTDialPreservesMountedPath(t *testing.T) {
	var gotPath, gotQuery string
	srv, base, pool, core := newWTTestServer(t, func(s *Server) {
		mux := http.NewServeMux()
		mux.Handle("/wd/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
			s.ServeHTTP(w, r)
		}))
		s.Handler = mux
	})
	_ = srv

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := core.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	endpoint := base + "?next=https://upstream.example/a/"
	ctx, cancel := context.WithTimeout(context.Background(), wtTestTimeout)
	defer cancel()
	client, err := (&Dialer{TLSConfig: &tls.Config{RootCAs: pool}}).Dial(ctx, endpoint)
	require.NoError(t, err)
	defer client.Close()

	select {
	case server := <-accepted:
		defer server.Close()
	case <-time.After(wtTestTimeout):
		t.Fatal("timed out waiting for the mounted handler to accept")
	}

	require.Equal(t, "/wd/", gotPath)
	require.Equal(t, "next=https://upstream.example/a/", gotQuery)
}

// TestWTHandlerDelegatesNonWebTransportRequests proves one endpoint serves both
// WebTransport and, over the same HTTP/3 listener, the SSE fallback.
func TestWTHandlerDelegatesNonWebTransportRequests(t *testing.T) {
	_, base, pool, core := newWTTestServer(t)

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := core.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	rt := &http3.Transport{TLSClientConfig: withH3ALPN(&tls.Config{RootCAs: pool})}
	defer rt.Close()
	client := &http.Client{Transport: rt}

	req, err := http.NewRequest(http.MethodGet, base, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// The core server opens every SSE stream with the session id event.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "event: sid", strings.TrimSpace(line))

	select {
	case conn := <-accepted:
		conn.Close()
	case <-time.After(wtTestTimeout):
		t.Fatal("timed out waiting for the delegated SSE connection to be accepted")
	}
}

// TestWTHandlerOutlivesSession is the regression guard for the rule that the
// handler must not return while the session is live: a WebTransport session
// lives only as long as its CONNECT stream, which the handler owns.
func TestWTHandlerOutlivesSession(t *testing.T) {
	pair := newWTTestPair(t)

	for i := range 3 {
		// A handler that returned after handing the connection over would have
		// finished its response by now and taken the session with it.
		time.Sleep(150 * time.Millisecond)

		payload := []byte{byte('a' + i)}
		_, err := pair.client.Write(payload)
		require.NoErrorf(t, err, "write on round %d", i)

		buf := make([]byte, 8)
		require.NoError(t, pair.server.SetReadDeadline(time.Now().Add(wtTestTimeout)))
		n, err := pair.server.Read(buf)
		require.NoErrorf(t, err, "read on round %d", i)
		require.Equal(t, payload, buf[:n])
	}
}

// TestWTUpgradeAfterCoreCloseClosesSession checks the handoff refusal path:
// Push closes the connection, the handler returns, and the client must find
// out rather than hanging on a session nobody owns.
func TestWTUpgradeAfterCoreCloseClosesSession(t *testing.T) {
	_, base, pool, core := newWTTestServer(t)
	require.NoError(t, core.Close())

	ctx, cancel := context.WithTimeout(context.Background(), wtTestTimeout)
	defer cancel()
	client, err := (&Dialer{TLSConfig: &tls.Config{RootCAs: pool}}).Dial(ctx, base)
	if err != nil {
		return // refused outright, which is also a correct outcome
	}
	defer client.Close()

	errCh := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		errCh <- err
	}()
	select {
	case err := <-errCh:
		require.Error(t, err, "reading a connection the server refused must fail")
	case <-time.After(wtTestTimeout):
		t.Fatal("timed out waiting for the refused session to tear down")
	}
}

func TestWTServerCloseEndsSessions(t *testing.T) {
	pair := newWTTestPair(t)
	require.NoError(t, pair.wt.Close())

	errCh := make(chan error, 1)
	go func() {
		_, err := pair.client.Read(make([]byte, 1))
		errCh <- err
	}()
	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(wtTestTimeout):
		t.Fatal("timed out waiting for server close to reach the client")
	}
}

func TestWTDialRejectsBadScheme(t *testing.T) {
	_, err := Dial(context.Background(), "ws://example.com/wd/")
	require.Error(t, err)
	require.Contains(t, err.Error(), "webtransport requires an https URL")
}

package webdial

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const sseIOTestTimeout = 2 * time.Second

type sseTestPair struct {
	client net.Conn
	server net.Conn
	srv    *Server
	http   *httptest.Server
}

func newSSETestPair(t *testing.T, wrap func(http.Handler) http.Handler) sseTestPair {
	t.Helper()
	srv := NewServer()
	srv.KeepAlive = -1
	handler := http.Handler(srv)
	if wrap != nil {
		handler = wrap(handler)
	}
	ts := httptest.NewServer(handler)

	accepted := make(chan contextConnResult, 1)
	go func() {
		conn, err := srv.Accept()
		accepted <- contextConnResult{conn: conn, err: err}
	}()
	client, err := dialSSE(context.Background(), ts.URL)
	if err != nil {
		ts.Close()
		srv.Close()
		t.Fatalf("dial SSE: %v", err)
	}
	server := waitForContextConn(t, accepted, "server to accept SSE connection")
	require.NoError(t, server.err)

	pair := sseTestPair{client: client, server: server.conn, srv: srv, http: ts}
	t.Cleanup(func() {
		pair.client.Close()
		pair.server.Close()
		pair.http.Close()
		pair.srv.Close()
	})
	return pair
}

func requireTimeoutError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	var netErr net.Error
	require.ErrorAs(t, err, &netErr)
	require.True(t, netErr.Timeout())
}

func waitForIOResult(t *testing.T, result <-chan contextReadResult, description string) contextReadResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(sseIOTestTimeout):
		t.Fatalf("timed out waiting for %s", description)
		return contextReadResult{}
	}
}

func asyncWrite(conn net.Conn, data []byte) <-chan contextReadResult {
	result := make(chan contextReadResult, 1)
	go func() {
		n, err := conn.Write(data)
		result <- contextReadResult{data: data[:n], err: err}
	}()
	return result
}

func TestSSEClientReadDeadline(t *testing.T) {
	pair := newSSETestPair(t, nil)
	require.NoError(t, pair.client.SetReadDeadline(time.Now().Add(40*time.Millisecond)))

	_, err := pair.client.Read(make([]byte, 1))
	requireTimeoutError(t, err)

	// A deadline does not poison the connection. Clearing it lets the decoder
	// pump deliver subsequent bytes.
	require.NoError(t, pair.client.SetReadDeadline(time.Time{}))
	require.NoError(t, pair.server.SetWriteDeadline(time.Now().Add(sseIOTestTimeout)))
	serverWrite := asyncWrite(pair.server, []byte("ok"))
	buf := make([]byte, 2)
	_, err = io.ReadFull(pair.client, buf)
	require.NoError(t, err)
	require.Equal(t, "ok", string(buf))
	require.NoError(t, waitForIOResult(t, serverWrite, "server write after read deadline").err)
}

func TestSSEServerReadDeadline(t *testing.T) {
	pair := newSSETestPair(t, nil)
	require.NoError(t, pair.server.SetReadDeadline(time.Now().Add(40*time.Millisecond)))

	_, err := pair.server.Read(make([]byte, 1))
	requireTimeoutError(t, err)

	require.NoError(t, pair.server.SetReadDeadline(time.Time{}))
	clientWrite := asyncWrite(pair.client, []byte("ok"))
	buf := make([]byte, 2)
	_, err = io.ReadFull(pair.server, buf)
	require.NoError(t, err)
	require.Equal(t, "ok", string(buf))
	require.NoError(t, waitForIOResult(t, clientWrite, "client write after read deadline").err)
}

func TestSSEClientWriteDeadlineInterruptsBlockedDelivery(t *testing.T) {
	postDone := make(chan struct{}, 1)
	pair := newSSETestPair(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				defer func() { postDone <- struct{}{} }()
			}
			next.ServeHTTP(w, r)
		})
	})
	// The accepted connection deliberately never reads, so the POST cannot be
	// acknowledged until the write deadline cancels its request.
	require.NoError(t, pair.client.SetWriteDeadline(time.Now().Add(40*time.Millisecond)))
	_, err := pair.client.Write([]byte("blocked"))
	requireTimeoutError(t, err)
	waitForContextSignal(t, postDone, "timed-out POST handler to stop")

	// Resetting the deadline makes later writes usable.
	require.NoError(t, pair.client.SetWriteDeadline(time.Time{}))
	write := asyncWrite(pair.client, []byte("delivered"))
	buf := make([]byte, len("delivered"))
	_, err = io.ReadFull(pair.server, buf)
	require.NoError(t, err)
	require.Equal(t, "delivered", string(buf))
	require.NoError(t, waitForIOResult(t, write, "client write after write deadline").err)
}

func TestSSEClientCloseInterruptsBlockedIO(t *testing.T) {
	postStarted := make(chan struct{})
	var postOnce sync.Once
	pair := newSSETestPair(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				postOnce.Do(func() { close(postStarted) })
			}
			next.ServeHTTP(w, r)
		})
	})

	read := asyncContextReadFull(pair.client, 1)
	write := asyncWrite(pair.client, []byte("blocked"))
	waitForContextSignal(t, postStarted, "blocked POST to reach the server")

	closed := make(chan error, 1)
	go func() { closed <- pair.client.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(sseIOTestTimeout):
		t.Fatal("client Close blocked behind in-flight Write")
	}
	require.Error(t, waitForIOResult(t, read, "blocked client read to stop").err)
	require.Error(t, waitForIOResult(t, write, "blocked client write to stop").err)
}

func TestSSEServerCloseInterruptsBlockedRead(t *testing.T) {
	pair := newSSETestPair(t, nil)
	readStarted := make(chan struct{})
	read := make(chan contextReadResult, 1)
	go func() {
		close(readStarted)
		buf := make([]byte, 1)
		n, err := pair.server.Read(buf)
		read <- contextReadResult{data: buf[:n], err: err}
	}()
	waitForContextSignal(t, readStarted, "server read to start")

	require.NoError(t, pair.server.Close())
	require.Error(t, waitForIOResult(t, read, "blocked server read to stop").err)
}

// deadlineBlockingResponseWriter deterministically models a ResponseWriter
// stalled in a network write. ResponseController discovers SetWriteDeadline
// directly and uses it to wake the blocked Write.
type deadlineBlockingResponseWriter struct {
	header       http.Header
	writeStarted chan struct{}
	startOnce    sync.Once
	deadline     connDeadline
}

func newDeadlineBlockingResponseWriter() *deadlineBlockingResponseWriter {
	return &deadlineBlockingResponseWriter{
		header:       make(http.Header),
		writeStarted: make(chan struct{}),
		deadline:     newConnDeadline(),
	}
}

func (w *deadlineBlockingResponseWriter) Header() http.Header { return w.header }

func (w *deadlineBlockingResponseWriter) WriteHeader(int) {}

func (w *deadlineBlockingResponseWriter) Write([]byte) (int, error) {
	w.startOnce.Do(func() { close(w.writeStarted) })
	for {
		deadline := w.deadline.snapshot()
		if deadline.expired {
			return 0, os.ErrDeadlineExceeded
		}
		select {
		case <-deadline.timerC():
			return 0, os.ErrDeadlineExceeded
		case <-deadline.changed:
			deadline.stop()
		}
	}
}

func (w *deadlineBlockingResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline.set(deadline)
	return nil
}

func newTestSSEServerConn(w http.ResponseWriter) *sseServerConn {
	return &sseServerConn{
		w:             w,
		response:      http.NewResponseController(w),
		inbound:       make(chan []byte),
		readDeadline:  newConnDeadline(),
		writeDeadline: newConnDeadline(),
		closeCh:       make(chan struct{}),
	}
}

func TestSSEServerWriteDeadlineInterruptsBlockedWrite(t *testing.T) {
	w := newDeadlineBlockingResponseWriter()
	conn := newTestSSEServerConn(w)
	write := asyncWrite(conn, []byte("blocked"))
	waitForContextSignal(t, w.writeStarted, "server response write to block")

	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(40*time.Millisecond)))
	result := waitForIOResult(t, write, "server write deadline")
	requireTimeoutError(t, result.err)
}

func TestSSEServerCloseInterruptsBlockedWrite(t *testing.T) {
	w := newDeadlineBlockingResponseWriter()
	conn := newTestSSEServerConn(w)
	write := asyncWrite(conn, []byte("blocked"))
	waitForContextSignal(t, w.writeStarted, "server response write to block")

	require.NoError(t, conn.Close())
	result := waitForIOResult(t, write, "server write to stop after Close")
	require.Error(t, result.err)
}

type notifyingReader struct {
	reader *bytes.Reader
	read   chan struct{}
	once   sync.Once
}

func (r *notifyingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.once.Do(func() { close(r.read) })
	}
	return n, err
}

func TestSSEPostCancellationIsNotAcknowledged(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	w := newDeadlineBlockingResponseWriter()
	conn := newTestSSEServerConn(w)
	const sid = "blocked-session"
	srv.sessions.Store(sid, &sseSession{conn: conn})
	defer srv.sessions.Delete(sid)

	bodyRead := make(chan struct{})
	body := &notifyingReader{reader: bytes.NewReader([]byte("undelivered")), read: bodyRead}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/?s="+sid, body).WithContext(ctx)
	res := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(res, req)
		close(done)
	}()
	waitForContextSignal(t, bodyRead, "POST body to be read")
	cancel()
	waitForContextSignal(t, done, "canceled POST handler to return")

	require.Equal(t, http.StatusGone, res.Code)
	conn.Close()
}

func TestSSEClientWriteSurfacesDeliveryFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, "not delivered", http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: sid\ndata: failed-delivery\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	conn, err := dialSSE(context.Background(), ts.URL)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("lost"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "post returned 410")
}

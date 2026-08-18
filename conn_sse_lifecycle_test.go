package webdial

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jpillora/eventsource"
	"github.com/stretchr/testify/require"
)

func TestSSEServerRemoteDisconnectReturnsEOFAndRejectsWrites(t *testing.T) {
	getDone := make(chan struct{})
	srv := NewServer()
	srv.KeepAlive = -1
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
		if r.Method == http.MethodGet {
			close(getDone)
		}
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { require.NoError(t, srv.Close()) })

	accepted := make(chan contextConnResult, 1)
	go func() {
		conn, err := srv.Accept()
		accepted <- contextConnResult{conn: conn, err: err}
	}()
	client, err := dialSSE(context.Background(), ts.URL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	server := waitForContextConn(t, accepted, "server to accept SSE connection")
	require.NoError(t, server.err)

	read := asyncContextReadFull(server.conn, 1)
	// Closing only the response body models an abrupt remote stream loss rather
	// than a cooperative server-side connection Close.
	require.NoError(t, client.(*sseClientConn).sseResp.Body.Close())
	waitForContextSignal(t, getDone, "SSE handler to return after remote disconnect")

	readResult := waitForContextRead(t, read, "server read to observe remote disconnect")
	require.ErrorIs(t, readResult.err, io.EOF)
	_, err = server.conn.Write([]byte("after handler return"))
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

func TestSSEBlockedPostReleasedByRemoteDisconnect(t *testing.T) {
	pair := newSSETestPair(t, nil)
	client := pair.client.(*sseClientConn)
	value, ok := pair.srv.sessions.Load(client.sessionID)
	require.True(t, ok)
	session := value.(*sseSession)

	type postResult struct {
		status int
		err    error
	}
	postResultCh := make(chan postResult, 1)
	go func() {
		resp, err := http.Post(client.postURL, "application/octet-stream", strings.NewReader("blocked"))
		if err != nil {
			postResultCh <- postResult{err: err}
			return
		}
		resp.Body.Close()
		postResultCh <- postResult{status: resp.StatusCode}
	}()
	require.Eventually(t, func() bool {
		return len(session.postLock) == 1
	}, sseIOTestTimeout, time.Millisecond, "POST never entered serialized delivery")

	require.NoError(t, client.sseResp.Body.Close())
	select {
	case result := <-postResultCh:
		require.NoError(t, result.err)
		require.Equal(t, http.StatusGone, result.status)
	case <-time.After(sseIOTestTimeout):
		t.Fatal("timed out waiting for blocked POST to stop after remote disconnect")
	}
}

func TestSSEDeliveryCommitSurvivesConcurrentShutdown(t *testing.T) {
	// With one P, Gosched runs the delivery goroutine until it parks on the
	// unbuffered send. The receiving goroutine then keeps the P long enough to
	// publish shutdown before the sender resumes, deterministically exercising
	// the boundary immediately after the handoff commits.
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)

	conn := newTestSSEServerConn(httptest.NewRecorder())
	want := []byte("committed")
	deliveryStarted := make(chan struct{})
	deliveryResult := make(chan error, 1)
	go func() {
		close(deliveryStarted)
		deliveryResult <- conn.deliver(context.Background(), want)
	}()
	<-deliveryStarted
	runtime.Gosched()

	got := <-conn.inbound
	require.Equal(t, want, got)
	// Model the winning shutdown's publication at precisely this boundary. The
	// full shutdown follows after observing the delivery result.
	conn.closed.Store(true)
	require.NoError(t, <-deliveryResult)
	conn.shutdown(net.ErrClosed)
}

type unsupportedBlockingResponseWriter struct {
	header          http.Header
	block           atomic.Bool
	writeStarted    chan struct{}
	releaseWrite    chan struct{}
	startOnce       sync.Once
	handlerReturned atomic.Bool
	lateWrites      atomic.Int32
}

func newUnsupportedBlockingResponseWriter() *unsupportedBlockingResponseWriter {
	return &unsupportedBlockingResponseWriter{
		header:       make(http.Header),
		writeStarted: make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
}

func (w *unsupportedBlockingResponseWriter) Header() http.Header { return w.header }

func (w *unsupportedBlockingResponseWriter) WriteHeader(int) {}

func (w *unsupportedBlockingResponseWriter) Write(p []byte) (int, error) {
	if w.handlerReturned.Load() {
		w.lateWrites.Add(1)
	}
	if w.block.Load() {
		w.startOnce.Do(func() { close(w.writeStarted) })
		<-w.releaseWrite
	}
	if w.handlerReturned.Load() {
		w.lateWrites.Add(1)
	}
	return len(p), nil
}

func TestSSEHandlerWaitsForBlockedUnsupportedWriter(t *testing.T) {
	srv := NewServer()
	srv.KeepAlive = -1
	defer srv.Close()
	w := newUnsupportedBlockingResponseWriter()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	handlerDone := make(chan struct{})
	go func() {
		srv.ServeHTTP(w, req)
		w.handlerReturned.Store(true)
		close(handlerDone)
	}()

	conn, err := srv.Accept()
	require.NoError(t, err)
	w.block.Store(true)
	write := asyncWrite(conn, []byte("blocked"))
	waitForContextSignal(t, w.writeStarted, "application response write to block")
	cancel()
	waitForContextSignal(t, conn.(*sseServerConn).closeCh, "remote shutdown to be published")

	select {
	case <-handlerDone:
		t.Fatal("SSE handler returned while an application write still owned ResponseWriter")
	case <-time.After(50 * time.Millisecond):
	}
	close(w.releaseWrite)
	_ = waitForIOResult(t, write, "blocked application write to finish")
	waitForContextSignal(t, handlerDone, "SSE handler to finish after response write")

	require.Zero(t, w.lateWrites.Load())
	_, err = conn.Write([]byte("late"))
	require.ErrorIs(t, err, io.ErrClosedPipe)
	require.Zero(t, w.lateWrites.Load(), "closed connection touched ResponseWriter after handler return")
}

type gatedDeadlineResponseWriter struct {
	header          http.Header
	deadlineStarted chan struct{}
	releaseDeadline chan struct{}
	startOnce       sync.Once
	deadlineCalls   atomic.Int32
}

func newGatedDeadlineResponseWriter() *gatedDeadlineResponseWriter {
	return &gatedDeadlineResponseWriter{
		header:          make(http.Header),
		deadlineStarted: make(chan struct{}),
		releaseDeadline: make(chan struct{}),
	}
}

func (w *gatedDeadlineResponseWriter) Header() http.Header { return w.header }

func (w *gatedDeadlineResponseWriter) WriteHeader(int) {}

func (w *gatedDeadlineResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func (w *gatedDeadlineResponseWriter) SetWriteDeadline(time.Time) error {
	w.deadlineCalls.Add(1)
	w.startOnce.Do(func() { close(w.deadlineStarted) })
	<-w.releaseDeadline
	return http.ErrNotSupported
}

func TestSSEShutdownPublishesOnceBeforeUnsupportedControllerReturns(t *testing.T) {
	w := newGatedDeadlineResponseWriter()
	conn := newTestSSEServerConn(w)
	read := asyncContextReadFull(conn, 1)

	const shutdowns = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(shutdowns)
	for i := 0; i < shutdowns; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			switch i % 3 {
			case 0:
				_ = conn.Close()
			case 1:
				conn.finishResponse()
			case 2:
				conn.shutdown(io.EOF)
			}
		}()
	}
	close(start)
	waitForContextSignal(t, w.deadlineStarted, "shutdown ResponseController call")

	// closeCh is published before the potentially unsupported controller call,
	// so blocked application reads do not inherit that delay.
	readResult := waitForContextRead(t, read, "read release during controller call")
	require.True(t, errors.Is(readResult.err, io.EOF) || errors.Is(readResult.err, net.ErrClosed))
	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()
	select {
	case <-allDone:
		t.Fatal("concurrent shutdown returned before winning teardown completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(w.releaseDeadline)
	waitForContextSignal(t, allDone, "all simultaneous shutdown calls")
	require.EqualValues(t, 1, w.deadlineCalls.Load())
	require.True(t, conn.closed.Load())
}

func TestSSEClosePostSurfacesEOFToServerReads(t *testing.T) {
	pair := newSSETestPair(t, nil)
	client := pair.client.(*sseClientConn)

	read := asyncContextReadFull(pair.server, 1)
	closeURL, err := appendURLQueryParam(client.postURL, "close", "1")
	require.NoError(t, err)
	resp, err := http.Post(closeURL, "application/octet-stream", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// The peer ended the connection cooperatively; that is a clean EOF, the
	// same signal an orderly remote stream teardown produces, not the
	// local-misuse net.ErrClosed.
	readResult := waitForContextRead(t, read, "server read to observe cooperative remote close")
	require.ErrorIs(t, readResult.err, io.EOF)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSSEClientWriteDeliveredDespiteConcurrentClose(t *testing.T) {
	// Keep the decode goroutine parked so only this test controls lifecycle
	// state. Closing pw on cleanup lets it exit.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	var conn *sseClientConn
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, r.Body)
		// The POST body has been fully delivered; model a concurrent Close
		// landing before the writer observes the result.
		conn.closed.Store(true)
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	conn, err := newSSEClientConn("http://example.invalid/wd", "sid-1",
		&http.Response{Body: io.NopCloser(pr)}, eventsource.NewDecoder(pr),
		&http.Client{Transport: transport}, ctx, cancel)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	n, err := conn.Write([]byte("delivered"))
	require.NoError(t, err, "a completed 204 is a delivered write, even during close")
	require.Equal(t, len("delivered"), n)
}

func TestSSEClientReadAfterLocalCloseReturnsErrClosed(t *testing.T) {
	pair := newSSETestPair(t, nil)
	require.NoError(t, pair.client.Close())
	// Close claims the terminal error before canceling the stream, so the
	// decode goroutine's own teardown error is discarded and every later read
	// agrees, regardless of goroutine scheduling.
	for i := 0; i < 20; i++ {
		_, err := pair.client.Read(make([]byte, 1))
		require.ErrorIs(t, err, net.ErrClosed)
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSSEServerRejectsCloseEventAfterShutdown(t *testing.T) {
	conn := newTestSSEServerConn(httptest.NewRecorder())
	require.NoError(t, conn.Close())
	// finishResponse may return the moment shutdown publishes closed; no event
	// type, including close, may touch the ResponseWriter afterwards.
	err := conn.writeEvent(eventsource.Event{Type: "close"})
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

func TestSSESimultaneousClientServerAndConnectionClose(t *testing.T) {
	for range 20 {
		srv := NewServer()
		srv.KeepAlive = -1
		ts := httptest.NewServer(srv)
		accepted := make(chan contextConnResult, 1)
		go func() {
			conn, err := srv.Accept()
			accepted <- contextConnResult{conn: conn, err: err}
		}()
		client, err := dialSSE(context.Background(), ts.URL)
		require.NoError(t, err)
		server := waitForContextConn(t, accepted, "server to accept SSE connection")
		require.NoError(t, server.err)

		start := make(chan struct{})
		errs := make(chan error, 3)
		go func() {
			<-start
			errs <- client.Close()
		}()
		go func() {
			<-start
			errs <- srv.Close()
		}()
		go func() {
			<-start
			errs <- server.conn.Close()
		}()
		close(start)
		for range 3 {
			require.NoError(t, <-errs)
		}
		ts.Close()
		_, err = server.conn.Write([]byte("closed"))
		require.ErrorIs(t, err, io.ErrClosedPipe)
	}
}

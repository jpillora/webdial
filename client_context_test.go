package webdial

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const contextTestTimeout = 2 * time.Second

type contextConnResult struct {
	conn net.Conn
	err  error
}

type contextReadResult struct {
	data []byte
	err  error
}

func asyncContextDialSSE(ctx context.Context, url string) <-chan contextConnResult {
	result := make(chan contextConnResult, 1)
	go func() {
		conn, err := dialSSE(ctx, url)
		result <- contextConnResult{conn: conn, err: err}
	}()
	return result
}

func waitForContextSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(contextTestTimeout):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForContextConn(t *testing.T, result <-chan contextConnResult, description string) contextConnResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(contextTestTimeout):
		t.Fatalf("timed out waiting for %s", description)
		return contextConnResult{}
	}
}

func asyncContextReadFull(conn net.Conn, size int) <-chan contextReadResult {
	result := make(chan contextReadResult, 1)
	go func() {
		buf := make([]byte, size)
		_, err := io.ReadFull(conn, buf)
		result <- contextReadResult{data: buf, err: err}
	}()
	return result
}

func waitForContextRead(t *testing.T, result <-chan contextReadResult, description string) contextReadResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(contextTestTimeout):
		t.Fatalf("timed out waiting for %s", description)
		return contextReadResult{}
	}
}

func TestDialSSEContextCanceledBeforeConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	conn, err := dialSSE(ctx, "http://127.0.0.1:1")
	require.Nil(t, conn)
	require.ErrorIs(t, err, context.Canceled)
}

func TestDialSSEContextCanceledBeforeHeaders(t *testing.T) {
	requestStarted := make(chan struct{})
	handlerCanceled := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
		close(handlerCanceled)
	}))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	result := asyncContextDialSSE(ctx, ts.URL)
	waitForContextSignal(t, requestStarted, "request to reach the server")
	cancel()

	got := waitForContextConn(t, result, "dial to return after context cancellation")
	require.Nil(t, got.conn)
	require.ErrorIs(t, got.err, context.Canceled)
	waitForContextSignal(t, handlerCanceled, "server request context cancellation")
}

func TestDialSSEContextCanceledWhileWaitingForSID(t *testing.T) {
	headersSent := make(chan struct{})
	handlerCanceled := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(headersSent)
		<-r.Context().Done()
		close(handlerCanceled)
	}))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	result := asyncContextDialSSE(ctx, ts.URL)
	waitForContextSignal(t, headersSent, "SSE response headers")
	cancel()

	got := waitForContextConn(t, result, "dial to return after context cancellation")
	require.Nil(t, got.conn)
	require.ErrorIs(t, got.err, context.Canceled)
	waitForContextSignal(t, handlerCanceled, "server stream context cancellation")
}

func TestDialSSEContextDoesNotOwnEstablishedConnection(t *testing.T) {
	srv := NewServer()
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	accepted := make(chan contextConnResult, 1)
	go func() {
		conn, err := srv.Accept()
		accepted <- contextConnResult{conn: conn, err: err}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	conn, err := dialSSE(ctx, ts.URL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	// This is the common DialContext pattern: release establishment resources
	// immediately once Dial returns, then use the connection normally.
	cancel()

	serverSide := waitForContextConn(t, accepted, "server to accept SSE connection")
	require.NoError(t, serverSide.err)
	t.Cleanup(func() { require.NoError(t, serverSide.conn.Close()) })
	serverDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 4)
		_, readErr := io.ReadFull(serverSide.conn, buf)
		if readErr != nil {
			serverDone <- readErr
			return
		}
		if string(buf) != "ping" {
			serverDone <- fmt.Errorf("server read %q, want ping", buf)
			return
		}
		_, readErr = serverSide.conn.Write([]byte("pong"))
		serverDone <- readErr
	}()

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	read := waitForContextRead(t, asyncContextReadFull(conn, 4), "SSE response after caller cancellation")
	require.NoError(t, read.err)
	require.Equal(t, "pong", string(read.data))
	select {
	case err := <-serverDone:
		require.NoError(t, err)
	case <-time.After(contextTestTimeout):
		t.Fatal("timed out waiting for server-side SSE exchange")
	}
}

func TestDialContextDoesNotOwnEstablishedWebSocket(t *testing.T) {
	srv := NewServer()
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	accepted := make(chan contextConnResult, 1)
	go func() {
		conn, err := srv.Accept()
		accepted <- contextConnResult{conn: conn, err: err}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	conn, err := Dial(ctx, ts.URL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	cancel()

	serverSide := waitForContextConn(t, accepted, "server to accept WebSocket connection")
	require.NoError(t, serverSide.err)
	t.Cleanup(func() { require.NoError(t, serverSide.conn.Close()) })
	require.NoError(t, serverSide.conn.SetWriteDeadline(time.Now().Add(contextTestTimeout)))
	_, err = serverSide.conn.Write([]byte("alive"))
	require.NoError(t, err)
	read := waitForContextRead(t, asyncContextReadFull(conn, 5), "WebSocket data after caller cancellation")
	require.NoError(t, read.err)
	require.Equal(t, "alive", string(read.data))
}

func TestSSECloseCancelsStreamRequest(t *testing.T) {
	streamCanceled := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: sid\ndata: context-test\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(streamCanceled)
	}))
	t.Cleanup(ts.Close)

	conn, err := dialSSE(context.Background(), ts.URL)
	require.NoError(t, err)
	read := asyncContextReadFull(conn, 1)
	require.NoError(t, conn.Close())
	waitForContextSignal(t, streamCanceled, "Close to cancel the SSE stream request")
	readAfterClose := waitForContextRead(t, read, "stream read to unblock after Close")
	require.Error(t, readAfterClose.err)
}

package webdial

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

const wsIOTestTimeout = 5 * time.Second

type wsTestPair struct {
	client net.Conn
	server net.Conn
}

func newWSTestPair(t *testing.T) wsTestPair {
	t.Helper()
	srv := NewServer()
	t.Cleanup(func() { srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := srv.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	client, err := Dial(context.Background(), ts.URL)
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })

	var server net.Conn
	select {
	case server = <-accepted:
	case <-time.After(wsIOTestTimeout):
		t.Fatal("timed out waiting for the server to accept")
	}
	t.Cleanup(func() { server.Close() })
	return wsTestPair{client: client, server: server}
}

// A fired read deadline means "not yet", not "never again". Delegating to
// gorilla made it fatal: its readErr latches, so every later Read returned the
// same stale timeout and the connection was silently dead.
func TestWSReadDeadlineIsRecoverable(t *testing.T) {
	for _, side := range []string{"server", "client"} {
		t.Run(side, func(t *testing.T) {
			pair := newWSTestPair(t)
			reader, writer := pair.server, pair.client
			if side == "client" {
				reader, writer = pair.client, pair.server
			}

			buf := make([]byte, 4096)
			require.NoError(t, reader.SetReadDeadline(time.Now().Add(50*time.Millisecond)))
			_, err := reader.Read(buf)
			requireTimeoutError(t, err)

			require.NoError(t, reader.SetReadDeadline(time.Time{}))
			_, err = writer.Write([]byte("after-deadline"))
			require.NoError(t, err)

			n, err := reader.Read(buf)
			require.NoError(t, err, "the connection must survive a fired read deadline")
			require.Equal(t, "after-deadline", string(buf[:n]))
		})
	}
}

// The idle-timeout loop every net.Conn consumer writes: read, time out, do
// housekeeping, extend the deadline, read again.
func TestWSReadDeadlineRepeatedTimeoutsThenData(t *testing.T) {
	pair := newWSTestPair(t)
	buf := make([]byte, 4096)
	for i := 0; i < 3; i++ {
		require.NoError(t, pair.server.SetReadDeadline(time.Now().Add(30*time.Millisecond)))
		_, err := pair.server.Read(buf)
		requireTimeoutError(t, err)
	}
	require.NoError(t, pair.server.SetReadDeadline(time.Now().Add(wsIOTestTimeout)))
	_, err := pair.client.Write([]byte("still here"))
	require.NoError(t, err)
	n, err := pair.server.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "still here", string(buf[:n]))
}

// The acid test for the pump owning the buffer: a deadline that fires partway
// through a message must not lose the bytes already read, nor kill the
// connection. The peer opens a writer, flushes a non-final frame, and stalls.
func TestWSReadDeadlineDuringPartialMessage(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ws, _, err := websocket.DefaultDialer.Dial(strings.Replace(ts.URL, "http://", "ws://", 1), nil)
	require.NoError(t, err)
	defer ws.Close()
	server, err := srv.Accept()
	require.NoError(t, err)
	defer server.Close()

	const size = 20 << 10
	payload := bytes.Repeat([]byte("m"), size)
	w, err := ws.NextWriter(websocket.BinaryMessage)
	require.NoError(t, err)
	// Well over twice gorilla's write buffer, so this leaves the socket as a
	// non-final frame while the message stays open.
	_, err = w.Write(payload)
	require.NoError(t, err)

	buf := make([]byte, size)
	require.NoError(t, server.SetReadDeadline(time.Now().Add(150*time.Millisecond)))
	_, err = server.Read(buf)
	requireTimeoutError(t, err)

	// The connection must still be alive, and the message intact.
	require.NoError(t, w.Close())
	require.NoError(t, server.SetReadDeadline(time.Now().Add(wsIOTestTimeout)))
	n, err := server.Read(buf)
	require.NoError(t, err, "a mid-message timeout must not kill the connection")
	require.Equal(t, size, n, "no byte of the message may be lost to the timeout")
	require.True(t, bytes.Equal(payload, buf[:n]))
}

// The browser closes a socket that goes 15s without a pong. Answering pings
// only from inside Read meant a server busy elsewhere was torn down by its own
// client; the pump answers them regardless.
func TestWSControlFrameAnsweredWithoutRead(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ws, _, err := websocket.DefaultDialer.Dial(strings.Replace(ts.URL, "http://", "ws://", 1), nil)
	require.NoError(t, err)
	defer ws.Close()
	server, err := srv.Accept()
	require.NoError(t, err)
	defer server.Close()
	// Deliberately never call server.Read.

	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte("ping:42")))
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(wsIOTestTimeout)))
	mt, data, err := ws.ReadMessage()
	require.NoError(t, err, "a ping must be answered while no one is in Read")
	require.Equal(t, websocket.TextMessage, mt)
	require.Equal(t, "pong:42", string(data))
}

// A read timeout must satisfy errors.Is(err, os.ErrDeadlineExceeded), like
// every other transport in this package. Gorilla's opaque netError does not.
func TestWSReadDeadlineErrorIsDeadlineExceeded(t *testing.T) {
	pair := newWSTestPair(t)
	require.NoError(t, pair.server.SetReadDeadline(time.Now().Add(30*time.Millisecond)))
	_, err := pair.server.Read(make([]byte, 16))
	requireTimeoutError(t, err)
}

func TestWSReadAfterCloseReturnsErrClosed(t *testing.T) {
	pair := newWSTestPair(t)
	require.NoError(t, pair.server.Close())
	// Deterministic regardless of how the pump's own teardown races: Close
	// records the terminal error before it closes the socket.
	for i := 0; i < 20; i++ {
		_, err := pair.server.Read(make([]byte, 16))
		require.ErrorIs(t, err, net.ErrClosed)
		time.Sleep(time.Millisecond)
	}
	// Close is idempotent and, like the SSE conn, always reports success.
	require.NoError(t, pair.server.Close())
}

func TestWSZeroLengthBufferDoesNotBlock(t *testing.T) {
	pair := newWSTestPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		n, err := pair.server.Read(nil)
		require.NoError(t, err)
		require.Zero(t, n)
	}()
	select {
	case <-done:
	case <-time.After(wsIOTestTimeout):
		t.Fatal("Read with an empty buffer must not block")
	}
}

// Two goroutines per conn, and both must be reaped by Close.
func TestWSCloseReapsGoroutines(t *testing.T) {
	settle := func() int {
		var n int
		for i := 0; i < 50; i++ {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
			n = runtime.NumGoroutine()
		}
		return n
	}
	before := settle()
	srv := NewServer()
	ts := httptest.NewServer(srv)
	conns := make([]net.Conn, 0, 8)
	for i := 0; i < 8; i++ {
		client, err := Dial(context.Background(), ts.URL)
		require.NoError(t, err)
		server, err := srv.Accept()
		require.NoError(t, err)
		conns = append(conns, client, server)
	}
	for _, conn := range conns {
		require.NoError(t, conn.Close())
	}
	srv.Close()
	ts.Close()
	require.LessOrEqual(t, settle(), before+4, "Close must reap the pump and control loop")
}

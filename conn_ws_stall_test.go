package webdial

import (
	"crypto/rand"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// stalledWriter returns a client whose server-side conn is guaranteed to be
// blocked mid-write: the client never reads, so the socket buffers fill and
// gorilla's WriteMessage parks. Payloads are random so per-message deflate
// cannot shrink them away. Anything the client sends arrives on inbound.
func stalledWriter(t *testing.T, srv *Server) (ws *websocket.Conn, inbound chan []byte) {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { ws.Close() })
	conn, err := srv.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	inbound = make(chan []byte, 16)
	var written atomic.Int64
	chunk := make([]byte, 64*1024)
	rand.Read(chunk)
	go func() {
		for {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
			written.Add(1)
		}
	}()
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				select {
				case inbound <- append([]byte(nil), buf[:n]...):
				default:
				}
			}
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		before := written.Load()
		time.Sleep(200 * time.Millisecond)
		if written.Load() == before {
			return ws, inbound
		}
		require.True(t, time.Now().Before(deadline), "server writes never stalled")
	}
}

// A blocked outbound write must not stall the read path. The peer's own
// heartbeat is the trigger: answering it inline used to take the write lock the
// stalled writer already held, wedging every subsequent inbound frame.
func TestWSReadSurvivesStalledWrite(t *testing.T) {
	srv := NewServer()
	srv.WriteTimeout = -1
	defer srv.Close()
	ws, inbound := stalledWriter(t, srv)
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte("ping:1")))
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("keystroke")))
	select {
	case got := <-inbound:
		require.Equal(t, "keystroke", string(got))
	case <-time.After(5 * time.Second):
		t.Fatal("inbound data never reached the reader while a write was stalled")
	}
}

// A peer that stops reading must not pin the connection forever.
func TestWSWriteTimeoutUnblocks(t *testing.T) {
	srv := NewServer()
	srv.WriteTimeout = 300 * time.Millisecond
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer ws.Close()
	conn, err := srv.Accept()
	require.NoError(t, err)
	defer conn.Close()
	chunk := make([]byte, 64*1024)
	rand.Read(chunk)
	errCh := make(chan error, 1)
	go func() {
		for {
			if _, err := conn.Write(chunk); err != nil {
				errCh <- err
				return
			}
		}
	}()
	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("write never timed out against a peer that stopped reading")
	}
}

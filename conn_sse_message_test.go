package webdial

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The SSE fallback has to keep the same promise the websocket transport makes:
// one Write is one message, and one Read yields it whole. It used to read the
// POST body out in 32KB slices and deliver each as its own message, so any
// framing layered on top — a type byte at the front, a length prefix — was
// silently broken past that size: every piece after the first began with
// payload where the header belonged.
func TestSSEReadPreservesMessageBoundaries(t *testing.T) {
	for _, size := range []int{1024, 32 << 10, (32 << 10) + 1, 100 << 10, 512 << 10} {
		t.Run(byteLabel(size), func(t *testing.T) {
			srv := NewServer()
			defer srv.Close()
			ts := httptest.NewServer(srv)
			defer ts.Close()
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte('a' + i%26)
			}
			got := make(chan []byte, 1)
			go func() {
				conn, err := srv.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 1<<20)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				got <- append([]byte(nil), buf[:n]...)
			}()
			conn, err := dialSSE(context.Background(), ts.URL)
			require.NoError(t, err)
			defer conn.Close()
			// Written from its own goroutine so a split body fails this test
			// rather than hanging it: the reader above takes exactly one
			// message, so a second chunk would block the POST forever.
			wrote := make(chan error, 1)
			go func() {
				_, err := conn.Write(payload)
				wrote <- err
			}()
			select {
			case b := <-got:
				require.Equal(t, size, len(b), "one Read must yield the whole message")
				require.True(t, bytes.Equal(payload, b))
			case <-time.After(5 * time.Second):
				t.Fatal("timeout waiting for the message")
			}
			select {
			case err := <-wrote:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("write never completed, so the body was delivered as more than one message")
			}
		})
	}
}

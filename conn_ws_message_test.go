package webdial

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// A webdial conn carries discrete messages, and every consumer that reads one
// (a length-prefixed frame, a JSON object) needs a Read to yield exactly one.
// Gorilla's message reader is a stream, so without reassembly a message larger
// than its internal read buffer arrives split across Reads and any consumer
// parsing per-Read silently drops it.
func TestWSReadPreservesMessageBoundaries(t *testing.T) {
	for _, size := range []int{1024, 4096, 4097, 64 << 10, 512 << 10} {
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
			conn, err := Dial(context.Background(), ts.URL)
			require.NoError(t, err)
			defer conn.Close()
			_, err = conn.Write(payload)
			require.NoError(t, err)
			select {
			case b := <-got:
				require.Equal(t, size, len(b), "one Read must yield the whole message")
				require.True(t, bytes.Equal(payload, b))
			case <-t.Context().Done():
				t.Fatal("timeout")
			}
		})
	}
}

func byteLabel(n int) string {
	switch {
	case n >= 1<<20:
		return string(rune('0'+n/(1<<20))) + "MB"
	case n >= 1<<10:
		return itoa(n/(1<<10)) + "KB"
	default:
		return itoa(n) + "B"
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A message larger than the caller's buffer still streams across Reads, so a
// consumer treating the conn as a byte stream keeps working. Consumers that
// need whole messages size their buffer to their maximum frame.
func TestWSReadOversizedMessageStreams(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	const size = 40 << 10
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
		var acc []byte
		buf := make([]byte, 8<<10)
		for len(acc) < size {
			n, err := conn.Read(buf)
			acc = append(acc, buf[:n]...)
			if err != nil {
				break
			}
		}
		got <- acc
	}()
	conn, err := Dial(context.Background(), ts.URL)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write(payload)
	require.NoError(t, err)
	select {
	case b := <-got:
		require.True(t, bytes.Equal(payload, b), "every byte must arrive across Reads")
	case <-t.Context().Done():
		t.Fatal("timeout")
	}
}

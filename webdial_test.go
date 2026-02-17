package webdial

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWSTransport(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	go func() {
		conn, err := srv.Accept()
		require.NoError(t, err)
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		require.NoError(t, err)
		require.Equal(t, "hello", string(buf[:n]))
		_, err = conn.Write([]byte("world"))
		require.NoError(t, err)
	}()
	conn, err := Dial(context.Background(), ts.URL)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "world", string(buf[:n]))
	require.NotNil(t, conn.LocalAddr())
}

func TestSSETransport(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	mux := http.NewServeMux()
	mux.Handle("/sse", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handler().ServeHTTP(w, r)
	}))
	mux.Handle("/post", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handler().ServeHTTP(w, r)
	}))
	ts := httptest.NewServer(mux)
	defer ts.Close()
	go func() {
		conn, err := srv.Accept()
		require.NoError(t, err)
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		require.NoError(t, err)
		require.Equal(t, "ping", string(buf[:n]))
		_, err = conn.Write([]byte("pong"))
		require.NoError(t, err)
	}()
	conn, err := dialSSE(context.Background(), ts.URL)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "pong", string(buf[:n]))
	require.Equal(t, "webdial-sse", conn.LocalAddr().Network())
}

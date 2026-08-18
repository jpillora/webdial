package webdial

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebsocketURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "root endpoint",
			in:   "http://example.com",
			want: "ws://example.com",
		},
		{
			name: "nested path and trailing slash",
			in:   "http://example.com/one/two/",
			want: "ws://example.com/one/two/",
		},
		{
			name: "existing query",
			in:   "http://example.com/wd/?mode=fast",
			want: "ws://example.com/wd/?mode=fast",
		},
		{
			name: "encoded query value",
			in:   "http://example.com/wd/?next=https%3A%2F%2Fupstream.example%2Fa%2Fb",
			want: "ws://example.com/wd/?next=https%3A%2F%2Fupstream.example%2Fa%2Fb",
		},
		{
			name: "literal URL in query",
			in:   "http://example.com/wd/?next=https://upstream.example/a/",
			want: "ws://example.com/wd/?next=https://upstream.example/a/",
		},
		{
			name: "HTTPS with HTTP query value",
			in:   "https://example.com/wd/?next=http://upstream.example/a/",
			want: "wss://example.com/wd/?next=http://upstream.example/a/",
		},
		{
			name: "IPv6 host",
			in:   "http://[2001:db8::1]:8080/wd/",
			want: "ws://[2001:db8::1]:8080/wd/",
		},
		{
			name: "fragment",
			in:   "http://example.com/wd/?x=1#next=https://fragment.example/",
			want: "ws://example.com/wd/?x=1#next=https://fragment.example/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := websocketURL(tt.in)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestAppendURLQueryParam(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "root endpoint",
			in:   "http://example.com",
			want: "http://example.com?s=session%2Fid",
		},
		{
			name: "trailing slash",
			in:   "http://example.com/wd/",
			want: "http://example.com/wd/?s=session%2Fid",
		},
		{
			name: "existing raw query and fragment",
			in:   "http://example.com/wd/?next=https://upstream.example/a%2Fb&token=a%20b#section",
			want: "http://example.com/wd/?next=https://upstream.example/a%2Fb&token=a%20b&s=session%2Fid#section",
		},
		{
			name: "IPv6 HTTPS endpoint",
			in:   "https://[2001:db8::1]:8443/wd/?next=http%3A%2F%2Fupstream.example#section",
			want: "https://[2001:db8::1]:8443/wd/?next=http%3A%2F%2Fupstream.example&s=session%2Fid#section",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := appendURLQueryParam(tt.in, "s", "session/id")
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestDialPreservesMountedPath(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	mux := http.NewServeMux()
	mux.Handle("/wd/", srv)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	go func() {
		for {
			conn, err := srv.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(conn)
		}
	}()

	endpoint := ts.URL + "/wd/?next=https://upstream.example/a/"
	tests := []struct {
		name   string
		dial   func(context.Context, string) (net.Conn, error)
		wantWS bool
	}{
		{name: "forced WebSocket", dial: dialWS, wantWS: true},
		{name: "forced SSE", dial: dialSSE},
		{name: "automatic prefers WebSocket", dial: Dial, wantWS: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := tt.dial(context.Background(), endpoint)
			require.NoError(t, err)
			defer conn.Close()
			if tt.wantWS {
				require.IsType(t, &wsConn{}, conn)
			}

			message := []byte(tt.name)
			_, err = conn.Write(message)
			require.NoError(t, err)
			got := make([]byte, len(message))
			_, err = io.ReadFull(conn, got)
			require.NoError(t, err)
			require.Equal(t, message, got)
		})
	}
}

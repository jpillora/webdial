package webdial

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/jpillora/eventsource"
	"github.com/stretchr/testify/require"
)

func TestWSTransport(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	ts := httptest.NewServer(srv)
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

func TestWebSocketDefaultOriginPolicy(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1)

	tests := []struct {
		name       string
		origin     string
		wantStatus int
	}{
		{
			name:       "same origin",
			origin:     ts.URL,
			wantStatus: http.StatusSwitchingProtocols,
		},
		{
			name:       "foreign origin",
			origin:     "https://attacker.example",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "absent origin",
			wantStatus: http.StatusSwitchingProtocols,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{}
			if tt.origin != "" {
				header.Set("Origin", tt.origin)
			}
			ws, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
			require.NotNil(t, resp)
			defer resp.Body.Close()
			require.Equal(t, tt.wantStatus, resp.StatusCode)

			if tt.wantStatus != http.StatusSwitchingProtocols {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			defer ws.Close()
			accepted, err := srv.Accept()
			require.NoError(t, err)
			require.NoError(t, accepted.Close())
		})
	}
}

func TestWebSocketOriginPolicyOverride(t *testing.T) {
	const allowedOrigin = "https://app.example.com"
	srv := NewServer()
	srv.CheckOrigin = func(r *http.Request) bool {
		return r.Header.Get("Origin") == allowedOrigin
	}
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1)

	header := http.Header{"Origin": []string{allowedOrigin}}
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Body.Close()
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	defer ws.Close()
	accepted, err := srv.Accept()
	require.NoError(t, err)
	require.NoError(t, accepted.Close())
}

func TestSSETransport(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	ts := httptest.NewServer(srv)
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

func TestWSPingPong(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	go func() {
		conn, err := srv.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 1024)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer ws.Close()
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte("ping:12345")))
	mt, msg, err := ws.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, mt)
	require.Equal(t, "pong:12345", string(msg))
}

func TestSSEPingPong(t *testing.T) {
	srv := NewServer()
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	dec := eventsource.NewDecoder(resp.Body)
	var ev eventsource.Event
	require.NoError(t, dec.Decode(&ev))
	require.Equal(t, "sid", ev.Type)
	sid := string(ev.Data)
	pres, err := http.Post(ts.URL+"?s="+sid+"&ping=12345", "", nil)
	require.NoError(t, err)
	pres.Body.Close()
	require.Equal(t, http.StatusNoContent, pres.StatusCode)
	require.NoError(t, dec.Decode(&ev))
	require.Equal(t, "pong", ev.Type)
	require.Equal(t, "12345", string(ev.Data))
}

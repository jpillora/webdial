# webdial

**A real `net.Conn`, tunnelled over whatever the network lets through.**

You get a plain byte stream in Go and an equivalent read/write/close interface
in JavaScript. Underneath, webdial uses WebSocket where it can, silently falls
back to SSE+POST where WebSocket is blocked, and can add WebTransport (HTTP/3)
when you want it. Your code never changes.

![screenshot](screenshot.png)

## Why

Streaming to a browser usually means picking a transport and then rewriting your
app around it. WebSocket is the obvious choice right up until a corporate proxy,
a TLS-inspecting middlebox, or an old load balancer eats the upgrade — and then
you are writing a second implementation over long-polling.

webdial makes the transport an implementation detail:

- **It is a `net.Conn`.** Not a message bus, not a framing protocol. Anything
  stream-oriented works over it — `bufio.Scanner`, `encoding/gob`, `net/http`,
  gRPC, SSH, your own protocol.
- **It falls back on its own.** WebSocket first, SSE+POST when that fails. The
  fallback needs nothing more exotic than HTTP/1.1, so it survives the networks
  that break everything else.
- **One handler, one accept loop.** The server is an `http.Handler` you mount
  anywhere; connections arrive from `srv.Accept()` no matter how they got in.
- **Both ends, no dependencies.** A Go client, and a zero-dependency ESM client
  for browsers and Node 22+.
- **Grown-up connection semantics.** Read/write deadlines, back-pressure,
  keep-alives, and a watchdog that notices a peer that silently disappeared.
- **WebTransport is opt-in.** Import the subpackage and you get HTTP/3; skip it
  and quic-go is never linked into your binary.

Endpoint paths are used exactly as supplied. Include the trailing slash when
mounting on a subtree such as `/wd/`; existing query parameters are preserved.

## Install

```
go get github.com/jpillora/webdial
```

```
npm install webdial
```

Or load the ESM client straight from a `<script type="module">`:

```html
<script type="module">
  import { dial } from "/client.mjs";
</script>
```

---

## 1. Minimal — an echo server

The whole server is a handler plus an accept loop.

```go
package main

import (
	"io"
	"net/http"

	"github.com/jpillora/webdial"
)

func main() {
	srv := webdial.NewServer()
	http.Handle("/wd/", srv)
	go http.ListenAndServe(":8080", nil)

	for {
		conn, err := srv.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			io.Copy(conn, conn) // echo
		}()
	}
}
```

`srv.Accept()` returns a `net.Conn`. That is the entire server-side API.

From the browser or Node:

```js
import { dial } from "webdial";

const conn = await dial("http://localhost:8080/wd/");

await conn.write("hello");
const data = await conn.read(); // Uint8Array, or null at EOF
console.log(new TextDecoder().decode(data)); // "hello"

await conn.close();
```

Or from Go:

```go
conn, err := webdial.Dial(ctx, "http://localhost:8080/wd/")
if err != nil {
	log.Fatal(err)
}
defer conn.Close()

conn.Write([]byte("hello"))

buf := make([]byte, 1024)
n, _ := conn.Read(buf)
fmt.Println(string(buf[:n])) // "hello"
```

`Dial` tries WebSocket, then SSE+POST. As with `net.Dialer.DialContext`, `ctx`
governs establishment only — cancelling it later does not close the connection.

---

## 2. Medium — a broadcast chat with real framing

A `net.Conn` is a byte stream, so give it a real protocol. Here that is just
newline-delimited text, read with `bufio.Scanner`.

```go
// hub fans every inbound line out to all the other connections.
type hub struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (h *hub) add(c net.Conn)    { h.mu.Lock(); h.conns[c] = struct{}{}; h.mu.Unlock() }
func (h *hub) remove(c net.Conn) { h.mu.Lock(); delete(h.conns, c); h.mu.Unlock() }

func (h *hub) broadcast(from net.Conn, line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		if c == from {
			continue
		}
		// A peer that stopped reading must not stall the broadcast.
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintln(c, line)
	}
}
```

Wire it up, with a few server options worth knowing about:

```go
srv := webdial.NewServer()
srv.MaxPostBytes = 64 << 10 // cap each SSE write; oversized bodies get HTTP 413
srv.CheckOrigin = func(r *http.Request) bool {
	return r.Header.Get("Origin") == "https://app.example.com"
}

mux := http.NewServeMux()
mux.Handle("/chat/", srv) // mount anywhere; the path is preserved verbatim
go http.ListenAndServe(":8080", mux)

h := &hub{conns: map[net.Conn]struct{}{}}
for {
	conn, err := srv.Accept()
	if err != nil {
		return
	}
	go func() {
		defer conn.Close()
		h.add(conn)
		defer h.remove(conn)

		scan := bufio.NewScanner(conn)
		for scan.Scan() {
			h.broadcast(conn, scan.Text())
		}
	}()
}
```

On the client, read until EOF. `read()` resolves `null` when the peer closes,
which is the idiomatic loop terminator:

```js
const conn = await dial("https://example.com/chat/");

(async () => {
  while (true) {
    const data = await conn.read();
    if (data === null) break; // peer closed
    append(new TextDecoder().decode(data));
  }
})();

await conn.write("hello everyone\n");
```

### Origin policy

WebSocket handshakes use a secure same-origin default: browser requests whose
`Origin` host does not match `Host` are rejected, while non-browser clients that
omit `Origin` (including both webdial clients) are accepted. Set `CheckOrigin`
for an explicit cross-origin allowlist, as above. Avoid a blanket `return true`
— browsers do not apply CORS protections to WebSocket handshakes, so pair
cross-origin access with real authentication.

---

## 3. Complex — run any `net` server over it

This is the payoff of being a `net.Conn`. Adapt the server to `net.Listener` and
every listener-shaped thing in the ecosystem works unmodified — here, an entire
`net/http` server tunnelled over WebSocket-over-HTTP.

```go
// listener adapts the webdial server to net.Listener.
type listener struct{ *webdial.Server }

func (listener) Addr() net.Addr { return &net.TCPAddr{} }
```

That is the whole adapter — `*webdial.Server` already has `Accept` and `Close`.

```go
srv := webdial.NewServer()
mux := http.NewServeMux()
mux.Handle("/wd/", srv)
go http.ListenAndServe(":8080", mux)

// A completely ordinary HTTP server, speaking over webdial connections.
api := http.NewServeMux()
api.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"proto": r.Proto,
		"path":  r.URL.Path,
	})
})
go http.Serve(listener{srv}, api)
```

The client side is an ordinary `http.Client` whose connections happen to be
webdial conns:

```go
client := &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return webdial.Dial(ctx, "http://localhost:8080/wd/")
		},
	},
}

resp, err := client.Get("http://tunnel/whoami")
// 200 {"path":"/whoami","proto":"HTTP/1.1"}
```

You now have HTTP/1.1 running inside a WebSocket, with SSE+POST fallback, and
neither the API handler nor the client knows. Swap `http.Serve` for a gRPC
server, an SSH server, or `yamux` and the shape is identical.

---

## 4. Optional — add WebTransport (HTTP/3)

WebTransport lives in a subpackage, so programs that do not import it never link
quic-go:

```
go get github.com/jpillora/webdial/wt
```

HTTP/3 runs over UDP, so it needs its own listener. Point it at the same core
server and **the accept loop does not change** — connections arrive from
`srv.Accept()` exactly as before:

```go
srv := webdial.NewServer()

mux := http.NewServeMux()
mux.Handle("/wd/", srv)
go http.ListenAndServe(":8080", mux) // ws + sse

// --- the only WebTransport-specific wiring ---
wts := wt.NewServer(srv)
wts.Addr = ":8443"
wts.TLSConfig = myTLSConfig // WebTransport is HTTPS-only
h3 := http.NewServeMux()
h3.Handle("/wd/", wts)
wts.Handler = h3
go wts.ListenAndServe()
// --- everything below is unchanged ---

for {
	conn, err := srv.Accept()
	// ...
}
```

`wt.Server` answers WebTransport itself and delegates every other request to the
core server, so the same endpoint also serves the SSE fallback over HTTP/3. It
honours `CheckOrigin` with the same secure default as WebSocket, and inherits
the core server's keep-alive interval as the QUIC keep-alive period.

From Go, WebTransport has its own entry point, because the root package does not
import quic-go:

```go
conn, err := wt.Dial(ctx, "https://localhost:8443/wd/")
```

Use `wt.Dialer` to supply a `TLSConfig` — for example to trust a development
certificate. Note the HTTP/3 endpoint is usually on a different port from the
HTTP one, even when the path is identical.

From JavaScript it is opt-in, and composes with the fallback chain:

```js
// try WebTransport, then WebSocket, then SSE
const conn = await dial(url, { transport: ["wt", "ws", "sse"] });
```

Because the HTTP/3 listener is usually on another port, `wtURL` overrides the
endpoint while `conn.url` keeps reporting the URL you passed. `wt` is handed
straight to the `WebTransport` constructor:

```js
const conn = await dial("https://example.com/wd/", {
  transport: "wt",
  wtURL: "https://example.com:8443/wd/",
  wt: { serverCertificateHashes: [{ algorithm: "sha-256", value: hashBytes }] },
});
```

### Why it is opt-in

WebTransport is never tried automatically. It needs a separate HTTP/3 listener,
so probing it would cost every dial a failed connection attempt wherever it is
not deployed — and in exactly the restrictive networks this library exists for,
UDP is blocked and that failure only arrives after a QUIC handshake timeout.
Naming it explicitly keeps the default path fast.

Browser support is Chromium-based browsers and Firefox. Where the API is
missing, `dial` reports `webdial: WebTransport is not supported`, so a transport
list simply falls through to the next entry. `serverCertificateHashes` — used to
pin a self-signed development certificate — is Chromium-only and requires an
ECDSA P-256 certificate valid for no more than two weeks.
`testdata/devserver` mints one and publishes its hash at `/wt.json`; run it and
open `example/` to try the transports in a browser.

---

## Transports

| Transport | Mechanism | Binary | Requirements |
|-----------|-----------|--------|--------------|
| `ws` | WebSocket | native | WebSocket support |
| `sse` | Server-Sent Events (read) + POST (write) | base64 | HTTP/1.1+ |
| `wt` | WebTransport bidirectional stream (HTTP/3) | native | HTTP/3 listener, HTTPS |

WebSocket is preferred. SSE+POST is the fallback for environments where
WebSocket is blocked. WebTransport is opt-in on both clients.

WebTransport also applies genuine back-pressure: when unread data reaches
`maxBufferedBytes` the client stops reading from the stream, slowing the sender
through QUIC flow control instead of failing the connection the way SSE must.

## API

### Go — `webdial`

```go
func NewServer() *Server
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)
func (s *Server) Accept() (net.Conn, error)
func (s *Server) Close() error
func (s *Server) Push(ctx context.Context, conn net.Conn) error // feed in an external transport
func (s *Server) KeepAliveInterval() time.Duration

func Dial(ctx context.Context, baseURL string) (net.Conn, error)
```

Server options: `KeepAlive` (0 → 25s, negative disables), `CheckOrigin`,
`MaxPostBytes` (0 → 1 MiB, negative disables), `CompressionLevel`,
`WriteTimeout` (0 → 30s, negative unbounded).

### Go — `webdial/wt`

```go
func NewServer(core *webdial.Server) *Server
func (s *Server) ListenAndServe() error
func (s *Server) Serve(conn net.PacketConn) error     // bring your own UDP socket
func (s *Server) ServeQUICConn(conn *quic.Conn) error // bring your own QUIC listener
func (s *Server) Close() error

func Dial(ctx context.Context, baseURL string) (net.Conn, error)
func (d *Dialer) Dial(ctx context.Context, baseURL string) (net.Conn, error)
```

### JavaScript

```js
const conn = await dial(url, opts);

await conn.write(dataOrString); // Uint8Array | string
const data = await conn.read(); // Uint8Array, or null at EOF
await conn.close();             // idempotent
conn.ping();                    // probe now and restart the staleness window

conn.transport; // "ws" | "sse" | "wt"
conn.url;       // the base URL exactly as supplied
conn.latency;   // ms from the last pong, or null
conn.onLatency = (ms) => {};
```

Options: `transport` (a name or an ordered list; unknown names throw a
`TypeError`), `pingIntervalMs`, `pongTimeoutMs`, `maxBufferedBytes`, plus
`wtURL` and `wt` for WebTransport.

The SSE transport decodes control events in the background, so keep-alives and
remote closes are handled even while the application is not calling `read()`.
Buffered data is bounded to 1 MiB and 1,024 events by default; exceeding either
closes the connection rather than silently dropping data.

## Protocol

The server is a single `http.Handler` that routes by content negotiation:

- `Upgrade: websocket` — WebSocket upgrade; binary frames carry data, text
  frames carry `ping:<ts>`/`pong:<ts>`
- `GET` with `Accept: text/event-stream` — SSE stream; the first event is `sid`
  (session ID), `d` events carry base64 data, `close` signals shutdown
- `POST` with `?s=<sid>` — write body bytes to the session; append `&close=1` to
  close, or `&ping=<ts>` to probe

WebTransport is served from a separate HTTP/3 listener on the same path:

- HTTP/3 extended `CONNECT` establishes the session; routing keys on the method
  alone, since the protocol token differs between drafts
- one client-initiated bidirectional stream carries the byte stream, with no
  framing and no base64
- datagrams carry `ping:<ts>`/`pong:<ts>`, the same control vocabulary the
  WebSocket transport puts in text frames
- QUIC PING frames provide the keep-alive, so there is no webdial-level server
  heartbeat, matching WebSocket

## License

MIT

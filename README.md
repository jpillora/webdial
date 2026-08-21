# webdial

`net.Conn` over HTTP. Uses WebSocket when available, falls back to SSE+POST.
WebTransport (HTTP/3) is available opt-in.

![screenshot](screenshot.png)

Both sides get a standard `net.Conn` (Go) or an equivalent read/write/close interface (JavaScript), so any stream-oriented protocol works over it.

Endpoint paths are used exactly as supplied. Include the trailing slash when
the handler is mounted on a subtree such as `/wd/`; existing query parameters
are preserved.

## Go

### Install

```
go get github.com/jpillora/webdial
```

### Server

`*Server` implements `http.Handler`, so mount it directly:

```go
srv := webdial.NewServer()

mux := http.NewServeMux()
mux.Handle("/wd/", srv)

go http.ListenAndServe(":8080", mux)

for {
    conn, err := srv.Accept()
    if err != nil {
        break
    }
    go func() {
        defer conn.Close()
        io.Copy(conn, conn) // echo
    }()
}
```

`srv.Accept()` returns a `net.Conn`. Use it with any protocol that works over a byte stream.

SSE data POSTs are limited to 1 MiB each by default. Configure a different
limit when constructing the server (a negative value explicitly disables it):

```go
srv.MaxPostBytes = 4 << 20 // 4 MiB
```

Oversized bodies receive HTTP 413, which both clients return as a write error.
Successful POST bodies for one SSE connection are delivered contiguously and
one at a time. When clients issue concurrent writes, the body that acquires the
server first is delivered first; callers that require a specific order should
await each write's successful response before starting the next.

#### WebSocket origin policy

WebSocket handshakes use a secure same-origin policy by default. Browser requests
whose `Origin` host does not match the request `Host` are rejected; non-browser
clients that omit `Origin`, including the Go and Node.js clients, are accepted.

If a trusted web application is hosted on a different origin, configure an
explicit allowlist on that server:

```go
srv.CheckOrigin = func(r *http.Request) bool {
    switch r.Header.Get("Origin") {
    case "https://app.example.com", "https://admin.example.com":
        return true
    default:
        return false
    }
}
```

Cross-origin WebSockets should also be protected with explicit authentication.
Avoid a blanket `return true`: browsers do not apply CORS protections to
WebSocket handshakes.

#### WebTransport (HTTP/3)

WebTransport lives in a subpackage so that programs using only WebSocket and
SSE do not link quic-go:

```
go get github.com/jpillora/webdial/wt
```

Importing only `webdial` links no quic-go code and adds nothing to your
`go.sum`; quic-go appears in the module graph, so manifest-based scanners may
list it. Adding WebTransport costs roughly 2 MB of stripped binary.

It needs its own listener, because HTTP/3 runs over UDP. Point it at the same
core server and one `Accept` loop serves every transport:

```go
srv := webdial.NewServer()

mux := http.NewServeMux()
mux.Handle("/wd/", srv)
go http.ListenAndServe(":8080", mux) // ws + sse

wts := wt.NewServer(srv)
wts.Addr = ":8443"
wts.TLSConfig = myTLSConfig
h3 := http.NewServeMux()
h3.Handle("/wd/", wts)
wts.Handler = h3
go wts.ListenAndServe() // webtransport, same Accept loop
```

`wt.Server` answers WebTransport itself and delegates every other request to
the core server, so the same endpoint also serves the SSE fallback over
HTTP/3. It honours `CheckOrigin` with the same secure default as WebSocket,
and inherits the core server's keep-alive interval as the QUIC keep-alive
period.

Because a WebTransport session lives only as long as its CONNECT stream, the
handler blocks until the session ends; this is handled internally.

### Client

```go
conn, err := webdial.Dial(ctx, "http://localhost:8080/wd/")
if err != nil {
    log.Fatal(err)
}
defer conn.Close()

conn.Write([]byte("hello"))

buf := make([]byte, 1024)
n, err := conn.Read(buf)
fmt.Println(string(buf[:n])) // "hello"
```

`Dial` tries WebSocket first and falls back to SSE+POST automatically. The returned `net.Conn` works the same regardless of transport.

#### Deadlines

Read deadlines behave as `net.Conn` requires on every transport: a fired one
ends that `Read` with an error satisfying `errors.Is(err, os.ErrDeadlineExceeded)`
and leaves the connection usable, so the usual idle-timeout loop works.

```go
for {
    conn.SetReadDeadline(time.Now().Add(30 * time.Second))
    n, err := conn.Read(buf)
    if errors.Is(err, os.ErrDeadlineExceeded) {
        continue // idle, not broken
    }
    ...
}
```

On WebSocket, where a `Read` returns a whole message when it fits, a read
deadline can expire after part of a message has arrived; no byte is lost, and
the next `Read` yields the complete message.

A fired *write* deadline on WebSocket is terminal, deliberately: the timed-out
write has already put a partial frame on the wire, leaving the peer's parser
desynchronised, so there is no resume point.

WebTransport is reached through its own entry point, since the root package
does not import quic-go:

```go
conn, err := wt.Dial(ctx, "https://localhost:8443/wd/")
```

Use `wt.Dialer` to supply a `TLSConfig` — for example to trust a development
certificate. Note the HTTP/3 endpoint is usually on a different port from the
HTTP one, even when the path is identical.

As with `net.Dialer.DialContext`, `ctx` controls connection establishment only. Canceling it after `Dial` returns does not close the established connection; call `conn.Close()` to end the connection.

## JavaScript

The ESM client (`client.mjs`) works in both browsers and Node.js 22+. Zero dependencies.

### Install

```
npm install webdial
```

Or use it directly from a `<script type="module">`:

```html
<script type="module">
import { dial } from "/path/to/client.mjs";
</script>
```

### Usage

```js
import { dial } from "webdial";

const conn = await dial("http://localhost:8080/wd/");

// Send text
await conn.write("hello");

// Send binary
await conn.write(new Uint8Array([1, 2, 3]));

// Read (returns Uint8Array, or null on close)
const data = await conn.read();
console.log(new TextDecoder().decode(data));

// Close
await conn.close();
```

### Options

Force a specific transport, or give an ordered list to try in turn:

```js
const conn = await dial(url, { transport: "ws" });  // WebSocket only
const conn = await dial(url, { transport: "sse" }); // SSE+POST only
const conn = await dial(url, { transport: ["wt", "ws", "sse"] });
```

By default, `dial` tries WebSocket first and falls back to SSE+POST.
An unrecognised transport name throws a `TypeError`.

WebTransport is opt-in and never tried automatically: it needs an HTTP/3
listener on a separate UDP port, so probing it would cost every dial a failed
connection attempt wherever it is not deployed — and in the restrictive
networks this library exists for, that failure only arrives after a QUIC
handshake timeout.

Because the HTTP/3 listener is usually on another port, `wtURL` overrides the
endpoint while `conn.url` keeps reporting the URL you passed. `wt` is handed
to the `WebTransport` constructor:

```js
const conn = await dial("https://example.com/wd/", {
  transport: "wt",
  wtURL: "https://example.com:8443/wd/",
  wt: { serverCertificateHashes: [{ algorithm: "sha-256", value: hashBytes }] },
});
```

WebTransport applies real back-pressure: when unread data reaches
`maxBufferedBytes` the client stops reading from the stream, which slows the
sender through QUIC flow control instead of failing the connection the way SSE
must.

Browser support is Chromium-based browsers and Firefox; `dial` reports
`webdial: WebTransport is not supported` where the API is missing, so a
transport list falls through to the next entry. `serverCertificateHashes`,
used to pin a self-signed development certificate, is Chromium-only and
requires an ECDSA P-256 certificate valid for no more than two weeks — see
`testdata/devserver` for a working example.

The SSE transport decodes control events in the background, so keep-alives and
remote closes are handled even when the application is not calling `read()`.
Decoded data waiting for a reader is bounded to 1 MiB and 1,024 events by
default. Use `maxBufferedBytes` to change the byte limit:

```js
const conn = await dial(url, {
  transport: "sse",
  maxBufferedBytes: 256 * 1024,
});
```

If either receive-buffer limit would be exceeded, the connection closes rather
than silently dropping data. Its pending and subsequent reads reject with an
SSE receive-buffer error, and subsequent writes report a closed connection.

### Connection properties

- `conn.transport` — `"ws"`, `"sse"` or `"wt"`
- `conn.url` — the base URL used to connect

## Transports

| Transport | Mechanism | Binary | Requirements |
|-----------|-----------|--------|-------------|
| `ws` | WebSocket | native | WebSocket support |
| `sse` | Server-Sent Events (read) + POST (write) | base64 | HTTP/1.1+ |
| `wt` | WebTransport bidirectional stream (HTTP/3) | native | HTTP/3 listener, HTTPS |

WebSocket is preferred. SSE+POST is the fallback for environments where WebSocket connections are blocked (e.g. some corporate proxies). WebTransport is opt-in on both clients: it needs a separate HTTP/3 listener, and UDP is exactly what the restrictive networks SSE exists for tend to block.

## Protocol

The server is a single `http.Handler` that routes by content-negotiation:

- `Upgrade: websocket` header — WebSocket upgrade, binary frames carry data
- `GET` with `Accept: text/event-stream` — SSE stream; first event is `sid` (session ID), subsequent `d` events carry base64-encoded data, `close` event signals shutdown
- `POST` with `?s=<sid>` — write body bytes to the session; append `&close=1` to close

WebTransport is served from a separate HTTP/3 listener on the same path:

- HTTP/3 extended `CONNECT` — establishes the session; routing keys on the
  method alone, since the protocol token differs between drafts
- one client-initiated bidirectional stream carries the byte stream, with no
  framing and no base64
- datagrams carry `ping:<ts>`/`pong:<ts>`, the same control vocabulary the
  WebSocket transport puts in text frames; the peer echoes the timestamp so
  the client can measure latency
- QUIC PING frames provide the keep-alive, so there is no webdial-level server
  heartbeat, matching WebSocket

// webdial ESM client — zero dependencies, browser + Node.js 22+
// Implements net.Conn-like interface over WebSocket with SSE+POST fallback.

const PING_INTERVAL_MS = 5000;
// A transport that dies without a close handshake stays "open" as far as the
// caller can tell, so writes keep succeeding into a peer that already forgot
// us. An unanswered ping is the only evidence available. Timers are throttled
// in background tabs, but message delivery is not, so a live peer still clears
// this well before the deadline.
const PONG_TIMEOUT_MS = 3 * PING_INTERVAL_MS;
const MAX_BUFFERED_BYTES = 1024 * 1024;
// A byte limit alone does not bound the bookkeeping for empty data events.
const SSE_MAX_BUFFERED_EVENTS = 1024;

/** Decode a base64 string (handles unpadded) to Uint8Array. */
function base64Decode(str) {
  // Add padding if needed
  while (str.length % 4 !== 0) str += "=";
  const bin = atob(str);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

const TRANSPORTS = { ws: dialWS, sse: dialSSE, wt: dialWT };

// WebTransport is deliberately absent: it needs an HTTP/3 listener on a
// separate UDP port, so probing it by default would cost every dial a failed
// connection attempt everywhere it is not deployed — and in exactly the
// networks this library exists for, that failure arrives only after a QUIC
// handshake timeout.
const DEFAULT_CHAIN = ["ws", "sse"];

/**
 * Dial connects to a webdial server.
 * Tries WebSocket first, falls back to SSE+POST. WebTransport is opt-in.
 * @param {string} baseURL
 * @param {{ transport?: 'ws' | 'sse' | 'wt' | Array<'ws'|'sse'|'wt'>, pingIntervalMs?: number, pongTimeoutMs?: number, maxBufferedBytes?: number, wtURL?: string, wt?: object }} [opts]
 *   transport names one transport, or an ordered list to try in turn.
 *   pingIntervalMs paces the keep-alive; pongTimeoutMs is how long an
 *   unanswered ping stands before the connection is presumed dead (default
 *   three intervals). maxBufferedBytes limits buffered data waiting for a
 *   reader (default 1 MiB); SSE also caps the backlog at 1024 data events.
 *   wtURL overrides the endpoint used for WebTransport, whose HTTP/3 listener
 *   is often on a different port; wt is passed to the WebTransport
 *   constructor, for options such as serverCertificateHashes.
 * @returns {Promise<WebDialConn>}
 */
export async function dial(baseURL, opts) {
  const chain = transportChain(opts?.transport);
  let lastErr;
  for (const name of chain) {
    try {
      return await TRANSPORTS[name](baseURL, opts);
    } catch (err) {
      lastErr = err;
    }
  }
  throw lastErr;
}

function transportChain(transport) {
  if (transport === undefined || transport === null) return DEFAULT_CHAIN;
  const chain = Array.isArray(transport) ? transport : [transport];
  if (chain.length === 0) {
    throw new TypeError("webdial: transport list is empty");
  }
  for (const name of chain) {
    // An unrecognised name used to fall through to the automatic chain, so a
    // typo quietly connected over some other transport instead.
    if (!Object.hasOwn(TRANSPORTS, name)) {
      throw new TypeError(`webdial: unknown transport ${JSON.stringify(name)}`);
    }
  }
  return chain;
}

// --- WebSocket transport ---

async function dialWS(baseURL, opts) {
  const wsURL = websocketURL(baseURL);
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsURL);
    ws.binaryType = "arraybuffer";
    ws.onopen = () => {
      ws.onopen = null;
      ws.onerror = null;
      resolve(new WSConn(ws, baseURL, opts));
    };
    ws.onerror = () => {
      ws.onopen = null;
      ws.onerror = null;
      reject(new Error("webdial: websocket connection failed"));
    };
  });
}

function websocketURL(baseURL) {
  const url = parseURL(baseURL);
  if (url.protocol === "http:") url.protocol = "ws:";
  else if (url.protocol === "https:") url.protocol = "wss:";
  return url.href;
}

// Encode only the new parameter. Mutating URL.searchParams directly would
// reserialize the caller's entire query (for example, changing %20 to +).
function appendURLQueryParam(baseURL, name, value) {
  const url = parseURL(baseURL);
  const encoded = new URLSearchParams([[name, value]]).toString();
  url.search = url.search ? `${url.search}&${encoded}` : encoded;
  return url.href;
}

function parseURL(baseURL) {
  // Browsers historically accepted relative endpoints because fetch and
  // WebSocket resolve them against the page. Keep that behavior while Node,
  // which has no document base URL, continues to require an absolute URL.
  return new URL(baseURL, globalThis.location?.href);
}

class WSConn {
  #ws;
  #queue = [];
  #waiters = [];
  #closed = false;
  #closeErr = null;
  #url;
  #latency = null;
  #pingTimer = null;
  #pingSentAt = null;
  #lastRecvAt = 0;
  #pingInterval = PING_INTERVAL_MS;
  #pongTimeout = PONG_TIMEOUT_MS;
  onLatency = null;

  constructor(ws, url, opts) {
    this.#ws = ws;
    this.#url = url;
    this.#pingInterval = opts?.pingIntervalMs ?? PING_INTERVAL_MS;
    this.#pongTimeout = opts?.pongTimeoutMs ?? this.#pingInterval * 3;
    ws.onmessage = (event) => {
      this.#lastRecvAt = performance.now();
      if (typeof event.data === "string") {
        this.#handleControl(event.data);
        return;
      }
      const data = new Uint8Array(event.data);
      if (this.#waiters.length > 0) {
        this.#waiters.shift().resolve(data);
      } else {
        this.#queue.push(data);
      }
      // Any data from the peer answers the current liveness question just as
      // conclusively as a pong. Clear that probe so the next interval starts
      // a fresh timeout window; retaining its timestamp would make this one
      // frame permanent evidence that the peer is alive.
      this.#pingSentAt = null;
    };
    ws.onclose = () => {
      this.#closed = true;
      this.#stopPing();
      for (const w of this.#waiters) w.resolve(null);
      this.#waiters = [];
    };
    ws.onerror = () => {
      this.#closed = true;
      this.#stopPing();
      this.#closeErr = new Error("webdial: websocket error");
      for (const w of this.#waiters) w.reject(this.#closeErr);
      this.#waiters = [];
    };
    this.#startPing();
  }

  /** @returns {Promise<Uint8Array|null>} null on EOF/close */
  async read() {
    if (this.#queue.length > 0) return this.#queue.shift();
    if (this.#closed) {
      if (this.#closeErr) throw this.#closeErr;
      return null;
    }
    return new Promise((resolve, reject) => {
      this.#waiters.push({ resolve, reject });
    });
  }

  /** @param {Uint8Array|string} data */
  async write(data) {
    if (this.#closed) throw new Error("webdial: connection closed");
    if (typeof data === "string") data = new TextEncoder().encode(data);
    this.#ws.send(data);
  }

  async close() {
    if (this.#closed) return;
    this.#closed = true;
    this.#stopPing();
    this.#ws.close();
  }

  #handleControl(text) {
    if (text.startsWith("pong:")) {
      this.#pingSentAt = null;
      const ts = parseFloat(text.slice(5));
      if (!Number.isNaN(ts)) {
        this.#latency = performance.now() - ts;
        this.onLatency?.(this.#latency);
      }
    }
  }

  /**
   * Ping now and restart the staleness window. A caller that knows wall-clock
   * passed while nothing here was running — a tab returning to the foreground,
   * a machine waking — needs both halves: the next scheduled tick would
   * otherwise judge the socket against a #pingSentAt from before the gap and
   * close a healthy connection, and waiting for that tick is the only other way
   * to find out the socket did not survive.
   */
  ping() {
    try {
      this.#ws.send("ping:" + performance.now());
    } catch {
      return;
    }
    this.#pingSentAt = performance.now();
  }

  #startPing() {
    this.#pingTimer = setInterval(() => {
      if (this.#stale()) {
        this.#ws.close();
        return;
      }
      const now = performance.now();
      if (now - this.#lastRecvAt < this.#pingInterval) return;
      try {
        this.#ws.send("ping:" + now);
        if (this.#pingSentAt === null) this.#pingSentAt = now;
      } catch {}
    }, this.#pingInterval);
  }

  #stale() {
    if (this.#pingSentAt === null) return false;
    // Frames the caller has not read yet are evidence that the peer was alive,
    // and prevent the watchdog from closing a working connection while a slow
    // consumer drains its backlog.
    if (this.#queue.length > 0) return false;
    return performance.now() - this.#pingSentAt > this.#pongTimeout;
  }

  #stopPing() {
    if (this.#pingTimer) {
      clearInterval(this.#pingTimer);
      this.#pingTimer = null;
    }
  }

  get latency() {
    return this.#latency;
  }
  get transport() {
    return "ws";
  }
  get url() {
    return this.#url;
  }
}

// --- SSE + POST transport ---

async function dialSSE(baseURL, opts) {
  const maxBufferedBytes =
    opts?.maxBufferedBytes ?? MAX_BUFFERED_BYTES;
  if (!Number.isSafeInteger(maxBufferedBytes) || maxBufferedBytes < 0) {
    throw new TypeError(
      "webdial: maxBufferedBytes must be a non-negative safe integer",
    );
  }
  const resp = await fetch(baseURL, {
    headers: { Accept: "text/event-stream" },
  });
  if (!resp.ok) throw new Error(`webdial: sse returned ${resp.status}`);
  const decoder = new SSEDecoder(resp.body.getReader());
  const first = await decoder.next();
  if (!first || first.event !== "sid") {
    throw new Error(`webdial: expected sid event, got ${first?.event}`);
  }
  return new SSEConn(baseURL, first.data, decoder, opts, maxBufferedBytes);
}

class SSEDecoder {
  #reader;
  #buf = "";
  #done = false;
  #dec = new TextDecoder();

  constructor(reader) {
    this.#reader = reader;
  }

  async next() {
    while (true) {
      const ev = this.#parse();
      if (ev) return ev;
      if (this.#done) return null;
      const { value, done } = await this.#reader.read();
      if (done) {
        this.#done = true;
        return this.#parse();
      }
      this.#buf += this.#dec
        .decode(value, { stream: true })
        .replace(/\r\n/g, "\n")
        .replace(/\r/g, "\n");
    }
  }

  async cancel() {
    this.#done = true;
    await this.#reader.cancel();
  }

  #parse() {
    const idx = this.#buf.indexOf("\n\n");
    if (idx === -1) return null;
    const block = this.#buf.slice(0, idx);
    this.#buf = this.#buf.slice(idx + 2);
    let event = "";
    let data = "";
    for (const line of block.split("\n")) {
      if (line.startsWith("event:")) event = line.slice(6).trimStart();
      else if (line.startsWith("data:")) data = line.slice(5).trimStart();
    }
    return { event, data };
  }
}

class SSEConn {
  #baseURL;
  #sid;
  #decoder;
  #queue = [];
  #queuedBytes = 0;
  #waiters = [];
  #closed = false;
  #closeErr = null;
  #url;
  #postURL;
  #latency = null;
  #pingTimer = null;
  #pingSentAt = null;
  #pingInterval = PING_INTERVAL_MS;
  #pongTimeout = PONG_TIMEOUT_MS;
  #maxBufferedBytes = MAX_BUFFERED_BYTES;
  onLatency = null;

  constructor(baseURL, sid, decoder, opts, maxBufferedBytes) {
    this.#pingInterval = opts?.pingIntervalMs ?? PING_INTERVAL_MS;
    this.#pongTimeout = opts?.pongTimeoutMs ?? this.#pingInterval * 3;
    this.#baseURL = baseURL;
    this.#sid = sid;
    this.#decoder = decoder;
    this.#maxBufferedBytes = maxBufferedBytes;
    this.#url = baseURL;
    this.#postURL = appendURLQueryParam(baseURL, "s", sid);
    this.#startPing();
    // Exactly one task owns the decoder. Public reads consume its deliveries,
    // never the event stream itself, so control traffic remains live even when
    // the application is idle or has multiple reads pending.
    void this.#pump();
  }

  /** @returns {Promise<Uint8Array|null>} null on EOF/close */
  async read() {
    if (this.#queue.length > 0) {
      const data = this.#queue.shift();
      this.#queuedBytes -= data.byteLength;
      return data;
    }
    if (this.#closed) {
      if (this.#closeErr) throw this.#closeErr;
      return null;
    }
    return new Promise((resolve, reject) => {
      this.#waiters.push({ resolve, reject });
    });
  }

  /** @param {Uint8Array|string} data */
  async write(data) {
    if (this.#closed) throw new Error("webdial: connection closed");
    if (typeof data === "string") data = new TextEncoder().encode(data);
    const resp = await fetch(this.#postURL, {
      method: "POST",
      headers: { "Content-Type": "application/octet-stream" },
      body: data,
    });
    if (resp.status !== 204) {
      throw new Error(`webdial: post returned ${resp.status}`);
    }
  }

  async close() {
    if (this.#closed) return;
    this.#finish(null, true);
    // Cancel the decoder before starting the best-effort close POST. A slow or
    // lost POST must not leave the pump or application reads blocked.
    const cancel = this.#decoder.cancel().catch(() => {});
    try {
      await fetch(appendURLQueryParam(this.#postURL, "close", "1"), {
        method: "POST",
      });
    } catch {}
    await cancel;
  }

  /** See WSConn.ping. */
  ping() {
    this.#pingSentAt = performance.now();
    fetch(appendURLQueryParam(this.#postURL, "ping", performance.now()), {
      method: "POST",
    }).catch(() => {});
  }

  #startPing() {
    this.#pingTimer = setInterval(() => {
      if (this.#stale()) {
        this.close().catch(() => {});
        return;
      }
      if (this.#pingSentAt === null) this.#pingSentAt = performance.now();
      fetch(appendURLQueryParam(this.#postURL, "ping", performance.now()), {
        method: "POST",
      }).catch(() => {});
    }, this.#pingInterval);
  }

  #stale() {
    if (this.#pingSentAt === null) return false;
    return performance.now() - this.#pingSentAt > this.#pongTimeout;
  }

  async #pump() {
    try {
      while (!this.#closed) {
        const ev = await this.#decoder.next();
        if (this.#closed) return;
        if (!ev) {
          this.#finish(null, false);
          return;
        }
        if (ev.event === "d") {
          // Data satisfies the outstanding liveness probe. The next timer tick
          // starts a fresh window, so finite activity cannot permanently mask
          // a dead peer.
          this.#pingSentAt = null;
          const data = base64Decode(ev.data);
          if (this.#waiters.length > 0) {
            this.#waiters.shift().resolve(data);
            continue;
          }
          if (
            this.#queue.length >= SSE_MAX_BUFFERED_EVENTS ||
            this.#queuedBytes + data.byteLength > this.#maxBufferedBytes
          ) {
            const err = new Error(
              `webdial: SSE receive buffer exceeded (${this.#maxBufferedBytes} bytes or ${SSE_MAX_BUFFERED_EVENTS} events)`,
            );
            this.#finish(err, true);
            await this.#decoder.cancel().catch(() => {});
            return;
          }
          this.#queue.push(data);
          this.#queuedBytes += data.byteLength;
          continue;
        }
        if (ev.event === "pong") {
          this.#pingSentAt = null;
          const ts = parseFloat(ev.data);
          if (!Number.isNaN(ts)) {
            this.#latency = performance.now() - ts;
            this.onLatency?.(this.#latency);
          }
          continue;
        }
        if (ev.event === "ping") {
          // Server heartbeats are also direct evidence that the peer is live.
          this.#pingSentAt = null;
          continue;
        }
        if (ev.event === "close") {
          this.#finish(null, false);
          await this.#decoder.cancel().catch(() => {});
          return;
        }
      }
    } catch (err) {
      if (!this.#closed) {
        const closeErr =
          err instanceof Error ? err : new Error(`webdial: SSE read: ${err}`);
        this.#finish(closeErr, false);
        await this.#decoder.cancel().catch(() => {});
      }
    }
  }

  #finish(err, discardQueue) {
    if (this.#closed) return;
    this.#closed = true;
    this.#closeErr = err;
    this.#stopPing();
    if (discardQueue) {
      this.#queue = [];
      this.#queuedBytes = 0;
    }
    for (const waiter of this.#waiters) {
      if (err) waiter.reject(err);
      else waiter.resolve(null);
    }
    this.#waiters = [];
  }

  #stopPing() {
    if (this.#pingTimer) {
      clearInterval(this.#pingTimer);
      this.#pingTimer = null;
    }
  }

  get latency() {
    return this.#latency;
  }
  get transport() {
    return "sse";
  }
  get url() {
    return this.#url;
  }
}

// --- WebTransport transport ---

async function dialWT(baseURL, opts) {
  // Absence is the common case outside Chromium, so it is reported as a plain
  // failure rather than a thrown ReferenceError from the constructor.
  if (typeof globalThis.WebTransport !== "function") {
    throw new Error("webdial: WebTransport is not supported");
  }
  const maxBufferedBytes = opts?.maxBufferedBytes ?? MAX_BUFFERED_BYTES;
  if (!Number.isSafeInteger(maxBufferedBytes) || maxBufferedBytes < 0) {
    throw new TypeError(
      "webdial: maxBufferedBytes must be a non-negative safe integer",
    );
  }
  // The HTTP/3 listener usually sits on a different port from the page, so the
  // endpoint may be overridden without disturbing the reported url.
  const url = webtransportURL(opts?.wtURL ?? baseURL);
  const wt = new WebTransport(url, opts?.wt);
  try {
    await wt.ready;
  } catch (err) {
    throw new Error("webdial: webtransport connection failed", { cause: err });
  }
  const stream = await wt.createBidirectionalStream();
  return new WTConn(wt, stream, baseURL, opts, maxBufferedBytes);
}

function webtransportURL(baseURL) {
  const url = parseURL(baseURL);
  // Unlike websocketURL, which leaves unknown schemes alone, anything that is
  // not http(s) is rejected here: WebTransport has no cleartext form, and the
  // alternative is an opaque failure inside the QUIC handshake.
  if (url.protocol === "http:") url.protocol = "https:";
  else if (url.protocol !== "https:") {
    throw new TypeError(
      `webdial: webtransport requires an https URL, got ${url.protocol}`,
    );
  }
  return url.href;
}

class WTConn {
  #wt;
  #stream;
  #reader;
  #writer;
  #datagramWriter = null;
  #queue = [];
  #queuedBytes = 0;
  #waiters = [];
  #resume = null;
  #closed = false;
  #closeErr = null;
  #url;
  #latency = null;
  #pingTimer = null;
  #pingSentAt = null;
  #pingInterval = PING_INTERVAL_MS;
  #pongTimeout = PONG_TIMEOUT_MS;
  #maxBufferedBytes = MAX_BUFFERED_BYTES;
  onLatency = null;

  constructor(wt, stream, url, opts, maxBufferedBytes) {
    this.#wt = wt;
    this.#stream = stream;
    this.#url = url;
    this.#maxBufferedBytes = maxBufferedBytes ?? MAX_BUFFERED_BYTES;
    this.#pingInterval = opts?.pingIntervalMs ?? PING_INTERVAL_MS;
    this.#pongTimeout = opts?.pongTimeoutMs ?? this.#pingInterval * 3;
    this.#reader = stream.readable.getReader();
    // getWriter locks the stream, so the writer is acquired once and reused
    // rather than per write.
    this.#writer = stream.writable.getWriter();
    // Opening a stream is local, and its WebTransport header may not reach the
    // server until the first write. Flushing an empty chunk makes the stream
    // visible so the server can accept it before either side sends data.
    this.#writer.write(new Uint8Array(0)).catch(() => {});
    // The session can die while no read is outstanding, so EOF and errors have
    // to arrive through here rather than only through the read path.
    wt.closed?.then?.(
      () => this.#finish(null),
      (err) => this.#finish(err),
    );
    this.#startPing();
    void this.#pump();
    void this.#datagramPump();
  }

  /** @returns {Promise<Uint8Array|null>} null on EOF/close */
  async read() {
    if (this.#queue.length > 0) return this.#take();
    if (this.#closed) {
      if (this.#closeErr) throw this.#closeErr;
      return null;
    }
    return new Promise((resolve, reject) => {
      this.#waiters.push({ resolve, reject });
    });
  }

  /** @param {Uint8Array|string} data */
  async write(data) {
    if (this.#closed) throw new Error("webdial: connection closed");
    if (typeof data === "string") data = new TextEncoder().encode(data);
    // ready resolves when the stream can accept more, which is what applies
    // QUIC back-pressure to a caller that outruns the peer.
    await this.#writer.ready;
    await this.#writer.write(data);
  }

  async close() {
    if (this.#closed) return;
    // Set closed before awaiting anything, so a concurrent close returns here
    // and the peer-facing teardown below runs exactly once.
    this.#finish(null);
    this.#reader.cancel().catch(() => {});
    try {
      await this.#writer.close();
    } catch {
      // A session that already failed cannot flush; closing it is all that is
      // left to do.
    }
    try {
      this.#wt.close();
    } catch {}
  }

  #take() {
    const data = this.#queue.shift();
    this.#queuedBytes -= data.byteLength;
    // Room again: let the pump resume pulling from the stream.
    if (this.#resume && this.#queuedBytes < this.#maxBufferedBytes) {
      const resume = this.#resume;
      this.#resume = null;
      resume();
    }
    return data;
  }

  // pump owns the reader. Unlike SSE, which must fail a connection whose
  // backlog overflows because the server has already pushed the bytes,
  // WebTransport can simply stop pulling: the unread data stays in the peer's
  // flow-control window and slows the sender instead.
  async #pump() {
    try {
      while (!this.#closed) {
        if (this.#queuedBytes >= this.#maxBufferedBytes) {
          await new Promise((resolve) => {
            this.#resume = resolve;
          });
          continue;
        }
        const { value, done } = await this.#reader.read();
        if (done) {
          this.#finish(null);
          return;
        }
        const data = new Uint8Array(
          value.buffer,
          value.byteOffset,
          value.byteLength,
        );
        if (this.#waiters.length > 0) {
          this.#waiters.shift().resolve(data);
        } else {
          this.#queue.push(data);
          this.#queuedBytes += data.byteLength;
        }
        // Inbound data answers the liveness question as conclusively as a pong.
        this.#pingSentAt = null;
      }
    } catch (err) {
      this.#finish(err);
    }
  }

  async #datagramPump() {
    const readable = this.#wt.datagrams?.readable;
    if (!readable) return;
    const reader = readable.getReader();
    try {
      while (!this.#closed) {
        const { value, done } = await reader.read();
        if (done) return;
        this.#handleControl(new TextDecoder().decode(value));
      }
    } catch {
      // Losing the datagram channel costs latency reporting, not the
      // connection: the stream carries the data.
    }
  }

  #handleControl(text) {
    if (text.startsWith("pong:")) {
      this.#pingSentAt = null;
      const ts = parseFloat(text.slice(5));
      if (!Number.isNaN(ts)) {
        this.#latency = performance.now() - ts;
        this.onLatency?.(this.#latency);
      }
    }
  }

  #finish(err) {
    if (this.#closed) return;
    this.#closed = true;
    this.#closeErr = err ?? null;
    this.#stopPing();
    // A parked pump would otherwise hold the last reference to this reader.
    if (this.#resume) {
      const resume = this.#resume;
      this.#resume = null;
      resume();
    }
    for (const w of this.#waiters) {
      if (err) w.reject(err);
      else w.resolve(null);
    }
    this.#waiters = [];
  }

  /**
   * Ping now and restart the staleness window. See WSConn.ping: a caller that
   * knows wall-clock passed while nothing here was running needs both halves.
   */
  ping() {
    if (!this.#sendPing()) return;
    this.#pingSentAt = performance.now();
  }

  #sendPing() {
    const writable = this.#wt.datagrams?.writable;
    if (!writable) return false;
    try {
      this.#datagramWriter ??= writable.getWriter();
      this.#datagramWriter.write(
        new TextEncoder().encode("ping:" + performance.now()),
      );
      return true;
    } catch {
      return false;
    }
  }

  #startPing() {
    this.#pingTimer = setInterval(() => {
      if (this.#stale()) {
        void this.close();
        return;
      }
      if (this.#sendPing() && this.#pingSentAt === null) {
        this.#pingSentAt = performance.now();
      }
    }, this.#pingInterval);
  }

  #stale() {
    if (this.#pingSentAt === null) return false;
    // Data the caller has not read yet is evidence the peer was alive, and
    // stops the watchdog closing a working connection under a slow consumer.
    if (this.#queue.length > 0) return false;
    return performance.now() - this.#pingSentAt > this.#pongTimeout;
  }

  #stopPing() {
    if (this.#pingTimer) {
      clearInterval(this.#pingTimer);
      this.#pingTimer = null;
    }
  }

  get latency() {
    return this.#latency;
  }
  get transport() {
    return "wt";
  }
  get url() {
    return this.#url;
  }
}

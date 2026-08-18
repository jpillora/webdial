// webdial ESM client — zero dependencies, browser + Node.js 22+
// Implements net.Conn-like interface over WebSocket with SSE+POST fallback.

const PING_INTERVAL_MS = 5000;
// A transport that dies without a close handshake stays "open" as far as the
// caller can tell, so writes keep succeeding into a peer that already forgot
// us. An unanswered ping is the only evidence available. Timers are throttled
// in background tabs, but message delivery is not, so a live peer still clears
// this well before the deadline.
const PONG_TIMEOUT_MS = 3 * PING_INTERVAL_MS;

/** Decode a base64 string (handles unpadded) to Uint8Array. */
function base64Decode(str) {
  // Add padding if needed
  while (str.length % 4 !== 0) str += "=";
  const bin = atob(str);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

/**
 * Dial connects to a webdial server.
 * Tries WebSocket first, falls back to SSE+POST.
 * @param {string} baseURL
 * @param {{ transport?: 'ws' | 'sse', pingIntervalMs?: number, pongTimeoutMs?: number }} [opts]
 *   pingIntervalMs paces the keep-alive; pongTimeoutMs is how long an
 *   unanswered ping stands before the connection is presumed dead (default
 *   three intervals).
 * @returns {Promise<WebDialConn>}
 */
export async function dial(baseURL, opts) {
  const transport = opts?.transport;
  if (transport === "sse") return dialSSE(baseURL, opts);
  if (transport === "ws") return dialWS(baseURL, opts);
  try {
    return await dialWS(baseURL, opts);
  } catch {
    return await dialSSE(baseURL, opts);
  }
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
      this.#lastRecvAt = performance.now();
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
      try {
        this.#ws.send("ping:" + performance.now());
        if (this.#pingSentAt === null) this.#pingSentAt = performance.now();
      } catch {}
    }, this.#pingInterval);
  }

  #stale() {
    if (this.#pingSentAt === null) return false;
    // Frames the caller has not read yet are the same evidence, and they
    // survive the case #lastRecvAt cannot cover: while a slow consumer drains
    // the queue in one uninterrupted run of microtasks, neither onmessage nor
    // this timer gets to run, and which of them the event loop reaches first
    // afterwards is not ours to decide.
    if (this.#queue.length > 0) return false;
    // Data arriving after the ping went out proves the peer is there, so the
    // missing pong is queued rather than lost. That is the normal state of a
    // connection whose peer is sending faster than this end can consume: the
    // pong sits behind the backlog, and every frame still to come is evidence
    // against the conclusion that nobody is home. Closing here would tear down
    // a working connection precisely when it is busiest.
    if (this.#lastRecvAt > this.#pingSentAt) return false;
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
  const resp = await fetch(baseURL, {
    headers: { Accept: "text/event-stream" },
  });
  if (!resp.ok) throw new Error(`webdial: sse returned ${resp.status}`);
  const decoder = new SSEDecoder(resp.body.getReader());
  const first = await decoder.next();
  if (!first || first.event !== "sid") {
    throw new Error(`webdial: expected sid event, got ${first?.event}`);
  }
  return new SSEConn(baseURL, first.data, decoder, opts);
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
  #closed = false;
  #url;
  #postURL;
  #latency = null;
  #pingTimer = null;
  #pingSentAt = null;
  #lastRecvAt = 0;
  #pingInterval = PING_INTERVAL_MS;
  #pongTimeout = PONG_TIMEOUT_MS;
  onLatency = null;

  constructor(baseURL, sid, decoder, opts) {
    this.#pingInterval = opts?.pingIntervalMs ?? PING_INTERVAL_MS;
    this.#pongTimeout = opts?.pongTimeoutMs ?? this.#pingInterval * 3;
    this.#baseURL = baseURL;
    this.#sid = sid;
    this.#decoder = decoder;
    this.#url = baseURL;
    this.#postURL = appendURLQueryParam(baseURL, "s", sid);
    this.#startPing();
  }

  /** @returns {Promise<Uint8Array|null>} null on EOF/close */
  async read() {
    if (this.#closed) return null;
    while (true) {
      const ev = await this.#decoder.next();
      if (!ev) {
        this.#closed = true;
        this.#stopPing();
        return null;
      }
      if (ev.event === "d") {
        this.#lastRecvAt = performance.now();
        return base64Decode(ev.data);
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
      if (ev.event === "close") {
        this.#closed = true;
        this.#stopPing();
        return null;
      }
    }
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
    this.#closed = true;
    this.#stopPing();
    try {
      await fetch(appendURLQueryParam(this.#postURL, "close", "1"), {
        method: "POST",
      });
    } catch {}
    try {
      await this.#decoder.cancel();
    } catch {}
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
    // Data arriving after the ping went out proves the peer is there, so the
    // missing pong is queued rather than lost. That is the normal state of a
    // connection whose peer is sending faster than this end can consume: the
    // pong sits behind the backlog, and every frame still to come is evidence
    // against the conclusion that nobody is home. Closing here would tear down
    // a working connection precisely when it is busiest.
    if (this.#lastRecvAt > this.#pingSentAt) return false;
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
    return "sse";
  }
  get url() {
    return this.#url;
  }
}

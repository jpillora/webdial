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
 * @param {{ transport?: 'ws' | 'sse' }} [opts]
 * @returns {Promise<WebDialConn>}
 */
export async function dial(baseURL, opts) {
  baseURL = baseURL.replace(/\/+$/, "");
  const transport = opts?.transport;
  if (transport === "sse") return dialSSE(baseURL);
  if (transport === "ws") return dialWS(baseURL);
  try {
    return await dialWS(baseURL);
  } catch {
    return await dialSSE(baseURL);
  }
}

// --- WebSocket transport ---

async function dialWS(baseURL) {
  const wsURL = baseURL.replace(/^https:/, "wss:").replace(/^http:/, "ws:");
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsURL);
    ws.binaryType = "arraybuffer";
    ws.onopen = () => {
      ws.onopen = null;
      ws.onerror = null;
      resolve(new WSConn(ws, baseURL));
    };
    ws.onerror = () => {
      ws.onopen = null;
      ws.onerror = null;
      reject(new Error("webdial: websocket connection failed"));
    };
  });
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
  onLatency = null;

  constructor(ws, url) {
    this.#ws = ws;
    this.#url = url;
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
    }, PING_INTERVAL_MS);
  }

  #stale() {
    if (this.#pingSentAt === null) return false;
    return performance.now() - this.#pingSentAt > PONG_TIMEOUT_MS;
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

async function dialSSE(baseURL) {
  const resp = await fetch(baseURL, {
    headers: { Accept: "text/event-stream" },
  });
  if (!resp.ok) throw new Error(`webdial: sse returned ${resp.status}`);
  const decoder = new SSEDecoder(resp.body.getReader());
  const first = await decoder.next();
  if (!first || first.event !== "sid") {
    throw new Error(`webdial: expected sid event, got ${first?.event}`);
  }
  return new SSEConn(baseURL, first.data, decoder);
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
  onLatency = null;

  constructor(baseURL, sid, decoder) {
    this.#baseURL = baseURL;
    this.#sid = sid;
    this.#decoder = decoder;
    this.#url = baseURL;
    const sep = baseURL.includes("?") ? "&" : "?";
    this.#postURL = `${baseURL}${sep}s=${sid}`;
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
      if (ev.event === "d") return base64Decode(ev.data);
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
      await fetch(`${this.#postURL}&close=1`, {
        method: "POST",
      });
    } catch {}
    try {
      await this.#decoder.cancel();
    } catch {}
  }

  #startPing() {
    this.#pingTimer = setInterval(() => {
      if (this.#stale()) {
        this.close().catch(() => {});
        return;
      }
      if (this.#pingSentAt === null) this.#pingSentAt = performance.now();
      fetch(`${this.#postURL}&ping=${performance.now()}`, {
        method: "POST",
      }).catch(() => {});
    }, PING_INTERVAL_MS);
  }

  #stale() {
    if (this.#pingSentAt === null) return false;
    return performance.now() - this.#pingSentAt > PONG_TIMEOUT_MS;
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

// webdial ESM client — zero dependencies, Node.js 22+
// Implements net.Conn-like interface over WebSocket with SSE+POST fallback.

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
  const wsURL =
    baseURL.replace(/^https:/, "wss:").replace(/^http:/, "ws:") + "/ws";
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

  constructor(ws, url) {
    this.#ws = ws;
    this.#url = url;
    ws.onmessage = (event) => {
      const data = new Uint8Array(event.data);
      if (this.#waiters.length > 0) {
        this.#waiters.shift().resolve(data);
      } else {
        this.#queue.push(data);
      }
    };
    ws.onclose = () => {
      this.#closed = true;
      for (const w of this.#waiters) w.resolve(null);
      this.#waiters = [];
    };
    ws.onerror = () => {
      this.#closed = true;
      this.#closeErr = new Error("webdial: websocket error");
      for (const w of this.#waiters) w.reject(this.#closeErr);
      this.#waiters = [];
    };
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
    this.#ws.close();
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
  const resp = await fetch(baseURL + "/sse", {
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

  constructor(baseURL, sid, decoder) {
    this.#baseURL = baseURL;
    this.#sid = sid;
    this.#decoder = decoder;
    this.#url = baseURL;
  }

  /** @returns {Promise<Uint8Array|null>} null on EOF/close */
  async read() {
    if (this.#closed) return null;
    while (true) {
      const ev = await this.#decoder.next();
      if (!ev) {
        this.#closed = true;
        return null;
      }
      if (ev.event === "d") return Buffer.from(ev.data, "base64");
      if (ev.event === "close") {
        this.#closed = true;
        return null;
      }
    }
  }

  /** @param {Uint8Array|string} data */
  async write(data) {
    if (this.#closed) throw new Error("webdial: connection closed");
    if (typeof data === "string") data = new TextEncoder().encode(data);
    const resp = await fetch(`${this.#baseURL}/post?s=${this.#sid}`, {
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
    try {
      await fetch(`${this.#baseURL}/post?s=${this.#sid}&close=1`, {
        method: "POST",
      });
    } catch {}
    try {
      await this.#decoder.cancel();
    } catch {}
  }

  get transport() {
    return "sse";
  }
  get url() {
    return this.#url;
  }
}

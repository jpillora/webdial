// WebTransport client tests. Node has no WebTransport implementation, so the
// global is mocked; the browser path is exercised by example/index.html against
// testdata/devserver.
import { strict as assert } from "node:assert";
import { dial } from "./client.mjs";

const TEST_TIMEOUT_MS = 2000;

function withTimeout(promise, what) {
  return Promise.race([
    promise,
    new Promise((_, reject) =>
      setTimeout(
        () => reject(new Error(`timed out waiting for ${what}`)),
        TEST_TIMEOUT_MS,
      ),
    ),
  ]);
}

async function waitFor(predicate, what) {
  const deadline = Date.now() + TEST_TIMEOUT_MS;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await new Promise((r) => setTimeout(r, 5));
  }
  throw new Error(`timed out waiting for ${what}`);
}

// MockWebTransport mirrors the browser API surface WTConn depends on:
// ready/closed promises, one bidirectional stream, and the datagram pair.
class MockWebTransport {
  static instances = [];

  constructor(url, opts) {
    this.url = url;
    this.opts = opts;
    this.closeCalls = 0;
    this.writerCloseCalls = 0;
    this.written = [];
    this.datagramsOut = [];
    this.readableCancelled = false;
    MockWebTransport.instances.push(this);

    this.ready = MockWebTransport.failReady
      ? Promise.reject(new Error("mock: ready failed"))
      : Promise.resolve();
    this.ready.catch(() => {});

    this.closed = new Promise((resolve, reject) => {
      this._resolveClosed = resolve;
      this._rejectClosed = reject;
    });
    this.closed.catch(() => {});

    this.datagrams = {
      readable: new ReadableStream({
        start: (c) => {
          this._datagramCtl = c;
        },
      }),
      writable: new WritableStream({
        write: (chunk) => {
          this.datagramsOut.push(new TextDecoder().decode(chunk));
        },
      }),
    };
  }

  createBidirectionalStream() {
    const self = this;
    this.pullCount = 0;
    return Promise.resolve({
      readable: new ReadableStream({
        start(c) {
          self._streamCtl = c;
        },
        pull() {
          self.pullCount++;
          if (self.autoFeed) self._streamCtl.enqueue(new Uint8Array(8));
        },
        cancel() {
          self.readableCancelled = true;
        },
      }),
      writable: new WritableStream({
        write(chunk) {
          self.written.push(chunk);
        },
        close() {
          self.writerCloseCalls++;
        },
      }),
    });
  }

  close() {
    this.closeCalls++;
    this._resolveClosed();
  }

  // -- test helpers --
  push(bytes) {
    this._streamCtl.enqueue(bytes);
  }
  eof() {
    this._streamCtl.close();
  }
  sendDatagram(text) {
    this._datagramCtl.enqueue(new TextEncoder().encode(text));
  }
  fail(err) {
    this._rejectClosed(err);
  }
  // The constructor flushes an empty chunk to make the stream visible to the
  // server; application writes are everything after that.
  get payloads() {
    return this.written.filter((c) => c.byteLength > 0);
  }
}

function installWT() {
  MockWebTransport.instances = [];
  MockWebTransport.failReady = false;
  globalThis.WebTransport = MockWebTransport;
  return MockWebTransport;
}

let failed = 0;
async function test(name, fn) {
  console.log(`test ${name}...`);
  const savedWT = globalThis.WebTransport;
  const savedWS = globalThis.WebSocket;
  const savedFetch = globalThis.fetch;
  try {
    await fn();
    console.log("  pass");
  } catch (err) {
    failed++;
    console.log(`  FAIL: ${err.stack || err.message}`);
  } finally {
    globalThis.WebTransport = savedWT;
    globalThis.WebSocket = savedWS;
    globalThis.fetch = savedFetch;
  }
}

await test("wt derives the endpoint URL and preserves the reported url", async () => {
  const WT = installWT();
  const cases = [
    ["http://example.com/wd/", "https://example.com/wd/"],
    ["https://example.com/wd/", "https://example.com/wd/"],
    ["https://example.com/one/two/", "https://example.com/one/two/"],
    ["https://example.com/wd/?mode=fast", "https://example.com/wd/?mode=fast"],
    [
      "https://example.com/wd/?next=https%3A%2F%2Fupstream.example%2Fa%2Fb",
      "https://example.com/wd/?next=https%3A%2F%2Fupstream.example%2Fa%2Fb",
    ],
    [
      "https://example.com/wd/?next=https://upstream.example/a/",
      "https://example.com/wd/?next=https://upstream.example/a/",
    ],
    ["http://[2001:db8::1]:8080/wd/", "https://[2001:db8::1]:8080/wd/"],
    [
      "https://example.com/wd/?x=1#next=https://fragment.example/",
      "https://example.com/wd/?x=1#next=https://fragment.example/",
    ],
  ];
  for (const [input, want] of cases) {
    WT.instances = [];
    const conn = await dial(input, { transport: "wt" });
    assert.equal(WT.instances[0].url, want, `endpoint for ${input}`);
    assert.equal(conn.url, input, `reported url for ${input}`);
    assert.equal(conn.transport, "wt");
  }
});

await test("wt rejects non-http(s) schemes without constructing", async () => {
  const WT = installWT();
  for (const bad of ["ws://example.com/wd/", "wss://example.com/wd/"]) {
    await assert.rejects(
      () => dial(bad, { transport: "wt" }),
      (err) =>
        err instanceof TypeError &&
        /webtransport requires an https URL/.test(err.message),
      `expected ${bad} to be rejected`,
    );
  }
  assert.equal(WT.instances.length, 0, "no WebTransport should be constructed");
});

await test("wt honours wtURL and passes wt options through", async () => {
  const WT = installWT();
  const hashes = [{ algorithm: "sha-256", value: new Uint8Array([1, 2, 3]) }];
  const conn = await dial("http://page.example/wd/", {
    transport: "wt",
    wtURL: "https://h3.example:4433/wd/",
    wt: { serverCertificateHashes: hashes },
  });
  assert.equal(WT.instances[0].url, "https://h3.example:4433/wd/");
  assert.equal(WT.instances[0].opts.serverCertificateHashes, hashes);
  assert.equal(conn.url, "http://page.example/wd/", "url stays as supplied");
});

await test("wt round-trips data", async () => {
  const WT = installWT();
  const conn = await dial("https://example.com/wd/", { transport: "wt" });
  const wt = WT.instances[0];

  await conn.write("hello");
  assert.equal(wt.payloads.length, 1);
  assert.equal(new TextDecoder().decode(wt.payloads[0]), "hello");

  await conn.write(new Uint8Array([0, 1, 2, 255]));
  assert.deepEqual(Array.from(wt.payloads[1]), [0, 1, 2, 255]);

  wt.push(new TextEncoder().encode("world"));
  const got = await withTimeout(conn.read(), "inbound data");
  assert.equal(new TextDecoder().decode(got), "world");
});

await test("wt read returns null at EOF and writes then fail", async () => {
  const WT = installWT();
  const conn = await dial("https://example.com/wd/", { transport: "wt" });
  const wt = WT.instances[0];

  const pending = conn.read();
  wt.eof();
  assert.equal(await withTimeout(pending, "EOF"), null);
  assert.equal(await conn.read(), null, "reads stay at EOF");
  await assert.rejects(
    () => conn.write("x"),
    /webdial: connection closed/,
    "write after EOF must fail",
  );
});

await test("wt session failure surfaces to a pending read", async () => {
  const WT = installWT();
  const conn = await dial("https://example.com/wd/", { transport: "wt" });
  const wt = WT.instances[0];

  const pending = conn.read();
  wt.fail(new Error("session lost"));
  await assert.rejects(() => withTimeout(pending, "read rejection"), /session lost/);
});

await test("wt close is idempotent", async () => {
  const WT = installWT();
  const conn = await dial("https://example.com/wd/", { transport: "wt" });
  const wt = WT.instances[0];

  const pending = conn.read();
  await Promise.all([conn.close(), conn.close()]);
  await conn.close();

  assert.equal(await withTimeout(pending, "read to resolve on close"), null);
  assert.equal(wt.closeCalls, 1, "session closed exactly once");
  assert.equal(wt.writerCloseCalls, 1, "writer closed exactly once");
  assert.equal(wt.readableCancelled, true, "reader cancelled");
});

await test("wt reports latency from a pong datagram", async () => {
  const WT = installWT();
  const conn = await dial("https://example.com/wd/", { transport: "wt" });
  const wt = WT.instances[0];

  const seen = [];
  conn.onLatency = (ms) => seen.push(ms);

  // An unrecognised datagram must be ignored rather than throw.
  wt.sendDatagram("garbage");
  wt.sendDatagram("pong:" + (performance.now() - 25));

  await waitFor(() => seen.length > 0, "latency callback");
  assert.ok(conn.latency >= 20, `latency ${conn.latency} should be ~25ms`);
  assert.equal(seen.length, 1);
});

await test("wt ping writes a ping datagram", async () => {
  const WT = installWT();
  const conn = await dial("https://example.com/wd/", { transport: "wt" });
  const wt = WT.instances[0];

  conn.ping();
  await waitFor(() => wt.datagramsOut.length > 0, "ping datagram");
  assert.match(wt.datagramsOut[0], /^ping:\d+(\.\d+)?$/);
});

await test("wt back-pressure stops pulling until the reader drains", async () => {
  const WT = installWT();
  const conn = await dial("https://example.com/wd/", {
    transport: "wt",
    maxBufferedBytes: 16,
  });
  const wt = WT.instances[0];
  wt.autoFeed = true;
  wt._streamCtl.enqueue(new Uint8Array(8));

  // The pump must stop pulling once the backlog reaches the limit, rather than
  // buffering without bound the way the SSE transport has to.
  await waitFor(() => wt.pullCount > 0, "initial pulls");
  let stable = 0;
  let last = -1;
  await waitFor(async () => {
    if (wt.pullCount === last) stable++;
    else stable = 0;
    last = wt.pullCount;
    return stable >= 5;
  }, "pull count to settle");
  const paused = wt.pullCount;

  await withTimeout(conn.read(), "buffered chunk");
  await withTimeout(conn.read(), "buffered chunk");
  await waitFor(() => wt.pullCount > paused, "pulls to resume after draining");
});

await test("wt is never chosen by the automatic chain", async () => {
  installWT();
  let wsCreated = 0;
  globalThis.WebSocket = class {
    constructor() {
      wsCreated++;
      queueMicrotask(() => this.onopen?.());
    }
    send() {}
    close() {}
  };
  const conn = await dial("http://example.com/wd/");
  assert.equal(conn.transport, "ws", "auto must prefer ws over wt");
  assert.equal(wsCreated, 1);
  assert.equal(MockWebTransport.instances.length, 0, "wt must not be probed");
});

await test("wt falls back to sse when WebSocket is unavailable", async () => {
  installWT();
  delete globalThis.WebSocket;
  globalThis.fetch = async () =>
    new Response(
      new ReadableStream({
        start(c) {
          c.enqueue(new TextEncoder().encode("event: sid\ndata: abc\n\n"));
        },
      }),
      { status: 200 },
    );
  const conn = await dial("http://example.com/wd/");
  assert.equal(conn.transport, "sse");
  assert.equal(MockWebTransport.instances.length, 0, "wt must not be probed");
});

await test("wt is used when named in a transport chain", async () => {
  const WT = installWT();
  const conn = await dial("https://example.com/wd/", {
    transport: ["wt", "ws", "sse"],
  });
  assert.equal(conn.transport, "wt");
  assert.equal(WT.instances.length, 1);
});

await test("a failing wt falls through to the next transport in the chain", async () => {
  const WT = installWT();
  WT.failReady = true;
  let wsCreated = 0;
  globalThis.WebSocket = class {
    constructor() {
      wsCreated++;
      queueMicrotask(() => this.onopen?.());
    }
    send() {}
    close() {}
  };
  const conn = await dial("http://example.com/wd/", {
    transport: ["wt", "ws"],
  });
  assert.equal(conn.transport, "ws");
  assert.equal(wsCreated, 1);
});

await test("unknown transports are rejected", async () => {
  installWT();
  for (const bad of ["quic", ["ws", "nope"], []]) {
    await assert.rejects(
      () => dial("https://example.com/wd/", { transport: bad }),
      (err) => err instanceof TypeError,
      `expected ${JSON.stringify(bad)} to be rejected`,
    );
  }
});

await test("wt without platform support reports clearly but still falls back", async () => {
  delete globalThis.WebTransport;
  await assert.rejects(
    () => dial("https://example.com/wd/", { transport: "wt" }),
    /WebTransport is not supported/,
  );

  let wsCreated = 0;
  globalThis.WebSocket = class {
    constructor() {
      wsCreated++;
      queueMicrotask(() => this.onopen?.());
    }
    send() {}
    close() {}
  };
  const conn = await dial("http://example.com/wd/", {
    transport: ["wt", "ws"],
  });
  assert.equal(conn.transport, "ws");
  assert.equal(wsCreated, 1);
});

console.log(failed === 0 ? "\nall WebTransport tests passed" : `\n${failed} failed`);
process.exit(failed ? 1 : 0);

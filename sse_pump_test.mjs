import { strict as assert } from "node:assert";
import { dial } from "./client.mjs";

const originalFetch = globalThis.fetch;
const encoder = new TextEncoder();
const decoder = new TextDecoder();
const TEST_TIMEOUT_MS = 2000;

class ControlledReader {
  chunks = [];
  pending = [];
  done = false;
  failure = null;
  activeReads = 0;
  maxActiveReads = 0;
  cancelCalls = 0;

  constructor() {
    this.cancelled = new Promise((resolve) => {
      this.resolveCancelled = resolve;
    });
  }

  read() {
    this.activeReads++;
    this.maxActiveReads = Math.max(this.maxActiveReads, this.activeReads);
    return new Promise((resolve, reject) => {
      this.pending.push({
        resolve: (result) => {
          this.activeReads--;
          resolve(result);
        },
        reject: (err) => {
          this.activeReads--;
          reject(err);
        },
      });
      this.drain();
    });
  }

  enqueue(text) {
    assert.equal(this.done, false, "cannot enqueue after reader termination");
    this.chunks.push(encoder.encode(text));
    this.drain();
  }

  end() {
    this.done = true;
    this.drain();
  }

  fail(err) {
    this.failure = err;
    this.done = true;
    this.drain();
  }

  async cancel() {
    this.cancelCalls++;
    this.done = true;
    this.resolveCancelled();
    this.drain();
  }

  drain() {
    while (this.pending.length > 0) {
      if (this.chunks.length > 0) {
        this.pending.shift().resolve({ value: this.chunks.shift(), done: false });
      } else if (this.failure) {
        this.pending.shift().reject(this.failure);
      } else if (this.done) {
        this.pending.shift().resolve({ value: undefined, done: true });
      } else {
        return;
      }
    }
  }
}

class MockSSEPeer {
  reader = new ControlledReader();
  calls = [];
  respondToPings = false;
  holdClosePost = false;
  applicationWrites = 0;

  constructor() {
    this.reader.enqueue("event: sid\ndata: pump-test\n\n");
    this.closePostStarted = new Promise((resolve) => {
      this.resolveClosePostStarted = resolve;
    });
    this.closePostReleased = new Promise((resolve) => {
      this.releaseClosePost = resolve;
    });
  }

  fetch = async (resource, init = {}) => {
    const url = String(resource);
    this.calls.push({ url, init });
    if (init.method !== "POST") {
      return {
        ok: true,
        status: 200,
        body: { getReader: () => this.reader },
      };
    }

    const parsed = new URL(url);
    if (parsed.searchParams.has("ping")) {
      if (this.respondToPings) {
        this.pong(parsed.searchParams.get("ping"));
      }
    } else if (parsed.searchParams.get("close") === "1") {
      this.resolveClosePostStarted();
      if (this.holdClosePost) await this.closePostReleased;
    } else {
      this.applicationWrites++;
    }
    return { status: 204 };
  };

  data(text) {
    const encoded = btoa(text);
    this.reader.enqueue(`event: d\ndata: ${encoded}\n\n`);
  }

  emptyDataEvents(count) {
    this.reader.enqueue("event: d\ndata:\n\n".repeat(count));
  }

  pong(timestamp) {
    this.reader.enqueue(`event: pong\ndata: ${timestamp}\n\n`);
  }

  close() {
    this.reader.enqueue("event: close\n\n");
  }
}

function withTimeout(promise, description) {
  let timer;
  return Promise.race([
    promise,
    new Promise((_, reject) => {
      timer = setTimeout(
        () => reject(new Error(`timed out waiting for ${description}`)),
        TEST_TIMEOUT_MS,
      );
    }),
  ]).finally(() => clearTimeout(timer));
}

async function waitFor(predicate, description) {
  const deadline = Date.now() + TEST_TIMEOUT_MS;
  while (!predicate()) {
    if (Date.now() >= deadline) {
      throw new Error(`timed out waiting for ${description}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
}

async function open(peer, opts = {}) {
  globalThis.fetch = peer.fetch;
  return dial("http://pump.test/wd/?exact=a%20b#fragment", {
    transport: "sse",
    pingIntervalMs: 60_000,
    ...opts,
  });
}

try {
  {
    console.log("test idle SSE control processing and ordered later reads...");
    const peer = new MockSSEPeer();
    peer.respondToPings = true;
    const conn = await open(peer, {
      pingIntervalMs: 10,
      pongTimeoutMs: 30,
    });
    let pongs = 0;
    conn.onLatency = () => pongs++;
    peer.data("one");
    peer.data("two");
    peer.data("three");

    // No application read occurs until the pump has processed enough pongs to
    // span several timeout windows.
    await waitFor(() => pongs >= 8, "eight background pongs");
    await conn.write("still alive");
    assert.equal(peer.applicationWrites, 1);
    assert.deepEqual(
      await Promise.all([conn.read(), conn.read(), conn.read()]).then((items) =>
        items.map((item) => decoder.decode(item)),
      ),
      ["one", "two", "three"],
    );
    await conn.close();
    console.log("  pass");
  }

  {
    console.log("test a genuinely silent SSE peer still times out...");
    const peer = new MockSSEPeer();
    const conn = await open(peer, {
      pingIntervalMs: 10,
      pongTimeoutMs: 30,
    });
    assert.equal(
      await withTimeout(conn.read(), "silent-peer watchdog"),
      null,
    );
    assert.ok(peer.reader.cancelCalls > 0, "watchdog did not cancel the pump");
    console.log("  pass");
  }

  {
    console.log("test Close settles reads before a stalled close POST...");
    const peer = new MockSSEPeer();
    peer.holdClosePost = true;
    const conn = await open(peer);
    const reads = [conn.read(), conn.read(), conn.read()];
    let closeSettled = false;
    const closing = conn.close().finally(() => {
      closeSettled = true;
    });

    await withTimeout(peer.reader.cancelled, "decoder cancellation");
    await withTimeout(peer.closePostStarted, "close POST start");
    assert.deepEqual(await Promise.all(reads), [null, null, null]);
    assert.equal(closeSettled, false, "test close POST did not remain stalled");
    peer.releaseClosePost();
    await closing;
    console.log("  pass");
  }

  {
    console.log("test concurrent readers share one decoder pump...");
    const peer = new MockSSEPeer();
    const conn = await open(peer);
    const reads = [conn.read(), conn.read(), conn.read()];
    await Promise.resolve();
    assert.equal(peer.reader.maxActiveReads, 1);
    peer.data("first");
    peer.data("second");
    peer.data("third");
    assert.deepEqual(
      await Promise.all(reads).then((items) =>
        items.map((item) => decoder.decode(item)),
      ),
      ["first", "second", "third"],
    );

    const streamError = new Error("controlled decoder failure");
    const rejected = [
      assert.rejects(conn.read(), streamError),
      assert.rejects(conn.read(), streamError),
    ];
    peer.reader.fail(streamError);
    await Promise.all(rejected);
    await assert.rejects(conn.read(), streamError);
    console.log("  pass");
  }

  {
    console.log("test EOF settles every pending reader...");
    const peer = new MockSSEPeer();
    const conn = await open(peer);
    const reads = [conn.read(), conn.read()];
    peer.reader.end();
    assert.deepEqual(await Promise.all(reads), [null, null]);
    assert.equal(await conn.read(), null);
    console.log("  pass");
  }

  {
    console.log("test a remote close is processed behind queued data...");
    const peer = new MockSSEPeer();
    const conn = await open(peer);
    peer.data("last");
    peer.close();
    await withTimeout(peer.reader.cancelled, "remote-close cancellation");
    assert.equal(decoder.decode(await conn.read()), "last");
    assert.equal(await conn.read(), null);
    await assert.rejects(conn.write("nope"), /connection closed/);
    console.log("  pass");
  }

  {
    console.log("test decoded-byte buffer overflow fails closed...");
    const peer = new MockSSEPeer();
    const conn = await open(peer, { maxBufferedBytes: 5 });
    peer.data("abc");
    peer.data("def");
    await withTimeout(peer.reader.cancelled, "byte-overflow cancellation");
    await assert.rejects(conn.read(), /SSE receive buffer exceeded \(5 bytes/);
    await assert.rejects(conn.write("nope"), /connection closed/);
    console.log("  pass");
  }

  {
    console.log("test event-count buffer overflow fails closed...");
    const peer = new MockSSEPeer();
    const conn = await open(peer, { maxBufferedBytes: 1024 * 1024 });
    peer.emptyDataEvents(1025);
    await withTimeout(peer.reader.cancelled, "event-overflow cancellation");
    await assert.rejects(conn.read(), /SSE receive buffer exceeded/);
    console.log("  pass");
  }

  {
    console.log("test maxBufferedBytes validation happens before fetch...");
    let fetched = false;
    globalThis.fetch = async () => {
      fetched = true;
      throw new Error("unexpected fetch");
    };
    await assert.rejects(
      dial("http://pump.test/wd/", {
        transport: "sse",
        maxBufferedBytes: -1,
      }),
      /maxBufferedBytes must be a non-negative safe integer/,
    );
    assert.equal(fetched, false);
    console.log("  pass");
  }

  console.log("\nall SSE pump tests passed");
} finally {
  globalThis.fetch = originalFetch;
}

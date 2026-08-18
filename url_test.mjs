import { strict as assert } from "node:assert";
import { dial } from "./client.mjs";

const originalWebSocket = globalThis.WebSocket;
const originalFetch = globalThis.fetch;

class RecordingWebSocket {
  static urls = [];

  constructor(url) {
    RecordingWebSocket.urls.push(String(url));
    queueMicrotask(() => this.onopen?.());
  }

  send() {}

  close() {
    this.onclose?.();
  }
}

try {
  globalThis.WebSocket = RecordingWebSocket;

  const cases = [
    ["root endpoint", "http://example.com", "ws://example.com/"],
    [
      "nested path and trailing slash",
      "http://example.com/one/two/",
      "ws://example.com/one/two/",
    ],
    [
      "existing query",
      "http://example.com/wd/?mode=fast",
      "ws://example.com/wd/?mode=fast",
    ],
    [
      "encoded query value",
      "http://example.com/wd/?next=https%3A%2F%2Fupstream.example%2Fa%2Fb",
      "ws://example.com/wd/?next=https%3A%2F%2Fupstream.example%2Fa%2Fb",
    ],
    [
      "literal URL in query",
      "http://example.com/wd/?next=https://upstream.example/a/",
      "ws://example.com/wd/?next=https://upstream.example/a/",
    ],
    [
      "HTTPS with HTTP query value",
      "https://example.com/wd/?next=http://upstream.example/a/",
      "wss://example.com/wd/?next=http://upstream.example/a/",
    ],
    [
      "IPv6 host",
      "http://[2001:db8::1]:8080/wd/",
      "ws://[2001:db8::1]:8080/wd/",
    ],
    [
      "fragment",
      "http://example.com/wd/?x=1#next=https://fragment.example/",
      "ws://example.com/wd/?x=1#next=https://fragment.example/",
    ],
  ];

  for (const [name, endpoint, expected] of cases) {
    RecordingWebSocket.urls = [];
    const conn = await dial(endpoint, {
      transport: "ws",
      pingIntervalMs: 60_000,
    });
    assert.equal(RecordingWebSocket.urls[0], expected, name);
    assert.equal(conn.url, endpoint, `${name}: public URL changed`);
    await conn.close();
  }

  const calls = [];
  globalThis.fetch = async (resource, init = {}) => {
    calls.push({ url: String(resource), init });
    if (init.method === "POST") return new Response(null, { status: 204 });
    const body = new ReadableStream({
      start(controller) {
        controller.enqueue(
          new TextEncoder().encode("event: sid\ndata: session/id\n\n"),
        );
      },
    });
    return new Response(body, { status: 200 });
  };

  const endpoint =
    "http://example.com/wd/?next=https://upstream.example/a%2Fb&token=a%20b#section";
  const postURL =
    "http://example.com/wd/?next=https://upstream.example/a%2Fb&token=a%20b&s=session%2Fid#section";
  const conn = await dial(endpoint, {
    transport: "sse",
    pingIntervalMs: 60_000,
  });
  assert.equal(conn.url, endpoint);
  assert.equal(calls[0].url, endpoint, "SSE endpoint changed");

  await conn.write("hello");
  assert.equal(calls[1].url, postURL, "session parameter URL is malformed");

  conn.ping();
  await Promise.resolve();
  const pingURL = new URL(calls[2].url);
  assert.equal(pingURL.hash, "#section");
  assert.ok(pingURL.searchParams.has("ping"));
  assert.ok(
    calls[2].url.startsWith(postURL.slice(0, -"#section".length) + "&ping="),
    "ping parameter did not preserve the existing raw query",
  );

  await conn.close();
  assert.equal(
    calls[3].url,
    postURL.slice(0, -"#section".length) + "&close=1#section",
    "close parameter URL is malformed",
  );

  console.log("all URL tests passed");
} finally {
  globalThis.WebSocket = originalWebSocket;
  globalThis.fetch = originalFetch;
}

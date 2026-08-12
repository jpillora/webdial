// The staleness watchdog decides whether an unanswered ping means the peer is
// gone. It has to say yes for a peer that has genuinely stopped, and no for one
// that is simply sending faster than this end can drain — where the pong is
// queued behind the backlog rather than lost. Getting the second case wrong
// tears down connections precisely when they are busiest.
import { spawn } from "node:child_process";
import { strict as assert } from "node:assert";
import { dial } from "./client.mjs";

const PING_MS = 100;
const TIMEOUT_MS = 300;

const server = spawn("go", ["run", "./testdata/silentserver"], {
  cwd: import.meta.dirname,
});
server.stderr.on("data", (d) => process.stderr.write(d));

const url = await new Promise((resolve, reject) => {
  const timer = setTimeout(() => reject(new Error("server start timeout")), 30000);
  let buf = "";
  server.stdout.on("data", (data) => {
    buf += data.toString();
    const line = buf.split("\n")[0].trim();
    if (line.startsWith("http://")) {
      clearTimeout(timer);
      resolve(line);
    }
  });
  server.on("error", (err) => {
    clearTimeout(timer);
    reject(err);
  });
});

const opts = { transport: "ws", pingIntervalMs: PING_MS, pongTimeoutMs: TIMEOUT_MS };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failed = false;

try {
  {
    console.log("test data keeps a pong-less connection alive...");
    const conn = await dial(`${url}/talking`, opts);
    // Read enough to outlive several pong timeouts, in the shape that matters:
    // frames keep arriving and no pong ever does.
    const deadline = Date.now() + TIMEOUT_MS * 5;
    let frames = 0;
    while (Date.now() < deadline) {
      const data = await conn.read();
      assert.notEqual(data, null, `connection closed after ${frames} frames despite data still flowing`);
      frames++;
    }
    assert.ok(frames > 10, `expected a continuous stream, got ${frames} frames`);
    assert.equal(conn.latency, null, "the server under test must never have answered a ping");
    await conn.close();
    console.log(`  pass (${frames} frames, no pong)`);
  }

  {
    console.log("test a peer that says nothing is still closed...");
    const conn = await dial(`${url}/silent`, opts);
    const read = conn.read();
    const closed = await Promise.race([
      read.then(() => "eof"),
      sleep(TIMEOUT_MS * 10).then(() => "timeout"),
    ]);
    assert.equal(closed, "eof", "watchdog never fired for a peer that sent nothing at all");
    console.log("  pass");
  }

  console.log("\nall tests passed");
} catch (err) {
  console.error("\nFAILED:", err);
  failed = true;
} finally {
  server.kill();
  process.exit(failed ? 1 : 0);
}

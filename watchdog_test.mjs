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

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failed = false;

try {
  for (const transport of ["ws", "sse"]) {
    const opts = { transport, pingIntervalMs: PING_MS, pongTimeoutMs: TIMEOUT_MS };

    console.log(`test ${transport} data keeps a pong-less connection alive...`);
    let conn = await dial(`${url}/talking`, opts);
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

    if (transport === "ws") {
      console.log("test ws active traffic suppresses redundant probes...");
      conn = await dial(`${url}/talking?rejectPing=1`, opts);
      const activeDeadline = Date.now() + PING_MS * 5;
      let activeFrames = 0;
      while (Date.now() < activeDeadline) {
        const data = await conn.read();
        assert.notEqual(data, null, `connection closed after redundant ping with ${activeFrames} active frames`);
        activeFrames++;
      }
      assert.ok(activeFrames > 10, `expected continuous active traffic, got ${activeFrames} frames`);
      await conn.close();
      console.log("  pass");
    }

    console.log(`test ${transport} finite data does not disable the watchdog...`);
    conn = await dial(`${url}/burst`, opts);
    const frame = await Promise.race([
      conn.read(),
      sleep(TIMEOUT_MS * 5).then(() => "timeout"),
    ]);
    assert.notEqual(frame, "timeout", "finite data frame never arrived");
    assert.equal(new TextDecoder().decode(frame), "frame");
    const afterBurst = await Promise.race([
      conn.read().then((data) => data === null ? "eof" : "data"),
      sleep(TIMEOUT_MS * 10).then(() => "timeout"),
    ]);
    assert.equal(afterBurst, "eof", "finite data permanently disabled the watchdog");
    console.log("  pass");

    console.log(`test a silent ${transport} peer is still closed...`);
    conn = await dial(`${url}/silent`, opts);
    const silent = await Promise.race([
      conn.read().then((data) => data === null ? "eof" : "data"),
      sleep(TIMEOUT_MS * 10).then(() => "timeout"),
    ]);
    assert.equal(silent, "eof", "watchdog never fired for a peer that sent nothing at all");
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

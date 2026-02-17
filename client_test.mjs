import { spawn } from "node:child_process";
import { strict as assert } from "node:assert";
import { dial } from "./client.mjs";

// Start Go echo server
const server = spawn("go", ["run", "./testdata/echoserver"], {
  cwd: import.meta.dirname,
});

server.stderr.on("data", (d) => process.stderr.write(d));

const url = await new Promise((resolve, reject) => {
  const timeout = setTimeout(() => reject(new Error("server start timeout")), 30000);
  let buf = "";
  server.stdout.on("data", (data) => {
    buf += data.toString();
    const line = buf.split("\n")[0].trim();
    if (line.startsWith("http://")) {
      clearTimeout(timeout);
      resolve(line);
    }
  });
  server.on("error", (err) => {
    clearTimeout(timeout);
    reject(err);
  });
  server.on("exit", (code) => {
    if (!buf.includes("http://")) {
      clearTimeout(timeout);
      reject(new Error(`server exited with code ${code}`));
    }
  });
});

console.log("echo server:", url);
let failed = false;

try {
  // --- WebSocket transport ---
  {
    console.log("test ws text...");
    const conn = await dial(url, { transport: "ws" });
    assert.equal(conn.transport, "ws");
    await conn.write("hello ws");
    const data = await conn.read();
    assert.equal(new TextDecoder().decode(data), "hello ws");
    await conn.close();
    console.log("  pass");
  }

  {
    console.log("test ws binary...");
    const conn = await dial(url, { transport: "ws" });
    await conn.write(new Uint8Array([1, 2, 3, 4, 5]));
    const data = await conn.read();
    assert.deepEqual(new Uint8Array(data), new Uint8Array([1, 2, 3, 4, 5]));
    await conn.close();
    console.log("  pass");
  }

  // --- SSE transport ---
  {
    console.log("test sse text...");
    const conn = await dial(url, { transport: "sse" });
    assert.equal(conn.transport, "sse");
    await conn.write("hello sse");
    const data = await conn.read();
    assert.equal(new TextDecoder().decode(data), "hello sse");
    await conn.close();
    console.log("  pass");
  }

  {
    console.log("test sse binary...");
    const conn = await dial(url, { transport: "sse" });
    await conn.write(new Uint8Array([10, 20, 30]));
    const data = await conn.read();
    assert.deepEqual(new Uint8Array(data), new Uint8Array([10, 20, 30]));
    await conn.close();
    console.log("  pass");
  }

  // --- Auto-detection (should prefer ws) ---
  {
    console.log("test auto-detect...");
    const conn = await dial(url);
    assert.equal(conn.transport, "ws");
    await conn.write("auto");
    const data = await conn.read();
    assert.equal(new TextDecoder().decode(data), "auto");
    await conn.close();
    console.log("  pass");
  }

  // --- Multiple round-trips ---
  {
    console.log("test multiple round-trips (ws)...");
    const conn = await dial(url, { transport: "ws" });
    for (let i = 0; i < 10; i++) {
      const msg = `msg-${i}`;
      await conn.write(msg);
      const data = await conn.read();
      assert.equal(new TextDecoder().decode(data), msg);
    }
    await conn.close();
    console.log("  pass");
  }

  {
    console.log("test multiple round-trips (sse)...");
    const conn = await dial(url, { transport: "sse" });
    for (let i = 0; i < 10; i++) {
      const msg = `msg-${i}`;
      await conn.write(msg);
      const data = await conn.read();
      assert.equal(new TextDecoder().decode(data), msg);
    }
    await conn.close();
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

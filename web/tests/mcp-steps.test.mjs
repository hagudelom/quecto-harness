import assert from "node:assert/strict";
import test from "node:test";
import { createServer } from "vite";

test("Deferred tools show the MCP server during execution and after replay", async (t) => {
  const vite = await createServer({
    configFile: false,
    optimizeDeps: { noDiscovery: true },
    server: { middlewareMode: true, ws: false, watch: null },
  });
  t.after(() => vite.close());
  const { stepName, stepDesc } = await vite.ssrLoadModule("/src/lib/steps.ts");
  const call = { kind: "tool", name: "tool_call", running: true, args: { name: "mcp__notes__find" } };
  assert.equal(stepName(call), "notes");
  assert.equal(stepDesc(call, true, ""), "find");
  const saved = { ...call, running: false, meta: { server: "archive", tool: "find.record" } };
  assert.equal(stepName(saved), "archive");
  assert.equal(stepDesc(saved, false, ""), "find.record");
  const search = { ...call, name: "tool_search", args: { query: "find" } };
  assert.equal(stepName(search), "MCP");
  assert.equal(stepDesc(search, true, ""), "Search");
  assert.equal(stepName({ ...search, meta: { server: "notes", tool: "Search" } }), "notes");
});

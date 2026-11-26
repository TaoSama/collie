import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
const section = (first, next) => {
  const i = app.indexOf(first), end = app.indexOf(next, i + first.length);
  assert.ok(i >= 0 && end > i);
  return app.slice(i, end);
};
const paneURL = (id, kind) => `/api/panes/${encodeURIComponent(id)}/${kind}`;

test("pane composer lets the execution server decide the pane type", async () => {
  const calls = [];
  let send;
  vm.runInNewContext(section('  bindComposer($("#composer")', '  // ───────────────────────────── menus'), {
    $: id => id,
    current: "w1:p1@node",
    bindComposer: (_form, _input, callback) => { send = callback; },
    trackSend: async (_key, _draft, ready, post) => post(await ready),
    paneURL,
    find: () => undefined,
    setLocalState: () => {},
    renderSide: () => {},
    api: async (path, options) => {
      calls.push([path, options?.body?.text, options?.body?.enter]);
      if (path === "/api/state") throw new Error("timeout");
    },
  });
  await send("draft", Promise.resolve("submitted text"));
  assert.deepEqual(calls, [[paneURL("w1:p1@node", "submit"), "submitted text", undefined]]);
});

function directPost(fetch) {
  const hubCalls = [];
  let dropped = 0;
  const context = vm.createContext({
    DIRECT_TIMEOUT_MS: 3000,
    AbortSignal,
    TypeError,
    directOf: () => ({}),
    directURL: (_direct, id, kind) => paneURL(id, kind),
    dropDirect: () => { dropped++; },
    api: async path => { hubCalls.push(path); },
    paneURL,
    fetch,
  });
  vm.runInContext(section("  async function postPane(", "  // ───────────────────────────── one agent:"), context);
  return { send: () => context.postPane("w1:p1@node", "keys", { keys: ["enter"] }), hubCalls, dropped: () => dropped };
}

test("an ambiguous direct write is not sent a second time through the hub", async t => {
  for (const err of [new TypeError("network error"), new DOMException("deadline", "TimeoutError")]) {
    await t.test(err.name, async () => {
      const op = directPost(async () => { throw err; });
      await assert.rejects(op.send(), /确认.*执行/);
      assert.deepEqual(op.hubCalls, []);
      assert.equal(op.dropped(), 1);
    });
  }
});

test("a direct authentication rejection can use the hub", async () => {
  const op = directPost(async () => ({ ok: false, status: 401, json: async () => ({ error: "unauthorized" }) }));
  await op.send();
  assert.deepEqual(op.hubCalls, [paneURL("w1:p1@node", "keys")]);
  assert.equal(op.dropped(), 1);
});

// web/sw.js's push handling, run with stand-ins for the service worker's
// globals: node --test scripts/test/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

function loadSW({ windows = [], maxActions = 2 } = {}) {
  const handlers = {}, shown = [], fetched = [];
  const self = {
    addEventListener: (type, fn) => { handlers[type] = fn; },
    clients: { matchAll: async () => windows, openWindow: async () => {} },
    registration: { showNotification: async (title, opts) => { shown.push({ title, ...opts }); } },
    Notification: { maxActions },
  };
  const ctx = {
    self, Notification: self.Notification, location: { origin: "https://c.example" }, caches: {}, URL, console,
    fetch: async (url, init) => { fetched.push({ url, body: JSON.parse(init.body) }); return { ok: true }; },
  };
  vm.runInNewContext(readFileSync(new URL("../../web/sw.js", import.meta.url), "utf8"), ctx);
  const push = async (payload) => {
    let done;
    handlers.push({ data: { json: () => payload, text: () => JSON.stringify(payload) }, waitUntil: (p) => { done = p; } });
    await done;
  };
  return { push, shown, fetched, ctx };
}

const dialog = { title: "Collie", body: "readme 需要你选：Yes / No", tag: "w1:p1", hash: "#/d/e7", answer: { id: "e7" }, choices: ["Yes", "No"] };

test("a push shows the decision, with buttons that answer it", async () => {
  const sw = loadSW();
  await sw.push(dialog);
  assert.equal(sw.shown.length, 1);
  const n = sw.shown[0];
  assert.equal(n.body, dialog.body);
  assert.equal(n.tag, "w1:p1");
  assert.equal(n.renotify, true);
  assert.deepEqual(n.actions.map((a) => a.title), ["Yes", "No"]);
  assert.equal(n.data.url, "https://c.example/#/d/e7");
  await sw.ctx.notificationClicked(n.data, "1");
  assert.deepEqual(sw.fetched, [{ url: "/api/super/decide", body: { id: "e7", choice: "No" } }]);
});

test("choices that don't all fit open the page instead", async () => {
  const sw = loadSW({ maxActions: 2 });
  await sw.push({ ...dialog, choices: ["A", "B", "C"] });
  assert.equal(sw.shown[0].actions.length, 0);
  assert.equal(sw.shown[0].data.answer, null);
});

test("a page in front of you already shows it", async () => {
  const sw = loadSW({ windows: [{ visibilityState: "visible", focused: true }] });
  await sw.push(dialog);
  assert.equal(sw.shown.length, 0);
  const bg = loadSW({ windows: [{ visibilityState: "hidden", focused: false }] });
  await bg.push(dialog);
  assert.equal(bg.shown.length, 1);
});

test("a bad payload still shows something (Chrome wants every push shown)", async () => {
  const sw = loadSW();
  await sw.push({});
  assert.equal(sw.shown.length, 1);
  assert.equal(sw.shown[0].title, "Collie");
  assert.equal(sw.shown[0].renotify, false);
});

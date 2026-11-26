import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
function section(from, to) {
  const start = app.indexOf(from), end = app.indexOf(to, start);
  assert.ok(start >= 0 && end > start, "history functions are in app.js");
  return app.slice(start, end);
}

function fixture() {
  const requests = [], toasts = [];
  let redraws = 0, scrollWrites = 0;
  const el = {
    scrollHeight: 400,
    get scrollTop() { return 20; },
    set scrollTop(_) { scrollWrites++; },
    _redraw() { redraws++; },
  };
  const history = vm.runInNewContext([
    section("  function applyPatch(", "  // What you sent shows"),
    section("  const earlierCursor =", "  // Any conversation scrolled"),
    "({ resetEarlier, applyPatch, loadEarlier, threadMsgs })",
  ].join("\n"), {
    api(path) {
      return new Promise((resolve, reject) => requests.push({ path, resolve, reject }));
    },
    scroller: () => el,
    toast: (message) => toasts.push(message),
  });
  const urlA = (cursor) => "/A/" + cursor;
  const urlB = (cursor) => "/B/" + cursor;
  const snapshot = (latest) => history.applyPatch(el, { base: 0, from: 0, msgs: [latest], earlier: 789 });
  const enter = (url, latest) => { history.resetEarlier(el, url); snapshot(latest); };
  const visible = () => Array.from(history.threadMsgs(el));
  const counts = () => ({ redraws, scrollWrites });
  const load = () => history.loadEarlier(el);
  return { urlA, urlB, snapshot, enter, visible, counts, load, requests, toasts };
}

test("a late page cannot join another session with the same cursor", async () => {
  const f = fixture();
  f.enter(f.urlA, "A latest");
  const pending = f.load();
  f.enter(f.urlB, "B latest");
  const before = f.counts();
  assert.equal(f.requests[0].path, "/A/789");
  f.requests[0].resolve({ messages: ["A earlier"], earlier: 321 });
  await pending;
  assert.deepEqual(f.visible(), ["B latest"]);
  assert.deepEqual(f.counts(), before);
});

test("A can page again after A → B → A while its first request is still in flight", async () => {
  const f = fixture();
  f.enter(f.urlA, "A latest");
  const first = f.load();
  f.enter(f.urlB, "B latest");
  f.enter(f.urlA, "A latest again");
  const second = f.load();
  assert.deepEqual(f.requests.map((r) => r.path), ["/A/789", "/A/789"]);
  const before = f.counts();
  f.requests[0].resolve({ messages: ["not current"], earlier: 321 });
  await first;
  assert.deepEqual(f.visible(), ["A latest again"]);
  assert.deepEqual(f.counts(), before);
  const duplicate = f.load();
  assert.equal(f.requests.length, 2, "the first page's completion did not unlock the second");
  await duplicate;
  f.requests[1].resolve({ messages: ["current earlier"], earlier: 321 });
  await second;
  assert.deepEqual(f.visible(), ["current earlier", "A latest again"]);
  assert.equal(f.counts().scrollWrites, before.scrollWrites + 1);
});

test("a stale failure stays silent and does not redraw or scroll the new view", async () => {
  const f = fixture();
  f.enter(f.urlA, "A latest");
  const pending = f.load();
  f.enter(f.urlB, "B latest");
  const before = f.counts();
  f.requests[0].reject(new Error("A failed to load"));
  await pending;
  assert.deepEqual(f.toasts, []);
  assert.deepEqual(f.counts(), before);
  const next = f.load();
  assert.equal(f.requests[1].path, "/B/789");
  f.requests[1].reject(new Error("B failed to load"));
  await next;
  assert.deepEqual(f.toasts, ["B failed to load"]);
});

test("a replacement snapshot invalidates an in-flight page even when the cursor is reused", async () => {
  const f = fixture();
  f.enter(f.urlA, "first log");
  const pending = f.load();
  f.snapshot("replacement log");
  const before = f.counts();
  f.requests[0].resolve({ messages: ["first log earlier"], earlier: 321 });
  await pending;
  assert.deepEqual(f.visible(), ["replacement log"]);
  assert.deepEqual(f.counts(), before);
});

// web/threads.js: which messages make a run of replies, and what the main
// chat folds. node --test scripts/test/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {} };
vm.runInNewContext(readFileSync(new URL("../../web/threads.js", import.meta.url), "utf8"), ctx);
const { group } = ctx.window.collieThreads;
const key = (e) => e.id || e._k;
// The script runs in its own realm: compare plain copies.
const ids = (list) => Array.from(list, (e) => (typeof e === "string" ? e : e.id));
const say = (id, role, re, extra = {}) => ({ id, role, text: id, ...(re ? { re: { entry: re, who: "x", text: "quoted " + re } } : {}), ...extra });

test("two messages are no run; a third makes one", () => {
  const a = say("e1", "user"), b = say("e2", "collie", "e1");
  assert.equal(group([a, b], key).threads.size, 0);
  const c = say("e3", "user", "e2");
  const g = group([a, b, c], key);
  const t = g.threads.get("e1");
  assert.deepEqual(ids(t.items), ["e2", "e3"]);
  assert.equal(t.latest.id, "e3");
  assert.deepEqual(ids(g.folded), ["e2"]);
  assert.equal(g.lastOf.get("e3"), "e1");
});

test("other talk in between stays out; branches join the same run", () => {
  const list = [say("e1", "user"), say("e2", "collie", "e1"), say("e3", "user"), say("e4", "user", "e1"), say("e5", "collie", "e2"), { id: "e6", role: "event", text: "w 完成了一轮" }];
  const g = group(list, key);
  assert.equal(g.threads.size, 1);
  assert.deepEqual(ids(g.threads.get("e1").items), ["e2", "e4", "e5"]);
  assert.ok(!g.member.has("e3") && !g.member.has("e6"));
  assert.deepEqual(ids(g.folded).sort(), ["e2", "e4"]);
});

test("a quote about a worker or an event is no reply", () => {
  const list = [say("e1", "collie"), { id: "e2", role: "event", text: "w" }, say("e3", "collie", "e2"), say("e4", "user", "e3"),
    { id: "e5", role: "collie", text: "", re: { worker: "w", entry: "e1", who: "w", text: "" } }];
  assert.equal(group(list, key).threads.size, 0);
});

test("a first message too early to be loaded counts, from its quote", () => {
  const g = group([say("e8", "collie", "e1"), say("e9", "user", "e8")], key);
  const t = g.threads.get("e1");
  assert.equal(t.root.stub, true);
  assert.equal(t.root.text, "quoted e1");
  assert.deepEqual(ids(g.folded), ["e8"]);
});

test("a message on its way joins by its stand-in key", () => {
  const sending = { id: "", _k: "l1", role: "user", text: "还有", re: { entry: "e3", who: "Collie", text: "e3" }, pending: true };
  const g = group([say("e1", "user"), say("e2", "collie", "e1"), say("e3", "collie", "e2"), sending], key);
  assert.equal(g.threads.get("e1").latest, sending);
  assert.deepEqual(ids(g.folded).sort(), ["e2", "e3"]);
});

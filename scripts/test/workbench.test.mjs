// web/workbench.js: how the workbench orders its groups and modules, and
// names new ones. node --test scripts/test/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {}, URL };
vm.runInNewContext(readFileSync(new URL("../../web/workbench.js", import.meta.url), "utf8"), ctx);
const { arrange, freeId, shift, LINKS } = ctx.window.collieWorkbench;
const plain = (v) => JSON.parse(JSON.stringify(v));

const cfg = () => ({
  groups: [{ id: "fav", title: "收藏" }, { id: "life", title: "生活" }],
  modules: [
    { id: "a", group: "fav" }, { id: "books", group: "life" }, { id: "b", group: "fav" }, { id: "lost", group: "gone" },
  ],
});

test("arrange keeps group order, module order, and puts orphans first", () => {
  const g = plain(arrange(cfg()));
  assert.deepEqual(g.map((x) => x.id), ["fav", "life"]);
  assert.deepEqual(g[0].modules.map((m) => m.id), ["a", "b", "lost"]);
  assert.deepEqual(g[1].modules.map((m) => m.id), ["books"]);
  assert.deepEqual(plain(arrange({ groups: [], modules: [{ id: "x" }] })), []);
});

test("freeId comes from the host or the title and never repeats", () => {
  assert.equal(freeId("https://books.example.com/x", new Set()), "books");
  assert.equal(freeId("https://books.example.com/", new Set(["books", "books-2"])), "books-3");
  assert.equal(freeId("Homelab", new Set(), "g"), "homelab");
  assert.equal(freeId("读书", new Set(), "g"), "g");
  assert.equal(freeId("读书", new Set(["g"]), "g"), "g-2");
});

test("shift moves a module within its group only", () => {
  const ids = (c) => c.modules.map((m) => m.id);
  assert.deepEqual(ids(shift(cfg(), "b", -1)), ["b", "books", "a", "lost"]);
  assert.deepEqual(ids(shift(cfg(), "a", -1)), ["a", "books", "b", "lost"]); // first stays
  assert.deepEqual(ids(shift(cfg(), "books", 1)), ["a", "books", "b", "lost"]); // alone in its group
});

test("shortcut targets match the hub's list", () => {
  const go = readFileSync(new URL("../../internal/collie/workbench.go", import.meta.url), "utf8");
  const hub = [...go.matchAll(/"((?:auto|devices):[a-z]+)": true/g)].map((m) => m[1]).sort();
  assert.deepEqual(Object.keys(LINKS).sort(), hub);
});

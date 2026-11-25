// web/machines.js: which computers a picker offers, in what order, and how
// each is drawn. node --test scripts/test/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {} };
vm.runInNewContext(readFileSync(new URL("../../web/machines.js", import.meta.url), "utf8"), ctx);
const { order, choices, options } = ctx.window.collieMachines;
const ids = (list) => Array.from(list, (m) => m.id);

const hub = { id: "", name: "vps", local: true, online: false, agents: [] };
const mini = { id: "mini", name: "macmini", os: "darwin/arm64", online: true, agents: ["claude"] };
const box = { id: "box", name: "devbox", os: "linux/amd64", online: true, agents: ["codex"] };
const air = { id: "air", name: "macair", os: "darwin/arm64", online: false, agents: ["claude"] };
const far = { id: "x@h2", name: "h2 · x", hub: "h2", online: true, agents: ["claude"] };

test("by name, the hub last without an agent CLI and first with one", () => {
  assert.deepEqual(ids(order([hub, mini, box])), ["box", "mini", ""]);
  assert.deepEqual(ids(order([mini, { ...hub, agents: ["claude"] }, box])), ["", "box", "mini"]);
});

test("offers this hub's computers that are up, and the picked one even when not", () => {
  const all = [hub, mini, box, air, far];
  assert.deepEqual(ids(choices(all)), ["box", "mini", ""]);
  assert.deepEqual(ids(choices(all, { keep: "air" })), ["box", "air", "mini", ""]);
  assert.deepEqual(ids(choices(all, { others: true })), ["box", "x@h2", "mini", ""]);
  assert.deepEqual(ids(choices(all, { isUp: (m) => m.online })), ["box", "mini"]);
});

test("each option: a dot, hub or the OS, offline, and the default", () => {
  const html = options([hub, air, mini], { cur: "air", def: "mini" });
  assert.match(html, /<option value="" data-dot="online" data-sub="hub">vps<\/option>/);
  assert.match(html, /<option value="air" data-dot="offline" data-sub="darwin\/arm64 · 离线" selected>macair<\/option>/);
  assert.match(html, /<option value="mini" data-dot="online" data-sub="darwin\/arm64 · 默认">macmini<\/option>/);
  assert.match(options([{ id: "q", name: "q", online: true }], { def: "q" }), /data-sub="默认"/);
});

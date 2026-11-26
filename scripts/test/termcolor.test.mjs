// web/termcolor.js: a dark-terminal agent's bars on a light skin.
// node --test scripts/test/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {} };
vm.runInNewContext(readFileSync(new URL("../../web/termcolor.js", import.meta.url), "utf8"), ctx);
const { adapt } = ctx.window.collieTermColor;
const plain = (s) => s.replace(/\x1b\[[0-9;]*m/g, "");
const bgOf = (s) => [...s.matchAll(/48;2;(\d+);(\d+);(\d+)/g)].map((m) => m.slice(1).map(Number));

// Codex 0.172's input bar and an earlier prompt's bar, as herdr reads them.
const row = "\x1b[0m\x1b[1m\x1b[48;2;65;69;76m›\x1b[0m\x1b[48;2;65;69;76m \x1b[0m\x1b[2m\x1b[48;2;65;69;76mAsk Codex to do anything\x1b[0m";
const prompt = "\x1b[0m\x1b[1m\x1b[2m\x1b[48;2;74;77;84m› \x1b[0m\x1b[48;2;74;77;84mRun it\x1b[0m";

test("on a light skin, dark bars turn light, in the same order, text untouched", () => {
  const a = adapt(row, "#ffffff", "#1a1c1f"), b = adapt(prompt, "#ffffff", "#1a1c1f");
  assert.equal(plain(a), plain(row));
  assert.equal(plain(b), plain(prompt));
  const [bar] = bgOf(a), [old] = bgOf(b);
  for (const v of bar) assert.ok(v > 200, `light: ${bar}`);
  assert.ok(old[0] < bar[0], "the lighter bar stays the lighter one");
  assert.equal(a.replace(/48;2;\d+;\d+;\d+/g, ""), row.replace(/48;2;\d+;\d+;\d+/g, ""), "only the background numbers change");
});

test("dark skins, light backgrounds and every other colour are left alone", () => {
  assert.equal(adapt(row, "#181818", "#dfdfdf"), row);
  const light = "\x1b[48;2;240;240;240mx\x1b[0m";
  assert.equal(adapt(light, "#ffffff", "#1a1c1f"), light);
  const fgs = "\x1b[38;2;10;10;10;48;5;0mx\x1b[38;5;48;2mx";
  assert.equal(adapt(fgs, "#ffffff", "#1a1c1f"), fgs, "foregrounds and 256 colours aren't backgrounds");
  assert.equal(adapt(row, "rgba(0,0,0,0)", "#1a1c1f"), row, "no colour to work from");
});

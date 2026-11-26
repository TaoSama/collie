// web/wheel.js: the wheel reports screen mode sends a fullscreen agent, and
// turning its view back down on leaving. node --test scripts/test/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {} };
vm.runInNewContext(readFileSync(new URL("../../web/wheel.js", import.meta.url), "utf8"), ctx);
const { report, tracker } = ctx.window.collieWheel;
const count = (text, button) => text.split(`\x1b[<${button};`).length - 1;

test("one SGR wheel report per line, at the middle of the pane", () => {
  assert.equal(report(-2, 120, 40), "\x1b[<64;60;20M\x1b[<64;60;20M");
  assert.equal(report(1, 0, 0), "\x1b[<65;1;1M");
});

test("a view scrolled up goes back down with room to spare, once", () => {
  const w = tracker();
  w.scrolled("w1:p1", -30, 120, 40);
  w.scrolled("w1:p1", 10, 120, 40);
  const home = w.home();
  assert.equal(home.pane, "w1:p1");
  assert.ok(count(home.text, 65) >= 2 * 20, "more than the way up, which an agent may speed up");
  assert.equal(count(home.text, 64), 0);
  assert.ok(home.text.endsWith("\x1b[1;5F"), "and ctrl+End, which Claude Code jumps to the bottom on");
  assert.equal(w.home(), null);
});

test("nothing to send for a view that is back at the bottom, or never scrolled", () => {
  const w = tracker();
  assert.equal(w.home(), null);
  w.scrolled("w1:p1", -5, 80, 24);
  w.scrolled("w1:p1", 50, 80, 24); // down past where it started: still the bottom
  assert.equal(w.home(), null);
  w.scrolled("w1:p1", 5, 80, 24);
  assert.equal(w.home(), null);
});

test("only the pane scrolled last, and not one that is gone", () => {
  const w = tracker();
  w.scrolled("w1:p1", -5, 80, 24);
  w.scrolled("w1:p2", -3, 80, 24);
  assert.equal(w.home().pane, "w1:p2");
  w.scrolled("w1:p1", -5, 80, 24);
  w.forget();
  assert.equal(w.home(), null);
});

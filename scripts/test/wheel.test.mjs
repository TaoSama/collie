// web/wheel.js: the wheel reports screen mode sends a fullscreen agent, and
// turning its view back down on leaving. node --test scripts/test/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {} };
vm.runInNewContext(readFileSync(new URL("../../web/wheel.js", import.meta.url), "utf8"), ctx);
const { report, tracker, sender, BURST } = ctx.window.collieWheel;
const count = (text, button) => text.split(`\x1b[<${button};`).length - 1;

test("one SGR wheel report per line, at the middle of the pane", () => {
  assert.equal(report(-2, 120, 40), "\x1b[<64;60;20M\x1b[<64;60;20M");
  assert.equal(report(1, 0, 0), "\x1b[<65;1;1M");
});

// An ESC that reaches an agent with nothing after it is the Esc key: it stops
// the agent's turn. A terminal cuts long input into pieces (1 KB on macOS),
// so nothing here may grow long.
const MAX_SEND = 160;

test("a view scrolled up goes back with a few short keys, however far up, once", () => {
  const w = tracker();
  w.scrolled("w1:p1", -3000);
  w.scrolled("w1:p1", 10);
  const home = w.home();
  assert.equal(home.pane, "w1:p1");
  assert.ok(home.text.startsWith("\x1b[1;5F"), "ctrl+End: Claude Code's and Codex's jump to the bottom");
  assert.ok(home.text.includes("\x1b[6~"), "PageDown: agy's way down");
  assert.ok(home.text.length < MAX_SEND, `short: ${home.text.length} bytes`);
  assert.equal(count(home.text, 64) + count(home.text, 65), 0);
  assert.equal(w.home(), null);
});

test("a fast flick goes out a few lines per send, one send at a time, in order", async () => {
  const sent = [];
  let release;
  const s = sender((pane, text) => { sent.push({ pane, text }); return new Promise((r) => { release = r; }); });
  s.scroll("w1:p1", -50, 238, 50); // 13-byte reports
  s.scroll("w1:p1", -30, 238, 50);
  assert.equal(sent.length, 1, "the rest waits for the first send");
  for (let i = 0; i < 40 && release; i++) { const r = release; release = null; r(); await new Promise((d) => setTimeout(d, 0)); }
  const lines = sent.reduce((n, x) => n + count(x.text, 64), 0);
  assert.equal(lines, 80, "no line lost");
  for (const x of sent) {
    assert.ok(count(x.text, 64) <= BURST && x.text.length < MAX_SEND, `short sends: ${x.text.length} bytes`);
    assert.equal(x.pane, "w1:p1");
  }
});

test("scrolling back cancels lines not sent yet; leaving drops them", async () => {
  const sent = [];
  let release;
  const s = sender((pane, text) => { sent.push(text); return new Promise((r) => { release = r; }); });
  const tick = async () => { const r = release; release = null; r?.(); await new Promise((d) => setTimeout(d, 0)); };
  s.scroll("w1:p1", -BURST, 120, 40); // out at once
  s.scroll("w1:p1", -4, 120, 40); // owed while that send is out...
  s.scroll("w1:p1", 4, 120, 40); // ...and taken back
  await tick();
  assert.equal(sent.length, 1, "nothing owed after netting out");
  s.scroll("w1:p1", -20, 120, 40);
  s.drop();
  await tick();
  assert.equal(sent.length, 2, "nothing after leaving");
});

test("drop detaches a stalled in-flight send so the next pane scrolls immediately", async () => {
  const sent = [];
  let releaseStuck;
  const s = sender((pane, text) => {
    sent.push(pane);
    if (pane === "w1:p1") return new Promise((r) => { releaseStuck = r; });
    return Promise.resolve();
  });
  s.scroll("w1:p1", -4, 120, 40);
  assert.deepEqual(sent, ["w1:p1"]);
  s.drop(); // leave w1:p1 while its request is still stalled
  s.scroll("w2:p1", -4, 120, 40);
  assert.deepEqual(sent, ["w1:p1", "w2:p1"], "new pane must not wait behind stalled old pane");
  releaseStuck?.();
  await new Promise((d) => setTimeout(d, 0));
});

test("nothing to send for a view that is back at the bottom, or never scrolled", () => {
  const w = tracker();
  assert.equal(w.home(), null);
  w.scrolled("w1:p1", -5);
  w.scrolled("w1:p1", 50); // down past where it started: still the bottom
  assert.equal(w.home(), null);
  w.scrolled("w1:p1", 5);
  assert.equal(w.home(), null);
});

test("only the pane scrolled last, and not one that is gone", () => {
  const w = tracker();
  w.scrolled("w1:p1", -5);
  w.scrolled("w1:p2", -3);
  assert.equal(w.home().pane, "w1:p2");
  w.scrolled("w1:p1", -5);
  w.forget();
  assert.equal(w.home(), null);
});

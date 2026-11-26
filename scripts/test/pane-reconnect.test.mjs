import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
function section(from, to) {
  const start = app.indexOf(from), end = app.indexOf(to, start);
  assert.ok(start >= 0 && end > start);
  return app.slice(start, end);
}
const msg = (text) => ({ kind: "assistant", text });

function fixture({ mode = "chat", random = 0.5, loadPage = async () => ({ messages: [msg("older")], earlier: 0 }) } = {}) {
  const sources = [], timers = new Map(), shown = [], events = new EventTarget();
  const document = Object.assign(new EventTarget(), { hidden: false });
  const nodes = { "#pane-reconnect": { textContent: "" }, "#thread": {}, "#gone": { hidden: true } };
  let selector = 0;
  class Source extends EventTarget {
    constructor(url) { super(); this.url = url; sources.push(this); }
    close() { this.closed = true; }
    emit(type, data) {
      this.dispatchEvent(Object.assign(new Event(type), { data: JSON.stringify(data) }));
    }
  }
  const runtime = vm.runInNewContext([
    "let stream = null, current = 'A', term = null, view = 'pane';",
    section("  const PANE_RETRY_START_MS", "  function drawScreen("),
    section("  function openChat(", "  // Threads on a"),
    section("  function applyPatch(", "  // What you sent shows"),
    section("  const earlierCursor =", "  // Any conversation scrolled"),
    "({ open: () => openStream(), load: () => loadEarlier($('#thread')), switchTo(id) { closeStream(); resetChat(); current = id; openStream(); } })",
  ].join("\n"), {
    $: (s) => nodes[s], document, addEventListener: events.addEventListener.bind(events),
    navigator: { onLine: true }, Math: Object.assign(Object.create(Math), { random: () => random }),
    curMode: () => mode, applyMode() {}, ensureTerm() {}, primeThread() {}, keepThread() {},
    withSending: (_, msgs) => msgs,
    directOf: () => null, probeDirect: async () => null,
    openES: (url) => new Source(url), paneURL: (id, part) => `/${id}/${part}`,
    wheelOut: { drop() {} }, wheelUp: { home() {}, forget() {} },
    drawThread: (msgs) => shown.push(Array.from(msgs, (m) => m.text)),
    drawLog: (log) => shown.push(log), drawScreen: (f) => shown.push(f.text),
    drawLive() {}, drawFallback: (log) => shown.push(log),
    resetChat() { nodes["#thread"]._msgs = null; shown.push([]); },
    scroller: () => nodes["#thread"], toast() {},
    api: loadPage,
    setTimeout(callback, delay) { timers.set(++selector, { callback, delay }); return selector; },
    clearTimeout: (id) => timers.delete(id),
  });
  const runTimer = () => {
    const [[id, timer]] = timers;
    timers.delete(id);
    timer.callback();
    return timer.delay;
  };
  runtime.open();
  return { runtime, sources, timers, shown, nodes, events, document, runTimer };
}

test("chat retries a 503 with increasing jittered backoff and clears status on a usable frame", () => {
  const f = fixture({ random: 0 });
  f.sources[0].emit("error");
  assert.equal(f.sources[0].closed, true);
  assert.equal(f.nodes["#pane-reconnect"].textContent, "重连中");
  const first = f.runTimer();
  f.sources[1].emit("error");
  const second = f.runTimer();
  assert.ok(first > 0 && second > first);
  f.sources[2].emit("open");
  assert.equal(f.nodes["#pane-reconnect"].textContent, "重连中");
  f.sources[2].emit("patch", { base: 0, from: 0, msgs: [msg("recovered")] });
  assert.equal(f.nodes["#pane-reconnect"].textContent, "");
  assert.deepEqual(f.shown.at(-1), ["recovered"]);
  f.sources[2].emit("error");
  assert.equal(f.runTimer(), first);
  const other = fixture({ random: 0.99 });
  other.sources[0].emit("error");
  assert.ok(other.runTimer() > first);
});

test("a reconnect reset waits for the new chat snapshot and preserves paged history", async () => {
  const f = fixture();
  f.sources[0].emit("patch", { base: 10, from: 10, earlier: 100, msgs: [msg("one"), msg("two")] });
  await f.runtime.load();
  f.sources[0].emit("error");
  f.runTimer();
  const fresh = f.sources[1];
  fresh.emit("reset");
  fresh.emit("meta", { structured: true });
  assert.deepEqual(f.shown.at(-1), ["older", "one", "two"]);
  fresh.emit("patch", { base: 0, from: 0, earlier: 200, msgs: [msg("two"), msg("three")] });
  assert.deepEqual(f.shown.at(-1), ["older", "one", "two", "three"]);
});

test("an in-flight earlier page keeps its cursor across a shifted reconnect snapshot", async () => {
  let finish;
  const f = fixture({ loadPage: () => new Promise((resolve) => { finish = resolve; }) });
  f.sources[0].emit("patch", { base: 10, from: 10, earlier: 100, msgs: [msg("one"), msg("two")] });
  const page = f.runtime.load();
  f.sources[0].emit("error");
  f.runTimer();
  f.sources[1].emit("reset");
  f.sources[1].emit("patch", { base: 0, from: 0, earlier: 200, msgs: [msg("two"), msg("three")] });
  finish({ messages: [msg("page in flight")], earlier: 0 });
  await page;
  assert.deepEqual(f.shown.at(-1), ["page in flight", "one", "two", "three"]);
});

test("an unrelated replacement transcript still discards the prior history", async () => {
  const f = fixture();
  f.sources[0].emit("patch", { base: 10, from: 10, earlier: 100, msgs: [msg("before")] });
  await f.runtime.load();
  f.sources[0].emit("error");
  f.runTimer();
  f.sources[1].emit("reset");
  f.sources[1].emit("patch", { base: 10, from: 10, earlier: 100, msgs: [msg("replacement")] });
  assert.deepEqual(f.shown.at(-1), ["replacement"]);
});

test("screen and log keep their frame through a disconnect and wake immediately when visible or online", () => {
  for (const [mode, signal] of [["screen", "visibilitychange"], ["log", "online"]]) {
    const f = fixture({ mode });
    f.sources[0].emit("frame", { text: "last terminal frame" });
    f.sources[0].emit("error");
    assert.deepEqual(f.shown, ["last terminal frame"]);
    f.document.hidden = true;
    f.document.dispatchEvent(new Event("visibilitychange"));
    assert.equal(f.sources.length, 1);
    f.document.hidden = false;
    (signal === "online" ? f.events : f.document).dispatchEvent(new Event(signal));
    assert.equal(f.sources.length, 2);
    assert.equal(f.timers.size, 0);
    f.sources[0].emit("frame", { text: "late" });
    f.sources[0].emit("gone");
    assert.equal(f.nodes["#gone"].hidden, true);
    assert.deepEqual(f.shown, ["last terminal frame"]);
    f.sources[1].emit("frame", { text: "new terminal frame" });
    assert.deepEqual(f.shown, ["last terminal frame", "new terminal frame"]);
    assert.equal(f.nodes["#pane-reconnect"].textContent, "");
  }
});

test("rapid A → B → A cancels retries and discards stale events even on the same pane", () => {
  const f = fixture();
  f.sources[0].emit("error");
  f.runtime.switchTo("B");
  f.runtime.switchTo("A");
  f.sources[2].emit("patch", { base: 0, from: 0, msgs: [msg("active")] });
  f.sources[0].emit("reset");
  f.sources[0].emit("patch", { base: 0, from: 0, msgs: [msg("stale")] });
  f.sources[1].emit("gone");
  assert.deepEqual(f.shown.at(-1), ["active"]);
  assert.equal(f.nodes["#gone"].hidden, true);
  assert.equal(f.timers.size, 0);
});

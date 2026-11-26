import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
function section(from, to) {
  const start = app.indexOf(from), end = app.indexOf(to, start);
  assert.ok(start >= 0 && end > start, `missing section ${from} .. ${to}`);
  return app.slice(start, end);
}

function harness({
  clientWidth = 384,
  clientHeight = 580,
  padX = 26,
  padY = 16,
  cols = 120,
  rows = 40,
  wheel = true,
  narrow = true,
  charRatio = 0.602,
  lineRatio = 1.24,
  lineHeight = 1.15,
  mode = "screen",
  view = "pane",
  current = "w1:p1",
} = {}) {
  const mem = new Map();
  let curModeVal = mode;
  let screenWheelVal = wheel;
  let viewVal = view;
  let currentVal = current;
  let hiddenVal = false;
  const size = { cols, rows };
  const btn = { textContent: "", hidden: false, listeners: {}, addEventListener(t, fn) { this.listeners[t] = fn; } };
  const kbd = { addEventListener() {} };
  const rootClasses = new Set();
  const viewerClasses = new Set();
  const viewer = { classList: { contains: (c) => viewerClasses.has(c) } };
  const dims = { css: { cell: { width: 0, height: 0 } } };
  const el = {
    clientWidth,
    clientHeight,
    scrollTop: 0,
    scrollLeft: 0,
    get scrollWidth() { return Math.max(this.clientWidth, Math.ceil(padX + dims.css.cell.width * size.cols)); },
    get scrollHeight() { return Math.max(this.clientHeight, Math.ceil(padY + dims.css.cell.height * size.rows)); },
    querySelector() { return null; },
  };
  const term = {
    _fs: 13,
    options: {
      lineHeight,
      get fontSize() { return term._fs; },
      set fontSize(v) {
        term._fs = v;
        dims.css.cell.width = v * charRatio;
        dims.css.cell.height = v * lineHeight * lineRatio;
      },
    },
    _core: { _renderService: { dimensions: dims } },
  };
  term.options.fontSize = 13;

  const winListeners = {};
  const posts = [];
  const timers = new Map();
  let nextTimer = 1;
  const narrowMQ = { matches: narrow };
  const ctx = {
    $: (s) => (s === "#screen" ? el : s === "#term-fit" ? btn : s === "#viewer" ? viewer : kbd),
    document: {
      get hidden() { return hiddenVal; },
      documentElement: { classList: { contains: (c) => rootClasses.has(c) } },
    },
    matchMedia: () => narrowMQ,
    getComputedStyle: () => ({ paddingLeft: String(padX / 2), paddingRight: String(padX / 2), paddingTop: String(padY / 2), paddingBottom: String(padY / 2) }),
    store: {
      get: (k, d) => (mem.has(k) ? mem.get(k) : d),
      set: (k, v) => mem.set(k, v),
    },
    term,
    size,
    get view() { return viewVal; },
    get current() { return currentVal; },
    get screenWheel() { return screenWheelVal; },
    set screenWheel(v) { screenWheelVal = v; },
    curMode: () => curModeVal,
    screenNearBottom: () => true,
    hideJump() {},
    updateScreenScrollbar() {},
    requestAnimationFrame(fn) { fn(); },
    addEventListener(t, fn) { winListeners[t] = fn; },
    setTimeout(fn, ms) { const id = nextTimer++; timers.set(id, { fn, ms }); return id; },
    clearTimeout(id) { timers.delete(id); },
    postPane(pane, kind, body, opts = {}) {
      posts.push({ pane, kind, body, opts });
      return Promise.resolve({ ok: true, ...body });
    },
  };
  const src = section("  const narrowScreen =", "  const PANE_RETRY_START_MS");
  const api = vm.runInNewContext(
    src + "\n({ fitTerm, scheduleScreenResize, restoreScreenSize, noteScreenActivity, computeScreenResizeTarget })",
    ctx,
  );
  return {
    fit: () => api.fitTerm(),
    sync: (delay = 0) => api.scheduleScreenResize(delay),
    restore: (keepalive = false) => api.restoreScreenSize(keepalive),
    activity: () => api.noteScreenActivity(),
    target: () => api.computeScreenResizeTarget(),
    clickToggle: () => btn.listeners.click(),
    triggerResize: () => winListeners.resize(),
    triggerPageHide: () => winListeners.pagehide(),
    flushTimers(filterMs = null) {
      for (const [id, t] of [...timers.entries()]) {
        if (filterMs !== null && t.ms !== filterMs) continue;
        timers.delete(id);
        t.fn();
      }
    },
    setViewport(w, h, isNarrow = w <= 760) {
      el.clientWidth = w;
      el.clientHeight = h;
      narrowMQ.matches = isNarrow;
    },
    setPane(c, r, isWheel = true) {
      size.cols = c;
      size.rows = r;
      screenWheelVal = isWheel;
    },
    setMode(m) { curModeVal = m; },
    setHidden(h) { hiddenVal = h; },
    setKeyboard(on, viaTyping = false) {
      rootClasses.clear();
      viewerClasses.clear();
      if (on) (viaTyping ? viewerClasses : rootClasses).add(viaTyping ? "typing" : "kb-open");
    },
    term, el, btn, dims, posts, timers,
  };
}

test("folded phone requests PTY resize at 12px and renders reflowed pane without clipping", () => {
  const h = harness({ clientWidth: 384, clientHeight: 580, cols: 120, rows: 40, wheel: true, narrow: true });
  h.sync(0);
  assert.equal(h.posts.length, 1);
  assert.equal(h.posts[0].kind, "resize");
  const { cols, rows } = h.posts[0].body;
  assert.ok(cols >= 45 && cols <= 52, `expected ~49 cols on 384px, got ${cols}`);
  assert.ok(rows >= 28 && rows <= 36, `expected ~32 rows on 580px, got ${rows}`);

  // Once herdr streams the reflowed pane, font stays readable (12px) with 0 overflow.
  h.setPane(cols, rows, true);
  h.fit();
  assert.equal(h.term.options.fontSize, 12);
  assert.ok(h.el.scrollWidth <= h.el.clientWidth, `scrollWidth ${h.el.scrollWidth} > ${h.el.clientWidth}`);
  assert.ok(h.el.scrollHeight <= h.el.clientHeight, `scrollHeight ${h.el.scrollHeight} > ${h.el.clientHeight}`);
  assert.equal(h.btn.hidden, true);
});

test("unfolded foldable requests PTY resize at 13px and live fold/unfold updates cols", () => {
  const h = harness({ clientWidth: 675, clientHeight: 680, cols: 120, rows: 40, wheel: true, narrow: true });
  h.sync(0);
  assert.equal(h.posts.length, 1);
  const unfolded = h.posts[0].body;
  assert.ok(unfolded.cols >= 78 && unfolded.cols <= 86, `expected ~82 cols on 675px, got ${unfolded.cols}`);
  h.setPane(unfolded.cols, unfolded.rows, true);
  h.fit();
  assert.equal(h.term.options.fontSize, 13);
  assert.ok(h.el.scrollWidth <= h.el.clientWidth);

  // Fold back to 384px -> debounced resize shrinks cols cleanly.
  h.setViewport(384, 580, true);
  h.triggerResize();
  h.flushTimers(120);
  assert.equal(h.posts.length, 2);
  assert.ok(h.posts[1].body.cols < unfolded.cols - 20);
});

test("keyboard popup shrinking viewport height does not shrink PTY rows", () => {
  const h = harness({ clientWidth: 384, clientHeight: 580, cols: 120, rows: 40, wheel: true, narrow: true });
  h.sync(0);
  const initial = h.posts[0].body;

  // Virtual keyboard pops up (iOS kb-open or Android typing focus), cutting height in half.
  h.setKeyboard(true, true);
  h.setViewport(384, 290, true);
  h.triggerResize();
  h.flushTimers(120);
  assert.equal(h.posts.length, 1, "keyboard height shrink must not trigger a PTY resize");

  // Even without typing class, a pure vertical drop at the same width preserves rows.
  h.setKeyboard(false);
  h.setViewport(384, 300, true);
  assert.equal(h.target().rows, initial.rows);
});

test("leaving narrow screen, pagehide, and idle timeout restore original PTY size", () => {
  const h = harness({ clientWidth: 384, clientHeight: 580, cols: 120, rows: 40, wheel: true, narrow: true });
  h.sync(0);
  assert.equal(h.posts.length, 1);

  // Stretching to desktop (> 760px) immediately restores.
  h.setViewport(1200, 800, false);
  h.triggerResize();
  assert.equal(h.posts.length, 2);
  assert.equal(h.posts[1].body.restore, true);

  // Re-enter narrow, then idle timeout restores; user activity re-applies.
  h.setViewport(384, 580, true);
  h.sync(0);
  assert.equal(h.posts.length, 3);
  h.flushTimers(120_000);
  assert.equal(h.posts.length, 4);
  assert.equal(h.posts[3].body.restore, true);
  h.activity();
  assert.equal(h.posts.length, 5);
  assert.ok(h.posts[4].body.cols > 0);

  // pagehide restores with keepalive.
  h.triggerPageHide();
  assert.equal(h.posts.length, 6);
  assert.equal(h.posts[5].body.restore, true);
  assert.equal(h.posts[5].opts.keepalive, true);
});

test("#term-fit overview toggle still works when pane remains 120 cols wide", () => {
  const h = harness({ clientWidth: 384, clientHeight: 580, cols: 120, rows: 40, wheel: true, narrow: true });
  h.fit();
  assert.equal(h.term.options.fontSize, 12);
  assert.equal(h.btn.hidden, false);
  assert.equal(h.btn.textContent, "全览");

  h.clickToggle();
  assert.ok(h.term.options.fontSize <= 5.5, `overview fs = ${h.term.options.fontSize}`);
  assert.equal(h.btn.textContent, "放大");

  h.clickToggle();
  assert.equal(h.term.options.fontSize, 12);
  assert.equal(h.btn.textContent, "全览");
});

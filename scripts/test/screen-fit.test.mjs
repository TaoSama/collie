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
  clientWidth = 675,
  clientHeight = 680,
  padX = 26,
  padY = 16,
  cols = 120,
  rows = 40,
  wheel = true,
  narrow = true,
  touch = true,
  charRatio = 0.60,
  lineRatio = 1.24,
  lineHeight = 1.15,
  mode = "screen",
} = {}) {
  const mem = new Map();
  let curModeVal = mode;
  let screenWheelVal = wheel;
  const size = { cols, rows };
  const btn = { textContent: "", hidden: false, listeners: {}, addEventListener(t, fn) { this.listeners[t] = fn; } };
  const kbd = { addEventListener() {} };
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
  const narrowMQ = { matches: narrow };
  const touchMQ = { matches: touch };
  const ctx = {
    $: (s) => (s === "#screen" ? el : s === "#term-fit" ? btn : kbd),
    matchMedia: () => narrowMQ,
    touchMQ,
    getComputedStyle: () => ({ paddingLeft: String(padX / 2), paddingRight: String(padX / 2), paddingTop: String(padY / 2), paddingBottom: String(padY / 2) }),
    store: {
      get: (k, d) => (mem.has(k) ? mem.get(k) : d),
      set: (k, v) => mem.set(k, v),
    },
    term,
    size,
    get screenWheel() { return screenWheelVal; },
    set screenWheel(v) { screenWheelVal = v; },
    curMode: () => curModeVal,
    screenNearBottom: () => true,
    hideJump() {},
    updateScreenScrollbar() {},
    requestAnimationFrame(fn) { fn(); },
    addEventListener(t, fn) { winListeners[t] = fn; },
  };
  const src = section("  const narrowScreen =", "  const PANE_RETRY_START_MS");
  const api = vm.runInNewContext(src + "\n({ fitTerm, setMode(m) { curModeVal = m; } })", ctx);
  return {
    fit: () => api.fitTerm(),
    clickToggle: () => btn.listeners.click(),
    triggerResize: () => winListeners.resize(),
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
    term, el, btn, dims,
  };
}

test("unfolded foldable fits 120x40 Codex/Antigravity/Claude without horizontal or vertical overflow", () => {
  const h = harness({ clientWidth: 675, clientHeight: 680, cols: 120, rows: 40, wheel: true, narrow: true });
  h.fit();
  assert.ok(h.el.scrollWidth <= h.el.clientWidth, `scrollWidth ${h.el.scrollWidth} > clientWidth ${h.el.clientWidth}`);
  assert.ok(h.el.scrollHeight <= h.el.clientHeight, `scrollHeight ${h.el.scrollHeight} > clientHeight ${h.el.clientHeight}`);
  assert.ok(h.term.options.fontSize >= 8.5 && h.term.options.fontSize <= 9.25, `unexpected fs ${h.term.options.fontSize}`);
  assert.equal(h.btn.textContent, "放大");
  assert.equal(h.btn.hidden, false);
});

test("folded foldable phone fits 120x40 with zero overflow and supports zooming to 11px and back", () => {
  const h = harness({ clientWidth: 384, clientHeight: 640, cols: 120, rows: 40, wheel: true, narrow: true });
  h.fit();
  assert.ok(h.el.scrollWidth <= h.el.clientWidth, `folded scrollWidth ${h.el.scrollWidth} > ${h.el.clientWidth}`);
  assert.ok(h.el.scrollHeight <= h.el.clientHeight, `folded scrollHeight ${h.el.scrollHeight} > ${h.el.clientHeight}`);
  const fitted = h.term.options.fontSize;
  assert.ok(fitted >= 4.5 && fitted <= 5.25, `unexpected folded fs ${fitted}`);

  h.clickToggle();
  assert.equal(h.term.options.fontSize, 11);
  assert.equal(h.btn.textContent, "全览");

  h.clickToggle();
  assert.equal(h.term.options.fontSize, fitted);
  assert.equal(h.btn.textContent, "放大");
});

test("narrow-column Claude pane on unfolded foldable scales up past 14px to fill screen ratio", () => {
  const h = harness({ clientWidth: 675, clientHeight: 680, cols: 60, rows: 28, wheel: true, narrow: true });
  h.fit();
  assert.ok(h.term.options.fontSize > 14, `expected fs > 14 to fill screen, got ${h.term.options.fontSize}`);
  assert.ok(h.el.scrollWidth <= h.el.clientWidth);
  assert.ok(h.el.scrollHeight <= h.el.clientHeight);
  assert.equal(h.btn.hidden, true);
});

test("wider monospace font ratio steps down in closed loop so no horizontal clip occurs", () => {
  const h = harness({ clientWidth: 640, clientHeight: 680, cols: 120, rows: 40, wheel: false, narrow: true, charRatio: 0.65 });
  h.fit();
  assert.ok(h.el.scrollWidth <= h.el.clientWidth, `wide-font scrollWidth ${h.el.scrollWidth} > ${h.el.clientWidth}`);
  assert.ok(h.dims.css.cell.width * 120 <= 640 - 26 + 0.5);
});

test("live fold/unfold resize adapts fontSize dynamically and ignores hidden screen mode", () => {
  const h = harness({ clientWidth: 384, clientHeight: 640, cols: 120, rows: 40, wheel: true, narrow: true });
  h.fit();
  const foldedFs = h.term.options.fontSize;

  h.setViewport(675, 680, true);
  h.triggerResize();
  const unfoldedFs = h.term.options.fontSize;
  assert.ok(unfoldedFs > foldedFs + 3, `expected unfold growth ${foldedFs} -> ${unfoldedFs}`);
  assert.ok(h.el.scrollWidth <= h.el.clientWidth);

  // Hidden viewport / chat mode must not clobber fontSize.
  h.setMode("chat");
  h.setViewport(0, 0, true);
  h.triggerResize();
  assert.equal(h.term.options.fontSize, unfoldedFs);
});

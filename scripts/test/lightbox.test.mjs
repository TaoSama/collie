import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
const start = app.indexOf("  // ───────────────────────────── fullscreen image lightbox");
const end = app.indexOf("  async function openSandboxedHtmlRemote(", start);
assert.ok(start >= 0 && end > start, "lightbox section");
const source = app.slice(start, end);

function el(props = {}) {
  const classes = new Set();
  const listeners = new Map();
  const attrs = new Map();
  return {
    hidden: false,
    disabled: false,
    style: {},
    textContent: "",
    open: false,
    offsetLeft: 0,
    offsetTop: 0,
    offsetWidth: 0,
    offsetHeight: 0,
    naturalWidth: 0,
    naturalHeight: 0,
    classList: {
      add: (...xs) => xs.forEach((x) => classes.add(x)),
      remove: (...xs) => xs.forEach((x) => classes.delete(x)),
      toggle: (x, v) => { const on = v === undefined ? !classes.has(x) : !!v; if (on) classes.add(x); else classes.delete(x); return on; },
      contains: (x) => classes.has(x),
    },
    setAttribute: (k, v) => attrs.set(k, String(v)),
    getAttribute: (k) => attrs.get(k) ?? null,
    removeAttribute: (k) => attrs.delete(k),
    hasAttribute: (k) => attrs.has(k),
    addEventListener: (t, fn) => listeners.set(t, [...(listeners.get(t) || []), fn]),
    emit(t, ev = {}) {
      for (const fn of listeners.get(t) || []) fn({ preventDefault() {}, target: this, ...ev });
    },
    setPointerCapture() {},
    getBoundingClientRect: () => ({ left: 0, top: 0, width: 0, height: 0 }),
    ...props,
  };
}

function makeLightbox({ stageW = 1280, stageH = 800, imgLeft = 171, imgTop = 66, imgW = 939, imgH = 704, natW = 1600, natH = 1200, isNarrow = false } = {}) {
  let now = 1000;
  const lightbox = el({
    showModal() { this.open = true; },
    close() { this.open = false; this.emit("close"); },
  });
  const lbStage = el({
    getBoundingClientRect: () => ({ left: 0, top: 0, width: stageW, height: stageH }),
  });
  const lbImg = el({
    offsetLeft: imgLeft, offsetTop: imgTop, offsetWidth: imgW, offsetHeight: imgH,
    naturalWidth: natW, naturalHeight: natH,
  });
  Object.defineProperty(lbImg, "src", {
    set(v) { this._src = v; if (v) this.onload?.(); },
    get() { return this._src || ""; },
  });
  const nodes = {
    "#lightbox": lightbox, "#lb-stage": lbStage, "#lb-img": lbImg,
    "#lb-name": el(), "#lb-count": el(), "#lb-meta": el(), "#lb-status": el(),
    "#lb-zoom": el(), "#lb-zoom-pct": el(), "#lb-open": el(), "#lb-dl": el(),
    "#lb-prev": el(), "#lb-next": el(), "#lb-close": el(), "#lb-edit": el(),
  };
  const doc = el({ activeElement: null, querySelector: () => null });
  const ctx = vm.createContext({
    $: (s) => nodes[s] || null,
    document: doc,
    window: { CSS: { supports: () => false } },
    HTMLDialogElement: function HTMLDialogElement() {},
    performance: { now: () => now },
    requestAnimationFrame: (fn) => fn(),
    URL: { revokeObjectURL() {} },
    state: { machines: [] },
    thumbs: new Map(),
    thumbMeta: new Map(),
    narrow: () => isNarrow,
    view: "collie",
    viewScroller: () => null,
    markProgrammaticScroll() {},
    fmtSize: String,
    machineName: String,
  });
  ctx.HTMLDialogElement.prototype.closedBy = "any";
  vm.runInContext(
    source + "\nglobalThis.__lb = { openLightbox, zoomLbTo, get state() { return { scale: lbScale, x: lbX, y: lbY, idx: lbIdx }; } };",
    ctx,
  );
  const api = ctx.__lb;
  api.openLightbox([
    { url: "blob:one", alt: "one.png" },
    { url: "blob:two", alt: "two.png" },
  ], 0);
  const cx = imgLeft + imgW / 2, cy = imgTop + imgH / 2;
  const screenPoint = (fx, fy) => {
    const { scale, x, y } = api.state;
    return {
      x: cx + x + (fx - 0.5) * imgW * scale,
      y: cy + y + (fy - 0.5) * imgH * scale,
    };
  };
  return {
    lightbox, lbStage, lbImg, doc, api, screenPoint,
    advance(ms) { now += ms; },
  };
}

test("wheel zoom on landscape and portrait keeps focal point fixed under cursor with no-anim", () => {
  for (const cfg of [
    { stageW: 1280, stageH: 800, imgLeft: 171, imgTop: 66, imgW: 939, imgH: 704 },
    { stageW: 1280, stageH: 800, imgLeft: 442, imgTop: 66, imgW: 396, imgH: 704, natW: 900, natH: 1600 },
  ]) {
    const lb = makeLightbox(cfg);
    const target = lb.screenPoint(0.75, 0.25);
    for (let i = 0; i < 6; i++) {
      lb.lightbox.emit("wheel", { clientX: target.x, clientY: target.y, deltaX: 0, deltaY: -120, deltaMode: 0, ctrlKey: false });
      assert.equal(lb.lightbox.classList.contains("no-anim"), true);
      const after = lb.screenPoint(0.75, 0.25);
      assert.ok(Math.abs(after.x - target.x) < 0.01, `driftX=${after.x - target.x}`);
      assert.ok(Math.abs(after.y - target.y) < 0.01, `driftY=${after.y - target.y}`);
    }
    for (let i = 0; i < 4; i++) {
      lb.lightbox.emit("wheel", { clientX: target.x, clientY: target.y, deltaX: 0, deltaY: 120, deltaMode: 0, ctrlKey: false });
      const after = lb.screenPoint(0.75, 0.25);
      assert.ok(Math.abs(after.x - target.x) < 0.01, `out driftX=${after.x - target.x}`);
      assert.ok(Math.abs(after.y - target.y) < 0.01, `out driftY=${after.y - target.y}`);
    }
  }
});

test("2-finger pinch tracks midpoint pan and hands off remaining finger without jumping", () => {
  const lb = makeLightbox({ stageW: 390, stageH: 844, imgLeft: 16, imgTop: 305, imgW: 358, imgH: 269, isNarrow: true });
  const startPt = lb.screenPoint(0.25, 0.25);
  lb.lbStage.emit("pointerdown", { pointerId: 1, pointerType: "touch", clientX: startPt.x - 20, clientY: startPt.y, target: lb.lbImg });
  lb.lbStage.emit("pointerdown", { pointerId: 2, pointerType: "touch", clientX: startPt.x + 20, clientY: startPt.y, target: lb.lbImg });
  // Spread 2.5x while shifting midpoint (+35, +40)
  const midX = startPt.x + 35, midY = startPt.y + 40;
  lb.lbStage.emit("pointermove", { pointerId: 1, pointerType: "touch", clientX: midX - 50, clientY: midY });
  lb.lbStage.emit("pointermove", { pointerId: 2, pointerType: "touch", clientX: midX + 50, clientY: midY });
  assert.ok(Math.abs(lb.api.state.scale - 2.5) < 0.01);
  const afterPinch = lb.screenPoint(0.25, 0.25);
  assert.ok(Math.abs(afterPinch.x - midX) < 0.1, `pinch driftX=${afterPinch.x - midX}`);
  assert.ok(Math.abs(afterPinch.y - midY) < 0.1, `pinch driftY=${afterPinch.y - midY}`);

  // Lift finger 2, pan with finger 1 by (+20, -15), then lift finger 1 (must stay zoomed)
  lb.lbStage.emit("pointerup", { pointerId: 2, pointerType: "touch", clientX: midX + 50, clientY: midY });
  lb.lbStage.emit("pointermove", { pointerId: 1, pointerType: "touch", clientX: midX - 30, clientY: midY - 15 });
  const afterHandoff = lb.screenPoint(0.25, 0.25);
  assert.ok(Math.abs(afterHandoff.x - (midX + 20)) < 0.1);
  assert.ok(Math.abs(afterHandoff.y - (midY - 15)) < 0.1);
  lb.lbStage.emit("pointerup", { pointerId: 1, pointerType: "touch", clientX: midX - 30, clientY: midY - 15 });
  assert.ok(lb.api.state.scale > 2);
});

test("touch double-tap toggles zoom at tap point; mouse single-click toggles zoom at click point", () => {
  const lb = makeLightbox({ stageW: 390, stageH: 844, imgLeft: 16, imgTop: 305, imgW: 358, imgH: 269, isNarrow: true });
  const pt = lb.screenPoint(0.25, 0.25);
  // First touch tap does not zoom
  lb.lbStage.emit("pointerdown", { pointerId: 1, pointerType: "touch", clientX: pt.x, clientY: pt.y, target: lb.lbImg });
  lb.lbStage.emit("pointerup", { pointerId: 1, pointerType: "touch", clientX: pt.x, clientY: pt.y, target: lb.lbImg });
  assert.equal(lb.api.state.scale, 1);
  // Second touch tap within 320ms zooms in at pt (animated: no-anim off)
  lb.advance(120);
  lb.lbStage.emit("pointerdown", { pointerId: 2, pointerType: "touch", clientX: pt.x, clientY: pt.y, target: lb.lbImg });
  lb.lbStage.emit("pointerup", { pointerId: 2, pointerType: "touch", clientX: pt.x, clientY: pt.y, target: lb.lbImg });
  assert.ok(lb.api.state.scale > 1.5);
  assert.equal(lb.lightbox.classList.contains("no-anim"), false);
  const after = lb.screenPoint(0.25, 0.25);
  assert.ok(Math.abs(after.x - pt.x) < 0.1);
  assert.ok(Math.abs(after.y - pt.y) < 0.1);

  // Mouse single click zooms back out
  lb.lbStage.emit("pointerdown", { pointerId: 3, pointerType: "mouse", button: 0, clientX: pt.x, clientY: pt.y, target: lb.lbImg });
  lb.lbStage.emit("pointerup", { pointerId: 3, pointerType: "mouse", button: 0, clientX: pt.x, clientY: pt.y, target: lb.lbImg });
  assert.equal(lb.api.state.scale, 1);
});

test("horizontal touch swipe & trackpad wheel switch slides only at 1x, never while zoomed", () => {
  const lb = makeLightbox();
  // Touch left swipe at 1x -> next slide (idx 1)
  lb.lbStage.emit("pointerdown", { pointerId: 1, pointerType: "touch", clientX: 300, clientY: 400, target: lb.lbImg });
  lb.lbStage.emit("pointermove", { pointerId: 1, pointerType: "touch", clientX: 180, clientY: 402 });
  lb.lbStage.emit("pointerup", { pointerId: 1, pointerType: "touch", clientX: 180, clientY: 402 });
  assert.equal(lb.api.state.idx, 1);

  // Trackpad horizontal wheel right swipe (deltaX < 0) -> prev slide (idx 0), inertia ignored
  lb.lightbox.emit("wheel", { clientX: 640, clientY: 400, deltaX: -40, deltaY: 0, deltaMode: 0, ctrlKey: false });
  lb.lightbox.emit("wheel", { clientX: 640, clientY: 400, deltaX: -40, deltaY: 0, deltaMode: 0, ctrlKey: false });
  assert.equal(lb.api.state.idx, 0);
  lb.advance(100);
  lb.lightbox.emit("wheel", { clientX: 640, clientY: 400, deltaX: -80, deltaY: 0, deltaMode: 0, ctrlKey: false });
  assert.equal(lb.api.state.idx, 0, "inertia during cooldown must not double-switch");

  // Zoom in, then both touch drag and horizontal wheel must NOT switch slides
  lb.api.zoomLbTo(2.5, 640, 418, true);
  lb.advance(600);
  lb.lightbox.emit("wheel", { clientX: 640, clientY: 400, deltaX: 120, deltaY: 0, deltaMode: 0, ctrlKey: false });
  assert.equal(lb.api.state.idx, 0);
  lb.lbStage.emit("pointerdown", { pointerId: 2, pointerType: "touch", clientX: 300, clientY: 400, target: lb.lbImg });
  lb.lbStage.emit("pointermove", { pointerId: 2, pointerType: "touch", clientX: 100, clientY: 400 });
  lb.lbStage.emit("pointerup", { pointerId: 2, pointerType: "touch", clientX: 100, clientY: 400 });
  assert.equal(lb.api.state.idx, 0);
});

test("keyboard +/- scales pan proportionally around screen center", () => {
  const lb = makeLightbox();
  const pt = lb.screenPoint(0.75, 0.25);
  lb.api.zoomLbTo(2, pt.x, pt.y, true);
  // Remember what image point is currently at the image's untransformed center (cx, cy)
  const midBefore = {
    u: -lb.api.state.x / lb.api.state.scale,
    v: -lb.api.state.y / lb.api.state.scale,
  };
  lb.doc.emit("keydown", { key: "+" });
  const midAfter = {
    u: -lb.api.state.x / lb.api.state.scale,
    v: -lb.api.state.y / lb.api.state.scale,
  };
  assert.ok(Math.abs(midAfter.u - midBefore.u) < 0.01);
  assert.ok(Math.abs(midAfter.v - midBefore.v) < 0.01);
});

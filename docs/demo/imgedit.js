/* Collie's image editor for a picture attached to a message: Box and Pen in
   six inks and three widths, Crop (free or a fixed shape, a quarter turn
   left, Reset), Undo / Redo over all of it and Clear. The source is never
   changed: what comes back is the picture drawn from the edits at the
   source's resolution, and the edits with their history, so the same picture
   opened again carries on where it stopped.

   window.collieImageEditor.open(src, state, warn) → Promise of
   { blob, state } (blob null when nothing is left done) or null (cancelled). */
"use strict";
(() => {
  // ───────────────────────────── edits and their geometry
  // Edits: { turns (quarter turns clockwise, 0–3), crop (fractions of the
  // turned picture), shape, marks }. A mark is { tool: "box" | "pen", ink,
  // width, points } in the source's pixels, before it was turned or cropped.
  const INKS = { red: "#ff3b30", yellow: "#ffcc00", green: "#34c759", blue: "#007aff", white: "#ffffff", black: "#000000" };
  const INK_NAME = { red: "红色", yellow: "黄色", green: "绿色", blue: "蓝色", white: "白色", black: "黑色" };
  const LINE_WIDTHS = [2, 3.5, 7]; // on screen; burned in as wide relative to the picture as seen
  const SHAPES = { free: null, original: 0, square: 1, "4:3": 4 / 3, "3:4": 3 / 4, "16:9": 16 / 9, "9:16": 9 / 16 };
  const TURNED_SHAPE = { "4:3": "3:4", "3:4": "4:3", "16:9": "9:16", "9:16": "16:9" };
  const CROP = { inset: 28, corner: 20, grab: 22, minimum: 64, dim: 0.6 };
  const MAX_UPLOAD = 25 << 20; // what /api/uploads takes
  const SAVE_FAILED = "没存成：浏览器读不了这张图的像素", TOO_LARGE = "编辑后的图片太大了（最多 25 MB）";
  const WHOLE = { x: 0, y: 0, width: 1, height: 1 };

  const clamp = (v, lo, hi) => Math.min(Math.max(v, lo), hi);
  const sameRect = (a, b) => a.x === b.x && a.y === b.y && a.width === b.width && a.height === b.height;
  const noEdits = () => ({ turns: 0, crop: WHOLE, shape: "free", marks: [] });
  const sameEdits = (a, b) => a.turns === b.turns && a.shape === b.shape && sameRect(a.crop, b.crop)
    && a.marks.length === b.marks.length && a.marks.every((m, i) => m === b.marks[i]);
  const isEmpty = (e) => e.turns === 0 && sameRect(e.crop, WHOLE) && !e.marks.length;
  const framedAsIs = (e) => e.turns === 0 && sameRect(e.crop, WHOLE) && e.shape === "free";

  const turnedSize = (e, s) => e.turns % 2 ? { width: s.height, height: s.width } : s;
  // [a, b, c, d, e, f] as setTransform takes it: the source's pixels turned.
  function turnMatrix(e, s) {
    switch (e.turns) {
      case 1: return [0, 1, -1, 0, s.height, 0];
      case 2: return [-1, 0, 0, -1, s.width, s.height];
      case 3: return [0, -1, 1, 0, 0, s.width];
      default: return [1, 0, 0, 1, 0, 0];
    }
  }
  function unturnPoint(e, p, s) {
    switch (e.turns) {
      case 1: return { x: p.y, y: s.height - p.x };
      case 2: return { x: s.width - p.x, y: s.height - p.y };
      case 3: return { x: s.width - p.y, y: p.x };
      default: return p;
    }
  }
  function turnLeft(e) {
    const c = e.crop;
    return { ...e, turns: (e.turns + 3) % 4, crop: { x: c.y, y: 1 - (c.x + c.width), width: c.height, height: c.width }, shape: TURNED_SHAPE[e.shape] || e.shape };
  }
  const resetFrame = (e) => ({ ...e, turns: 0, crop: WHOLE, shape: "free" });

  // Width over height in pixels for a picture of size s (as turned); null: any.
  const shapeRatio = (shape, s) => SHAPES[shape] === 0 ? (s.height > 0 ? s.width / s.height : null) : SHAPES[shape];
  // The largest crop of `ratio` round the crop's middle, inside the picture.
  function fitCrop(crop, ratio, s) {
    const k = (ratio * s.height) / s.width;
    let w = 1, h = 1 / k;
    if (h > 1) { h = 1; w = k; }
    const mx = crop.x + crop.width / 2, my = crop.y + crop.height / 2;
    return { x: clamp(mx - w / 2, 0, 1 - w), y: clamp(my - h / 2, 0, 1 - h), width: w, height: h };
  }
  function reshape(e, shape, s) {
    const t = turnedSize(e, s), r = shapeRatio(shape, t);
    return { ...e, shape, crop: r == null ? e.crop : fitCrop(e.crop, r, t) };
  }
  // The crop in the turned picture's whole pixels.
  function cropRect(e, s) {
    const t = turnedSize(e, s);
    const l = Math.max(0, Math.floor(e.crop.x * t.width + 1e-9)), tp = Math.max(0, Math.floor(e.crop.y * t.height + 1e-9));
    const r = Math.min(t.width, Math.ceil((e.crop.x + e.crop.width) * t.width - 1e-9));
    const b = Math.min(t.height, Math.ceil((e.crop.y + e.crop.height) * t.height - 1e-9));
    return { x: l, y: tp, width: Math.max(0, r - l), height: Math.max(0, b - tp) };
  }

  // A crop dragged by a handle ("t", "bl", "inside"…) by `d` (fractions),
  // no smaller than `min`; with `k` (width / height in fractions) it keeps
  // its shape, pinned at the corner or edge opposite the one dragged.
  function dragCrop(c, handle, d, min, k) {
    const minW = Math.min(min.width, 1), minH = Math.min(min.height, 1);
    if (handle === "inside") return { ...c, x: clamp(c.x + d.x, 0, 1 - c.width), y: clamp(c.y + d.y, 0, 1 - c.height) };
    const L = handle.includes("l"), R = handle.includes("r"), T = handle.includes("t"), B = handle.includes("b");
    const x1 = c.x + c.width, y1 = c.y + c.height;
    if (!(k > 0)) {
      let a = c.x, b = x1, u = c.y, v = y1;
      if (L) a = Math.min(Math.max(a + d.x, 0), b - minW);
      if (R) b = Math.max(Math.min(b + d.x, 1), a + minW);
      if (T) u = Math.min(Math.max(u + d.y, 0), v - minH);
      if (B) v = Math.max(Math.min(v + d.y, 1), u + minH);
      return { x: a, y: u, width: b - a, height: v - u };
    }
    const least = Math.min(Math.max(minW, minH * k), 1, k);
    if ((L || R) && (T || B)) {
      const ax = L ? x1 : c.x, ay = T ? y1 : c.y;
      const askW = L ? c.width - d.x : c.width + d.x, askH = T ? c.height - d.y : c.height + d.y;
      const roomW = L ? ax : 1 - ax, roomH = T ? ay : 1 - ay;
      const w = Math.max(Math.min(Math.max(askW, askH * k), roomW, roomH * k), Math.min(least, roomW, roomH * k)), h = w / k;
      return { x: L ? ax - w : ax, y: T ? ay - h : ay, width: w, height: h };
    }
    if (L || R) {
      const ax = L ? x1 : c.x, roomW = L ? ax : 1 - ax;
      const w = Math.max(Math.min(L ? c.width - d.x : c.width + d.x, roomW, k), Math.min(least, roomW)), h = w / k;
      return { x: L ? ax - w : ax, y: clamp(c.y + c.height / 2 - h / 2, 0, 1 - h), width: w, height: h };
    }
    const ay = T ? y1 : c.y, roomH = T ? ay : 1 - ay;
    const h = Math.max(Math.min(T ? c.height - d.y : c.height + d.y, roomH, 1 / k), Math.min(least / k, roomH)), w = h * k;
    return { x: clamp(c.x + c.width / 2 - w / 2, 0, 1 - w), y: T ? ay - h : ay, width: w, height: h };
  }
  // What the pointer at p takes of the crop's frame f: a corner, an edge, inside it, or nothing.
  function handleAt(p, f) {
    const g = CROP.grab, x1 = f.x + f.width, y1 = f.y + f.height;
    if (p.x < f.x - g || p.x > x1 + g || p.y < f.y - g || p.y > y1 + g) return null;
    const l = Math.abs(p.x - f.x) <= g, r = !l && Math.abs(p.x - x1) <= g;
    const t = Math.abs(p.y - f.y) <= g, b = !t && Math.abs(p.y - y1) <= g;
    const h = (t ? "t" : b ? "b" : "") + (l ? "l" : r ? "r" : "");
    return h || (p.x >= f.x && p.x <= x1 && p.y >= f.y && p.y <= y1 ? "inside" : null);
  }
  const CURSOR = { tl: "nwse-resize", br: "nwse-resize", tr: "nesw-resize", bl: "nesw-resize", t: "ns-resize", b: "ns-resize", l: "ew-resize", r: "ew-resize", inside: "move" };

  const fitIn = (s, box) => {
    const k = Math.min(box.width / s.width, box.height / s.height), w = s.width * k, h = s.height * k;
    return { x: box.x + (box.width - w) / 2, y: box.y + (box.height - h) / 2, width: w, height: h };
  };
  // Where things are on screen: marking, the crop fills the room; cropping,
  // the whole turned picture shows, inset so the handles on its edges can be
  // taken. picture: the whole turned picture's frame; visible: what shows.
  function layout(e, s, area, cropping) {
    const t = turnedSize(e, s), none = { x: 0, y: 0, width: 0, height: 0 };
    if (!t.width || !t.height || area.width <= 0 || area.height <= 0) return { picture: none, scale: 1, visible: none, crop: none };
    if (cropping) {
      const i = CROP.inset, picture = fitIn(t, { x: i, y: i, width: Math.max(0, area.width - 2 * i), height: Math.max(0, area.height - 2 * i) });
      const c = e.crop;
      return { picture, scale: picture.width / t.width, visible: picture,
        crop: { x: picture.x + c.x * picture.width, y: picture.y + c.y * picture.height, width: c.width * picture.width, height: c.height * picture.height } };
    }
    const kept = cropRect(e, s);
    const shown = fitIn({ width: kept.width, height: kept.height }, { x: 0, y: 0, width: area.width, height: area.height });
    const scale = shown.width / Math.max(kept.width, 1);
    return { picture: { x: shown.x - kept.x * scale, y: shown.y - kept.y * scale, width: t.width * scale, height: t.height * scale }, scale, visible: shown, crop: shown };
  }
  function screenToPixel(l, p) {
    const v = l.visible;
    return { x: (clamp(p.x, v.x, v.x + v.width) - l.picture.x) / l.scale, y: (clamp(p.y, v.y, v.y + v.height) - l.picture.y) / l.scale };
  }
  const boxRect = (a, b) => ({ x: Math.min(a.x, b.x), y: Math.min(a.y, b.y), width: Math.abs(b.x - a.x), height: Math.abs(b.y - a.y) });
  // more than a tap
  function keepMark(m) {
    if (m.points.length < 2) return false;
    if (m.tool === "pen") return true;
    const r = boxRect(m.points[0], m.points[m.points.length - 1]);
    return r.width >= 2 || r.height >= 2;
  }
  function strokeMark(ctx, m) {
    const ps = m.points;
    if (ps.length < 2) return;
    ctx.strokeStyle = INKS[m.ink];
    ctx.lineWidth = m.width;
    ctx.beginPath();
    if (m.tool === "box") { const r = boxRect(ps[0], ps[ps.length - 1]); ctx.rect(r.x, r.y, r.width, r.height); }
    else { ctx.moveTo(ps[0].x, ps[0].y); for (const p of ps.slice(1)) ctx.lineTo(p.x, p.y); }
    ctx.stroke();
  }
  // Outside the crop dimmed, a line round it, its corners marked, the thirds while dragged.
  function drawCropFrame(ctx, area, f, dragging) {
    ctx.fillStyle = `rgba(0, 0, 0, ${CROP.dim})`;
    ctx.beginPath(); ctx.rect(0, 0, area.width, area.height); ctx.rect(f.x, f.y, f.width, f.height); ctx.fill("evenodd");
    ctx.strokeStyle = "#fff"; ctx.lineWidth = 1; ctx.strokeRect(f.x, f.y, f.width, f.height);
    if (dragging) {
      ctx.strokeStyle = "rgba(255, 255, 255, .45)";
      ctx.beginPath();
      for (const i of [1, 2]) {
        const x = f.x + (f.width * i) / 3, y = f.y + (f.height * i) / 3;
        ctx.moveTo(x, f.y); ctx.lineTo(x, f.y + f.height); ctx.moveTo(f.x, y); ctx.lineTo(f.x + f.width, y);
      }
      ctx.stroke();
    }
    const c = Math.min(CROP.corner, f.width / 2, f.height / 2);
    ctx.strokeStyle = "#fff"; ctx.lineWidth = 3; ctx.lineCap = "square";
    ctx.beginPath();
    for (const [x, y, dx, dy] of [[f.x, f.y, c, c], [f.x + f.width, f.y, -c, c], [f.x, f.y + f.height, c, -c], [f.x + f.width, f.y + f.height, -c, -c]]) {
      ctx.moveTo(x + dx, y); ctx.lineTo(x, y); ctx.lineTo(x, y + dy);
    }
    ctx.stroke();
  }

  // ───────────────────────────── the dialog
  const dlg = document.getElementById("imgedit");
  if (!dlg) return;
  const $ = (s) => dlg.querySelector(s);
  const canvas = $(".ie-stage canvas"), stage = $(".ie-stage"), status = $(".ie-stage .lb-status");
  const widthSel = $('[data-ie="width"]'), shapeSel = $('[data-ie="shape"]');
  $(".ie-inks").innerHTML = Object.keys(INKS).map((k) => `<button type="button" class="ie-ink" data-ink="${k}" style="background:${INKS[k]}" aria-label="${INK_NAME[k]}" title="${INK_NAME[k]}"></button>`).join("");

  let img = null, natural = { width: 0, height: 0 }, size = { width: 0, height: 0 };
  let edits = noEdits(), history = { past: [], future: [] };
  let tool = "box", ink = "red", lineWidth = 1, current = null, cropping = null, busy = false, finish = null, warn = () => {};

  const set = (sel, v) => { if (sel.value !== String(v)) { sel.value = String(v); sel.dispatchEvent(new Event("change", { bubbles: true })); } };
  function sync() {
    const off = !img || busy;
    $('[data-ie="undo"]').disabled = off || !history.past.length;
    $('[data-ie="redo"]').disabled = off || !history.future.length;
    $('[data-ie="done"]').disabled = off;
    $('[data-ie="clear"]').disabled = off || !edits.marks.length;
    $('[data-ie="clear"]').hidden = tool === "crop";
    $('[data-ie="reset"]').disabled = off || framedAsIs(edits);
    $('[data-ie="rotate"]').disabled = off;
    $(".ie-marks").hidden = tool === "crop";
    $(".ie-crop").hidden = tool !== "crop";
    for (const b of dlg.querySelectorAll("[data-tool]")) b.setAttribute("aria-pressed", String(b.dataset.tool === tool));
    for (const b of dlg.querySelectorAll("[data-ink]")) b.setAttribute("aria-pressed", String(b.dataset.ink === ink));
    set(shapeSel, edits.shape);
    canvas.classList.toggle("marking", tool !== "crop");
    draw();
  }
  const view = () => layout(edits, natural, size, tool === "crop");
  function draw() {
    if (!img || !size.width) return;
    const dpr = devicePixelRatio || 1, l = view();
    const w = Math.round(size.width * dpr), h = Math.round(size.height * dpr);
    // resizing the canvas reallocates it: only when the room changed
    if (canvas.width !== w || canvas.height !== h) { canvas.width = w; canvas.height = h; }
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, size.width, size.height);
    ctx.save();
    ctx.beginPath(); ctx.rect(l.visible.x, l.visible.y, l.visible.width, l.visible.height); ctx.clip();
    ctx.translate(l.picture.x, l.picture.y);
    ctx.scale(l.scale, l.scale);
    ctx.transform(...turnMatrix(edits, natural));
    ctx.drawImage(img, 0, 0, natural.width, natural.height);
    ctx.lineCap = "round"; ctx.lineJoin = "round";
    for (const m of current ? [...edits.marks, current] : edits.marks) strokeMark(ctx, m);
    ctx.restore();
    if (tool === "crop") drawCropFrame(ctx, size, l.crop, !!cropping);
  }
  let frame = 0;
  const drawSoon = () => { frame ||= requestAnimationFrame(() => { frame = 0; draw(); }); };
  new ResizeObserver(() => { size = { width: stage.clientWidth, height: stage.clientHeight }; draw(); }).observe(stage);

  // one step of the history: the edits changed by `make`, the ones before kept for Undo
  function change(make) {
    const next = make(edits);
    if (sameEdits(next, edits)) return;
    history = { past: [...history.past, edits], future: [] };
    edits = next;
    sync();
  }
  function step(back) {
    const [from, to] = back ? ["past", "future"] : ["future", "past"];
    if (!history[from].length || current || cropping) return;
    const next = history[from][history[from].length - 1];
    history = { [from]: history[from].slice(0, -1), [to]: [...history[to], edits] };
    edits = next;
    sync();
  }

  const local = (e) => { const r = canvas.getBoundingClientRect(); return { x: e.clientX - r.left, y: e.clientY - r.top }; };
  canvas.addEventListener("pointerdown", (e) => {
    if (!img || busy || (e.pointerType === "mouse" && e.button !== 0) || current || cropping) return;
    const p = local(e), l = view();
    if (tool === "crop") {
      const handle = handleAt(p, l.crop);
      if (!handle) return;
      canvas.setPointerCapture(e.pointerId);
      cropping = { id: e.pointerId, handle, from: edits.crop, at: p };
      return;
    }
    canvas.setPointerCapture(e.pointerId);
    current = { tool, ink, width: LINE_WIDTHS[lineWidth] / Math.max(l.scale, Number.EPSILON), points: [unturnPoint(edits, screenToPixel(l, p), natural)], id: e.pointerId };
  });
  canvas.addEventListener("pointermove", (e) => {
    const p = local(e), l = view();
    if (cropping && cropping.id === e.pointerId) {
      if (l.picture.width <= 0 || l.picture.height <= 0) return;
      const d = { x: (p.x - cropping.at.x) / l.picture.width, y: (p.y - cropping.at.y) / l.picture.height };
      const r = shapeRatio(edits.shape, turnedSize(edits, natural));
      const crop = dragCrop(cropping.from, cropping.handle, d, { width: CROP.minimum / l.picture.width, height: CROP.minimum / l.picture.height }, r == null ? null : (r * l.picture.height) / l.picture.width);
      edits = { ...edits, crop };
      drawSoon();
    } else if (current && current.id === e.pointerId) {
      const q = unturnPoint(edits, screenToPixel(l, p), natural);
      if (current.tool === "box") current.points[1] = q; else current.points.push(q);
      drawSoon();
    } else if (tool === "crop" && e.pointerType === "mouse") canvas.style.cursor = CURSOR[handleAt(p, l.crop)] || "";
  });
  const release = (e, cancel) => {
    if (cropping && cropping.id === e.pointerId) {
      const before = { ...edits, crop: cropping.from };
      cropping = null;
      // the crop let go is one step: back to where it was before this drag
      if (cancel) edits = before;
      else if (!sameEdits(before, edits)) history = { past: [...history.past, before], future: [] };
      sync();
    } else if (current && current.id === e.pointerId) {
      const { id, ...m } = current;
      current = null;
      if (!cancel && keepMark(m)) change((ed) => ({ ...ed, marks: [...ed.marks, m] }));
      else draw();
    }
  };
  canvas.addEventListener("pointerup", (e) => release(e, false));
  canvas.addEventListener("pointercancel", (e) => release(e, true));

  dlg.addEventListener("click", (e) => {
    const b = e.target.closest("button");
    if (!b || b.disabled) return;
    if (b.dataset.tool) { tool = b.dataset.tool; canvas.style.cursor = ""; sync(); }
    else if (b.dataset.ink) { ink = b.dataset.ink; sync(); }
    else switch (b.dataset.ie) {
      case "cancel": close(null); break;
      case "done": done(); break;
      case "undo": step(true); break;
      case "redo": step(false); break;
      case "clear": change((ed) => ({ ...ed, marks: [] })); break;
      case "rotate": change(turnLeft); break;
      case "reset": change(resetFrame); break;
    }
  });
  widthSel.addEventListener("change", () => { lineWidth = +widthSel.value; });
  shapeSel.addEventListener("change", () => { if (shapeSel.value !== edits.shape) change((ed) => reshape(ed, shapeSel.value, natural)); });
  // closed some other way (a browser lets a repeated Esc through): cancelled
  dlg.addEventListener("close", () => close(null));
  dlg.addEventListener("cancel", (e) => { e.preventDefault(); if (!dlg.querySelector('.pick[aria-expanded="true"]')) close(null); });
  dlg.addEventListener("keydown", (e) => {
    const k = e.key.toLowerCase();
    if (!(e.metaKey || e.ctrlKey) || (k !== "z" && !(k === "y" && e.ctrlKey))) return;
    e.preventDefault();
    step(!(k === "y" || e.shiftKey));
  });

  const encode = (c, type, q) => new Promise((ok) => c.toBlob(ok, type, q));
  async function done() {
    if (!img || busy) return;
    const state = { edits, history };
    if (isEmpty(edits)) { close({ blob: null, state }); return; }
    busy = true; sync();
    // cancelled meanwhile (and maybe another picture opened): this result is for no one
    const mine = finish;
    try {
      const out = cropRect(edits, natural), t = turnMatrix(edits, natural);
      if (out.width < 1 || out.height < 1) throw new Error(SAVE_FAILED);
      const c = document.createElement("canvas");
      c.width = out.width; c.height = out.height;
      const ctx = c.getContext("2d");
      if (!ctx) throw new Error(SAVE_FAILED);
      // white under a transparent picture, so it does not go black as JPEG
      ctx.fillStyle = INKS.white;
      ctx.fillRect(0, 0, out.width, out.height);
      ctx.setTransform(t[0], t[1], t[2], t[3], t[4] - out.x, t[5] - out.y);
      ctx.imageSmoothingQuality = "high";
      ctx.drawImage(img, 0, 0, natural.width, natural.height);
      ctx.lineCap = "round"; ctx.lineJoin = "round";
      for (const m of edits.marks) strokeMark(ctx, m);
      const png = await encode(c, "image/png");
      // a big screenshot as PNG can pass what an upload takes: JPEG then
      const blob = png && png.size <= MAX_UPLOAD ? png : await encode(c, "image/jpeg", 0.9);
      if (!blob) throw new Error(SAVE_FAILED);
      if (blob.size > MAX_UPLOAD) throw new Error(TOO_LARGE);
      if (finish === mine) close({ blob, state });
    } catch (err) {
      if (finish !== mine) return;
      busy = false; sync();
      warn(err.message === TOO_LARGE ? TOO_LARGE : SAVE_FAILED);
    }
  }
  function close(result) {
    if (!finish) return;
    const f = finish;
    finish = null; img = null; current = null; cropping = null; busy = false;
    if (dlg.open) dlg.close();
    f(result);
  }

  function open(src, state, onWarn) {
    warn = onWarn;
    close(null);
    edits = state?.edits || noEdits();
    history = state?.history || { past: [], future: [] };
    tool = "box"; ink = "red"; lineWidth = 1; busy = false;
    set(widthSel, lineWidth);
    status.hidden = false;
    dlg.showModal();
    size = { width: stage.clientWidth, height: stage.clientHeight };
    const el = new Image();
    el.src = src;
    const p = new Promise((ok) => { finish = ok; });
    const mine = finish;
    el.decode().then(() => {
      if (finish !== mine) return;
      img = el; natural = { width: el.naturalWidth, height: el.naturalHeight };
      status.hidden = true;
      sync();
    }, () => {
      if (finish !== mine) return;
      warn("图片读不出来，没法编辑");
      close(null);
    });
    sync();
    return p;
  }
  window.collieImageEditor = { open, isOpen: () => dlg.open };
})();

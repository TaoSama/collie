// web/screencache.js: local scroll cache, delta decoder, Claude sticky-row fix,
// and real scrollbar state for Claude, Codex, and agy.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {}, performance: { now: () => 1000 } };
vm.runInNewContext(readFileSync(new URL("../../web/screencache.js", import.meta.url), "utf8"), ctx);
const { applyFramePatch, polishClaudeRow0, segmentFrame, findOverlap, createScreenCache } = ctx.window.collieScreenCache;

function makeClaudeFrame(startNum, { sticky = false, pill = false } = {}) {
  const rows = [];
  if (sticky) {
    rows.push("\x1b[0m\x1b[38;2;80;80;80m\x1b[48;2;55;55;55m❯ Print 100 lines…\x1b[0m\x1b[48;2;55;55;55m \x1b[0m");
  }
  const bodyLen = sticky ? 32 : 33;
  for (let i = 0; i < bodyLen; i++) {
    const n = String(startNum + i).padStart(3, "0");
    let line = `  LINE-${n} word${n}`;
    if (pill && i === bodyLen - 1) line += "        \x1b[48;2;55;55;55mJump to bottom (ctrl+End) ↓\x1b[0m";
    rows.push(line);
  }
  rows.push("────────────────────────────────────────────────────────");
  rows.push("❯ ");
  rows.push("────────────────────────────────────────────────────────");
  rows.push("  Opus 5.5 (1M context) │ 44k/1000k");
  rows.push("  +0/-0");
  rows.push("  Claude Code 79%");
  rows.push("  ⏵⏵ bypass permissions on");
  return rows;
}

function makeCodexFrame(startNum, { sticky = true, pill = false } = {}) {
  const rows = [];
  if (sticky) {
    rows.push("\x1b[0m\x1b[1m\x1b[2m\x1b[48;2;74;77;84m› \x1b[0m\x1b[48;2;74;77;84mPrint 100 lines…");
  }
  const bodyLen = sticky ? 33 : 34;
  for (let i = 0; i < bodyLen; i++) {
    const n = String(startNum + i).padStart(3, "0");
    let line = `  LINE-${n} codex${n}`;
    if (pill && i === bodyLen - 1) line += "   ↓ Back to bottom · esc";
    rows.push(line);
  }
  rows.push("\x1b[48;2;52;56;64m                                        \x1b[0m");
  rows.push("\x1b[48;2;52;56;64m› Ask Codex to do anything              \x1b[0m");
  rows.push("\x1b[48;2;52;56;64m                                        \x1b[0m");
  rows.push("  GPT-6.2 Sol medium fast · Context 3% used");
  rows.push("  ← for agents · ? for shortcuts");
  return rows;
}

function makeAgyFrame(startNum, { sticky = false } = {}) {
  const rows = [];
  if (sticky) {
    rows.push("\x1b[0m\x1b[1m\x1b[38;5;4m> Print 100 lines...\x1b[0m");
    rows.push("────────────────────────────────────────────────────────");
  }
  const bodyLen = sticky ? 34 : 36;
  for (let i = 0; i < bodyLen; i++) {
    const n = String(startNum + i).padStart(3, "0");
    rows.push(`  LINE-${n} agy${n}`);
  }
  rows.push("────────────────────────────────────────────────────────");
  rows.push(">");
  rows.push("────────────────────────────────────────────────────────");
  rows.push("? for shortcuts                                 auto");
  return rows;
}

test("applyFramePatch decodes full text and shifted row patches identically", () => {
  const prev = ["HDR", "L10", "L11", "L12", "L13", "FTR"];
  const patched = applyFramePatch(prev, {
    n: 6,
    shift: -2,
    patch: [[0, "HDR"], [1, "L08"], [2, "L09"], [5, "FTR"]],
  });
  assert.deepEqual([...patched], ["HDR", "L08", "L09", "L10", "L11", "FTR"]);
});

test("polishClaudeRow0 lifts unreadable dark-on-dark Claude sticky header only", () => {
  const raw = "\x1b[0m\x1b[38;2;80;80;80m\x1b[48;2;55;55;55m❯ Print 100 lines…\x1b[0m";
  const fixed = polishClaudeRow0(raw, "claude");
  assert.ok(fixed.includes("38;2;225;225;225"), "lifted fg for readability");
  assert.ok(!fixed.includes("38;2;80;80;80"));
  // Codex colours stay completely untouched
  assert.equal(polishClaudeRow0(raw, "codex"), raw);
});

test("segmentFrame separates sticky headers and bottom chrome for Claude, Codex, and agy", () => {
  const c = segmentFrame(makeClaudeFrame(50, { sticky: true }), "claude");
  assert.equal(c.topCount, 1);
  assert.equal(c.botPin.length, 7);
  assert.equal(c.body.length, 32);

  const x = segmentFrame(makeCodexFrame(50, { sticky: true }), "codex");
  assert.equal(x.topCount, 1);
  assert.equal(x.botPin.length, 5);
  assert.equal(x.body.length, 33);

  const a = segmentFrame(makeAgyFrame(50, { sticky: true }), "agy");
  assert.equal(a.topCount, 2);
  assert.equal(a.botPin.length, 4);
  assert.equal(a.body.length, 34);
});

test("scrolling up stitches lines into cache, then scrolling back down and up hits cache immediately", () => {
  const cache = createScreenCache();
  // Frame 0: bottom (LINE-068..100, 33 body rows)
  const r0 = cache.ingest(makeClaudeFrame(68, { sticky: false }), { pane: "w1:p1", kind: "claude", cols: 120, rows: 40, hist: 110, now: 100 });
  assert.equal(r0.usedCache, false);
  assert.equal(cache.state().cached, 33);

  // Cold scroll UP 3 lines -> cache miss, requests prefetch burst
  const s1 = cache.scroll(-3, 200);
  assert.equal(s1.hit, false);
  assert.ok(s1.sendTicks <= -8, "prefetches up to BURST lines");

  // Authoritative frame arrives shifted up by 8 lines (LINE-060..091, with sticky row & overlay pill)
  const r1 = cache.ingest(makeClaudeFrame(60, { sticky: true, pill: true }), { pane: "w1:p1", kind: "claude", cols: 120, rows: 40, now: 210 });
  assert.equal(cache.state().cached, 41, "prepended 8 new older lines");
  // User only asked for 3 lines up, so 5 prefetched lines remain above viewTop!
  assert.equal(cache.state().viewTop, 5);
  assert.equal(r1.usedCache, true);
  assert.ok(r1.renderLines[1].includes("LINE-065"), `expected LINE-065 at row 1, got ${r1.renderLines[1]}`);
  assert.ok(!r1.renderLines[32].includes("Jump to bottom"), "clean cached line preserved without overlay pill");

  // Next scroll UP 3 lines -> INSTANT CACHE HIT (0 network wait!)
  const s2 = cache.scroll(-3, 230);
  assert.equal(s2.hit, true);
  assert.ok(s2.renderLines[1].includes("LINE-062"), `expected LINE-062, got ${s2.renderLines[1]}`);
  assert.equal(s2.needPrefetch, true, "triggers predictive prefetch near top edge");

  // Scroll back DOWN 3 lines -> INSTANT CACHE HIT
  const s3 = cache.scroll(3, 250);
  assert.equal(s3.hit, true);
  assert.ok(s3.renderLines[1].includes("LINE-065"));

  // Scroll all the way back to bottom -> goHome = true
  const s4 = cache.scroll(20, 280);
  assert.equal(s4.hit, true);
  assert.equal(s4.goHome, true);
  assert.equal(cache.state().atBottom, true);
});

test("scrollbar reflects real position and supports dragging across cached and unseen regions", () => {
  const cache = createScreenCache();
  cache.ingest(makeCodexFrame(68, { sticky: true }), { pane: "w2:p1", kind: "codex", cols: 120, rows: 40, hist: 120, now: 100 });
  const sb0 = cache.scrollbar();
  assert.equal(sb0.atBottom, true);
  assert.equal(sb0.ratio, 1);
  assert.ok(sb0.thumbRatio > 0.2 && sb0.thumbRatio < 0.4, `thumbRatio=${sb0.thumbRatio}`);

  // Ingest 30 more lines upward so we have 63 cached lines
  cache.scroll(-10, 120);
  cache.ingest(makeCodexFrame(38, { sticky: true }), { pane: "w2:p1", kind: "codex", cols: 120, rows: 40, now: 130 });
  const sb1 = cache.scrollbar();
  assert.ok(sb1.ratio < 1, "ratio moved up from bottom");

  // Dragging near top triggers pageUp/wheel fetch into unseen history
  const dragTop = cache.scrollToRatio(0.05, 160);
  assert.equal(dragTop.hit, true);
  assert.ok(dragTop.pageUp > 0 || dragTop.sendTicks < 0, "requests unseen older lines");

  // Dragging to bottom returns cleanly to live bottom
  const dragBot = cache.scrollToRatio(1.0, 200);
  assert.equal(dragBot.goHome, true);
  assert.equal(cache.scrollbar().atBottom, true);
});

test("dedicated scroll-indicator rows (Codex Back-to-bottom, agy ↓ N lines) never bake into cached body", () => {
  const codexScrolled = makeCodexFrame(40, { sticky: true });
  codexScrolled[33] = "                    ↓ Back to bottom · esc                    ";
  const segC = segmentFrame(codexScrolled, "codex");
  assert.equal(segC.scrolledIndicator, true);
  assert.ok(!segC.body.some((l) => l.includes("Back to bottom")), "Back to bottom kept in botPin");

  const agyScrolled = makeAgyFrame(40, { sticky: true });
  agyScrolled[35] = "                                                         ↓ 44 lines";
  const segA = segmentFrame(agyScrolled, "agy");
  assert.equal(segA.scrolledIndicator, true);
  assert.ok(!segA.body.some((l) => l.includes("44 lines")), "↓ 44 lines kept in botPin");

  // Claude's 1-row blank spacer above the bottom rule belongs to botPin so
  // scrolling up never injects blank holes into cached transcript lines.
  const claudeWithSpacer = makeClaudeFrame(60, { sticky: true, pill: false });
  claudeWithSpacer[31] = "  LINE-090 word090   \x1b[48;2;55;55;55m Jump to bottom (ctrl+End) ↓ \x1b[0m";
  claudeWithSpacer[32] = "";
  const segCl = segmentFrame(claudeWithSpacer, "claude");
  assert.equal(segCl.scrolledIndicator, true);
  assert.equal(segCl.botPin.length, 8);
  assert.equal(segCl.body.length, 31);
  assert.ok(segCl.body.at(-1).includes("LINE-090"));

  // Pane opened while already scrolled up to top still shows a scrollbar with unknownBelow
  const c = createScreenCache();
  const topFrame = makeClaudeFrame(1, { sticky: false, pill: true });
  topFrame[0] = "  Claude Code v2.1.302";
  c.ingest(topFrame, { pane: "w3:p1", kind: "claude", cols: 120, rows: 40, hist: 120, now: 100 });
  const sb = c.scrollbar();
  assert.equal(sb.visible, true);
  assert.equal(sb.atBottom, false);
  assert.ok(sb.unknownBelow > 0);
});

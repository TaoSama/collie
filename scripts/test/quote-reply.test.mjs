import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
function section(start, end) {
  const first = app.indexOf(start);
  const last = app.indexOf(end, first + start.length);
  assert.ok(first >= 0 && last > first, `missing section: ${start}`);
  return app.slice(first, last);
}

const quoteSrc = [
  section("  // Markdown as one plain line, for quotes:", "  // A user message's attachments:"),
  section("  const quoteLine = (q, attr = \"\")", "  // One of your messages or Collie's"),
  section("  const QUOTE_PREFIX_RE =", "  function bindComposer("),
  section("  // Quoting a message: rides along with what you send next", "  async function sendSuper("),
].join("\n");

function makeEl(id = "", { classes = [], dataset = {}, text = "", parent = null, hidden = false, tag = "DIV" } = {}) {
  const listeners = new Map();
  const children = [];
  const cls = new Set(classes);
  const el = {
    id,
    tagName: tag,
    hidden,
    dataset: { ...dataset },
    value: "",
    textContent: text,
    innerText: text,
    scrollTop: 0,
    parentElement: parent,
    focused: 0,
    children,
    classList: {
      contains: (c) => cls.has(c),
      add: (c) => cls.add(c),
      remove: (c) => cls.delete(c),
    },
    addEventListener(type, cb) {
      listeners.set(type, [...(listeners.get(type) || []), cb]);
    },
    emit(type, ev = {}) {
      for (const cb of listeners.get(type) || []) cb(ev);
    },
    focus() { el.focused++; },
    setSelectionRange() {},
    cloneNode() {
      return {
        innerText: el.innerText,
        textContent: el.textContent,
        querySelectorAll: () => [],
      };
    },
    closest(sel) {
      let cur = el;
      while (cur) {
        if (matches(cur, sel)) return cur;
        cur = cur.parentElement;
      }
      return null;
    },
  };
  if (parent) parent.children.push(el);
  return el;
}

function matches(el, sel) {
  if (sel.trim() === ":is(#super-thread, #run-thread, #thread, #x-thread) .msg") {
    return el.classList.contains("msg") && !!el.closest("#super-thread, #run-thread, #thread, #x-thread");
  }
  for (const raw of sel.split(",").map((s) => s.trim())) {
    if (raw === ".re-btn[data-re]") {
      if (el.classList.contains("re-btn") && el.dataset.re) return true;
      continue;
    }
    if (raw.startsWith("#") && el.id === raw.slice(1)) return true;
    if (raw.startsWith(".") && el.classList.contains(raw.slice(1))) return true;
    if (/^[a-z]+$/i.test(raw) && el.tagName === raw.toUpperCase()) return true;
  }
  return false;
}

function createHarness() {
  const docListeners = new Map();
  let now = 0;
  let nextTimerId = 1;
  const timers = new Map();
  const fakeSetTimeout = (fn, ms) => {
    const id = nextTimerId++;
    timers.set(id, { fn, at: now + ms });
    return id;
  };
  const fakeClearTimeout = (id) => timers.delete(id);
  const advance = (ms) => {
    now += ms;
    for (const [id, t] of [...timers.entries()]) {
      if (t.at <= now) {
        timers.delete(id);
        t.fn();
      }
    }
  };

  let selectionText = "";
  let selectionCleared = 0;

  function makeDraftUI(formId, boxId, inputId) {
    const form = makeEl(formId);
    const box = makeEl(boxId, { hidden: true });
    const who = makeEl("", { classes: ["re-who"], parent: box });
    const text = makeEl("", { classes: ["re-t"], parent: box });
    const cancel = makeEl("", { tag: "BUTTON", parent: box });
    const input = makeEl(inputId, { tag: "TEXTAREA" });
    const draft = { reply: null };
    form._selectDraft = () => draft;
    return { form, box, who, text, cancel, input, draft };
  }

  const supUI = makeDraftUI("super-composer", "re-draft", "super-input");
  const paneUI = makeDraftUI("composer", "pane-re-draft", "input");
  const extUI = makeDraftUI("x-composer", "x-re-draft", "x-input");
  const runUI = makeDraftUI("run-composer", "run-re-draft", "run-input");

  const bySel = new Map([
    ["#super-composer", supUI.form], ["#re-draft", supUI.box], ["#re-draft button", supUI.cancel], ["#super-input", supUI.input],
    ["#composer", paneUI.form], ["#pane-re-draft", paneUI.box], ["#pane-re-draft button", paneUI.cancel], ["#input", paneUI.input],
    ["#x-composer", extUI.form], ["#x-re-draft", extUI.box], ["#x-re-draft button", extUI.cancel], ["#x-input", extUI.input],
    ["#run-composer", runUI.form], ["#run-re-draft", runUI.box], ["#run-re-draft button", runUI.cancel], ["#run-input", runUI.input],
  ]);

  const entries = [
    { id: "e1", role: "collie", text: "**方案说明**：先出样机再实现" },
    { id: "e2", role: "user", text: "好的，按这个来" },
  ];

  const doc = {
    addEventListener(type, cb) {
      docListeners.set(type, [...(docListeners.get(type) || []), cb]);
    },
    emit(type, ev = {}) {
      const event = {
        isPrimary: true,
        pointerType: "mouse",
        button: 0,
        clientX: 100,
        clientY: 100,
        defaultPrevented: false,
        stopped: false,
        preventDefault() { this.defaultPrevented = true; },
        stopPropagation() { this.stopped = true; },
        ...ev,
      };
      for (const cb of docListeners.get(type) || []) cb(event);
      return event;
    },
  };

  const ctx = vm.createContext({
    $: (sel, root) => {
      if (root) {
        const ui = [supUI, paneUI, extUI, runUI].find((u) => u.box === root);
        if (ui && sel === ".re-who") return ui.who;
        if (ui && sel === ".re-t") return ui.text;
        return null;
      }
      return bySel.get(sel) || null;
    },
    document: doc,
    window: {
      getSelection: () => ({
        toString: () => selectionText,
        removeAllRanges: () => { selectionText = ""; selectionCleared++; },
      }),
    },
    setTimeout: fakeSetTimeout,
    clearTimeout: fakeClearTimeout,
    esc: (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"),
    withImages: (s) => s,
    linkUserFiles: (s) => s,
    IMG_EXT: /\.(?:png|jpe?g|gif|webp|svg)$/i,
    thumbBtn: (p) => `[thumb:${p}]`,
    fileLinkAttrs: (p) => `data-path="${p}"`,
    chatEntries: () => entries,
    ext: [{ id: "ext1", kind: "codex", name: "Codex Session" }],
    extCur: "ext1",
    current: "w1:p1",
    find: () => ({ pane_id: "w1:p1", kind: "claude", name: "worker-a" }),
    title: (v) => v.name,
  });

  vm.runInContext(
    quoteSrc + "\nglobalThis.exports = { withQuotePrefix, userText, clipQuote, setReply };",
    ctx
  );

  return {
    ctx, doc, advance, supUI, paneUI, extUI, runUI,
    setSelection(s) { selectionText = s; },
    getSelectionCleared: () => selectionCleared,
  };
}

test("long-pressing a Collie or user message quotes directly into composer and focuses input", () => {
  const { doc, advance, supUI } = createHarness();
  const viewer = makeEl("", { classes: ["viewer"] });
  const superThread = makeEl("super-thread", { parent: viewer });
  const collieMsg = makeEl("", { classes: ["msg", "assistant"], dataset: { id: "e1" }, text: "方案说明：先出样机再实现", parent: superThread });
  const userMsg = makeEl("", { classes: ["msg", "user"], dataset: { id: "e2" }, text: "好的，按这个来", parent: superThread });

  // Desktop mouse long-press on Collie message
  doc.emit("pointerdown", { target: collieMsg, pointerType: "mouse", button: 0, clientX: 120, clientY: 200 });
  advance(479);
  assert.equal(supUI.box.hidden, true);
  advance(1);
  assert.equal(supUI.box.hidden, false);
  assert.equal(supUI.who.textContent, "Collie");
  assert.equal(supUI.text.textContent, "方案说明：先出样机再实现");
  assert.equal(supUI.input.focused, 1);
  assert.equal(supUI.draft.reply.entry, "e1");
  // No message highlight class is added
  assert.equal(collieMsg.classList.contains("quote-picking"), false);

  // Trailing click after long-press is suppressed
  doc.emit("pointerup", { target: collieMsg });
  const clickEv = doc.emit("click", { target: collieMsg });
  assert.equal(clickEv.defaultPrevented, true);
  assert.equal(clickEv.stopped, true);

  // Cancel quote via × button
  supUI.cancel.emit("click");
  assert.equal(supUI.box.hidden, true);
  assert.equal(supUI.draft.reply, null);

  // Touch long-press on user's own message quotes with "你" and refocuses on touchend
  doc.emit("pointerdown", { target: userMsg, pointerType: "touch", clientX: 150, clientY: 320 });
  advance(480);
  assert.equal(supUI.box.hidden, false);
  assert.equal(supUI.who.textContent, "你");
  assert.equal(supUI.text.textContent, "好的，按这个来");
  assert.equal(supUI.input.focused, 2);
  const touchEndEv = doc.emit("touchend", { target: userMsg });
  assert.equal(touchEndEv.defaultPrevented, true);
  assert.equal(supUI.input.focused, 3);
});

test("long-pressing an agent session message quotes into pane composer, and read-only ext ignores it", () => {
  const { doc, advance, paneUI, extUI } = createHarness();
  const thread = makeEl("thread", { classes: ["thread"] });
  const agMsg = makeEl("", { classes: ["msg", "assistant"], text: "已完成测试修复", parent: thread });

  doc.emit("pointerdown", { target: agMsg, pointerType: "mouse", clientX: 80, clientY: 140 });
  advance(480);
  assert.equal(paneUI.box.hidden, false);
  assert.equal(paneUI.who.textContent, "Claude");
  assert.equal(paneUI.text.textContent, "已完成测试修复");
  assert.equal(paneUI.input.focused, 1);

  // Read-only external session (#x-composer hidden) does not trigger
  extUI.form.hidden = true;
  const xThread = makeEl("x-thread");
  const xMsg = makeEl("", { classes: ["msg", "assistant"], text: "只读会话内容", parent: xThread });
  doc.emit("pointerdown", { target: xMsg, pointerType: "touch", clientX: 80, clientY: 140 });
  advance(500);
  assert.equal(extUI.box.hidden, true);

  // Writable external session triggers
  extUI.form.hidden = false;
  doc.emit("pointerdown", { target: xMsg, pointerType: "touch", clientX: 80, clientY: 140 });
  advance(480);
  assert.equal(extUI.box.hidden, false);
  assert.equal(extUI.who.textContent, "Codex");
  assert.equal(extUI.input.focused, 1);
});

test("conflict guards: drag selection, scroll, interactive elements, active selection, and inbox do not trigger", () => {
  const { doc, advance, supUI, setSelection } = createHarness();
  const viewer = makeEl("", { classes: ["viewer"] });
  const superThread = makeEl("super-thread", { parent: viewer });
  const msg = makeEl("", { classes: ["msg", "assistant"], dataset: { id: "e1" }, text: "正文消息", parent: superThread });
  const thumb = makeEl("", { classes: ["thumb"], parent: msg });
  const link = makeEl("", { tag: "A", parent: msg });
  const inbox = makeEl("inbox-detail");
  const inboxMsg = makeEl("", { classes: ["msg", "assistant"], text: "飞书消息", parent: inbox });

  // 1) Mouse drag > 6px cancels long-press (preserves text selection)
  doc.emit("pointerdown", { target: msg, pointerType: "mouse", clientX: 100, clientY: 100 });
  doc.emit("pointermove", { target: msg, pointerType: "mouse", clientX: 112, clientY: 100 });
  advance(500);
  assert.equal(supUI.box.hidden, true);

  // 2) Thread scroll > 8px cancels long-press; <= 8px jitter does not
  viewer.scrollTop = 100;
  doc.emit("pointerdown", { target: msg, pointerType: "touch", clientX: 100, clientY: 100 });
  viewer.scrollTop = 120;
  doc.emit("scroll", { target: viewer });
  advance(500);
  assert.equal(supUI.box.hidden, true);

  // 3) Interactive / media elements are ignored
  for (const target of [thumb, link, inboxMsg]) {
    doc.emit("pointerdown", { target, pointerType: "touch", clientX: 100, clientY: 100 });
    advance(500);
    assert.equal(supUI.box.hidden, true);
  }

  // 4) Pre-existing text selection prevents triggering
  setSelection("selected text");
  doc.emit("pointerdown", { target: msg, pointerType: "mouse", clientX: 100, clientY: 100 });
  advance(500);
  assert.equal(supUI.box.hidden, true);
  setSelection("");

  // 5) Existing hover .re-btn button still works alongside long-press
  const reBtn = makeEl("", { classes: ["re-btn"], dataset: { re: "e1" }, tag: "BUTTON", parent: msg });
  doc.emit("click", { target: reBtn });
  assert.equal(supUI.box.hidden, false);
  assert.equal(supUI.draft.reply.entry, "e1");
});

test("agent quote prefix formats cleanly and userText renders it as a quote block", () => {
  const { ctx } = createHarness();
  const { withQuotePrefix, userText } = ctx.exports;
  const longText = "第一行 **加粗** " + "字".repeat(200);
  const formatted = withQuotePrefix("按这个改动实现", { who: "Claude", text: longText });
  assert.match(formatted, /^> 引用 Claude：第一行 加粗 字+…\n\n按这个改动实现$/);

  const html = userText(formatted);
  assert.match(html, /^<div class="re"\s+title="[^"]+"><span class="re-who">Claude<\/span>第一行 加粗 /);
  assert.ok(html.endsWith("按这个改动实现"));
  assert.doesNotMatch(html, /&gt; 引用/);
});

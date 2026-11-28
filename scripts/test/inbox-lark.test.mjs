import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const source = (name) => readFileSync(new URL("../../web/" + name, import.meta.url), "utf8");
const topic = { id: "root", thread_id: "thread", text: "发布说明", reactions: [{ key: "THUMBSUP", count: 2 }, { key: "HEART", count: 1 }] };
const nextTick = () => new Promise((resolve) => setImmediate(resolve));

function eventTarget(props = {}) {
  const handlers = new Map();
  return {
    ...props,
    addEventListener: (name, listener) => handlers.set(name, listener),
    dispatch(name, data = {}) {
      const event = { target: this, preventDefault() { this.defaultPrevented = true; }, ...data };
      handlers.get(name)?.(event);
      return event;
    },
  };
}

function fixture({ topicMode = false, forwardError } = {}) {
  let now = 1000, timer, focusCount = 0;
  const calls = [], toasts = [];
  const select = { value: "larkx:destination" };
  const dialog = eventTarget({
    open: false,
    style: { setProperty() {} },
    classList: { toggle() {} },
    setAttribute() {},
    showModal() { this.open = true; },
    close() { this.open = false; },
    querySelector(sel) { return sel === "[data-lark-forward-target]" ? select : null; },
    contains: () => true,
  });
  const document = eventTarget({ body: { append() {} }, createElement: () => dialog });
  const root = eventTarget({ querySelector: () => ({ focus: () => focusCount++ }) });
  const window = { collieIcons: { svg: () => "" }, innerWidth: 400, innerHeight: 800, getSelection: () => null };
  const ctx = {
    window, document, performance: { now: () => now },
    setTimeout: (fn) => { timer = fn; return 1; },
    clearTimeout: () => { timer = null; },
    collieInboxMessage: { legacyPreview: (s) => String(s || "") },
  };
  vm.runInNewContext(source("inbox-lark.js"), ctx);
  const lark = window.collieInboxLark;
  lark.init((s) => String(s ?? ""));
  const conv = { id: "larkx:g", ext: "larkx", kind: "group" };
  const detail = { conv, msgs: [topic], lark: { topic_mode: topicMode, emoji: [{ key: "HEART", label: "爱心" }] } };
  const binding = lark.bind(root, {
    detail: () => detail,
    convs: () => [{ id: select.value, ext: "larkx" }],
    action: async (body) => {
      calls.push(body);
      if (body.action === "forward" && forwardError) throw forwardError;
      return { msgs: [topic] };
    },
    draw() {}, busy() {}, replyMode() {}, topicChanged() {},
    topicBack: () => false,
    reload: async () => {},
    toast: (...args) => toasts.push(args),
  });
  binding.reset(conv.id);
  const msg = {
    isConnected: true,
    getBoundingClientRect: () => ({ left: 0, top: 700, bottom: 760 }),
    closest(sel) {
      if (sel === "[data-lark-message]") return this;
      if (sel === "[data-msg-id]") return { dataset: { msgId: topic.id } };
      return null;
    },
  };
  const button = (dataset = {}, attrs = []) => ({
    dataset,
    closest: (sel) => sel === "[data-msg-id]" ? msg.closest(sel) : null,
    hasAttribute: (attr) => attrs.includes(attr),
    getBoundingClientRect: msg.getBoundingClientRect,
  });
  const click = (b) => dialog.dispatch("click", { target: { closest: () => b } });
  return {
    root, document, dialog, msg, button, click, binding, calls, toasts, lark, detail,
    focusCount: () => focusCount,
    fireTimer: () => { timer?.(); timer = null; },
    advance: (ms) => { now += ms; },
  };
}

test("releasing a long touch on a menu button is swallowed even after the initial guard window", async () => {
  const f = fixture();
  const reaction = f.button({ larkEmoji: "HEART" });
  f.root.dispatch("pointerdown", { target: f.msg, pointerType: "touch", isPrimary: true });
  f.fireTimer();
  assert.equal(f.dialog.open, true);
  f.advance(1500);
  assert.equal(f.click(reaction).defaultPrevented, true, "menu is guarded while the finger is still down");
  f.document.dispatch("pointerup");
  assert.equal(f.document.dispatch("touchend").defaultPrevented, true);
  assert.equal(f.click(reaction).defaultPrevented, true, "the release's click is also guarded");
  assert.equal(f.calls.length, 0);
  f.dialog.dispatch("pointerdown");
  f.click(reaction);
  await nextTick();
  assert.equal(f.calls[0].action, "reaction");
});

test("scrolling the conversation cancels a pending long touch", () => {
  const f = fixture();
  f.root.dispatch("pointerdown", { target: f.msg, pointerType: "touch", isPrimary: true });
  f.root.dispatch("scroll");
  f.fireTimer();
  assert.equal(f.dialog.open, false);
  assert.equal(f.document.dispatch("touchend").defaultPrevented, undefined);
});

test("forward closes the dialog on a structured uncertain result regardless of wording", async () => {
  for (const status of ["uncertain", "rejected"]) {
    const err = Object.assign(new Error("请先到目标会话查看"), { code: status });
    const f = fixture({ forwardError: err });
    f.root.dispatch("contextmenu", { target: f.msg });
    f.click(f.button({ larkMenuAction: "forward" }));
    f.click(f.button({}, ["data-lark-forward-confirm"]));
    await nextTick();
    assert.equal(f.calls.length, 1);
    assert.equal(f.calls[0].action, "forward");
    assert.equal(f.dialog.open, status !== "uncertain");
    assert.equal(f.toasts[0][0], err.message);
  }
});

test("topic-card reply opens and focuses comments without leaving a quote on the feed", async () => {
  const f = fixture({ topicMode: true });
  f.root.dispatch("contextmenu", { target: f.msg });
  f.click(f.button({ larkMenuAction: "quote" }));
  await nextTick();
  assert.equal(f.focusCount() > 0, true);
  assert.equal(f.binding.opts().topicRoot.id, topic.id);
  assert.equal(f.binding.opts().larkReply, null);
  assert.deepEqual({ ...f.binding.sendOpts() }, { reply_to: topic.id, reply_mode: "comment" });
  f.binding.click(f.button({ larkBack: "" }));
  assert.deepEqual({ ...f.binding.sendOpts() }, { reply_mode: "publish" });
});

test("the topic feed jumps to the unread card without a chronological divider or a duplicate like", () => {
  const f = fixture({ topicMode: true });
  const older = { id: "old-root", thread_id: "old-thread", text: "早期话题" };
  const unreadComment = { id: "comment", thread_id: topic.thread_id, root_id: topic.id };
  const html = f.lark.topicFeed([older, topic, unreadComment], f.detail.conv, f.detail.lark,
    () => "", () => "", "all", unreadComment.id, 1);
  assert.match(html, /data-msg-id="root" data-unread-anchor/);
  assert.equal(html.indexOf('data-msg-id="root"') < html.indexOf('data-msg-id="old-root"'), true);
  assert.doesNotMatch(html, /conv-unread-line|以下是|data-lark-reaction="THUMBSUP"/);
  assert.match(html, /data-lark-reaction="HEART"/);
  assert.match(html, /data-lark-like[^>]*><span>点赞 2/);
});

test("confirming an AI draft retains the selected topic reply or publish payload", async () => {
  const app = source("app.js");
  const start = app.indexOf("  const inboxReplies = new Set();");
  const end = app.indexOf("  const inboxCardActions = new Set();", start);
  assert.ok(start >= 0 && end > start);
  for (const sendOpts of [{ reply_to: topic.id, reply_mode: "comment" }, { reply_mode: "publish" }]) {
    const calls = [];
    const ctx = {
      inboxCur: "larkx:g", inboxDetail: { conv: { label: "飞书" } }, inboxDraft: { text: "AI 草稿" },
      inboxInput: "", inboxDraftSeq: 0, inboxTopicContext: "", inboxMode: "ai",
      inboxLark: { sendOpts: () => sendOpts, sent() {} },
      $: (sel) => sel.includes("draft-text") ? { value: "AI 草稿" } : { checked: true },
      selectInboxFiles: () => ({ files: [] }), inboxFileBox: null,
      inboxReferenceOnly: () => true, busy() {}, toast() {},
      api: async (path, opts) => { calls.push({ path, body: { ...opts.body, files: [...opts.body.files] } }); return { state: "sent" }; },
      inboxUnread: null, inboxNeedBottom: false, inboxNeedTop: false, inboxLoading: null,
      loadConv() {}, fetchAuto() {}, view: "auto", autoTab: "inbox", autoData: {}, drawAuto() {},
    };
    await vm.runInNewContext(app.slice(start, end) + "\nsendInboxReply({}, true)", ctx);
    assert.deepEqual(calls, [{
      path: "/api/inbox/reply",
      body: { id: "larkx:g", text: "AI 草稿", ai_footer: true, files: [], ...sendOpts },
    }]);
  }
});

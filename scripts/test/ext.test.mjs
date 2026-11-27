// web/ext.js: extension list ordering and WeChat nickname/avatar settings.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {} };
ctx.collieInboxMessage = ctx.window.collieInboxMessage = {
  render: (v) => String(v?.text || ""),
  legacyPreview: (s) => String(s || ""),
};
vm.runInNewContext(readFileSync(new URL("../../web/icons.js", import.meta.url), "utf8"), ctx);
vm.runInNewContext(readFileSync(new URL("../../web/machines.js", import.meta.url), "utf8"), ctx);
vm.runInNewContext(readFileSync(new URL("../../web/ext.js", import.meta.url), "utf8"), ctx);
const ext = ctx.window.collieExt;
ext.init({
  esc: (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])),
  when: (t) => String(t ?? ""),
  md: (s) => String(s ?? ""),
  workerHref: () => "",
});

test("hooksList places built-in extensions before marketplace and local extensions", () => {
  const html = ext.hooksList({
    extensions: [
      { manifest: { id: "aster-statusline", name: "Aster", market: { repo: "o/r" } }, setting: { enabled: false } },
      { manifest: { id: "workbench", name: "工作台", runtime: { feature: "workbench" } }, setting: { enabled: true } },
      { manifest: { id: "000-local", name: "本地", local: true }, setting: { enabled: false } },
      { manifest: { id: "wechat", name: "微信", runtime: { builtin: "wechat" } }, setting: { enabled: true } },
      { manifest: { id: "larkx", name: "飞书", runtime: { command: "bin/larkx" } }, setting: { enabled: true } },
      { manifest: { id: "notes", name: "笔记", market: { repo: "o/r" } }, setting: { enabled: false } },
    ],
    machines: [],
  }, { convs: [] }, { markets: [] });
  const ids = [...html.matchAll(/href="#\/auto\/hooks\/ext\/([^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(ids, ["larkx", "wechat", "workbench", "aster-statusline", "notes", "000-local"]);
});

test("extPage shows WeChat nickname and avatar controls beside it, and not on other extensions", () => {
  const wechatManifest = JSON.parse(readFileSync(new URL("../../extensions/wechat/collie-extension.json", import.meta.url), "utf8"));
  assert.equal(wechatManifest.configuration.properties.conversation_name.title, "昵称");

  const base = {
    manifest: { ...wechatManifest, icon_url: "/api/ext/wechat/icon?v=1" },
    setting: { id: "wechat", enabled: true, machine: "", config: { conversation_name: "微信ClawBot" } },
  };
  ext.learn({ extensions: [base] });

  const unsetHTML = ext.extPage(base, { machines: [] }, null, { convs: [] });
  assert.match(unsetHTML, /<label>昵称<input data-ext-config="conversation_name"/);
  assert.doesNotMatch(unsetHTML, /会话显示名/);
  assert.match(unsetHTML, /class="ext-avatar"><span>头像<\/span>/);
  assert.match(unsetHTML, /data-ext-avatar-pick>上传<\/button>/);
  assert.doesNotMatch(unsetHTML, /data-ext-avatar-clear/);

  const setHTML = ext.extPage({ ...base, avatar_url: "/api/ext/wechat/avatar?v=abc123" }, { machines: [] }, null, { convs: [] });
  assert.match(setHTML, /<img class="inbox-pic" src="\/api\/ext\/wechat\/avatar\?v=abc123"/);
  assert.match(setHTML, /data-ext-avatar-pick>更换<\/button>/);
  assert.match(setHTML, /data-ext-avatar-clear>恢复默认<\/button>/);

  const larkHTML = ext.extPage({
    manifest: { id: "larkx", name: "飞书", version: "1.0.0", runtime: { command: "bin/larkx" }, configuration: { type: "object", properties: { region: { type: "string", title: "站点" } } } },
    setting: { id: "larkx", enabled: true, machine: "" },
  }, { machines: [] }, null, { convs: [] });
  assert.doesNotMatch(larkHTML, /ext-avatar/);
});

test("hooksList and marketsHTML render loading skeletons and retry errors instead of blank or false empty states", () => {
  const hooksLoading = ext.hooksList(undefined, undefined, undefined, { extLoading: true, marketsLoading: true });
  assert.match(hooksLoading, /skel-row/);
  assert.match(hooksLoading, /正在读取扩展/);
  assert.match(hooksLoading, /正在读取市场/);
  assert.doesNotMatch(hooksLoading, /0 个市场/);
  assert.doesNotMatch(hooksLoading, /没有可用的扩展/);

  const hooksErr = ext.hooksList(undefined, undefined, undefined, { extErr: "503", marketsErr: "timeout" });
  assert.match(hooksErr, /读取扩展列表失败：503/);
  assert.match(hooksErr, /data-auto-retry="ext"/);
  assert.match(hooksErr, /读取市场失败/);

  const marketsLoading = ext.marketsHTML(undefined, { loading: true });
  assert.match(marketsLoading, /扩展市场/);
  assert.match(marketsLoading, /skel-row/);
  assert.match(marketsLoading, /正在读取扩展市场/);
  assert.doesNotMatch(marketsLoading, /还没有市场/);

  const marketsErr = ext.marketsHTML(undefined, { error: "network down" });
  assert.match(marketsErr, /读取扩展市场失败：network down/);
  assert.match(marketsErr, /data-auto-retry="markets"/);

  // Installed extension in market renders its official icon when known.
  ext.learn({ extensions: [{ manifest: { id: "demo-ext", name: "Demo", icon_url: "/api/ext/demo-ext/icon" } }] });
  const marketsLoaded = ext.marketsHTML({
    markets: [{
      repo: "o/r",
      name: "Market",
      installed: { "demo-ext": "1.0.0" },
      index: [{ id: "demo-ext", name: "Demo", version: "1.1.0", description: "desc" }],
    }],
  });
  assert.match(marketsLoaded, /src="\/api\/ext\/demo-ext\/icon"/);
});

test("inbox search filters and highlights matches without dropping bot-chat DM exclusion", () => {
  const r = {
    trigger: "/ai",
    convs: [
      { id: "larkx:g1", ext: "larkx", chat: "g1", label: "飞书", name: "发布值班群", kind: "group", unread: 5, at_me: true, preview: "陈敏: 切 10% 流量", last_at: "2026-11-27T16:40:00+08:00" },
      { id: "larkx:g2", ext: "larkx", chat: "g2", label: "飞书", name: "数据库值班群", kind: "group", unread: 2, preview: "告警已恢复", last_at: "2026-11-27T16:30:00+08:00" },
      { id: "larkx:d1", ext: "larkx", chat: "d1", label: "飞书", name: "李四", kind: "dm", unread: 1, preview: "明天评审", last_at: "2026-11-27T16:20:00+08:00" },
      { id: "larkx:b1", ext: "larkx", chat: "b1", label: "飞书", name: "会议纪要助手", kind: "dm", tags: ["bot"], unread: 2, preview: "纪要已生成", last_at: "2026-11-27T16:10:00+08:00" },
    ],
  };
  assert.equal(ext.inboxShown(r, "larkx", "dm").length, 1);
  assert.equal(ext.inboxShown(r, "larkx", "", "值班").length, 2);
  const rows = ext.inboxListRowsHTML(r, "larkx:g1", "larkx", "", "值班");
  assert.match(rows, /<mark class="inbox-hl">值班<\/mark>/);
  const empty = ext.inboxListRowsHTML(r, "", "larkx", "", "不存在的词");
  assert.match(empty, /data-inbox-search-clear/);
});

test("convHTML renders chronological order, unread anchor + jump controls, human vs AI replies, and honest WeChat bot labels", () => {
  const larkDetail = {
    conv: { id: "larkx:g1", ext: "larkx", chat: "g1", label: "飞书", name: "发布值班群", kind: "group", send_as: "self", on: false },
    msgs: [
      { id: "m1", at: "2026-11-27T15:00:00+08:00", sender_name: "赵六", text: "第一条旧消息" },
      { id: "m2", at: "2026-11-27T15:05:00+08:00", mine: true, human: true, ref: "o1", text: "我本人直接回复" },
      { id: "m3", at: "2026-11-27T15:06:00+08:00", mine: true, ref: "o2", text: "AI 确认回复" },
    ],
    outbox: {},
  };
  const html = ext.convHTML(larkDetail, "/ai", {
    unreadAnchor: "m2",
    unreadCount: 2,
    composerMode: "reply",
  });
  assert.ok(html.indexOf("第一条旧消息") < html.indexOf("我本人直接回复") && html.indexOf("我本人直接回复") < html.indexOf("AI 确认回复"), "chronological order");
  assert.match(html, /data-unread-anchor><span>以下是 2 条未读消息/);
  assert.match(html, /data-conv-jump-unread[^>]*>.*2 条未读/);
  assert.match(html, /data-conv-jump-bottom/);
  assert.match(html, /我直接回复/);
  assert.match(html, /以你本人身份直接发送到飞书，不带 AI 尾标/);
  assert.doesNotMatch(html, /class="form conv-settings"/);

  // Direct human reply (human: true) has "你" and NOT "AI 代答"; AI reply (m3) has "AI 代答".
  const m2Chunk = html.slice(html.indexOf('data-msg-id="m2"'), html.indexOf('data-msg-id="m3"'));
  assert.match(m2Chunk, /<b>你<\/b>/);
  assert.doesNotMatch(m2Chunk, /AI 代答/);
  const m3Chunk = html.slice(html.indexOf('data-msg-id="m3"'));
  assert.match(m3Chunk, /AI 代答/);

  // AI draft confirmation card shows refine buttons, footer checkbox, and shared + attach button on Feishu.
  const aiHtml = ext.convHTML(larkDetail, "/ai", {
    composerMode: "ai",
    draft: { text: "收到，我会跟进。", footer: true, via: "Collie" },
  });
  assert.match(aiHtml, /data-conv-draft-refine="shorter"/);
  assert.match(aiHtml, /data-conv-draft-refine="formal"/);
  assert.match(aiHtml, /data-conv-draft-footer/);
  assert.match(aiHtml, /确认发送到飞书/);
  assert.match(aiHtml, /class="cbox conv-cbox"/);
  assert.match(aiHtml, /class="icon-btn attach"/);
  assert.match(aiHtml, /data-conv-mode="ai"/);

  // Shared + attach button also present in reply, ai prompt, and collie modes.
  assert.match(html, /class="cbox conv-cbox"/);
  assert.match(html, /class="icon-btn attach"/);
  const collieHtml = ext.convHTML(larkDetail, "/ai", { composerMode: "collie" });
  assert.match(collieHtml, /class="cbox conv-cbox"/);
  assert.match(collieHtml, /class="icon-btn attach"/);

  // WeChat bot chat omits AI 帮回 tab completely (only 2 tabs), honestly labels bot identity, and includes + attach button.
  const wxDetail = {
    conv: { id: "wechat:u1", ext: "wechat", chat: "u1", label: "微信", name: "微信ClawBot", kind: "dm", owner: true, send_as: "bot", send_media: "all", no_footer: true, on: true },
    msgs: [],
    outbox: {},
  };
  const wxHtml = ext.convHTML(wxDetail, "/ai", { composerMode: "reply" });
  assert.match(wxHtml, /class="conv-modes two"/);
  assert.doesNotMatch(wxHtml, /data-conv-mode="ai"/);
  assert.doesNotMatch(wxHtml, /AI 帮回/);
  assert.match(wxHtml, /机器人发送/);
  assert.match(wxHtml, /仅支持以机器人「微信ClawBot」身份发到你的微信（不能以你本人身份发送，支持文字\/图片\/文件）/);
  assert.match(wxHtml, /以机器人发送/);
  assert.match(wxHtml, /class="cbox conv-cbox" data-send-media="all"/);
  assert.match(wxHtml, /class="icon-btn attach"/);
  // Even if composerMode="ai" is requested on WeChat, it falls back to reply mode.
  const wxFallback = ext.convHTML(wxDetail, "/ai", { composerMode: "ai" });
  assert.match(wxFallback, /data-mode="reply"/);
  assert.doesNotMatch(wxFallback, /AI 帮回/);
});

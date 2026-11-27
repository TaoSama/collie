import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const context = vm.createContext({ window: { collieIcons: { svg: () => "<svg></svg>" } }, URL });
for (const file of ["markdown.js", "inbox-message.js"]) {
  vm.runInContext(readFileSync(new URL(`../../web/${file}`, import.meta.url), "utf8"), context);
}
const { render, legacyPreview } = context.window.collieInboxMessage;

test("renders nested card content and contextual controls", () => {
  const html = render({
    kind: "card", title: "审批进度", nodes: [
      { kind: "columns", children: [{ kind: "group", children: [{ kind: "markdown", text: "**待处理**" }] }] },
      { kind: "fields", children: [{ kind: "field", tone: "short", text: "申请人" }] },
      { kind: "actions", children: [
        { kind: "button", text: "查看", tone: "primary_filled", url: "https://example.com/detail" },
        { kind: "button", text: "通过" },
      ] },
    ],
  });
  assert.match(html, /<strong>审批进度<\/strong>/);
  assert.match(html, /im-columns.*im-group.*<strong>待处理<\/strong>/);
  assert.match(html, /im-field short">申请人/);
  assert.match(html, /href="https:\/\/example.com\/detail".*rel="noopener noreferrer">查看/);
  assert.match(html, /<button[^>]+disabled[^>]*>通过<\/button>/);
});

test("treats HTML, URLs and images in third-party content as untrusted", () => {
  const html = render({
    kind: "card", title: '<script>alert("title")</script>', url: "javascript:alert(1)",
    nodes: [
      { kind: "markdown", text: '<svg onload="alert(1)"></svg> ![tracker](https://evil.example/i.png) [local](file:///home/private.txt)' },
      { kind: "button", text: "坏链接", url: "javascript:alert(1)" },
      { kind: "image", url: "https://evil.example/remote.png" },
      { kind: "image", text: '图" onload="f()', url: "/api/inbox/media?id=a&key=k" },
    ],
  });
  assert.match(html, /&lt;script&gt;/);
  assert.match(html, /&lt;svg onload=&quot;/);
  assert.doesNotMatch(html, /<svg|<script|href="javascript|href="file:|evil\.example/);
  assert.match(html, /src="\/api\/inbox\/media\?id=a&amp;key=k"/);
  assert.match(html, /alt="图&quot; onload=&quot;f\(\)"/);
});

test("makes calendar, file, and clipped legacy previews readable", () => {
  assert.match(render({ kind: "calendar", title: "评审", start: "2026-11-27", all_day: true }), /评审.*2026-11-27.*全天/);
  assert.match(render({ kind: "media", nodes: [{ kind: "file", text: "报告.pdf", tone: "4096", url: "/api/inbox/media?id=1" }] }),
    /download>.*报告\.pdf.*4\.0 KB/);
  assert.equal(legacyPreview('飞书日历: [日历] {"title":{"element_ids":…'), "飞书日历: [日历]");
  assert.equal(legacyPreview("正常消息 [日历] 明天上午"), "正常消息 [日历] 明天上午");
});

test("renders inline emojis through hub proxy and stickers without bubble border", () => {
  const html = render({
    kind: "post",
    nodes: [{
      kind: "paragraph",
      children: [
        { kind: "text", text: "难哦" },
        { kind: "emoji", text: "流泪", url: "/api/inbox/avatar?id=larkx%3A1&u=https%3A%2F%2Fsf3-cn.feishucdn.com%2Fobj%2Flark-reaction-cn%2Femoji_sob.png&sig=abc" },
        { kind: "emoji", text: "外链", url: "https://evil.example/e.png" },
        { kind: "emoji", text: "未知" },
      ],
    }],
  });
  assert.match(html, /<img class="im-emoji" src="\/api\/inbox\/avatar\?[^"]+" alt="\[流泪\]" title="流泪"/);
  assert.match(html, /<span class="im-emoji-alt">\[外链\]<\/span>/);
  assert.match(html, /<span class="im-emoji-alt">\[未知\]<\/span>/);
  assert.doesNotMatch(html, /evil\.example/);

  const sticker = render({
    kind: "media",
    nodes: [{ kind: "image", text: "表情包", tone: "sticker", url: "/api/inbox/media?id=larkx%3A1&key=v3_abc" }],
  });
  assert.match(sticker, /<img class="im-image im-sticker" src="\/api\/inbox\/media\?[^"]+" alt="表情包"/);
});

test("renders interactive card buttons, selects, pickers, forms, and calendar RSVP dropdown", () => {
  const card = render({
    kind: "card", title: "报警处理",
    nodes: [
      { kind: "actions", children: [
        { kind: "button", text: "Approve", tone: "primary", action: "act_1", sig: "sig_1" },
        { kind: "button", text: "Reject", action: "act_2", sig: "sig_2" },
      ] },
      { kind: "select", placeholder: "Ack and Mute", action: "act_sel", sig: "sig_sel", options: [
        { text: "Mute 30m", value: "30" }, { text: "Mute 1h", value: "60" },
      ] },
      { kind: "select", placeholder: "选人", tone: "person", disabled: true },
      { kind: "overflow", options: [{ text: "详情", url: "https://example.com/a" }, { text: "忽略", action: "act_ov", sig: "sig_ov", value: "ig" }] },
      { kind: "date", action: "act_d", sig: "sig_d", value: "2026-11-30" },
      { kind: "form", children: [
        { kind: "input", name: "reason", placeholder: "备注" },
        { kind: "button", text: "提交", name: "submit", action: "act_f", sig: "sig_f" },
      ] },
    ],
  }, { convId: "larkx:c1", msgId: "m1" });
  assert.match(card, /data-im-conv="larkx:c1" data-im-msg="m1"/);
  assert.match(card, /data-im-act="card" data-im-id="act_1" data-im-sig="sig_1">Approve<\/button>/);
  assert.match(card, /<select class="im-select" data-im-act="card" data-im-id="act_sel" data-im-sig="sig_sel"/);
  assert.match(card, /title="选人组件请在飞书中使用"/);
  assert.match(card, /class="im-select im-overflow".*data-url="https:\/\/example\.com\/a"/);
  assert.match(card, /type="date" class="im-picker" value="2026-11-30"/);
  assert.match(card, /<form class="im-form" data-im-form>.*name="reason".*type="submit"/);

  const calReplied = render({
    kind: "calendar", title: "数据回流评审", start: "2026-11-30T11:00:00Z", end: "2026-11-30T12:00:00Z",
    conflict: true, status: "accept", action: "cal:uk1:0", sig: "sig_cal",
  }, { convId: "larkx:c2", msgId: "m2" });
  assert.match(calReplied, /与已有日程冲突/);
  assert.match(calReplied, /已回复.*<select class="im-select im-rsvp-select" data-pick="compact" data-im-act="calendar" data-im-id="cal:uk1:0" data-im-sig="sig_cal".*<option value="accept" selected>接受<\/option>/);

  const calPending = render({
    kind: "calendar", title: "切RTA需求", start: "2026-11-30T09:30:00Z", end: "2026-11-30T10:00:00Z",
    status: "needs_action", action: "cal:uk2:0", sig: "sig_cal2",
  }, { convId: "larkx:c2", msgId: "m3" });
  assert.doesNotMatch(calPending, /与已有日程冲突/);
  assert.match(calPending, /data-im-act="calendar".*data-im-val="accept">接受<\/button>/);
  assert.match(calPending, /data-im-val="decline">拒绝<\/button>/);
  assert.match(calPending, /data-im-val="tentative">待定<\/button>/);

  const calCancelled = render({ kind: "calendar", title: "已取消会议", start: "2026-11-30T09:30:00Z", status: "cancelled" });
  assert.match(calCancelled, /日程已取消/);
});

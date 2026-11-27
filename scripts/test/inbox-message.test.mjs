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

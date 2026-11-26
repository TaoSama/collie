// web/header_status.js: generic header status slot rendering & safety.
// node --test scripts/test/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {} };
vm.runInNewContext(readFileSync(new URL("../../web/header_status.js", import.meta.url), "utf8"), ctx);
const hs = ctx.window.collieHeaderStatus;

test("empty slots render nothing", () => {
  assert.equal(hs.render([]), "");
  assert.equal(hs.render(null), "");
});

test("renders desktop items, mini bars, subline, and popover sections safely", () => {
  const html = hs.render([{
    id: "meter",
    name: "Meter",
    label: "Meter",
    state: "ok",
    status: {
      title: "Meter <script>",
      compact: "5H 62% · 7D 30%",
      subline: "Reset 23:40",
      updated_at: "2026-11-26T14:00:00Z",
      items: [
        { label: "5H", value: "62%", percent: 62, tone: "ok" },
        { label: "7D", value: "30%", percent: 30, tone: "warn" },
      ],
      sections: [{
        title: "Pool",
        meta: "2/2",
        rows: [{
          label: "Primary",
          badge: "Pro",
          value: "62%",
          current: true,
          meters: [{ label: "5H", value: "62%", percent: 140, tone: "bad-tone" }],
        }],
      }],
    },
  }], { openId: "meter", when: () => "刚刚" });

  assert.match(html, /Meter &lt;script&gt;/);
  assert.doesNotMatch(html, /<script>/);
  assert.match(html, /width:62%/);
  assert.match(html, /width:100%/); // clamped from 140
  assert.doesNotMatch(html, /hs-bad-tone/); // unknown tones ignored
  assert.match(html, /hs-ok/);
  assert.match(html, /hs-warn/);
  assert.match(html, /Reset 23:40/);
  assert.match(html, /aria-expanded="true"/);
  assert.doesNotMatch(html, /data-hs-fold=/);
  assert.match(html, /刚刚/);
});

test("mobile summary always stays expanded without a collapse toggle", () => {
  const html = hs.render([{
    id: "m",
    name: "M",
    state: "ok",
    status: {
      compact: "Pool 2/2 · 5H 62% · 7D 30%",
      items: [{ label: "5H", value: "62%", percent: 62 }],
    },
  }]);
  assert.doesNotMatch(html, /is-folded/);
  assert.doesNotMatch(html, /hs-fold-btn/);
  assert.match(html, /hs-mobile-t">Pool 2\/2 · 5H 62% · 7D 30%</);
});

test("starting and error states render clear fallbacks", () => {
  const starting = hs.render([{ id: "s", name: "S", state: "starting" }]);
  assert.match(starting, /启动中/);
  const failed = hs.render([{ id: "e", name: "E", state: "error", error: "timeout" }]);
  assert.match(failed, /timeout/);
});

test("pinned state renders pin badge, scrim, and close button", () => {
  const html = hs.render([{ id: "p", name: "P", state: "ok", status: { items: [{ label: "A", value: "1" }] } }], { openId: "p", pinnedId: "p" });
  assert.match(html, /is-pinned/);
  assert.match(html, /已固定/);
  assert.match(html, /data-hs-close/);
  assert.match(html, /hs-scrim/);
});

test("renders collapsible sections with icons, sublabels, multi-badges, metrics, and folded rows", () => {
  const slot = {
    id: "pool",
    name: "Pool",
    state: "ok",
    status: {
      items: [{ label: "5H", value: "80%", percent: 80 }],
      sections: [{
        title: "Claude",
        meta: "3 个账号",
        icon: "claude",
        collapsible: true,
        rows: [
          {
            label: "Primary OAuth",
            sublabel: "user@example.com",
            badges: [
              { text: "Max 5x ($100)" },
              { text: "额度保护", tone: "warn" },
            ],
            metric: { label: "并发", value: "2/100", percent: 2 },
            meters: [{ label: "5H", value: "剩 80%", sub: "用 20% · 2h 后 (03:00)", percent: 80 }],
          },
          {
            label: "其余 2 个账号额度充足",
            detail: "所有窗口 ≥90%，状态正常",
            folded_rows: [
              { label: "healthy1@example.com", badges: [{ text: "Pro" }] },
              { label: "healthy2@example.com", badges: [{ text: "Free", tone: "muted" }] },
            ],
          },
        ],
      }],
    },
  };

  const expanded = hs.render([slot], {
    openId: "pool",
    icons: { claude: '<svg data-ico="claude"></svg>' },
  });
  assert.match(expanded, /data-hs-sec="pool:Claude"/);
  assert.match(expanded, /data-ico="claude"/);
  assert.match(expanded, /Primary OAuth/);
  assert.match(expanded, /hs-row-sub"[^>]*>user@example\.com</);
  assert.match(expanded, /Max 5x \(\$100\)/);
  assert.match(expanded, /hs-badge hs-warn">额度保护</);
  assert.match(expanded, /hs-row-metric/);
  assert.match(expanded, /2\/100/);
  assert.match(expanded, /data-hs-fold-row="pool:Claude:1"/);
  assert.doesNotMatch(expanded, /healthy1@example\.com/);

  // Expanding the folded row reveals its children.
  const withFoldOpen = hs.render([slot], {
    openId: "pool",
    expandedFolds: { "pool:Claude:1": true },
  });
  assert.match(withFoldOpen, /healthy1@example\.com/);
  assert.match(withFoldOpen, /healthy2@example\.com/);
  assert.match(withFoldOpen, />收起</);

  // Collapsing the section hides its rows.
  const collapsed = hs.render([slot], {
    openId: "pool",
    secState: { "pool:Claude": true },
  });
  assert.match(collapsed, /is-collapsed/);
  assert.doesNotMatch(collapsed, /Primary OAuth/);
});

test("renders interactive selectable rows, section chips, and brand accents", () => {
  const html = hs.render([{
    id: "meter",
    name: "Meter",
    state: "ok",
    status: {
      items: [{ label: "Codex", value: "2/2", icon: "codex", accent: "codex" }],
      sections: [{
        title: "Pools",
        chips: [
          { label: "跟随模型", selected: false, action: { name: "set_focus", args: { focus: "auto" } } },
          { label: "全部", selected: false, action: { name: "set_focus", args: { focus: "all" } } },
        ],
        rows: [{
          label: "Codex",
          icon: "codex",
          accent: "codex",
          selected: true,
          action: { name: "set_focus", args: { focus: "codex" }, label: "切换顶栏显示" },
          meters: [{ label: "7D", value: "剩 26%", percent: 26, tone: "hot" }],
        }],
      }],
    },
  }], { openId: "meter", icons: { codex: '<svg data-ico="codex"></svg>' } });

  assert.match(html, /hs-brand-codex/);
  assert.match(html, /is-selected/);
  assert.match(html, /is-action/);
  assert.match(html, /data-hs-action="set_focus"/);
  assert.match(html, /aria-pressed="true"/);
  assert.match(html, /hs-hot/);
  assert.match(html, /hs-chip/);
  assert.match(html, /跟随模型/);
  assert.match(html, /data-hs-sec-box="meter:Pools"/);
  assert.match(html, /data-hs-key="meter:Pools:0"/);
  assert.equal(typeof hs.patch, "function");
});

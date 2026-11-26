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
  assert.match(html, /data-hs-fold/);
  assert.match(html, /刚刚/);
});

test("folded mobile mode uses shortest item summary and toggles label", () => {
  const html = hs.render([{
    id: "m",
    name: "M",
    state: "ok",
    status: {
      compact: "Pool 2/2 · 5H 62% · 7D 30%",
      items: [{ label: "5H", value: "62%", percent: 62 }],
    },
  }], { folded: true });
  assert.match(html, /is-folded/);
  assert.match(html, /hs-mobile-t">5H 62%</);
  assert.match(html, />展开</);
});

test("starting and error states render clear fallbacks", () => {
  const starting = hs.render([{ id: "s", name: "S", state: "starting" }]);
  assert.match(starting, /启动中/);
  const failed = hs.render([{ id: "e", name: "E", state: "error", error: "timeout" }]);
  assert.match(failed, /timeout/);
});

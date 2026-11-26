import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
const drawThreadAt = app.indexOf("  function drawThread(");
const drawFallbackAt = app.indexOf("\n  function drawFallback(", drawThreadAt);
assert.ok(drawThreadAt >= 0 && drawFallbackAt > drawThreadAt);

function render(messages) {
  const ctx = {
    $$: () => [], nearBottom: () => false, stick: () => {},
    esc: (value) => value ?? "", prefs: { fold: () => true },
  };
  vm.runInNewContext(app.slice(drawThreadAt, drawFallbackAt), ctx);
  const el = {};
  ctx.drawThread(messages, el);
  return el.innerHTML;
}

const tool = (name, result = {}) => ({ kind: "tool", name, ...result });

test("a recorded empty result clears pending, with compatibility for old nodes", () => {
  const html = render([
    tool("Waiting"),
    tool("Empty", { tool_done: true }),
    tool("Old result", { output: "done" }),
  ]);
  assert.match(html, /class="tool pending" data-i="0"/);
  assert.match(html, /class="tool\s*" data-i="1"/);
  assert.match(html, /class="tool\s*" data-i="2"/);
  assert.match(html, /<pre>done<\/pre>/);
});

test("a folded run keeps the latest pending tool visible ahead of settled ones", () => {
  const html = render([
    tool("Failed", { tool_done: true, error: true }),
    tool("Still running"),
    tool("Empty", { tool_done: true }),
    tool("Completed", { tool_done: true, output: "done" }),
  ]);
  assert.match(html, /<\/div>\s*<\/details><details class="tool pending" data-i="1"/);
  assert.equal((html.match(/data-i="1"/g) || []).length, 1);
});

test("a folded run keeps a known failure visible when all tools have settled", () => {
  const html = render([
    tool("Failed", { tool_done: true, error: true }),
    tool("Empty", { tool_done: true }),
    tool("Complete", { tool_done: true }),
    tool("Old result", { output: "done" }),
  ]);
  assert.match(html, /<\/div>\s*<\/details><details class="tool err" data-i="0"/);
  assert.match(html, /1 个出错/);
});

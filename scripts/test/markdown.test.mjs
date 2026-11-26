import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const context = vm.createContext({ window: {} });
for (const file of ["markdown.js", "highlight.js"]) {
  vm.runInContext(readFileSync(new URL(`../../web/${file}`, import.meta.url), "utf8"), context);
}
const { createRenderer, esc } = context.window.collieMarkdown;
const md = createRenderer({
  filePattern: /(?!)/g, imagePattern: /\.(png|jpg)$/, thumbnailURL: () => "",
  ...context.window.collieHighlight.createHighlighter(esc),
});
const reply = (src) => md(src, { session: true });

test("separates headings and merges adjacent quote lines in session replies", () => {
  const src = "# One\n## Two\n### Three\n#### Four\n\n> **First**\n>\n> Second\n\n> Separate";
  assert.equal(reply(src), "<h1>One</h1><h2>Two</h2><h3>Three</h3><h4>Four</h4>"
    + "<blockquote><strong>First</strong><br><br>Second</blockquote><blockquote>Separate</blockquote>");
  assert.match(md(src), /<h2>One<\/h2>.*<blockquote><strong>First<\/strong><\/blockquote>/);
});

test("right-aligns numeric columns and honors explicit table alignment", () => {
  const src = "| Sample | Count | Rate | Mixed | Empty |\n| --- | --- | :---: | --- | --- |\n"
    + "| A | 12 | 2.5% | 8 | |\n| B | -1,204 | 10% | 1 item | |";
  const html = reply(src);
  assert.match(html, /<th>Sample<\/th><th class="md-number" style="text-align:right">Count<\/th>/);
  assert.match(html, /<th class="md-number" style="text-align:center">Rate<\/th><th>Mixed<\/th><th>Empty<\/th>/);
  assert.match(html, /<td class="md-number" style="text-align:right">-1,204<\/td>/);
  assert.match(reply("| Name | Code |\n| --- | :--- |\n| A | 123 |"), /style="text-align:left">123<\/td>/);
  assert.doesNotMatch(md(src), /md-number/);
});

test("adds a code toolbar and only closes fences on a matching standalone line", () => {
  const src = ["````text", "echo ```", "```", "```` trailing", "<tag>", "`````  ", "after"].join("\n");
  const html = reply(src);
  assert.match(html, /class="md-code-bar"><span>text<\/span><button[^>]+>复制<\/button>/);
  assert.match(html, /<pre><code>echo ```\n```\n```` trailing\n&lt;tag&gt;<\/code><\/pre><\/div><p>after<\/p>/);
  assert.match(reply("~~~go\r\npackage main\r\n~~~"), /<span>Go<\/span>.*data-lang="go"/);
  assert.match(reply("``` <unsafe>\n partial"), /<span>&lt;unsafe&gt;<\/span>.*<pre><code> partial<\/code><\/pre>/);
  assert.match(reply("```\n"), /<span>纯文本<\/span>/);
  assert.doesNotMatch(md("```go\npackage main\n```"), /md-code/);
});

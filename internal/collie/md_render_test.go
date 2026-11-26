package collie

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/TaoSama/collie/web"
)

func TestWebEmbeddedKaTeXOffline(t *testing.T) {
	cssBytes, err := fs.ReadFile(web.FS, "vendor/katex/katex.min.css")
	if err != nil {
		t.Fatalf("read katex.min.css: %v", err)
	}
	css := string(cssBytes)
	if strings.Contains(css, "http://") || strings.Contains(css, "https://") {
		t.Fatal("katex.min.css must not reference external URLs")
	}
	if strings.Contains(css, ".woff)") || strings.Contains(css, ".ttf)") {
		t.Fatal("katex.min.css should only reference embedded .woff2 fonts")
	}
	reFont := regexp.MustCompile(`url\((fonts/[^)]+\.woff2)\)`)
	matches := reFont.FindAllStringSubmatch(css, -1)
	if len(matches) < 10 {
		t.Fatalf("expected embedded woff2 font declarations in katex.min.css, got %d", len(matches))
	}
	for _, m := range matches {
		fontPath := "vendor/katex/" + m[1]
		if _, err := fs.Stat(web.FS, fontPath); err != nil {
			t.Fatalf("referenced KaTeX font missing from embed.FS: %s (%v)", fontPath, err)
		}
	}
	for _, req := range []string{"vendor/katex/katex.min.js", "vendor/katex/LICENSE"} {
		if _, err := fs.Stat(web.FS, req); err != nil {
			t.Fatalf("required KaTeX asset missing from embed.FS: %s (%v)", req, err)
		}
	}

	srv := &Server{token: "test-tok", home: t.TempDir(), web: web.FS}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vendor/katex/fonts/KaTeX_Main-Regular.woff2", nil)
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET woff2 status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "font/woff2") {
		t.Fatalf("GET woff2 Content-Type = %q, want font/woff2", ct)
	}
}

func TestMarkdownEscapingAndMathRender(t *testing.T) {
	runAppJS(t, `
const katex = require(path.join(root, "web/vendor/katex/katex.min.js"));
const loadRenderer = (withKatex) => loadApp("md, plainLine", withKatex ? { katex } : {});

const { md: mdRender, plainLine, warnings } = loadRenderer(true);
const stripTags = (h) => h.replace(/<[^>]+>/g, "");

// 1. Exact screenshot regression case
{
  const screenshotMd = [
    "### 1. 2,500 compute units 相当于多少 H100 算力？",
    "",
    "按 Google Colab 社区与官方实测的 H100 消耗速率计算：",
    "",
    "- **H100 (80GB HBM3) 消耗速率**：约 **18.05 compute units / 小时**（动态计费，会随集群负载小幅波动；按官方约 100 CU ≈ \\$9.99 折算，约合 \\$1.80/小时，2,500 CU 账面价值约 \\$250）。",
    "- **折合总时长**：",
    "",
    "$$\\frac{2500}{18.05} \\approx 138.5\\text{ 小时}$$",
    "大约相当于 **138 小时单卡 H100 算力**（连续跑接近 5.7 天）。",
    "",
    "- **典型硬件规格**：单卡 **80 GB VRAM**、约 **83–85 GB RAM**、约 **226–236 GB 本地 SSD**。",
    "",
    "> 如需确认你账号当前的精确实时费率，可开一台 H100 后立刻运行 \x60colab usage\x60 查看 \x60Usage rate\x60，确认后马上 \x60colab stop\x60 释放即可。",
  ].join("\n");

  const html = mdRender(screenshotMd);
  assert.ok(html.includes("$9.99"), "escaped $9.99 should render as $9.99");
  assert.ok(html.includes("$1.80/小时"), "escaped $1.80 should render as $1.80");
  assert.ok(html.includes("$250"), "escaped $250 should render as $250");
  assert.ok(!html.includes("\\$"), "no raw backslash before $ should leak");
  assert.ok(html.includes('class="md-math-block"'), "display math block should render");
  assert.ok(html.includes('class="katex-display"'), "KaTeX display markup should render");
  assert.ok(html.includes("小时"), "CJK text inside \\text{} should render");
  assert.ok(!html.includes("$$\\frac"), "raw LaTeX delimiters/source must not leak as text");
  assert.ok(html.includes("<code data-chip>colab usage</code>"), "inline code preserved");
  assert.ok(html.includes("<blockquote>"), "blockquote preserved");
  assert.equal(warnings.length, 0, "expected zero KaTeX console warnings for CJK \\text{}, got: " + warnings.join("; "));
}

// 2. CommonMark ASCII punctuation backslash unescaping
{
  const html = mdRender("\\*not bold\\* \\_under\\_ \\#hash \\+plus \\-minus 1\\.dot \\!bang \\\x60not code\\\x60 \\\\slash \\$9.99");
  assert.equal(html, "<p>*not bold* _under_ #hash +plus -minus 1.dot !bang \x60not code\x60 \\slash $9.99</p>");

  // Escaped bold does not produce <strong>
  assert.equal(mdRender("\\*\\*literal\\*\\*"), "<p>**literal**</p>");
  // Escaped link brackets do not produce markdown [label](url) link
  const escLink = mdRender("\\[not a link\\](https://example.com)");
  assert.ok(escLink.startsWith("<p>[not a link]("), "escaped brackets must stay literal: " + escLink);
  assert.ok(!escLink.includes(">not a link</a>"), "must not become markdown link: " + escLink);
  // Escaped parens and brackets around prose stay literal, not math
  assert.equal(mdRender("escaped \\[brackets\\] and \\(parens\\) and [label]\\(not url\\)"), "<p>escaped [brackets] and (parens) and [label](not url)</p>");
  assert.equal(mdRender("\\(中文备注\\) 与 \\[注意\\] 与 arr\\[0\\]"), "<p>(中文备注) 与 [注意] 与 arr[0]</p>");
  // Escaped block markers do not trigger block syntax
  assert.equal(mdRender("\\# not heading\n\\- not list\n1\\. not ol\n\\> not quote"), "<p># not heading<br>- not list<br>1. not ol<br>&gt; not quote</p>");
  // Escaped HTML punctuation remains HTML-escaped (XSS safe)
  assert.equal(mdRender("\\<script\\> \\\"q\\\" \\'s\\' \\&amp;"), "<p>&lt;script&gt; &quot;q&quot; &#39;s&#39; &amp;amp;</p>");
  // Non-punctuation backslashes remain intact (Windows paths, regex tokens)
  assert.equal(mdRender("C:\\Users\\alice\\new_file \\d+ \\n"), "<p>C:\\Users\\alice\\new_file \\d+ \\n</p>");
  // Escaped backslash before math allows math to open; triple backslash stays literal \$
  const bsMath = mdRender("\\\\$x^2$ and \\\\\\$250");
  assert.ok(bsMath.startsWith("<p>\\<span class=\"md-math\">"), "double backslash before $x^2$ should render \\ + math: " + bsMath);
  assert.ok(bsMath.endsWith("and \\$250</p>"), "triple backslash before $ should render \\$: " + bsMath);
  // Escaped $ inside bold and link labels
  assert.equal(mdRender("**共 \\$9.99** 与 [价格 \\$1.80](https://example.com)"), '<p><strong>共 $9.99</strong> 与 <a href="https://example.com" target="_blank" rel="noopener noreferrer">价格 $1.80</a></p>');
  // plainLine quote preview unescapes \$
  assert.equal(plainLine("约合 \\$9.99 / 月"), "约合 $9.99 / 月");
}

// 3. All LaTeX math delimiters: $...$, $$...$$, multiline $$, \[...\], \(...\)
{
  const inline1 = mdRender("质能方程 $E=mc^2$ 与 $\\alpha \\le \\beta$，以及 \\(x_i + y_i = 1\\) 和 \\(\\frac{1}{2}\\)。");
  assert.equal((inline1.match(/class="md-math"/g) || []).length, 4, "expected 4 inline math spans: " + inline1);
  assert.ok(!inline1.includes("$E=mc^2$") && !inline1.includes("\\(x_i"), "inline math source must not leak");

  // CJK tightly adjacent to $...$
  const cjkAdj = mdRender("当$x>0$时成立");
  assert.ok(cjkAdj.includes('class="md-math"'), "CJK-adjacent inline math should render: " + cjkAdj);

  // Escaped \$ inside math
  const dollarInMath = mdRender("公式 $\\text{费用} = \\$10$ 与 $$\\text{总价} = \\$250$$");
  assert.ok(dollarInMath.includes('class="md-math"'), "inline math with \\$ should render");
  assert.ok(dollarInMath.includes('class="md-math-block"'), "inline display math with \\$ should render");

  // Multiline $$ block (aligned / matrix / sum / integral / sqrt)
  const multiDollar = mdRender([
    "推导如下：",
    "$$",
    "\\begin{aligned}",
    "f(x) &= \\int_0^\\infty e^{-x^2}\\,dx = \\frac{\\sqrt{\\pi}}{2} \\\\",
    "\\sum_{k=1}^{n} k &= \\frac{n(n+1)}{2}",
    "\\end{aligned}",
    "$$",
    "结论成立。",
  ].join("\n"));
  assert.ok(multiDollar.includes('<div class="md-math-block"'), "multiline $$ should render block div");
  assert.ok(multiDollar.includes("<p>推导如下：</p>") && multiDollar.includes("<p>结论成立。</p>"), "surrounding paragraphs preserved");

  // Single-line and multiline \[...\]
  const bracketSingle = mdRender("\\[\\frac{a}{b} \\times c \\ge d\\]");
  assert.ok(bracketSingle.includes('<div class="md-math-block"'), "single-line \\[...\\] should render block div");

  const bracketMulti = mdRender("\\[\n\\begin{pmatrix} 1 & 2 \\\\ 3 & 4 \\end{pmatrix}\n\\]");
  assert.ok(bracketMulti.includes('<div class="md-math-block"'), "multiline \\[...\\] should render block div");

  // Inline \[...\] inside a sentence
  const bracketInline = mdRender("公式如下：\\[E = mc^2\\] 如上。");
  assert.ok(bracketInline.includes('<span class="md-math-block"'), "inline \\[...\\] should render span block");

  // Unclosed $$ must not swallow subsequent valid math or text
  const unclosed = mdRender("$$ unclosed line\n\n正常段落\n\n$$E = mc^2$$");
  assert.ok(unclosed.includes("$$ unclosed line"), "unclosed $$ stays literal");
  assert.ok(unclosed.includes("正常段落"), "paragraph after unclosed $$ preserved");
  assert.ok(unclosed.includes('<div class="md-math-block"'), "later valid $$ still renders");
}

// 4. Anti-false-positive: currency, shell vars, code blocks, inline code
{
  const negatives = [
    "单价 $9.99，折合 $1.80/小时，总计 $250。",
    "预算大约 $10 到 $20，或者 $10 ~ $20，区间 $10-$20，从$10到$20。",
    "费用 US$100 和 US$200，合计 $300。",
    "检查环境变量 $HOME 和 $PATH，路径 $HOME/$USER 与 $HOME:$PATH，以及 ${HOME}/${PATH}。",
    "价格 $10，运行 \x60echo $HOME\x60 与 \x60a$\x60，完成。",
  ];
  for (const s of negatives) {
    const html = mdRender(s);
    assert.ok(!html.includes("katex"), "must NOT trigger math on: " + s + "\nGOT: " + html);
  }

  // Mixed currency / shell vars AND genuine math on the SAME line
  const mixed = mdRender("环境变量 $HOME 与单价 $9.99，公式 $E=mc^2$ 和 $a + b = c$ 混排。");
  assert.equal((mixed.match(/class="md-math"/g) || []).length, 2, "expected exactly 2 math spans in mixed line: " + mixed);
  assert.ok(mixed.includes("$HOME") && mixed.includes("$9.99"), "env var and currency preserved verbatim: " + mixed);

  // Code fence protects $, $$, and \ escapes completely
  const fenced = mdRender([
    "\x60\x60\x60bash",
    "echo \"$HOME\" \"$PATH\" \"\\$9.99\"",
    "echo '$$\\frac{1}{2}$$'",
    "\x60\x60\x60",
  ].join("\n"));
  assert.ok(!fenced.includes("katex"), "fenced code must never trigger KaTeX");
  assert.ok(fenced.includes("\\$9.99") && fenced.includes("$$\\frac{1}{2}$$"), "fenced code preserves raw escapes and math delimiters: " + fenced);

  // Inline code protects $, $$, \ escapes, and preserves clickable paths/URLs
  const inlineCode = mdRender("查看 \x60$HOME\x60、\x60\\$250\x60、\x60$$x^2$$\x60、\x60\\\x60、\x60/tmp/demo.go\x60、\x60https://example.com\x60。");
  assert.ok(!inlineCode.includes("katex"), "inline code must never trigger KaTeX");
  assert.ok(inlineCode.includes("<code data-chip>$HOME</code>"), "$HOME in code preserved");
  assert.ok(inlineCode.includes("<code data-chip>\\$250</code>"), "\\$250 in code preserved");
  assert.ok(inlineCode.includes("<code data-chip>$$x^2$$</code>"), "$$x^2$$ in code preserved");
  assert.ok(inlineCode.includes("<code data-chip>\\</code>"), "single backslash in code preserved");
  assert.ok(inlineCode.includes('data-path="/tmp/demo.go"') && inlineCode.includes('class="code-link"'), "clickable code path preserved");
  assert.ok(inlineCode.includes('href="https://example.com"') && inlineCode.includes('class="code-link"'), "clickable code URL preserved");

  // Short inline code is kept whole in a table cell; long code may wrap
  const longCode = mdRender("\x60P:docs/assistant/{knowledge,day-recap,meeting-minutes}\x60");
  assert.ok(longCode.includes("<code>P:docs/assistant/"), "long inline code is not a chip: " + longCode);
}

// 5. Existing mdRender features & XSS safety
{
  // Soft line breaks + second-line bare URL
  const softUrl = mdRender("第一行说明\nhttps://example.com/docs");
  assert.ok(softUrl.includes("<br><a class=\"url\" href=\"https://example.com/docs\""), "soft line break + URL preserved: " + softUrl);

  // GFM table with escaped currency + inline math
  const tbl = mdRender("| 项目 | 价格 | 公式 |\n| --- | ---: | :---: |\n| H100 | \\$1.80 | $x^2 + 1$ |");
  assert.ok(tbl.includes('class="md-table"') && tbl.includes("$1.80") && !tbl.includes("\\$1.80") && tbl.includes('class="md-math"'), "table + escape + math works: " + tbl);

  // XSS safety inside math (trust: false disables \href; HTML tags escaped)
  const xss1 = mdRender("$$\\href{javascript:alert(1)}{click}$$");
  assert.ok(!/<a[\s>]/i.test(xss1) && !xss1.includes("href="), "KaTeX trust:false must not emit anchor/href: " + xss1);
  const xss2 = mdRender("$<img src=x onerror=alert(1)>$");
  assert.ok(!xss2.includes("<img"), "HTML tags inside math must not inject elements: " + xss2);
  const xss3 = mdRender("[click](https://example.com/a\\\"onclick=alert(1))");
  assert.ok(!xss3.includes('"onclick='), "escaped quote in URL must stay entity-encoded: " + xss3);
}

// 6. Graceful fallback when KaTeX global is absent
{
  const fb = loadRenderer(false);
  const html = fb.md("$$\\frac{1}{2}$$\n\n行内 $x^2$ 与 \\$9.99");
  assert.ok(html.includes("md-math-fallback"), "fallback class present without KaTeX");
  assert.ok(html.includes("$9.99") && !html.includes("\\$9.99"), "escaping still works without KaTeX");
}

console.log("ALL_MD_MATH_TESTS_PASSED");`, "ALL_MD_MATH_TESTS_PASSED")
}

// appJSHarness runs web/app.js in a stubbed page: loadApp(expose) returns the
// functions named in expose (a comma-separated list of its locals).
const appJSHarness = `
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const root = process.argv[1];
const appSrc = fs.readFileSync(path.join(root, "web/app.js"), "utf8");

function loadApp(expose, extra = {}) {
  const noop = () => {};
  const stubEl = () => ({
    hidden: false,
    className: "",
    dataset: {},
    style: { setProperty: noop, removeProperty: noop },
    classList: { add: noop, remove: noop, toggle: noop, contains: () => false },
    setAttribute: noop,
    getAttribute: () => "",
    addEventListener: noop,
    removeEventListener: noop,
    querySelector: () => stubEl(),
    querySelectorAll: () => [],
    append: noop,
    after: noop,
    closest: () => null,
    contains: () => false,
    getBoundingClientRect: () => ({ top: 0, bottom: 0, left: 0, right: 0, width: 0, height: 0 }),
    focus: noop,
    scrollIntoView: noop,
    options: [],
    selectedOptions: [],
    children: [],
  });
  const doc = {
    documentElement: stubEl(),
    body: stubEl(),
    hidden: false,
    createElement: () => stubEl(),
    getElementById: () => stubEl(),
    querySelector: () => stubEl(),
    querySelectorAll: () => [],
    addEventListener: noop,
    hasFocus: () => false,
  };
  const warnings = [];
  const ctx = {
    console: { log: noop, info: noop, warn: (...a) => warnings.push(a.join(" ")), error: (...a) => warnings.push(a.join(" ")) },
    document: doc,
    window: null,
    location: { hash: "", search: "", pathname: "/", origin: "http://127.0.0.1", hostname: "127.0.0.1" },
    history: { state: {}, replaceState: noop, back: noop },
    navigator: { userAgent: "NodeTest" },
    localStorage: { getItem: () => null, setItem: noop },
    matchMedia: () => ({ matches: false, addEventListener: noop }),
    MutationObserver: class { observe() {} },
    ResizeObserver: class { observe() {} },
    EventSource: class { addEventListener() {} close() {} },
    CSS: { escape: (s) => String(s) },
    URL,
    URLSearchParams,
    performance: { now: () => 0 },
    requestAnimationFrame: () => 0,
    setTimeout: () => 0,
    clearTimeout: noop,
    setInterval: () => 0,
    clearInterval: noop,
    addEventListener: noop,
    removeEventListener: noop,
    dispatchEvent: noop,
    Event,
    HTMLDialogElement: { prototype: {} },
    getComputedStyle: () => ({ getPropertyValue: () => "" }),
    fetch: () => Promise.reject(new Error("no network")),
    innerWidth: 1440,
    innerHeight: 900,
  };
  ctx.window = ctx;
  ctx.globalThis = ctx;
  Object.assign(ctx, extra);

  const marker = "  boot();\n})();";
  assert.ok(appSrc.includes(marker), "expected boot() tail in web/app.js");
  const instrumented = appSrc.replace(marker, "  globalThis.__collieTest = { " + expose + " };\n})();");
  // The scripts app.js uses go first, as on the page.
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(root, "web/threads.js"), "utf8"), ctx, { filename: "web/threads.js" });
  vm.runInContext(fs.readFileSync(path.join(root, "web/machines.js"), "utf8"), ctx, { filename: "web/machines.js" });
  vm.runInContext(fs.readFileSync(path.join(root, "web/ext.js"), "utf8"), ctx, { filename: "web/ext.js" });
  vm.runInContext(fs.readFileSync(path.join(root, "web/workbench.js"), "utf8"), ctx, { filename: "web/workbench.js" });
  vm.runInContext(fs.readFileSync(path.join(root, "web/markdown.js"), "utf8"), ctx, { filename: "web/markdown.js" });
  vm.runInContext(instrumented, ctx, { filename: "web/app.js" });
  return { ...ctx.__collieTest, ctx, warnings };
}

`

// runAppJS runs a node script after appJSHarness; it passes when it prints done.
func runAppJS(t *testing.T, script, done string) {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping web/app.js tests")
	}
	out, err := exec.Command(nodeBin, "-e", appJSHarness+script, filepath.Join("..", "..")).CombinedOutput()
	if err != nil {
		t.Fatalf("node test failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), done) {
		t.Fatalf("unexpected node test output:\n%s", out)
	}
}

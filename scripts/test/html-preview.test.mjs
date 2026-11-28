import assert from "node:assert/strict";
import { mkdir, readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { join } from "node:path";
import { test } from "node:test";

// Run against an automation browser after scripts/demo.sh:
// COLLIE_TEST_CDP=http://127.0.0.1:9222 node --test scripts/test/html-preview.test.mjs
// Set COLLIE_TEST_PLAYWRIGHT to the installed Playwright module path if needed.
const endpoint = process.env.COLLIE_TEST_CDP;
const screenshots = process.env.COLLIE_TEST_SCREENSHOTS;
const origin = "https://collie-test.example";
const htmlPath = "~/code/shop-api/bench-pagination.html";
const contentTypes = {
  html: "text/html; charset=utf-8", js: "application/javascript", css: "text/css",
  png: "image/png", svg: "image/svg+xml", webp: "image/webp", woff2: "font/woff2",
};
const fixture = `<!doctype html><meta charset="utf-8">
<p id="result"></p>
<button id="external" onclick="location.href='https://outside.test/navigation'">外部导航</button>
<button id="same-origin" onclick="location.href='${origin}/navigation'">同站导航</button>
<script>
const denied = read => {
  try { read(); return false; }
  catch (error) { return error.name === "SecurityError"; }
};
const result = {
  origin,
  cookie: denied(() => document.cookie),
  parent: denied(() => parent.document.cookie),
  storage: denied(() => localStorage.length),
  parentStorage: denied(() => parent.localStorage.length),
};
const fetchBlocked = fetch("https://outside.test/fetch").then(() => false, () => true);
const imageBlocked = new Promise(resolve => {
  const image = new Image();
  image.onload = () => resolve(false);
  image.onerror = () => resolve(true);
  image.src = "https://outside.test/image.png";
});
Promise.all([fetchBlocked, imageBlocked]).then(([fetchBlocked, imageBlocked]) => {
  document.querySelector("#result").textContent = JSON.stringify({...result, fetchBlocked, imageBlocked});
});
</script>`;

test("HTML reports use the Collie reader and isolate their scripts", { skip: !endpoint }, async () => {
  const require = createRequire(import.meta.url);
  const { chromium } = require(process.env.COLLIE_TEST_PLAYWRIGHT || "playwright");
  const browser = await chromium.connectOverCDP(endpoint, { noDefaults: true });
  try {
    for (const [name, mobile, viewport] of [
      ["desktop", false, { width: 1360, height: 870 }],
      ["mobile", true, { width: 390, height: 844 }],
    ]) {
      const context = await browser.newContext({ viewport, isMobile: mobile, hasTouch: mobile, reducedMotion: "reduce", serviceWorkers: "block" });
      try {
        let report = null;
        const network = [];
        await context.addCookies([{ url: origin, name: "collie_token", value: "test-cookie", httpOnly: true }]);
        await context.route("**/*", async route => {
          const { origin: requestedOrigin, pathname } = new URL(route.request().url());
          if (pathname === "/navigation" || requestedOrigin !== origin) {
            network.push(pathname);
            return route.abort();
          }
          if (pathname === "/demo/bench-pagination.html" && report) return route.fulfill({ contentType: contentTypes.html, body: report });
          const path = pathname === "/demo/" ? "demo/index.html" : pathname.slice(1);
          const contentType = contentTypes[path.split(".").pop()];
          if (!/^(demo|img)\/[-\w./]+$/.test(path) || path.includes("..") || !contentType) return route.fulfill({ status: 404 });
          try {
            const body = await readFile(new URL("../../docs/" + path, import.meta.url));
            return route.fulfill({ contentType, body });
          } catch (error) {
            if (error.code !== "ENOENT") throw error;
            return route.fulfill({ status: 404 });
          }
        });
        const page = await context.newPage();
        const home = origin + "/demo/?clean";
        await page.goto(home + "#/collie");
        const link = path => page.locator(`a.fpath[data-path="${path}"]`).first();
        await link("~/code/shop-web/docs/preview-dark.png").click();
        await page.locator("#lightbox[open] #lb-open:not([aria-disabled=true])").waitFor();
        await page.locator("#lb-close").click();
        await page.locator("#lightbox").waitFor({ state: "hidden" });
        await link("~/code/shop-api/docs/perf-audit-v1.4.md").click();
        await page.locator("#fv-body .fv-md").waitFor();
        await closeFile(page);
        await link(htmlPath).click();
        const embedded = reportFrame(page, "#fv-body");
        await reportControls(embedded);
        assert.equal(await page.locator("#fv-copy-wrap").isVisible(), false);
        assert.equal(await page.locator("#fv-dl").isVisible(), true);
        if (screenshots) {
          await mkdir(screenshots, { recursive: true });
          await page.screenshot({ path: join(screenshots, `${name}-html-preview.png`), animations: "disabled" });
        }
        const opened = page.waitForEvent("popup");
        await page.getByRole("link", { name: "在浏览器中打开" }).click();
        const popup = await opened;
        await reportControls(reportFrame(popup, "#rv-body"));
        assert.equal(await popup.evaluate(() => window.opener), null);
        await popup.close();
        await closeFile(page);
        assert.equal(await page.locator("#fv-body iframe").count(), 0);

        report = fixture;
        const hash = "#/file?" + new URLSearchParams({ path: htmlPath, machine: "" });
        for (const selector of ["#external", "#same-origin"]) {
          const probe = await context.newPage();
          await probe.goto(home + hash);
          await probe.evaluate(() => localStorage.setItem("test-marker", "test-storage"));
          const frame = reportFrame(probe, "#rv-body");
          await frame.locator("#result").filter({hasText: "imageBlocked"}).waitFor();
          assert.deepEqual(JSON.parse(await frame.locator("#result").textContent()), {
            origin: "null", cookie: true, parent: true, storage: true, parentStorage: true, fetchBlocked: true, imageBlocked: true,
          });
          const blocked = probe.waitForEvent("console", { predicate: message => message.text().includes("frame-src 'none'") });
          await frame.locator(selector).click();
          await blocked;
          await probe.close();
        }
        assert.deepEqual(network, []);
      } finally {
        await context.close();
      }
    }
  } finally {
    await browser.close();
  }
});

function reportFrame(page, body) {
  return page.frameLocator(body + " > .fv-html-sandbox").frameLocator('iframe[sandbox="allow-scripts"]');
}

async function reportControls(frame) {
  await frame.getByRole("button", { name: "P50 延迟" }).click();
  assert.equal(await frame.locator("#kpi-p95").textContent(), "2.2 ms");
  const summary = frame.locator("summary");
  await summary.click();
  assert.equal(await frame.locator("details").getAttribute("open"), "");
  await summary.click();
  assert.equal(await frame.locator("details").getAttribute("open"), null);
  await summary.click();
}

async function closeFile(page) {
  await page.locator("#file-view [data-fv-close]:visible").first().click();
  await page.locator("#file-view").waitFor({ state: "hidden" });
}

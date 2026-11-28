import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
function section(start, end) {
  const from = app.indexOf(start), to = app.indexOf(end, from);
  assert.ok(from >= 0 && to > from, start);
  return app.slice(from, to);
}
const routes = section("  let hubsLoaded;", "  // Ids in a remote hub");
const files = section("  const IMG_EXT =", "  // Images a message carries");
const path = "~/report.html";
const html = "<!doctype html><script>document.title = 'report'</script>";
const hub = { key: "remote", url: "https://remote.example", token: "test-token" };

function viewer(options = {}) {
  const requests = [], nodes = new Map();
  const ctx = vm.createContext({
    view: "ext", extCur: "live", ext: [], hist: [], current: "pane", find: () => null,
    state: { machines: [{ id: "other", online: true }] },
    URLSearchParams, AbortController, URL, IMG_FIXED: /$^/,
    findReport: () => documentReport(), machineName: String, fmtSize: String,
    esc: String, showFilePane() {}, directOfMachine: () => false, navigator: {},
    async fetch(url, init) {
      requests.push({ url: new URL(url, "https://local.example"), init });
      return ctx.findReport(url);
    },
    $(selector) {
      if (!nodes.has(selector)) {
        nodes.set(selector, {
          classList: { remove() {} },
          addEventListener() {}, setAttribute() {}, removeAttribute() {},
          replaceChildren(frame) { this.frame = frame; frame.parentNode = this; },
        });
      }
      return nodes.get(selector);
    },
    document: {
      createElement(tag) {
        assert.equal(tag, "iframe");
        return {
          addEventListener(type, listener) {
            assert.equal(type, "load");
            this.load = listener;
          },
        };
      },
    },
    ...options,
  });
  vm.runInContext(routes + files + `
    Object.assign(globalThis, {
      fileContextMachine, fetchFile, fileLinkAttrs, wideFileHash, openFile, openWideFile, renderHTMLFile,
      setHubs(values, ready) { hubs = values; hubsLoaded = ready; },
    });
  `, ctx);
  return { ctx, nodes, requests };
}

function documentReport({ kind = "html", size = Buffer.byteLength(html), body = html } = {}) {
  return new Response(body, { headers: { "X-File-Kind": kind, "X-File-Size": size } });
}

test("HTML selects the known session machine or owning hub", async () => {
  for (const [extCur, entry, machine] of [
    ["live", { id: "live" }, ""],
    ["h:past", { id: "past" }, ""],
    ["live", { id: "live", hub: "remote" }, "!remote"],
    ["h:past", { id: "past", hub: "remote" }, "!remote"],
    ["live", { id: "live", hub: "remote", machine: "node!remote" }, "node!remote"],
  ]) {
    const { ctx, nodes, requests } = viewer({
      extCur, ext: [entry], hist: [entry],
      findReport: () => new Response(null, { status: 404 }),
    });
    ctx.setHubs([hub]);
    assert.equal(ctx.fileContextMachine(), machine);
    await ctx.openFile(path);
    assert.equal(requests.length, 1, "a missing HTML report must not fall back to another machine");
    const [{ url, init }] = requests;
    assert.equal(url.host, machine ? "remote.example" : "local.example");
    assert.equal(url.searchParams.get("machine") || "", machine === "node!remote" ? "node" : "");
    assert.equal(init.headers.Authorization, machine ? "Bearer test-token" : undefined);
    assert.match(nodes.get("#fv-body").innerHTML, /没找到/);
    assert.equal(nodes.get("#fv-wide").hidden, true);
  }
  const { ctx } = viewer();
  assert.equal(ctx.fileContextMachine(), undefined);
  ctx.view = "pane";
  ctx.find = () => ({ machine: "computer" });
  assert.equal(ctx.fileContextMachine(), "computer");
});

test("HTML preview links use the pane and preserve the local machine in the browser link", async () => {
  const { ctx, nodes } = viewer({ ext: [{ id: "live" }] });
  assert.match(ctx.fileLinkAttrs(path, path, "打开文件"), /href="#"/);
  assert.equal(ctx.wideFileHash(path, undefined), "#/file?path=%7E%2Freport.html");
  await ctx.openFile(path);
  assert.equal(nodes.get("#fv-wide").href, "#/file?path=%7E%2Freport.html&machine=");
  assert.equal(nodes.get("#fv-wide").title, "在浏览器中打开");
  assert.equal(nodes.get("#fv-copy-wrap").hidden, true);
  assert.equal(nodes.get("#fv-dl").hidden, false);
});

test("the HTML browser reader waits for the owning hub and looks up only that machine", async () => {
  const { ctx, nodes, requests } = viewer({ view: "file" });
  let ready;
  ctx.setHubs([], new Promise((resolve) => { ready = resolve; }));
  const loading = ctx.openWideFile(new URLSearchParams({ path, machine: "!remote" }).toString());
  assert.equal(requests.length, 0);
  ctx.setHubs([hub]);
  ready();
  await loading;
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url.host, "remote.example");
  assert.equal(requests[0].url.searchParams.get("machine"), "");
  assert.equal(requests[0].init.credentials, "omit");
  assert.equal(nodes.get("#rv-body").frame.src, "html-viewer.html");

  ctx.setHubs([]);
  await ctx.openWideFile(new URLSearchParams({ path, machine: "!remote" }).toString());
  assert.equal(requests.length, 1, "an unknown remote hub must not be treated as the local hub");

  ctx.findReport = () => new Response(null, { status: 404 });
  await ctx.openWideFile(new URLSearchParams({ path, machine: "" }).toString());
  assert.equal(requests.length, 2, "an explicitly local browser link does not try other machines");
  assert.equal(requests[1].url.host, "local.example");
  assert.match(nodes.get("#rv-body").innerHTML, /没找到/);
});

test("HTML fetch rejects the wrong kind, oversized files and incomplete bytes", async () => {
  const { ctx } = viewer();
  assert.equal(await (await ctx.fetchFile(path, "")).blob.text(), html);
  for (const [options, error] of [
    [{ kind: "text" }, /暂不支持/],
    [{ size: (5 << 20) + 1 }, /超过 5 MB/],
    [{ size: Buffer.byteLength(html) + 1 }, /读取不完整/],
  ]) {
    ctx.findReport = () => documentReport(options);
    await assert.rejects(ctx.fetchFile(path, ""), error);
  }
});

test("report bytes go only to the scripts-only sandbox of the trusted viewer", () => {
  const { ctx } = viewer();
  const body = ctx.$("#fv-body");
  ctx.renderHTMLFile(body, html);
  const frame = body.frame, report = {};
  assert.equal(frame.src, "html-viewer.html");
  frame.contentDocument = {
    querySelector(selector) {
      assert.equal(selector, 'iframe[sandbox="allow-scripts"]');
      return report;
    },
  };
  assert.equal(report.srcdoc, undefined);
  frame.load();
  assert.equal(report.srcdoc, html);

  ctx.renderHTMLFile(body, html);
  const detached = body.frame;
  detached.parentNode = null;
  detached.load();
  assert.equal(body.innerHTML, undefined);
});

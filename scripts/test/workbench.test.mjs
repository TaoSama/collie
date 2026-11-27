// web/workbench.js: how the workbench orders its groups and modules, and
// names new ones. node --test scripts/test/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {}, URL };
vm.runInNewContext(readFileSync(new URL("../../web/workbench.js", import.meta.url), "utf8"), ctx);
const { arrange, freeId, shift, LINKS } = ctx.window.collieWorkbench;
const plain = (v) => JSON.parse(JSON.stringify(v));

const cfg = () => ({
  groups: [{ id: "fav", title: "收藏" }, { id: "life", title: "生活" }],
  modules: [
    { id: "a", group: "fav" }, { id: "books", group: "life" }, { id: "b", group: "fav" }, { id: "lost", group: "gone" },
  ],
});

test("arrange keeps group order, module order, and puts orphans first", () => {
  const g = plain(arrange(cfg()));
  assert.deepEqual(g.map((x) => x.id), ["fav", "life"]);
  assert.deepEqual(g[0].modules.map((m) => m.id), ["a", "b", "lost"]);
  assert.deepEqual(g[1].modules.map((m) => m.id), ["books"]);
  assert.deepEqual(plain(arrange({ groups: [], modules: [{ id: "x" }] })), []);
});

test("freeId comes from the host or the title and never repeats", () => {
  assert.equal(freeId("https://books.example.com/x", new Set()), "books");
  assert.equal(freeId("https://books.example.com/", new Set(["books", "books-2"])), "books-3");
  assert.equal(freeId("Homelab", new Set(), "g"), "homelab");
  assert.equal(freeId("读书", new Set(), "g"), "g");
  assert.equal(freeId("读书", new Set(["g"]), "g"), "g-2");
});

test("shift moves a module within its group only", () => {
  const ids = (c) => c.modules.map((m) => m.id);
  assert.deepEqual(ids(shift(cfg(), "b", -1)), ["b", "books", "a", "lost"]);
  assert.deepEqual(ids(shift(cfg(), "a", -1)), ["a", "books", "b", "lost"]); // first stays
  assert.deepEqual(ids(shift(cfg(), "books", 1)), ["a", "books", "b", "lost"]); // alone in its group
});

test("shortcut targets match the hub's list", () => {
  const go = readFileSync(new URL("../../internal/collie/workbench.go", import.meta.url), "utf8");
  const hub = [...go.matchAll(/"((?:auto|devices):[a-z]+)": true/g)].map((m) => m[1]).sort();
  assert.deepEqual(Object.keys(LINKS).sort(), hub);
});

test("workbench home links delegate to goHome and keep modifier clicks native", async () => {
  const listeners = new Map();
  const makeEl = (id = "") => ({
    id,
    innerHTML: "",
    textContent: "",
    hidden: false,
    open: false,
    classList: { toggle() {} },
    addEventListener(type, fn) { listeners.set(`${id}:${type}`, fn); },
    querySelector() { return makeEl(); },
    querySelectorAll() { return []; },
  });
  const nodes = {
    "#v-wb": makeEl("v-wb"),
    "#wb-name": makeEl("wb-name"),
    "#wb-meta": makeEl("wb-meta"),
    "#wb-acts": makeEl("wb-acts"),
    "#wb-nav": makeEl("wb-nav"),
    "#wb-main": makeEl("wb-main"),
    "#wb-edit": makeEl("wb-edit"),
  };
  const mctx = {
    window: {},
    URL,
    document: { querySelector: (s) => nodes[s] || makeEl(s) },
    location: { hash: "#/wb/books", replace() {} },
  };
  vm.runInNewContext(readFileSync(new URL("../../web/workbench.js", import.meta.url), "utf8"), mctx);
  const calls = [];
  let wb;
  wb = mctx.window.collieWorkbench.mount({
    api: async () => ({
      groups: [{ id: "life", title: "生活" }],
      modules: [{ id: "books", title: "读书", group: "life", kind: "web", url: "https://example.com", embed: "tab", share: "off" }],
    }),
    toast() {},
    esc: (s) => String(s ?? ""),
    ask: async () => false,
    openLink() {},
    goHome(replace = false) { calls.push(replace); wb.open(""); },
  });

  await wb.open("books");
  assert.match(nodes["#wb-name"].innerHTML, /class="wb-crumb"[^>]*data-wb-home/);
  assert.match(nodes["#wb-nav"].innerHTML, /href="#\/wb"[^>]*data-wb-home/);

  const onClick = listeners.get("v-wb:click");
  let prevented = false;
  onClick({
    button: 0,
    defaultPrevented: false,
    target: { closest: (sel) => (sel === "[data-wb-home]" ? {} : null) },
    preventDefault() { prevented = true; },
  });
  assert.equal(prevented, true);
  assert.deepEqual(calls, [false]);

  // Modifier click (open in new tab) should not be hijacked.
  let modPrevented = false;
  onClick({
    button: 0,
    metaKey: true,
    defaultPrevented: false,
    target: { closest: (sel) => (sel === "[data-wb-home]" ? {} : null) },
    preventDefault() { modPrevented = true; },
  });
  assert.equal(modPrevented, false);
  assert.deepEqual(calls, [false]);

  // Unknown module replaces instead of pushing.
  await wb.open("missing");
  assert.deepEqual(calls, [false, true]);
});

test("app.js goWbHome pops when previous entry is #/wb and otherwise replaces", () => {
  const src = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
  const start = src.indexOf("const normHash =");
  const end = src.indexOf("function fallbackHash()");
  assert.ok(start > 0 && end > start, "expected routing helpers in web/app.js");
  // Extract only the history helpers we need.
  const normBlock = src.slice(start, src.indexOf("let booted = false;") + "let booted = false;".length);
  const helperBlock = src.slice(src.indexOf("function syncHistoryIndex()"), end);

  function createHarness(initialEntries, initialPos, bootedInit = true) {
    const entries = initialEntries.map((e) => ({ hash: e.hash, state: e.state ? { ...e.state } : null }));
    let pos = initialPos;
    let routeCalls = 0;
    const location = {
      get hash() { return entries[pos].hash; },
      set hash(v) {
        entries.splice(pos + 1);
        entries.push({ hash: v, state: null });
        pos = entries.length - 1;
        runRoute();
      },
    };
    const history = {
      get state() { return entries[pos].state; },
      replaceState(st, _t, url) {
        entries[pos].state = st ? { ...st } : null;
        if (typeof url === "string") entries[pos].hash = url;
      },
      pushState(st, _t, url) {
        entries.splice(pos + 1);
        entries.push({ hash: typeof url === "string" ? url : entries[pos].hash, state: st ? { ...st } : null });
        pos = entries.length - 1;
      },
      back() {
        assert.ok(pos > 0, "history.back() called at start of history");
        pos -= 1;
        runRoute();
      },
    };
    const sandbox = { location, history, Map };
    vm.runInNewContext(`${normBlock}\n${helperBlock}\nfunction route() { _onRoute(); }\nthis.api = { syncHistoryIndex, replaceHash, prevHistoryHash, goWbHome, setBooted: (v) => { booted = v; }, setTrail: (k, v) => navTrail.set(k, v), onRouteSync: () => { navHash = normHash(location.hash); navTrail.set(navCur, navHash); }, getNavCur: () => navCur };`, sandbox);
    function runRoute() {
      routeCalls += 1;
      sandbox.api.syncHistoryIndex();
      sandbox.api.onRouteSync();
    }
    sandbox._onRoute = runRoute;
    sandbox.api.setBooted(bootedInit);
    return { api: sandbox.api, location, history, entries, pos: () => pos, routeCalls: () => routeCalls, runRoute };
  }

  // 1) Normal mobile journey: #/ -> #/wb -> #/wb/books -> goWbHome -> system back -> #/
  const h1 = createHarness([{ hash: "#/", state: null }], 0, false);
  h1.runRoute();
  h1.api.setBooted(true);
  h1.location.hash = "#/wb";
  h1.location.hash = "#/wb/books";
  assert.equal(h1.entries.length, 3);
  assert.equal(h1.entries[2].state.colliePrev, "#/wb");

  h1.api.goWbHome();
  assert.equal(h1.location.hash, "#/wb");
  assert.equal(h1.pos(), 1);
  // System back goes to #/, never back to #/wb/books!
  h1.history.back();
  assert.equal(h1.location.hash, "#/");
  assert.equal(h1.pos(), 0);

  // 2) Repeated visits to different modules never accumulate loop entries.
  h1.location.hash = "#/wb";
  h1.location.hash = "#/wb/books";
  h1.api.goWbHome();
  assert.equal(h1.location.hash, "#/wb");
  assert.equal(h1.pos(), 1);
  h1.location.hash = "#/wb/ledger";
  h1.api.goWbHome();
  assert.equal(h1.location.hash, "#/wb");
  assert.equal(h1.pos(), 1);
  h1.history.back();
  assert.equal(h1.location.hash, "#/");

  // 3) Deep link on mobile: padded [#/ (0), #/wb/books (1, colliePrev: "#/")] -> goWbHome replaces!
  const h2 = createHarness([
    { hash: "#/", state: { collieIdx: 0 } },
    { hash: "#/wb/books", state: { collieIdx: 1, colliePrev: "#/" } },
  ], 1, true);
  h2.api.setTrail(0, "#/");
  h2.runRoute();
  h2.api.goWbHome();
  assert.equal(h2.location.hash, "#/wb");
  assert.equal(h2.entries.length, 2);
  assert.equal(h2.pos(), 1);
  h2.history.back();
  assert.equal(h2.location.hash, "#/");

  // 4) Reload inside #/wb/books (navTrail lacks index 1, falls back to history.state.colliePrev).
  const h3 = createHarness([
    { hash: "#/", state: { collieIdx: 0 } },
    { hash: "#/wb", state: { collieIdx: 1, colliePrev: "#/" } },
    { hash: "#/wb/books", state: { collieIdx: 2, colliePrev: "#/wb" } },
  ], 2, false);
  h3.runRoute();
  h3.api.setBooted(true);
  h3.api.goWbHome();
  assert.equal(h3.location.hash, "#/wb");
  assert.equal(h3.pos(), 1);
  h3.history.back();
  assert.equal(h3.location.hash, "#/");

  // 5) Jump directly from #/collie to #/wb/books -> goWbHome replaces, then back goes to #/collie.
  const h4 = createHarness([{ hash: "#/", state: null }], 0, false);
  h4.runRoute();
  h4.api.setBooted(true);
  h4.location.hash = "#/collie";
  h4.location.hash = "#/wb/books";
  h4.api.goWbHome();
  assert.equal(h4.location.hash, "#/wb");
  assert.equal(h4.entries.length, 3);
  h4.history.back();
  assert.equal(h4.location.hash, "#/collie");
});

test("mobile workbench home control has data-wb-home and >=44px touch target rules", () => {
  const html = readFileSync(new URL("../../web/index.html", import.meta.url), "utf8");
  const css = readFileSync(new URL("../../web/workbench.css", import.meta.url), "utf8");
  assert.match(html, /class="wb-home"[^>]*data-wb-home/);
  assert.match(css, /#v-wb\.wb-on-page \.wb-home\s*\{[^}]*min-width:\s*44px[^}]*min-height:\s*44px/);
});

test("workbench overview gives every card (including shortcuts) an edit button, single add-web button, and separate tag row", async () => {
  const makeEl = (id = "") => ({
    id, innerHTML: "", textContent: "", hidden: false, open: false, disabled: false,
    classList: { toggle() {} },
    addEventListener() {},
    querySelector() { return makeEl(); },
    querySelectorAll() { return []; },
  });
  const nodes = {
    "#v-wb": makeEl("v-wb"), "#wb-name": makeEl("wb-name"), "#wb-meta": makeEl("wb-meta"),
    "#wb-acts": makeEl("wb-acts"), "#wb-nav": makeEl("wb-nav"), "#wb-main": makeEl("wb-main"),
    "#wb-edit": makeEl("wb-edit"),
  };
  const mctx = {
    window: { collieIcons: { svg: (n) => `<svg data-ico="${n}"></svg>` } },
    URL,
    document: { querySelector: (s) => nodes[s] || makeEl(s) },
    location: { hash: "#/wb", replace() {} },
  };
  vm.runInNewContext(readFileSync(new URL("../../web/workbench.js", import.meta.url), "utf8"), mctx);
  const wb = mctx.window.collieWorkbench.mount({
    api: async () => ({
      groups: [{ id: "fav", title: "收藏" }, { id: "collie", title: "Collie" }],
      modules: [
        { id: "gw", title: "网关", group: "fav", kind: "web", url: "https://gw.example.com", embed: "tab", share: "keep", snap_err: "timeout" },
        { id: "inbox", title: "收件箱", icon: "📥", group: "collie", kind: "link", url: "auto:inbox", share: "off" },
      ],
    }),
    toast() {}, esc: (s) => String(s ?? ""), ask: async () => false, openLink() {},
  });
  await wb.open("");

  // Single primary add button in header; none in nav or footer.
  const addCount = (nodes["#wb-acts"].innerHTML.match(/data-wb-add/g) || []).length
    + (nodes["#wb-nav"].innerHTML.match(/data-wb-add/g) || []).length
    + (nodes["#wb-main"].innerHTML.match(/data-wb-add/g) || []).length;
  assert.equal(addCount, 1);
  assert.match(nodes["#wb-acts"].innerHTML, /class="btn primary small"[^>]*data-wb-add/);

  // Both web and link cards expose direct edit buttons on overview.
  assert.match(nodes["#wb-main"].innerHTML, /data-wb-edit="gw"/);
  assert.match(nodes["#wb-main"].innerHTML, /data-wb-edit="inbox"/);

  // Built-in shortcut uses Lucide icon instead of legacy emoji; share/error in separate tag row.
  assert.match(nodes["#wb-main"].innerHTML, /data-ico="inbox"/);
  assert.doesNotMatch(nodes["#wb-main"].innerHTML, /📥/);
  assert.match(nodes["#wb-main"].innerHTML, /wb-card-tags.*沉淀.*快照失败/);
});

test("wb-edit close and cancel buttons have type=button and error banner is outside sheet-body", () => {
  const html = readFileSync(new URL("../../web/index.html", import.meta.url), "utf8");
  const dlg = html.slice(html.indexOf('id="wb-edit"'), html.indexOf("</dialog>", html.indexOf('id="wb-edit"')));
  // Every button before the submit save button must be type="button" so Enter submits save.
  const buttons = [...dlg.matchAll(/<button\b[^>]*>/g)].map((m) => m[0]);
  const submitButtons = buttons.filter((b) => !/type="button"/.test(b));
  assert.equal(submitButtons.length, 1);
  assert.match(submitButtons[0], /value="save"/);
  // Error element sits after .sheet-body closes, directly above .sheet-foot.
  assert.ok(dlg.indexOf('id="wb-edit-error"') > dlg.lastIndexOf("</div>", dlg.indexOf('id="wb-edit-error"')));
  assert.ok(dlg.indexOf('id="wb-edit-error"') < dlg.indexOf('<footer class="sheet-foot"'));
});

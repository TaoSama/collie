import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
function section(start, end) {
  const first = app.indexOf(start);
  const last = app.indexOf(end, first);
  assert.ok(first >= 0 && last > first, start);
  return app.slice(first, last);
}
const source = [
  section("  function offerReload()", "  // ───────────────────────────── other hubs"),
  section("  function bindFiles(", "  // Images a Collie message"),
  section("  function bindComposer(", '  bindComposer($("#composer")'),
  section('  document.addEventListener("click", (ev) => {\n    const b = ev.target.closest("[data-act=ask-reply]");', "  // How long a turn took"),
].join("\n");

function element() {
  return {
    value: "",
    listeners: new Map(),
    setAttribute() {},
    addEventListener(type, callback) {
      this.listeners.set(type, [...(this.listeners.get(type) || []), callback]);
    },
    emit(type, event = {}) {
      for (const callback of this.listeners.get(type) || []) {
        callback({ preventDefault() {}, currentTarget: this, ...event });
      }
    },
  };
}
function defer() {
  let resolve, reject;
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail; });
  return { promise, resolve, reject };
}
const flush = () => new Promise((resolve) => setImmediate(resolve));
const file = (name) => ({ name, type: "text/plain", size: 2 });
const uploaded = (name) => ({ ok: true, json: async () => ({ path: "/uploads/" + name }) });

function composer({ send = (_, ready) => ready, upload = async (_, { body }) => uploaded(body.name) } = {}) {
  const form = element(), input = element(), box = element(), picker = element(), attach = element();
  const document = {
    ...element(),
    createElement: element,
    getElementById: () => null,
    body: { append() {}, classList: { add() {} } },
  };
  const nodes = new Map([[".files", box], ['input[type="file"]', picker], ["textarea", input], [".attach", attach]]);
  form.querySelector = (selector) => nodes.get(selector);
  form.append = () => {};
  form.requestSubmit = () => form.emit("submit");
  const hubs = [{ key: "other", url: "https://other.example", token: "test" }];
  let pane = "A", machine = "node-a", reloads = 0, menu;
  const photo = defer();
  const context = vm.createContext({
    form, input, document,
    send,
    $: (selector) => nodes.get(selector),
    $$: (selector) => selector === "textarea" ? [input] : selector === "form.composer" ? [form] : [],
    esc: (text) => String(text),
    fmtSize: String, RETRY_SVG: "", grow() {}, toast() {},
    navigator: { userAgent: "Desktop" },
    window: {},
    AbortController, URL, URLSearchParams,
    touched: false, fleetUp: false,
    location: { reload() { reloads++; } },
    touchMQ: { matches: false, addEventListener() {} },
    openAttach: (_, actions) => { menu = actions; },
    takePhoto: () => photo.promise,
    shrinkImage: async (blob) => blob,
    seedImage() {},
    hubOf: (id) => hubs.find((hub) => id.includes("!" + hub.key)) || null,
    untag: (value, hub) => value.replace("!" + hub.key, ""),
    routeReq: (path, _, pinnedHub) => {
      const hub = pinnedHub || hubs.find((candidate) => path.includes("!" + candidate.key));
      return { url: hub ? hub.url + path.replace("!" + hub.key, "") : path, hub: hub || null };
    },
    fetch: upload,
    machineOf: () => machine,
    scope: () => pane,
  });
  vm.runInContext(source + "\nbindComposer(form, input, send, machineOf, null, null, scope);", context);
  form._selectDraft?.();
  return {
    input, box, form,
    open(id, node) { pane = id; machine = node; form._selectDraft?.(); },
    add(f) { input.emit("paste", { clipboardData: { files: [f] } }); },
    camera() { attach.emit("click"); menu.camera(); return photo; },
    reply(text) {
      const button = { dataset: { text }, closest: () => ({ querySelector: () => form }) };
      document.emit("click", { target: { closest: () => button } });
    },
    offerReload() { context.offerReload(); return reloads; },
  };
}

test("failed text and attachments return only to the originating pane", async () => {
  const result = defer();
  const { input, box, form, open, add } = composer({ send: async (_, ready) => { await ready; await result.promise; } });
  input.value = "A draft";
  add(file("A.txt"));
  form.requestSubmit();
  open("B", "node-b");
  input.value = "B draft";
  add(file("B.txt"));
  result.reject(new Error("A failed"));
  await flush();
  assert.equal(input.value, "B draft");
  assert.match(box.innerHTML, /B\.txt/);
  assert.doesNotMatch(box.innerHTML, /A\.txt/);
  open("A", "node-a");
  assert.equal(input.value, "A draft");
  assert.match(box.innerHTML, /A\.txt/);
  assert.doesNotMatch(box.innerHTML, /B\.txt/);
  open("B", "node-b");
  assert.equal(input.value, "B draft");
});

test("an attachment retry goes to the originally selected computer", async () => {
  const paths = [];
  const { form, open, add } = composer({ upload: async (url, { body }) => {
    paths.push(url);
    if (paths.length === 1) throw new Error("temporary error");
    return uploaded(body.name);
  } });
  open("A", "node-a!other");
  add(file("attachment.txt"));
  await flush();
  form.requestSubmit();
  open("B", "node-b");
  await flush();
  assert.equal(paths.length, 2);
  assert.ok(paths.every((path) => new URL(path, "http://localhost").searchParams.get("machine") === "node-a"));
  assert.ok(paths.every((path) => path.startsWith("https://other.example/")));
});

test("a delayed camera result belongs to the composer that opened it", async () => {
  const paths = [];
  const { box, open, camera } = composer({ upload: async (url, { body }) => { paths.push(url); return uploaded(body.name); } });
  const photo = camera();
  open("B", "node-b");
  photo.resolve(file("photo.png"));
  await flush();
  assert.doesNotMatch(box.innerHTML, /photo\.png/);
  assert.ok(paths[0].endsWith("&machine=node-a"));
  open("A", "node-a");
  assert.match(box.innerHTML, /photo\.png/);
});

test("an attachment-only draft in another pane prevents an automatic refresh", async () => {
  const { add, open, offerReload } = composer();
  add(file("draft.txt"));
  open("B", "node-b");
  assert.equal(offerReload(), 0);
  await flush();
});

test("an end-of-turn reply sends only its label and keeps the current draft", async () => {
  const sent = [];
  const { input, box, add, reply } = composer({ send: async (_, ready) => { sent.push(await ready); } });
  input.value = "message in progress";
  add(file("draft.txt"));
  reply("继续");
  await flush();
  assert.deepEqual(sent, ["继续"]);
  assert.equal(input.value, "message in progress");
  assert.match(box.innerHTML, /draft\.txt/);
});

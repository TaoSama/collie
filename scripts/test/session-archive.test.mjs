import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
const start = app.indexOf("  const changingArchive =");
const end = app.indexOf("  async function closeTerm(", start);

function fixture(others, changeSession = false, apiResult = {}) {
  const requests = [], confirmations = [], marks = [], toasts = [];
  const target = { pane_id: "target", workspace_id: "work", session: "thread", status: "working", kind: "codex" };
  const state = { agents: [target, ...others], shells: [] };
  const runtime = vm.runInNewContext([
    "let state = initial;",
    app.slice(start, end),
    "({ archiveRow, getState: () => state })",
  ].join("\n"), {
    initial: state,
    view: "collie",
    ask(opts) {
      confirmations.push(opts);
      if (changeSession) target.session = "another-thread";
      return true;
    },
    api: async (url, opts) => { requests.push([url, opts.body.just_closed]); return apiResult; },
    paneURL: (id, action) => `/api/panes/${id}/${action}`,
    sessionPath: (row) => row.session,
    find: (id) => state.agents.find((a) => a.pane_id === id),
    setMark: async (key, change) => marks.push([key, { ...change }]),
    AGENT_LABEL: { codex: "Codex" },
    toast: (msg, kind, opts) => toasts.push({ msg, kind, opts }),
    loadExternal() {}, loadHistory() {},
  });
  return {
    ...runtime, requests, confirmations, marks, toasts,
    archive: () => runtime.archiveRow({ id: "target", key: "key", kind: "codex", src: "pane", ws: "work", session: "thread", status: "working" }, true),
  };
}

for (const others of [[], [{ pane_id: "sibling", workspace_id: "work", status: "working" }]]) {
  test(`archiving a Codex pane closes only that pane (${others.length} siblings)`, async () => {
    const f = fixture(others);
    await f.archive();
    assert.deepEqual(f.requests, [
      ["/api/panes/target/close", undefined],
      ["/api/sessions/thread/archive", true],
    ]);
    assert.deepEqual(Array.from(f.getState().agents), others);
    assert.match(f.confirmations[0].text, /正在运行/);
  });
}

test("an archive confirmation cannot close a pane that switched sessions", async () => {
  const f = fixture([], true);
  await f.archive();
  assert.deepEqual(f.requests, []);
  assert.deepEqual(f.marks, []);
});

test("archiving offers an undo toast action that unarchives on agent and hub", async () => {
  const f = fixture([]);
  await f.archive();
  assert.equal(f.toasts.length, 1);
  assert.equal(f.toasts[0].msg, "已归档");
  assert.equal(f.toasts[0].opts?.action?.label, "撤销");

  await f.toasts[0].opts.action.onClick();
  assert.deepEqual(f.requests.at(-1), ["/api/sessions/thread/unarchive", false]);
  assert.deepEqual(f.marks.at(-1), ["key", { collie_archived: false }]);
  assert.equal(f.toasts.at(-1).msg, "已取消归档");
  assert.equal(f.toasts.at(-1).opts, undefined);
});

test("archiving with a note still offers undo", async () => {
  const f = fixture([], false, { note: "仅在 Collie 隐藏" });
  await f.archive();
  assert.equal(f.toasts[0].kind, "attn");
  assert.equal(f.toasts[0].opts?.action?.label, "撤销");
});

test("sidebar view filter omits the archived view and resets stored archive view", () => {
  assert.doesNotMatch(app, /sideView === "archive"/);
  assert.doesNotMatch(app, /\["archive",\s*"已归档"\]/);
  assert.doesNotMatch(app, /侧栏「已归档」/);
});

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
const start = app.indexOf("  const changingArchive =");
const end = app.indexOf("  async function closeTerm(", start);

function fixture(others, changeSession = false) {
  const requests = [], confirmations = [], marks = [];
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
    api: async (url, opts) => { requests.push([url, opts.body.just_closed]); return {}; },
    paneURL: (id, action) => `/api/panes/${id}/${action}`,
    sessionPath: (row) => row.session,
    find: (id) => state.agents.find((a) => a.pane_id === id),
    setMark: async (key) => marks.push(key),
    AGENT_LABEL: { codex: "Codex" },
    toast() {}, loadExternal() {}, loadHistory() {},
  });
  return {
    ...runtime, requests, confirmations, marks,
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

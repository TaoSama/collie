import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
const pinStart = app.indexOf("  const changingPin =");
const pinEnd = app.indexOf("  const changingArchive =", pinStart);
const archEnd = app.indexOf("  async function closeTerm(", pinEnd);

function fixture() {
  const apiCalls = [], markCalls = [], toasts = [];
  const state = { agents: [{ pane_id: "p1", session: "codex-1", kind: "codex", pinned: false }], shells: [] };
  const ext = [
    { id: "ag-1", session: "agy-desk", kind: "agy", surface: "", pinned: false },
    { id: "ag-cli", session: "agy-cli", kind: "agy", surface: "cli", pinned: false },
  ];
  const hist = [];
  let rendered = 0;
  const runtime = vm.runInNewContext([
    "let state = initState, ext = initExt, hist = initHist;",
    app.slice(pinStart, archEnd),
    "({ pinRow, archiveRow, getState: () => state, getExt: () => ext })",
  ].join("\n"), {
    initState: state, initExt: ext, initHist: hist,
    view: "collie",
    ask: () => true,
    api: async (url, opts) => { apiCalls.push([url, opts?.body]); return {}; },
    paneURL: (id, action) => `/api/panes/${id}/${action}`,
    sessionPath: (row) => row.session,
    find: (id) => state.agents.find((a) => a.pane_id === id),
    setMark: async (key, change) => { markCalls.push([key, change]); },
    AGENT_LABEL: { codex: "Codex", claude: "Claude", agy: "Antigravity" },
    toast: (msg, kind, act) => toasts.push({ msg, kind, act }),
    renderSessions: () => { rendered++; },
    loadExternal() {}, loadHistory() {},
  });
  return { ...runtime, apiCalls, markCalls, toasts, rendered: () => rendered };
}

const plain = (v) => JSON.parse(JSON.stringify(v));

test("pinRow routes Codex and agy desktop to native API, Claude and agy CLI to hub marks only", async () => {
  const f = fixture();
  await f.pinRow({ id: "p1", key: "|codex|codex-1", kind: "codex", src: "pane", surface: "cli", session: "codex-1" }, true);
  assert.deepEqual(plain(f.apiCalls), [["/api/sessions/codex-1/pin", { kind: "codex", surface: "cli", pinned: true }]]);
  assert.deepEqual(plain(f.markCalls), []);
  assert.equal(f.getState().agents[0].pinned, true);

  await f.pinRow({ id: "ag-1", key: "|agy|agy-desk", kind: "agy", src: "ext", surface: "", session: "agy-desk" }, true);
  assert.equal(f.apiCalls.length, 2);
  assert.deepEqual(plain(f.apiCalls[1]), ["/api/sessions/agy-desk/pin", { kind: "agy", surface: "", pinned: true }]);
  assert.deepEqual(plain(f.markCalls), [["|agy|agy-desk", { pinned: true }]]);

  await f.pinRow({ id: "ag-cli", key: "|agy|agy-cli", kind: "agy", src: "ext", surface: "cli", session: "agy-cli" }, true);
  await f.pinRow({ id: "c1", key: "|claude|c1", kind: "claude", src: "pane", surface: "cli", session: "c1" }, true);
  assert.equal(f.apiCalls.length, 2);
  assert.deepEqual(plain(f.markCalls.slice(1)), [
    ["|agy|agy-cli", { pinned: true }],
    ["|claude|c1", { pinned: true }],
  ]);
});

test("archiveRow reuses existing undo toast action that unarchives", async () => {
  const f = fixture();
  const row = { id: "c1", key: "|claude|c1", kind: "claude", src: "ext", surface: "cli", session: "c1", name: "测试会话", cwd: "/tmp" };
  await f.archiveRow(row, true);
  assert.equal(f.toasts.length, 1);
  assert.equal(f.toasts[0].act?.action?.label, "撤销");
  await f.toasts[0].act.action.onClick();
  assert.equal(f.apiCalls.at(-1)[0], "/api/sessions/c1/unarchive");
});

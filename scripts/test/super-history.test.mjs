import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
const start = app.indexOf("  let older = ");
const end = app.indexOf("  // Where scroller", start);
const normStart = app.indexOf("  function normSup(");
const normEnd = app.indexOf("  const isWorker", normStart);
assert.ok(start >= 0 && end > start && normStart >= 0 && normEnd > normStart);

test("main conversation can retry the same history page after an error", async () => {
  const initial = { entries: [{ id: "latest" }], earlier: true };
  const requests = [];
  const scroller = { scrollHeight: 1000, scrollTop: 12 };
  const { canLoadOlder, loadOlder, chatEntries, receive } = vm.runInNewContext(
    [
      app.slice(normStart, normEnd),
      app.slice(start, end),
      "let sup = initial; ({ canLoadOlder, loadOlder, chatEntries, receive: (snapshot) => { sup = normSup(snapshot); } })",
    ].join("\n"),
    {
      initial, pendingSays: [], $: () => ({ parentElement: scroller }),
      view: "pane", scrollByRoute: new Map(), pinnedLocal: new Map(),
      renderSuper() {}, markProgrammaticScroll() {}, toast() {},
      api(path) {
        requests.push(path);
        if (requests.length === 1) throw new Error("503 Service Unavailable");
        return { entries: [{ id: "earlier" }], more: false };
      },
    },
  );
  assert.equal(await loadOlder(), false);
  assert.equal(canLoadOlder(), true);
  assert.equal(scroller.scrollTop, 12);

  // The live window may slide forward before we retry.
  receive({ entries: [{ id: "new" }] });
  assert.equal(canLoadOlder(), true);
  assert.equal(await loadOlder(), true);
  assert.deepEqual(requests, ["/api/super/earlier?before=latest", "/api/super/earlier?before=latest"]);
  assert.deepEqual(Array.from(chatEntries(), (entry) => entry.id), ["earlier", "latest", "new"]);
  assert.equal(canLoadOlder(), false);
});

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const app = readFileSync(new URL("../../web/app.js", import.meta.url), "utf8");
function fragment(start, end) {
  const from = app.indexOf(start);
  const to = app.indexOf(end, from + start.length);
  assert.ok(from >= 0 && to >= 0, start);
  return app.slice(from, to);
}

test("parsed choices wait for a signature without offering a raw Enter", () => {
  const ctx = vm.createContext({
    readDialog: () => ({ q: "Proceed?", blocks: [] }),
    esc: (value) => String(value), showRaw: false, title: () => "agent",
  });
  for (const name of ["askHTML", "superAskHTML"]) {
    vm.runInContext(fragment(`  function ${name}(`, "\n  }\n") + "\n  }", ctx);
    const where = name === "askHTML" ? "agent" : { pane_id: "w1:p1" };
    assert.doesNotMatch(ctx[name]({ choices: [{ label: "Yes" }] }, where, ""), /data-k="enter"/);
    assert.match(ctx[name]({ choices: [] }, where, ""), /data-k="enter"/);
  }
});

for (const box of ["#live", "#super-thread"]) {
  test(`${box} submits the displayed version and refreshes immediately on conflict`, async () => {
    const pane = "w1:p1", calls = [], refreshed = [], notices = [];
    const card = { dataset: { pane, sig: "previous-sig" }, style: {} };
    const btn = { dataset: { choice: "0", label: "Yes" }, closest: () => card, hasAttribute: () => false };
    let click;
    const ctx = vm.createContext({
      $: () => ({ addEventListener: (event, fn) => { if (event === "click") click = fn; } }),
      api: async (path, opts) => {
        calls.push({ path, body: opts.body });
        throw Object.assign(new Error("对话框已变化"), { status: 409 });
      },
      current: pane, view: box === "#live" ? "pane" : "collie", curMode: () => "chat",
      superAsks: new Map(),
      sendKeys: (keys) => calls.push({ keys }),
      paneURL: (id, part) => `/api/panes/${encodeURIComponent(id)}/${part}`,
      sleep: async () => { assert.fail("a conflict should refresh without a delay"); },
      loadSuperAsk: (id, force) => refreshed.push({ pane: id, force }),
      refreshChatAsk: (id) => refreshed.push({ pane: id }),
      toast: (message) => notices.push(message),
      setTimeout,
    });
    vm.runInContext(fragment("  async function answerCard(", "\n  }\n") + "\n  }", ctx);
    const source = fragment(`  $("${box}").addEventListener("click",`, "\n  });\n");
    vm.runInContext(source + "\n  });", ctx);
    await click({ target: { closest: (selector) => selector === "button" ? btn : undefined } });
    assert.deepEqual(JSON.parse(JSON.stringify(calls)), [{
      path: "/api/super/decide", body: { pane, sig: "previous-sig", choice: "Yes" },
    }]);
    assert.deepEqual(refreshed, [box === "#live" ? { pane } : { pane, force: true }]);
    assert.deepEqual(notices, ["对话框已变化"]);
    assert.equal(card.dataset.busy, undefined);
  });
}

// web/ext.js: extension list ordering and WeChat nickname/avatar settings.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {} };
vm.runInNewContext(readFileSync(new URL("../../web/icons.js", import.meta.url), "utf8"), ctx);
vm.runInNewContext(readFileSync(new URL("../../web/machines.js", import.meta.url), "utf8"), ctx);
vm.runInNewContext(readFileSync(new URL("../../web/ext.js", import.meta.url), "utf8"), ctx);
const ext = ctx.window.collieExt;
ext.init({
  esc: (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])),
  when: (t) => String(t ?? ""),
  md: (s) => String(s ?? ""),
  workerHref: () => "",
});

test("hooksList places built-in extensions before marketplace and local extensions", () => {
  const html = ext.hooksList({
    extensions: [
      { manifest: { id: "aster-statusline", name: "Aster", market: { repo: "o/r" } }, setting: { enabled: false } },
      { manifest: { id: "workbench", name: "工作台", runtime: { feature: "workbench" } }, setting: { enabled: true } },
      { manifest: { id: "000-local", name: "本地", local: true }, setting: { enabled: false } },
      { manifest: { id: "wechat", name: "微信", runtime: { builtin: "wechat" } }, setting: { enabled: true } },
      { manifest: { id: "larkx", name: "飞书", runtime: { command: "bin/larkx" } }, setting: { enabled: true } },
      { manifest: { id: "notes", name: "笔记", market: { repo: "o/r" } }, setting: { enabled: false } },
    ],
    machines: [],
  }, { convs: [] }, { markets: [] });
  const ids = [...html.matchAll(/href="#\/auto\/hooks\/ext\/([^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(ids, ["larkx", "wechat", "workbench", "aster-statusline", "notes", "000-local"]);
});

test("extPage shows WeChat nickname and avatar controls beside it, and not on other extensions", () => {
  const wechatManifest = JSON.parse(readFileSync(new URL("../../extensions/wechat/collie-extension.json", import.meta.url), "utf8"));
  assert.equal(wechatManifest.configuration.properties.conversation_name.title, "昵称");

  const base = {
    manifest: { ...wechatManifest, icon_url: "/api/ext/wechat/icon?v=1" },
    setting: { id: "wechat", enabled: true, machine: "", config: { conversation_name: "微信ClawBot" } },
  };
  ext.learn({ extensions: [base] });

  const unsetHTML = ext.extPage(base, { machines: [] }, null, { convs: [] });
  assert.match(unsetHTML, /<label>昵称<input data-ext-config="conversation_name"/);
  assert.doesNotMatch(unsetHTML, /会话显示名/);
  assert.match(unsetHTML, /class="ext-avatar"><span>头像<\/span>/);
  assert.match(unsetHTML, /data-ext-avatar-pick>上传<\/button>/);
  assert.doesNotMatch(unsetHTML, /data-ext-avatar-clear/);

  const setHTML = ext.extPage({ ...base, avatar_url: "/api/ext/wechat/avatar?v=abc123" }, { machines: [] }, null, { convs: [] });
  assert.match(setHTML, /<img class="inbox-pic" src="\/api\/ext\/wechat\/avatar\?v=abc123"/);
  assert.match(setHTML, /data-ext-avatar-pick>更换<\/button>/);
  assert.match(setHTML, /data-ext-avatar-clear>恢复默认<\/button>/);

  const larkHTML = ext.extPage({
    manifest: { id: "larkx", name: "飞书", version: "1.0.0", runtime: { command: "bin/larkx" }, configuration: { type: "object", properties: { region: { type: "string", title: "站点" } } } },
    setting: { id: "larkx", enabled: true, machine: "" },
  }, { machines: [] }, null, { convs: [] });
  assert.doesNotMatch(larkHTML, /ext-avatar/);
});

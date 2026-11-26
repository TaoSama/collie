// web/tasks.js: what the task panel reads out of a task file.
// node --test scripts/test/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const ctx = { window: {} };
vm.runInNewContext(readFileSync(new URL("../../web/tasks.js", import.meta.url), "utf8"), ctx);
const parse = (name, text) => ({ ...ctx.window.collieTasks.parse(name, text) });

test("a bare slug heading takes its title from the goal, without the ids", () => {
  const t = parse("tasks/deliver-check.md", "# deliver-check\n目标：「消息停在输入框没提交」误报（#t4109 实际已送达）→ 以 agent 记录确认送达+重试（t4116）。worker deliver-check。\n- 下一步：根因回来告诉人，合 main 排队发版。");
  assert.equal(t.slug, "deliver-check");
  assert.equal(t.title, "「消息停在输入框没提交」误报");
  assert.equal(t.goal, "");
  assert.equal(t.next, "根因回来告诉人，合 main 排队发版。");
});

test("a heading drops the slug it repeats and the trigger id", () => {
  const t = parse("tasks/ext-design.md", "# ext-design · 扩展（larkx/微信/市场）开发（t2609）\n目标：把扩展接进来。");
  assert.equal(t.title, "扩展（larkx/微信/市场）开发");
  assert.equal(t.goal, "把扩展接进来。");
});

test("a heading of its own stays as it is", () => {
  assert.equal(parse("tasks/ext-design.md", "# 飞书扩展（larkx）改进\nworker ext-design").title, "飞书扩展（larkx）改进");
});

test("nothing to go on: the slug", () => {
  const t = parse("tasks/db-lookup.md", "# db-lookup\n- 查一下表");
  assert.equal(t.title, "db-lookup");
  assert.equal(t.next, "");
  assert.equal(t.phase, "");
});

test("the newest timestamped entry says whether it waits on you or a release", () => {
  const ship = "# a\n- 下一步：截图\n- 17:02 已问人要不要\n- 18:35 合 main a0944af，随 .19。";
  assert.equal(parse("tasks/a.md", ship).phase, "ship");
  assert.equal(parse("tasks/a.md", "# a\n- 18:13 合 main 6200150\n- 18:34 清单完成，已问人。").phase, "ask");
  assert.equal(parse("tasks/a.md", "# a\n- 18:20 worker 保留待人看图").phase, "ask");
  assert.equal(parse("tasks/a.md", "# a\n- 18:31 人：三类都要能翻；resume worker").phase, "");
  // A plan is not a fact: 下一步 alone says nothing about where it stands.
  assert.equal(parse("tasks/a.md", "# a\n- 下一步：合 main 后截图给人，排队发版。").phase, "");
});

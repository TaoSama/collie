import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const script = fileURLToPath(new URL("../../.claude/skills/cleanup/scripts/agy_sessions.py", import.meta.url));
const id = "11111111-1111-1111-1111-111111111111";
const other = "22222222-2222-2222-2222-222222222222";
const roots = ["antigravity", "antigravity-cli"];

function fixture(t) {
  const temp = mkdtempSync(join(tmpdir(), "collie-agy-cleanup-test."));
  t.after(() => rmSync(temp, { recursive: true, force: true }));
  const home = join(temp, "home");
  const scratch = join(temp, "scratch");
  mkdirSync(scratch);
  const path = (root, relative) => join(home, ".gemini", root, relative);
  const write = (root, relative, content) => {
    const file = path(root, relative);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, content);
  };
  const run = (...args) => {
    const result = spawnSync("python3", [script, ...args], {
      env: { ...process.env, HOME: home, TMPDIR: scratch },
      encoding: "utf8",
    });
    assert.ifError(result.error);
    return result;
  };
  return { path, write, run, scratch };
}

test("agy cleanup refuses missing, empty or path-like IDs before touching either data root", (t) => {
  const f = fixture(t);
  for (const root of roots) f.write(root, `brain/${id}/transcript.md`, root);
  const missing = f.run("clean");
  assert.equal(missing.status, 2);
  assert.match(missing.stderr, /--id/);
  for (const invalid of ["", "..", "*"]) {
    const result = f.run("clean", "--id", invalid);
    assert.equal(result.status, 2);
    assert.match(result.stderr, /有效的 Antigravity 会话 ID/);
  }
  for (const root of roots) assert.equal(readFileSync(f.path(root, `brain/${id}/transcript.md`), "utf8"), root);
  assert.deepEqual(readdirSync(f.scratch), []);
});

test("agy cleanup backs up every removed original and keeps relative paths in both roots", (t) => {
  const f = fixture(t);
  const deleted = [
    `conversations/${id}.db`,
    `conversations/${id}.db-wal`,
    `conversations/${id}.pb`,
    `brain/${id}/artifacts/transcript.md`,
    `annotations/${id}.pbtxt`,
    `presence/${id}.lock`,
  ];
  for (const root of roots) {
    for (const file of deleted) f.write(root, file, `${root}:${file}`);
    f.write(root, `brain/${other}/transcript.md`, "untouched");
    f.write(root, "cache/last_conversations.json", JSON.stringify({ target: id, unrelated: other }));
  }

  const backupOnly = f.run("backup", "--id", id);
  assert.equal(backupOnly.status, 0, backupOnly.stderr);
  const beforeF4 = backupOnly.stdout.trim();
  for (const root of roots) {
    for (const file of deleted) {
      assert.equal(readFileSync(join(beforeF4, root, file), "utf8"), `${root}:${file}`);
      assert.ok(existsSync(f.path(root, file)));
    }
  }

  const result = f.run("clean", "--id", id);
  assert.equal(result.status, 0, result.stderr);
  const backup = result.stdout.split("\n")[0];
  for (const root of roots) {
    for (const file of deleted) {
      assert.equal(readFileSync(join(backup, root, file), "utf8"), `${root}:${file}`);
      assert.ok(!existsSync(f.path(root, file)));
    }
    const last = "cache/last_conversations.json";
    assert.deepEqual(JSON.parse(readFileSync(join(backup, root, last), "utf8")), { target: id, unrelated: other });
    assert.deepEqual(JSON.parse(readFileSync(f.path(root, last), "utf8")), { unrelated: other });
    assert.equal(readFileSync(f.path(root, `brain/${other}/transcript.md`), "utf8"), "untouched");
    assert.ok(!existsSync(join(backup, root, "brain", other)));
  }
});

test("agy cleanup leaves both data roots intact if backing up the second root fails", (t) => {
  const f = fixture(t);
  const conversation = `conversations/${id}.pb`;
  for (const root of roots) f.write(root, conversation, root);
  f.write("antigravity-cli", `brain/${id}/transcript.md`, "untouched");
  const fifo = f.path("antigravity-cli", `brain/${id}/uncopyable-fifo`);
  const create = spawnSync("mkfifo", [fifo], { encoding: "utf8" });
  assert.ifError(create.error);
  assert.equal(create.status, 0, create.stderr);

  const result = f.run("clean", "--id", id);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /备份失败，已停止清理/);
  for (const root of roots) assert.equal(readFileSync(f.path(root, conversation), "utf8"), root);
  assert.equal(readFileSync(f.path("antigravity-cli", `brain/${id}/transcript.md`), "utf8"), "untouched");
  assert.ok(existsSync(fifo));
  assert.deepEqual(readdirSync(f.scratch), []);
});

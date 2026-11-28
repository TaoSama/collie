# Antigravity 会话清理

`cleanup` 技能的一种对象类型。下面的第 1 步对应通用流程的盘点和备份，第 2、3 步是执行，第 4 步是核对。

agy 没有删除会话的子命令。桌面应用和 CLI 各有一个数据目录，会话的痕迹散在几个文件里，漏删一处或没停掉持内存缓存的进程，Collie 或 agy 自己就还会把条目写回来：

| 数据目录 | 谁用 |
| --- | --- |
| `~/.gemini/antigravity` | 桌面应用（旧版 CLI 也用它） |
| `~/.gemini/antigravity-cli` | CLI，终端或 herdr pane 里跑的 agy |

**为什么摘要删了又回填**：
- 每个运行中的 `agy` CLI（用 `antigravity-cli`）或桌面 `language_server`（用 `antigravity`）在启动时都会把 `jetbox_summaries_proto.pb` 整份读进自己的进程内存。它更新**任意**会话摘要并刷盘（`persist`）时，会把这份内存快照整份覆盖写回 `jetbox_summaries_proto.pb`，不会先重读磁盘。只要还有在目标会话存在期间就已启动的进程活着，哪怕你刚删完磁盘文件，过几秒也会被它写回。
- 下次任意 `agy` / `language_server` 启动时，`store_client` 的启动对账（`trigger=startup`）会把 `jetbox_summaries_proto.pb` 里的全部记录同步回填进 `conversation_summaries.db`（即使 `conversations/<ID>.db` 已不存在）；若磁盘上还留着 `conversations/<ID>.db`（或 `.pb`），`reconcile_summaries` 也会从它重建摘要。

下文 `ID` 是会话 ID，`ROOT` 依次取这两个目录。命令用 bash 跑，在会话所在的那台电脑上执行（用 `python3` 内置 `sqlite3`，避免部分 Linux 主机缺少 `sqlite3` 命令行）。

## 1. 先看清要删什么

```bash
ID=<会话 ID>
python3 - "$ID" <<'PY'
import glob, json, os, sqlite3, sys

tid = sys.argv[1]
tb = tid.encode()

def read_varint(buf, i):
    val, shift = 0, 0
    while True:
        b = buf[i]; i += 1
        val |= (b & 0x7f) << shift
        if not (b & 0x80): return val, i
        shift += 7

def pb_has_id(path):
    if not os.path.isfile(path): return []
    buf = open(path, "rb").read()
    i, hits = 0, []
    while i < len(buf):
        tag, i = read_varint(buf, i)
        fn, wt = tag >> 3, tag & 7
        if wt == 2:
            ln, i = read_varint(buf, i)
            val = buf[i:i+ln]; i += ln
            if fn in (1, 2) and len(val) > 2 and val[0] == 0x0a:
                klen, ki = read_varint(val, 1)
                if val[ki:ki+klen] == tb:
                    hits.append(fn)
        elif wt == 0: _, i = read_varint(buf, i)
        elif wt == 1: i += 8
        elif wt == 5: i += 4
        else: break
    return hits

for name in ("antigravity", "antigravity-cli"):
    root = os.path.expanduser(f"~/.gemini/{name}")
    if not os.path.isdir(root): continue
    print(f"== {root}")
    for pat in (f"conversations/{tid}.*", f"brain/{tid}", f"annotations/{tid}.pbtxt", f"presence/{tid}.lock"):
        for p in sorted(glob.glob(os.path.join(root, pat))):
            print("  file:", p)
    db = os.path.join(root, "conversation_summaries.db")
    if os.path.isfile(db):
        c = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
        cols = {r[1] for r in c.execute("PRAGMA table_info(conversation_summaries)")}
        sel = "conversation_id, title, step_count, last_modified_time" if "step_count" in cols else "conversation_id, title, last_modified_time"
        rows = c.execute(f"SELECT {sel} FROM conversation_summaries WHERE conversation_id = ?", (tid,)).fetchall()
        if rows: print("  sqlite conversation_summaries:", rows)
        for tbl, q in (("conversations_fts", "SELECT count(*) FROM conversations_fts WHERE conversation_id = ?"),
                       ("conversation_parents", "SELECT count(*) FROM conversation_parents WHERE conversation_id = ? OR parent_id = ?")):
            if c.execute("SELECT 1 FROM sqlite_master WHERE name = ?", (tbl,)).fetchone():
                args = (tid,) if tbl == "conversations_fts" else (tid, tid)
                n = c.execute(q, args).fetchone()[0]
                if n: print(f"  sqlite {tbl}: {n}")
        c.close()
    pb_hits = pb_has_id(os.path.join(root, "jetbox_summaries_proto.pb"))
    if pb_hits: print("  jetbox_summaries_proto.pb fields:", pb_hits)
    meta = os.path.join(root, "cache/conversation_metadata.json")
    if os.path.isfile(meta):
        d = json.load(open(meta))
        if tid in d or (isinstance(d.get("conversations"), dict) and tid in d["conversations"]):
            print("  cache/conversation_metadata.json: present")
    last = os.path.join(root, "cache/last_conversations.json")
    if os.path.isfile(last):
        d = json.load(open(last))
        ks = [k for k, v in d.items() if v == tid]
        if ks: print("  cache/last_conversations.json:", ks)
PY
# 看当前活着的 agy / language_server 进程及启动时间，以及是否持 presence 锁
ps -eo pid,lstart,args | grep -E '([[:space:]]|/)agy([[:space:]]|$)|language_server' | grep -v grep || true
(lslocks 2>/dev/null || lsof ~/.gemini/antigravity*/presence/"$ID".lock 2>/dev/null) | grep "$ID" || true
```

- `step_count = 0` 且 `last_modified_time` 是 `0001-01-01` 的是**空壳**：新建失败留下的，`/resume` 不显示，F4 也删不了，走第 3 步。其余正常会话优先走第 2 步，已删过但摘要仍回填或原始 `.db` 已缺失的直接走第 3 步。
- 把上面的输出整理成「要删的清单」报告出来，再动手。多个会话一起删时，逐个列。

**备份**：动手前把两个数据目录里涉及的文件拷到临时目录，删错了能还原。

```bash
BAK=$(mktemp -d /tmp/agy-cleanup.XXXXXX)
for ROOT in ~/.gemini/antigravity ~/.gemini/antigravity-cli; do
  [ -d "$ROOT" ] || continue
  d="$BAK/$(basename "$ROOT")"; mkdir -p "$d"
  cp -p "$ROOT"/conversation_summaries.db* "$ROOT/jetbox_summaries_proto.pb" \
        "$ROOT/cache/conversation_metadata.json" "$ROOT/cache/last_conversations.json" \
        "$ROOT/annotations/$ID.pbtxt" "$d/" 2>/dev/null || true
  cp -p "$ROOT/conversations/$ID.db"* "$d/" 2>/dev/null || true
done
echo "$BAK"
```

## 2. 正常会话：F4 删除

1. 在 agy CLI 里打开 `/resume`，选中这个会话按 **F4**（Delete）。CLI 和桌面应用的数据库会同步删掉。
2. **注意写回风险**：F4 只会从**当前这一个** `agy` 进程的内存里移除摘要。如果同一 `$ROOT` 下还有其他在 `$ID` 存在期间就已启动的 `agy` 或桌面 `language_server` 进程在跑，它们会在下一次刷盘时把 `$ID` 重新写回 `jetbox_summaries_proto.pb`。
3. 再跑一遍第 1 步的盘点；若 `jetbox_summaries_proto.pb`、SQLite、`cache/`、`brain/`、`annotations/` 或 `presence/*.lock` 仍有残留，按第 3 步先停进程再清干净。

## 3. 空壳或摘要回填残留：先停进程再手动删

**第 1 步：停掉可能持缓存或持锁的进程**（不停直接改文件会被刷盘写回）：

- 先关闭打开着 `$ID` 的会话/窗格，确认 `presence/$ID.lock` 已无持锁进程。
- 要清 `~/.gemini/antigravity`（桌面目录）：退出 Antigravity 桌面应用，并确认没有残留的 `language_server` 进程在跑。
- 要清 `~/.gemini/antigravity-cli`（CLI 目录）：停掉**所有在 `$ID` 创建之后启动的 `agy` 进程**（清理旧会话或不确定各进程启动先后时，停掉该机上全部 `agy` 进程；严格早于 `$ID` 创建前就已启动的 `agy` 进程内存里没有 `$ID`，不会写回它）。

**第 2 步：按两个数据目录一次清干净**（哪个目录没有就跳过）：

```bash
python3 - "$ID" <<'PY'
import glob, json, os, shutil, sqlite3, sys, tempfile

tid = sys.argv[1]
tb = tid.encode()

def read_varint(buf, i):
    val, shift = 0, 0
    while True:
        b = buf[i]; i += 1
        val |= (b & 0x7f) << shift
        if not (b & 0x80): return val, i
        shift += 7

def strip_pb(path):
    if not os.path.isfile(path): return
    buf = open(path, "rb").read()
    out, i, dropped = bytearray(), 0, 0
    while i < len(buf):
        start = i
        tag, i = read_varint(buf, i)
        fn, wt = tag >> 3, tag & 7
        if wt == 2:
            ln, i = read_varint(buf, i)
            val = buf[i:i+ln]; i += ln
            drop = False
            if fn in (1, 2) and len(val) > 2 and val[0] == 0x0a:
                klen, ki = read_varint(val, 1)
                if val[ki:ki+klen] == tb:
                    drop = True
            if drop:
                dropped += 1
            else:
                out.extend(buf[start:i])
        elif wt == 0: _, i = read_varint(buf, i); out.extend(buf[start:i])
        elif wt == 1: i += 8; out.extend(buf[start:i])
        elif wt == 5: i += 4; out.extend(buf[start:i])
        else: out.extend(buf[start:]); break
    if dropped:
        mode = os.stat(path).st_mode & 0o777
        fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".jetbox.")
        try:
            with os.fdopen(fd, "wb") as f:
                f.write(out)
            os.chmod(tmp, mode)
            os.replace(tmp, path)
        except Exception:
            os.unlink(tmp); raise
        print(f"  stripped {dropped} entry(s) from {path}")

def atomic_json(path, fn):
    if not os.path.isfile(path): return
    d = json.load(open(path))
    if not fn(d): return
    mode = os.stat(path).st_mode & 0o777
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".cache.")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(d, f, indent=2, ensure_ascii=False)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except Exception:
        os.unlink(tmp); raise
    print(f"  updated {path}")

for name in ("antigravity", "antigravity-cli"):
    root = os.path.expanduser(f"~/.gemini/{name}")
    if not os.path.isdir(root): continue
    print(f"== cleaning {root}")
    for pat in (f"conversations/{tid}.*", f"annotations/{tid}.pbtxt", f"presence/{tid}.lock"):
        for p in glob.glob(os.path.join(root, pat)):
            os.remove(p); print("  removed", p)
    brain = os.path.join(root, "brain", tid)
    if os.path.isdir(brain):
        shutil.rmtree(brain); print("  removed", brain)
    db = os.path.join(root, "conversation_summaries.db")
    if os.path.isfile(db):
        c = sqlite3.connect(db, timeout=5)
        c.execute("DELETE FROM conversation_summaries WHERE conversation_id = ?", (tid,))
        for tbl, q in (("conversations_fts", "DELETE FROM conversations_fts WHERE conversation_id = ?"),
                       ("conversation_parents", "DELETE FROM conversation_parents WHERE conversation_id = ? OR parent_id = ?")):
            if c.execute("SELECT 1 FROM sqlite_master WHERE name = ?", (tbl,)).fetchone():
                args = (tid,) if tbl == "conversations_fts" else (tid, tid)
                c.execute(q, args)
        c.commit()
        c.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        c.close()
        print(f"  cleaned {db}")
    strip_pb(os.path.join(root, "jetbox_summaries_proto.pb"))
    atomic_json(os.path.join(root, "cache/conversation_metadata.json"), lambda d: bool(
        (d.pop(tid, None) is not None) |
        (isinstance(d.get("conversations"), dict) and d["conversations"].pop(tid, None) is not None)
    ))
    atomic_json(os.path.join(root, "cache/last_conversations.json"), lambda d: bool(
        [d.pop(k) for k in [k for k, v in d.items() if v == tid]]
    ))
PY
```

- `jetbox_summaries_proto.pb` 的清理只按精确 ID 去掉 field 1（`summaries`）和 field 2（`file_mod_times`）里匹配的条目，原样保留其余条目的原始字节，不要直接删整份文件。
- `conversation_metadata.json` 新版嵌套在 `.conversations[$id]` 下、旧版在顶层 `.[$id]`，上面脚本两处都会检查并只删目标 ID，其余内容保留。

## 4. 核对与收尾

- 删完立刻跑一遍第 1 步的盘点，两个目录都应该什么都查不到。
- **防回填复查**：启动一次新的 `agy`（或重开桌面应用），触发启动对账（`store_client` + `reconcile_summaries`）后再等几分钟（或等仍在运行的其它进程刷盘后），再跑一遍第 1 步的盘点。如果 `jetbox_summaries_proto.pb` 或 `conversation_summaries.db` 里又出现目标 ID，说明仍有旧的 `agy` 或 `language_server` 进程在内存里留着它——用 `ps` 找出并停掉后重跑第 3 步。
- `/resume` 列表、桌面应用列表和 Collie 页面（`/api/state`、`/api/external`、`/api/history`）都不再出现。
- 确认无误后删掉备份目录 `$BAK`。核对不通过就用备份还原，再查原因。

报告写：删了哪些会话（ID、标题、正常还是空壳），停过哪些进程、每个数据目录各删了哪些文件和行，以及复查结果。

# Antigravity 会话清理

`cleanup` 技能的一种对象类型。下面的第 1 步对应通用流程的盘点和备份，第 2、3 步是执行，第 4 步是核对。

agy 没有删除会话的子命令。桌面应用和 CLI 各有一个数据目录，会话的痕迹散在几个文件里，漏删一处 Collie 或 agy 自己就还会列出它：

| 数据目录 | 谁用 |
| --- | --- |
| `~/.gemini/antigravity` | 桌面应用（旧版 CLI 也用它） |
| `~/.gemini/antigravity-cli` | CLI，终端或 herdr pane 里跑的 agy |

下文 `ID` 是会话 ID，`ROOT` 依次取这两个目录。命令用 bash 跑，在会话所在的那台电脑上执行。

## 1. 先看清要删什么

```bash
ID=<会话 ID>
for ROOT in ~/.gemini/antigravity ~/.gemini/antigravity-cli; do
  echo "== $ROOT"
  ls "$ROOT/conversations/$ID.db" 2>/dev/null
  sqlite3 "$ROOT/conversation_summaries.db" "SELECT conversation_id, title, step_count, last_modified_time FROM conversation_summaries WHERE conversation_id = '$ID'"
  sqlite3 "$ROOT/conversation_summaries.db" "SELECT count(*) FROM conversations_fts WHERE conversation_id = '$ID'" 2>/dev/null
  jq --arg id "$ID" 'has($id)' "$ROOT/cache/conversation_metadata.json" 2>/dev/null
  grep -l "$ID" "$ROOT"/presence/*.lock 2>/dev/null
  grep -rl "$ID" "$ROOT/cache" 2>/dev/null
done
```

- 旧索引可能没有 `step_count` 列，报错就去掉这一列再查。
- `step_count = 0` 且 `last_modified_time` 是 `0001-01-01` 的是**空壳**：新建失败留下的，`/resume` 不显示，F4 也删不了，走第 3 步。其余走第 2 步。
- 把上面的输出整理成「要删的清单」报告出来，再动手。多个会话一起删时，逐个列。

**备份**：动手前把两个数据目录里涉及的文件拷到临时目录，删错了能还原。

```bash
BAK=$(mktemp -d /tmp/agy-cleanup.XXXXXX)
for ROOT in ~/.gemini/antigravity ~/.gemini/antigravity-cli; do
  d="$BAK/$(basename "$ROOT")"; mkdir -p "$d"
  cp -p "$ROOT"/conversation_summaries.db* "$ROOT/cache/conversation_metadata.json" "$d/" 2>/dev/null
  cp -p "$ROOT/conversations/$ID.db" "$d/" 2>/dev/null
done
echo "$BAK"
```

## 2. 正常会话：F4 删除

1. 在 agy CLI 里打开 `/resume`，选中这个会话按 **F4**（Delete）。CLI 和桌面应用的数据库会同步删掉。
2. 再跑一遍第 1 步的检查，手动清掉剩下的 `presence/*.lock` 和 `cache/` 里的残留文件。`.lock` 文件确认没有正在运行的 agy 用着它再删。

## 3. 空壳：手动删

先确认这个会话没有在任何窗口或 pane 里打开。对两个数据目录都做一遍，哪个目录里没有就跳过：

```bash
for ROOT in ~/.gemini/antigravity ~/.gemini/antigravity-cli; do
  rm -f "$ROOT/conversations/$ID.db"
  sqlite3 "$ROOT/conversation_summaries.db" "DELETE FROM conversation_summaries WHERE conversation_id = '$ID'"
  # CLI 的索引还有一行 chunk_index = -1 的会话头；桌面的索引没有这行时报错可忽略
  sqlite3 "$ROOT/conversation_summaries.db" "DELETE FROM conversations_fts WHERE conversation_id = '$ID' AND chunk_index = -1"
  f="$ROOT/cache/conversation_metadata.json"
  [ -f "$f" ] && jq --arg id "$ID" 'del(.[$id])' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
done
```

- `conversation_metadata.json` 是以会话 ID 为 key 的对象，只删这一个 key，其他内容原样保留。
- 桌面应用运行时可能把缓存写回去。删完重开应用，再核对一次。

## 4. 核对与收尾

- 再跑一遍第 1 步的检查，两个目录都应该什么都查不到。
- 如果新启动 agy 后摘要重新出现，按精确 ID 检查同一数据目录的 `jetbox_summaries_proto.pb`。它可能仍含旧摘要并回填 SQLite。优先用原生删除入口。需要清理旧缓存时，先备份文件并确认格式，只去掉目标 ID 对应的映射条目，保留其他条目的原始字节。完成后复查 SQLite 和缓存。
- `/resume` 列表和桌面应用的会话列表里没有它，Collie 页面上也不再出现。
- 确认无误后删掉备份目录 `$BAK`。核对不通过就用备份还原，再查原因。

报告写：删了哪些会话（ID、标题、正常还是空壳），每个数据目录各删了哪些文件和行，以及核对结果。

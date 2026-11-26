# 测试会话收尾

测试启动时，用 `verify-<任务ID>-<agent>` 命名窗格，把夹具放在本次独占的临时目录。每次创建后记下机器、Agent、原生会话 ID、窗格和 workspace ID、目录及浏览器设备 ID。等会话结束时再补录 Collie 历史 ID。正常结束和异常退出都执行收尾，结果中记录未清完的对象。

## 盘点

`collie list` 确认可用机器和 worker 状态。用已经认证的 hub API 并行读取三组 Collie 列表。下面的 `api` 函数沿用 `collie-web-verify` 技能。

```bash
for list in state external history; do api "/api/$list" > "$AUDIT_DIR/$list.json" & done
wait
jq -c '(.agents // []) + (.shells // []) + (.sessions // []) | .[] |
  {machine,kind,session,pane_id,workspace_id,id,name,title,cwd,surface,status,human_at}' \
  "$AUDIT_DIR"/{state,external,history}.json
```

这三组列表覆盖 Collie 各机器的可见条目，原生列表可能有额外记录。逐台读取 Codex 的 `state_*.sqlite`、Claude Code 会话文件及 Claude Desktop 的 `local_*.json` 索引、Antigravity 桌面和 CLI 的会话摘要及 FTS 头行。用创建时间判断轮次。若只能找到最近更新时间，用夹具启动日志或已登记的 ID 确认。目录名、标题和简短提示只用来筛选候选。用 worker 日志或夹具清单确认归属。

## 执行和复查

1. 核对机器、会话 ID、路径和当前窗格状态。关闭已结束且独占的测试 workspace。共享 workspace 只关测试窗格，保留运行中的 worker 和归属不明的会话。
2. 对有原始记录的会话优先在 Agent 侧归档。Codex 走当前 app-server 的 `thread/archive`，Antigravity 桌面用会话归档，CLI 走 Collie 的归档接口同步 FTS 头行和注解。Claude Desktop 用界面的归档操作，Claude Code CLI 没有原生归档。
3. 在 Collie 侧按会话标记归档，再核对 Agent 和 Collie。可通过 `POST /api/sessions/<会话ID>@<机器>/archive` 同步原生状态，填 `kind` 和可选的 `surface`。本地 hub 的会话省略机器后缀。还需单独写入 `/api/marks`，设置 `collie_archived: true`，键为 `机器|Agent 简称|会话 ID`。用聚合列表中的机器和 Agent 值生成键。接口成功时也要读回原生状态，Claude Code 的响应可能只说明 Collie 完成归档。
4. 遇到 Codex 的 `no rollout found`，先确认会话文件确实不存在，再用原生删除入口移除明确的测试索引。Antigravity 原始数据库缺失时，按 [Antigravity 残留](agy-sessions.md) 检查摘要、FTS、缓存、注解和锁。缓存文件结构随版本变化，读回当前结构后按 ID 修改。确认 CLI 记录和桌面索引不存在后，再移除对应的孤立 Collie 历史条目（`POST /api/history/<历史 ID>/delete`）。关闭窗格时 hub 可能异步生成历史，关完后再次按原生会话 ID 检查。
5. 删除清单中已确认无人使用的测试目录。重新扫描原生索引、Collie 历史、外部会话和窗格，按需展开线上侧栏历史复查。验收证据目录可单独保留。

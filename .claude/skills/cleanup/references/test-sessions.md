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

这三组列表覆盖 Collie 各机器的可见条目，原生列表可能有额外记录。逐台读取 Codex 的 `state_*.sqlite`（及 `thread_history_*.sqlite`、`thread-writer-locks/`）、Claude Code 会话文件及 Claude Desktop 的 `local_*.json` 索引、Antigravity 桌面和 CLI 的会话摘要、FTS 头行及 `jetbox_summaries_proto.pb`。用创建时间判断轮次。若只能找到最近更新时间，用夹具启动日志或已登记的 ID 确认。目录名、标题和简短提示只用来筛选候选。用 worker 日志或夹具清单确认归属。

## 执行和复查

1. 核对机器、会话 ID、路径和当前窗格状态。关闭已结束且独占的测试 workspace。共享 workspace 只关测试窗格，保留运行中的 worker 和归属不明的会话。
2. 对有原始记录的会话优先在 Agent 侧处理：
   - **Codex**：按下面「Codex 会话被 App / 进程占用」查锁并走**当前持锁的 app-server** 调 `thread/archive`（归档）或 `thread/delete`（彻底删除测试会话）。不要在锁还被占着时另起 `codex app-server` 或手删文件。
   - **Antigravity**：桌面用会话归档，CLI 走 Collie 的归档接口同步 FTS 头行和注解；若要彻底删除而非归档，按 [Antigravity 会话清理](agy-sessions.md) 先停掉持内存摘要缓存的进程再清文件、SQLite 和 `jetbox_summaries_proto.pb`。
   - **Claude**：Claude Desktop 用界面的归档操作，Claude Code CLI 没有原生归档。
3. 在 Collie 侧按会话标记归档，再核对 Agent 和 Collie。可通过 `POST /api/sessions/<会话ID>@<机器>/archive` 同步原生状态，填 `kind` 和可选的 `surface`。本地 hub 的会话省略机器后缀。还需单独写入 `/api/marks`，设置 `collie_archived: true`，键为 `机器|Agent 简称|会话 ID`。用聚合列表中的机器和 Agent 值生成键。接口成功时也要读回原生状态，Claude Code 的响应可能只说明 Collie 完成归档。
4. 遇到 Codex 的 `no rollout found`（rollout 已删但 `state_*.sqlite` 仍留行），确认无进程持锁后直接调 app-server 的 `thread/delete` 即可清掉孤立索引。Antigravity 原始数据库缺失或摘要删后回填时，按 [Antigravity 会话清理](agy-sessions.md) 先停相关 `agy` / `language_server` 进程，再清摘要、FTS、父子表、`jetbox_summaries_proto.pb`、缓存、注解和锁。确认原生记录和索引都不存在后，再移除对应的孤立 Collie 历史条目（`POST /api/history/<历史 ID>/delete`）。关闭窗格时 hub 可能异步生成历史，关完后再次按原生会话 ID 检查。
5. 删除清单中已确认无人使用的测试目录。隔几分钟（必要时重启对应 Agent 进程 / 重连 App）后重新扫描原生索引、摘要缓存、Collie 历史、外部会话和窗格，按需展开线上侧栏历史复查，确认没有回填。验收证据目录可单独保留。

## Codex 会话被 App / 进程占用

Codex 用 `~/.codex/thread-writer-locks/<会话ID>.lock` 的排他 `flock` 保护活跃会话。删不掉或删了又回来，常见原因有三个：
- **连错了 app-server**：devbox 上远程 Codex App（经 `codex app-server proxy`）和连后台的 CLI 共用共享后台 `~/.codex/app-server-control/app-server-control.sock`，锁由它持有；另起一个独立 `codex app-server`（stdio）去调 `thread/archive` / `thread/delete` 会报 `already has an active writer`。
- **私有 app-server 持锁**：桌面版 Codex / ChatGPT.app（11-27 起 `ipc.sock` 失效，App 跑自己的私有 `app-server`）或未连共享后台的独立 `codex` CLI 占着锁；此时连共享后台也会报 `already has an active writer` / `already has a live local writer`。
- **持锁时手删文件 / 改库**：直接删 `sessions/` 里的 rollout 或改 `state_*.sqlite`，持锁进程内存里的会话还在，会删不掉、留孤立记录或重新写回。

**步骤**：

1. **先查谁持锁**（在会话所在电脑上跑）：
   ```bash
   TID=<Codex 会话 ID>
   # 看哪个 PID 持写锁
   (lslocks 2>/dev/null || lsof ~/.codex/thread-writer-locks/"$TID".lock 2>/dev/null) | grep "$TID" || true
   # 看共享后台 socket 和进程
   codex app-server daemon version 2>/dev/null || true
   ps -ef | grep -E 'codex.*app-server' | grep -v grep || true
   ```
2. **分情况释放并归档 / 删除**：
   - **情况 A：持锁 PID 就是共享后台（`app-server-control.sock`，devbox 远程 Codex App 走这里），或当前无人持锁**：
     直接连 `~/.codex/app-server-control/app-server-control.sock`（Unix WebSocket JSON-RPC）调 `thread/archive`（归档：移到 `archived_sessions/` 并置 `state_*.sqlite` `archived=1`）或 `thread/delete`（测试会话彻底删除：卸载活跃会话、释放并删 `.lock`、删 rollout、清 `state_*.sqlite` 和 `thread_history_*.sqlite`，`no rollout found` 也能清）。即使远程 Codex App 正打开并订阅着该会话，经共享后台调用也能直接卸载并生效：
     ```bash
     # METHOD 取 thread/archive 或 thread/delete
     METHOD=thread/delete
     python3 - "$TID" "$METHOD" <<'PY'
     import base64, json, os, socket, struct, sys

     tid, method = sys.argv[1], sys.argv[2]
     sock = os.path.expanduser("~/.codex/app-server-control/app-server-control.sock")
     s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
     s.settimeout(15); s.connect(sock)
     key = base64.b64encode(os.urandom(16)).decode()
     s.sendall((
         "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
         f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n"
     ).encode())
     resp = b""
     while b"\r\n\r\n" not in resp: resp += s.recv(4096)
     assert b" 101 " in resp.split(b"\r\n", 1)[0], resp[:200]

     def send(obj):
         data = json.dumps(obj).encode()
         mask = os.urandom(4)
         hdr = bytearray([0x81])
         n = len(data)
         if n < 126: hdr.append(0x80 | n)
         elif n < 65536: hdr.append(0x80 | 126); hdr.extend(struct.pack("!H", n))
         else: hdr.append(0x80 | 127); hdr.extend(struct.pack("!Q", n))
         s.sendall(bytes(hdr) + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))

     def recv():
         def exact(n):
             buf = bytearray()
             while len(buf) < n:
                 c = s.recv(n - len(buf))
                 if not c: raise EOFError()
                 buf.extend(c)
             return bytes(buf)
         while True:
             h = exact(2)
             op, ln = h[0] & 0x0f, h[1] & 0x7f
             if ln == 126: ln = struct.unpack("!H", exact(2))[0]
             elif ln == 127: ln = struct.unpack("!Q", exact(8))[0]
             payload = exact(ln)
             if op == 1: return json.loads(payload.decode())
             if op == 9:
                 m = os.urandom(4)
                 s.sendall(bytes([0x8a, 0x80 | len(payload)]) + m + bytes(b ^ m[i % 4] for i, b in enumerate(payload)))

     def call(cid, m, p):
         send({"jsonrpc": "2.0", "id": cid, "method": m, "params": p})
         for _ in range(200):
             msg = recv()
             if msg.get("id") == cid: return msg
         raise TimeoutError(m)

     assert "error" not in call(1, "initialize", {"clientInfo": {"name": "cleanup", "version": "1"}, "capabilities": {"experimentalApi": True}})
     send({"jsonrpc": "2.0", "method": "initialized", "params": {}})
     res = call(2, method, {"threadId": tid})
     print(json.dumps(res, ensure_ascii=False))
     if "error" in res: sys.exit(1)
     PY
     ```
     仅归档时也可以直接调 Collie 的 `POST /api/sessions/<TID>@<机器>/archive`（Collie 内部已走 `app-server-control.sock`）。
   - **情况 B：持锁 PID 是桌面 App / ChatGPT.app 的私有 `app-server`，或独立 `codex` CLI 进程**：
     不要在持锁时手删文件或改 SQLite。先让持锁方释放：在 Codex App 内直接归档/删除该会话，或关闭占锁的独立 `codex` 窗格/进程（私有 `app-server` 仍不释放时退出桌面 App）。再次执行第 1 步确认 `thread-writer-locks/$TID.lock` 已无进程持锁后，再按情况 A 调用 `thread/archive` 或 `thread/delete`（该机没有共享后台时，用短生命周期 `codex app-server` stdio 调用同一 RPC）。
3. **回查无残留**：
   检查 `thread/loaded/list`（不含 `$TID`）、`state_*.sqlite` 的 `threads` 表（删除应无行；归档应 `archived = 1` 且路径在 `archived_sessions/`）、`thread_history_*.sqlite`（删除应无行）、`~/.codex/sessions/` 与 `~/.codex/archived_sessions/`、`~/.codex/thread-writer-locks/$TID.lock`，以及 Collie 的 `/api/state`、`/api/external`、`/api/history`。

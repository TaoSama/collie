# 外部会话（`internal/collie/external.go`）

## Claude 进程的 cwd 不是它启动的目录

Claude Code 进入 worktree（`EnterWorktree`）时会把自己的进程 `chdir` 进去，`~/.claude/sessions/<pid>.json` 里的 `cwd` 也跟着变。所以 `lsof` 读到的进程 cwd 是它现在待的地方，不是用户启动它的目录。

- 接管时要在启动目录下 `claude --resume`：取同一终端上父 shell 的 cwd（`startDir`），没有这样的 shell 时才用进程 cwd。worktree 由 Claude 在恢复时自己切回去。
- Codex 不会自己换目录，但可能用 `-C` 指定了别的目录，所以 Codex 仍用进程 cwd，不看父 shell。

## 接管要走它启动时的 alias

经 alias（`ccx`：自带网关、模型、`CLAUDE_CONFIG_DIR`）启动的外部会话，用裸 `claude --resume` 续会丢掉 alias 的配置，配置目录不同时还会找不到会话。所以接管时从进程反推 alias（`launchedBy`，`internal/collie/external_alias.go`）：

- 读进程启动时的命令行和环境变量（macOS 用 `kern.procargs2`，Linux 读 `/proc`），和这台电脑的 alias 比：alias 写的参数要是程序名后面的开头几个词，设的变量进程里都要有、字面值要相等。`$VAR` 和反引号只要求有，`~` 按 home 展开，`[1m]` 这类 glob 字符按原样比。命中最多的那个就是。
- 内容一样、名字不同的 alias（`ccx` 和 `ccsl5`）分不出来，看 shell 历史：启动时刻前后 10 秒里敲的那个，否则是敲得最多的那个。大多数 shell 退出时才写历史，所以多半靠次数。内容不同又同样命中时，接管对话框让人选。
- macOS 不让读 Apple 自带程序（如 `/bin/sleep`）的环境变量，`ps -E` 也一样；agent 的能读。测试要模拟进程时，用测试二进制本身当 agent（`TestMain` 里的 `COLLIE_STAND_IN`）。
- 会话文件按进程自己的 `CLAUDE_CONFIG_DIR` / `CODEX_HOME` 找（`External.envOr`），不是 Collie 的。

## 在 agent 里测试外部会话

在 herdr 的 agent 里起的 `claude` 会被当成 herdr 会话跳过，还会继承 `CLAUDE_CODE_SESSION_ID` 等环境变量（不登记、也不写自己的会话记录）。要模拟外部终端：脱离 herdr 的进程树（双 fork 加 `setsid`），用 `env -i` 起，只带上 `HOME`、`PATH`、`TERM` 和 `ANTHROPIC_*`，再给它一个 pty。

实测接管：`COLLIE_EXTERNAL=1 go test -run TestExternalLive -v` 列出这台电脑的外部会话和猜到的 alias，`COLLIE_TAKEOVER=<id> go test -run TestTakeOverLive -v` 把一个接管进本机 herdr（不用部署）。

测完后关掉测试用的 herdr workspace，并从 hub 的 `/api/history` 删掉它们留下的记录。不删的话，它们会留在侧栏里。

## Codex App 的线程列表先滤掉 worker，再截断

`codexAppRows`（`internal/collie/desktop.go`）读 App 的 `threads` 表。Collie 自己 worker 的线程也在表里，而且通常最新。要是 SQL 先 `LIMIT` 再在 Go 里滤掉 worker，worker 越多，App 自己的线程被挤掉越多（曾经 7 天内 29 个只显示 16 个）。所以 SQL 不加 `LIMIT`，Go 里滤完 worker 再数满 `codexMax`。ACTIVE 自动化写入的线程（`automations/<id>/automation.toml` 的 `target_thread_id`）不受上限限制。三个桌面应用的侧栏都会列出旧会话，所以 Collie 不按时间截断（曾经只看 7 天内，macair 22 个 Codex 线程只显示 6 个），每个应用只按各自的上限（Claude 和 Antigravity 是 `desktopMax`，Codex 是 `codexMax`）保留最新的，每台机器分别计数。

## Antigravity 索引里的 CLI 会话，记录在 CLI 的目录

Antigravity 桌面 App 的 `conversation_summaries.db` 也列出 agy CLI 起的会话，但这些会话的 `brain/<id>/.system_generated/logs/` 在 `~/.gemini/antigravity-cli` 下，不在 App 的 `~/.gemini/antigravity`。`agyDesktopRows`（`internal/collie/desktop.go`）两处都找，只找 App 目录时打开会显示「还没有找到这个会话的记录」。

## Antigravity 只有 CLI 起的会话能在 herdr 里继续

`agy --conversation <id>` 只认 CLI 自己的会话。桌面 App 起的会话，CLI 报 `not found in any store`（macpro 实测），CLI 也没有指定数据目录的参数或环境变量。所以 `desktopSessions`（`internal/collie/desktop.go`）只给 App 索引里 `source` 是 CLI 的行开放接管，App 自己的会话要在 App 里继续。这些行没有进程可以先结束，所以 App 说这个会话正在跑或在等后台任务时，也不给接管。herdr 外正在跑的 agy 进程、herdr 里跑过已结束的 agy 会话，各自走原来的接管和续接。接管和其他启动一样走 `startAgentAs`：只有带 `--dangerously-skip-permissions` 启动时，才预先信任目录（`trustForStart`）。否则新目录会停在 agy 的目录信任提示，在 Collie 里显示成等你确认（`agy -p` 不问信任，所以它用过的目录可能还没被信任）。

## 没有 Antigravity App 的电脑读 CLI 自己的索引

App 的 `~/.gemini/antigravity/conversation_summaries.db` 也列出 CLI 起的会话，所以有它的电脑只读它，不再读 CLI 的，免得同一个会话列两次。Linux 上没有 App，`agyDesktopRows`（`internal/collie/desktop.go`）改读 CLI 自己的 `~/.gemini/antigravity-cli/conversation_summaries.db`。这样在 herdr 外起的、已经结束的 CLI 会话也能列出来，并能在 herdr 里继续。两份索引的表结构相同，但 CLI 在自己的索引里不填 `source`，所以从它读出的行都按 CLI 的会话算。空会话（`step_count = 0`）照样不列。CLI 的索引里还可能留着会话已被删掉的行，这种行既打不开也续不上，所以找不到 `brain/<id>/…/transcript.jsonl` 的行也不列。

## Antigravity 没建成的会话留在索引里

新建失败的会话会在 `conversation_summaries` 里留一行：标题空、`step_count = 0`、`last_modified_time` 是零值 `0001-01-01`、`killed = 0`。App 自己加载不到它（日志报 `conversation … not found`），所以从 Collie 改名时 `UpdateConversation` 也会失败。`agyDesktopRows` 只列 `step_count > 0`（旧索引没有这一列就不看）且有修改时间的会话；`UpdateConversation` 的 `not_found` 也可能出现在有对话记录的会话上。Collie 可以从 CLI 或 App 目录直接读记录，App 能否打开会话由它自己的服务决定。Codex 的线程表里没见到这种空行（App 的线程都带第一条消息），没有加同样的过滤。

## 一轮做完折成「已工作 Xm Ys」

Codex 和 Antigravity 每几步就插一句过程消息，连续的工具调用很少超过 3 个，「N 个步骤」那种按连续工具合并的折叠收不起来，所以按轮折叠：解析时给一轮的最后一条消息标上 `turn_ms`，前端把它之前、上一条用户消息之后的过程收成一行（`drawThread`）。Codex 用日志里 `event_msg` 的 `task_complete.duration_ms`；Antigravity 没有轮结束的记录，把「有内容、不调用工具的 `PLANNER_RESPONSE`」当作结束，从 `USER_INPUT` 的 `created_at` 算起。被中断的轮没有 `task_complete`，不折叠。

## Codex 一周没动的会话文件会被压成 .zst

Codex 把修改时间满 7 天的 rollout 压成 `<名字>.jsonl.zst`，原文件删掉，`threads.rollout_path` 还记着 `.jsonl`。列表和标题从压缩正本流式提取最近人类消息时间、首条标题和上下文用量，同一进程按源文件路径、大小和修改时间复用摘要。

只有详情和分页需要随机访问。读者共享同一次解压，仍使用原始 JSONL 的字节游标。每个 Collie 数据目录的持久回放缓存上限为 256 MiB，按最近访问时间淘汰，已经打开的文件描述符仍可读。超出预算的单份回放通过请求期间的匿名临时文件读取，最后一个读者离开后释放。hub 和 node 启动时回收旧版本留下的解压副本。浏览器先显示骨架，400 ms 后才显示读取提示，解压进度按压缩正本已读字节计算。

## Codex 和 Antigravity 自己归档的会话要从每条列表里拿掉

两家各有归档，Collie 得照着读（`internal/collie/archived.go`）：Codex 是 `state_<n>.sqlite` 的 `threads.archived = 1`（会话文件挪进 `archived_sessions/`）；Antigravity 是 `conversation_summaries.db` 里搜索表 `conversations_fts` 中 `chunk_index = -1` 那行的 `archived`，App 的索引和 CLI 的索引各有一份，两处都要看。`annotations/<id>.pbtxt` 只存改过的标题之类，App 自己的归档列表也按搜索表查。

只滤 App 的线程表不够：人在 Codex 里归档的会话，常常还开在 herdr 窗格里（接管过来的），或已经结束、留在 hub 的历史里。所以节点在 `/api/titles` 里给窗格会话标上 `archived`，并回答 hub 带来的历史会话里哪些已归档；侧栏把已归档、已经停下的窗格和历史行藏起来，正在跑或等确认的照常显示。取消归档后几秒内回来。Antigravity 的 App 行在 Go 里滤掉归档再数满上限，SQL 不加 `LIMIT`。

Collie 菜单里的归档统一写到 hub 的 `session_marks`（`marks.go`），键包含电脑、Agent 和会话 ID，外部会话、herdr 窗格和历史共用。归档移到侧栏的「已归档」，在那里的会话菜单可取消归档。此操作会保留正在运行的会话。旧版 hub 的 `archived` 标记只对 Claude Code 生效，新操作写 `collie_archived`。在 Codex 或 Antigravity 自己归档的会话仍由相应应用管理。

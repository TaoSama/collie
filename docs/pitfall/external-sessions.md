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

## Antigravity 没建成的会话留在索引里

新建失败的会话会在 `conversation_summaries` 里留一行：标题空、`step_count = 0`、`last_modified_time` 是零值 `0001-01-01`、`killed = 0`。App 自己加载不到它（日志报 `conversation … not found`），所以从 Collie 归档、改名时 `UpdateConversation` 也会失败。`agyDesktopRows` 只列 `step_count > 0`（旧索引没有这一列就不看）且有修改时间的会话；`agyAnnotate` 遇到 `not_found` 时说明 App 加载不到这个会话。Codex 的线程表里没见到这种空行（App 的线程都带第一条消息），没有加同样的过滤。

## 一轮做完折成「已工作 Xm Ys」

Codex 和 Antigravity 每几步就插一句过程消息，连续的工具调用很少超过 3 个，「N 个步骤」那种按连续工具合并的折叠收不起来，所以按轮折叠：解析时给一轮的最后一条消息标上 `turn_ms`，前端把它之前、上一条用户消息之后的过程收成一行（`drawThread`）。Codex 用日志里 `event_msg` 的 `task_complete.duration_ms`；Antigravity 没有轮结束的记录，把「有内容、不调用工具的 `PLANNER_RESPONSE`」当作结束，从 `USER_INPUT` 的 `created_at` 算起。被中断的轮没有 `task_complete`，不折叠。

## Codex 一周没动的会话文件会被压成 .zst

Codex 把修改时间满 7 天的 rollout 压成 `<名字>.jsonl.zst`，原文件删掉，`threads.rollout_path` 却还记着 `.jsonl`。Codex 自己照常能读（文件是正本，paginated 线程打开时读 `thread_history_1.sqlite` 里的投影）。Collie 按字节偏移逐行读记录，只认 `.jsonl` 的话，这些会话在侧栏照常列出，点开却是「还没有找到这个会话的记录」。所以 `codexRollout`（`internal/collie/codex_zst.go`）在 `.jsonl` 不在时把 `.zst` 解压一次，放到 `COLLIE_HOME/cache/codex/<原绝对路径>`，修改时间和 `.zst` 一致，之后所有读取都当普通文件读；`.zst` 不在了（被归档或删除），对应的副本每小时清一次。App 行（`codexAppRows`）和按会话 ID 找记录（`findTranscript`）都走它。

## Codex 和 Antigravity 自己归档的会话要从每条列表里拿掉

两家各有归档，Collie 得照着读（`internal/collie/archived.go`）：Codex 是 `state_<n>.sqlite` 的 `threads.archived = 1`（会话文件挪进 `archived_sessions/`）；Antigravity 是 `conversation_summaries.db` 里搜索表 `conversations_fts` 中 `chunk_index = -1` 那行的 `archived`，App 的索引和 CLI 的索引各有一份，两处都要看。`annotations/<id>.pbtxt` 只存改过的标题之类，App 自己的归档列表也按搜索表查。

只滤 App 的线程表不够：人在 Codex 里归档的会话，常常还开在 herdr 窗格里（接管过来的），或已经结束、留在 hub 的历史里。所以节点在 `/api/titles` 里给窗格会话标上 `archived`，并回答 hub 带来的历史会话里哪些已归档；侧栏把已归档、已经停下的窗格和历史行藏起来，正在跑或等确认的照常显示。取消归档后几秒内回来。Antigravity 的 App 行在 Go 里滤掉归档再数满上限，SQL 不加 `LIMIT`。

Codex 和 Antigravity 的归档只认它们自己的标记，hub 的 `archived` 标记（`marks.go`）只藏 Claude Code 的会话。从 Collie 菜单归档这两家时，调用它们自己的归档，页面只在本地临时藏起，等列表跟上（成功后 60 秒）；归档失败就马上显示并报错。旧版本给这两家打过的 hub 标记不再起作用，否则在它们那边取消归档后，Collie 里还会一直藏着。

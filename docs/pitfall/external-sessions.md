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

`codexAppRows`（`internal/collie/desktop.go`）读 App 的 `threads` 表。Collie 自己 worker 的线程也在表里，而且通常最新。要是 SQL 先 `LIMIT` 再在 Go 里滤掉 worker，worker 越多，App 自己的线程被挤掉越多（曾经 7 天内 29 个只显示 16 个）。所以 SQL 不加 `LIMIT`，Go 里滤完 worker 再数满 `codexMax`。App 侧栏没有 7 天窗口，月度、每周的自动化两次运行之间会超过一周，所以 ACTIVE 自动化写入的线程（`automations/<id>/automation.toml` 的 `target_thread_id`）不受窗口和上限限制。

# 外部会话（`internal/collie/external.go`）

## Claude 进程的 cwd 不是它启动的目录

Claude Code 进入 worktree（`EnterWorktree`）时会把自己的进程 `chdir` 进去，`~/.claude/sessions/<pid>.json` 里的 `cwd` 也跟着变。所以 `lsof` 读到的进程 cwd 是它现在待的地方，不是用户启动它的目录。

- 接管时要在启动目录下 `claude --resume`：取同一终端上父 shell 的 cwd（`startDir`），没有这样的 shell 时才用进程 cwd。worktree 由 Claude 在恢复时自己切回去。
- Codex 不会自己换目录，但可能用 `-C` 指定了别的目录，所以 Codex 仍用进程 cwd，不看父 shell。

## 在 agent 里测试外部会话

在 herdr 的 agent 里起的 `claude` 会被当成 herdr 会话跳过，还会继承 `CLAUDE_CODE_SESSION_ID` 等环境变量（不登记、也不写自己的会话记录）。要模拟外部终端：脱离 herdr 的进程树（双 fork 加 `setsid`），用 `env -i` 起，只带上 `HOME`、`PATH`、`TERM` 和 `ANTHROPIC_*`，再给它一个 pty。

测完后关掉测试用的 herdr workspace，并从 hub 的 `/api/history` 删掉它们留下的记录。不删的话，它们会留在侧栏里。

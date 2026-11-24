# Worker（Collie 派出的 agent）的坑

## resume 要走启动时的 alias

alias 可以把 agent 指到另一个配置目录（例如 `CLAUDE_CONFIG_DIR`），会话记录存在那里。用裸 `claude --resume <id>` 续上一个经 alias 启动的 worker，找不到会话，报 `No conversation found with session ID`，agent 退回 shell。

- `Spawn` 把实际用的 alias 记在 `Worker.Alias`（`""` 表示没用；`nil` 是记录这个字段之前的 worker，只能走默认）。
- `spawn --resume` 没指定 `--alias` / `--no-alias` 时沿用它（`spawnBody.carryOver`，`internal/collie/superapi.go`）。

## 起不来的 worker 不留终端

启动失败（`startAgentAs` 报错）或 agent 退回 shell（tick 判定 `workerDead`）时，先读屏幕尾部写进事件和给 Router 的消息，再关掉它的工作区。不关的话，侧栏会留一个空 shell，没人会去用它。

代码：`Spawn`、`workerDead`、`paneTail`（`internal/collie/super.go`）。

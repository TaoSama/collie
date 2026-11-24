# Worker（Collie 派出的 agent）的坑

## resume 要走启动时的 alias

alias 可以把 agent 指到另一个配置目录（例如 `CLAUDE_CONFIG_DIR`），会话记录存在那里。用裸 `claude --resume <id>` 续上一个经 alias 启动的 worker，找不到会话，报 `No conversation found with session ID`，agent 退回 shell。

- `Spawn` 把实际用的 alias 记在 `Worker.Alias`（`""` 表示没用；`nil` 是记录这个字段之前的 worker，只能走默认）。
- `spawn --resume` 没指定 `--alias` / `--no-alias` 时沿用它（`spawnBody.carryOver`，`internal/collie/superapi.go`）。

## 选了 alias 就不传 --model

alias 往往自带网关，网关只认它自己的模型（`ccx` 配 Fable 会被网关 409 拒掉）。所以模型跟随 alias，只有思考强度可选：

- 启动时 `aliasArgs` 丢掉任何 `--model` / `-m` / `-c model=`（`internal/collie/aliases_parse.go`）；`Spawn` 和 `launchAgent` 记下的模型是 alias 的。
- 设置保存时有 alias 就清掉对应模型（`Settings.aliasesOwnModels`）；`collie spawn --alias X --model Y` 忽略 `--model`，返回 `model_ignored` 提示改用 `--no-alias`。
- 页面上派活栏、设置、新建对话框的模型选择器置灰，显示 alias 实际跑的模型（`followAlias`，`web/app.js`）。这个模型由 node 读 alias 时算好放在 `runs`：先看 alias 钉的 `--model` / `ANTHROPIC_MODEL`，没有就读它指向的配置（`CLAUDE_CONFIG_DIR` 的 settings.json、`CODEX_HOME` 的 config.toml 及 `-p` profile），`sonnet` 这类族名再按 `ANTHROPIC_DEFAULT_*_MODEL` 换成实际模型（`aliasRuns`，`internal/collie/aliases_model.go`）。都读不到时显示 agent 自己的默认模型。

## 起不来的 worker 不留终端

启动失败（`startAgentAs` 报错）或 agent 退回 shell（tick 判定 `workerDead`）时，先读屏幕尾部写进事件和给 Router 的消息，再关掉它的工作区。不关的话，侧栏会留一个空 shell，没人会去用它。

代码：`Spawn`、`workerDead`、`paneTail`（`internal/collie/super.go`）。

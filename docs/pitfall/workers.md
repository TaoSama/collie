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

## 目录在 worker 那台电脑上检查

Router 在 macpro 上跑 `collie spawn --machine devbox --cwd ~/x`，`~` 先被本机 shell 展开成 `/Users/<u>/x`。devbox 上没有这个目录，herdr 不报错，直接在家目录起 shell，worker 就在错的地方干活。

- CLI 把本机 `$HOME` 开头的 `--cwd` 换回 `~/…` 再发（`cliSpawn`，`internal/collie/cli.go`）。
- hub 派到别的电脑前，经 node 的 `/api/dirs` 确认目录在不在；node 上不存在、又在别人家目录下（`/Users/<u>`、`/home/<u>`、`/data00/home/<u>`、`/root`）的路径，换到本机家目录下同一位置；hub 记得的其他电脑家目录再试一次（`resolveDir`、`resolveDirOn`，`internal/collie/cwd.go`）。仍找不到就报错，不起 worker。spawn 的返回带 `cwd`，是实际用的目录。
- 存在的路径原样用：devbox 上真有 `/Users/luwentao/Workspaces`，它不会被改。
- 新建会话页面（`launchAgent`）、终端（`openShell`）和目录选择器走同一个 `resolveDir`。

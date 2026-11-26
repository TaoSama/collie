# Router（Collie 本体）的坑

## 看不到 Router ≠ Router 没了

hub 的 `agent.list` 跨机器合并：没连上（或这次没答）的电脑整台被跳过，不报错。hub 刚重启、node 自升级重连的几秒里，tick 看不到另一台电脑上的 Router，就会去 `ensureRouter`。

- 启动前（目录同步成功、那台电脑已连上之后）再看一次 agent 列表，Router 在就沿用，不再起新的；清理旧的 `collie · router` 工作区也放在这之后，否则看不见的旧工作区关不掉。
- herdr 仍拒绝名字 `collie`（`agent_name_taken`）时：占名的是 Router 目录里的活 agent，就保留它、关掉刚起的工作区；否则新 Router 取 `collie-2`…`collie-9`。
- 实际 herdr 名字记在 kv `router_on` 的 `name`；`liveAgents` 把它映射回逻辑名 `routerName`，发消息用 `routerAgent()` 取真实名字。别再直接 `Prompt(ctx, routerName, …)`。

代码：`internal/collie/router_name.go`、`ensureRouter`（`internal/collie/super.go`）。

## 系统错误先给能处理的一方

人只该看到要他做决定或动手的错误。新加往聊天里发 `"error"` 条目（会推送）之前，先想谁能处理：

- 会自己过去的（电脑重连、名字被占）：程序重试。Router 起不来时每次只记日志，连续失败 `routerFailsToTell` 次才对人说一次；没装 agent 这种重试没用的立刻说。
- 要判断的（worker 没起来、agent 报额度/登录错误、alias 用不了）：作为 spawn 的返回或 `ingest` 事件交给 Router，Router 重试或用一句话告诉人。
- spawn 的名字被 herdr 里别的 agent 占了：自动换 `名字-2`…，返回里的 `worker` 是实际名字；被正在跑的 worker 占了就报错并给出可用名字。

代码：`internal/collie/self_heal.go`。

## 做梦的清理要找对电脑

Collie 和做梦都跑在 `router_machine` 上，workspace ID 带 `@电脑` 后缀。清理只看 hub 本机的 herdr，就关不掉那台电脑上上一次做梦的会话：它一直占着名字 `collie-dream`，之后每次新起的做梦 agent 都改名失败，失败的 workspace 也会一个个留下（11-26）。

- 按标签关 workspace 时带上电脑：开始做梦前关旧 Router 所在和这次要跑的那台，结束时关这次做梦跑的那台（记在 `dreamRun.machine`）。
- 开始前再按名字清一遍：那台电脑上还叫 `collie-dream` 的 agent，不管 workspace 标签是什么都关掉。
- agent 起来但没能启动或改名时，关掉刚建的 workspace。

代码：`closeWorkspaces`、`closeOldDreams`、`StartDream`（`internal/collie/dream.go`）。

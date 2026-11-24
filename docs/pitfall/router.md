# Router（Collie 本体）的坑

## 看不到 Router ≠ Router 没了

hub 的 `agent.list` 跨机器合并：没连上（或这次没答）的电脑整台被跳过，不报错。hub 刚重启、node 自升级重连的几秒里，tick 看不到另一台电脑上的 Router，就会去 `ensureRouter`。

- 启动前（目录同步成功、那台电脑已连上之后）再看一次 agent 列表，Router 在就沿用，不再起新的；清理旧的 `collie · router` 工作区也放在这之后，否则看不见的旧工作区关不掉。
- herdr 仍拒绝名字 `collie`（`agent_name_taken`）时：占名的是 Router 目录里的活 agent，就保留它、关掉刚起的工作区；否则新 Router 取 `collie-2`…`collie-9`。
- 实际 herdr 名字记在 kv `router_on` 的 `name`；`liveAgents` 把它映射回逻辑名 `routerName`，发消息用 `routerAgent()` 取真实名字。别再直接 `Prompt(ctx, routerName, …)`。

代码：`internal/collie/router_name.go`、`ensureRouter`（`internal/collie/super.go`）。

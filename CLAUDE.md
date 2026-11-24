# collie

面向使用者的介绍、安装和用法写在 `README.md`，本文件只写给 agent 和开发者。

本仓库 fork 自 [lihuanyu0w0/collie](https://github.com/lihuanyu0w0/collie)，remote `upstream` 指向它（只拉不推）。同步：`git fetch upstream && git merge upstream/main`，解决冲突后照常验证。

## 约定

- **本机和账号相关的值**（主机名、本地路径、个人凭证的位置）写在 `CLAUDE.local.md`，不提交。
- **踩到坑随手记下来**。各类文档放在哪、怎么归档，见 `docs/README.md`。
- **本文件保持简短**：只放项目是什么、这些约定、验证命令、最常见的坑和目录结构。架构写在 `docs/architecture.md`，搭建、部署、发布步骤写在 `docs/runbook/`，在这里留链接。
- **一条命令完成全部验证**：第一个模块落地后，加一个统一入口（如 `scripts/check.sh`）跑构建、lint 和测试。Git 钩子和 CI 都调用它，下面的「构建与验证」只写这条命令。
- **项目级 `.claude/` 只放本项目特有的内容**：`.claude/rules/` 下的规则，以及 `.claude/skills/` 下针对本项目部署、发布或领域流程的技能。通用技能留在用户级。
- **界面改动遵循设计系统**：样式的值以 design token 的形式放在代码里，规则写在 `docs/design/DESIGN_SYSTEM.md`。改界面前先读它，第一次做界面时创建它。
- **引用承载细节的文档**：本文件里的规则、坑和目录表的行，结尾带上对应文档的路径，例如 ``| `server/` | Go 模块（`docs/server/server.md`） |``，让读的人知道下一步该打开什么。文档移动时同步更新这些路径。

## 构建与验证

需要 Go 1.25+。启动本地开发实例前运行 herdr 服务，或用 `--socket` 指定测试用的 herdr socket。开发实例使用独立的 Collie 数据目录和端口。

```bash
go build -o bin/collie .
COLLIE_HOME="$PWD/.collie-dev" ./bin/collie --addr 127.0.0.1:17789
```

- Go 检查运行 `go build ./...`、`go vet ./...`、`go test ./... -count=1`。
- 前端检查运行 `node --check web/app.js`。改了 `web/` 后运行 `sh scripts/demo.sh` 重建 `docs/demo/`，随源码一起提交。
- 在仓库内运行 `sh docs/install.sh hub` 会用源码构建并安装服务。安装 node 用 `sh docs/install.sh node --hub URL --code CODE`，已配对后可只传 `node`。Linux 后台服务日志用 `journalctl --user -u collie-hub -f`，node 服务名为 `collie-node`。macOS 日志在 `~/Library/Logs/collie-hub.log` 和 `~/Library/Logs/collie-node.log`。
- 发布前可运行 `sh scripts/release.sh v2026.1124.1 --dry`，交叉编译和校验和输出到 `dist/`。正式版本号格式为 `vYYYY.MMDD.N`。

## 架构与调试

浏览器通过 HTTP 和 SSE 连到 hub。hub 把前端打包进 Go 二进制，通过 Unix socket 的 NDJSON API 控制本机 herdr。node 通过出站 yamux 隧道转发远端 herdr 事件及 pane 请求。

- **会话**。对话流来自 Agent 会话记录，终端请求直接走 herdr。外部会话按进程、会话 ID 或工作目录定位记录，具体方式随 Agent 和桌面应用而异。决策卡片从终端状态识别问题和选项，再把选择转换成对应的终端输入。
- **Router 和 worker**。聊天、Webhook、定时器和 worker 状态作为 Trigger 入队。先按来源策略（`respond` / `candidate` / `ignore`）处理，再交给 Router 或订阅该线程的 worker。会话忙时排队，空闲后可批量投递。超过 700 字符的投递正文可用 `collie show ID` 取回。
- **调度 CLI**。Router 用 `collie spawn`、`send`、`subscribe`、`stop`、`answer` 和 `reply` 分派及反馈。worker 用 `collie report` 回报，对应 pane 由 `HERDR_PANE_ID` 识别。用 `collie list` 查看 worker，用 `collie hook SOURCE TEXT` 模拟外部事件。参数和 Router 手册见 [cli.go](internal/collie/cli.go)。
- **持久化**。默认数据目录是 `~/.collie`（`COLLIE_HOME` 可覆盖）。`herd.db` 保存事件、线程、worker、对话、定时器、设置和配对令牌哈希。hub 的 `token` 和 `addr` 供 CLI 发现本机服务，node 用 `node.json` 保存 hub 地址和设备令牌。Router 的 `router/` 目录保存生成的手册与 `memory/`。`go run` 启动时会写出稳定的 CLI 路径 `bin/collie`，供 Agent 调用。
- **运行参数**。用 `collie --help` 和 `collie node --help` 查看参数。hub 支持 `COLLIE_ADDR`、`HERDR_SOCKET_PATH`、`COLLIE_TOKEN` 和 `COLLIE_URL`。node 的 `--ui` 控制本机界面监听地址，传 `off` 可关闭。

## 最常见的坑

坑按主题记录在 `docs/pitfall/`，动某个模块前先读对应的文件。

- Router 的 herdr 名字不一定是 `collie`，发消息用 `routerAgent()`；看不到 Router 不代表它退出了（`docs/pitfall/router.md`）
- **herdr 0.8**。按 pane ID 查找 Agent，Collie 持久化自己启动的 Agent 名称。遇到 `launch_pending` 时，只有确认 Agent 空闲且终端无选择框才回退为粘贴输入。Claude 目录信任提示可能被报告为空闲，Collie 会结合终端内容识别并标记为待确认。
- **同一会话的识别**。工作目录可能同时有多个会话。Antigravity 会通过会话索引和目录定位记录，匹配不到时显示终端输出。外部会话和 herdr 会话去重需核对会话 ID、来源和运行电脑。接管及发送限制见 [外部会话](docs/pitfall/external-sessions.md)。

## 目录结构

| 路径 | 内容 |
| --- | --- |
| `main.go` | 入口，包含版本号和 `collie.Main` |
| `internal/collie/` | hub（`hub.go`）、node（`node.go`）、herdr 客户端（`herdr.go`）与多机路由（`fleet.go`） |
| `internal/collie/` 会话 | HTTP 与 SSE（`server.go`）、对话（`chat.go`、`agy.go`）、外部会话与接管（`external.go`、`desktop.go`、`handover.go`）、输入框 `/` 补全（`commands*.go`，`docs/pitfall/slash-commands.md`） |
| `internal/collie/` 调度 | 调度（`super.go`）、API 与 Webhook（`superapi.go`）、CLI（`cli.go`）、数据与持久化（`store.go`、`db.go`） |
| `web/` | 原生 HTML/CSS/JS，由 [embed.go](web/embed.go) 打进二进制 |
| `scripts/` | `release.sh` 交叉编译和发布，`demo.sh` 用真实前端和 `scripts/demo/demo.js` 的示例数据生成在线体验 |
| `docs/` | 项目文档（[文档索引](docs/README.md)）及 GitHub Pages 站点（`index.html`、`install.sh`、`demo/`、`img/`） |

# collie

面向使用者的介绍、安装和用法写在 `README.md`，本文件只写给 agent 和开发者。

本仓库 fork 自 [lihuanyu0w0/collie](https://github.com/lihuanyu0w0/collie)，remote `upstream` 指向它（只拉不推）。同步：`git fetch upstream && git merge upstream/main`，解决冲突后照常验证。

## 约定

- **本机和账号相关的值**（主机名、本地路径、个人凭证的位置）写在 `CLAUDE.local.md`，不提交。
- **踩到坑随手记下来**。各类文档放在哪、怎么归档，见 `docs/README.md`。
- **本文件保持简短**：只放项目是什么、这些约定、验证命令、最常见的坑和目录结构。架构写在 `docs/architecture.md`，搭建、部署、发布步骤写在 `docs/runbook/`，在这里留链接。
- **常规验证入口**：Git 钩子、CI 和发布流程共用 `scripts/check.sh`。
- **项目级 `.claude/` 只放本项目特有的内容**：`.claude/rules/` 下的规则，以及 `.claude/skills/` 下针对本项目部署、发布或领域流程的技能。通用技能留在用户级。
- **界面改动遵循设计系统**：样式的值以 design token 的形式放在代码里，规则写在 `docs/design/DESIGN_SYSTEM.md`。改界面前先读它，第一次做界面时创建它。
- **引用承载细节的文档**：本文件里的规则、坑和目录表的行，结尾带上对应文档的路径，例如 ``| `server/` | Go 模块（`docs/server/server.md`） |``，让读的人知道下一步该打开什么。文档移动时同步更新这些路径。

## 构建与验证

需要 Go 1.25+。启动本地开发实例前运行 herdr 服务，或用 `--socket` 指定测试用的 herdr socket。开发实例使用独立的 Collie 数据目录和端口。

```bash
go build -o bin/collie .
COLLIE_HOME="$PWD/.collie-dev" ./bin/collie --addr 127.0.0.1:17789
```

- 运行 `sh scripts/check.sh` 进行 Go 构建、vet、测试以及前端语法检查和测试。
- 改了 `web/` 后运行 `sh scripts/demo.sh` 重建 `docs/demo/`，随源码一起提交。
- 在仓库内运行 `sh docs/install.sh hub` 会用源码构建并安装服务。安装 node 用 `sh docs/install.sh node --hub URL --code CODE`，已配对后可只传 `node`。Linux 后台服务日志用 `journalctl --user -u collie-hub -f`，node 服务名为 `collie-node`。macOS 日志在 `~/Library/Logs/collie-hub.log` 和 `~/Library/Logs/collie-node.log`。
- 发布前运行 `sh scripts/release.sh v2026.1124.1 --dry`，四平台产物和校验和输出到 `dist/`。核对后用同一版本号和 `--publish` 发布这些产物。正式版本号格式为 `vYYYY.MMDD.N`。
- 部署上线（发布 Release、切换 hub、逐台升级 node、线上核对）按 `collie-deploy` 技能做（`.claude/skills/collie-deploy/SKILL.md`）。

## 架构与调试

浏览器通过 HTTP 和 SSE 连到 hub。hub 把前端打包进 Go 二进制，通过 Unix socket 的 NDJSON API 控制本机 herdr。node 通过出站 yamux 隧道转发远端 herdr 事件及 pane 请求。

- **会话**。对话流来自 Agent 会话记录，终端请求直接走 herdr。外部会话按进程、会话 ID 或工作目录定位记录，具体方式随 Agent 和桌面应用而异。决策卡片从终端状态识别问题和选项，再把选择转换成对应的终端输入。
- **Router 和 worker**。聊天、Webhook、定时器和 worker 状态作为 Trigger 入队。先按来源策略（`respond` / `candidate` / `ignore`）处理，再交给 Router 或订阅该线程的 worker。会话忙时排队，空闲后可批量投递。超过 700 字符的投递正文可用 `collie show ID` 取回。
- **调度 CLI**。Router 用 `collie spawn`、`send`、`subscribe`、`stop`、`answer` 和 `reply` 分派及反馈。worker 用 `collie report` 回报，对应 pane 由 `HERDR_PANE_ID` 识别。用 `collie list` 查看 worker，用 `collie hook SOURCE TEXT` 模拟外部事件。参数和 Router 手册见 [cli.go](internal/collie/cli.go)。
- **Web Push**。hub 用 VAPID 把需要你处理的通知推送到订阅过的浏览器，sw.js 负责显示，页面关掉也能收到（`docs/pitfall/notifications.md`）。
- **通知直达决策**。需要人处理的通知（worker 的对话框、`--opt` / `--check`）链接到 `#/d/<entry id>`，任意会话的对话框链接到 `#/d/pane/<pane id>`，页面用 `GET /api/super/decision` 定位并高亮卡片。通知正文只写任务名和选项，不放屏幕内容（`docs/pitfall/notifications.md`）。
- **工作台**。内置扩展，在「接入」里打开才生效（默认关）。侧栏底部田字格进 `#/wb`：钉网页（能嵌就用 iframe，否则开新标签）和 Collie 自己的页面。每个模块按 off/read/keep 决定给不给 Agent：Agent 用 `collie wb ls/read` 读，keep 档每天存快照到 `router/wb/`，做梦时提炼进记忆（`docs/workbench/workbench.md`）。
- **持久化**。默认数据目录是 `~/.collie`（`COLLIE_HOME` 可覆盖）。`herd.db` 保存事件、线程、worker、对话、定时器、设置和配对令牌哈希。hub 的 `token` 和 `addr` 供 CLI 发现本机服务，node 用 `node.json` 保存 hub 地址和设备令牌。Router 的 `router/` 目录保存生成的手册与 `memory/`。`go run` 启动时会写出稳定的 CLI 路径 `bin/collie`，供 Agent 调用。
- **运行参数**。用 `collie --help` 和 `collie node --help` 查看参数。hub 支持 `COLLIE_ADDR`、`HERDR_SOCKET_PATH`、`COLLIE_TOKEN` 和 `COLLIE_URL`。node 的 `--ui` 控制本机界面监听地址，传 `off` 可关闭。

## 最常见的坑

坑按主题记录在 `docs/pitfall/`，动某个模块前先读对应的文件。

- 手机布局按 `(max-width: 760px), (max-height: 500px) and (pointer: coarse)` 判断（横屏手机也是单栏），`narrow()` 和 CSS 要一起改；iOS 键盘只缩 `visualViewport`，高度用 `--vh`（`docs/pitfall/mobile.md`）
- Router 的 herdr 名字不一定是 `collie`，发消息用 `routerAgent()`；看不到 Router 不代表它退出了（`docs/pitfall/router.md`）
- **herdr 0.8**。按 pane ID 查找 Agent，Collie 持久化自己启动的 Agent 名称。遇到 `launch_pending` 时，只有确认 Agent 空闲且终端无选择框才回退为粘贴输入。Claude 目录信任提示可能被报告为空闲，Collie 会结合终端内容识别并标记为待确认。
- 往会话发消息一律走 `Herdr.Prompt`（发后回读确认，不要绕过它直接 `agent.prompt`）；tick 里不在列表中的 worker 不一定退出了：可能刚起来，或它那台电脑这轮没答话（`docs/pitfall/delivery.md`）
- collie 常被 `kill -9`、被自更新 exec 替换，子进程得自己能退出：不要用 `script`/pty 包常驻程序，exec 后要回收旧子进程（`docs/pitfall/child-processes.md`）
- 扩展（飞书、微信）只能经 `internal/extproto` 的协议和 Collie 说话，规范在 `extensions/README.md`：同一个扩展全局只跑一份，发送按 ref 只发一次、结果不明不重发；上游 LarkAgentX 一行不改，适配层在 `extensions/larkx/pkg/`，触发在 Collie 侧判定（`docs/pitfall/extensions.md`）
- **同一会话的识别**。工作目录可能同时有多个会话。Antigravity 会通过会话索引和目录定位记录，匹配不到时显示终端输出。外部会话和 herdr 会话去重需核对会话 ID、来源和运行电脑。接管及发送限制见 [外部会话](docs/pitfall/external-sessions.md)。

## 目录结构

| 路径 | 内容 |
| --- | --- |
| `main.go` | 入口，包含版本号和 `collie.Main` |
| `internal/collie/` | hub（`hub.go`）、node（`node.go`）、herdr 客户端（`herdr.go`）与多机路由（`fleet.go`） |
| `internal/collie/` 会话 | HTTP 与 SSE（`server.go`）、对话（`chat.go`、`agy.go`）、外部会话与接管（`external.go`、`desktop.go`、`handover.go`）、输入框 `/` 补全（`commands*.go`，`docs/pitfall/slash-commands.md`） |
| `internal/collie/` 调度 | 调度（`super.go`）、API 与 Webhook（`superapi.go`）、Web Push（`webpush.go`、`push.go`、`pushapi.go`）、CLI（`cli.go`）、数据与持久化（`store.go`、`db.go`） |
| `extensions/` | 扩展，一个子目录一个，`collie-extension.json` 编进二进制；规范见 `extensions/README.md` |
| `internal/collie/` 扩展 | 宿主（`ext_host.go`）、进程与安装（`ext_proc.go`、`ext_install.go`）、收件箱与触发（`inbox.go`）、回复与尾标（`ext_outbox.go`）、扩展市场（`ext_market.go`，Release 包的下载和解包在 `ext_market_pkg.go`）、大文件下载链接（`ext_links.go`）；协议在 `internal/extproto/`（`docs/pitfall/extensions.md`） |
| `internal/collie/` 工作台 | 模块与权限（`workbench.go`）、嵌入探测与读取（`workbench_fetch.go`）、快照（`workbench_snap.go`）、hub 保管登录的代理与 cookie（`workbench_proxy.go`、`workbench_jar.go`）、API（`workbench_api.go`）、CLI（`cli_wb.go`）（`docs/workbench/workbench.md`） |
| `web/` | 原生 HTML/CSS/JS，由 [embed.go](web/embed.go) 打进二进制；样式变量和皮肤见 `docs/design/DESIGN_SYSTEM.md` |
| `scripts/` | `release.sh` 交叉编译和发布，`demo.sh` 用真实前端和 `scripts/demo/demo.js` 的示例数据生成在线体验 |
| `docs/` | 项目文档（[文档索引](docs/README.md)）及 GitHub Pages 站点（`index.html`、`install.sh`、`demo/`、`img/`） |

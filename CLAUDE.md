# collie

在手机上管理多台电脑上的 Claude Code、Codex、Antigravity 会话：一个 Go 单文件 hub 加上内嵌的网页前端，各台电脑作为 node 主动连到 hub。面向同时开着多台电脑、跑多个 agent 的开发者。对外介绍见 `README.md`。

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

- Go：`go build ./...`、`go vet ./...`、`go test ./... -count=1`
- 前端：`node --check web/app.js`；改了 `web/` 后运行 `sh scripts/demo.sh` 重建 `docs/demo/`，和改动一起提交

## 最常见的坑

坑按主题记录在 `docs/pitfall/`，动某个模块前先读对应的文件。

## 目录结构

| 路径 | 内容 |
| --- | --- |
| `main.go` | 入口：版本号和 `collie.Main` |
| `internal/collie/` | hub、node 和 CLI（一个 Go 包） |
| `web/` | 前端：原生 HTML/CSS/JS，无构建步骤，由 `embed.go` 打进二进制 |
| `scripts/` | `release.sh` 交叉编译和发布，`demo.sh` 构建在线体验 |
| `docs/` | 项目文档，各类文档放在哪见 `docs/README.md`；同时是 GitHub Pages 站点（`index.html`、`install.sh`、`demo/`、`img/`） |

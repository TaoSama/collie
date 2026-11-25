# 飞书（larkx）

以你本人的飞书身份收发消息：扫码登录飞书网页版，你的会话出现在「自动化 → 收件箱」，打开某个会话后，以触发词（默认 `/ai`）开头的消息交给 Collie 或这个会话的专属 agent，回复以你的身份发在原消息的话题下，末尾带尾标。

## 打开前要知道

- **非官方协议**：底层是 [cv-cat/LarkAgentX](https://github.com/cv-cat/LarkAgentX)，逆向飞书网页版。飞书升级后可能失效，账号可能被风控。用哪个账号由你决定；公司账号先确认公司规定。
- **登录态是完整的网页会话**：只保存在运行它的那台电脑的 `COLLIE_HOME/ext/larkx/secrets.json`（0600），不上传 hub。消息库 `larkx.db` 是明文，也只在那台电脑。
- **跑在哪台**：在「接入」里任选。默认是 Router 所在的电脑。放在机房 IP 的 hub 上，风控风险可能更高。
- **前提**：那台电脑有 `python3`（≥3.9），首次启动能访问 raw.githubusercontent.com、codeload.github.com、pypi.org、files.pythonhosted.org（扩展进程继承 Collie 的代理变量）。

## 这个目录里有什么

本仓库**不放 larkx 的代码**（上游没有许可证），只有：

- `collie-extension.json`：清单。`source.base` 锁定 [nwverse/persona-extensions](https://github.com/nwverse/persona-extensions) 的 commit `443ac26`，`source.files` 列出要下载的每个文件和 sha256。第一次打开时 Collie 逐个下载并校验，装到 `COLLIE_HOME/ext/larkx/pkg/<version>/`。larkx 启动后再按它自己锁定的版本下载 LarkAgentX 和依赖（也都校验 hash）。
- 本说明。

## 和 Collie 怎么接（不改 larkx 的任何逻辑）

- **握手、登录、状态**：`initialize`、`connector.login.step`（返回要编码成二维码的文字）、`connector.status`、`connector.logout`。登录态经 `connector.secrets.*` 存在本机。
- **收消息**：larkx 有新消息时发 `connector.changed`，Collie 拉 `connector.archive.changes`，在 Collie 侧判定会话开关、谁能唤起和触发词。所以触发词能改、能放开给别人，代价是约 3 秒延迟。第一次拉取只记游标，历史不触发；10 分钟前的消息不触发；Collie 自己发出的回复回到 archive 里时按 message id、原文和尾标认出来，不会再触发。
- **larkx 自带的 `/agent`**：larkx 仍会把你发的 `/agent …` 用 `channel.receive` 交过来，Collie 应答但不处理（larkx 不再重试）。所以它的 `agent_reply_suffix`、`agent_mention`、`agent_group_topics` 三个配置在 Collie 里不显示，只保留「站点」。
- **发消息**：`connector.actions.do{name:"larkx_send", args:{chat, text, reply_to}, request_id}`，`request_id` 就是 Collie 的发送 ref。larkx 按它保证只发一次，结果不明时不重发，Collie 把它标成「可能已发出，去飞书确认」。
- **按会话**，不按话题：archive 里没有话题信息，回复用 `reply_to` 挂到原消息的话题下。

## 测试

```sh
# Collie 侧完整流程用测试扩展（不需要 Python）
go test ./internal/collie -run 'TestExt|TestAfterTrigger|TestFooter|TestCatalog|TestInstallPinned' -count=1
# 真实的 larkx 包 + 它自带的 fixture 后端（不联网、不需要账号）：先把锁定 commit 的 extensions/larkx 解到某个目录
COLLIE_LARKX_PKG=/path/to/extensions/larkx go test ./internal/collie -run TestLarkxFixture -count=1
```

`TestLarkxFixture` 用清单里锁定的 sha256 从本地目录「下载」安装，再走握手、扫码、archive 和一次发送。开发实例里想不登录就看效果：`COLLIE_EXT_PASS_ENV=LARKX_PERSONA_BACKEND LARKX_PERSONA_BACKEND=fixture collie ...`。

## 实测

- 2026-11-25，devbox 开发实例：真实下载安装（15 个文件 sha256 全部通过），fixture 后端下扫码、收件箱、发送通过。**真实账号未测**：需要你扫码，见上线报告。

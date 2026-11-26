# 微信扩展（内置）

用腾讯官方的 iLink Bot 接口扫码绑定一个微信 bot。你私聊这个 bot 的消息交给 Collie，Collie 的回复、进度和定时任务结果发回微信。只处理扫码那个微信号（绑定人）的消息，其他人的消息标记已处理后静默跳过。

作为 `collie ext-wechat` 子进程运行，stdin/stdout 上跑 `internal/extproto` 协议，日志写 stderr（每行一个 JSON，字段白名单，不含正文、号码和 token）。规范见 [`../README.md`](../README.md)。

## 风险：回复窗口

- 你每发一条消息，微信开放 **23 小时**的回复窗口，整个账号共享 **10 条**额度：分段、图片、文件、进度、续期提醒都算。
- 进度消息每个请求最多 3 条、每条 ≤2000 字，并且总给最终回复留 3 条额度；窗口或额度不够后，这个请求不再发进度。
- 窗口过期或额度用完时，回复和结果排队（`channel.send` 回 `deferred`），等你再发一条消息后送达。窗口最后 1 小时、还有待发内容时，会发一条续期提醒。
- 发出去但没拿到明确回执的消息标为 `uncertain`，**永不自动重发**；界面提示去微信确认。微信明确拒收的，最多试 3 次后过期。

## 怎么登录

在「自动化 → 接入」打开微信扩展，点登录：

1. 扩展调用 `get_bot_qrcode?bot_type=3`，回 `{step:"qr", kind:"qr", url:<qrcode_img_content>, interval:2}`，界面把 url 画成二维码。
2. 之后每次 `connector.login.step{step:"qr"}` 长轮询 `get_qrcode_status`：`wait`、`scaned`（提示在手机上确认）、`scaned_but_redirect`（换到跳转的 host 继续轮询）、`expired`（自动换新码，最多 3 次）、`need_verifycode`（回 `kind:"secret"`，下一次 `{step:"code", secret:"<手机上的数字>"}`）、`confirmed`。
3. 确认后凭证（token、bot_id、user_id、base_url）经 `connector.secrets.save` 交给 Collie 保存，回 `{done:true, account:"你的微信"}`。登录返回的绑定人 id 放在 `account_id`（接入页悬停可见），`detail` 的「绑定人」显示缩短的 id。
4. 新登录指纹（账号 + token）和上次不同时，游标、context token、去重键、outbox、额度全部清空，先排空历史（基线），之后的消息才处理。

扫码确认和消息轮询都只提供 bot 或用户的标识，`getconfig` 提供输入状态凭据。Collie 无法读取微信端的会话备注和头像，默认显示「微信ClawBot」，可在「接入 › 微信」修改「昵称」和「头像」（只改 Collie 里的显示）。

`getupdates` 回 `errcode -14` 视为登录过期：状态变为 `not_signed_in`，发 `connector.changed{scope:"status"}`，需要重新扫码。轮询失败按 1 秒到 60 秒指数退避，成功后清零。

## 和 Collie 的接口

| 方向 | 方法 | 说明 |
| --- | --- | --- |
| 扩展 → Collie | `channel.receive` | `message.id` 是 deliveryKey（账号、聊天、消息 id），`owner:true`，图片和文件为解密后的 base64，`quote` 是引用文本，`unsupported` 列出随消息一起但不能转交的类型。Collie 回 `{}` 才算交付；回错（如 `hub_unavailable`）时这个聊天阻塞，只重试队首那条 |
| Collie → 扩展 | `channel.send{kind:answer\|push}` | 冻结答案存在扩展自己的数据目录，进 reply outbox 后发送：先图片、再文件、最后文字，文字按 3800 个 UTF-16 单位原样切分 |
| Collie → 扩展 | `channel.send{kind:progress}` | 按 `reply_to` 计每个请求的 3 条进度 |
| Collie → 扩展 | `channel.send{kind:result}` | 进结果队列：文字 ≤3 段；带文件为附件模式（摘要 + 文件）；带 link 为下载模式（摘要 + 链接两条）；超过 3 段时摘要 + 全文作为 `result.md` 附件 |
| 扩展 → Collie | `channel.delivery` | `sent / deferred / uncertain / expired`，`reason` 是给人看的中文短句 |
| Collie → 扩展 | `channel.interest` | 每 30 秒一次：哪些聊天有启用中的定时或待发结果，决定续期提醒 |
| Collie → 扩展 | `connector.status` | `detail` 里有绑定人、阶段、连续错误、上次成功轮询、回复窗口剩余、剩余额度、待发回复 |

超过 40 MiB 的文件不经过本扩展：Collie 发 24 小时有效的签名下载链接，`answer` 写在正文里，`result` 放在 `link`，本扩展按下载模式发「摘要 + 完整结果下载：链接」（`../README.md` 第 3.7 节）。

## 数据

`initialize` 给的 `data_dir` 下，全部 0600 文件 / 0700 目录，原子写（临时文件、fsync、rename、逐级 fsync 目录）：

| 文件 | 内容 |
| --- | --- |
| `state.json` | 游标、context token、去重键、回复分段计划、窗口与额度、入站水位、outbox 引用、已交付消息的内容哈希 |
| `state.json.inbox.json` | 已收下、还没交给 Collie 的消息全文（16 MiB、1 万条上限，文件锁） |
| `answers.json`、`answer-blobs/` | 冻结的答案和它们的图片、文件 |
| `pending-media.json`、`inbound-blobs/` | 只发了图片或文件、等下一条文字一起交给 Collie 的附件 |
| `results.json`、`artifacts/` | 结果队列和结果附件 |
| `result-outbox.json` | 结果投递的 claim / defer / receipt 意图 |

## 怎么测试

```bash
go build ./... && go vet ./extensions/wechat/... && go test -race ./extensions/wechat/... -count=1
```

测试全部离线：iLink 和 CDN 用 `http.RoundTripper` 桩（`testkit_test.go`），端到端测试 `e2e_test.go` 用 `httptest` TLS 服务冒充 iLink，用 `io.Pipe` 接一个假 Collie，走完 登录 → 基线 → 收消息 → receive → channel.send → 分段发送 → 回执 → delivery 事件 → 同 ref 不重发 → 登录过期。

原测试逐条移植：`testdata/original.txt` 是原实现`gateway/tests/{wechat,runtime,results,gateway}` 的全部用例，`testdata/ported.txt` 写明每条对应的 Go 测试或跳过原因，每个 Go 测试上方有 `// from: <原文件>: <原用例名>`。`coverage_test.go` 校验三者一一对应，少一条就失败。测试里 trace 总是打开，设 `WECHAT_TRACE=1` 可看到它的行。

## 移植对照表

原代码路径相对原仓库 `gateway/src/`。

| # | 逻辑 | 原代码 | Go |
| --- | --- | --- | --- |
| A1 | 默认 base、baseUrl 校验 | `wechat/client.ts` | `ilink.go`: `validateILinkBaseURL`、`NewILinkClient` |
| A2 | 请求头、`base_info.channel_version`、拒绝跳转 | `client.ts` `request`、`protocol.ts` | `ilink.go`: `request`、`ChannelVersion` |
| A3 | 长轮询与时长提示 | `client.ts` `pollBatch`、`runtime/budgets.ts` | `poll.go`: `pollBatch`；`budgets.go` |
| A4 | 只收 `message_type==1`，context_token | `client.ts` | `poll.go`: `pollBatch` |
| A5 | 去重 id 与 deliveryKey | `client.ts`、`runtime/state.ts` | `poll.go`: `pollBatch`；`state.go`: `ComputeDeliveryKey` |
| A6 | 发送回执 | `wechat/send-receipt.ts` | `receipt.go`: `ParseILinkJSON`、`ValidateSendReceipt`、`IsRejectedSendReceipt` |
| A7 | 每个聊天一把发送锁 | `client.ts` `withChatSend` | `ilink.go`: `withChatSend` |
| A8 | 发送前检查、按 final / progress 扣额度 | `client.ts` | `ilink.go`: `SetBeforeSendCheck`、`consumeSendBudget` |
| A9 | sendmessage 请求体 | `client.ts` `sendMessageItem` | `ilink.go`: `sendMessageItem` |
| A10 | 发送中上下文变化即 deferred | `client.ts`、`wechat/file-delivery.ts` | `send.go`: `sendImageReply`、`sendFileReply` |
| A11 | 分段 | `client.ts` `splitWeChatReply` | `text.go`: `SplitWeChatReply` |
| A12 | 分段计划冻结、意图→发送→确认、不明不重发 | `client.ts` `sendMarkdownReply`、`state.ts` | `send.go`: `sendMarkdownReply`；`replies.go` |
| A13 | 超时与传输预算 | `runtime/budgets.ts`、`transfer-budget.ts`、`abort.ts` | `budgets.go`: 常量、`transferTimeoutMs`、`withTimeout`（`context` 替代 `beforeAbort`） |
| A14 | typing | `wechat/typing.ts` | `typing.go`: `TypingIndicator` |
| B1 | item 类型、语音转写、引用、附件数、过滤 | `wechat/inbound-content.ts` | `inbound.go`: `ParseInboundContent` |
| B2 | AES key 兼容 hex / base64 | `inbound-content.ts` | `inbound.go`: `NormalizeAESKey` |
| B3 | CDN 下载、解密、大小、魔数、文件名 | `wechat/cdn-download.ts` | `cdn.go`: `downloadInboundMedia`、`DecryptAES128ECB`、`MediaDownloadURL`、`safeFileName` |
| B4 | CDN 上传与重试 | `wechat/cdn-upload.ts` | `cdn.go`: `cdnUploader.upload` |
| B5 | 回复图片和文件 | `media-upload.ts`、`media-delivery.ts`、`file-upload.ts`、`file-delivery.ts` | `media.go`: `deliverImages`、`ImageUploader`、`FileUploader`；`send.go`: `sendImageReply`、`sendFileReply` |
| B6 | 入站附件交给引擎 | `gateway.ts` `storeInboundMedia` | `gateway_inbound.go`: `storeInboundMedia`、`forward`；`stash.go` |
| B7 | 回复里的图片和文件 | `gateway.ts` `deliverReply` | `gateway_reply.go`: `sendReply`、`attachment`；`answers.go` |
| C1 | 原子写私有 JSON | `runtime/state.ts` `writePrivateJsonSync` | `atomic.go`: `writePrivateJSON` |
| C2 | 登录指纹、换号清空 | `state.ts` | `state.go`: `LoginFingerprint`、`restore` |
| C3 | 基线状态机 | `state.ts` | `state.go`: `AssertCanEstablishBaseline`、`BeginBaseline`、`MarkBaselineEstablished`、`IsReadyForActivePolling` |
| C4 | 已处理键 1000 条、保留回复证据 | `state.ts` | `state.go`: `RecordProcessedMessageKey` |
| C5 | 回复计划 1000 条、拒收 3 次、uncertain 与永久失败 | `state.ts` | `replies.go` |
| C6 | 续期提醒独立计划 | `state.ts` `prepareRenewalReplyDelivery` | `replies.go`: `PrepareRenewalReplyDelivery` |
| C7 | 游标只在一批处理完后提交 | `state.ts`、`gateway.ts` | `state.go`: `CommitCursor`；`gateway.go`: `poll` |
| C8 | 入站水位与时钟偏差 | `runtime/delivery-state.ts` | `delivery_state.go`: `RecordInboundContext` |
| C9 | 额度策略 | `runtime/context-policy.ts`、`delivery-state.ts` | `policy.go`；`delivery_state.go`: `SendDeferReason`、`ConsumeSendBudget`、`RemainingSendBudget` |
| C10 | outbox 状态、来源、容量、背压 | `delivery-state.ts` | `delivery_state.go`: `PutOutbox`、`UpdateOutbox`、`RemoveOutbox`、`AssertOutboxCapacity` |
| C11 | 重启恢复窗口和额度 | `runtime/account-context.ts` | `policy.go`: `restoreAccountContext` |
| C12 | 持久入站收件箱与文件锁 | `runtime/inbox.ts` | `inbox.go`: `FileInboundInbox`；`filelock.go`: `withFileLock` |
| D1 | 基线排空 | `gateway.ts` `establishBaseline` | `gateway.go`: `establishBaseline` |
| D2 | pollOnce | `gateway.ts` `pollOnce` | `gateway.go`: `pollOnce`、`poll` |
| D3 | 准入 | `gateway.ts` `admitMessage` | `gateway_inbound.go`: `admitMessage` |
| D4 | 非白名单标记已处理后跳过 | `gateway.ts` | `gateway_inbound.go`: `processMessage`、`admitMessage`；`gateway.go`: `assertChatAllowed` |
| D5 | InboundRunner | `runtime/inbound-runner.ts` | `runner.go`: `InboundRunner` |
| D6 | 处理一条消息 | `gateway.ts` `processMessage` | `gateway_inbound.go`: `processMessage`、`process` |
| D7 | 固定文案与冲突 | `gateway.ts`、`leader/contracts.ts` | 不回固定文案（见下）；`gateway.go`: `MediaFailedNote`；`gateway_inbound.go`: `conflicts` |
| D8 | 投递回复 | `gateway.ts` `deliverReply` | `gateway_reply.go`: `deliverReply` |
| D9 | flushRepliesOnce | `gateway.ts` | `gateway_reply.go`: `flushRepliesOnce`、`frozenReply` |
| D10 | 引擎确认 | `gateway.ts` `ackEngineReply` | `gateway_reply.go`: `ackEngineReply`（发 `channel.delivery`） |
| D11 | 进度 | `leader/progress.ts`、`gateway.ts` `progressSink` | `progress.go`: `QueryProgress`；`gateway_inbound.go`: `progressSink`；`gateway_send.go`: `sendProgress` |
| D12 | 通知循环顺序 | `gateway.ts` `deliverNotificationsOnce` | `gateway.go`: `deliverNotificationsOnce`、`renewalInterestedChats`、`SetInterest` |
| D13 | 结果投递状态机 | `results/contracts.ts`、`delivery.ts`、`dispatcher.ts`、`outbox.ts` | `result_contract.go`、`result_runner.go`、`dispatcher.go`、`result_outbox.go`；结果服务端语义在 `result_store.go` |
| D14 | 旧版通知 outbox | `runtime/notification-outbox.ts` | 并入 D13 的同一条队列（`gateway_send.go`: `sendResult`）；未确认发送的诊断行 `gateway.go`: `reportNotificationSendFailure` |
| D15 | 通知失败恢复证据 | `runtime/notification-recovery-store.ts` | 不移植（见下） |
| D16 | 续期提醒 | `results/renewal.ts` | `renewal.go`: `RenewalNotifier` |
| D17 | runLoop | `gateway.ts` `runLoop` | `runloop.go`: `runLoop`、`pollBackoffMs`、`notifyOnce` |
| E1 | Markdown 原样发送 | `client.ts` verbatim 路径 | `send.go`: `sendMarkdownReply`（只有 `wechat-verbatim-v2`） |
| E2 | 旧版 digest 兼容 | `client.ts` | 不移植（见下） |
| E3 | 查看会话列表、控制台链接 | `packages/wechat-text/index.js` | 都不移植（见下） |
| F1 | 健康状态 | `runtime/health.ts` | `health.go`: `HealthTracker`、`SanitizeErrorType`；`bot.go`: `status`、`statusDetail` |
| F2 | message_latency trace | `runtime/telemetry.ts` | `telemetry.go`: `traceEvent`、`traceSpan`；默认关（见下） |
| F3 | 请求与通知诊断 | `runtime/request-diagnostics.ts`、`notification-diagnostics.ts` | 不移植（见下）；错误仍写一行日志（`writeLogLine`） |
| G1 | 扫码登录 | 新增 | `login.go`: `loginStep`、`loginClient` |
| G2 | `-14` 登录过期 | 新增 | `poll.go`: `pollBatch` → `ErrSessionExpired`；`bot.go`: `run` |
| G3 | 轮询失败退避 | 新增 | `runloop.go`: `pollBackoffMs` |

## 有意偏离

- **Markdown 原样发送**：所有发送都走原来的 verbatim 路径，`markdownToWeChat` 和旧 digest 兼容（`legacyDigest`、`wechat-plain-v1`、引用清洗）不移植。分段仍切原文。
- **快捷指令都删除**：「控制台链接」不移植；「查看会话列表」这类话不再绕过模型，和别的消息一样交给 Collie 的 Router 回答。
- **Controller / Leader 换成 Collie**：白名单只认登录时的 `ilink_user_id`；「交给引擎」是 `channel.receive`，答案经 `channel.send` 异步到达，冻结在扩展自己的数据目录（替代 `getFrozenReply`）；`ackEngineReply` 是 `channel.delivery` 事件；正在处理时来的新消息（steer）也直接 receive。因为答案是异步的，消息交给 Collie 后到答案到达之前（最长 `MaxEngineRequestMs`）这个聊天算「有回复待发」，结果投递会等它。
- **不替 Router 回话**：收不到的内容（视频等）、没有文字又下载失败的附件，原来回一段固定文案；现在照常交给 Collie，`channel.receive` 的 `unsupported` 写明收不到什么（下载失败时正文是 `MediaFailedNote`），由 Router 决定怎么回。
- **同一 deliveryKey 内容不同**：原来由引擎判定并回固定文案；现在扩展记录交给 Collie 的每条消息的内容哈希（最多 1000 条），不同就当作已经交过的那条，标记已处理、记一行 `message_conflict_ignored` 日志，不回复。
- **通知诊断和恢复证据不移植**：原来每轮通知循环的失败、恢复写成日志事件并存进 `notification-recovery.json`；它们不改变任何投递，观察的大多是原来远端服务的请求。健康看 `connector.status` 的 `detail` 和 Collie 的健康检查；可能已发出的结果仍写一行 `notification_send_failed`。
- **trace 默认关**：`message_latency` 每次轮询都写好几行，会把真正的报错挤出接入页的最近日志。要看时在 Collie 侧设 `COLLIE_EXT_PASS_ENV=WECHAT_TRACE WECHAT_TRACE=1`。
- **只有媒体的消息**：原来存成 Controller 的附件记录；现在存进 `pending-media.json`，和同一聊天的下一条文字一起 receive，超过回复窗口（23 小时）的不再带上。
- **通知 outbox 并入结果队列**：一条队列，行为取并集。明确拒收的结果按结果队列的规则改为等待（`deferred`，原因「微信拒收」），不再像旧通知 outbox 那样标为 uncertain。
- **健康不写 `health.json`**：放进 `connector.status` 的 `detail`；日志写 stderr，每行一个 JSON。
- **trace 字段白名单**去掉了只属于 Codex 引擎的字段（thread、turn、token 用量等）。
- **答案带文件**：原来只有图片；现在 `channel.send` 的 `files` 也按图片之后、文字之前发送，每个文件一份独立计划（`file-reply:<ref>:<序号>`）。
- **超长结果**：原来由模型写摘要；现在取前 3800 个字作为摘要，全文作为 `result.md` 附件。

## 实测

2026-11-26，开发实例（devbox，未发版）：打开扩展后 `connector.login.step{}` 从 `ilinkai.weixin.qq.com` 拿到真二维码（`liteapp.weixin.qq.com/q/…?bot_type=3`），再问一次 `step:"qr"` 长轮询 8 秒回 `wait`；没有扫码，后面的步骤还没用真号走过。

上线前用真号按下面走一遍，结果记在这里，注明日期和版本：

1. 「接入」打开微信，运行在 hub，点登录，出现二维码。
2. 用要绑定的微信扫码：界面变成「已扫码，请在手机上确认」；手机上确认。
3. 如果微信要求输入手机上显示的数字：界面出现输入框，填入后继续轮询直到「已登录」；填错时提示重填。
4. 状态变为已登录，`detail` 里有绑定人；收件箱出现「你和 bot 的私聊」，开关默认打开。
5. 登录前发给 bot 的旧消息不处理（基线）。
6. 给 bot 发一句普通请求：Collie 收到（事件里来源 `ext:wechat`），回复不带尾标、Markdown 原样发回微信。
7. 让 agent `collie reply --thread ext:wechat:… --file` 发一张图、一个小文件、一个超过 40 MiB 的文件：前两个是微信里的图片和文件，最后一个是下载链接，手机能打开下载，24 小时后链接失效。
8. 进度：`--progress` 最多 3 条。
9. 重启 Collie（或关掉再打开扩展）：不重新要求扫码，已回复的消息不再回复，没回复的继续。
10. 在手机上退出 bot 或等登录过期：状态变为未登录（`-14`），重新扫码后游标和额度清空重来。
11. 等回复窗口用完（或额度 10 条用完）：回复变成「等你在微信发一条消息后送达」，再发一条后补发。

### 登录协议里哪些是推断的

腾讯没有正式公开 iLink 的扫码登录协议，下面这些是推断的，还没有用真号核对过：

- `get_qrcode_status` 的状态值和拼写：`wait`、`scaned`、`scaned_but_redirect`（带 `redirect_host`，之后到那个 host 继续轮询）、`expired`、`need_verifycode`、`verify_code_blocked`、`binded_redirect`、`confirmed`。
- 验证码只在 `need_verifycode` 之后的那一次请求带 `verify_code`；之后的轮询不再带。数字不对时服务端再回一次 `need_verifycode`。
- 二维码 5 分钟过期、最多刷新 3 次，是本地的约束，不是服务端规定。
- `confirmed` 带回 `bot_token`、`ilink_bot_id`、`ilink_user_id`、`baseurl`，扫码的人（`ilink_user_id`）就是绑定人。
- 请求头 `iLink-App-Id: bot`、`iLink-App-ClientVersion`（2.4.6 按字节拼成整数），`get_bot_qrcode` 的 `local_token_list` 带上本机用过的旧 token。
- `getupdates` 回 `errcode -14` 表示登录过期。

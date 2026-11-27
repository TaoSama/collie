# 飞书（larkx）

以你本人的飞书身份收发消息：扫码登录飞书网页版，你的会话出现在「自动化 → 收件箱」，打开某个会话后，以触发词（默认 `/ai`）开头的消息交给 Collie 或这个会话的专属 agent，回复以你的身份发在原消息的话题下。回复是飞书富文本：Markdown 的段落、引用、行内代码、代码块和链接照样显示，尾标是末尾的一段引用。

## 打开前要知道

- **飞书网页版接口**：底层是 [cv-cat/LarkAgentX](https://github.com/cv-cat/LarkAgentX)，用的是飞书网页版接口，飞书升级后可能失效。用哪个账号由你决定；公司账号先确认公司规定。
- **登录态是完整的网页会话**：只保存在运行它的那台电脑的 `COLLIE_HOME/ext/larkx/secrets.json`（0600），不上传 hub。扩展自己不存历史消息，`data/` 里只有依赖包、已发送记录（`sent.json`，ref 和消息 id，保留 7 天）和 hub 不可达期间暂存的待补投队列（`pending.json`，送达后清除，最长保留 24 小时）。
- **跑在哪台**：在「接入」里任选。默认是 Router 所在的电脑。
- **前提**：那台电脑有 `python3`（≥3.9，带 pip），首次启动能访问 raw.githubusercontent.com、pypi.org、files.pythonhosted.org（扩展进程继承 Collie 的代理变量）。

## 这个目录里有什么

- `collie-extension.json`：清单。`source.base` 锁定 LarkAgentX 的 commit `97ad48e`，`source.files` 只列用到的 13 个文件和 sha256，第一次打开时逐个下载并校验。上游代码一行不改，本仓库也不放它（上游没有许可证）。
- `pkg/`：适配层，随 collie 二进制打包，安装时和下载的文件放在同一个包目录（`extensions/README.md` 第 9 节）。
  - `bin/larkx`：找一个 ≥3.9 的 `python3` 启动 `adapter/main.py`。
  - `adapter/main.py`：协议和状态。只用标准库，所以 Collie 立刻能握手；依赖在后台装进 `data/deps-<锁文件 hash>/`（`deps.py`，`pip --require-hashes`）。
  - `adapter/lark.py`：调用上游的扫码登录（`QrLogin`），将推送转成 Collie 消息，经网关（`_gateway_post`、`api`）发送帖子和表情。凭据由 `connector.secrets.*` 存取。
  - `adapter/lark_ws.py`：维护 WebSocket 连接，解码并分派推送。使用上游的地址、心跳、应答（`build_ws_url`、`heartbeat_loop`、`send_ack`）和消息解码。
  - `adapter/richtext.py`：Markdown 和飞书富文本互转。
  - `adapter/history.py`、`adapter/message_view.py`：按需读取最近消息，生成正文结构和预览，以及按消息资源键读取附件。
  - `adapter/fake.py`：不联网的假飞书，测试用（`COLLIE_LARKX_FAKE=1`）。
  - `requirements.txt`：上游用到的包，带 hash 锁定（`uv pip compile requirements.in --generate-hashes --universal --python-version 3.9`）。
- `test/test_adapter.py`：适配层的测试。

## 和 Collie 怎么接

- **重启后重新登录**：用保存的凭据登录时，连不上飞书（超时、连接失败、5xx）不算凭据过期：状态显示「正在连接飞书」，按 2s、5s、15s 退避重试，之后每分钟一次，日志里不提示重新导入。只有飞书明确说会话已失效、且隔 2 秒再问一次仍这么说，才算过期、要重新扫码。websocket 取 ticket 失败时同样先再确认一次。
- **握手、登录、状态**：`initialize`、`connector.login.step`（返回要编码成二维码的文字，每 2 秒轮询一次；过期自动换新码，最多 3 次）、`connector.status`、`connector.logout`。
- **收推送**：适配层自己读 websocket 的帧（上游的循环只认未压缩的 cmd 6）：压缩过的帧（gzip、zlib）先解开；cmd 6000 只推 sid，按 sid 补拉（`improto.PullPacketsBySidsRequest`）；cmd 14004 是云文档的评论、回复和 @你（`docs.PushDocFeedRequest`），每篇文档算一个会话（`doc:<feed id>`，类型「文档评论」），正文前写明是谁做了什么和文档链接。文档评论暂时只收不回，往这类会话发送会直接失败。读不了的帧计数，每 10 分钟最多记一条日志。
- **折叠的会话**：在飞书里被折叠的会话（会话的 feed 卡片挂在「折叠的会话」盒子下，`parentCardId` 不为 0，网关 `feed.PullFeedCardsByIdsRequest`），消息带上 `folded: true`，收件箱不列出它。和会话信息一样缓存一小时。
- **@自己唤起**（默认开）：你本人发的消息里用飞书的 @ 真的 @ 了自己（AT 元素的 userId 是你，不是打出来的文字），也算请求，交给 Collie 时去掉这个 @。别人 @ 你不算。和触发词一样，一收到就先加「在做了」。
- **群话题各开对话**（默认开）：群里不在话题里的请求开一段新对话（回复会在它下面开出话题），之后这个话题里的请求接着这段对话；私聊照旧是一段对话。关掉后群里的请求都进同一段对话。
- **收件箱显示**：「接入」里飞书扩展有三个开关，默认都开：免打扰的会话不显示、Meego 需求同步群不显示、Meego 缺陷同步群不显示。Meego 为每个需求或缺陷建的群，群描述就是它的链接（`…/story/detail/<id>` 或 `…/issue/detail/<id>`，域名 `meego.*` 或 `project.*`），适配层据此给消息带上 `chat_tags`（`meego-story`、`meego-issue`）；群名带「[需求同步群]」「[缺陷同步群]」的也算。只认群名不够：飞书把群名截到 64 个字符，长标题的「- [缺陷同步群]」常被截掉（2026-11-26 实测 54 个 Meego 群里有 19 个这样漏掉）。还没带过标签的旧会话按群名规则隐藏。和机器人或应用的私聊（成员的 `type` 是 2，真人是 1）带 `chat_tags: ["bot"]`，收件箱的「单聊」不算它们；读未读数时也一并带上，早就有的会话也能补上。它们是 Collie 自己的设置（清单里的 `x-hide`），改了不会重启扩展。隐藏只影响列表，你本人发的触发词照常处理。
- **已读和未读**（能力 `read`）：Collie 每 30 秒问一次每个会话在飞书里的未读数（`chats.PullChatsByIdsRequest`，100 个一批，不用一小时的名字缓存），以飞书的数为准。普通会话是 `lastMessagePositionBadgeCount - readPositionBadgeCount`；话题群（`chatMode` 3）飞书的数是未读的话题，用 `lastThreadPositionBadgeCount - readThreadPositionBadgeCount`。别人 @ 你或 @所有人（AT 元素的 userId 是你或 `all`）的消息带 `at_me`；每轮未读数也带上飞书自己记的未读 @（`messages.PullUnreadAtMessageIdsRequest` 给最新 100 条消息 id，`messages.PullMessagesByIdsRequest` 查出它们的会话，查过的记住），这样卡片里的 @、扩展没在跑时来的 @ 都算。设置里「在 Collie 里读过的同步为飞书已读」（默认关）打开后，在收件箱打开会话或点「全部已读」，适配层用 `messages.PutReadMessagesRequest` 把会话读到最后一条（`maxPosition` 取当时的 `lastMessagePosition`，答复里的 `readPosition` 不到它就算失败）；话题群的已读是按话题记的，不同步。已读位置只会往前走，给更小的 `maxPosition` 什么也不改。
- **头像**：群用会话信息里头像的 key，拼成飞书 CDN 上 180px 的 https 地址（`s1-imfile.feishucdn.com/static-resource/v1/<key>~?image_size=180x180…`，会话信息自带的是 http 的 pstatp 地址，不用）；私聊用对方的头像，和自己的会话用自己的（`chats.PullChatChattersRequest` 里的 `avatar.thumbnail`），一小时问一次，每轮未读数最多顺带查 10 个私聊。Lark 站点不知道 CDN，群不带头像。这些地址不需要登录态，由 hub 去取。
- **名字每轮更正**：未读数里带上每个会话现在的名字（群名；私聊按下面的规则），Collie 据此更正存下的名字，所以以前起错的名字（如「私聊（还不知道对方是谁）」）不用等新消息也会改过来。私聊的成员（名字和头像）一小时查一次，每轮最多新查 10 个私聊，其余下一轮再查。
- **私聊的名字**：私聊在飞书里没有会话名。对方发来的消息用对方的名字；你自己发的，用会话成员（`chats.PullChatChattersRequest`）找出对方；只有你一个人的是「和自己」，名字是「你的名字（我）」。都拿不到时显示「私聊」加 id 末 6 位，悬停看完整 id，不再出现「还不知道对方是谁」。已有会话下次收到消息时更新名字。
- **免打扰**：会话在飞书里设了免打扰（会话信息的 `isRemind` 为 false），消息带上 `muted: true`。Collie 的收件箱不为它提醒，未读数照常显示但用灰色。会话信息缓存一小时，所以在飞书里改了免打扰，最多一小时后跟上。
- **收消息**：websocket 推来的每条消息都用 `channel.receive` 交给 Collie，会话开关、谁能唤起和触发词在 Collie 侧判定。系统消息不交；图片和文件（20 MB 以内）下载后作为附件交过去；引用的消息在本次运行见过时带上原文。自己发出的帖子推回来时按 `cid` 认出来丢掉，不会再触发。
- **收件箱详情**：打开会话详情时按消息位置读最近 12 条（话题群 `chatMode == 3` 的会话本身没有消息位置，按 `lastVisibleThreadPosition` / `lastThreadPosition` 调 `thread.PullThreadsByPositionsRequest` 读最近话题及回复），后台缓存 30 秒，收到新推送时刷新。普通群话题里的回复（位置为负）不在会话自己的位置里，按位置读不到，详情把扩展这次运行以来推来的话题回复按时间并进来，所以列表预览是话题回复时，点开也能看到它；运行前的话题回复不在里面。每条消息带发送人的头像：同一窗口里没记住的人用一次 `chats.PullChatChattersByIDsRequest` 查（答复里的用户 id 是整数），记一小时，hub 代取图片。卡片、富文本、日历和附件走同一套结构化显示，系统通知随历史显示。卡片链接可打开，需向飞书提交回调的按钮在 Collie 中显示为不可用。未开处理的会话在 hub 仍只持久化预览。
- **发消息**：`channel.send`。正文和尾标分开（`footer`），排成一条 post（`PutMessageRequest` type 2）。按 ref 只发一次：发之前先记下 ref，答复里拿到消息 id 才算发出；连不上或被拒收是「失败」，其余（超时、断线、答复里没有 id）是「可能已发出」，不再重发。暂时只发文字，带图片或文件的发送直接失败。
- **在做了**：给 Collie 接下的请求加一个「OnIt」（在做了）表情回复，以你本人的身份（网关 `reactions.PutReactionRequest`，cmd 25），只加这个表情，不撤。你本人发的、以触发词开头的消息（触发词由 `channel.settings` 告知）在本地一收到就加，不等 Collie 往返（从收到推送到发出请求不到 1ms）；别人发的由 Collie 判定后用 `channel.working` 通知。每条消息只加一次。本地先加时判定和 Collie 一样严：只认你本人 10 分钟内的新消息、按 Collie 收到的同样文字判断触发词，不认自己发出的帖子；来源策略忽略飞书时一个也不加。日志里记下每次的耗时：收到推送后多久发出请求、飞书多久答复。
- **回复位置**：回复挂在原消息的话题下（原消息本身在话题里时挂到话题根）。话题群不带 `isReplyInThread`，其他会话带上，否则会落回主会话。

## 测试

```sh
# 适配层：需要锁定 commit 的上游 larkx/（LARKX_UPSTREAM 指向它的上一级）和装好依赖的 Python
LARKX_UPSTREAM=/path/to/upstream python -m unittest discover extensions/larkx/test
# Collie 侧完整流程：安装（下载 + 自带文件）、握手、扫码、收消息、发送和尾标
COLLIE_LARKX_UPSTREAM=/path/to/upstream COLLIE_LARKX_DEPS=/path/to/deps go test ./internal/collie -run TestLarkxFake -count=1
```

开发实例里想不登录就看效果：`COLLIE_EXT_PASS_ENV=COLLIE_LARKX_FAKE COLLIE_LARKX_FAKE=1 collie ...`，扫码后收件箱出现两个示例会话。

## 实测

- 2026-11-27：会话里的消息位置从 0 起（`firstChatMessagePosition` 是 -1），只有一条消息的机器人私聊 `lastMessagePosition` 是 0。详情原来从位置 1 起读，这类会话读出来是空的，只显示一行预览；现在从 0 起读。

- 2026-11-27：一个群列表显示的最新消息点开看不到：那条是话题回复，位置 -3，会话主线最后一条比它早约 4 小时，详情只按主线位置读。`thread.PullThreadMessagesByPositionsRequest` 能读话题里的消息，`thread.PullThreadsByPositionsRequest` 和 `messages.PullChatMessagesRequest` 网关返回 400。

- 2026-11-26：Collie 原来自己数的未读和飞书差得多，原因有两个：Collie 只数扩展运行以后推来的消息（话题群还把话题里的回复也算上），而且从来不知道你在飞书里读过。线上 218 个会话里，飞书已读的有 89 个，Collie 认为已读的只有 4 个，两边数一致的只有 5 个。309 个会话问一轮未读数约 2.7 秒（4 批）。

- 2026-11-26：276 个群里由 Meego 建的 54 个（15 个需求、39 个缺陷）描述都是对应的 story/issue 链接，按描述全部认出；其余 222 个群没有一个被认成 Meego 群。

- 2026-11-26，devbox：上游 13 个文件重新下载后 sha256 全部一致；Python 3.9 和 3.12 下适配层测试通过，`pip --require-hashes` 安装依赖约 20 秒；`TestLarkxFake` 通过。

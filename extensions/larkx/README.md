# 飞书（larkx）

以你本人的飞书身份收发消息：扫码登录飞书网页版，你的会话出现在「自动化 → 收件箱」，打开某个会话后，以触发词（默认 `/ai`）开头的消息交给 Collie 或这个会话的专属 agent，回复以你的身份发在原消息的话题下。回复是飞书富文本：Markdown 的段落、引用、行内代码、代码块和链接照样显示，尾标是末尾的一段引用。

## 打开前要知道

- **飞书网页版接口**：底层是 [cv-cat/LarkAgentX](https://github.com/cv-cat/LarkAgentX)，用的是飞书网页版接口，飞书升级后可能失效。用哪个账号由你决定；公司账号先确认公司规定。
- **登录态是完整的网页会话**：只保存在运行它的那台电脑的 `COLLIE_HOME/ext/larkx/secrets.json`（0600），不上传 hub。扩展自己不存消息，`data/` 里只有依赖包和已发送记录（ref 和消息 id，保留 7 天）。
- **跑在哪台**：在「接入」里任选。默认是 Router 所在的电脑。
- **前提**：那台电脑有 `python3`（≥3.9，带 pip），首次启动能访问 raw.githubusercontent.com、pypi.org、files.pythonhosted.org（扩展进程继承 Collie 的代理变量）。

## 这个目录里有什么

- `collie-extension.json`：清单。`source.base` 锁定 LarkAgentX 的 commit `97ad48e`，`source.files` 只列用到的 13 个文件和 sha256，第一次打开时逐个下载并校验。上游代码一行不改，本仓库也不放它（上游没有许可证）。
- `pkg/`：适配层，随 collie 二进制打包，安装时和下载的文件放在同一个包目录（`extensions/README.md` 第 9 节）。
  - `bin/larkx`：找一个 ≥3.9 的 `python3` 启动 `adapter/main.py`。
  - `adapter/main.py`：协议和状态。只用标准库，所以 Collie 立刻能握手；依赖在后台装进 `data/deps-<锁文件 hash>/`（`deps.py`，`pip --require-hashes`）。
  - `adapter/lark.py`：调用上游的扫码登录（`QrLogin`），将推送转成 Collie 消息，经网关（`_gateway_post`、`api`）发送帖子和表情。凭据由 `connector.secrets.*` 存取。
  - `adapter/lark_ws.py`：维护 WebSocket 连接，解码并分派推送。使用上游的地址、心跳、应答（`build_ws_url`、`heartbeat_loop`、`send_ack`）和消息解码（`decode_push_messages`）。
  - `adapter/richtext.py`：Markdown 和飞书富文本互转。
  - `adapter/fake.py`：不联网的假飞书，测试用（`COLLIE_LARKX_FAKE=1`）。
  - `requirements.txt`：上游用到的包，带 hash 锁定（`uv pip compile requirements.in --generate-hashes --universal --python-version 3.9`）。
- `test/test_adapter.py`：适配层的测试。

## 和 Collie 怎么接

- **握手、登录、状态**：`initialize`、`connector.login.step`（返回要编码成二维码的文字，每 2 秒轮询一次；过期自动换新码，最多 3 次）、`connector.status`、`connector.logout`。
- **收推送**：适配层自己读 websocket 的帧（上游的循环只认未压缩的 cmd 6）：压缩过的帧（gzip、zlib）先解开；cmd 6000 只推 sid，按 sid 补拉（`improto.PullPacketsBySidsRequest`）；cmd 14004 是云文档的评论、回复和 @你（`docs.PushDocFeedRequest`），每篇文档算一个会话（`doc:<feed id>`，类型「文档评论」），正文前写明是谁做了什么和文档链接。文档评论暂时只收不回，往这类会话发送会直接失败。读不了的帧计数，每 10 分钟最多记一条日志。
- **折叠的会话**：在飞书里被折叠的会话（会话的 feed 卡片挂在「折叠的会话」盒子下，`parentCardId` 不为 0，网关 `feed.PullFeedCardsByIdsRequest`），消息带上 `folded: true`，收件箱不列出它。和会话信息一样缓存一小时。
- **@自己唤起**（默认开）：你本人发的消息里用飞书的 @ 真的 @ 了自己（AT 元素的 userId 是你，不是打出来的文字），也算请求，交给 Collie 时去掉这个 @。别人 @ 你不算。和触发词一样，一收到就先加「在做了」。
- **群话题各开对话**（默认开）：群里不在话题里的请求开一段新对话（回复会在它下面开出话题），之后这个话题里的请求接着这段对话；私聊照旧是一段对话。关掉后群里的请求都进同一段对话。
- **收件箱显示**：「接入」里飞书扩展有三个开关，默认都开：免打扰的会话不显示、Meego 需求同步群不显示（群名带「[需求同步群]」）、Meego 缺陷同步群不显示（群名带「[缺陷同步群]」）。它们是 Collie 自己的设置（清单里的 `x-hide`），改了不会重启扩展。隐藏只影响列表，你本人发的触发词照常处理。
- **免打扰**：会话在飞书里设了免打扰（会话信息的 `isRemind` 为 false），消息带上 `muted: true`。Collie 的收件箱不为它提醒，未读数照常显示但用灰色。会话信息缓存一小时，所以在飞书里改了免打扰，最多一小时后跟上。
- **收消息**：websocket 推来的每条消息都用 `channel.receive` 交给 Collie，会话开关、谁能唤起和触发词在 Collie 侧判定。系统消息不交；图片和文件（20 MB 以内）下载后作为附件交过去；引用的消息在本次运行见过时带上原文。自己发出的帖子推回来时按 `cid` 认出来丢掉，不会再触发。
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

- 2026-11-26，devbox：上游 13 个文件重新下载后 sha256 全部一致；Python 3.9 和 3.12 下适配层测试通过，`pip --require-hashes` 安装依赖约 20 秒；`TestLarkxFake` 通过。

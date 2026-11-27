# 扩展（`internal/collie/ext*.go`、`inbox.go`，规范在 `extensions/README.md`）

## larkx：上游不改，只调用它现成的部分

LarkAgentX 锁 commit、一行不改，适配层（`extensions/larkx/pkg/`）只调用它的扫码登录、websocket、`_gateway_post`、`wrap_packet` 和 proto。坑：

- 上游 `larkx/__init__.py` 会导入它的 agent 和 SQLAlchemy 存储。适配层不导入这个包本身，而是登记一个只有 `__path__` 的空包（`lark.load_upstream`），再按需导入子模块，依赖就只要 `requirements.txt` 那几个。
- `LarkAuth` 读写 `~/.larkx/credentials.json`。适配层继承它，只覆盖 `load`/`save`，凭据走 `connector.secrets.*`；`LARKX_HOME` 指到扩展的 `data/`，上游别处想写文件也落在那里。
- 上游的 `send_msg` 只发纯文本（`PutMessageRequest` type 4）。富文本是适配层自己拼的 type 2 post，元素结构见 `richtext.build`。
- 自己发出的帖子会被 websocket 推回来。发之前先记下请求的 `cid`，推回来的消息 `cid` 对上就丢掉，不必等发送返回消息 id（推送可能比返回先到）。
- 上游的 websocket 循环只处理未压缩的 cmd 6 帧：连接时声明了 gzip，压缩的推送会被当坏帧丢掉；cmd 6000（只推 sid，机器人卡片常这样）和 cmd 14004（文档评论）也不处理。适配层用 `lark_ws.py` 的 `PushStream._session` 接收推送，借用上游的地址、心跳和应答。
- 第一次启动要装依赖（约 20 秒到 1 分钟）。协议部分只用标准库，先握手，依赖在后台装；这期间来的扫码请求最多等 40 秒（Collie 那边等 50 秒）。

## 下载包不要拉整个仓库

codeload 的 tar 包是整个仓库（几十 MB），devbox 走 Corplink 代理时 5 分钟都下不完。清单改为逐个文件锁定：`source.base`（锁 commit 的 raw 地址）+ `source.files`（路径 → sha256），只列运行需要的文件。算 sha256 时排序要用 `LC_ALL=C sort`，否则中文 locale 下顺序和 Go 的字节序不同。

## 扩展拿不到 Collie 的环境变量

扩展进程只继承白名单（`extEnvKeep`），所以测试或开发要传给它的变量（比如 `COLLIE_LARKX_FAKE=1`）得写进 `COLLIE_EXT_PASS_ENV`。

## 会话的专属 agent 不能落到 hub

`Spawn` 的 `Machine` 留空就是 hub 本机（`fleet.go` 的 `machine("")`），而 hub 不装 agent，于是回「has no codex installed」。Router 的 `collie spawn` 走 API，会填设置里的默认电脑；扩展会话自己起 agent 时要自己填：`convMachine` 读扩展自己的「运行在」设置（扩展的专属设置优先于 `worker_machine`），不推断、不兜底；扩展在 hub 上就回一句说明，不落 hub。启动失败的说明用 progress 发，不带尾标，免得出现「默认 · 默认 · 0秒」。

## 同一个扩展只能跑一份

两个轮询进程共用一个登录态会互相抢消息（微信尤其如此）。换电脑时先发 `$proc.stop` 让旧的停下，再在新电脑启动；node 断开超过 10 分钟（`extLease`），它那边的扩展自己停掉，免得 hub 已经在别处启动了新的。

## 收到消息先回应答，再慢慢处理

`channel.receive` 和 `$proc.*` 都走同一条 stream，hub 侧处理慢会拖住整条连接。会话 agent 的启动（`startConvWorker`）放在 goroutine 里，`incoming` 只做入库和 Ingest。

## 别用 `pkill -f <模式>` 清理开发实例

在 Claude Code 的 Bash 里，`pkill -f` 的模式会匹配到执行它的那个 shell 自己（命令行里就有这个字符串），把当前命令一起杀掉（exit 144）。先 `pgrep` 拿到 pid 再单独 `kill`。

## 回复带的文件不走 JSON 正文

API 的 `decode` 把请求体限在 1 MiB，base64 的文件放在 `/api/super/reply` 里，超过约 750 KB 就成了「invalid JSON body」。`collie reply --file` 改为先把每个文件流式传到 `/api/super/reply-files`，回复里只带 id；hub 仍接受旧 CLI 的内联 `data`。超过 40 MiB 的文件改发签名下载链接（`extensions/README.md` 第 3.7 节）：channel.send 一帧最多 64 MiB，base64 后的大文件装不下。

## 扩展来的请求不是 candidate

没有给来源设规则时，`policyFor` 对未知来源默认 `candidate`（Router 只在规则或记忆要求时才处理），微信绑定人发的「你好」就被标成了 candidate。扩展消息在 Collie 侧已经过了触发规则才成为 Trigger，所以 `prepareConvDeliveryLocked` 把没有规则的 `ext:*` 改成 `respond`；「来源策略」里设了规则的仍按规则。

## 验证码之后接着轮询

微信扫码后可能要求输入手机上显示的数字。提交数字后扩展通常回 `kind:"qr"`（继续等确认），不是 `done`：界面要接着按 `step` 轮询，不能停在「没通过」。

## 飞书会话的危险操作权限

启用「处理这个会话」并选「任何人」后，才显示「允许危险操作」。权限按飞书会话保存，群话题继承。改回「只有我」或关闭处理会重置开关，并撤销尚未投递给 Agent 的外部请求。只关闭危险操作时，等待中的请求按受限模式处理，重新打开也不会恢复它们入队时的旧授权。

飞书适配器给 Collie 的发送者标识用于识别本人，显示名和消息正文只用作请求资料。Router 和专属 Agent 共用 [每次投递的规则](../../internal/collie/lark_guard.go)，受限事件首次唤起 Router 时会把全局记忆初始化留到后续合适的请求。规则约束模型行为，已投递的请求仍按终端投递日志确认。

Collie 代发飞书前对正文、尾标和附件名称做高置信凭证匹配。自动代发命中后在 Collie 主对话里显示会话 ID 和拦截提示。收件箱直接发送时，由当前页面显示拦截提示。Hub 审计日志记录会话、投递与拦截标识、策略版本和匹配类型。扫描涵盖文本，附件内容及 Agent 通过独立工具发送的数据在其范围之外。

## 验证

- `go test ./internal/collie -run 'TestExt|TestLarkxFake'`：测试扩展（测试二进制自己当 `ext-fake`）走完整流程；`TestLarkxFake` 用真实的 larkx 包（上游文件 + 自带适配层）和它的假飞书，要设的变量见 `extensions/larkx/README.md`。
- 界面：开发实例 + `COLLIE_EXT_PASS_ENV=COLLIE_LARKX_FAKE COLLIE_LARKX_FAKE=1`，在「接入」打开飞书扩展，扫码后收件箱出现 2 个示例会话。

# 扩展（`internal/collie/ext*.go`、`inbox.go`，规范在 `extensions/README.md`）

## larkx：上游不改，只调用它现成的部分

LarkAgentX 锁 commit、一行不改，适配层（`extensions/larkx/pkg/`）只调用它的扫码登录、websocket、`_gateway_post`、`wrap_packet` 和 proto。坑：

- 上游 `larkx/__init__.py` 会导入它的 agent 和 SQLAlchemy 存储。适配层不导入这个包本身，而是登记一个只有 `__path__` 的空包（`lark.load_upstream`），再按需导入子模块，依赖就只要 `requirements.txt` 那几个。
- `LarkAuth` 读写 `~/.larkx/credentials.json`。适配层继承它，只覆盖 `load`/`save`，凭据走 `connector.secrets.*`；`LARKX_HOME` 指到扩展的 `data/`，上游别处想写文件也落在那里。
- 上游的 `send_msg` 只发纯文本（`PutMessageRequest` type 4）。富文本是适配层自己拼的 type 2 post，元素结构见 `richtext.build`。
- 自己发出的帖子会被 websocket 推回来。发之前先记下请求的 `cid`，推回来的消息 `cid` 对上就丢掉，不必等发送返回消息 id（推送可能比返回先到）。
- 上游的 websocket 循环只处理未压缩的 cmd 6 帧：连接时声明了 gzip，压缩的推送会被当坏帧丢掉；cmd 6000（只推 sid，机器人卡片常这样）和 cmd 14004（文档评论）也不处理。所以收推送的循环是适配层自己的（`Feishu._session`），只借用上游的地址、心跳和应答。
- 第一次启动要装依赖（约 20 秒到 1 分钟）。协议部分只用标准库，先握手，依赖在后台装；这期间来的扫码请求最多等 40 秒（Collie 那边等 50 秒）。

## 下载包不要拉整个仓库

codeload 的 tar 包是整个仓库（几十 MB），devbox 走 Corplink 代理时 5 分钟都下不完。清单改为逐个文件锁定：`source.base`（锁 commit 的 raw 地址）+ `source.files`（路径 → sha256），只列运行需要的文件。算 sha256 时排序要用 `LC_ALL=C sort`，否则中文 locale 下顺序和 Go 的字节序不同。

## 扩展拿不到 Collie 的环境变量

扩展进程只继承白名单（`extEnvKeep`），所以测试或开发要传给它的变量（比如 `COLLIE_LARKX_FAKE=1`）得写进 `COLLIE_EXT_PASS_ENV`。

## 会话的专属 agent 不能落到 hub

`Spawn` 的 `Machine` 留空就是 hub 本机（`fleet.go` 的 `machine("")`），而 hub 不装 agent，于是回「has no codex installed」。Router 的 `collie spawn` 走 API，会填设置里的默认电脑；扩展会话自己起 agent 时要自己填：`convMachine` 依次取默认电脑、Collie 所在的电脑、扩展所在的电脑，都没有就报错，不落 hub。启动失败的说明用 progress 发，不带尾标，免得出现「默认 · 默认 · 0秒」。

## 同一个扩展只能跑一份

两个轮询进程共用一个登录态会互相抢消息（微信尤其如此）。换电脑时先发 `$proc.stop` 让旧的停下，再在新电脑启动；node 断开超过 10 分钟（`extLease`），它那边的扩展自己停掉，免得 hub 已经在别处启动了新的。

## 收到消息先回应答，再慢慢处理

`channel.receive` 和 `$proc.*` 都走同一条 stream，hub 侧处理慢会拖住整条连接。会话 agent 的启动（`startConvWorker`）放在 goroutine 里，`incoming` 只做入库和 Ingest。

## 别用 `pkill -f <模式>` 清理开发实例

在 Claude Code 的 Bash 里，`pkill -f` 的模式会匹配到执行它的那个 shell 自己（命令行里就有这个字符串），把当前命令一起杀掉（exit 144）。先 `pgrep` 拿到 pid 再单独 `kill`。

## 验证

- `go test ./internal/collie -run 'TestExt|TestLarkxFake'`：测试扩展（测试二进制自己当 `ext-fake`）走完整流程；`TestLarkxFake` 用真实的 larkx 包（上游文件 + 自带适配层）和它的假飞书，要设的变量见 `extensions/larkx/README.md`。
- 界面：开发实例 + `COLLIE_EXT_PASS_ENV=COLLIE_LARKX_FAKE COLLIE_LARKX_FAKE=1`，在「接入」打开飞书扩展，扫码后收件箱出现 2 个示例会话。

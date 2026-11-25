# 扩展（`internal/collie/ext*.go`、`inbox.go`，规范在 `extensions/README.md`）

## larkx 的 `/agent` 是写死的，触发要在 Collie 侧判定

larkx 只把「本人发的 `/agent …` 或真 @ 自己」用 `channel.receive` 交出来，前缀是常量 `AGENT_PREFIX`。要让触发词可改、能放开给别人，又不改 larkx，就得用它的 `connector.changed` + `connector.archive.changes` 拉全部消息，在 Collie 里判定。这样一来，larkx 原来在 `_maybe_agent` 里做的事 Collie 都要自己做：第一次拉取只记游标（历史不触发）、10 分钟前的不触发、自己发出的回复回来时要认出来（按 message id、原文、尾标前缀三道）。archive 里没有 root/parent/@ 信息，所以只能按会话分。

## 下载包不要拉整个仓库

codeload 的 tar 包是整个仓库（persona-extensions 约 40 MB），devbox 走 Corplink 代理时 5 分钟都下不完。清单改为逐个文件锁定：`source.base`（锁 commit 的 raw 地址）+ `source.files`（路径 → sha256），只列运行需要的文件。算 sha256 时排序要用 `LC_ALL=C sort`，否则中文 locale 下顺序和 Go 的字节序不同。

## 扩展拿不到 Collie 的环境变量

扩展进程只继承白名单（`extEnvKeep`），所以测试或开发要传给它的变量（比如 `LARKX_PERSONA_BACKEND=fixture`）得写进 `COLLIE_EXT_PASS_ENV`。

## 同一个扩展只能跑一份

两个轮询进程共用一个登录态会互相抢消息（微信尤其如此）。换电脑时先发 `$proc.stop` 让旧的停下，再在新电脑启动；node 断开超过 10 分钟（`extLease`），它那边的扩展自己停掉，免得 hub 已经在别处启动了新的。

## 收到消息先回应答，再慢慢处理

`channel.receive` 和 `$proc.*` 都走同一条 stream，hub 侧处理慢会拖住整条连接。会话 agent 的启动（`startConvWorker`）放在 goroutine 里，`incoming` 只做入库和 Ingest。

## 别用 `pkill -f <模式>` 清理开发实例

在 Claude Code 的 Bash 里，`pkill -f` 的模式会匹配到执行它的那个 shell 自己（命令行里就有这个字符串），把当前命令一起杀掉（exit 144）。先 `pgrep` 拿到 pid 再单独 `kill`。

## 验证

- `go test ./internal/collie -run 'TestExt|TestLarkxFixture'`：测试扩展（测试二进制自己当 `ext-fake`）走完整流程；`TestLarkxFixture` 用真实 larkx 包和它的 fixture 后端。
- 界面：开发实例 + `COLLIE_EXT_PASS_ENV=LARKX_PERSONA_BACKEND LARKX_PERSONA_BACKEND=fixture`，在「接入」打开飞书扩展，扫码后收件箱出现 4 个示例会话。

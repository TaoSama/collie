# 屏幕模式（`internal/collie/screen_cursor.go`、`web/app.js` 的 `drawScreen`）

## 光标的位置是推断的

herdr 的 `pane.read` 不返回终端光标（0.9.5 和 2026-11-20 的 master 都是这样，内部有 `TerminalCursorState`，但 API 只暴露复制模式的光标）。所以 hub 在每一帧里自己找光标，优先级如下：

1. 可见行里单独一个反色字符：Claude Code 隐藏真光标，自己画一个反色块当光标，位置是准的。
2. 最后一个提示符行（`❯ ` `› ` `> `，带不带框线都算）：已经输入的文字末尾。如果后面跟着暗色占位文字（Codex 空输入框），放在占位文字前面。菜单的选中项（`❯ 1. Yes`）不算提示符行。
3. 最后一个非空行的末尾（shell）。如果行以 `$ % # >` 结尾，快照可能丢了后面的空格，所以补一个空格。

第 2、3 步是猜的：Codex、shell 的真光标不在行尾（按了左方向键、右侧有 RPROMPT）时会偏。herdr 的 API 以后提供光标时，直接用它替换 `markScreenCursor` 的查找。

## 标记方式

列号不在 Go 里算：中文、emoji 的宽度要和 xterm 的宽度表完全一致。hub 在帧文本里光标的位置插入 DECSC（`ESC 7`），并带上 `cursor: true`。前端重画后发 `ESC 8` 并显示光标，列号由 xterm 自己算。没有改动的行不重写，上一次保存的位置仍然有效。

Claude 画的反色块要去掉反色，否则闪烁的块光标和下面的反色块一模一样，看不出在闪。

## 外观

聚焦时显示闪烁的块光标，失焦时显示空心框（`cursorInactiveStyle: "outline"`）。颜色取 `--term-fg`，块里的字取 `--term-bg`，所以所有皮肤都看得见。xterm 在第一次聚焦之前不画光标。

## 能往上翻多少

屏幕模式把 `pane.read` 的 `recent` 整段画成一个和行数一样高的 xterm（`scrollback: 0`），靠 `#screen` 自己的滚动条往上翻；人往上看时新帧先压着不画（`pendingScreen`），回到底部再画并恢复跟随。

- herdr 的 `pane.read` 不管 `lines` 传多大，最多返回 1000 行（0.9.5 实测），所以 `screenLines` 就是 1000。herdr 自己留的 scrollback 有一万行左右，再多也读不到。
- agent 全屏画时在备用屏里，herdr 没有它的 scrollback（`pane.get` 的 `scroll.max_offset_from_bottom` 是 0），页面上只有当前一屏：Claude Code 设了 `tui: fullscreen`；Codex 默认就是（devbox 上手开的 `codex` 是带 `--no-alt-screen` 的 alias，所以是行内，Collie 起的 worker 直接 exec，不带这个参数，就在备用屏）；agy 设了 `altScreenMode: "always"`（默认行内）。这时 hub 在帧里带 `wheel: true`（`paneWheel`），页面滚到头以后，滚轮和手指上下拖动换算成行数，以 SGR 鼠标滚轮报告（`ESC [<64;列;行M` 往上，`65` 往下，每行一个）发给 agent，让它自己滚，下一帧就是滚过的画面（这类帧不压着，页面上本来就没有历史）。左右拖动还是页面自己的（手机横向看宽屏）。
- 页面把 agent 的视图往上滚过，离开屏幕模式（切到对话或日志、换会话）时要把它滚回底部，否则它一直停在旧内容上，读屏看到的是旧输出和「N new messages ↓」（`web/wheel.js`）。等量的向下报告不够：Claude Code 一次收到的一串报告最多滚约 125 行，所以另发 ctrl+End（`ESC [1;5F`，它的回到底部）；Codex 收多少滚多少、不认 ctrl+End；两者到底以后多余的报告都不起作用，所以按「往上的两倍加 200 行」发，再跟一个 ctrl+End（2026-11-26 在 devbox 实测 Claude Code 2.1.302、Codex）。
- 这些报告和 ctrl+End 不会打断忙着的 Claude Code：herdr 的一次 `send_text` 整段写进去，Claude 收到的是完整序列（2026-11-26 在 devbox 实测 Claude Code 2.1.302，跑命令时连发 2.5–12 KB 的回底序列、并发小段滚动、全机 CPU 打满、835k token 的大会话，都没有出现 Interrupted）。会打断的是被拆开、中间隔了约 0.3 秒以上的序列：开头单独的 ESC 被当成 Esc 键。所以页面发给 pane 的输入里只要带 Esc、ctrl+c 或转义序列，处理它的那台（hub 或直连的 node）都会记一行日志（pane、字节数、Esc 个数、哪台设备，不记内容），herdr 只记失败的发送，没有这行就查不出一次没人承认的 Interrupted 是谁发的（`server_terminal.go` 的 `logInterrupting`）。
- 只认 `agentKinds` 里的 agent（claude、codex、agy）且 scrollback 为 0：herdr 的 API 不告诉我们 pane 开没开鼠标上报，shell 收到这些报告会原样打出来。三种 agent 在行内模式下收到都会忽略，不会进输入框（Claude Code 2.1、Codex 0.172、agy 实测），所以行内但历史还不满一屏时误发也没事。别的 agent 没试过，不转发。
- 代价随行数线性涨：一直在滚的满屏 shell，每帧要重画全部行（DOM 渲染器），帧也是整段发、SSE 不压缩。80 列带颜色的输出持续滚动时，从 500 行到 1000 行，每帧从约 63 KB 涨到约 126 KB，浏览器主线程占用约翻倍。没有输出时不发帧，没有影响。

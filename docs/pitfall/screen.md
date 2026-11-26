# 屏幕模式（`internal/collie/screen_cursor.go`、`web/app.js` 的 `drawScreen`）

## 光标的位置是推断的

herdr 的 `pane.read` 不返回终端光标（0.9.5 和 2026-11-20 的 master 都是这样，内部有 `TerminalCursorState`，但 API 只暴露复制模式的光标）。所以 hub 在每一帧里自己找光标，优先级如下：

1. 只在 Claude Code 里：可见行里单独一个反色字符。Claude Code 隐藏真光标，自己画一个反色块当光标，位置是准的。Codex、agy 不画光标，它们画面里单独的反色字是自己的（高亮的键、选中的项），不动。
2. 最后一个提示符行（`❯ ` `› ` `> `，带不带框线都算）：已经输入的文字末尾。如果后面跟着暗色占位文字（Codex 空输入框），放在占位文字前面。菜单的选中项（`❯ 1. Yes`）不算提示符行。
3. 最后一个非空行的末尾（shell）。如果是 shell 且行以 `$ % # >` 结尾，快照可能丢了后面的空格，所以补一个空格；agent 的画面不补。

第 2、3 步是猜的：Codex、shell 的真光标不在行尾（按了左方向键、右侧有 RPROMPT）时会偏。herdr 的 API 以后提供光标时，直接用它替换 `markScreenCursor` 的查找。

## 标记方式

列号不在 Go 里算：中文、emoji 的宽度要和 xterm 的宽度表完全一致。hub 在帧文本里光标的位置插入 DECSC（`ESC 7`），并带上 `cursor: true`。前端重画后发 `ESC 8` 并显示光标，列号由 xterm 自己算。没有改动的行不重写，上一次保存的位置仍然有效。

Claude 画的反色块要去掉反色，否则闪烁的块光标和下面的反色块一模一样，看不出在闪。除了这个标记和 Claude 的反色块，agent 的画面原样画：文字、排版都和真实终端一样，屏幕模式只改颜色（见「外观」）。

## 外观

聚焦时显示闪烁的块光标，失焦时显示空心框（`cursorInactiveStyle: "outline"`）。颜色取 `--term-fg`，块里的字取 `--term-bg`，所以所有皮肤都看得见。xterm 在第一次聚焦之前不画光标。

agent 按 herdr 报给它的终端（深色）配色：Codex 的提示行、输入框是深灰底、文字用默认前景色。浅色皮肤下前景是深色，就成了深底深字，空输入框的暗色占位文字「Ask Codex to do anything」看不见，只剩光标块里的「A」，像删不掉的残留。所以页面只调颜色：浅色皮肤下把深色背景（相对亮度 < 0.2）换成皮肤背景往前景色偏一点的浅色，原来越亮偏得越多，深浅次序不变（`web/termcolor.js`）；xterm 设 `minimumContrastRatio: 4.5`，文字颜色只在看不清时挪到看得清。xterm 的主题背景要填真实的 `--term-bg`，填透明它会当成黑色来算对比度。

## 能往上翻多少

屏幕模式把 `pane.read` 的 `recent` 整段画成一个和行数一样高的 xterm（`scrollback: 0`），靠 `#screen` 自己的滚动条往上翻；人往上看时新帧先压着不画（`pendingScreen`），回到底部再画并恢复跟随。

- herdr 的 `pane.read` 不管 `lines` 传多大，最多返回 1000 行（0.9.5 实测），所以 `screenLines` 就是 1000。herdr 自己留的 scrollback 有一万行左右，再多也读不到。
- agent 全屏画时在备用屏里，herdr 没有它的 scrollback（`pane.get` 的 `scroll.max_offset_from_bottom` 是 0），页面上只有当前一屏：Claude Code 设了 `tui: fullscreen`；Codex 默认就是（devbox 上手开的 `codex` 是带 `--no-alt-screen` 的 alias，所以是行内，Collie 起的 worker 直接 exec，不带这个参数，就在备用屏）；agy 设了 `altScreenMode: "always"`（默认行内）。这时 hub 在帧里带 `wheel: true`（`paneWheel`），页面滚到头以后，滚轮和手指上下拖动换算成行数，以 SGR 鼠标滚轮报告（`ESC [<64;列;行M` 往上，`65` 往下，每行一个）发给 agent，让它自己滚，下一帧就是滚过的画面（这类帧不压着，页面上本来就没有历史）。左右拖动还是页面自己的（手机横向看宽屏）。
- 页面把 agent 的视图往上滚过，离开屏幕模式（切到对话或日志、换会话）时要把它滚回底部，否则它一直停在旧内容上，读屏看到的是旧输出和「N new messages ↓」（Codex 是「↓ Back to bottom · esc」）。回底只发 ctrl+End（`ESC [1;5F`）加 4 个 PageDown（`ESC [6~`），共 22 字节：Claude Code 2.1.302 和 Codex 0.172 按 ctrl+End 从最顶上直接回底，agy 不认 ctrl+End、一个 PageDown 就回底，到底以后多余的键不起作用（2026-11-26 devbox、macmini 实测）。不要用 Esc 回底：Codex 提示「esc latest」，但它忙时 Esc 就是打断。
- 发给 agent 的东西不能出现「后面还没到的 ESC」：Claude Code、Codex 读到一个 ESC、后面暂时没有字节，就当成 Esc 键，打断当前这一轮，剩下的半截序列作为文字进输入框（macmini 实测 Codex：先发 `ESC`、0.3 秒后发 `[<65;60;20M`，输入框里留下 `[<65;60;20M`，显示 Conversation interrupted）。终端把一次长写入分块传（macOS 的 `MAX_INPUT` 是 1024 字节，Linux 4 KB），块边界正好落在 ESC 后面就会出事。所以：滚轮报告每次最多发 8 个（约 100 字节），一次只有一个请求在路上，快速滑动多出来的行数攒到下一次，不拼成长串（`web/wheel.js` 的 `sender`）；回底改成 22 字节；hub 的 `/api/panes/{id}/text` 把带 ESC 的长文本切成不超过 256 字节、每块都在 ESC 前切开的多次写入（`escChunks`），旧页面和别的调用方也被兜住。页面发来的输入只要带 Esc、ctrl+c 或转义序列，处理它的那台都会记一行日志（pane、字节数、Esc 个数、设备，不记内容），herdr 只记失败的发送。以前的回底发「往上行数两倍加 200」个滚轮报告，2.5–12 KB，单次整段写进去时在 devbox、macmini 都没复现打断，但它是最可能被切开的东西，已去掉。
- herdr 0.9.5 的 `send_keys` 没有 `home`、`end`、`delete`、`pageup`、`pagedown` 这些键名（报 `invalid_key`），屏幕模式里按这几个键原样以文字发出整个序列。
- 只认 `agentKinds` 里的 agent（claude、codex、agy）且 scrollback 为 0：herdr 的 API 不告诉我们 pane 开没开鼠标上报，shell 收到这些报告会原样打出来。三种 agent 在行内模式下收到都会忽略，不会进输入框（Claude Code 2.1、Codex 0.172、agy 实测），所以行内但历史还不满一屏时误发也没事。别的 agent 没试过，不转发。
- 代价随行数线性涨：一直在滚的满屏 shell，每帧要重画全部行（DOM 渲染器），帧也是整段发、SSE 不压缩。80 列带颜色的输出持续滚动时，从 500 行到 1000 行，每帧从约 63 KB 涨到约 126 KB，浏览器主线程占用约翻倍。没有输出时不发帧，没有影响。

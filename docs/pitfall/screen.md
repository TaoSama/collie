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

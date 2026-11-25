# 对话框识别（确认卡片、`collie answer`）

## Codex 执行中提的问题是折叠的

Codex（0.172 实测）在一轮执行中调用 `request_user_input` 时，问题不弹菜单，而是折叠在输入框上方：

```
• Queued follow-up inputs
  ? 1 question
    ⇧← to answer
```

herdr 此时报 `blocked`，屏幕上却没有选项，所以卡片只剩 ↑ ↓ Esc 确认，`collie answer` 也认不出对话框。按 `shift+left` 才会展开成带光标的编号菜单（`› 1. Red` … `enter submit ⌃] skip ⇧→ main prompt`），之后就是普通菜单。

- 读对话框一律用 `Herdr.ReadAsk`（`internal/collie/fleet.go`），不要直接 `Read(…, "detection", …)`：它认出折叠的问题就按一次 `shift+left`，等展开后再返回屏幕。同一窗格 3 秒内只按一次。
- 展开后别按 `shift+right` 想把它收回去：实测一次，问题按当时选中的选项（Red）作答了。

样本：`internal/collie/testdata/codex-question-*.txt`，测试 `TestCodexQueuedQuestion`。

## 确认卡片不能每次重绘都重新入场

`.ask` 带入场动画 `rise`，元素一被重新插入就会再播一次。和 Collie 对话里的线程每来一条消息、事件或 worker 状态就整块 `innerHTML` 重建，会话页里光标移动（↑↓）也会重画卡片，所以卡片原先每次都会闪一下（实测线上 60 秒重建 7 次）。

- 重绘前已在屏幕上的卡片（会话页的 `#live`，对话页按 `data-pane`），重绘后立刻加 `still` 类关掉动画，只有第一次出现才入场。
- 对话页第一次读对话框还没返回时不画「等你确认」占位行，免得它马上被卡片替换；读一次没认出对话框、而 agent 仍是 `blocked` 时先保留卡片，连续两次才撤。
- 会话页的 live 流里 `ReadAsk` 失败（经 node 的慢链路）时沿用上一份对话框，不把卡片撤掉再加回来。

## 弹窗标题里没有断点的长词会撑破弹窗

`.sheet` 弹窗是 flex 布局，flex 子项默认 `min-width: auto`，最小宽度等于内容里最长的那个词。会话名、文件路径这类标题如果是一整串没有空格的 URL，`h3` 的最小宽度会远超弹窗，把整个 form 撑宽，弹窗的 `overflow: hidden` 再把超出的部分裁掉，标题开头、正文和按钮都会被切掉。只有标题很长且中间没有断点时才出现，所以看起来是偶发的。

- `.sheet-head h3` 和 `.ask-text` 设了 `min-width: 0; overflow-wrap: anywhere`，长词会在弹窗里折行。往弹窗里放会话名、路径等外部文本时，确认所在元素也能这样折行。

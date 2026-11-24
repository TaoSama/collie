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

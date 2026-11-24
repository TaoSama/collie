# 通知（`decisions.go`、`lark.go`、`web/sw.js`）

## 链接到决策本身

需要人处理的通知链接到决策，不链接到首页：

- `<hub>/#/d/<entry id>`：Collie 记下的决策，包括 worker 的「等你确认」事件（`Entry.Ask` 记录 pane、选项、指纹和结果）和带 `--opt` / `--check` 的消息。
- `<hub>/#/d/pane/<pane id>`：任意会话的对话框，用于浏览器通知。远端 pane 的 ID 带 `@machine`。

页面的 `openDecision`（`web/app.js`）先画出底下的视图，再请求 `GET /api/super/decision`。对话框还开着时打开会话并闪一下卡片；`--opt` 和 `--check` 跳到那条消息；已经处理过的显示「已处理：选了 X」。链接的 hub 地址取 `-url`，没有设置时取最近打开的页面。未登录时 `#/d/…` 留在地址栏里，登录后照常打开。

## 不泄露、不重复

- 通知正文只写「任务名 需要你选：A / B」，`--opt` 再加上 Collie 问题的第一行。对话框的屏幕内容（命令、路径）只在登录后的页面里显示。每个选项截到 48 字节。
- 同一个对话框（`askSig`：标题、问题和选项）还没人答又出现，或在 10 分钟内消失后又出现，算同一个决策，不再加一行，也不再推送。
- 通知每 20 秒合并发送一次。决策在等待期间被 Collie 或你答掉，就不再发送。
- 浏览器通知用决策作 `tag`，同一设备上会替换而不是叠加。打开页面时已在等待的 `--opt` 不补发，发过的 ID 记在 localStorage 的 `collie.notified` 里。

## 通知上的按钮

- 只有选项全部放得下（`Notification.maxActions`，Chrome 是 2）时才带按钮。iOS 和 Safari 没有按钮，点通知只能打开页面。
- 按钮由 `sw.js` 直接请求 `POST /api/super/decide`。对话框必须和通知里的指纹一致，选项按标签全文匹配，所以对话框换了就不会答错，接口返回 409，页面会打开。
- 页面卡片是按键作答的，hub 看不到选了哪个。所以页面点完会再调用 `POST /api/super/decided` 告诉 hub。这次调用只负责写上选项，对话框消失后才算已答，发送失败不会误判为已答。

## 测试

- Playwright 默认的 headless shell 里 `Notification.permission` 始终是 `denied`，要用 `chromium.launch({ channel: "chromium" })`。
- `clients.openWindow` 和 `WindowClient.focus` 只能在真实的通知点击中调用，headless 无法点击系统通知。测试时直接调用 `sw.js` 的 `notificationClicked(data, action)` 验证按钮作答和已打开页面收到消息；冷启动则在同一个 context 里新开窗口打开链接，看是否从 SW 缓存和启动快照起来。

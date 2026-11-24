# 通知（`decisions.go`、`lark.go`、`push.go`、`web/sw.js`）

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

## Web Push（`webpush.go`、`push.go`、`pushapi.go`）

手机把切到后台、锁屏的 PWA 页面挂起，页面脚本弹不了通知，所以需要你处理的通知由 hub 用 Web Push 发出：

- hub 第一次启动时生成 VAPID 密钥（P-256），以 PKCS #8 格式存在 `herd.db` 的 `kv.vapid` 里。**不要换密钥**，换了以后所有订阅都会失效，要等各个浏览器重新打开页面、重新订阅才会恢复（页面发现 `applicationServerKey` 和 hub 的不一样时会自己重订）。
- 只要「浏览器通知」开着并且已经授权，页面每次启动都会订阅一次，用 `POST /api/push/subscribe` 告诉 hub，订阅存进 `push_subs` 表并记在当前浏览器设备名下。关掉开关或退出登录时，页面先调用 `/api/push/unsubscribe` 再退订。在「设备」里删掉这个浏览器，它名下的订阅一起删除。推送服务返回 404 或 410 时，hub 也会删掉对应订阅。
- 推送的内容：worker 的对话框（`Entry.Ask`，`tag` 为 pane）、`--opt` / `--check`（`tag` 为 `d:<id>`），以及其他会话的对话框（tick 里 `push.watch` 看到状态变成 `blocked`，`tag` 为 pane，链接 `#/d/pane/…`）。这些推送和飞书、Webhook 共用 `sendNotifications`：每 20 秒合并发送一次，发送前已处理的不发，你在看页面时不发。正文由 `notice.line()` 生成，不含屏幕内容。agent「完成了」不推送，仍由后台还活着的页面自己弹。
- 其他会话的对话框，hub 等 `pushSettle`（3 秒）后再读屏取选项，Collie 自己会答的信任提示因此不会推出去。同一个对话框（`askSig`）1 分钟内又出现算闪烁，不再推送。在通知按钮或页面卡片上答过以后（`push.answered`），同一个对话框再出现算新的决策。hub 刚启动时就已经停在对话框上的会话不推送。
- 不重复弹：页面在前台时每 30 秒调用一次 `POST /api/super/seen {page}`，切到后台或关掉时用 `keepalive` 发 `{page, away:true}`，最后一个在前台的页面离开后，hub 立刻恢复推送，不用再等 75 秒。sw.js 收到推送时，如果有可见且有焦点的窗口就不弹（Chrome 只在没有页面在前台时，才要求每次推送都显示通知）。有订阅时，页面不再自己弹对话框和 `--opt` 的通知，别的 hub 的会话除外，那些 hub 不会推送给这个页面。
- 链接只带 `hash`，由 sw.js 拼上自己的 origin，所以从 node 本机界面（`127.0.0.1:17788`）订阅的，点开也回到本机界面。
- 设置 → 通知里的「后台推送」一行显示订阅状态，「发一条试试」会调用 `POST /api/push/test`，只推送给这个浏览器。
- 推送要经过浏览器厂商的推送服务：Chrome 走 FCM（`fcm.googleapis.com`），手机必须能连上 Google 才收得到。国内安卓手机没有 Google 服务、或者没开代理，订阅能成功，推送却一直收不到；系统设置里的「后台耗电 / 自启动」限制也会拦住推送。先用「发一条试试」区分是 hub 的问题还是手机的问题：接口返回 200 就说明推送服务已经收下了。

## 通知上的按钮

- 只有选项全部放得下（`Notification.maxActions`，Chrome 是 2）时才带按钮。iOS 和 Safari 没有按钮，点通知只能打开页面。
- 按钮由 `sw.js` 直接请求 `POST /api/super/decide`。对话框必须和通知里的指纹一致，选项按标签全文匹配，所以对话框换了就不会答错，接口返回 409，页面会打开。
- 页面卡片是按键作答的，hub 看不到选了哪个。所以页面点完会再调用 `POST /api/super/decided` 告诉 hub。这次调用只负责写上选项，对话框消失后才算已答，发送失败不会误判为已答。

## 测试

- 推送的加密有 RFC 8291 附录 A 的逐字节向量测试（`webpush_test.go`）。`push_test.go` 里的假推送服务会用浏览器的密钥解密，核对正文、tag、链接和按钮。sw.js 的 push 处理用 `node --test scripts/test/*.test.mjs` 测试。
- 端到端测试要用带推送服务的浏览器：Playwright 的 `channel: "chrome"`（正式版 Google Chrome，headless 也行）能拿到 FCM 订阅，ego lite 和 Playwright 自带的 Chromium 都拿不到。先用 `context.grantPermissions(["notifications"])` 授权，关掉所有页面后通过 `ctx.serviceWorkers()[0]` 读 `registration.getNotifications()`。「浏览器通知」默认开着，测试时点一下开关反而会把它关掉。
- `collie reply` 的 `--opt` / `--check` 要放在正文前面，放在后面会被当成正文。

- Playwright 默认的 headless shell 里 `Notification.permission` 始终是 `denied`，要用 `chromium.launch({ channel: "chromium" })`。
- `clients.openWindow` 和 `WindowClient.focus` 只能在真实的通知点击中调用，headless 无法点击系统通知。测试时直接调用 `sw.js` 的 `notificationClicked(data, action)` 验证按钮作答和已打开页面收到消息；冷启动则在同一个 context 里新开窗口打开链接，看是否从 SW 缓存和启动快照起来。

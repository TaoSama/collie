# 界面响应（`web/app.js`、`internal/collie/pane_wake.go`、`direct.go`、`node_watch.go`）

## 界面不要等网络

浏览器到 hub、hub 到 node 各隔一段公网。2026-11 实测 macpro 到香港 hub 往返约 345ms，抖动时一次请求要 1–2 秒。发一条消息要过浏览器→hub→node→herdr，herdr 的 `agent.prompt` 自己还要约 300ms。所以用户操作的即时反馈不能等接口返回：

- 输入框（`bindComposer`）提交时立刻清空，消息按顺序排队发出。发送失败时把原文按发送顺序放回框里，附件也放回去。
- 发出的消息先以 `.msg.user.sending` 显示在会话末尾（`trackSend`），等 agent 自己的记录里出现同样的文字再换掉。文字一直对不上时，发出 20 秒后去掉。
- 终端按键排队发送：前一个请求还没回来时，后面的文字和按键合进下一个请求（按键一次最多 64 个）。

node 收到输入后用 `paneWake` 叫醒这个 pane 的流，马上再读一次。之后 150ms 内每 10ms 读一次，3 秒内加快轮询：屏幕 50ms，对话 100ms。这样回显和消息不用等下一个轮询周期。每个流单独订阅：一个流关掉，不会让同一个 pane 上的其他流收不到叫醒。

## 启动不等网络

2026-11 实测（Pixel 7 仿真、Fast 4G 带宽、CPU 4 倍降速，到香港 hub 真实往返约 330ms），已安装的 app 冷启动要 4.4 秒才出侧栏：HTML 首字节约 1 秒（DNS、TCP、TLS），8 个静态文件逐个 304 重验证约 0.65 秒，`/api/session`、`super`、`state`、`hubs` 串行约 2.7 秒。nginx 只压缩 `text/html`，130KB 的 `/api/super` 在冷 TCP 窗口下要 1.6 秒。hub 本机处理这些请求都不到 10ms，时间全花在路上。现在的做法：

- **service worker**（`web/sw.js`）。缓存启动要的文件，页面和这些文件不再走网络。文件清单是 `server.go` 的 `shellFiles`，hub 发 `sw.js` 时写入清单和版本号（`webBuild`，涵盖清单里所有文件和 `sw.js`）。发新版后浏览器发现 `sw.js` 变了，装上新的缓存并删掉旧的。所以发版后第一次启动仍是旧代码，新的 service worker 接管后才换新。页面发现自己是旧代码时（`controllerchange`，或接口返回的 `X-Collie-Web` 变了），如果打开或回到前台后还没点过、没按过键，且输入框都是空的，就直接刷新；否则弹出「有新版本」。`/api/` 和 `/direct/` 永远不经过它。往 `index.html` 加启动必需的文件时同步改 `shellFiles`，`compress_test.go` 会检查两边一致。
- **启动快照**（`app.js` 的 `saveSnap` / `loadSnap`）。页面切到后台、关闭时，以及启动 5 秒后，把侧栏、Collie 对话最近 80 条、外部会话和历史存到 localStorage 的 `collie.snap`。启动时先画快照，再并行请求 `session`、`super`、`state`、`hubs`，回来后补齐。其他 hub 的令牌不存，它们的会话等 `/api/hubs` 回来后才能点开。
- **退出和失效**。点退出先清快照和 `collie-*` 缓存再刷新。任何 401（`showLogin`）都会清快照。设备被移除后，下次启动会先画出上次的快照，hub 拒绝后才回到登录页，这段时间约一次往返。
- **压缩**（`compress.go`）。hub 自己 gzip 文本响应（JSON、JS、CSS、HTML、SVG），不依赖 nginx 配置。事件流（SSE）不压缩，保证每条事件立刻送到。压缩后 ETag 变成弱标签 `W/"…"`，`static` 比较时去掉 `W/`，304 照常生效。

效果（同样仿真，本机 hub 加 330ms 延迟，中位数）：手机冷启动 2512ms → 162ms，热启动 2456ms → 140ms；桌面 2442ms → 64ms。剩下的时间主要是解析和执行 `app.js`、排版整页。测量脚本做法：Playwright 持久化配置模拟冷/热启动，MutationObserver 记下 `#app` 显示且 `#side-groups` 有内容的时刻。

## 终端直连本机 node

浏览器和 node 在同一台电脑上时，终端（屏幕流、文字和按键）不经过 hub，直接走 node 本机界面端口下的 `/direct/`（`http://127.0.0.1:17788/direct/…`），回显从两次公网往返降到本机。

- hub 的 `POST /api/direct` 只在浏览器的来源 IP 等于这台 node 连 hub 的 IP 时，才向 node 要一个 12 小时的令牌。hub 在 nginx 后面，IP 取 `X-Forwarded-For`（`clientIP`，只在请求来自本机反代时才信这个头）。
- node 只认自己发的令牌，只开放终端那几条路由，跨源只接受 hub 页面和本机界面这两个来源。
- 请求不带自定义头、正文用 `text/plain`，省掉 CORS 预检。Chrome 允许 https 页面访问 `http://127.0.0.1`，可能会先问一次是否允许访问本机。
- 直连失败（node 重启后令牌作废、浏览器拒绝、不是这台电脑）就自动改走 hub，过一会儿再试。

## 侧栏状态用 node 推来的列表

hub 以前每次组装状态都要依次问每台电脑的 workspace、pane、agent 三个列表，还要逐个读空闲 agent 的屏幕，一次 1.5 秒以上；Collie 对话的状态（`superState`）每次也要问一遍。现在 node 每 150ms 看一次本机 herdr，列表变了才用 `GET /api/watch` 推给 hub，10 秒没变也重发一次表示还活着。

- 只推 hub 显示用到的字段。pane 的 `revision` 和焦点一直在变，不去掉的话每次都算变化。
- 只有展示用的调用（`state`、`superState`，带 `withCachedLists`）用推来的列表。要据此动手的代码（起 Router、找 agent 名字）仍然现问，免得用到旧列表。
- 推来的列表超过 15 秒没更新就不用了。老 node 没有 `/api/watch`，hub 照旧去问它。

## 打字时别让整页重新排版

- `toLocaleTimeString` / `toLocaleString` 带 options 时，每次调用都会新建一个格式器。Collie 对话重绘时每条消息都要调几次，曾经占了重绘耗时的一半。格式器用模块级的 `Intl.DateTimeFormat`，只建一次。
- 自动长高的输入框（`grow`）先设 `height: auto` 再读 `scrollHeight`，会让整页重新排版，对话很长时每个字要几十毫秒。只在文字溢出或变短时才重新量，打字还在原来的行数里时什么也不改。

测量方法：用 Playwright 打开线上页面，用 Event Timing 和 CDP `Performance.getMetrics` 看每次按键的处理时间、排版次数和长任务。

## 读不到 pane 不等于 pane 关了

浏览器收到流的 `gone` 就显示「这个 pane 已经关闭。」并停掉这个流，不会自己恢复，所以只有 herdr 明确回 `*_not_found` 时才能发。连不上 herdr（它在重启或热升级，socket 暂时不在）、5 秒超时、node 正在重连（`machine_offline`），都只是这一轮没读到，下一轮再试。

- 对话流（`streamChat`）先问 `agent.get`：`agent_not_found` 说明这个 pane 没有 agent（shell，或 agent 还没起来），再读一下屏幕，`pane_not_found` 才是关了。`agent.get` 报别的错时，整轮跳过，不要当成 shell 发 `meta`，否则顶栏会闪成终端。
- 屏幕和日志流（`streamPane`）同样只认 `not_found`。两处共用 `notFound`（`server.go`）。

## 聊天里的图片

2026-11 实测线上（桌面和 Pixel 7 仿真，从缩略图出现在页面到图片显示），你发的附件 0.8–2.5 秒，Collie 回复里引用的截图 4–14 秒，再次打开一样慢。原因有三：

- **逐台串行找文件**。路径不带电脑，页面按 hub、各台电脑的顺序一台一台问，每次 404 都是一次往返（0.35–0.7 秒），文件在列表后面的电脑上就要等五六次。现在 `findFile` 同时问所有电脑，按原来的顺序取第一个有的，其余的请求取消。
- **原图过网**。1 MB 的照片从 devbox 经 hub 要 5.8 秒。缩略图请求 `?as=thumb`，文件所在的电脑把 PNG/JPEG 缩到 1320×780 以内（`thumb.go`，不透明的转 JPEG），450 KB 的截图变成约 100 KB，生成约 60ms。GIF、WebP、SVG 和小于 96 KB 的文件原样发。灯箱先显示缩略图，原图到了再换上。老版本 node 不认 `as=thumb` 时页面改要原图。
- **没有缓存**。响应是 `no-store`，图片只存在内存里，刷新就全部重取。现在缩略图存进 Cache Storage 的 `collie-img`（最多 400 张），再次打开直接从本地显示。Collie 自己命名的文件（`~/.collie/images/`、`~/.collie/uploads/`）不会变，不再请求；其他路径先显示存的，再后台取一次，内容变了才换。收到 401 或退出时删掉这个缓存。

你发的图片：上传要经 hub 到派活的那台电脑（多半是 devbox），450 KB 的截图从 macpro 传要约 5 秒。消息先用浏览器里的原文件显示（`withLocalFiles`，用只有本页认得的 `/collie-local/…` 路径），上传完成后换成真实路径，这时已经用本地文件填进缩略图和缓存（`seedImage`），刷新和重开都不再请求。同一台电脑上的其他浏览器、换一台设备看，才要经 hub 取。

图片就在浏览器所在的电脑上时，走终端同样的本机直连：`/direct/api/file` 只开放 `as=image` 和 `as=thumb`（不能下载，也不能打开 HTML）。页面第一次要这台电脑的图片时还不知道能不能直连，先经 hub，同时在后台探测。`~/.collie/uploads/`、`~/.collie/images/` 下的文件名只在一台电脑上有，哪台先答就用哪台，不再等排在前面的电脑回 404。其他路径按顺序取，能直连的本机排在消息来源之后、hub 之前，也不用等别的电脑。Chrome 访问 `127.0.0.1` 要用户允许访问本地网络。看图片只在这个权限已经是 `granted` 时才去探测直连（权限是打开本机终端时问的），绝不为看图弹出提示：v2026.1125.31 看到本机图片就探测，弹出的提示让 ego 等自动化浏览器把页面交还给人，看起来像所有会话卡在骨架屏。没允许、查不到这个权限或不是 Chrome（Safari 拦截 https 页面访问 http）时，照旧经 hub。

首次看到别人的图片受网络限制：浏览器到 hub 一次往返约 330ms，加上 hub 到那台电脑的往返和传输，到不了 20ms。

# 界面响应（`web/app.js`、`internal/collie/pane_wake.go`）

## 界面不要等网络

浏览器到 hub、hub 到 node 各隔一段公网。2026-11 实测 macpro 到香港 hub 往返约 345ms，抖动时一次请求要 1–2 秒。发一条消息要过浏览器→hub→node→herdr，herdr 的 `agent.prompt` 自己还要约 300ms。所以用户操作的即时反馈不能等接口返回：

- 输入框（`bindComposer`）提交时立刻清空，消息按顺序排队发出。发送失败时把原文按发送顺序放回框里，附件也放回去。
- 发出的消息先以 `.msg.user.sending` 显示在会话末尾（`trackSend`），等 agent 自己的记录里出现同样的文字再换掉。文字一直对不上时，发出 20 秒后去掉。
- 终端按键排队发送：前一个请求还没回来时，后面的文字和按键合进下一个请求（按键一次最多 64 个）。

node 收到输入后用 `paneWake` 叫醒这个 pane 的流，马上再读一次，之后 3 秒内加快轮询：屏幕 50ms，对话 100ms。这样回显和消息不用等下一个轮询周期。

## 打字时别让整页重新排版

- `toLocaleTimeString` / `toLocaleString` 带 options 时，每次调用都会新建一个格式器。Collie 对话重绘时每条消息都要调几次，曾经占了重绘耗时的一半。格式器用模块级的 `Intl.DateTimeFormat`，只建一次。
- 自动长高的输入框（`grow`）先设 `height: auto` 再读 `scrollHeight`，会让整页重新排版，对话很长时每个字要几十毫秒。只在文字溢出或变短时才重新量，打字还在原来的行数里时什么也不改。

测量方法：用 Playwright 打开线上页面，用 Event Timing 和 CDP `Performance.getMetrics` 看每次按键的处理时间、排版次数和长任务。

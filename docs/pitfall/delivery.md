# 往会话里送消息（`internal/collie/prompt.go`、`super.go` 的 `tick` / `deliver`）

Router 的事件、`collie send`、spawn / resume 的首条任务、页面上给 agent 发消息、新建会话的首个任务、切换模型的命令，都走 `Herdr.Prompt`。外部会话不经过 pane，有自己的确认（`peerTaken`，`external_send.go`）。

## herdr 回 ok 不等于送进去了

`agent.prompt` 成功只说明按键发出去了。`Prompt` 发完后回读 agent：

- `state_change_seq` 或状态变了，或者消息开头的几个字（`promptProbe`）在输入框上方多出现了一次，就算送到。
- 「开头几个字」取消息里第一行发送前屏幕上还没有的：每批事件都以 `[collie] 1 new event` 开头，拿它当探针，旧消息会被当成新的。
- 发送时连接断了（node 掉线）也先回读：看到送进去了就算成功，什么都没看到才交回调用方下一轮重投。
- 文字还停在输入框里（输入框里有它，或者 `[Pasted text` / `[Pasted Content`），只补按 Enter，不重发文字。
- 什么迹象都没有，而且 agent 一直空闲、屏幕上没有对话框，才重发一次。agent 在忙时排队的消息画在哪里不确定，看不到也不重发，按 herdr 的结果算。
- 几轮后仍没确认，返回 `prompt_unconfirmed`。`deliver` 收到它就停下（再投可能重复提交）：事件标成 `failed`（收件箱显示「未送达」），Collie 对话里出一条错误事件。页面直接发的消息照常把原文放回输入框并提示。

## tick 拿到的列表可能比 worker 旧

tick 要先问遍所有电脑的 agent 列表，慢的电脑要好几秒。worker 在这期间起来（改名、变成 idle），列表里还没有它，旧代码就判它「已退出」，同一轮再把它的首条任务改投给 Router，下一轮又「回来了」。2026-11-25 两次 `spawn --resume` 的任务就是这样进了 Router 的会话（Router 记录里的批量事件带着 worker 的任务）。

- 只有列表开始前就已经不是 `starting` 的那个 worker 对象，才可能因为不在列表里被判退出（`settled`）。

## 一台电脑卡住不能拖住所有投递

tick 原来只有一个 20 秒的总超时。一台电脑半断线、请求不回时，列表这一步每轮都把 20 秒用完，后面的投递拿着过期的 ctx 立刻失败，人的消息一直送不出去（2026-11-25 15:31 那条等了 4 分钟，直到 hub 重启）。现在：

- 列表单独限时 `tickListWait`，没答话的电脑这一轮跳过；每次投递有自己的 `deliverWait`。
- 没答话（或还没连上，比如 hub 刚重启）的电脑上的 worker 和 Router，`awayGrace` 内不判退出、不改投、不重起 Router，事件留着，电脑一答话下一轮就投。超过 `awayGrace` 才按退出处理。
- 拨 herdr socket 超时不等于 herdr 没在跑：只有 socket 不存在或被拒才去起 `herdr server`（`dialHerdr`）。

复现办法：开一个独立的 herdr 会话（`herdr --session NAME server`），用一个延迟回包的 unix socket 代理接到它，再用 `collie node --socket 代理` 把它作为另一台电脑接到开发实例上。

# 工作台

工作台是侧栏底部田字格图标打开的页面（`#/wb`，模块页是 `#/wb/<id>`）。人在这里钉自己关心的网页和 Collie 的页面，并为每个模块决定给不给 Agent。Collie 不在这里存业务数据。

工作台是内置扩展（`extensions/workbench/`，`runtime.feature`），在「接入」里打开才生效，默认关闭，升级也不自动打开。关闭时：侧栏入口隐藏，`#/wb` 回首页；`/api/wb*` 和 `/api/super/wb/*` 回 409，`collie wb` 跟着报错；ROUTER.md 里不写 `collie wb` 那段；keep 快照停；做梦不看工作台；其他扩展声明的模块也不显示。数据（模块、布局、token、快照、记忆笔记）一律不删，重新打开原样回来。开关一变，hub 推一次 super 事件（`workbench` 字段）并重写 ROUTER.md。

## 模块

- **网页**（`kind: web`）：
  - `url` 是人看的页面。`embed` 为 `iframe` 时嵌在工作台里，为 `tab` 时显示一张卡片，点开新标签。
  - 添加时，hub 用 `POST /api/wb/probe` 读页面每一跳的 `X-Frame-Options` 和 CSP `frame-ancestors`，再预选 `embed`。探测结果只作预选：浏览器登录后看到的响应头可能不同，人可以手动改。
  - `feed` 是给 Agent 读的数据地址，返回 markdown、文本或 JSON。只读 token 存在 hub 的 `COLLIE_HOME/wb/secrets.json`（0600），接口只回 `has_token`。
- **快捷入口**（`kind: link`）：打开 Collie 自己的页面。目标只能取 `wbLinks`（`internal/collie/workbench.go`）里的值，前端的 `LINKS`（`web/workbench.js`）和它一一对应，`scripts/test/workbench.test.mjs` 会核对两边是否一致。
- 布局（分组和模块的顺序）存在 `herd.db` 的 kv 表，键是 `workbench`，所有设备共用一份。

## 给 Agent

| 档 | 含义 |
| --- | --- |
| `off`（默认） | `collie wb ls` 里不出现 |
| `read` | Collie 和它派出的 worker 用 `collie wb read ID` 现取 |
| `keep` | 在 `read` 的基础上，每天存一份快照到 `router/wb/<id>/<日期>.md`。内容和上一份相同时不写新文件，保留 30 天。做梦时提炼进 `memory/notes/wb-<id>.md`，敏感模块写进 `wb-private-<id>.md` |

- Agent 用的两个接口在 `/api/super/wb/ls` 和 `/api/super/wb/read/<id>`：已配对的电脑的 token 只能访问 `/api/super/`，而 Collie 和 worker 可能跑在任何一台电脑上。改布局、token 和探测的接口在 `/api/wb`，电脑的 token 访问不到。
- **谁能读**由 hub 判断。它按 CLI 传来的 `HERDR_PANE_ID` 认出调用者，规则如下：
  - 不在 herdr 里运行（拿着 token 的人或脚本）：可以读。
  - Collie 本身，以及它派出的 worker：可以读。
  - 其他 Agent：拒绝。
- **敏感模块**不允许在「和外部的会话」里读取。外部会话指：
  - worker 所在或订阅的线程以 `ext:` 开头；
  - Collie 当前这一轮投递里有 `ext:` 事件（往前多算 30 分钟，宁可多拒）。
- 这条规则只防误用，不是沙箱：Agent 有 shell，仍然可以直接读快照文件。
- 页面上的「发给 Collie」按钮由人主动点击，可以读任何有 `feed` 的模块，用的是 `?human=1`。`human=1` 和不带 pane 都是调用方自报的：拿到 hub token 的进程都能这样调用，所以这条规则同样只防误用。
- 快照在 tick 开头检查，每分钟最多检查一次，不依赖 herdr。失败后每小时重试，原因显示在卡片上。内容没变时不写文件，也就不会因此触发做梦。`router/wb/` 跟着 router 文件夹同步到 Collie 运行的电脑（`router_remote.go`），做梦时读的就是那里。

## 安全

- iframe 设置了 `sandbox`，但不带 `allow-top-navigation`，被嵌的网页不能把 Collie 页面跳走。`allow-same-origin` 指的是被嵌网页自己的源，它读不到 Collie 的 token。
- **不要把第三方站点反代到 Collie 自己的域名下**：同源之后，那个站点的 JS 能读走 Collie 的 token。确实需要反代时，给每个站点一个独立的子域。
- 和 Collie 同属一个站点的网页（例如 `a.example.com` 与 Collie 所在的 `c.example.com`）嵌入后，`SameSite=Strict` 的 cookie 照常发送，登录态能保持。前提是对方的 `frame-ancestors` 允许 Collie 的源。

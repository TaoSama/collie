# 手机界面（`web/app.css`、`web/app.js`）

## 什么算手机

单栏布局的条件是 `(max-width: 760px), (max-height: 500px) and (pointer: coarse)`：横着拿的手机宽 800 多，但只有 390 高，放两栏时对话区只剩一百来像素，键盘一弹就是 0。`narrow()` 和 `app.css` 里所有单栏规则用同一个查询，改一处要改全部。终端字号（`fitTerm`）只看宽度，不在此列。

横屏还要避开左右的刘海：`.side`、`.view`、底部弹层、看图和文件面板都加了 `env(safe-area-inset-left/right)`。

## 键盘

- Android 按 `interactive-widget=resizes-content` 缩小整个视口；iOS 不认它，只缩 `visualViewport`，`innerHeight` 和 `dvh` 都不变。所以高度一律用 `setVH` 维护的 `--vh` / `--vtop`，底部弹层用 `margin-bottom: calc(100dvh - var(--vh) - var(--vtop))` 抬到键盘上面。
- 「键盘是否打开」只能在 iOS 上判断（`innerHeight - visualViewport.height > 120`，排除双指缩放），结果写成 `<html class="kb-open">`，用来去掉被键盘挡住的底部安全区留白。
- 手机上要给对话腾地方，用 `:has(textarea:focus)` 收起运行中 agent 的条带（横屏再收起标题栏），不要用高度媒体查询：iOS 键盘不改变视口高度。
- 输入框低于 16px 时 iOS 聚焦会放大页面。全局规则用了 `:where()`，任何自带 `font` 简写的输入框（登录、记忆编辑）都会把它盖掉，要在 `(pointer: coarse)` 里单独补上。

## 停在底部

对话停在最新一条时，下面的东西变高（输入框长高、待确认托盘、附件）会把最新消息顶到输入框下面。`ResizeObserver` 盯着 `.viewer`、`#log`、`#screen`，高度变小且之前在底部就贴回底部。一帧里可能连着缩好几次，滚动事件还没来得及更新 `_atBottom`，所以同时按旧高度再判断一次。

## 长按

侧栏长按 500ms 弹菜单。手指抬起时 Chromium 会补发 `mousedown` 和 `click`，`mousedown` 会把刚弹出的菜单关掉，所以菜单弹出后要在 `touchend` 上 `preventDefault()`。行上还要 `user-select: none`，否则长按会选中文字。无头 Chromium 不发 `contextmenu`，真机 Android 可能会发，两条路都要能用。

## 返回

PWA 没有浏览器的返回按钮，靠系统返回手势走 `history`。从通知或链接直接打开某个会话时，历史只有一条，返回会直接退出 app。所以手机冷启动到深链时，先在下面垫一条 `#/`。

盖在页面上的面板（回复串抽屉、文件预览）也要在历史里占一步，否则返回手势会把下面那一页也带走。打开时 `layerStep` 用 `pushState` 压一条同 hash 的历史，`history.state` 里的 `run` / `file` 记着这一步开着哪些面板（只记还开着的）。返回时 `route()` 看到 hash 没变，就按 `history.state` 开关面板（`syncRun`、`syncFile`）：回到打开那一步之前就关掉，前进到那一步就重新打开。手动关面板（返回按钮、×、Esc）时，如果当前这一步就是它打开的那一步，走 `history.back()` 收回这一步，不留空步；如果不是（比如桌面上抽屉和文件都开着，关的是下面那个），就直接关掉，并记下这一步（`runShut` / `fileShut`），之后再退回来也不重新打开。

子页顶栏回到父页的入口（例如工作台模块左上角的「‹ 工作台」）不能直接按 `<a href="#/wb">` 新压一条历史，否则回到父页后再点返回或系统返回手势会退回刚才的模块，形成循环。`app.js` 在 `navTrail` 和 `history.state.colliePrev` 里记录每一步的上一条地址，`goWbHome()` 在上一条就是 `#/wb` 时走 `history.back()` 弹出当前页，否则用 `replaceHash("#/wb")` 替换当前步。

## 测量

演示站（`sh scripts/demo.sh`，`python3 -m http.server -d docs/demo`）就是真实前端，可以用 Playwright 的手机模拟（`isMobile`、`hasTouch`、iOS/Android UA）和 CDP `Emulation.setSafeAreaInsetsOverride` 模拟刘海。量的是点击区域（主要控件不小于 44px，视觉小的圆按钮用 `::after` 扩大热区）、小于 12px 的字、超出视口的元素和横向滚动。iOS 键盘只能把 `--vh` 改小来模拟，Playwright 缩小视口相当于 Android 的行为。

# 设计系统

界面的颜色、圆角、阴影和字体都是 CSS 变量，值只写在两处：`web/app.css` 开头的 `:root` 和 `:root[data-theme="dark"]`（默认皮肤），以及 `web/skins.css`（其他皮肤）。改界面前先看这两处有哪些变量，组件规则里不写颜色字面值。

## 外观 = 皮肤 × 主题

- **主题**（浅色、深色、跟随系统）和**皮肤**（默认、Codex、Linear、Kami）互不相关，任意组合都要好看。
- `<html>` 上的 `data-theme` 总是 `light` 或 `dark`，跟随系统时也由脚本解析好再写上；`data-skin` 在默认皮肤时不设。两者在 `web/index.html` 的内联脚本里于首次绘制前设好，之后由 `applyLook`（`web/app.js`）维护，所以切换和刷新都没有闪烁。
- 选择按设备存在 localStorage（`collie.theme`、`collie.skin`），和其他偏好一样。
- 浏览器和 PWA 的状态栏颜色（`theme-color`）取当前的 `--bg`。

## 变量怎么用

| 变量 | 用途 |
| --- | --- |
| `--bg` / `--bg-2` / `--bg-3` | 主区、侧栏和次级面、悬停和按下 |
| `--line` / `--line-2` | 分隔线、较强的边框（输入框、按钮） |
| `--fg` / `--fg-2` / `--fg-3` | 正文、次要文字、弱化文字和图标 |
| `--accent` / `--on-accent` | 主按钮、开关、选中的选项 |
| `--attn` / `--attn-soft` / `--on-attn` | 需要人处理：待确认的会话、决策卡片、警告按钮 |
| `--ok` / `--danger` / `--pin` | 在线和成功、危险操作、置顶 |
| `--term-*` / `--code-*` / `--hl-*` | 终端、代码块、语法高亮 |
| `--shadow-sm` / `--shadow` / `--shadow-pop` / `--shadow-lg` | 选中块、卡片和输入框、浮动按钮、弹层 |
| `--scrim` / `--mark-mix` | 弹层背后的遮罩、搜索命中的底色深浅 |
| `--sans` / `--mono` | 界面字体、等宽字体（终端也用它）。只列系统里可能有的字体名，不打包字体文件 |
| `--round` | 圆角倍数。组件写 `calc(8px * var(--round))`，胶囊（`999px`）和圆形（`50%`）不乘 |
| `--btn-radius` / `--seg-radius` | 按钮、分段选择器的圆角 |
| `--user-bg` / `--user-fg` / `--user-radius` | 自己发的消息气泡 |
| `--composer-line` / `--composer-focus` / `--composer-shadow` | 输入框的边框、聚焦边框和阴影 |
| `--sel-bg` / `--sel-ring` | 侧栏里打开着的那一项 |

## 加一个皮肤

1. 在 `web/skins.css` 加三块：`:root[data-skin="名字"]` 放和明暗无关的值（`--round`、字体、组件形状），`[data-theme="light"]` 和 `[data-theme="dark"]` 两块各放一整套颜色。颜色只写在明暗块里，否则会漏到另一个主题。
2. 加一个 `.skin-swatch.名字`，在 `web/app.js` 的 `skins` 列表里登记。
3. 对比度：`--fg-2` 在 `--bg` 和 `--bg-2` 上至少 4.5:1，`--fg-3` 至少 3:1，`--on-accent` 在 `--accent` 上至少 4.5:1，语法色在 `--code-bg` 上至少 4:1。
4. 用演示站（`sh scripts/demo.sh`）把每个皮肤的浅色、深色在桌面和手机宽度下都看一遍：侧栏、对话、决策卡片、屏幕、自动化、设备、偏好设置。

## 皮肤的取向

- **默认**：中性灰白，黑白强调色。
- **Codex**：安静的冷灰，主按钮是墨色胶囊，输入框浮在页面上，自己的消息是浅灰气泡，彩色只留给状态。
- **Linear**：冷调石墨加一个靛紫强调色，圆角小、信息密。
- **Kami**：暖纸色加墨蓝，对比柔和、圆角大，适合长时间读对话。

为什么选 Linear 和 Kami 见 `DECISIONS.md`。

---
name: collie-web-verify
description: |
  在线上 Collie 页面做浏览器验收：在 hub 上生成一次性配对码登录，用自动化浏览器实测并截图，测完只删自己配对出来的设备，关掉测试建的会话、终端和 herdr workspace。
  用户说“线上验收”“打开线上页面看看”“截图确认”“在浏览器里实测”“部署后看下页面”，或改完界面、部署完需要在真实线上环境确认效果时，都用这个技能，即使只要一张截图。
  本地开发实例（`COLLIE_HOME=.collie-dev`）或 `docs/demo/` 的检查不需要它。
---

# Collie 线上页面验收

线上是人每天在用的环境，验收要做到两点：结论有截图为证，测完不留痕迹，也不碰人的东西。

主机、域名、1Password 条目写在仓库根目录的 `CLAUDE.local.md`（不提交），沿用 `collie-deploy` 技能的变量 `URL`、`HUB_ITEM`、`HUB_ADDR`。命令用 bash 跑。

## 1. 连上 hub

按 `collie-deploy` 第 2 步「SSH」建立到 hub 的连接（`op read` 读条目、私钥写临时文件、重试建 ControlMaster），定义好 `hub` 和 `api` 两个函数。只做验收不需要切换 hub 前的那些检查。

## 2. 配对登录

先记下现有的浏览器设备和当前时间，之后靠它们认出自己新加的那台：

```bash
T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)
api /api/devices | jq -r '.devices[] | [.id, .kind, .name, .created_at] | @tsv' > /tmp/collie-verify-devices.before
CODE=$(hub '~/.local/bin/collie pair' | grep -o 'pair=[A-Za-z0-9]*' | head -1 | cut -d= -f2)
echo "$URL/?pair=$CODE"
```

- 配对码 10 分钟内有效，只能用一次。输出里的地址可能是 IP，只取码，页面一律用 `$URL` 的域名打开。
- **浏览器用自动化环境，不用人自己的 Chrome**。按 `web:browser-workflows` 技能选后端（Linux 上是容器里的 Chrome + Playwright），开一个独立的浏览器上下文。在人的浏览器里配对会改掉它的登录 cookie，也分不清哪台设备是测出来的。
- 打开 `$URL/?pair=$CODE`，页面进入主界面、URL 里的 `pair=` 消失才算登录成功。

登录后立刻认出自己的设备：

```bash
api /api/devices | jq -r '.devices[] | [.id, .kind, .name, .created_at] | @tsv' > /tmp/collie-verify-devices.after
awk -F'\t' 'NR == FNR { seen[$1]; next } !($1 in seen)' /tmp/collie-verify-devices.before /tmp/collie-verify-devices.after
```

- 应当**恰好多出一台** `browser` 设备，`created_at` 晚于 `T0`（`created_at` 带 hub 的时区，比较时换算一下）。把它的 `id` 记为 `DEV_ID`。
- 多出不止一台或一台也没有，说明有人同时在配对，停下来，不要猜。

## 3. 实测与截图

- 每个要验的点都截图，存到 `/tmp/collie-verify-<时间>/`，文件名写清是哪一项。手机布局用窄视口（宽度不超过 760px）另截一组。
- 只看结论需要的页面。能用只读接口确认的（版本、静态文件、`/api/state`）先用接口，浏览器只验接口看不出来的显示和交互。

**测试数据**：

- 要触发确认卡片、通知或决策时，正文开头写明「测试，无需人点」，例如 `collie reply --check "测试，无需人点：验收确认卡片显示"`。人看到就知道可以忽略。
- 测试建的会话、终端、herdr workspace，名字带上 `verify-` 前缀，边建边记下 ID 和所在电脑。
- 不点人的会话里的按钮，不往人的会话里发消息，不改设置。

## 4. 收尾

**关掉测试数据**：逐个关掉第 3 步记下的会话和终端，herdr workspace 到它所在的电脑上用 `herdr workspace list` 核对名字后 `herdr workspace close <id>`。确认卡片按完或撤掉，不留在人的待办里。

**只删自己的设备**：

```bash
api /api/devices | jq --arg id "$DEV_ID" '.devices[] | select(.id == $id) | {id, name, created_at}'
api "/api/devices/$DEV_ID/delete" POST   # 期望 {"ok":true}
```

- 删之前再核对一次：`id` 等于 `DEV_ID`，`created_at` 晚于 `T0`，两项都对上才删。
- **不能按名字删**。自动化浏览器和人的浏览器可能同名（比如都叫「Mac · Chrome」），按名字删曾经误删过人正在用的设备。
- 删完再列一遍设备，和 `/tmp/collie-verify-devices.before` 一致才算干净。

**清理本机**：关掉浏览器上下文，删掉 `/tmp/collie-verify-devices.*` 和私钥临时文件，关掉 SSH 主连接。截图保留到报告发出。

报告写：验了哪几项、每项结论和对应截图路径、删掉的 `DEV_ID`、关掉的测试会话和 workspace。

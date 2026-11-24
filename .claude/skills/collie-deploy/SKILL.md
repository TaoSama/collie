---
name: collie-deploy
description: |
  把已推送到 origin/main 的 Collie 部署上线：构建并发布 Release，原子替换 hub 二进制后单次重启，再经 hub 逐台升级全部 node，最后核对线上版本、静态文件和 Router。
  用户说“部署”“上线”“发版”“升级 hub / node”“把 main 推到线上”“让所有电脑跑最新版”，或改动合并后需要让线上生效时，都用这个技能，即使只提到其中一步（只升 hub、只升某台 node、只做线上核对）。
  纯文档、skill 或 docs/ 站点的改动不需要部署，不用触发。
---

# Collie 部署上线（hub + 全部 node）

本仓库是公开的。主机、域名、1Password 条目、node 清单这些本机和账号相关的值写在仓库根目录的 `CLAUDE.local.md`（不提交），下文用变量指代，开始前先读它：

| 变量 | 含义 | 从哪取 |
| --- | --- | --- |
| `URL` | hub 的公网地址 | `CLAUDE.local.md`「打开界面」 |
| `HUB_ITEM` | hub 的 SSH 主机、端口、私钥所在的 1Password 条目 | `CLAUDE.local.md`「部署」 |
| `HUB_ADDR` | hub 本机监听地址（`-addr` 参数） | `CLAUDE.local.md`「部署」，或 hub 上 `systemctl --user cat collie-hub` |
| node 清单、各台的代理 | 哪些电脑要升级，哪台下载要走代理 | `CLAUDE.local.md`「部署」 |

不要把 IP、端口、私钥、令牌写进仓库里的任何文件，也不要贴进聊天。

## 0. 前提

- **只部署 `origin/main` 上已推送的提交**。`git fetch origin && TARGET=$(git rev-parse origin/main)`，本地未推送、未合并的改动不上线。
- **同一时间只有一个 agent 部署**。多人同时改代码时，由一个 agent 在大家都推完后统一部署一次。并发部署会反复重启 hub。开始前用 `collie list` 看有没有别的部署 worker 在跑，有就停下来报告。
- 只改了 `docs/`、`.claude/`、`*.md` 的提交不需要部署。

## 1. 构建与发布 Release

node 的 `/api/update` 从 GitHub Release 下载二进制（`internal/collie/updates.go` 的 `selfUpdate`），所以必须先发布 Release，node 才能升级。

```sh
WT=.claude/worktrees/deploy-$TARGET
git worktree add --detach "$WT" "$TARGET" && cd "$WT"
go build ./... && go vet ./... && go test ./... -count=1 && node --check web/app.js

# 版本号 vYYYY.MMDD.N：N 取当天已有标签的下一个
git tag -l "v$(date +%Y.%m%d).*"
V=v$(date +%Y.%m%d).1   # 按上一行结果递增

sh scripts/release.sh "$V" --dry
go version -m dist/collie-linux-amd64 | grep -E 'vcs\.(revision|modified)'
```

- `vcs.revision` 必须等于 `$TARGET`，`vcs.modified` 必须是 `false`，有一项不符就停下。不要在主工作区构建，那里可能有别人未提交的改动。
- 确认后正式发布：`cp dist/checksums.txt /tmp/checksums.dry && sh scripts/release.sh "$V"`。脚本会重新构建、打标签、推送标签并建 Release（需要 `gh` 已登录）。重新构建的产物应和 dry 的一致：`diff /tmp/checksums.dry dist/checksums.txt`。

## 2. 切换 hub

**SSH**：按 `CLAUDE.local.md` 用 `op` 读出 `HUB_ITEM` 的主机、端口和私钥（`op item get <条目> --vault <库> --format json` 看字段名）。私钥只写到 `mktemp` 出来的临时文件并 `chmod 600`，用 `trap 'rm -f "$KEY"' EXIT` 保证用完即删。主机和端口放进当前 shell 的变量 `HOST`、`PORT`，下文用两个辅助函数：

```sh
hub() { ssh -i "$KEY" -p "$PORT" "root@$HOST" "$@"; }
# api <路径> [方法]：在 hub 本机带主令牌调用 API
api() { hub "curl -s -H \"Authorization: Bearer \$(cat ~/.collie/token)\" ${2:+-X $2} 'http://$HUB_ADDR$1'"; }
```

**切换前**：

```sh
hub 'systemctl --user show collie-hub -p MainPID; ~/.local/bin/collie version; uname -m'
api /api/super | jq '{queued, queued_router, router: .router | {status, pane_id, machine, handover, dreaming}}'
```

- 记下 MainPID、当前版本、Router 的 `pane_id` 和 `machine`，最后核对要用。
- 等到 `queued_router == 0`、`handover` 和 `dreaming` 都不为 true、Router 不是 `working` 时再切换。这时切换不会打断 Router 正在处理的事情。
- 如果 MainPID 和你开始时看到的不一样，说明有人刚部署过。先看线上版本，已经是 `$V` 或更新就不要再部署，只做第 4 步的核对。

**替换并重启**，按 `uname -m` 选 `collie-linux-amd64` 或 `collie-linux-arm64`：

```sh
scp -i "$KEY" -P "$PORT" dist/collie-linux-amd64 docs/install.sh "root@$HOST:/tmp/"
hub 'COLLIE_BINARY=/tmp/collie-linux-amd64 COLLIE_NO_HERDR=1 COLLIE_NO_OPEN=1 sh /tmp/install.sh \
  && ~/.local/bin/collie version && systemctl --user restart collie-hub \
  && rm -f /tmp/collie-linux-amd64 /tmp/install.sh'
```

- 不带模式运行 `install.sh` 只安装二进制：先写到 `collie.new`，再 `mv` 到原位，替换是原子的，正在运行的 hub 不受影响。不要用 `install.sh hub`，它会重写 unit，然后先 `enable --now` 再 `restart`，hub 会重启两次。
- 由 systemd 用户 unit 只重启一次。之后轮询 `/api/state` 直到返回 `ok: true`，`journalctl --user -u collie-hub -n 50` 里不应有报错。
- 重启期间所有 pane 会短暂显示 exited，随后恢复，这是正常现象。

## 3. 逐台升级 node

hub 启动时会查一次最新 Release，先确认它已经知道 `$V`：

```sh
api /api/version     # latest 应为 $V，outdated 列出待升级的电脑
api /api/state | jq '.machines[] | {id, name, version, online, local}'
```

对每台 `local == false` 的电脑**一台一台**升级，等前一台确认后再升下一台。Router 所在的电脑（第 2 步记下的 `machine`）放到最后：

```sh
api '/api/update?machine=<id>' POST   # 期望 {"ok":true,"from":…,"version":"$V"}
```

- 升级后进程原地 exec 成新版本，herdr 和里面的 agent 继续运行。轮询 `/api/state`，直到这台 `online == true` 且 `version == $V`。
- 返回 400（源码编译的 `dev` 版本）或 502（连不上 GitHub）：到那台电脑上用 `CLAUDE.local.md` 里的安装命令升级（两个 `COLLIE_NO_*` 环境变量必须带上，需要代理的那台加上代理）。
- 旧版本自更新后可能留下僵尸子进程（`docs/pitfall/child-processes.md`），因为旧版本没有 `reapInherited`。在那台电脑上看 `ps -o pid,ppid,stat,comm -A | awk '$3 ~ /Z/'`，父进程是 collie 的话就优雅重启 node：macOS 用 `launchctl kickstart -k gui/$(id -u)/com.collie.node`，Linux 用 `systemctl --user restart collie-node`，没有 systemd 的机器重跑安装命令。
- devbox 掉线后重连有退避，最长约 5 分钟会自己恢复。先等，不要反复重启。

## 4. 线上核对

全部满足才算部署完成：

1. **静态文件和目标提交一致**：
   ```sh
   sum() { shasum -a 256 | cut -d' ' -f1; }
   for f in app.js app.css skins.css; do
     [ "$(curl -s "$URL/$f" | sum)" = "$(git show "$TARGET:web/$f" | sum)" ] && echo "ok $f" || echo "MISMATCH $f"
   done
   [ "$(curl -s "$URL/" | sum)" = "$(git show "$TARGET:web/index.html" | sum)" ] && echo "ok index.html" || echo "MISMATCH index.html"
   ```
   `index.html` 要用 `/` 取，请求 `/index.html` 会被重定向。`sw.js` 由服务端生成，不参与比较。
2. **认证接口正常**：`api /api/state | jq .ok` 为 `true`。
3. **全部 node 在线且版本相同**：`.machines[]` 都是 `online: true`、`version: "$V"`（hub 自己是 `local: true`，看 `/api/version` 的 `version`），`/api/version` 的 `outdated` 为空。
4. **Router 没换**：`/api/super` 的 `router.pane_id` 和第 2 步记下的一样，`alive: true`。

核对只用只读接口。不要在生产环境新建测试会话、测试 worker 或测试设备。如果为截图用配对码登录过，按 `CLAUDE.local.md` 删掉产生的临时浏览器设备。

## 5. 收尾

- `git worktree remove "$WT"`，删掉 `/tmp/checksums.dry` 和私钥临时文件。
- 报告内容：`$V`、`$TARGET`、hub 切换前后的 PID、每台 node 的结果，以及第 4 步四项核对的输出。

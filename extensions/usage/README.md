# 订阅用量（usage）

内置扩展，默认开启（可在「自动化 → 接入 → 订阅用量」关闭）。开启状态下，Collie 会向 hub 与所有在线 node 采集各台电脑上本机登录的 Agent 订阅额度，并在会话页顶部标题栏与展开抽屉中聚合展示。

## 两阶段去重读取与 Token 安全

1. **阶段一 · 纯本地盘点（零上游网络请求）**：每台在线电脑通过 `GET /api/sub-usage/inventory` 仅读取本机凭证元信息，上报非敏感的账号哈希、订阅档位提示、本地计算的 `access_token` 过期时间以及今日 Token/缓存统计。
2. **Hub 去重与选机**：Hub 按 `(provider, account_hash)` 去重，每个唯一账号只选 **1 台** `access_token` 有效且未处于退避期的机器（优先非 hub 节点、粘性沿用上次成功机器、否则选 `token_expires_at` 最晚者）。
3. **阶段二 · 定向单机读取**：仅向选中的那 1 台机器发起 `GET /api/sub-usage/probe`，拿到用量后映射回所有登录了该账号的机器卡片展示。遇 401/网络故障自动切换同账号的另一台有效机器；429 直接对该账号指数退避（5m → 10m → 20m → 30m）并保留最近成功缓存。
4. **零刷新、零写回**：读取用量绝不调用任何 OAuth 刷新接口，绝不修改本地凭证文件或 Keychain。若所有机器上的 `access_token` 都已过期，界面展示「等待本机 Agent 刷新」，待原生 Agent 自行刷新后自动恢复。

## 支持的订阅

| Agent | 本地凭证读取位置 | 只读查询接口 | 展示内容 |
| --- | --- | --- | --- |
| Claude Code | `~/.claude/.credentials.json`（或 `$CLAUDE_CONFIG_DIR`）、macOS Keychain `Claude Code-credentials`，以及 `~/.claude.json` 元信息 | `GET https://api.anthropic.com/api/oauth/usage` | 订阅档位（Pro / Max 5x / Max 20x / Team / Enterprise）、5H / 7D / 模型窗口剩余比例与重置时间 |
| Codex | `~/.codex/auth.json`（或 `$CODEX_HOME`）、macOS Keychain `Codex Auth`，离线降级读取最近会话 `rate_limits` | `GET https://chatgpt.com/backend-api/wham/usage` | 订阅档位（Plus / ProLite 5x / Pro 20x / Team）、主/次窗口剩余比例与重置时间、额外余额 |
| Antigravity | `~/.gemini/antigravity-cli/antigravity-oauth-token`、`~/.gemini/oauth_creds.json` | `POST /v1internal:loadCodeAssist` 与 `/v1internal:retrieveUserQuotaSummary`（回退 `/v1internal:fetchAvailableModels`） | 订阅档位（Free / Pro / G1 Ultra Lite / G1 Ultra）、Gemini 5H / 7D 与 Partner 5H / 7D 剩余比例与重置时间 |

## 配置项

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `percent_mode` | `remaining` | `remaining`（剩余比例）/ `used`（已用比例） |
| `show_tokens` | `true` | 是否在标题栏次行和抽屉中汇总各电脑今日 Token 与缓存命中率 |
| `dedupe_accounts` | `true` | 同一订阅账号在多台电脑登录时先去重再单机读取，避免重复请求 |
| `refresh_s` | `300` | 缓存刷新间隔（60–600 秒，默认 5 分钟；手动刷新最小冷却 30 秒） |
| `proxy_url` | `""` | 可选。读取订阅用量时的 HTTP/SOCKS5 代理（如 `http://127.0.0.1:7890` 或 `socks5h://127.0.0.1:1080`），支持逗号分隔按机器指定（如 `host1=socks5h://127.0.0.1:1080`，或 `host1=direct` 直连）；留空保持各机默认环境或系统代理 |

## 测试

```sh
go test -race -count=1 ./internal/collie -run 'TestSubUsage'
```

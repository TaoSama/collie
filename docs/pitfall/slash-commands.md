# 输入框 `/` 补全（`internal/collie/commands*.go`、`web/app.js` 的 slash commands 一节）

菜单由会话所在电脑的 node 扫描生成：`GET /api/commands?kind=&cwd=`，hub 按 `?machine=` 转发。结果按 agent 类型和目录缓存 10 秒，过期后先返回旧结果，同时在后台重扫；第一次请求会当场扫描，本机实测 Claude 约 55ms，Codex 约 170ms，主要花在插件缓存上。前端打开会话时预取，60 秒内的列表直接从内存显示，输入 `/` 到弹出约 10ms。

## 各 CLI 从哪里加载（2026-11 核对）

- **Claude Code 2.1.306**。用 `claude -p hi --output-format stream-json --verbose` 的 init 事件核对过，技能和命令名单与扫描结果一致。
  - `~/.claude/skills/<dir>/SKILL.md`：名字取 frontmatter 的 `name`，没有就用目录名。
  - `skills/` 下带 `.claude-plugin/plugin.json` 的文件夹会作为插件 `<name>@skills-dir` 加载。它的 `skills/` 和 `commands/` 以 manifest 的 `name` 加前缀，比如 `bundle-build` 里的技能叫 `build:doc-writer`。
  - `commands/**/*.md`：子目录用 `:` 连接。
  - 项目目录：从启动目录逐级向上找到仓库根，每一级都看 `.claude/skills|commands`。
  - 已安装插件：读 `plugins/installed_plugins.json`，再按 settings 的 `enabledPlugins` 过滤。directory 类型的 marketplace 可能没有缓存副本，这时到 `known_marketplaces.json` 的 `installLocation` 下读取。
  - frontmatter 写了 `user-invocable: false` 的不进菜单。
  - `CLAUDE_CONFIG_DIR` 用的是 node 进程自己的环境变量。通过 alias 换了配置目录的会话，菜单仍按默认目录生成。
- **Codex 0.172**。
  - 技能用 `$name` 调用，不进 Codex 自己的 `/` 菜单。Collie 在 `/` 和 `$` 下都列出技能，插入时写成 `$name`。
  - 扫描根目录：
    - 仓库内各级的 `.agents/skills` 和 `.codex/skills`；
    - `$CODEX_HOME/skills` 及其中的 `.system`；
    - `~/.agents/skills`、`/etc/codex/skills`；
    - `config.toml` 中 `[plugins."x@m"] enabled = true` 的插件，读 `plugins/cache/m/x/<版本>`。
  - 每个根目录最多递归 6 层。技能名的前缀取最近一层插件 manifest 的 `name`，manifest 可以是 `.codex-plugin`、`.claude-plugin` 或 `.cursor-plugin`。
  - 描述优先用 `agents/openai.yaml` 的 `interface.short_description`，其次 `metadata.short-description`，最后 `description`。
  - 插件缓存里可能有装到一半、只剩空 `.codex-plugin/` 的新版本目录，所以选「有 manifest 的最新版本」，不能只看修改时间。
  - 0.172 的二进制里已经没有 `~/.codex/prompts` 的加载逻辑（`prompts:` 字符串不存在），所以不再列出 prompts。
  - `[[skills.config]] enabled = false` 关掉的技能还会出现在菜单里，因为没有解析 TOML。
- **Antigravity CLI 1.5**。
  - 技能目录：仓库各级的 `.agents/skills` 和 `.agent/skills`，以及 `~/.gemini/config/skills`、`~/.gemini/antigravity-cli/skills`、`~/.gemini/skills`。
  - 旧 workflows：`.agents/workflows`、`~/.gemini/config/global_workflows|workflows`。
  - 插件：`~/.gemini/config/plugins/<p>/skills`，名字是 `p:skill`。
  - `disable-slash-command: true` 的不进菜单。
  - CLI 是否还加载 workflows，以及 `builtin/skills` 里哪些会进菜单，都没有核实；后者没有列出。

内置命令表在 `commands_builtin.go`，来源写在文件头部。CLI 升级后对照来源更新这张表。其他电脑上的 CLI 版本可能不同，菜单会多几项或少几项。

## 哪些会话列内置命令

CLI 的内置命令只有直接输入到它的终端才会执行，所以只在 herdr 会话的输入框里列出。主会话把消息交给 Collie，菜单按「派活」选中的电脑和 Agent 列技能，不带目录。外部会话的消息经收件箱或 app-server 送达，同样只列技能。

// Collie demo mode: the real web app, driven by made-up data in the browser.
// It replaces fetch("/api/…") and EventSource before app.js loads, so every
// screen works without a hub. Nothing leaves the page.
(() => {
  "use strict";
  window.__collieDemo = true;
  window.__collieDemoHtmlUrl = (p) => (/bench-pagination\.html$/i.test(p || "") ? "bench-pagination.html" : "");
  const now = Date.now();
  const ago = (min) => new Date(now - min * 60000).toISOString();
  const later = (min) => new Date(Date.now() + min * 60000).toISOString();

  // A computer's last hour, the same every time for the same computer.
  function demoSysmon(machine, since) {
    const mac = machine === "macbook";
    let seed = mac ? 7 : 3;
    const rnd = () => ((seed = (seed * 16807) % 2147483647) / 2147483647);
    const end = Math.floor(Date.now() / 10000) * 10, samples = [];
    const memTotal = mac ? 48 * 2 ** 30 : 124 * 2 ** 30, diskTotal = mac ? 494e9 : 5.5e12;
    for (let t = end - 3590; t <= end; t += 10) {
      const spike = rnd() < 0.08 ? 20 + rnd() * 25 : 0;
      samples.push({
        t, cpu: 3 + rnd() * 4 + spike, load: (mac ? 4 : 1) + rnd() * 2, mem_used: memTotal * ((mac ? 0.62 : 0.07) + rnd() * 0.02), mem_total: memTotal,
        rx: 20e3 + rnd() * 60e3 + (rnd() < 0.1 ? 400e3 * rnd() : 0), tx: 40e3 + rnd() * 50e3 + (rnd() < 0.1 ? 300e3 * rnd() : 0),
        disk_used: diskTotal * (mac ? 0.86 : 0.14), disk_total: diskTotal,
      });
    }
    const minute = Math.floor(end / 60) * 60, recent = Array.from({ length: 60 }, (_, i) => i > 54 ? 0 : Math.round(rnd() * 600e3 + 300e3));
    return {
      system: mac ? { os: "macOS 26.4.1", kernel: "Darwin 25.4.0", model: "Mac16,8", cpu: "Apple M4 Pro", cores: 14, boot: end - 86400 * 19 }
        : { os: "Debian GNU/Linux 12 (bookworm)", kernel: "6.1.0-28-amd64", model: "QEMU Standard PC", cpu: "AMD EPYC 9654", cores: 32, boot: end - 86400 * 41 },
      samples: samples.filter((x) => x.t > since),
      disks: mac ? [{ mount: "/", used: diskTotal * 0.86, total: diskTotal }] : [{ mount: "/", used: 14e9, total: 135e9 }, { mount: "/data00", used: diskTotal * 0.14 - 14e9, total: diskTotal - 135e9 }],
      tokens: { per_minute: mac ? 0 : 412e3, today: mac ? 31e6 : 139e6, recent: mac ? recent.map(() => 0) : recent, minute },
      versions: mac ? { claude: "2.1.306", codex: "0.167.0", herdr: "0.9.5" } : { claude: "2.1.302", codex: "0.167.0", agy: "1.5.16", herdr: "0.9.5" },
      every: 10,
    };
  }

  // ───────────────────────────── the scene
  const machines = [
    { id: "", name: "devbox", os: "linux/amd64", home: "/home/alex", local: true, online: true, agents: ["claude", "codex", "agy"] },
    { id: "macbook", name: "macbook", os: "darwin/arm64", home: "/Users/alex", online: true, version: "v0.2.41", agents: ["claude", "codex"] },
  ];
  const agent = (o) => ({ workspace_label: o.name, title: "", is_agent: true, machine: o.pane_id.includes("@") ? o.pane_id.split("@")[1] : "", session: "0193a1c2-" + o.workspace_id.replace(/\W/g, "") + "-demo", ...o });
  const agents = [
    agent({ pane_id: "w1:p1", workspace_id: "w1", name: "collie", is_router: true, kind: "claude", status: "idle", cwd: "/home/alex/.collie/router" }),
    agent({ pane_id: "w2:p1@macbook", workspace_id: "w2@macbook", name: "claude-1", kind: "claude", status: "working", cwd: "/Users/alex/code/shop-web", topic: "首页加暗色模式", run: ["claude-opus-5-5[1m]", "high"], context: { used: 120000, window: 1000000 } }),
    agent({ pane_id: "w3:p1", workspace_id: "w3", name: "codex-1", kind: "codex", status: "blocked", cwd: "/home/alex/code/shop-api", topic: "修复 /orders 分页的边界问题", run: ["gpt-6.1-sol", "high"], context: { used: 214000, window: 272000 } }),
    agent({ pane_id: "w4:p1", workspace_id: "w4", name: "agy-1", kind: "agy", status: "waiting", cwd: "/home/alex/code/shop-api", topic: "整理数据库迁移脚本", run: ["", ""] }),
    agent({ pane_id: "w5:p1", workspace_id: "w5", name: "fix-flaky-test", kind: "codex", status: "working", cwd: "/home/alex/code/shop-api", topic: "修复订单分页测试的偶发失败", run: ["gpt-6.1-sol", "medium"], context: { used: 48000, window: 272000 } }),
    agent({ pane_id: "w8:p1", workspace_id: "w8", name: "codex-2", kind: "codex", status: "done", cwd: "/home/alex/code/shop-api/.claude/worktrees/orders-index", topic: "给订单列表加复合索引", run: ["gpt-6.1-sol", "high"] }),
    agent({ pane_id: "w6:p1@macbook", workspace_id: "w6@macbook", name: "release-notes", kind: "claude", status: "done", cwd: "/Users/alex/code/shop-web", topic: "整理 v1.4 发布说明", run: ["sonnet", "low"] }),
    agent({ pane_id: "w9:p1", workspace_id: "w9", name: "codex-3", kind: "codex", status: "idle", cwd: "/home/alex/code/shop-web", topic: "在 Linux 上跑一遍 e2e 回归", run: ["gpt-6.1-sol", "medium"] }),
  ];
  const shells = [
    { pane_id: "w7:p1", workspace_id: "w7", workspace_label: "logs", name: "logs", kind: "", status: "shell", cwd: "/home/alex/code/shop-api", title: "", is_agent: false, machine: "" },
  ];
  const state = () => ({ ok: true, agents, shells, machines });
  const marks = {};

  const workers = [
    { name: "fix-flaky-test", kind: "codex", cwd: "/home/alex/code/shop-api", task: "找出 TestOrdersPagination 偶发失败的原因并修好", thread: "chat:main", pane_id: "w5:p1", workspace_id: "w5", machine: "", created_by: "collie", created_at: ago(14), status: "working", run: ["gpt-6.1-sol", "medium"], alias: "cx" },
    { name: "release-notes", kind: "claude", cwd: "/Users/alex/code/shop-web", task: "根据 v1.3..main 的提交写 v1.4 发布说明", thread: "chat:main", pane_id: "w6:p1@macbook", workspace_id: "w6@macbook", machine: "macbook", created_by: "collie", created_at: ago(14), status: "done", last_report: "CHANGELOG 已更新：6 条改动，2 条需要注意的升级事项。", report_at: ago(6), run: ["sonnet", "low"] },
  ];
  let seq = 100;
  const dismissed = new Set();
  const entry = (role, text, o = {}) => ({ id: "e" + (++seq), role, thread: "chat:main", text, at: o.at || new Date().toISOString(), ...o });
  const earlierEntries = [
    { id: "e62", role: "user", thread: "chat:main", text: "帮我查一下 shop-api 里优惠券接口 SCHEMA_AND_ID 校验报错的栈，看看是不是上周迁移改出来的。", at: ago(60 * 52) },
    { id: "e63", role: "collie", thread: "chat:main", text: "查到了：`coupon_validator.go` 在空购物车时跳过了 `SCHEMA_AND_ID` 默认值填充，导致老客户端传空串时返回 400。已经在 `~/code/shop-api/internal/coupons/validator.go` 补上兜底并加了单测。", at: ago(60 * 51.8) },
    { id: "e64", role: "event", thread: "chat:main", tone: "done", ref: "coupon-fix", text: "coupon-fix 回报：已修复 SCHEMA_AND_ID 空值校验并补单测。", at: ago(60 * 51.5) },
    { id: "e68", role: "user", thread: "chat:main", text: "结算页的缓存过期配置是不是要改名？顺便把压测基线跑一下。", at: ago(60 * 38) },
    { id: "e69", role: "collie", thread: "chat:main", text: "对，`cart.ttl` 统一改成 `cart.expire_after`（兼容旧字段一个版本）。1020 并发压测跑完了，P99 从 142ms 降到 89ms，报告在 [压测看板](https://metrics.example.com/shop-api/run-1020) 可以直接看。", at: ago(60 * 37.5) },
    { id: "e74", role: "user", thread: "chat:main", text: "商品图片首屏加载有点慢，让 macbook 上的 Claude 改成懒加载试试。", at: ago(60 * 26) },
    { id: "e75", role: "collie", thread: "chat:main", text: "已经改好合入了：首屏大图保留高优先级，折叠区下方的商品卡片全部加上 `loading=\"lazy\"`，首屏体积少了约 30%。", at: ago(60 * 25.4) },
    { id: "e80", role: "user", thread: "chat:main", text: "明天早上提醒我看一下 v1.4 的发布清单。", at: ago(60 * 18) },
    { id: "e81", role: "collie", thread: "chat:main", text: "好，明早 09:00 提醒你。", at: ago(60 * 18) },
  ];
  const entries = [
    entry("user", "订单接口的分页测试偶尔会挂，帮我查一下。顺便把 v1.4 的发布说明写了。\n\n附件：\n/home/alex/.collie/uploads/20261119/a1b2c3-ci-failure.png\n/home/alex/.collie/uploads/20261119/d4e5f6-query-plan.png", { at: ago(15) }),
    entry("collie", "好，分两路并行：\n\n- **fix-flaky-test**（Codex · devbox）查 `TestOrdersPagination` 为什么偶发失败\n- **release-notes**（Claude · macbook）按 v1.3 以来的提交写发布说明\n\n有结果我再告诉你。", { at: ago(14.5) }),
    entry("event", "派出 fix-flaky-test（codex · devbox）", { tone: "spawn", ref: "fix-flaky-test", at: ago(14) }),
    entry("event", "派出 release-notes（claude · macbook）", { tone: "spawn", ref: "release-notes", at: ago(14) }),
    entry("user", "另外首页首屏好像有点慢，有空看看。", { at: ago(7.2) }),
    entry("collie", "顺便问一下，`shop-api/migrations` 整理完了（41 → 33 个文件），那 3 个从没在生产跑过的旧回滚脚本怎么处理？", { at: ago(6.5), options: ["移到 archive 留档", "直接删掉"] }),
    entry("event", "release-notes 回报：CHANGELOG 已更新：6 条改动，2 条需要注意的升级事项。", { tone: "done", ref: "release-notes", at: ago(6) }),
    entry("collie", "发布说明写好了：6 条改动，其中 **配置项改名** 和 **Node 22 起步** 需要在升级说明里提醒。要我直接开 PR 吗？升级指南放在团队文档（https://docs.example.com/shop-web/upgrade-guide-v1-4#node-22，第 2 节）。详见 ~/code/shop-web/CHANGELOG.md\n\n顺带附上首页明暗主题验收宽截图与架构图：~/code/shop-web/docs/preview-dark.png 、~/code/shop-web/docs/preview-light.png 、~/code/shop-web/docs/architecture.svg\n\n分页测试那边还在跑，初步看是按 `created_at` 排序时同一秒内的订单顺序不稳定。首屏慢的事等它跑完我再派人看。", { at: ago(5.5), re: { worker: "release-notes", who: "release-notes", text: "根据 v1.3..main 的提交写 v1.4 发布说明" }, options: ["直接开 PR", "我先看一眼改动", "先不用"] }),
    entry("collie", "**fix-flaky-test** 找到根因了：同一秒创建的订单只按 `created_at` 排序，顺序不稳定。已在 `~/code/shop-api/internal/orders/list.go:362-385` 补上 `id DESC` 作为第二排序键：\n\n```go\nconst listSQL = `\n\tSELECT id, user_id, status, total_cents, created_at\n\tFROM orders\n\tWHERE user_id = $1\n\tORDER BY created_at DESC, id DESC\n\tLIMIT $2 OFFSET $3`\n```\n\n200 轮压测对比图表：`~/code/shop-api/bench-pagination.html`，全接口压测与索引复盘长报告已整理到 `~/code/shop-api/docs/perf-audit-v1.4.md`（含 8 列宽表，支持顶栏下载、复制原文或新标签宽屏阅读），压测看板见 `https://metrics.example.com/shop-api/run-1020`，暗色首屏基准图见 `~/code/shop-web/docs/preview-dark.png`。\n\n另外冷备归档路径 `hdfs://cluster-east/warehouse/shop_orders/dt=2026-11-20`、`/api/orders` 路由签名与本地复现命令 `go test -count=1 ./...` 均保持不变；顺带扫到已取消（`cancelled`）的订单目前仍计入列表页眉的 `total_count`，不确定前端是不是故意这样展示的，你空了看一眼。", { at: ago(2.5), check: true }),
  ];
  const pinnedIds = [entries[1].id];
  const pendingSummary = (t) => String(t || "").replace(/```[\s\S]*?```/g, " ").replace(/\[image:[^\]]+\]/g, " ")
    .replace(/\[([^\]\n]+)\]\([^)\n]+\)/g, "$1").split("\n")
    .map((l) => l.replace(/^\s*(?:[#>*•\-]+|\d+[.)])\s+/, "").replace(/\*\*|__|`/g, "").trim())
    .filter(Boolean).join(" ").slice(0, 120);
  const humanSummary = (e) => {
    let t = String(e.text || "");
    if (e.role === "user") {
      const att = t.lastIndexOf("附件：\n");
      if (att >= 0) t = (t.slice(0, att).trim() + " 📎").trim();
    }
    return pendingSummary(t);
  };
  function computePinned() {
    const all = [...earlierEntries, ...entries];
    return pinnedIds.map((id) => all.find((e) => e.id === id)).filter(Boolean)
      .map((e) => ({ id: e.id, at: e.at, role: e.role, summary: humanSummary(e) }));
  }
  function computePending() {
    const all = [...earlierEntries, ...entries];
    const out = [];
    for (let i = 0; i < all.length; i++) {
      const e = all[i];
      if (e.role !== "collie" || (!e.options?.length && !e.check) || dismissed.has(e.id)) continue;
      let resolved = false, otherCollie = false;
      for (let j = i + 1; j < all.length; j++) {
        const next = all[j];
        if (next.role === "user") {
          const reId = next.re?.entry || "";
          if (reId === e.id || (!reId && !otherCollie)) { resolved = true; break; }
        } else if (next.role === "collie") {
          otherCollie = true;
        }
      }
      if (!resolved) out.push({ id: e.id, at: e.at, summary: pendingSummary(e.text), ...(e.options?.length ? { options: e.options } : {}), ...(e.check ? { check: true } : {}) });
    }
    return out;
  }
  const sup = () => ({
    router: { alive: true, status: routerStatus, pane_id: "w1:p1", kind: "claude", context: 23, used: 230000, window: 1000000, effort: "medium", rotate_at: 70, machine: "macbook", alias: "ccl5" },
    entries, pending: computePending(), pinned: computePinned(), workers, queued: 0, queued_router: 0, paused: false, earlier: true, worker_title: settings.settings.worker_title,
    follow: { next: new Date(new Date().setHours(33, 0, 0, 0)).toISOString(), next_text: "复查首页暗色模式的回归测试" }, // a check-again tomorrow 09:00
  });
  let routerStatus = "idle";

  // Conversations, by pane. Tool steps read as one quiet line in the app.
  const tool = (name, input, output, error) => ({ kind: "tool", name, input, output, error });
  const chats = {
    "w2:p1@macbook": [
      { kind: "user", text: "[image:~/.collie/images/0d3a9e5c71b2f4e8a6c1.png]\n首页加一个暗色模式：跟随系统，右上角可以手动切换，记住用户的选择。参考这张图的配色。" },
      { kind: "assistant", text: "先看看现在的样式是怎么组织的。" },
      tool("Read", "src/styles/theme.css", "48 lines"),
      tool("Grep", "color: #", "src/components/Header.tsx:12\nsrc/components/Card.tsx:31\nsrc/pages/Home.tsx:77"),
      tool("Read", "src/components/Header.tsx", "64 lines"),
      { kind: "assistant", text: "颜色大多已经走 CSS 变量，只有三处写死了。计划：\n\n1. `theme.css` 里补一套 `[data-theme=dark]` 变量，并在 `prefers-color-scheme: dark` 下默认启用\n2. 三处硬编码颜色改成变量\n3. Header 加一个切换按钮，选择存进 `localStorage`" },
      tool("Edit", "src/styles/theme.css", "+31 −2"),
      tool("Edit", "src/components/Card.tsx", "+1 −1"),
      tool("Edit", "src/pages/Home.tsx", "+2 −2"),
    ],
    "w3:p1": [
      { kind: "user", text: "/orders?page=N 在最后一页会多返回一条重复记录，修一下并补测试。" },
      tool("Read", "internal/orders/list.go", "112 lines"),
      tool("Read", "internal/orders/list_test.go", "86 lines"),
      { kind: "assistant", text: "找到了：`offset` 用的是 `page * size`，但 `page` 从 1 开始，最后一页会和前一页重叠一条。改成 `(page-1) * size`，再加一个跨页不重复的测试。" },
      tool("Edit", "internal/orders/list.go", "+3 −1"),
      tool("Edit", "internal/orders/list_test.go", "+24"),
      { kind: "assistant", text: "改好了，跑一下测试确认。" },
    ],
    "w4:p1": [
      { kind: "user", text: "migrations/ 里有不少重复和废弃的脚本，整理一下，保证从空库能一路升到最新。" },
      tool("Bash", "ls migrations | wc -l", "41"),
      tool("Bash", "make db-reset && make migrate", "ok: 41 migrations applied"),
      tool("Edit", "migrations/README.md", "+18"),
      { kind: "assistant", text: "整理完了：\n\n- 合并了 6 个只加索引的小脚本，删掉 3 个从没在生产跑过的回滚脚本\n- 从空库升级到最新验证通过（41 → 33 个文件）\n- `migrations/README.md` 写了命名规则和怎么加新脚本" },
    ],
    "w5:p1": [
      { kind: "user", text: "[collie] 找出 TestOrdersPagination 偶发失败的原因并修好。完成后 collie report。" },
      tool("Bash", "go test -run TestOrdersPagination -count=50 ./internal/orders", "--- FAIL: 3 of 50"),
      tool("Read", "internal/orders/list.go", "112 lines"),
      { kind: "assistant", text: "50 次里挂 3 次。测试在同一秒内插入多条订单，而查询只按 `created_at` 排序，同一秒的顺序数据库不保证。加上 `id` 作为第二排序键。" },
      tool("Edit", "internal/orders/list.go", "+1 −1"),
      tool("Bash", "go test -run TestOrdersPagination -count=200 ./internal/orders"),
    ],
    "w9:p1": [
      { kind: "user", text: "在 Linux 上跑一遍 e2e 回归，看看暗色模式有没有影响" },
      { kind: "assistant", text: "先装依赖，再跑全量 e2e。" },
      tool("Bash", "pnpm install --frozen-lockfile", "Done in 8.2s"),
      tool("Bash", "pnpm test:e2e", "42 passed"),
      { kind: "assistant", text: "42 个 e2e 用例都过了，暗色模式的改动还没合进来，合并后再跑一次。\n\n| 套件 | 用例 | 结果 |\n|-|-|-|\n| 下单 | 18 | 通过 |\n| 购物车 | 15 | 通过 |\n| 账户 | 9 | 通过 |\n\n报告：https://ci.example.com/shop-web/pipelines/48213/jobs/e2e-linux/artifacts/report/index.html?tab=summary", turn_ms: 281000 },
    ],
    "w6:p1@macbook": [
      { kind: "user", text: "[collie] 根据 v1.3..main 的提交写 v1.4 发布说明。完成后 collie report。" },
      tool("Bash", "git log --oneline v1.3..main", "27 commits"),
      tool("Edit", "CHANGELOG.md", "+38"),
      { kind: "assistant", text: "CHANGELOG 已更新：6 条改动，2 条需要注意的升级事项（配置项 `api_url` 改名为 `api_base`；最低 Node 版本升到 22）。" },
    ],
    "w1:p1": [
      { kind: "user", text: "[collie] 1 个事件 · 对话 · 订单接口的分页测试偶尔会挂……" },
      tool("Bash", "collie spawn -kind codex -name fix-flaky-test \"…\"", "spawned fix-flaky-test on devbox"),
      tool("Bash", "collie spawn -kind claude -machine macbook -name release-notes \"…\"", "spawned release-notes on macbook"),
      tool("Bash", "collie reply \"好，分两路并行……\"", "ok"),
    ],
  };
  const lives = {
    "w2:p1@macbook": { status: "working", tail: "Editing src/components/Header.tsx…", meta: "42s · ↓ 1.8k tokens" },
    "w5:p1": { status: "working", tail: "Running go test -run TestOrdersPagination -count=200 ./internal/orders", meta: "18s" },
  };
  const approvals = {
    "w3:p1": {
      title: "codex 想运行一条命令",
      question: "Codex 想在 shop-api 里跑一遍测试。\n\n    go test ./internal/orders/...\n\n允许运行这条命令吗？",
      choices: [{ label: "允许", keys: ["1"] }, { label: "允许，这个会话里不再问", keys: ["2"] }, { label: "不允许，告诉它怎么做", keys: ["3"] }],
      hint: "",
      screen: "  Would you like to run the following command?\n\n  $ go test ./internal/orders/...\n\n› 1. Yes, proceed\n  2. Yes, and don't ask again for this command\n  3. No, and tell Codex what to do differently",
    },
  };
  const demoLogLines = Array.from({ length: 58 }, (_, i) => {
    const mm = String(Math.floor(i / 4)).padStart(2, "0"), ss = String((i * 13) % 60).padStart(2, "0");
    if (i % 9 === 0) return `12:${mm}:${ss} \x1b[33mWARN\x1b[0m  slow query orders_by_created_at (${110 + i}ms)`;
    return `12:${mm}:${ss} \x1b[32mINFO\x1b[0m  GET /orders?page=${(i % 6) + 1} 200 ${12 + (i % 19)}ms`;
  }).join("\n");
  const screens = {
    "w2:p1@macbook": "╭───────────────────────────────────────────────╮\n│ ✻ Claude Code                                 │\n│   ~/code/shop-web                             │\n╰───────────────────────────────────────────────╯\n\n> 首页加一个暗色模式：跟随系统，右上角可以手动切换，记住用户的选择。\n\n● 先看看现在的样式是怎么组织的。\n\n● Read(src/styles/theme.css)\n  ⎿  Read 48 lines\n\n● Grep(color: #)\n  ⎿  src/components/Header.tsx:12\n     src/components/Card.tsx:31\n     src/pages/Home.tsx:77\n\n● Read(src/components/Header.tsx)\n  ⎿  Read 64 lines\n\n● 颜色大多已经走 CSS 变量，只有三处写死了。计划：\n  1. theme.css 里补一套 [data-theme=dark] 变量，默认跟随系统\n  2. 三处硬编码颜色改成变量\n  3. Header 加切换按钮，选择存进 localStorage\n\n● Update(src/styles/theme.css)\n  ⎿  Updated with 31 additions and 2 removals\n     + :root[data-theme=\"dark\"] {\n     +   --surface: #111214;\n     +   --ink: #ededeb;\n     + }\n\n● Update(src/components/Card.tsx)\n  ⎿  Updated with 1 addition and 1 removal\n\n● Update(src/pages/Home.tsx)\n  ⎿  Updated with 2 additions and 2 removals\n\n✻ Editing src/components/Header.tsx… (42s · ↓ 1.8k tokens)\n\n╭───────────────────────────────────────────────╮\n│ > \x1b7                                            │\n╰───────────────────────────────────────────────╯\n  ? for shortcuts                    opus · high",
    "w7:p1": "alex@devbox:~/code/shop-api$ tail -n 60 -f logs/api.log\n" + demoLogLines + "\nalex@devbox:~/code/shop-api$ \x1b7",
  };
  const wrapCols = (s, max = 88) => s.split("\n").flatMap((line) => {
    const out = [];
    let cur = "", w = 0;
    for (const ch of line) {
      const cw = ch.codePointAt(0) > 0x2e7f ? 2 : 1;
      if (w + cw > max) { out.push(cur); cur = ch; w = cw; } else { cur += ch; w += cw; }
    }
    out.push(cur);
    return out;
  }).join("\n");
  const screenOf = (id) => screens[id] || wrapCols((chats[id] || []).filter((m) => m.kind !== "tool").map((m) => (m.kind === "user" ? "> " : "● ") + m.text).join("\n\n"));

  const external = [
    { id: "x1", kind: "claude", pid: 4242, cwd: "/Users/alex/code/notes", app: "iTerm", surface: "cli", started: ago(90), session: "s1", title: "周报草稿", updated: ago(20), machine: "macbook", status: "idle", can_send: false, live: true },
    { id: "x3", kind: "codex", pid: 5120, cwd: "/home/alex/code/shop-api", app: "VS Code", surface: "cli", started: ago(30), session: "s5", title: "排查 Redis 连接泄漏", updated: ago(1), status: "working", can_send: false, read_only: "这个 Codex 会话没有在共享后台里运行（旧版本，或用了独立的后台），只能看", live: true },
    { id: "cd-2", kind: "claude", pid: 4310, cwd: "/Users/alex/code/shop-web", app: "Claude 桌面版", surface: "desktop", started: ago(240), session: "s2", title: "重构购物车状态管理", updated: ago(55), machine: "macbook", status: "idle", can_send: true, live: true },
    { id: "cx-7", kind: "codex", pid: 0, cwd: "/Users/alex/code/shop-web", app: "Codex 桌面版", surface: "desktop", started: ago(300), session: "s7", title: "检查结算页的无障碍问题", updated: ago(70), machine: "macbook", status: "idle", can_send: true, live: true },
  ];
  const history = [
    { id: "h1", kind: "codex", cwd: "/home/alex/code/shop-api", name: "codex-3", session: "s3", ended: ago(60 * 20), topic: "升级 Go 到 1.25" },
    { id: "h2", kind: "claude", cwd: "/Users/alex/code/shop-web", name: "claude-4", session: "s4", ended: ago(60 * 30), topic: "图片懒加载", machine: "macbook" },
  ];
  const extChats = {
    x1: [{ kind: "user", text: "根据这周的 commit 帮我写周报，分成已完成、进行中、下周计划。" }, { kind: "assistant", text: "**已完成**\n- 订单列表分页修复\n- 迁移脚本整理（41 → 33）\n\n**进行中**\n- 首页暗色模式\n\n**下周计划**\n- v1.4 发布" }],
    "cx-7": [{ kind: "user", text: "检查结算页有哪些无障碍问题。" }, { kind: "assistant", text: "找到 4 处：两个按钮缺少可读名称，金额颜色对比度不够，错误提示没有关联到输入框。" }],
    "cd-2": [{ kind: "user", text: "购物车的状态现在分散在三个 context 里，合并成一个 store。" }, { kind: "assistant", text: "合并完成，三个 context 换成了一个 `useCart` store，组件改动 9 处，测试全部通过。" }],
  };

  const triggers = [
    { id: "t31", source: "chat", thread: "chat:main", from: "you", action: "respond", status: "delivered", text: entries[0].text, at: ago(15), targets: ["collie"] },
    { id: "t32", source: "worker", thread: "chat:main", from: "release-notes", action: "respond", status: "delivered", text: "report from release-notes: CHANGELOG 已更新", at: ago(6), targets: ["collie"] },
    { id: "t33", source: "github", thread: "github:alex/shop-api#58", from: "octo-dev", action: "candidate", status: "delivered", text: "issue_comment: 分页最后一页有重复数据，能看一下吗？@collie", at: ago(40), targets: ["collie"] },
    { id: "t34", source: "timer", thread: "chat:main", from: "", action: "respond", status: "delivered", text: "每天 9 点：检查各个任务的进展，有卡住的告诉我", at: ago(60 * 3), targets: ["collie"] },
  ];
  const threads = [
    { id: "chat:main", triggers: triggers.filter((t) => t.thread === "chat:main"), subscribers: [], last_at: ago(6) },
    { id: "github:alex/shop-api#58", triggers: [triggers[2]], subscribers: ["codex-1"], last_at: ago(40) },
  ];
  // The workbench: pinned pages and shortcuts (web/workbench.js).
  const wb = {
    groups: [{ id: "fav", title: "收藏" }, { id: "life", title: "生活" }, { id: "collie", title: "Collie" }],
    modules: [
      { id: "books", title: "读书", icon: "📚", group: "life", kind: "web", url: "https://example.com/books", embed: "tab", feed: "https://example.com/agent.md", share: "keep", has_token: true, snap_at: later(-600) },
      { id: "ledger", title: "账本", icon: "📒", group: "life", kind: "web", url: "https://example.com/ledger", embed: "tab", feed: "https://example.com/ledger.md", share: "read", sensitive: true, has_token: true },
      { id: "router", title: "路由代理", icon: "🛜", group: "fav", kind: "web", url: "https://example.com/router", embed: "tab", share: "off" },
      { id: "inbox", title: "收件箱", icon: "📥", group: "collie", kind: "link", url: "auto:inbox", share: "off" },
      { id: "memory", title: "记忆", icon: "🧠", group: "collie", kind: "link", url: "auto:memory", share: "off" },
      { id: "machines", title: "电脑", icon: "🖥", group: "collie", kind: "link", url: "devices:machines", share: "off" },
    ],
  };

  const timers = [
    { id: "tm1", spec: "weekdays 09:00", text: "检查各个任务的进展，有卡住的告诉我", target: "", enabled: true, next_run: later(60 * 18), owner: "you" },
    { id: "tm2", spec: "in 30m", text: "看一下 fix-flaky-test 跑完没有", target: "", enabled: true, next_run: later(16), owner: "collie" },
  ];
  const memFile = (name, text, min, size) => ({ name, text, size: size ?? text.length * 2, mod: ago(min) });
  const memory = {
    dir: "~/.collie", up_front: 5734, messages: 2867, loaded: 8601, budget: 10240, dreaming: false, last_dream: ago(60 * 9), next_dream: later(60 * 15), dreams: [
      { name: "2026-11-18.md", text: "从 fix-flaky-test 里记下了分页测试的坑，写进 shop-api.md；合并了两条重复的笔记\n\n- shop-api.md：同一秒内的订单按 created_at 排序不稳定，要加 id 兜底\n- agents.md：Codex 在 devbox 上跑 shop-api 的测试更稳\n- 你更喜欢先在 devbox 上跑测试，再在 Mac 上看界面（inferred）" },
    ],
    files: [
      memFile("instructions.md", "# 指令\n- 发布说明用中文，每条一句话\n- 跑测试优先用 devbox，界面相关的活放 macbook\n- 动生产数据库前先问我", 60 * 24, 1843),
      memFile("tasks/v1.4-release.md", "# v1.4 发布\n目标：整理 v1.3 以来的 27 个提交写发布说明（重点提醒配置项改名与 Node 22 起步），定位并修复 /orders 分页同一秒内记录重复的偶发失败（fix-flaky-test），回归通过后一起开 PR。\n- [x] 发布说明（release-notes）\n- [ ] 分页测试修复（fix-flaky-test）\n- [ ] 开 PR", 6, 2253),
      memFile("tasks/done/dark-mode-fix.md", "# 首页暗色模式修复\n- [x] 定位对比度问题\n- [x] 合入修复（ux-polish）\n已完成：11 月 18 日合入。", 50),
      memFile("tasks/done/ci-cache.md", "# CI 缓存提速\n- [x] 缓存依赖目录\n已完成：构建从 9 分钟降到 4 分钟。", 90),
      memFile("notes/index.md", "# 笔记索引\n- shop-api.md：订单服务，怎么跑测试、分页的坑\n- shop-web.md：前端，怎么起、发布说明放哪\n- agents.md：哪个 agent 在哪台电脑上好用\n- machines.md：devbox 和 macbook 各装了什么", 60 * 30, 1638),
      memFile("notes/shop-api.md", "# shop-api\n- 仓库 ~/code/shop-api（devbox），Go 1.25，测试 `make test`\n- 分页测试偶发失败：同一秒内的订单按 `created_at` 排序不稳定，排序要加 `id` 兜底（fix-flaky-test 查出来的）\n- 测试数据库用 docker 起，`make db`；跑完 `make db-down`\n- CI 只跑 `./internal/...`（截至 11-18）", 60 * 9),
      memFile("notes/shop-web.md", "# shop-web\n- 仓库 ~/code/shop-web（macbook），Vite + React，`pnpm dev`\n- 发布说明写在 CHANGELOG.md，按「需要注意 / 改动」两段\n- 首页明暗主题基准图：![首页暗色预览](~/code/shop-web/docs/preview-dark.png)", 60 * 30),
      memFile("notes/agents.md", "# agent 经验\n- Codex 在 devbox 上跑 shop-api 的测试更稳，Claude 偶尔会改错测试本身\n- 界面相关的活给 macbook 上的 Claude\n- macbook 上没装 Antigravity", 60 * 9),
      memFile("notes/machines.md", "# 电脑\n- devbox：Linux，claude / codex / agy 都有\n- macbook：claude、codex；周末常关机", 60 * 24 * 3),
      memFile("messages.md", "# What Collie recently told the human (written by the hub, oldest first)\n\n- 11-19 09:45 [chat:main] 好，分两路并行：fix-flaky-test 查分页测试，release-notes 写发布说明。\n- 11-19 09:53 [chat:main] shop-api/migrations 整理完了（41 → 33 个文件），那 3 个旧回滚脚本怎么处理？\n- 11-19 09:54 [chat:main] 发布说明写好了：6 条改动，要我直接开 PR 吗？", 5, 2867),
    ],
  };
  const settings = {
    bin: "collie", hook_key: "demo-key", router_dir: "~/.collie/router",
    policies: [{ source: "chat", action: "respond" }, { source: "github", action: "candidate" }, { source: "timer", action: "respond" }],
    settings: { router_kind: "claude", router_model: "sonnet", worker_model: "gpt-6.1-sol", worker_effort: "high", worker_alias: "cx", worker_kind: "codex", worker_cwd: "~/code", worker_machine: "", notify_level: "normal", stall_minutes: 20, reply_chars: 400, rotate_pct: 70, dream_at: "04:00", ask_permissions: false },
  };
  // Launcher aliases from each computer's shell rc file.
  const al = (name, kind, model, effort, base_url, command) => ({ name, kind, model, effort, base_url, command });
  const zshrc = [
    al("ccf", "claude", "fable[1m]", "", "https://relay.example.com", "ANTHROPIC_BASE_URL=https://relay.example.com ANTHROPIC_AUTH_TOKEN=*** claude --model 'fable[1m]'"),
    al("ccl5", "claude", "claude-opus-5-5[1m]", "high", "$ANTHROPIC_BASE_URL", "ANTHROPIC_BASE_URL=\"$ANTHROPIC_BASE_URL\" claude --model 'claude-opus-5-5[1m]' --effort high"),
    al("cx", "codex", "gpt-6.1-sol", "high", "", "codex -m gpt-6.1-sol -c model_reasoning_effort=high"),
    al("cxm", "codex", "gpt-6.1-mini", "", "", "command codex -m gpt-6.1-mini"),
    al("agp", "agy", "gemini-4-pro", "", "", "agy --model gemini-4-pro"),
  ];
  const aliasMachines = [
    { id: "", name: "devbox", shell: "zsh", rc: "~/.zshrc", synced_at: ago(1), stale: false, aliases: zshrc },
    { id: "macbook", name: "macbook", shell: "zsh", rc: "~/.zshrc", synced_at: ago(60 * 26), stale: true, error: "读取 alias 超时", aliases: zshrc.slice(0, 3) },
    { id: "devos", name: "devos", shell: "bash", rc: "~/.bashrc", synced_at: ago(60 * 50), stale: true, aliases: [al("ccs", "claude", "seed-evolving[1m]", "", "$ANTHROPIC_BASE_URL", "ANTHROPIC_BASE_URL=\"$ANTHROPIC_BASE_URL\" ANTHROPIC_AUTH_TOKEN=*** claude --model 'seed-evolving[1m]'")] },
  ];
  const aliases = () => {
    const by = new Map();
    for (const m of aliasMachines) for (const a of m.aliases) {
      if (!by.has(a.name)) by.set(a.name, { ...a, rc: m.rc, machines: [] });
      by.get(a.name).machines.push(m.id);
    }
    return { machines: aliasMachines, aliases: [...by.values()].sort((x, y) => x.name.localeCompare(y.name)) };
  };
  const models = {
    claude: { models: [{ id: "opus", label: "Opus", efforts: ["low", "medium", "high", "max"] }, { id: "sonnet", label: "Sonnet", efforts: ["low", "medium", "high"] }, { id: "haiku", label: "Haiku" }], default: "sonnet", efforts: ["low", "medium", "high", "max"], default_effort: "" },
    codex: { models: [{ id: "gpt-6.1-sol", label: "GPT-6.1-Sol", efforts: ["low", "medium", "high", "xhigh"] }, { id: "gpt-6.1-mini", label: "GPT-6.1-Mini" }], default: "gpt-6.1-sol", efforts: ["low", "medium", "high", "xhigh"], default_effort: "medium" },
    agy: { models: [{ id: "gemini-4-pro", label: "Gemini 4 Pro" }], default: "gemini-4-pro", efforts: [] },
  };

  // ───────────────────────────── streams
  const streams = new Set();
  class FakeES {
    constructor(url) {
      this.url = String(url); this.readyState = 0; this.ls = {}; this.onopen = null; this.onerror = null; this.onmessage = null;
      streams.add(this);
      setTimeout(() => { if (this.readyState === 2) return; this.readyState = 1; this.onopen?.({}); this.prime(); }, 30);
    }
    addEventListener(t, f) { (this.ls[t] ||= []).push(f); }
    removeEventListener(t, f) { this.ls[t] = (this.ls[t] || []).filter((x) => x !== f); }
    close() { this.readyState = 2; streams.delete(this); }
    emit(t, data) { if (this.readyState !== 1) return; const e = { type: t, data: JSON.stringify(data) }; (this.ls[t] || []).forEach((f) => f(e)); }
    get path() { return new URL(this.url, location.href).pathname.replace(/^.*?\/api\//, "/api/"); }
    prime() {
      const p = this.path, m = /^\/api\/(panes|external)\/([^/]+)\/(chat|stream)$/.exec(p);
      if (p === "/api/events") { this.emit("state", state()); this.emit("super", sup()); return; }
      if (!m) return;
      const id = decodeURIComponent(m[2]);
      if (m[1] === "external") { this.emit("patch", { base: 0, from: 0, msgs: extChats[id] || [] }); return; }
      if (m[3] === "chat") { this.emit("patch", { base: 0, from: 0, msgs: chats[id] || [] }); this.emit("live", liveOf(id)); return; }
      const text = screenOf(id); // the cursor marked (ESC 7) as the hub does
      this.emit("frame", { cols: 96, rows: 30, text, cursor: text.includes("\x1b7") });
    }
  }
  FakeES.CONNECTING = 0; FakeES.OPEN = 1; FakeES.CLOSED = 2;
  window.EventSource = FakeES;
  const liveOf = (id) => {
    const a = agents.find((x) => x.pane_id === id);
    if (a?.status === "blocked" && approvals[id]) return { status: "blocked", approval: approvals[id] };
    return { status: a?.status || "idle", ...(a?.status === "working" ? lives[id] || {} : {}) };
  };
  const pushAll = () => {
    for (const s of streams) {
      if (s.path === "/api/events") { s.emit("state", state()); s.emit("super", sup()); }
      const m = /^\/api\/panes\/([^/]+)\/chat$/.exec(s.path);
      if (m) { const id = decodeURIComponent(m[1]); s.emit("patch", { base: 0, from: 0, msgs: chats[id] || [] }); s.emit("live", liveOf(id)); }
    }
  };
  const setStatus = (id, st) => { const a = agents.find((x) => x.pane_id === id); if (a) a.status = st; const w = workers.find((x) => x.pane_id === id); if (w) w.status = st; };
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));

  // ───────────────────────────── scripted replies
  let demoN = 0;
  function collieSay(text, re) {
    const q = re && entries.find((e) => e.id === re);
    const mine = entry("user", text, q ? { re: { entry: q.id, who: "Collie", text: q.text } } : {});
    entries.push(mine); routerStatus = "working"; pushAll(); collieAnswer(text);
    return mine.id;
  }
  async function collieAnswer(text) {
    await wait(1400);
    const name = "task-" + ["deps", "tests", "docs", "perf"][demoN++ % 4];
    entries.push(entry("collie", `收到。这件事我交给 **${name}**（Codex · devbox）去做，它会在 \`~/code/shop-api\` 里动手；有结果或需要你拍板时我再说。`));
    const pane = "w" + (20 + demoN) + ":p1";
    agents.push(agent({ pane_id: pane, workspace_id: pane.split(":")[0], name, kind: "codex", status: "working", cwd: "/home/alex/code/shop-api", run: ["gpt-6.1-sol", "medium"] }));
    workers.push({ name, kind: "codex", cwd: "/home/alex/code/shop-api", task: text, thread: "chat:main", pane_id: pane, workspace_id: pane.split(":")[0], machine: "", created_by: "collie", created_at: new Date().toISOString(), status: "working", run: ["gpt-6.1-sol", "medium"] });
    chats[pane] = [{ kind: "user", text: "[collie] " + text }, tool("Bash", "rg -n TODO --stats", "37 matches in 12 files")];
    lives[pane] = { status: "working", tail: "Reading the code", meta: "12s" };
    entries.push(entry("event", `派出 ${name}（codex · devbox）`, { tone: "spawn", ref: name }));
    routerStatus = "idle"; pushAll();
    await wait(5000);
    chats[pane].push({ kind: "assistant", text: "看完了，结论和改动已经写进报告。" });
    setStatus(pane, "done");
    workers.find((w) => w.name === name).last_report = "完成：发现 2 处问题，已修复并补了测试。";
    entries.push(entry("event", `${name} 回报：完成：发现 2 处问题，已修复并补了测试。`, { tone: "done", ref: name }));
    routerStatus = "working"; pushAll();
    await wait(1200);
    entries.push(entry("collie", `${name} 做完了：发现 2 处问题，都已修复并补了测试，改动在 \`shop-api\` 的工作区里。要我开 PR 吗？`, { options: ["开 PR", "先不用"] }));
    routerStatus = "idle"; pushAll();
  }
  async function promptPane(id, text) {
    (chats[id] ||= []).push({ kind: "user", text });
    setStatus(id, "working"); lives[id] = { status: "working", tail: "Thinking…", meta: "3s" }; pushAll();
    await wait(1800);
    chats[id].push(tool("Read", "README.md", "52 lines"));
    pushAll();
    await wait(1600);
    chats[id].push({ kind: "assistant", text: "好的，已经按你说的改好了。这是演示模式，真实环境里这里会是 agent 的回答。" });
    setStatus(id, "done"); pushAll();
  }
  async function answerApproval(id) {
    delete approvals[id];
    setStatus(id, "working"); lives[id] = { status: "working", tail: "Running go test ./internal/orders/...", meta: "4s" };
    chats[id].push(tool("Bash", "go test ./internal/orders/...", "ok  \tshop-api/internal/orders\t0.412s"));
    pushAll();
    await wait(2600);
    chats[id].push({ kind: "assistant", text: "测试全部通过。改动：`list.go` 的 offset 修正，`list_test.go` 加了跨页不重复的用例。" });
    setStatus(id, "done"); pushAll();
  }

  const changelog = "# v1.4\n\n## 需要注意\n\n- **配置项改名**：`cart.ttl` 改为 `cart.expire_after`，旧名字还能用，下个版本移除。\n- **Node 22 起步**：构建和运行都需要 Node 22 以上。\n\n## 改动\n\n- 订单列表支持按状态筛选\n- 结算页显示预计送达时间\n- 修复优惠券在购物车为空时报错\n- 商品图片改为懒加载，首屏快了约 30%\n- 搜索框支持拼音\n- 依赖升级：react 19.2、vite 7\n";
  const perfAuditMd = `# shop-api v1.4 核心接口压测与分页稳定性复盘

> 测试环境：devbox（8 vCPU / 32 GB · PostgreSQL 16.4），压测基线对比 \`v1.3.8\` 与 \`v1.4.0-rc2\`（1020 并发持续 15 分钟）。

## 1. 全接口延迟与吞吐对比（宽表）

| 接口路由 | 压测场景 | v1.3 P50 | v1.3 P99 | v1.4 P50 | v1.4 P99 | 吞吐提升 | 关键变更与代码位置 |
| --- | --- | ---: | ---: | ---: | ---: | ---: | --- |
| \`GET /api/orders\` | 同秒多单翻页 + 状态过滤 | 18.4 ms | 146.2 ms | 7.9 ms | 28.5 ms | +118% | 复合索引 \`(user_id, created_at DESC, id DESC)\` · \`~/code/shop-api/internal/orders/list.go\` |
| \`POST /api/coupons/check\` | 空购物车 + 历史券码校验 | 11.2 ms | 84.0 ms | 6.1 ms | 19.4 ms | +64% | 补齐 \`SCHEMA_AND_ID\` 默认值兜底，消除回表重试 |
| \`GET /api/catalog/home\` | 首页首屏商品流（含缩略图） | 24.6 ms | 162.8 ms | 12.0 ms | 49.1 ms | +92% | 折叠区懒加载 + Redis 热键预热 · \`~/code/shop-web/CHANGELOG.md\` |
| \`POST /api/checkout/quote\` | 运费与预计送达时间试算 | 15.8 ms | 112.5 ms | 9.3 ms | 34.7 ms | +57% | 配置切换至 \`cart.expire_after\`，合并区域费率批量查询 |
| \`GET /api/orders/export\` | 月度账单流式导出（>5 万行） | 42.0 ms | 310.0 ms | 29.5 ms | 118.0 ms | +41% | 游标分页流式写出，冷备落盘 \`hdfs://cluster-east/warehouse/shop_orders/dt=2026-11-20\` |

## 2. 复合索引与查询计划

将原来的单列时间索引替换为覆盖排序键的复合索引后，消除了同秒记录的 \`Incremental Sort\` 抖动：

\`\`\`sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_user_created_id
\tON orders (user_id, created_at DESC, id DESC)
\tINCLUDE (status, total_cents);
\`\`\`

整体架构与读写分离链路示意：

![读写链路架构图](~/code/shop-web/docs/architecture.svg)

## 3. 回归验证清单

1. 连续 200 轮并发翻页回归：\`go test -run TestOrdersPagination -count=200 ./internal/orders\` 零失败。
2. 线上监控看板：\`https://metrics.example.com/shop-api/run-1020\`
3. 压测交互式分位曲线：\`~/code/shop-api/bench-pagination.html\`
`;
  const ordersListGo = `package orders

import (
\t"context"
\t"database/sql"
\t"fmt"
\t"time"
)

// Order is a single row in the customer order history page.
type Order struct {
\tID         int64     \`json:"id"\`
\tUserID     int64     \`json:"user_id"\`
\tStatus     string    \`json:"status"\`
\tTotalCents int64     \`json:"total_cents"\`
\tCreatedAt  time.Time \`json:"created_at"\`
}

const listSQL = \`
\tSELECT id, user_id, status, total_cents, created_at
\tFROM orders
\tWHERE user_id = $1
\tORDER BY created_at DESC, id DESC
\tLIMIT $2 OFFSET $3\`

// List returns one 1-indexed page of orders with deterministic tie-breaking.
func List(ctx context.Context, db *sql.DB, userID int64, page, size int) ([]Order, error) {
\tif page < 1 {
\t\tpage = 1
\t}
\tif size <= 0 || size > 100 {
\t\tsize = 20
\t}
\toffset := (page - 1) * size
\trows, err := db.QueryContext(ctx, listSQL, userID, size, offset)
\tif err != nil {
\t\treturn nil, fmt.Errorf("query orders page %d: %w", page, err)
\t}
\tdefer rows.Close()

\tvar out []Order
\tfor rows.Next() {
\t\tvar o Order
\t\tif err := rows.Scan(&o.ID, &o.UserID, &o.Status, &o.TotalCents, &o.CreatedAt); err != nil {
\t\t\treturn nil, err
\t\t}
\t\tout = append(out, o)
\t}
\treturn out, rows.Err()
}
`;

  // ───────────────────────────── extensions (接入, 收件箱)
  const larkx = { manifest_version: 1, id: "larkx", name: "飞书（以你本人身份）", version: "0.3.6", engines: { collie_protocol: 1 }, runtime: { command: "bin/larkx" },
    description: "用 LarkAgentX 扫码登录飞书网页版，以你本人的身份收发消息。收件箱列出你的飞书会话，打开某个会话后，发触发词即可交给 agent，回复以你的身份发出并带上尾标。",
    warning: "走逆向的飞书网页版协议，不是官方接口：飞书升级后可能失效，账号可能被风控。",
    contributes: { connector: { id: "larkx", capabilities: ["status", "login", "archive", "actions"] }, channel: { format: "markdown" } },
    configuration: { type: "object", properties: { region: { type: "string", title: "站点", default: "feishu", enum: ["feishu", "lark"], "x-labels": ["飞书", "Lark（国际版）"] } } } };
  const extData = { trigger: "", footer: "", default_trigger: "/ai", default_footer: "—— 🤖 {agent} · {model} · {elapsed}",
    machines: [{ id: "", name: "hub", online: true }, { id: "mini", name: "mac-mini", online: true }],
    extensions: [{ manifest: larkx, setting: { id: "larkx", enabled: true, machine: "mini", config: { region: "feishu" } },
      proc: { state: "running", pid: 4242 }, status: { state: "ok", account: "Alex" } }] };
  const inboxConvs = [
    { id: "larkx:oc_1", ext: "larkx", chat: "oc_1", label: "飞书", name: "产品周会", kind: "group", last_at: ago(3), preview: "张三: /ai 帮我总结今天的讨论", on: true, handler: "agent", worker: "lark-产品周会" },
    { id: "larkx:oc_2", ext: "larkx", chat: "oc_2", label: "飞书", name: "李四", kind: "dm", last_at: ago(25), preview: "李四: 明天的评审改到下午了吗", unread: 1, on: false, handler: "agent" },
    { id: "larkx:oc_3", ext: "larkx", chat: "oc_3", label: "飞书", name: "交付群", kind: "group", last_at: ago(90), preview: "王五: [图片]", on: false, handler: "agent" },
  ];
  const inboxMsgs = { "larkx:oc_1": [
    { id: "m1", conv: "larkx:oc_1", at: ago(9), sender_name: "李四", text: "评审定在周五，接口先冻结" },
    { id: "m2", conv: "larkx:oc_1", at: ago(4), sender_name: "张三", mine: true, text: "/ai 帮我总结今天的讨论" },
    { id: "out:o1", conv: "larkx:oc_1", at: ago(3), mine: true, ref: "o1", text: "今天定了三件事：\n\n1. 周五评审\n2. 接口先冻结\n3. 张三负责发布说明\n\n—— 🤖 Claude Code · opus-4-5 · 58秒" },
  ] };

  // ───────────────────────────── the API
  const json = (data, status = 200) => new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
  const realFetch = window.fetch.bind(window);
  window.fetch = async (input, init = {}) => {
    const u = new URL(typeof input === "string" ? input : input.url, location.href);
    const i = u.pathname.indexOf("/api/");
    if (i < 0) return realFetch(input, init);
    const p = u.pathname.slice(i), q = u.searchParams, method = (init.method || "GET").toUpperCase();
    let body = {};
    try { body = init.body ? JSON.parse(init.body) : {}; } catch {}
    await wait(60);
    let m;
    if (p === "/api/session") return json({ ok: true });
    if (p === "/api/state") return json(state());
    if (p === "/api/super") return json(sup());
    if (p === "/api/super/settings") {
      if (method === "POST" && body.settings) { settings.settings = body.settings; pushAll(); }
      return json(settings);
    }
    if (p === "/api/super/aliases") return json(aliases());
    if (p === "/api/super/seen") return json({ ok: true });
    if (p === "/api/super/dismiss") { if (body.id) dismissed.add(body.id); pushAll(); return json({ ok: true }); }
    if (p === "/api/super/pin") {
      const idx = pinnedIds.indexOf(body.id);
      if (body.pinned && body.id && idx < 0) pinnedIds.push(body.id);
      else if (!body.pinned && idx >= 0) pinnedIds.splice(idx, 1);
      pushAll();
      return json({ ok: true, pinned: !!body.pinned });
    }
    if (p === "/api/super/earlier") { // earlier history pages + direct `until` jump
      const idN = (id) => parseInt(String(id || "").replace(/^[a-z]+/i, ""), 10) || 0;
      const beforeN = idN(q.get("before")), untilN = idN(q.get("until"));
      const pool = earlierEntries.filter((e) => idN(e.id) < beforeN);
      if (!pool.length) return json({ more: false, entries: [] });
      const pageSize = 4;
      let slice;
      if (untilN > 0 && untilN < beforeN) {
        const idx = pool.findIndex((e) => idN(e.id) >= untilN);
        const start = idx < 0 ? 0 : Math.max(0, idx - 2);
        slice = pool.slice(start);
        return json({ more: start > 0, entries: slice });
      }
      const start = Math.max(0, pool.length - pageSize);
      slice = pool.slice(start);
      return json({ more: start > 0, entries: slice });
    }
    if (p === "/api/super/search") {
      const rawQ = (q.get("q") || "").trim();
      const lim = Math.min(50, Math.max(1, parseInt(q.get("limit") || "30", 10) || 30));
      if (!rawQ) return json({ query: "", results: [], total: 0, has_more: false });
      const terms = [...new Set(rawQ.toLowerCase().split(/\s+/).filter(Boolean))];
      const all = [...earlierEntries, ...entries].filter((e) => e.role === "user" || e.role === "collie");
      const cleanOne = (e, withLinks) => {
        let t = String(e.text || "").replace(/```[^\n`]*\n?([\s\S]*?)(?:```|$)/g, " $1 ");
        if (!withLinks) t = t.replace(/\[image:[^\]]+\]/g, " ").replace(/\[([^\]\n]+)\]\([^)\n]+\)/g, "$1");
        else t = t.replace(/\[([^\]\n]+)\]\(([^)\n]+)\)/g, "$1 ($2)") + (e.options?.length ? " " + e.options.join(" / ") : "");
        return t.split("\n").map((l) => l.replace(/^\s*(?:[#>*•]+|-\s)\s*/, "").replace(/\*\*|__|`/g, "").trim()).filter(Boolean).join(" ").replace(/\s+/g, " ").trim();
      };
      const snipOf = (text) => {
        const low = text.toLowerCase();
        for (const tm of terms) if (!low.includes(tm)) return null;
        const i0 = low.indexOf(terms[0]);
        if (text.length <= 140) return text;
        let s = Math.max(0, i0 - 36), end = Math.min(text.length, s + 140);
        s = Math.max(0, end - 140);
        return (s > 0 ? "…" : "") + text.slice(s, end).trim() + (end < text.length ? "…" : "");
      };
      const hits = [];
      for (const e of all) {
        const sn = snipOf(cleanOne(e, false)) ?? snipOf(cleanOne(e, true));
        if (sn !== null) hits.push({ id: e.id, role: e.role, at: e.at, snippet: sn });
      }
      hits.sort((a, b) => new Date(b.at) - new Date(a.at));
      return json({ query: rawQ, results: hits.slice(0, lim), total: hits.length, has_more: hits.length > lim });
    }
    if (p === "/api/super/lark") return json({ ready: true, who: "Alex" });
    if (p === "/api/super/triggers") return json({ triggers, threads });
    if (p === "/api/ext/settings") { Object.assign(extData, { trigger: body.trigger ?? extData.trigger, footer: body.footer ?? extData.footer }); return json({ trigger: extData.trigger, footer: extData.footer }); }
    if (p === "/api/ext") return json(extData);
    if (p === "/api/ext-markets") return json({ markets: [{ repo: "owner/collie-extensions", name: "我的扩展", commit: "4f2c9a1d", has_token: true, installed: { notes: "1.0.0" },
      index: [{ id: "notes", version: "1.1.0", name: "笔记搜索", description: "让 agent 搜索和打开你自己的笔记服务" }, { id: "photos", version: "0.3.0", name: "相册", description: "按日期和地点找照片" }] }] });
    if ((m = p.match(/^\/api\/ext\/([^/]+)$/)) && method === "POST") {
      const v = extData.extensions.find((x) => x.manifest.id === m[1]);
      if (v) Object.assign(v.setting, body.enabled != null ? { enabled: body.enabled } : {}, body.machine != null ? { machine: body.machine } : {});
      return json({ setting: v?.setting, view: v });
    }
    if (/^\/api\/ext\/[^/]+\/login$/.test(p)) return json({ step: { done: true } });
    if (p === "/api/inbox") return json({ convs: inboxConvs, trigger: extData.trigger || "/ai" });
    if (p === "/api/inbox/conv" && method === "POST") {
      const c = inboxConvs.find((x) => x.id === body.id);
      if (c) for (const k of ["on", "anyone", "handler"]) if (body[k] != null) c[k] = body[k];
      if (c && body.seen) c.unread = 0;
      return json({ conv: c });
    }
    if (p === "/api/inbox/conv") {
      const c = inboxConvs.find((x) => x.id === q.get("id"));
      return json({ conv: c, msgs: c?.on ? inboxMsgs[c.id] || [] : [], outbox: {} });
    }
    if (p === "/api/super/timers") {
      if (method === "POST") timers.push({ id: "tm" + (++seq), spec: body.spec, text: body.text, target: body.target, enabled: true, next_run: later(60), owner: "you" });
      return json({ timers });
    }
    if (p === "/api/super/memory") {
      if (method === "POST" && body.path) {
        const f = memory.files.find((x) => x.name === body.path);
        if (f) { f.text = body.text; f.size = new Blob([body.text]).size; f.mod = new Date().toISOString(); }
        memory.up_front = memory.files.filter((x) => x.name === "instructions.md" || x.name === "notes/index.md" || /^tasks\/[^/]+\.md$/.test(x.name)).reduce((s, x) => s + x.size, 0);
        memory.messages = memory.files.filter((x) => x.name === "messages.md").reduce((s, x) => s + x.size, 0);
        memory.loaded = memory.up_front + memory.messages;
      }
      return json(memory);
    }
    if (p === "/api/super/say") return json({ ok: true, entry: collieSay(body.text || "", body.re) });
    if (p === "/api/wb") {
      if (method === "POST") { wb.groups = body.groups; wb.modules = body.modules.map((m) => ({ ...wb.modules.find((x) => x.id === m.id), ...m })); }
      return json(wb);
    }
    if (p === "/api/wb/token") { const m = wb.modules.find((x) => x.id === body.id); if (m) m.has_token = !!body.token; return json({ ok: true }); }
    if (p === "/api/wb/probe") return json({ embed: "tab", reason: "演示里不检测网站" });
    if (p.startsWith("/api/super/wb/read/")) {
      const m = wb.modules.find((x) => x.id === decodeURIComponent(p.slice(19)));
      return json({ id: m?.id, title: m?.title, text: `# 工作台 · ${m?.title}\n\n（演示数据）最近一周读完 2 本，在读 1 本。` });
    }
    if (p === "/api/external") return json({ sessions: external });
    if (p === "/api/history") return json({ sessions: history });
    if (p === "/api/marks") {
      if (method === "POST") {
        const m = { ...marks[body.key] };
        for (const k of ["unread", "archived", "title"]) if (body[k] !== undefined) m[k] = body[k];
        if (m.unread || m.archived || m.title) marks[body.key] = m; else delete marks[body.key];
      }
      return json({ marks });
    }
    if (/^\/api\/sessions\/[^/]+\/(rename|archive)$/.test(p)) return json({ ok: true });
    if (p === "/api/hubs") return json({ hubs: [] });
    if (p === "/api/version") return json({ version: "v0.2.41", latest: "v0.2.41", outdated: [] });
    if (p === "/api/local-app") return json({ url: "" });
    if (p === "/api/models") return json(models[q.get("kind")] || { models: [], efforts: [] });
    if (p === "/api/commands") {
      const b = (name, desc, hint) => ({ name, desc, hint, src: "builtin" });
      const common = [b("compact", "Free up context by summarizing the conversation so far", "[instructions]"), b("model", "Choose what model and reasoning effort to use"), b("review", "Review my current changes and find issues")];
      const skills = [["deploy", "把当前分支部署到预发环境并跑冒烟测试"], ["db-migrate", "生成并检查数据库迁移脚本"], ["release-notes", "根据合并的 PR 整理发布说明"]];
      const codex = q.get("kind") === "codex";
      return json({ kind: q.get("kind"), commands: common.concat(skills.map(([name, desc]) => ({ name, desc, src: "skill", ...(codex ? { sigil: "$" } : {}) }))) });
    }
    if (p === "/api/file") {
      const fp = q.get("path") || "", mach = q.get("machine") || "", dl = q.get("download") === "1";
      const dlHead = (name) => dl ? { "Content-Disposition": `attachment; filename="${name}"` } : {};
      const demoImgs = [
        [/a1b2c3-ci-failure\.png$/i, "../img/screen.webp", ""],
        [/d4e5f6-query-plan\.png$/i, "../img/agent.webp", ""],
        [/images\/0d3a9e5c71b2f4e8a6c1\.png$/i, "../img/hero-dark.webp", "macbook"],
        [/preview-dark\.png$/i, "../img/hero-dark.webp", "macbook"],
        [/preview-light\.png$/i, "../img/hero-light.webp", "macbook"],
        [/architecture\.svg$/i, "../img/architecture-light.svg", "macbook"],
        [/uploads\/.*\.png$/i, "../img/hero-dark.webp", ""],
      ];
      const hitImg = demoImgs.find(([re, , wantM]) => re.test(fp) && (!wantM || mach === wantM));
      if (hitImg) {
        const b = await (await realFetch(new URL(hitImg[1], location.href))).blob();
        const baseName = fp.split("/").pop() || "image";
        const downloadName = b.type === "image/webp" ? baseName.replace(/\.[^.]+$/, "") + ".webp" : baseName;
        return new Response(b, { headers: { "X-File-Kind": dl ? "download" : "image", "X-File-Size": String(b.size), "X-File-Path": encodeURIComponent(fp), ...dlHead(downloadName) } });
      }
      if (/bench-pagination\.html$/i.test(fp)) {
        const html = await (await realFetch(new URL("bench-pagination.html", location.href))).text();
        const isHtml = !dl && q.get("as") === "html";
        return new Response(html, { headers: { "Content-Type": isHtml ? "text/html; charset=utf-8" : "application/octet-stream", "X-File-Kind": dl ? "download" : (isHtml ? "html" : "text"), "X-File-Size": String(new Blob([html]).size), "X-File-Path": encodeURIComponent("~/code/shop-api/bench-pagination.html"), ...dlHead("bench-pagination.html") } });
      }
      if (!mach && /perf-audit-v1\.4\.md$/.test(fp)) {
        return new Response(perfAuditMd, { headers: { "X-File-Kind": dl ? "download" : "text", "X-File-Size": String(new Blob([perfAuditMd]).size), "X-File-Path": encodeURIComponent("~/code/shop-api/docs/perf-audit-v1.4.md"), ...dlHead("perf-audit-v1.4.md") } });
      }
      if (mach === "macbook" && /CHANGELOG\.md$/.test(fp)) {
        return new Response(changelog, { headers: { "X-File-Kind": dl ? "download" : "text", "X-File-Size": String(new Blob([changelog]).size), "X-File-Path": encodeURIComponent("~/code/shop-web/CHANGELOG.md"), ...dlHead("CHANGELOG.md") } });
      }
      if (!mach && /(?:internal\/orders\/)?list\.go$/.test(fp)) {
        return new Response(ordersListGo, { headers: { "X-File-Kind": dl ? "download" : "text", "X-File-Size": String(new Blob([ordersListGo]).size), "X-File-Path": encodeURIComponent("~/code/shop-api/internal/orders/list.go"), ...dlHead("list.go") } });
      }
      return json({ error: "找不到这个文件" }, 404);
    }
    if (p === "/api/dirs") return json({ dirs: ["~/code/shop-api", "~/code/shop-web", "~/code/notes"] });
    if (p === "/api/sysmon") return json(demoSysmon(q.get("machine") || "", Number(q.get("since") || 0)));
    if (p === "/api/devices") return json({
      devices: [
        { id: "macbook", kind: "machine", name: "macbook", os: "darwin/arm64", online: true, home: "/Users/alex", version: "v0.2.41", agents: ["claude", "codex"], created_at: ago(60 * 24 * 3), last_seen: ago(0) },
        { id: "studio", kind: "machine", name: "studio", os: "darwin/arm64", online: false, home: "/Users/alex", version: "v0.2.41", agents: ["claude"], created_at: ago(60 * 24 * 9), last_seen: ago(60 * 26) },
        { id: "iphone", kind: "browser", name: "iPhone · Safari", current: false, created_at: ago(60 * 24 * 3), last_seen: ago(12) },
        { id: "chrome", kind: "browser", name: "Mac · Chrome", current: true, created_at: ago(60 * 24 * 2), last_seen: ago(0) },
      ],
      hub: { name: "devbox", home: "/home/alex", herdr: true, version: "v0.2.41", agents: ["claude", "codex", "agy"] }, addresses: ["http://192.168.1.20:7788"], latest: "v0.2.41",
    });
    if (p === "/api/pair/new") return json({ code: "DEMO2026", expires: new Date(Date.now() + 600000).toISOString(), kind: body.kind });
    if ((m = /^\/api\/panes\/([^/]+)\/ask$/.exec(p))) { const id = decodeURIComponent(m[1]); return json(approvals[id] && agents.find((a) => a.pane_id === id)?.status === "blocked" ? { asking: true, approval: approvals[id] } : { asking: false }); }
    if ((m = /^\/api\/panes\/([^/]+)\/model$/.exec(p))) {
      const a = agents.find((x) => x.pane_id === decodeURIComponent(m[1]));
      if (a) a.run = [body.model || "", body.effort || ""];
      return json({ ok: true });
    }
    if ((m = /^\/api\/panes\/([^/]+)\/(keys|text)$/.exec(p))) {
      const id = decodeURIComponent(m[1]);
      if (approvals[id]) answerApproval(id); else if (m[2] === "text" && body.text) promptPane(id, body.text);
      return json({ ok: true });
    }
    if ((m = /^\/api\/agents\/([^/]+)\/prompt$/.exec(p))) {
      const t = decodeURIComponent(m[1]), id = agents.find((a) => a.pane_id === t || a.name === t)?.pane_id || t;
      promptPane(id, body.text || body.prompt || ""); return json({ ok: true });
    }
    if ((m = /^\/api\/panes\/([^/]+)\/close$/.exec(p))) {
      const i = shells.findIndex((x) => x.pane_id === decodeURIComponent(m[1]));
      if (i >= 0) shells.splice(i, 1);
      pushAll(); return json({ ok: true });
    }
    if ((m = /^\/api\/panes\/([^/]+)\/(earlier|last)$/.exec(p))) return json({ messages: [], earlier: 0, text: "" });
    if (p === "/api/agents" && method === "POST") {
      const n = agents.length + 10, pane = `w${n}:p1${body.machine ? "@" + body.machine : ""}`;
      const kind = body.kind === "antigravity" ? "agy" : body.kind;
      agents.push(agent({ pane_id: pane, workspace_id: pane.split(":")[0], name: body.name || `${kind}-${n}`, kind, status: body.prompt ? "working" : "idle", cwd: (body.cwd || "~/code").replace(/^~/, body.machine ? "/Users/alex" : "/home/alex"), run: [body.model || "", body.effort || ""] }));
      chats[pane] = [];
      if (body.prompt) promptPane(pane, body.prompt); else pushAll();
      return json({ pane_id: pane, workspace_id: pane.split(":")[0] });
    }
    if (method === "POST") return json({ ok: true });
    return json({});
  };

  // The demo opens light unless this visitor picked a theme in 偏好设置.
  try { if (localStorage.getItem("collie.theme") === null) { localStorage.setItem("collie.theme", JSON.stringify("light")); document.documentElement.dataset.theme = "light"; } } catch {}

  // A demo has nothing to notify about: never ask for the permission.
  try { if (window.Notification) Object.defineProperty(Notification, "permission", { get: () => "denied" }); Notification.requestPermission = async () => "denied"; } catch {}

  // ───────────────────────────── a quiet badge: this is a demo
  if (!/[?&]clean\b/.test(location.search)) {
    addEventListener("DOMContentLoaded", () => {
      const b = document.createElement("a");
      b.href = "../"; b.className = "demo-badge"; b.textContent = "演示 · 数据是虚构的 · 返回主页";
      document.body.append(b);
    });
  }
})();

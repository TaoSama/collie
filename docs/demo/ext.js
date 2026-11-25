// Extensions (extensions/README.md): the 接入 page's cards, the 收件箱 of
// their conversations, and what became of each answer sent back. Only
// drawing lives here; app.js fetches and wires the clicks.
(() => {
  let esc = (s) => String(s ?? "");
  let when = (t) => String(t ?? "");
  let md = (s) => esc(s);
  let workerHref = () => "";

  const PROC = { installing: "安装中", starting: "启动中", running: "运行中", error: "出错", stopped: "已停止" };
  const OUT = {
    pending: ["发送中", ""], sent: ["已发出", "ok"], deferred: ["暂缓，稍后送达", "attn"],
    uncertain: ["可能已发出，为避免重复没有重发，去原应用确认", "bad"], failed: ["没发出", "bad"], expired: ["已过期，没发出", "bad"],
  };
  const KIND = { dm: "私聊", group: "群", self: "和自己" };

  function init(h) { esc = h.esc; when = h.when; md = h.md; workerHref = h.workerHref; }

  // Where an extension is, in four words (and what to do next).
  const declarative = (m) => !m.runtime?.command && !m.runtime?.builtin && !m.contributes?.connector?.id;
  const needsLogin = (m) => !!m.contributes?.connector?.login;
  function stateOf(v) {
    const m = v.manifest;
    if (!v.setting.enabled) return { key: "off", dot: "asleep", label: "未开启", text: "未开启" };
    if (declarative(m)) return { key: "on", dot: "idle", label: "已开启", text: "已开启（不需要运行程序）", ok: true };
    if (v.problem && !v.proc) return { key: "error", dot: "blocked", label: "出错", text: "连不上它所在的电脑：" + v.problem };
    const p = v.proc || { state: "starting" };
    if (p.state === "error") return { key: "error", dot: "blocked", label: "出错", text: p.error || "启动失败" };
    if (p.state !== "running") {
      const busy = p.state === "installing" ? "正在安装，第一次大约要 1–2 分钟" : "正在启动";
      return { key: needsLogin(m) ? "login" : "starting", dot: "starting", label: needsLogin(m) ? "未登录" : "启动中", text: busy + "…", busy: true };
    }
    const s = v.status;
    if (!s) return { key: "starting", dot: "starting", label: "启动中", text: "正在读取状态…", busy: true };
    if (s.state === "ok") return { key: "on", dot: "idle", label: "运行中", text: needsLogin(m) ? "已登录" + (s.account ? " " + s.account : "") : "运行中", ok: true };
    if (s.state === "not_signed_in") return { key: "login", dot: "blocked", label: "未登录", text: "未登录", login: true };
    // Just started, an extension may say "error" while it prepares (a first
    // start downloads what it needs): that is starting, not broken.
    if (p.since && Date.now() - Date.parse(p.since) < 5 * 60e3) return { key: needsLogin(m) ? "login" : "starting", dot: "starting", label: needsLogin(m) ? "未登录" : "启动中", text: s.error || "正在启动…", busy: true };
    return { key: "error", dot: "blocked", label: "出错", text: s.error || "出错" };
  }
  const extIcon = (m) => `<span class="ext-icon" aria-hidden="true">${esc((m.label || m.name || m.id).slice(0, 1))}</span>`;
  const loginWord = (m) => m.contributes?.connector?.login === "secret" ? "填写密钥" : "登录" + (m.label || "");

  function machineOptions(ms, cur, pick) {
    return (pick ? `<option value="" disabled selected>选一台电脑</option>` : "") +
      ms.map((m) => `<option value="${esc(m.id)}" ${!pick && m.id === cur ? "selected" : ""}>${esc(m.name)}${m.online ? "" : "（离线）"}</option>`).join("");
  }
  const machineName = (ms, id) => (ms.find((x) => x.id === (id || "")) || {}).name || id || "hub";

  // The actions it offers agents (collie ext do), and the switch for the
  // ones that change something.
  function actionsHTML(v) {
    const acts = v.manifest.contributes?.connector?.actions || [];
    if (!acts.length) return "";
    const writes = acts.some((a) => !a.readonly);
    return `<div class="card"><div class="card-h">给 agent 的动作</div><div class="form">
      <p class="ext-desc">agent 用 <code>collie ext do ${esc(v.manifest.id)} &lt;动作&gt;</code> 调用。</p>
      <ul class="ext-actions">${acts.map((a) => `<li><code>${esc(a.name)}</code>${a.readonly ? "" : ` <span class="tag attn">会改动</span>`}${a.untrusted ? ` <span class="tag">第三方内容</span>` : ""}${a.slow ? ` <span class="tag">慢</span>` : ""} <span class="muted">${esc(a.description)}</span></li>`).join("")}</ul>
      ${writes ? `<div class="conv-rule"><button type="button" class="switch" role="switch" aria-checked="${!v.setting.no_writes}" aria-label="允许会改动的动作" data-ext-writes></button><span>允许会改动的动作</span></div>` : ""}</div></div>`;
  }

  function configFields(v) {
    const props = v.manifest.configuration?.properties || {};
    return Object.entries(props).map(([k, p]) => {
      const val = v.setting.config?.[k] ?? p.default;
      if (p["x-secret"]) {
        const isSet = (v.secrets || []).includes(k);
        return `<label>${esc(p.title || k)}<input type="password" autocomplete="off" data-ext-config="${esc(k)}" placeholder="${isSet ? "已设置，留空不改" : "未设置"}">${p.description ? `<small>${esc(p.description)}</small>` : ""}</label>`;
      }
      if (p.type === "boolean") return `<label class="check"><input type="checkbox" data-ext-config="${esc(k)}" ${val ? "checked" : ""}><span>${esc(p.title)}</span></label>`;
      if (p.enum) return `<label>${esc(p.title)}<select data-ext-config="${esc(k)}" data-pick="compact">${p.enum.map((e, i) => `<option value="${esc(e)}" ${e === val ? "selected" : ""}>${esc(p["x-labels"]?.[i] || e)}</option>`).join("")}</select>${p.description ? `<small>${esc(p.description)}</small>` : ""}</label>`;
      return `<label>${esc(p.title)}<input data-ext-config="${esc(k)}" value="${esc(val ?? "")}">${p.description ? `<small>${esc(p.description)}</small>` : ""}</label>`;
    }).join("");
  }

  function detailLine(s) {
    const d = s?.detail;
    if (!d) return "";
    const parts = [];
    if (d.window_left) parts.push("回复窗口还剩 " + d.window_left);
    if (d.sends_left != null) parts.push("可发 " + d.sends_left + " 条");
    if (d.phase) parts.push(d.phase);
    return parts.length ? `<div class="ext-detail">${esc(parts.join(" · "))}</div>` : "";
  }

  // The sign-in in progress: a QR code to scan, or a key to paste.
  function loginPanel(m, qr) {
    if (!qr || qr.id !== m.id) return "";
    if (qr.waiting) return `<div class="ext-login"><span class="spinner" aria-hidden="true"></span><div><b>正在准备登录</b><p class="muted">扩展装好、启动后会自动出现${m.contributes?.connector?.login === "secret" ? "密钥输入框" : "二维码"}。第一次大约要 1–2 分钟。</p></div><button type="button" class="btn small ghost" data-ext-login-cancel>取消</button></div>`;
    if (qr.kind === "secret") return `<form class="ext-login ext-secret" data-ext-secret="${esc(qr.step || "")}"><label>${esc(qr.label || "密钥")}<input type="password" name="secret" autocomplete="off" required><small>${esc(qr.hint || "")} 提交后由扩展自己保存，Collie 不显示也不记录。</small></label>${qr.error ? `<p class="ext-warn">${esc(qr.error)}</p>` : ""}<div class="form-actions"><button type="button" class="btn small ghost" data-ext-login-cancel>取消</button><button class="btn primary small">提交</button></div></form>`;
    return `<div class="ext-login ext-qr">${qr.img ? `<img src="${esc(qr.img)}" alt="登录二维码">` : `<span class="spinner" aria-hidden="true"></span>`}<div><b>${qr.done ? "登录成功" : `用${esc(m.label || "手机")} App 扫码并确认`}</b><p class="muted">${esc(qr.hint || (qr.done ? "" : "确认后这里会自动继续，二维码过期会自动刷新。"))}</p>${qr.error ? `<p class="ext-warn">${esc(qr.error)}</p>` : ""}${qr.done ? "" : `<button type="button" class="btn small ghost" data-ext-login-cancel>取消</button>`}</div></div>`;
  }

  // 接入: one line per extension, then the shared settings, the
  // marketplaces and the webhooks, each a page of its own.
  function hooksList(r, inbox, markets) {
    const list = r?.extensions || [], ms = r?.machines || [];
    const convs = inbox?.convs || [];
    const row = (v) => {
      const m = v.manifest, st = stateOf(v);
      const n = convs.filter((c) => c.ext === m.id), on = n.filter((c) => c.on).length;
      const sub = [m.market ? "市场" : m.local ? "本地" : "内置", n.length ? `${n.length} 个会话${on ? `，${on} 个处理中` : ""}` : "",
        (m.contributes?.connector?.actions || []).length ? `${m.contributes.connector.actions.length} 个动作` : ""].filter(Boolean).join(" · ");
      return `<a class="row ext-row" href="#/auto/hooks/ext/${encodeURIComponent(m.id)}">${extIcon(m)}
        <div><div class="meta"><b>${esc(m.name)}</b></div><div class="text">${esc(sub)}</div></div>
        <span class="right ext-row-state"><span class="dot ${esc(st.dot)}"></span>${esc(st.label)}${v.setting.enabled && !declarative(m) ? `<span class="muted"> · ${esc(machineName(ms, v.setting.machine))}</span>` : ""}<span class="chev" aria-hidden="true">›</span></span></a>`;
    };
    const nav = (href, title, sub) => `<a class="row ext-row" href="${href}"><span class="ext-icon ghost" aria-hidden="true"></span><div><div class="meta"><b>${title}</b></div><div class="text">${esc(sub)}</div></div><span class="right"><span class="chev" aria-hidden="true">›</span></span></a>`;
    const updates = (markets?.markets || []).reduce((n, mk) => n + (mk.index || []).filter((e) => mk.installed?.[e.id] && mk.installed[e.id] !== e.version).length, 0);
    return `<p class="panel-intro">扩展让 Collie 收发飞书、微信消息，也给 agent 提供动作。都默认关闭：点进去开启并登录后才生效。</p>
      <div class="card"><div class="card-h">扩展</div><div class="rows">${list.map(row).join("") || `<div class="empty-row">没有可用的扩展</div>`}</div></div>
      <div class="card"><div class="rows">
        ${nav("#/auto/hooks/reply", "唤起与回复", `触发词 ${r?.trigger || r?.default_trigger || "/ai"} · 回复尾标`)}
        ${nav("#/auto/hooks/markets", "扩展市场", `${(markets?.markets || []).length} 个市场${updates ? ` · ${updates} 个可更新` : ""}`)}
      </div></div>`;
  }

  const pageHead = (title, sub = "") => `<div class="sub-head"><a class="sub-back" href="#/auto" data-sub-back aria-label="返回接入">‹ 接入</a><h3>${title}</h3>${sub}</div>`;

  // One extension's page: its state and the one thing to do next on top.
  function extPage(v, r, qr, inbox) {
    if (!v) return pageHead("扩展") + `<div class="card"><div class="empty-row">没有这个扩展</div></div>`;
    const m = v.manifest, st = stateOf(v), ms = r?.machines || [], on = v.setting.enabled;
    const ask = m.defaults?.machine === "ask" && !v.setting.chosen;
    const primary = st.key === "off" ? (needsLogin(m) ? `<button class="btn primary" data-ext-login>${esc(loginWord(m))}</button>` : `<button class="btn primary" data-ext-enable>开启</button>`)
      : st.login && !(qr && qr.id === m.id) ? `<button class="btn primary" data-ext-login>${esc(loginWord(m))}</button>` : "";
    const convs = (inbox?.convs || []).filter((c) => c.ext === m.id);
    const machine = declarative(m) ? "" : m.local
      ? `<div class="ext-field"><span class="tm-lab">运行在</span><span>${esc(machineName(ms, m.local_on))}<small class="muted">本地扩展，在找到它的那台电脑上运行</small></span></div>`
      : `<label class="ext-field"><span class="tm-lab">运行在</span><select data-ext-machine data-pick="compact">${machineOptions(ms, v.setting.machine || "", ask)}</select></label>
         <small class="muted ext-field-note">${ask ? "它的后端只在某一台电脑上：先选好再开启。" : ""}换电脑会先停掉原来那台上的，登录状态留在原来那台。</small>`;
    return `${pageHead(esc(m.name), `<span class="muted">${esc(m.version)} · ${m.market ? "市场" : m.local ? "本地" : "内置"}</span>`)}
      <div class="card ext-card" data-ext="${esc(m.id)}"><div class="form">
        <div class="ext-status"><span class="dot ${esc(st.dot)}"></span><div><b>${esc(st.text)}</b>${st.busy ? `<span class="spinner small" aria-hidden="true"></span>` : ""}${detailLine(v.status)}</div>${primary}</div>
        ${loginPanel(m, qr)}
        ${machine}
      </div></div>
      ${m.configuration ? `<div class="card" data-ext="${esc(m.id)}"><div class="card-h">设置</div><div class="form"><div class="set-grid">${configFields(v)}</div><div class="form-actions"><button class="btn small" data-ext-save>保存设置</button></div></div></div>` : ""}
      <div data-ext="${esc(m.id)}">${actionsHTML(v)}</div>
      ${m.contributes?.connector?.capabilities?.some((c) => c === "archive" || c === "receive") ? `<div class="card"><div class="rows"><a class="row ext-row" href="#/auto" data-auto-tab="inbox"><span class="ext-icon ghost" aria-hidden="true"></span><div><div class="meta"><b>会话</b></div><div class="text">${convs.length} 个会话${convs.filter((c) => c.on).length ? `，${convs.filter((c) => c.on).length} 个处理中` : ""}，在收件箱里打开要处理的</div></div><span class="right"><span class="chev" aria-hidden="true">›</span></span></a></div></div>` : ""}
      <div class="card"><div class="card-h">说明</div><div class="form">
        <p class="ext-desc">${esc(m.description)}</p>
        ${m.warning ? `<p class="ext-warn">${esc(m.warning)}</p>` : ""}
        ${m.market ? `<p class="ext-desc">来自市场 ${esc(m.market.repo)}（${esc((m.market.commit || "").slice(0, 7))}）</p>` : ""}
        ${m.local ? `<p class="ext-warn">来自本地目录（${esc(m.id)}/pkg/${esc(m.version)}），Collie 不校验其内容。</p>` : ""}
        ${(m.permissions?.network || []).length ? `<p class="ext-desc">会访问：${esc(m.permissions.network.join("、"))}</p>` : ""}
      </div></div>
      ${v.proc?.logs?.length ? `<div class="card"><details class="ext-logs"><summary class="card-h">最近日志</summary><pre>${esc(v.proc.logs.join("\n"))}</pre></details></div>` : ""}
      <div class="card" data-ext="${esc(m.id)}"><div class="form ext-danger">
        ${on && st.ok && needsLogin(m) ? `<button type="button" class="link-danger" data-ext-logout>退出登录</button>` : ""}
        ${on ? `<button type="button" class="link-danger" data-ext-disable>关闭扩展</button>` : `<button type="button" class="btn small" data-ext-enable>开启</button>`}
        ${m.market ? `<button type="button" class="link-danger" data-ext-uninstall>卸载</button>` : ""}
        <small class="muted">关闭后不再收发，数据和登录状态保留；退出登录会删掉登录状态。</small>
      </div></div>`;
  }

  function replyPage(r) {
    return `${pageHead("唤起与回复")}
      <div class="card"><form class="form" id="ext-settings-form">
        <div class="set-grid">
          <label>触发词<input name="trigger" value="${esc(r?.trigger || "")}" placeholder="${esc(r?.default_trigger)}" autocorrect="off" autocapitalize="off" spellcheck="false"><small>打开处理的会话里，以它开头的消息才交给 agent；留空用 ${esc(r?.default_trigger)}。微信的绑定人不需要</small></label>
          <label class="span2">回复尾标<input name="footer" value="${esc(r?.footer || "")}" placeholder="${esc(r?.default_footer)}" autocorrect="off" autocapitalize="off" spellcheck="false">
            <small>留空用默认。可用：{agent} {model} {effort} {elapsed} 耗时 {machine} 电脑 {worker} {ctx} 上下文 {time} {by} 谁唤起的。尾标也用来认出 Collie 自己发的消息</small></label>
        </div>
        <div class="form-actions"><button class="btn primary small">保存</button></div>
      </form></div>`;
  }

  // 扩展市场: the repositories extensions are installed from.
  function marketsHTML(mr) {
    const ms = mr?.markets || [];
    const entry = (m, e) => {
      const have = m.installed?.[e.id];
      const act = !have ? `<button class="btn small" data-market-install="${esc(e.id)}">安装</button>`
        : have !== e.version ? `<button class="btn small primary" data-market-install="${esc(e.id)}">更新到 ${esc(e.version)}</button>` : `<span class="muted">已安装</span>`;
      return `<li class="row"><span></span><div><div class="meta"><b>${esc(e.name || e.id)}</b><span class="mono">${esc(e.id)} ${esc(e.version)}</span>${have && have !== e.version ? `<span>已装 ${esc(have)}</span>` : ""}</div><div class="text">${esc(e.description || "")}</div></div><span class="right">${act}</span></li>`;
    };
    return `${pageHead("扩展市场")}<div class="card"><div class="card-h">市场<button class="btn small" data-market-refresh>刷新</button></div>
      <div class="form"><p class="ext-desc">自带的只有飞书和微信，其他扩展从市场安装。市场是一个 GitHub 仓库，根目录放 collie-marketplace.json（格式见 extensions/README.md）。私有仓库要填一个能读它的 token，只保存在 hub 上，之后不再显示。</p></div>
      ${ms.map((m) => `<div class="market" data-market="${esc(m.repo)}"><div class="market-h"><b>${esc(m.name || m.repo)}</b><span class="muted mono">${esc(m.repo)}${m.ref ? "@" + esc(m.ref) : ""}${m.commit ? " · " + esc(m.commit.slice(0, 7)) : ""}${m.has_token ? " · 有 token" : ""}</span><button class="btn small ghost" data-market-remove>移除</button></div>
        ${m.error ? `<p class="ext-warn">${esc(m.error)}</p>` : ""}
        <ul class="rows">${(m.index || []).map((e) => entry(m, e)).join("") || `<li class="empty-row">这个市场里还没有扩展</li>`}</ul></div>`).join("")}
      <form class="form" id="market-form"><div class="set-grid">
        <label>仓库<input name="repo" placeholder="owner/name" autocorrect="off" autocapitalize="off" spellcheck="false" required></label>
        <label>分支或标签<input name="ref" placeholder="默认分支" autocorrect="off" autocapitalize="off" spellcheck="false"></label>
        <label class="span2">Token（私有仓库需要）<input name="token" type="password" autocomplete="off" placeholder="留空不改"></label>
      </div><div class="form-actions"><button class="btn primary small">添加或更新市场</button></div></form></div>`;
  }

  function outLine(o) {
    if (!o || o.state === "sent") return "";
    const [text, tone] = OUT[o.state] || [o.state, ""];
    return `<span class="ext-out ${tone}">${esc(text)}${o.reason && o.state !== "pending" ? "：" + esc(o.reason) : ""}</span>`;
  }

  // 收件箱: every chat of every extension; the open one beside it.
  function inboxHTML(r, cur, detail, filter) {
    const convs = r?.convs || [];
    const shown = convs.filter((c) => filter === "on" ? c.on : filter && filter !== "all" ? c.ext === filter : true);
    const exts = [...new Set(convs.map((c) => c.ext))];
    const chips = [["all", "全部"], ["on", `处理中 ${convs.filter((c) => c.on).length}`], ...exts.map((x) => [x, convs.find((c) => c.ext === x)?.label || x])]
      .map(([k, l]) => `<button type="button" data-inbox-filter="${esc(k)}" class="${(filter || "all") === k ? "on" : ""}">${esc(l)}</button>`).join("");
    const row = (c) => `<li class="row inbox-row ${c.id === cur ? "sel" : ""}" data-conv="${esc(c.id)}">
        <span class="dot ${c.on ? "idle" : "asleep"}" title="${c.on ? "处理中" : "不处理"}"></span>
        <div><div class="meta"><b>${esc(c.name || c.chat)}</b><span>${esc(c.label)} · ${esc(KIND[c.kind] || c.kind || "")}</span>${c.unread ? `<span class="inbox-unread">${c.unread}</span>` : ""}</div>
          <div class="text">${esc(c.preview || "")}</div>${c.on ? outLine(c.out) : ""}</div>
        <span class="right">${when(c.last_at)}</span></li>`;
    const intro = `<p class="panel-intro">打开的扩展收到的会话都在这里。你本人发的、以触发词 <b>${esc(r?.trigger || "/ai")}</b> 开头的消息，在任何会话都会交给 Collie 或这个会话的专属 agent，回复带尾标发回原会话；会话开关只管别人的消息。没打开的会话只留最后一条预览。</p>`;
    if (!convs.length) return intro + `<div class="card"><div class="empty-row">还没有会话。先在 <a href="#/auto" data-auto-tab="hooks">接入</a> 打开一个扩展并登录</div></div>`;
    return intro + `<div class="seg inbox-filter">${chips}</div>
      <div class="inbox-split ${cur ? "has-cur" : ""}">
        <div class="card inbox-list"><ul class="rows">${shown.map(row).join("") || `<li class="empty-row">没有</li>`}</ul></div>
        <div class="inbox-detail" id="inbox-detail">${detailHTML(cur, detail, r?.trigger)}</div>
      </div>`;
  }

  const detailHTML = (cur, detail, trigger) => cur ? convHTML(detail, trigger) : `<div class="card"><div class="empty-row">选一个会话</div></div>`;

  function convHTML(d, trigger) {
    if (!d) return `<div class="card"><div class="empty-row">正在读取…</div></div>`;
    const c = d.conv, msgs = d.msgs || [], outs = d.outbox || {};
    const seg = (name, val, opts) => `<div class="seg">${opts.map(([v, l]) => `<button type="button" data-conv-set="${name}" data-v="${v}" class="${String(val) === String(v) ? "on" : ""}">${l}</button>`).join("")}</div>`;
    const href = c.worker ? workerHref(c.worker) : "";
    const worker = c.worker ? (href ? `<a href="${esc(href)}">${esc(c.worker)} →</a>` : esc(c.worker)) : "";
    const msg = (m) => {
      const o = m.ref ? outs[m.ref] : null;
      return `<li class="conv-msg ${m.mine ? "mine" : ""}"><div class="meta"><b>${esc(m.mine ? "你" : m.sender_name || "")}</b><span>${when(m.at)}</span>${o ? outLine(o) : ""}</div>
        <div class="msg ${m.ref ? "assistant" : ""}">${m.ref ? md(m.text) : esc(m.text)}</div></li>`;
    };
    return `<div class="card conv-card">
      <div class="card-h"><span>${esc(c.name || c.chat)}</span><button type="button" class="icon-btn conv-close" data-conv-close title="返回">×</button></div>
      <div class="form">
        <div class="conv-rule"><button type="button" class="switch" role="switch" aria-checked="${!!c.on}" aria-label="处理这个会话" data-conv-on></button><span><b>处理这个会话</b>${c.on ? "" : `<small class="muted">打开后，以 ${esc(trigger || "/ai")} 开头的消息交给 agent，并开始保存这个会话的消息</small>`}</span></div>
        ${c.on ? `<div class="conv-rule"><span class="tm-lab">谁能唤起</span>${seg("anyone", !!c.anyone, [[false, "只有我"], [true, "任何人"]])}</div>
          ${c.anyone ? `<p class="ext-warn">任何人都能唤起时，专属 agent 在隔离目录里工作，但那不是沙箱：只对信任的群打开。</p>` : ""}
          <div class="conv-rule"><span class="tm-lab">交给</span>${seg("handler", c.handler, [["collie", "Collie"], ["agent", "专属 agent"]])}${c.handler === "agent" ? `<span class="muted">${worker || "第一次唤起时启动"}</span>` : ""}</div>` : ""}
      </div>
      ${c.on ? `<ul class="conv-msgs">${msgs.map(msg).join("") || `<li class="empty-row">打开以后的消息会出现在这里</li>`}</ul>` : `<div class="empty-row">${esc(c.preview || "")}</div>`}
    </div>`;
  }

  // The timer form's 发到: Collie, or an open conversation.
  function timerChannels(r) {
    return (r?.convs || []).filter((c) => c.on).map((c) => `<option value="ext:${esc(c.id)}" data-sub="${esc(c.label)}">${esc(c.label)} · ${esc(c.name || c.chat)}</option>`).join("");
  }

  // Short names of extensions, from what the pages fetched (manifest label).
  const labels = {};
  function learn(r) {
    for (const v of r?.extensions || []) labels[v.manifest.id] = v.manifest.label || v.manifest.name;
    for (const c of r?.convs || []) labels[c.ext] = c.label;
  }
  const label = (source) => source?.startsWith("ext:") ? labels[source.slice(4)] || source.slice(4) : "";

  window.collieExt = { init, hooksList, extPage, replyPage, marketsHTML, pageHead, inboxHTML, convHTML, detailHTML, timerChannels, label, learn, stateOf };
})();

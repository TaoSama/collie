// Extensions (extensions/README.md): the 接入 page's cards, the 收件箱 of
// their conversations, and what became of each answer sent back. Only
// drawing lives here; app.js fetches and wires the clicks.
(() => {
  let esc = (s) => String(s ?? "");
  let when = (t) => String(t ?? "");
  let md = (s) => esc(s);
  let workerHref = () => "";

  const AI_HANDLED_LABEL = "已交给 AI";
  const PROC = { installing: "安装中", starting: "启动中", running: "运行中", error: "出错", stopped: "已停止" };
  const OUT = {
    pending: ["发送中", ""], sent: ["已发出", "ok"], deferred: ["暂缓，稍后送达", "attn"],
    uncertain: ["可能已发出，为避免重复没有重发，去原应用确认", "bad"], failed: ["没发出", "bad"], expired: ["已过期，没发出", "bad"],
  };
  const KIND = { dm: "私聊", group: "群", self: "和自己", doc: "文档评论" };
  const KIND_ICON = { dm: "user", group: "users", self: "user", doc: "file-text" };
  const { svg: ico, title: cardTitle } = window.collieIcons;
  // A line icon on the same tile as an extension's logo, for the rows
  // beside them that lead somewhere else.
  const symTile = (name) => `<span class="ext-icon sym" aria-hidden="true">${ico(name)}</span>`;

  function init(h) { esc = h.esc; when = h.when; md = h.md; workerHref = h.workerHref; }

  // Where an extension is, in four words (and what to do next).
  const declarative = (m) => !m.runtime?.command && !m.runtime?.builtin && !m.contributes?.connector?.id;
  const needsLogin = (m) => !!m.contributes?.connector?.login;
  function stateOf(v) {
    const m = v.manifest;
    if (!v.setting.enabled) return { key: "off", dot: "asleep", label: "未开启", text: "未开启" };
    if (declarative(m)) return { key: "on", dot: "online", label: "已开启", text: "已开启（不需要运行程序）", ok: true };
    if (v.problem && !v.proc) return { key: "error", dot: "blocked", label: "出错", text: "连不上它所在的电脑：" + v.problem };
    const p = v.proc || { state: "starting" };
    if (p.state === "error") return { key: "error", dot: "blocked", label: "出错", text: p.error || "启动失败" };
    if (p.state !== "running") {
      const busy = p.state === "installing" ? "正在安装，第一次大约要 1–2 分钟" : "正在启动";
      return { key: needsLogin(m) ? "login" : "starting", dot: "starting", label: needsLogin(m) ? "未登录" : "启动中", text: busy + "…", busy: true };
    }
    const s = v.status;
    if (!s) return { key: "starting", dot: "starting", label: "启动中", text: "正在读取状态…", busy: true };
    if (s.state === "ok") return { key: "on", dot: "online", label: "运行中", text: needsLogin(m) ? "已登录" + (s.account ? " · " + s.account : "") : "运行中", title: s.account_id || "", ok: true };
    if (s.state === "not_signed_in") return { key: "login", dot: "blocked", label: "未登录", text: "未登录", login: true };
    // Just started, an extension may say "error" while it prepares (a first
    // start downloads what it needs): that is starting, not broken.
    if (p.since && Date.now() - Date.parse(p.since) < 5 * 60e3) return { key: needsLogin(m) ? "login" : "starting", dot: "starting", label: needsLogin(m) ? "未登录" : "启动中", text: s.error || "正在启动…", busy: true };
    return { key: "error", dot: "blocked", label: "出错", text: s.error || "出错" };
  }
  // The state as a coloured pill (app.css .pill): running green, needs you
  // amber, broken red, getting ready blue, off grey.
  const TONE_OF = { on: "ok", login: "attn", error: "bad", starting: "info busy", off: "off" };
  const statePill = (st) => `<span class="pill ${TONE_OF[st.key] || "off"}">${esc(st.label)}</span>`;
  // Its logo (or first letter) on a tile that says how it is: faded when
  // off, ringed when it needs you.
  const TONE = { asleep: "off", blocked: "bad" };
  const extIcon = (m, st, big) => `<span class="ext-icon ${TONE[st?.dot] || ""}${m.icon_url ? " logo" : ""}${big ? " big" : ""}" aria-hidden="true">${m.icon_url ? `<img src="${esc(m.icon_url)}" alt="">` : esc((m.label || m.name || m.id).slice(0, 1))}</span>`;
  const loginWord = (m) => m.contributes?.connector?.login === "secret" ? "填写密钥" : "登录" + (m.label || "");

  // Until one is chosen, none is picked; drawn as every computer picker (machines.js).
  function machineOptions(ms, cur, pick) {
    const M = window.collieMachines;
    return (pick ? `<option value="" disabled selected>选一台电脑</option>` : "") + M.options(M.choices(ms, { keep: cur }), { cur: pick ? undefined : cur });
  }
  const machineName = (ms, id) => { const x = ms.find((x) => x.id === (id || "")); return x?.local ? "hub" : x?.name || id || "hub"; };

  // The actions it offers agents (collie ext do), and the switch for the
  // ones that change something.
  function actionsHTML(v) {
    const acts = v.manifest.contributes?.connector?.actions || [];
    if (!acts.length) return "";
    const writes = acts.some((a) => !a.readonly);
    return `<div class="card"><div class="card-h">${cardTitle("zap", "给 agent 的动作")}</div><div class="form">
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
      if (p.type === "boolean") return `<label class="check"><input type="checkbox" class="switch" role="switch" data-ext-config="${esc(k)}" ${val ? "checked" : ""}><span>${esc(p.title)}${p.description ? `<small>${esc(p.description)}</small>` : ""}</span></label>`;
      if (p.enum) return `<label class="ext-field">${esc(p.title)}<select data-ext-config="${esc(k)}" data-pick="compact">${p.enum.map((e, i) => `<option value="${esc(e)}" ${e === val ? "selected" : ""}>${esc(p["x-labels"]?.[i] || e)}</option>`).join("")}</select>${p.description ? `<small>${esc(p.description)}</small>` : ""}</label>`;
      if (p.type === "integer") return `<label>${esc(p.title)}<input type="number" step="1" data-ext-config="${esc(k)}" data-ext-type="integer" value="${esc(val ?? "")}">${p.description ? `<small>${esc(p.description)}</small>` : ""}</label>`;
      return `<label>${esc(p.title)}<input data-ext-config="${esc(k)}" value="${esc(val ?? "")}">${p.description ? `<small>${esc(p.description)}</small>` : ""}</label>`;
    });
  }
  function wechatAvatarField(v) {
    if (v?.manifest?.runtime?.builtin !== "wechat") return "";
    const pic = v.avatar_url ? `<img class="inbox-pic" src="${esc(v.avatar_url)}" alt="">` : "";
    return `<div class="ext-avatar"><span>头像</span><div class="ext-avatar-row">
      <span class="inbox-ava sq">${ico("user")}${pic}${logo(v.manifest.id)}</span>
      <input type="file" accept="image/png,image/jpeg,image/gif,image/webp" hidden data-ext-avatar-input>
      <button type="button" class="btn small" data-ext-avatar-pick>${v.avatar_url ? "更换" : "上传"}</button>
      ${v.avatar_url ? `<button type="button" class="btn small ghost" data-ext-avatar-clear>恢复默认</button>` : ""}
    </div><small>显示在 Collie 的微信会话中。</small></div>`;
  }
  // The choices first, then the switches in one column, and those that
  // leave chats out of the 收件箱 (x-hide) under a heading of their own.
  function configGrid(v) {
    const props = Object.entries(v.manifest.configuration?.properties || {});
    const fields = configFields(v), hides = props.map(([, p]) => p.type === "boolean" && !!p["x-hide"]);
    const pick = (f) => fields.filter((_, i) => f(props[i][1], hides[i])).join("");
    const choices = pick((p) => p.type !== "boolean") + wechatAvatarField(v), switches = pick((p, h) => p.type === "boolean" && !h), hidden = pick((_, h) => h);
    return (choices ? `<div class="set-grid">${choices}</div>` : "") + (switches ? `<div class="ext-switches">${switches}</div>` : "")
      + (hidden ? `<div class="set-sub">收件箱里不显示</div><div class="ext-switches">${hidden}</div>` : "");
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
  const extRank = (m) => (m?.market ? 1 : m?.local ? 2 : 0);
  function hooksList(r, inbox, markets) {
    const list = [...(r?.extensions || [])].sort((a, b) => extRank(a.manifest) - extRank(b.manifest) || String(a.manifest?.id || "").localeCompare(String(b.manifest?.id || "")));
    const ms = r?.machines || [];
    const convs = inbox?.convs || [];
    const row = (v) => {
      const m = v.manifest, st = stateOf(v);
      const n = convs.filter((c) => c.ext === m.id), on = n.filter((c) => c.on).length;
      const sub = [m.market ? "市场" : m.local ? "本地" : "内置", n.length ? `${n.length} 个会话${on ? `，${on} 个${AI_HANDLED_LABEL}` : ""}` : "",
        (m.contributes?.connector?.capabilities || []).includes("header_status") ? "标题栏状态" : "",
        (m.contributes?.connector?.actions || []).length ? `${m.contributes.connector.actions.length} 个动作` : ""].filter(Boolean).join(" · ");
      return `<a class="row ext-row" href="#/auto/hooks/ext/${encodeURIComponent(m.id)}">${extIcon(m, st)}
        <div><div class="meta"><b>${esc(m.name)}</b></div><div class="text">${esc(sub)}</div></div>
        <span class="right ext-row-state"><span class="ext-st">${statePill(st)}${v.setting.enabled && !declarative(m) ? `<span class="ext-st-on">${esc(machineName(ms, v.setting.machine))}</span>` : ""}</span><span class="chev" aria-hidden="true">›</span></span></a>`;
    };
    const nav = (href, title, sub, icon) => `<a class="row ext-row" href="${href}">${symTile(icon)}<div><div class="meta"><b>${title}</b></div><div class="text">${esc(sub)}</div></div><span class="right"><span class="chev" aria-hidden="true">›</span></span></a>`;
    const updates = (markets?.markets || []).reduce((n, mk) => n + (mk.index || []).filter((e) => mk.installed?.[e.id] && mk.installed[e.id] !== e.version).length, 0);
    return `<p class="panel-intro">接上飞书、微信，就能在那边和 Collie 对话；有的扩展还能让 agent 多做些事。扩展默认都关着，点进去开启、登录后才生效。</p>
      <div class="card"><div class="card-h">扩展</div><div class="rows">${list.map(row).join("") || `<div class="empty-row">没有可用的扩展</div>`}</div></div>
      <div class="card"><div class="rows">
        ${nav("#/auto/hooks/reply", "唤起与回复", `触发词 ${r?.trigger || r?.default_trigger || "/ai"} · 回复尾标`, "message-square-reply")}
        ${nav("#/auto/hooks/markets", "扩展市场", `${(markets?.markets || []).length} 个市场${updates ? ` · ${updates} 个可更新` : ""}`, "store")}
      </div></div>`;
  }

  // icon: an extension's page leads with its icon, the name and version beside it.
  const pageHead = (title, sub = "", icon = "") => `<div class="sub-head"><a class="sub-back" href="#/auto" data-sub-back aria-label="返回接入">‹ 接入</a>${icon ? `<div class="ext-head">${icon}<div><h3>${title}</h3>${sub}</div></div>` : `<h3>${title}</h3>${sub}`}</div>`;

  // One extension's page: its state and the one thing to do next on top.
  function extPage(v, r, qr, inbox) {
    if (!v) return pageHead("扩展") + `<div class="card"><div class="empty-row">没有这个扩展</div></div>`;
    const m = v.manifest, st = stateOf(v), ms = r?.machines || [], on = v.setting.enabled;
    const ask = m.defaults?.machine === "ask" && !v.setting.chosen;
    const primary = st.key === "off" ? (needsLogin(m) ? `<button class="btn primary" data-ext-login>${esc(loginWord(m))}</button>` : `<button class="btn primary" data-ext-enable>开启</button>`)
      : st.login && !(qr && qr.id === m.id) ? `<button class="btn primary" data-ext-login>${esc(loginWord(m))}</button>` : "";
    const convs = (inbox?.convs || []).filter((c) => c.ext === m.id);
    // Where it runs sits on the state's line, what that means under it.
    const machine = declarative(m) ? "" : m.local
      ? `<span class="ext-field">运行在<b>${esc(machineName(ms, m.local_on))}</b></span>`
      : `<label class="ext-field">运行在<select data-ext-machine data-pick="compact" data-title="在哪台电脑上运行">${machineOptions(ms, v.setting.machine || "", ask)}</select></label>`;
    const machineNote = declarative(m) ? "" : `<small class="muted ext-field-note">${m.local ? "本地扩展，只在装了它的那台电脑上运行" : `${ask ? "它只能在一台电脑上运行，先选好再开启。" : ""}换电脑会停掉原来那台上的；登录状态留在原来那台，新电脑上可能要重新登录。`}</small>`;
    // The buttons that stop or remove it, each with what it does beside it.
    const danger = (attr, label, note) => `<button type="button" class="btn small danger" ${attr}>${label}</button><small class="muted">${note}</small>`;
    return `${pageHead(esc(m.name), `<span class="muted">${esc(m.version)} · ${m.market ? "市场" : m.local ? "本地" : "内置"}</span>`, extIcon(m, st, true))}
      <div class="card ext-card" data-ext="${esc(m.id)}"><div class="form">
        <div class="ext-status">${statePill(st)}<div><b${st.title ? ` title="${esc(st.title)}"` : ""}>${esc(st.text)}</b>${st.busy ? `<span class="spinner small" aria-hidden="true"></span>` : ""}${detailLine(v.status)}</div>${machine}${primary}</div>
        ${machineNote}
        ${loginPanel(m, qr)}
      </div></div>
      ${m.configuration ? `<div class="card" data-ext="${esc(m.id)}"><div class="card-h">${cardTitle("settings-2", "设置")}</div><div class="form ext-config">${configGrid(v)}<div class="form-actions"><span class="ext-dirty muted">有改动，保存后生效</span><button class="btn small" data-ext-save>保存设置</button></div></div></div>` : ""}
      <div data-ext="${esc(m.id)}">${actionsHTML(v)}</div>
      ${m.contributes?.connector?.capabilities?.some((c) => c === "archive" || c === "receive") ? `<div class="card"><div class="rows"><a class="row ext-row" href="#/auto" data-auto-tab="inbox">${symTile("inbox")}<div><div class="meta"><b>会话</b></div><div class="text">${convs.length} 个会话${convs.filter((c) => c.on).length ? `，${convs.filter((c) => c.on).length} 个${AI_HANDLED_LABEL}` : ""}，在收件箱里打开要处理的</div></div><span class="right"><span class="chev" aria-hidden="true">›</span></span></a></div></div>` : ""}
      <div class="card"><div class="card-h">${cardTitle("info", "说明")}</div><div class="form">
        <p class="ext-desc">${esc(m.description)}</p>
        ${m.warning ? `<p class="ext-warn">${esc(m.warning)}</p>` : ""}
        ${m.market ? `<p class="ext-desc">从市场 ${esc(m.market.repo)}（${m.market.package ? esc(m.market.package.replace(/^release:/, "Release ")) : esc((m.market.commit || "").slice(0, 7))}）安装</p>` : ""}
        ${m.local ? `<p class="ext-warn">从本地目录（${esc(m.id)}/pkg/${esc(m.version)}）加载，Collie 不检查它的内容，请确认来源可信。</p>` : ""}
        ${(m.permissions?.network || []).length ? `<p class="ext-desc">会连接这些网站：${esc(m.permissions.network.join("、"))}</p>` : ""}
      </div></div>
      ${v.proc?.logs?.length ? `<div class="card"><details class="ext-logs"><summary class="card-h">${cardTitle("scroll-text", "最近日志")}</summary><pre>${esc(v.proc.logs.join("\n"))}</pre></details></div>` : ""}
      <div class="card" data-ext="${esc(m.id)}"><div class="form ext-danger">
        ${on ? danger("data-ext-disable", "关闭扩展", "只是暂停收发，数据和登录都还在。") : `<button type="button" class="btn small" data-ext-enable>开启</button><small class="muted">开启后开始收发。</small>`}
        ${on && st.ok && needsLogin(m) ? danger("data-ext-logout", "退出登录", "会清掉登录状态，之后要重新登录。") : ""}
        ${m.market ? danger("data-ext-uninstall", "卸载", "停掉它，删掉它的设置和安装包。") : ""}
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

  // 扩展市场: the repositories extensions are installed from, each with how
  // it is and what each of its extensions is to you; adding one below, apart.
  function marketsHTML(mr) {
    const ms = mr?.markets || [];
    const entry = (m, e) => {
      const have = m.installed?.[e.id], newer = have && have !== e.version;
      const state = !have ? "" : newer ? `<span class="pill info">可更新 · 已装 ${esc(have)}</span>` : `<span class="pill ok">已安装</span>`;
      const act = !have ? `<button class="btn small" data-market-install="${esc(e.id)}">安装</button>`
        : newer ? `<button class="btn small primary" data-market-install="${esc(e.id)}">更新到 ${esc(e.version)}</button>` : "";
      return `<li class="row market-entry">${symTile("package")}<div><div class="meta"><b>${esc(e.name || e.id)}</b><span class="mono">${esc(e.id)} ${esc(e.version)}</span></div><div class="text">${esc(e.description || "")}</div></div><span class="right market-act">${state}${act}</span></li>`;
    };
    const market = (m) => `<div class="card market" data-market="${esc(m.repo)}">
        <div class="card-h market-h">${symTile("folder-git-2")}<div class="market-t"><b>${esc(m.name || m.repo)}</b><span class="muted mono">${esc(m.repo)}${m.ref ? "@" + esc(m.ref) : ""}${m.commit ? " · " + esc(m.commit.slice(0, 7)) : ""}</span></div>
          ${m.error ? `<span class="pill bad">出错</span>` : `<span class="pill ok">可用</span>`}${m.has_token ? `<span class="pill plain off">有 token</span>` : ""}
          <button class="btn small danger" data-market-remove>移除</button></div>
        ${m.error ? `<p class="ext-warn bad market-err">${esc(m.error)}</p>` : ""}
        <ul class="rows">${(m.index || []).map((e) => entry(m, e)).join("") || (m.error ? "" : `<li class="empty-row">这个市场里还没有扩展</li>`)}</ul></div>`;
    return `${pageHead("扩展市场")}
      <div class="market-bar"><p class="ext-desc">飞书、微信和工作台是自带的，其他扩展从市场安装。市场就是一个 GitHub 仓库，根目录放 collie-marketplace.json（格式见 extensions/README.md）。</p>
        ${ms.length ? `<button class="btn small" data-market-refresh>${ico("refresh-cw")}刷新</button>` : ""}</div>
      ${ms.map(market).join("") || `<div class="card"><div class="empty-row with-ico">${ico("store")}<div>还没有市场，在下面添加一个</div></div></div>`}
      <div class="card"><div class="card-h">${cardTitle("plus", "添加市场")}</div>
        <form class="form" id="market-form"><div class="set-grid">
          <label>仓库<input name="repo" placeholder="owner/name" autocorrect="off" autocapitalize="off" spellcheck="false" required></label>
          <label>分支或标签<input name="ref" placeholder="默认分支" autocorrect="off" autocapitalize="off" spellcheck="false"></label>
          <label class="span2">Token（私有仓库需要）<input name="token" type="password" autocomplete="off" placeholder="留空不改"><small>只存在 hub 上，填完不再显示。填已有市场的仓库就是更新它的分支或 token。</small></label>
        </div><div class="form-actions"><button class="btn primary small">添加或更新</button></div></form></div>`;
  }

  function outLine(o) {
    if (!o || o.state === "sent") return "";
    const [text, tone] = OUT[o.state] || [o.state, ""];
    return `<span class="ext-out ${tone}">${esc(text)}${o.reason && o.state !== "pending" ? "：" + esc(o.reason) : ""}</span>`;
  }

  // A list's time: the hour today, then 昨天, the weekday within a week,
  // the date this year, the full date before.
  const WEEKDAY = "日一二三四五六";
  function shortTime(t) {
    const d = new Date(t);
    if (!(d.getFullYear() > 2000)) return "";
    const now = new Date(), day = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
    const days = Math.round((day(now) - day(d)) / 864e5);
    const p2 = (n) => String(n).padStart(2, "0");
    if (days <= 0) return `${p2(d.getHours())}:${p2(d.getMinutes())}`;
    if (days === 1) return "昨天";
    if (days < 7) return "周" + WEEKDAY[d.getDay()];
    return d.getFullYear() === now.getFullYear() ? `${d.getMonth() + 1}月${d.getDate()}日` : `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`;
  }
  const unreadText = (n) => n > 99 ? "99+" : String(n);
  // A message's day as a chat shows it (今天, 昨天, the date) and its hour.
  function dayLabel(t) {
    const d = new Date(t), now = new Date();
    const days = Math.round((new Date(now.getFullYear(), now.getMonth(), now.getDate()) - new Date(d.getFullYear(), d.getMonth(), d.getDate())) / 864e5);
    if (days <= 0) return "今天";
    if (days === 1) return "昨天";
    return d.getFullYear() === now.getFullYear() ? `${d.getMonth() + 1}月${d.getDate()}日` : `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日`;
  }
  const clock = (t) => { const d = new Date(t); return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`; };
  // A chat's round tile: its picture (through the hub) over the icon of its
  // kind, which stays when there is none or it fails to load; the app's logo
  // in the corner.
  // WeChat's pictures are rounded squares, as WeChat draws them; others round.
  const SQUARE = new Set(["wechat"]);
  const avatar = (c, title = "") => `<span class="inbox-ava${SQUARE.has(c.ext) ? " sq" : ""}"${title ? ` title="${esc(title)}"` : ""}>${ico(KIND_ICON[c.kind] || "message-circle")}${c.avatar ? `<img class="inbox-pic" src="${esc(c.avatar)}" alt="" loading="lazy" decoding="async">` : ""}${logo(c.ext)}</span>`;

  // 收件箱: the channels first (飞书, 微信 …) with 已交给 AI apart, a
  // channel's own filters above its chats, and the open chat beside them.
  const atMe = (c) => c.at_me && c.unread > 0;
  const AI_ON = "on"; // the channel key of 已交给 AI
  // A channel's filters: [key, label, which chats, icon]; 单聊、@我、群聊
  // only where it has both direct chats and groups. 全部 lists every chat,
  // the others only the unread ones; each counts the chats it lists.
  const unread = (c) => c.unread > 0;
  const SUBS = [
    ["", "全部", () => true, "message-square-text"],
    ["unread", "未读", unread, "message-square-dot"],
    ["dm", "单聊", (c) => unread(c) && (c.kind === "dm" || c.kind === "self") && !c.tags?.includes("bot"), "user", true],
    ["at", "@我", atMe, "at-sign", true],
    ["group", "群聊", (c) => unread(c) && c.kind === "group", "users", true],
  ];
  const listed = (r) => (r?.convs || []).filter((c) => !c.hidden); // folded, or switched off in 接入
  // The channels, by id: label, chats, and whether it has both kinds of chat.
  function channels(r) {
    const by = new Map();
    for (const c of listed(r)) {
      if (!by.has(c.ext)) by.set(c.ext, { id: c.ext, label: c.label || c.ext, convs: [] });
      by.get(c.ext).convs.push(c);
    }
    for (const ch of by.values()) ch.mixed = new Set(ch.convs.map((c) => c.kind === "group" ? "group" : "dm")).size > 1;
    return [...by.values()].sort((a, b) => a.id.localeCompare(b.id));
  }
  // Where the list is: a channel (the first when none or a gone one is
  // asked for) and its filter, or 已交给 AI.
  function inboxPlace(r, channel, sub) {
    const chs = channels(r);
    if (channel === AI_ON) return { chs, on: true, sub: "" };
    const ch = chs.find((x) => x.id === channel) || chs[0];
    const s = ch && SUBS.find(([k, , , , mixed]) => k === (sub || "") && (!mixed || ch.mixed));
    return { chs, ch, sub: s ? s[0] : "" };
  }
  function inboxShown(r, channel, sub) {
    const p = inboxPlace(r, channel, sub);
    if (p.on) return listed(r).filter((c) => c.on);
    return p.ch ? p.ch.convs.filter(SUBS.find(([k]) => k === p.sub)[2]) : [];
  }
  // Unread chats, as the chat app's own badge counts them: muted ones aside.
  const unreadOf = (convs) => convs.filter((c) => c.unread > 0 && !c.muted).length;

  // The channel list: each channel with its unread chats (amber with an @
  // for you), then 已交给 AI apart. On a phone, a row of tabs.
  function channelsHTML(p, r) {
    const item = (key, lead, label, n, tone, on) => `<button type="button" class="inbox-ch${on ? " on" : ""}" data-inbox-channel="${esc(key)}"${on ? ' aria-current="true"' : ""}>${lead}<span class="inbox-ch-l">${esc(label)}</span>${n ? `<span class="inbox-ch-n${tone}">${unreadText(n)}</span>` : ""}</button>`;
    const ai = listed(r).filter((c) => c.on).length;
    return `<nav class="inbox-chs" aria-label="渠道">
        ${p.chs.map((ch) => item(ch.id, logo(ch.id) || ico("message-circle"), ch.label, unreadOf(ch.convs), ch.convs.some(atMe) ? " attn" : "", !p.on && p.ch?.id === ch.id)).join("")}
        <span class="inbox-chs-sep" aria-hidden="true"></span>
        ${item(AI_ON, ico("bot"), AI_HANDLED_LABEL, ai, " plain", p.on)}
      </nav>`;
  }
  // A channel's filters, with icons and how many chats each lists.
  function subsHTML(p) {
    if (p.on || !p.ch) return "";
    const chip = ([k, l, pick, icon, mixed]) => {
      if (mixed && !p.ch.mixed) return "";
      const n = p.ch.convs.filter(pick).length, on = p.sub === k;
      const tone = n && k === "at" ? " attn" : n && k ? " info" : "";
      return `<button type="button" class="inbox-chip${on ? " on" : ""}" data-inbox-sub="${esc(k)}"${on ? ' aria-current="true"' : ""}>${ico(icon)}<span>${esc(l)}</span><span class="inbox-nav-n${tone}">${n}</span></button>`;
    };
    return `<div class="inbox-chips" role="toolbar" aria-label="筛选${esc(p.ch.label)}会话">${SUBS.map(chip).join("")}</div>`;
  }

  function inboxHTML(r, cur, detail, channel, sub) {
    const convs = listed(r);
    const p = inboxPlace(r, channel, sub);
    const shown = inboxShown(r, channel, sub);
    const unreadHere = shown.some((c) => c.unread > 0);
    const row = (c) => {
      const n = c.unread > 0 ? c.unread : 0, kind = KIND[c.kind] || c.kind || "";
      const name = c.name || c.chat;
      // A direct chat's preview needn't repeat who it is with.
      const line = c.ext === "larkx" ? collieInboxMessage.legacyPreview(c.preview) : c.preview || "";
      const preview = c.kind === "dm" && line.startsWith(name + ": ") ? line.slice(name.length + 2) : line;
      return `<li class="row inbox-row${n ? " unread" : ""}${c.id === cur ? " sel" : ""}" data-conv="${esc(c.id)}">
        ${avatar(c, `${c.label} · ${kind}`)}
        <div class="inbox-main">
          <div class="inbox-l1"><b class="inbox-name"${name !== c.chat ? ` title="${esc(name)}"` : ""}>${esc(name)}</b>${c.muted ? `<span class="inbox-mute" title="在原应用里设了免打扰">${ico("bell-off")}</span>` : ""}${c.on ? `<span class="pill plain info inbox-on">${AI_HANDLED_LABEL}</span>` : ""}<time class="inbox-time" datetime="${esc(c.last_at || "")}" title="${esc(when(c.last_at))}">${esc(shortTime(c.last_at))}</time></div>
          <div class="inbox-l2"><span class="inbox-prev">${atMe(c) ? `<span class="inbox-at">[有人@我]</span>` : ""}${esc(preview)}</span>${n ? `<span class="inbox-unread${c.muted ? " muted" : ""}" title="${n} 条未读${c.muted ? "（免打扰）" : ""}">${unreadText(n)}</span>` : ""}</div>
          ${c.on ? outLine(c.out) : ""}
        </div></li>`;
    };
    const intro = `<p class="panel-intro">打开的扩展收到的会话都在这里。你本人发的、以触发词 <b>${esc(r?.trigger || "/ai")}</b> 开头的消息，在任何会话都会交给 Collie 或这个会话的专属 agent，回复带尾标发回原会话；会话开关只管别人的消息。飞书会话可查看最近消息。</p>`;
    if (!convs.length) return intro + `<div class="card"><div class="empty-row with-ico">${ico("inbox")}<div>还没有会话。先在 <a href="#/auto" data-auto-tab="hooks">接入</a> 打开一个扩展并登录</div></div></div>`;
    const readAll = `<button type="button" class="btn small ghost inbox-read-all" data-inbox-read-all aria-label="全部已读" ${unreadHere ? "" : "disabled"} title="把这个列表里的会话都标为已读">${ico("check-check")}<span>全部已读</span></button>`;
    return intro + `<div class="inbox-split ${cur ? "has-cur" : ""}">
        ${channelsHTML(p, r)}
        <div class="inbox-col">
          <div class="inbox-head">${subsHTML(p) || `<span class="inbox-head-t">${esc(p.on ? AI_HANDLED_LABEL : p.ch?.label || "")}<span class="muted">${shown.length}</span></span>`}${readAll}</div>
          <div class="card inbox-list"><ul class="rows">${shown.map(row).join("") || `<li class="empty-row">${p.sub ? "都读过了" : "没有"}</li>`}</ul></div>
        </div>
        <div class="inbox-detail" id="inbox-detail">${detailHTML(cur, detail, r?.trigger)}</div>
      </div>`;
  }

  const detailHTML = (cur, detail, trigger) => cur ? convHTML(detail, trigger) : `<div class="card"><div class="empty-row with-ico">${ico("messages-square")}<div>选一个会话</div></div></div>`;

  // Newest message first, so the latest reply is the first thing you see.
  function convHTML(d, trigger) {
    if (!d) return `<div class="card"><div class="empty-row">正在读取…</div></div>`;
    const c = d.conv, msgs = d.msgs || [], outs = d.outbox || {};
    const seg = (name, val, opts) => `<div class="seg">${opts.map(([v, l]) => `<button type="button" data-conv-set="${name}" data-v="${v}" class="${String(val) === String(v) ? "on" : ""}">${l}</button>`).join("")}</div>`;
    const href = c.worker ? workerHref(c.worker) : "";
    const worker = c.worker ? (href ? `<a href="${esc(href)}">${esc(c.worker)} →</a>` : esc(c.worker)) : "";
    // A chat's messages as a chat app shows them: yours (and Collie's answers,
    // sent from your account) on the right, others on the left with their
    // picture or initial; in your own chat with a bot (c.owner) the answers are
    // the bot's, on the left. A day line between days; one header for a run of
    // messages from the same sender within a few minutes; system notices in
    // the middle.
    const RUN_GAP = 5 * 60e3;
    const who = (m) => m.ref || m.bot ? "ai" : m.mine ? "me" : m.sender || m.sender_name || "";
    const msg = (m, i, list) => {
      const o = m.ref ? outs[m.ref] : null;
      const rich = m.view && !m.ref;
      const text = c.ext === "larkx" ? collieInboxMessage.legacyPreview(m.text) : m.text;
      const day = dayLabel(m.at), newer = list[i - 1], older = list[i + 1];
      const line = !newer || dayLabel(newer.at) !== day ? `<li class="conv-day"><span>${esc(day)}</span></li>` : "";
      if (m.view?.kind === "system") return `${line}<li class="conv-sys">${collieInboxMessage.render(m.view)}</li>`;
      const ai = who(m) === "ai", mine = ai ? !c.owner : m.mine;
      // Newest first: a run's header sits on its oldest message, the one shown lowest.
      const head = !older || who(older) !== who(m) || dayLabel(older.at) !== day || Date.parse(m.at) - Date.parse(older.at) > RUN_GAP;
      // In your chat with a bot its answers carry the bot's name and picture (c's), and the AI 代答 mark.
      const bot = ai && c.owner, pic = bot ? c.avatar : m.avatar;
      const name = bot ? c.name : ai ? "" : m.mine ? "你" : m.sender_name || "";
      const card = rich && (m.view.kind === "card" || m.view.kind === "calendar" || m.view.kind === "media");
      const body = rich ? collieInboxMessage.render(m.view) : m.ref ? md(m.text) : esc(text);
      return `${line}<li class="conv-msg ${mine ? "me" : "them"}${head ? " head" : ""}">
        ${mine ? "" : `<span class="conv-ava${SQUARE.has(c.ext) ? " sq" : ""}" aria-hidden="true">${head ? `${ai ? ico("bot") : esc((name || "?").slice(0, 1))}${pic ? `<img class="inbox-pic" src="${esc(pic)}" alt="" loading="lazy" decoding="async">` : ""}` : ""}</span>`}
        <div class="conv-body">${head ? `<div class="meta">${name ? `<b>${esc(name)}</b>` : ""}${ai ? `<span class="conv-ai">${ico("bot")}AI 代答</span>` : ""}<time title="${esc(when(m.at))}">${esc(clock(m.at))}</time></div>` : ""}
          <div class="msg conv-bubble${card ? " bare" : ""} ${rich ? "im-rich" : m.ref ? "assistant" : ""}">${body}</div>${o ? outLine(o) : ""}</div></li>`;
    };
    return `<div class="card conv-card">
      <div class="card-h"><span class="conv-title">${avatar(c)}<span${c.name && c.name !== c.chat ? ` title="${esc(c.chat)}"` : ""}>${esc(c.name || c.chat)}</span></span><button type="button" class="icon-btn conv-close" data-conv-close title="返回">×</button></div>
      <div class="form">
        <div class="conv-rule"><button type="button" class="switch" role="switch" aria-checked="${!!c.on}" aria-label="处理这个会话" data-conv-on></button><span><b>处理这个会话</b>${c.on ? "" : `<small class="muted">打开后，以 ${esc(trigger || "/ai")} 开头的消息交给 agent，并保存会话上下文</small>`}</span></div>
        ${c.on ? `<div class="conv-rule"><span class="tm-lab">谁能唤起</span>${seg("anyone", !!c.anyone, [[false, "只有我"], [true, "任何人"]])}</div>
          ${c.anyone ? `<p class="ext-warn">任何人都能唤起时，专属 agent 在隔离目录里工作，但那不是沙箱：只对信任的群打开。</p>` : ""}
          <div class="conv-rule"><span class="tm-lab">交给</span>${seg("handler", c.handler, [["collie", "Collie"], ["agent", "专属 agent"]])}${c.handler === "agent" ? `<span class="muted">${worker || "第一次唤起时启动"}</span>` : ""}</div>` : ""}
      </div>
      ${msgs.length ? `<ul class="conv-msgs" aria-label="最近消息">${msgs.slice().reverse().map(msg).join("")}</ul>`
        : `<div class="empty-row">${esc(c.ext === "larkx" ? collieInboxMessage.legacyPreview(c.preview) : c.preview || "")
          || (c.on ? "打开以后的消息会出现在这里" : "暂无消息")}</div>`}
      ${d.history_unavailable ? `<div class="empty-row">暂时无法从飞书读取最近消息</div>` : ""}
    </div>`;
  }

  // The timer form's 发到: Collie, or an open conversation.
  function timerChannels(r) {
    return (r?.convs || []).filter((c) => c.on).map((c) => `<option value="ext:${esc(c.id)}" data-sub="${esc(c.label)}">${esc(c.label)} · ${esc(c.name || c.chat)}</option>`).join("");
  }

  // Short names and logos of extensions, from what the pages fetched (manifest label).
  const labels = {}, icons = {};
  function learn(r) {
    for (const v of r?.extensions || []) { labels[v.manifest.id] = v.manifest.label || v.manifest.name; icons[v.manifest.id] = v.manifest.icon_url || ""; }
    for (const c of r?.convs || []) labels[c.ext] = c.label;
  }
  const label = (source) => source?.startsWith("ext:") ? labels[source.slice(4)] || source.slice(4) : "";
  // A small logo beside an extension's name, once its list has been read.
  function logo(id) { const u = icons[id]; return u ? `<img class="src-logo" src="${esc(u)}" alt="">` : ""; }
  const known = () => Object.keys(icons).length > 0;

  window.collieExt = { init, hooksList, extPage, replyPage, marketsHTML, pageHead, inboxHTML, inboxShown, inboxPlace, convHTML, detailHTML, timerChannels, wechatAvatarField, label, logo, known, learn, stateOf };
})();

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
  // A direct chat with a bot (tag bot) says so, not 私聊.
  const isBot = (c) => !!c.tags?.includes("bot");
  const kindLabel = (c) => isBot(c) ? "bot" : KIND[c.kind] || c.kind || "";
  const KIND_ICON = { dm: "user", group: "users", self: "user", doc: "file-text" };
  const { svg: ico, title: cardTitle } = window.collieIcons;
  // A line icon on the same tile as an extension's logo, for the rows
  // beside them that lead somewhere else.
  const symTile = (name) => `<span class="ext-icon sym" aria-hidden="true">${ico(name)}</span>`;

  function init(h) { esc = h.esc; when = h.when; md = h.md; workerHref = h.workerHref; }

  // Where an extension is, in four words (and what to do next).
  const declarative = (m) => !!m.runtime?.feature || (!m.runtime?.command && !m.runtime?.builtin && !m.contributes?.connector?.id);
  const needsLogin = (m) => !!m.contributes?.connector?.login;
  function stateOf(v) {
    const m = v.manifest;
    if (!v.setting.enabled) return { key: "off", dot: "asleep", label: "未开启", text: "未开启" };
    if (declarative(m)) {
      if (v.status?.state === "ok") {
        return { key: "on", dot: "online", label: "运行中", text: "已开启" + (v.status.account ? " · " + v.status.account : ""), ok: true };
      }
      return { key: "on", dot: "online", label: "已开启", text: "已开启（不需要运行程序）", ok: true };
    }
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
  const extRank = (m) => (m?.id === "usage" ? -1 : m?.market ? 1 : m?.local ? 2 : 0);
  const skelRow = (small = false) => `<div class="row skel-row" aria-hidden="true"><span class="skel-box${small ? " sm" : ""}"></span><div class="skel-lines"><span class="skel-line"></span><span class="skel-line short"></span></div><span class="right"><span class="skel-pill"></span></span></div>`;
  const loadStatus = (text) => `<div class="skel-status" role="status"><span class="spinner small" aria-hidden="true"></span><span>${esc(text)}</span></div>`;
  const errBox = (msg, src) => `<div class="skel-err"><p class="ext-warn bad">${esc(msg)}</p><button type="button" class="btn small" data-auto-retry="${esc(src)}">重试</button></div>`;
  function hooksList(r, inbox, markets, state = {}) {
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
    const extBody = r
      ? (list.map(row).join("") || `<div class="empty-row with-ico">${ico("package")}<div>没有可用的扩展</div></div>`)
      : state.extErr
        ? errBox("读取扩展列表失败：" + state.extErr, "ext")
        : skelRow() + skelRow() + skelRow() + loadStatus("正在读取扩展…");
    const marketsSub = markets
      ? `${(markets.markets || []).length} 个市场${updates ? ` · ${updates} 个可更新` : ""}`
      : state.marketsErr
        ? "读取市场失败，点进去重试"
        : "正在读取市场…";
    return `<p class="panel-intro">接上飞书、微信，就能在那边和 Collie 对话；有的扩展还能让 agent 多做些事。扩展默认都关着，点进去开启、登录后才生效。</p>
      <div class="card"${!r && !state.extErr ? ' aria-busy="true"' : ""}><div class="card-h">${cardTitle("package", "扩展")}</div><div class="rows">${extBody}</div></div>
      <div class="card"><div class="rows">
        ${nav("#/auto/hooks/reply", "唤起与回复", `触发词 ${r?.trigger || r?.default_trigger || "/ai"} · 回复尾标`, "message-square-reply")}
        ${nav("#/auto/hooks/markets", "扩展市场", marketsSub, "store")}
      </div></div>`;
  }

  // icon: an extension's page leads with its icon, the name and version beside it.
  const pageHead = (title, sub = "", icon = "") => `<div class="sub-head"><a class="sub-back" href="#/auto" data-sub-back aria-label="返回接入">‹ 接入</a>${icon ? `<div class="ext-head">${icon}<div><h3>${title}</h3>${sub}</div></div>` : `<h3>${title}</h3>${sub}`}</div>`;

  // One extension's page: its state and the one thing to do next on top.
  function extPage(v, r, qr, inbox, state = {}) {
    if (!v) {
      if (!r && state.extErr) return pageHead("扩展") + `<div class="card">${errBox("读取扩展信息失败：" + state.extErr, "ext")}</div>`;
      if (!r && state.extLoading) return pageHead("扩展") + `<div class="card" aria-busy="true"><div class="rows">${skelRow()}</div>${loadStatus("正在读取扩展信息…")}</div>`;
      return pageHead("扩展") + `<div class="card"><div class="empty-row">没有这个扩展</div></div>`;
    }
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
        ${on ? danger("data-ext-disable", "关闭扩展", m.runtime?.feature ? "关闭后立即停用该功能，配置会保留。" : "只是暂停收发，数据和登录都还在。") : `<button type="button" class="btn small" data-ext-enable>开启</button><small class="muted">${m.runtime?.feature ? "开启后立即生效。" : "开启后开始收发。"}</small>`}
        ${on && st.ok && needsLogin(m) ? danger("data-ext-logout", "退出登录", "会清掉登录状态，之后要重新登录。") : ""}
        ${m.market ? danger("data-ext-uninstall", "卸载", "停掉它，删掉它的设置和安装包。") : ""}
      </div></div>`;
  }

  function replyPage(r, state = {}) {
    if (!r && state.extErr) return pageHead("唤起与回复") + `<div class="card">${errBox("读取设置失败：" + state.extErr, "ext")}</div>`;
    if (!r && state.extLoading) return pageHead("唤起与回复") + `<div class="card" aria-busy="true"><div class="rows">${skelRow()}</div>${loadStatus("正在读取设置…")}</div>`;
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
  function marketsHTML(mr, state = {}) {
    const ms = mr?.markets || [];
    const entryTile = (e, have) => {
      const u = have && icons[e.id];
      return u ? `<span class="ext-icon logo" aria-hidden="true"><img src="${esc(u)}" alt=""></span>` : symTile("package");
    };
    const entry = (m, e) => {
      const have = m.installed?.[e.id], newer = have && have !== e.version;
      const state = !have ? "" : newer ? `<span class="pill info">可更新 · 已装 ${esc(have)}</span>` : `<span class="pill ok">已安装</span>`;
      const dis = m.error ? " disabled" : "";
      const act = !have ? `<button class="btn small" data-market-install="${esc(e.id)}"${dis}>安装</button>`
        : newer ? `<button class="btn small primary" data-market-install="${esc(e.id)}"${dis}>更新到 ${esc(e.version)}</button>` : "";
      return `<li class="row market-entry">${entryTile(e, have)}<div><div class="meta"><b>${esc(e.name || e.id)}</b><span class="mono">${esc(e.id)} ${esc(e.version)}</span></div><div class="text">${esc(e.description || "")}</div></div><span class="right market-act">${state}${act}</span></li>`;
    };
    const market = (m) => `<div class="card market" data-market="${esc(m.repo)}">
        <div class="card-h market-h">${symTile("folder-git-2")}<div class="market-t"><b>${esc(m.name || m.repo)}</b><span class="muted mono">${esc(m.repo)}${m.ref ? "@" + esc(m.ref) : ""}${m.commit ? " · " + esc(m.commit.slice(0, 7)) : ""}</span></div>
          ${m.error ? `<span class="pill bad">出错</span>` : `<span class="pill ok">可用</span>`}${m.has_token ? `<span class="pill plain off">有 token</span>` : ""}
          <button class="btn small danger" data-market-remove>移除</button></div>
        ${m.error ? `<p class="ext-warn bad market-err">${esc(m.error)}</p>` : ""}
        <ul class="rows">${(m.index || []).map((e) => entry(m, e)).join("") || (m.error ? "" : `<li class="empty-row">这个市场里还没有扩展</li>`)}</ul></div>`;
    const body = mr
      ? `${state.error ? `<div class="card">${errBox("刷新扩展市场失败：" + state.error, "markets")}</div>` : ""}${ms.map(market).join("") || `<div class="card"><div class="empty-row with-ico">${ico("store")}<div>还没有市场，在下面添加一个</div></div></div>`}`
      : state.error
        ? `<div class="card">${errBox("读取扩展市场失败：" + state.error, "markets")}</div>`
        : `<div class="card market" aria-busy="true"><div class="card-h market-h"><span class="skel-box sm" aria-hidden="true"></span><div class="market-t skel-lines"><span class="skel-line"></span><span class="skel-line short"></span></div></div><div class="rows">${skelRow(true)}${skelRow(true)}</div>${loadStatus("正在读取扩展市场…")}</div>`;
    return `${pageHead("扩展市场")}
      <div class="market-bar"><p class="ext-desc">订阅用量、飞书、微信和工作台是自带的，其他扩展从市场安装。市场就是一个 GitHub 仓库，根目录放 collie-marketplace.json（格式见 extensions/README.md）。</p>
        ${mr && ms.length ? `<button class="btn small" data-market-refresh>${ico("refresh-cw")}刷新</button>` : ""}</div>
      ${body}
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
    ["dm", "单聊", (c) => unread(c) && (c.kind === "dm" || c.kind === "self") && !isBot(c), "user", true],
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
  function inboxShown(r, channel, sub, query = "", matches) {
    const p = inboxPlace(r, channel, sub);
    let list = p.on ? listed(r).filter((c) => c.on)
      : (p.ch ? p.ch.convs.filter(SUBS.find(([k]) => k === p.sub)[2]) : []);
    const q = String(query || "").trim().toLowerCase();
    if (q) {
      list = list.filter((c) => {
        const hay = `${c.name || ""} ${c.chat || ""} ${c.preview || ""}`.toLowerCase();
        return hay.includes(q) || matches?.has(c.id);
      });
    }
    return list;
  }
  function hi(text, query) {
    const s = String(text ?? ""), q = String(query || "").trim();
    if (!q) return esc(s);
    const low = s.toLowerCase(), ql = q.toLowerCase();
    let out = "", pos = 0, idx;
    while ((idx = low.indexOf(ql, pos)) >= 0) {
      out += esc(s.slice(pos, idx)) + `<mark class="inbox-hl">${esc(s.slice(idx, idx + q.length))}</mark>`;
      pos = idx + q.length;
    }
    return out + esc(s.slice(pos));
  }
  // Unread chats, as the chat app's own badge counts them: muted ones aside.
  const unreadOf = (convs) => convs.filter((c) => c.unread > 0 && !c.muted).length;

  // The channel list: each channel with its unread chats (amber with an @
  // for you), then 已交给 AI apart. On narrow screens, a row of tabs.
  function channelsHTML(p, r, helpOpen = false) {
    const item = (key, lead, label, n, tone, on) => `<button type="button" class="inbox-ch${on ? " on" : ""}" data-inbox-channel="${esc(key)}"${on ? ' aria-current="true"' : ""}>${lead}<span class="inbox-ch-l">${esc(label)}</span>${n ? `<span class="inbox-ch-n${tone}">${unreadText(n)}</span>` : ""}</button>`;
    const ai = listed(r).filter((c) => c.on).length;
    return `<nav class="inbox-chs" aria-label="渠道">
        ${p.chs.map((ch) => item(ch.id, logo(ch.id) || ico("message-circle"), ch.label, unreadOf(ch.convs), ch.convs.some(atMe) ? " attn" : "", !p.on && p.ch?.id === ch.id)).join("")}
        <span class="inbox-chs-sep" aria-hidden="true"></span>
        ${item(AI_ON, ico("bot"), AI_HANDLED_LABEL, ai, " plain", p.on)}
        <button type="button" class="inbox-help-btn${helpOpen ? " on" : ""}" data-inbox-help aria-expanded="${!!helpOpen}" title="收件箱使用说明">${ico("help-circle")}<span>说明</span></button>
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

  function inboxListRowsHTML(r, cur, channel, sub, query = "", matches) {
    const p = inboxPlace(r, channel, sub);
    const shown = inboxShown(r, channel, sub, query, matches);
    const q = String(query || "").trim();
    const row = (c) => {
      const n = c.unread > 0 ? c.unread : 0, kind = kindLabel(c);
      const name = c.name || c.chat;
      const line = c.ext === "larkx" ? collieInboxMessage.legacyPreview(c.preview) : c.preview || "";
      const preview = c.kind === "dm" && line.startsWith(name + ": ") ? line.slice(name.length + 2) : line;
      return `<li class="row inbox-row${n ? " unread" : ""}${c.id === cur ? " sel" : ""}" data-conv="${esc(c.id)}">
        ${avatar(c, `${c.label} · ${kind}`)}
        <div class="inbox-main">
          <div class="inbox-l1"><b class="inbox-name"${name !== c.chat ? ` title="${esc(name)}"` : ""}>${hi(name, q)}</b>${c.muted ? `<span class="inbox-mute" title="在原应用里设了免打扰">${ico("bell-off")}</span>` : ""}${c.on ? `<span class="pill plain info inbox-on">AI</span>` : ""}<time class="inbox-time" datetime="${esc(c.last_at || "")}" title="${esc(when(c.last_at))}">${esc(shortTime(c.last_at))}</time></div>
          <div class="inbox-l2"><span class="inbox-prev">${atMe(c) ? `<span class="inbox-at">[有人@我]</span>` : ""}${hi(preview, q)}</span>${n ? `<span class="inbox-unread${c.muted ? " muted" : ""}" title="${n} 条未读${c.muted ? "（免打扰）" : ""}">${unreadText(n)}</span>` : ""}</div>
          ${c.on ? outLine(c.out) : ""}
        </div></li>`;
    };
    if (shown.length) return shown.map(row).join("");
    if (q) return `<li class="empty-row inbox-empty-q"><div>没有匹配「${esc(q)}」的会话</div><button type="button" class="btn small ghost" data-inbox-search-clear>清空搜索</button></li>`;
    return `<li class="empty-row">${p.sub ? "都读过了" : "没有会话"}</li>`;
  }

  function inboxHTML(r, cur, detail, channel, sub, opts = {}) {
    const convs = listed(r);
    const query = String(opts.query || "");
    const helpOpen = !!opts.helpOpen;
    const introText = `打开的扩展收到的会话都在这里。你本人发的、以触发词 <b>${esc(r?.trigger || "/ai")}</b> 开头的消息，在任何会话都会交给 Collie 或专属 agent，回复带尾标发回原会话；会话开关只管别人的消息。<br>交给专属 agent 时，/ai 后可加 alias 指定 agent，如 /ai agyn …；不写用默认`;
    if (!convs.length) return `<p class="panel-intro">${introText}</p><div class="card"><div class="empty-row with-ico">${ico("inbox")}<div>还没有会话。先在 <a href="#/auto" data-auto-tab="hooks">接入</a> 打开一个扩展并登录</div></div></div>`;
    const p = inboxPlace(r, channel, sub);
    const shown = inboxShown(r, channel, sub, query, opts.matches);
    const unreadHere = shown.some((c) => c.unread > 0);
    const readAll = `<button type="button" class="btn small ghost inbox-read-all" data-inbox-read-all aria-label="全部已读" ${unreadHere ? "" : "disabled"} title="把这个列表里的会话都标为已读">${ico("check-check")}<span>全部已读</span></button>`;
    const helpBar = helpOpen ? `<div class="inbox-help-bar"><p>${introText}</p><button type="button" class="icon-btn" data-inbox-help aria-label="收起说明">${ico("x")}</button></div>` : "";
    const chLabel = p.on ? AI_HANDLED_LABEL : p.ch?.label || "";
    const search = `<div class="inbox-search">
        ${ico("search")}
        <input type="search" value="${esc(query)}" placeholder="搜索${esc(chLabel)}会话或已保留消息…" aria-label="搜索会话" title="搜索会话名称、预览和 Collie 保留的消息正文" data-inbox-search autocomplete="off" spellcheck="false">
        <span class="inbox-search-meta"${query.trim() ? "" : " hidden"}><span class="inbox-search-cnt">${shown.length}</span><button type="button" class="inbox-search-clr" data-inbox-search-clear aria-label="清空搜索" title="清空搜索">${ico("x")}</button></span>
      </div>`;
    return `${helpBar}<div class="inbox-split ${cur ? "has-cur" : ""}">
        ${channelsHTML(p, r, helpOpen)}
        <div class="inbox-col">
          ${search}
          <div class="inbox-head">${subsHTML(p) || `<span class="inbox-head-t">${esc(chLabel)}<span class="muted">${shown.length}</span></span>`}${readAll}</div>
          <div class="card inbox-list"><ul class="rows" id="inbox-rows">${inboxListRowsHTML(r, cur, channel, sub, query, opts.matches)}</ul></div>
        </div>
        <div class="inbox-detail" id="inbox-detail">${detailHTML(cur, detail, r?.trigger, opts.detailOpts)}</div>
      </div>`;
  }

  const detailHTML = (cur, detail, trigger, opts = {}) => cur ? convHTML(detail, trigger, opts)
    : `<div class="card conv-empty-card"><div class="empty-row with-ico">${ico("messages-square")}<div><b>选一个会话</b><span class="muted">查看时间正序历史、直接回复、AI 帮回或带上下文发给 Collie</span></div></div></div>`;

  // Chronological order (oldest on top, newest at bottom), scrolled to latest on open.
  function convHTML(d, trigger, opts = {}) {
    if (!d) return `<div class="card conv-card"><div class="empty-row">正在读取…</div></div>`;
    const c = d.conv, msgs = d.msgs || [], outs = d.outbox || {};
    const chName = c.label || (c.ext === "wechat" ? "微信" : "飞书");
    const kindName = kindLabel(c);
    const seg = (name, val, optsList) => `<div class="seg">${optsList.map(([v, l]) => `<button type="button" data-conv-set="${name}" data-v="${v}" class="${String(val) === String(v) ? "on" : ""}">${l}</button>`).join("")}</div>`;
    const href = c.worker ? workerHref(c.worker) : "";
    const worker = c.worker ? (href ? `<a href="${esc(href)}">${esc(c.worker)} →</a>` : esc(c.worker)) : "";
    const subParts = [chName, kindName, c.on ? (c.handler === "agent" ? (c.worker || "专属 agent") : "Collie 处理") : ""].filter(Boolean);

    const RUN_GAP = 5 * 60e3;
    const who = (m) => (!m.human && (m.ref || m.bot)) ? "ai" : m.mine ? "me" : m.sender || m.sender_name || "";
    const unreadAnchor = opts.unreadAnchor || "";
    const unreadCount = opts.unreadCount || 0;
    const sendAs = c.send_as || (c.owner ? "bot" : "self");
    const canSend = sendAs !== "none";
    const botSend = sendAs === "bot";
    const hasAI = canSend && !botSend && c.ext !== "wechat";
    const noFooter = !!c.no_footer || botSend;
    const sendMedia = c.send_media || (c.ext === "larkx" ? "none" : (canSend ? "all" : "none"));

    // Surface failed/uncertain outbox items even when the AI switch is off (UX-33).
    const badOuts = Object.values(outs).filter((o) => o && (o.state === "failed" || o.state === "uncertain" || o.state === "expired"));
    const outAlert = badOuts.length ? `<div class="conv-banner warn">${outLine(badOuts[badOuts.length - 1])}</div>` : "";
    const sentBanner = opts.sentBanner ? `<div class="conv-banner info"><span>${esc(opts.sentBanner)}</span><a href="#/collie">查看对话 →</a><button type="button" class="icon-btn" data-conv-banner-close aria-label="关闭提示">${ico("x")}</button></div>` : "";

    const msg = (m, i, list) => {
      const o = m.ref ? outs[m.ref] : null;
      const rich = m.view && !m.ref;
      const text = c.ext === "larkx" ? collieInboxMessage.legacyPreview(m.text) : m.text;
      const day = dayLabel(m.at), prev = list[i - 1];
      const dayEl = !prev || dayLabel(prev.at) !== day ? `<li class="conv-day"><span>${esc(day)}</span></li>` : "";
      const unreadEl = unreadAnchor && m.id === unreadAnchor && unreadCount > 0
        ? `<li class="conv-unread-line" data-unread-anchor><span>以下是 ${unreadCount} 条未读消息</span></li>` : "";
      if (m.view?.kind === "system") return `${dayEl}${unreadEl}<li class="conv-sys">${collieInboxMessage.render(m.view)}</li>`;
      const ai = who(m) === "ai", mine = ai ? !c.owner : m.mine;
      // Chronological: a run's header sits on its first (top) message, or right after the unread divider.
      const head = !prev || !!unreadEl || who(prev) !== who(m) || dayLabel(prev.at) !== day || Date.parse(m.at) - Date.parse(prev.at) > RUN_GAP;
      const bot = ai && c.owner, pic = bot ? c.avatar : m.avatar;
      const name = bot ? c.name : ai ? "" : m.mine ? "你" : m.sender_name || "";
      const onlyLinkCards = rich && m.view.kind === "post" && !m.view.title && Array.isArray(m.view.nodes) && m.view.nodes.length > 0 && m.view.nodes.every((n) => n && n.kind === "link-card");
      const card = rich && (m.view.kind === "card" || m.view.kind === "calendar" || m.view.kind === "media" || onlyLinkCards);
      const body = rich ? collieInboxMessage.render(m.view, { convId: c.id, msgId: m.id }) : (m.ref && !m.human) ? md(m.text) : esc(text);
      return `${dayEl}${unreadEl}<li class="conv-msg ${mine ? "me" : "them"}${head ? " head" : ""}" data-msg-id="${esc(m.id || "")}">
        ${mine ? "" : `<span class="conv-ava${SQUARE.has(c.ext) ? " sq" : ""}" aria-hidden="true">${head ? `${ai ? ico("bot") : esc((name || "?").slice(0, 1))}${pic ? `<img class="inbox-pic" src="${esc(pic)}" alt="" loading="lazy" decoding="async">` : ""}` : ""}</span>`}
        <div class="conv-body">${head ? `<div class="meta">${name ? `<b>${esc(name)}</b>` : ""}${ai ? `<span class="conv-ai">${ico("bot")}AI 代答</span>` : ""}<time title="${esc(when(m.at))}">${esc(clock(m.at))}</time></div>` : ""}
          <div class="msg conv-bubble${card ? " bare" : ""} ${rich ? "im-rich" : (m.ref && !m.human) ? "assistant" : ""}">${body}</div>${o ? outLine(o) : ""}</div></li>`;
    };

    let mode = canSend ? (opts.composerMode || "reply") : "collie";
    if (mode === "ai" && !hasAI) mode = "reply";
    const inputText = opts.inputText || "";
    const draft = opts.draft || null;
    const showUnreadBtn = !!unreadAnchor && unreadCount > 0;
    const ctxN = Math.min(msgs.length, 20);
    const replyTabLabel = botSend ? "机器人发送" : "我直接回复";
    const sendBtnLabel = botSend ? "以机器人发送" : `发送到${chName}`;
    const confirmBtnLabel = `确认发送到${chName}`;
    const attachBtn = `<button type="button" class="icon-btn attach" title="添加图片或文件" aria-haspopup="menu" aria-expanded="false"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg></button>`;

    const draftProgress = (label) => `<span class="conv-draft-title"><span class="spinner small" aria-hidden="true"></span><b data-draft-stage role="status">${label}</b><span class="muted conv-draft-elapsed" data-draft-elapsed></span></span>`;
    let dockBody = "";
    if (mode === "reply") {
      const replyHint = botSend
        ? `${esc(chName)}仅支持以机器人「${esc(c.name || chName)}」身份发到你的${esc(chName)}（不能以你本人身份发送，支持文字/图片/文件）`
        : `以你本人身份直接发送到${esc(chName)}，不带 AI 尾标`;
      dockBody = `<div class="conv-dock-hint">${logo(c.ext)}<span>${replyHint}</span></div>
        <div class="cbox conv-cbox" data-send-media="${esc(sendMedia)}">
          <div class="files" hidden></div>
          <div class="conv-media-warn" data-conv-media-warn hidden></div>
          <textarea class="conv-input" rows="1" data-conv-input placeholder="${botSend ? `以机器人身份发到你的${esc(chName)}…` : `回复「${esc(c.name || c.chat)}」…`}">${esc(inputText)}</textarea>
          <div class="cbar conv-cbar">
            ${attachBtn}
            <span class="spacer"></span>
            <button type="button" class="btn primary small conv-act-btn" data-conv-send-reply>${ico("send")}<span>${esc(sendBtnLabel)}</span></button>
          </div>
          <input type="file" multiple hidden>
        </div>`;
    } else if (mode === "ai") {
      if (draft && draft.loading && !draft.text) {
        dockBody = `<div class="conv-draft conv-draft-wait">
            <div class="conv-draft-head">
              <span class="conv-draft-status">
                ${draftProgress("正在起草回复…")}
                <span class="muted conv-draft-context">生成后可编辑草稿</span>
              </span>
              <button type="button" class="btn small ghost" data-conv-draft-cancel>取消</button>
            </div>
          </div>`;
      } else if (draft && draft.text) {
        dockBody = `<div class="conv-draft">
            <div class="conv-draft-head">
              <span class="conv-draft-status">${draft.loading
                ? draftProgress("改写中…")
                : `<span class="conv-draft-title">${ico("sparkles")}<b>AI 草稿</b></span>`}
                <span class="muted conv-draft-context">确认后发往${esc(chName)}，尚未发出</span></span>
              <div class="conv-draft-tools">
                <button type="button" class="conv-chip-btn" data-conv-draft-refine="shorter" ${draft.loading ? "disabled" : ""}>${draft.loading ? "改写中…" : "更简短"}</button>
                <button type="button" class="conv-chip-btn" data-conv-draft-refine="formal" ${draft.loading ? "disabled" : ""}>更正式</button>
                <button type="button" class="icon-btn" data-conv-draft-cancel aria-label="取消草稿" title="放弃草稿">${ico("x")}</button>
              </div>
            </div>
            <div class="cbox conv-cbox" data-send-media="${sendMedia === "none" ? "draft" : esc(sendMedia)}">
              <div class="files" hidden></div>
              <div class="conv-media-warn" data-conv-media-warn hidden></div>
              <textarea class="conv-draft-text" rows="3" data-conv-draft-text aria-label="编辑 AI 起草的回复">${esc(draft.text)}</textarea>
              <div class="cbar conv-cbar conv-draft-foot">
                ${attachBtn}
                ${noFooter ? `<span class="muted conv-draft-note">由 ${esc(draft.via || "Collie")} 起草</span>` : `<label class="conv-draft-opt"><input type="checkbox" data-conv-draft-footer ${draft.footer ? "checked" : ""}><span>附带 AI 代答尾标</span></label>`}
                <span class="spacer"></span>
                <div class="conv-draft-acts">
                  <button type="button" class="btn small ghost" data-conv-draft-cancel>放弃</button>
                  <button type="button" class="btn primary small" data-conv-draft-send ${draft.loading ? "disabled" : ""}>${ico("check")}<span>${esc(confirmBtnLabel)}</span></button>
                </div>
              </div>
              <input type="file" multiple hidden>
            </div>
          </div>`;
      } else {
        dockBody = `<div class="conv-dock-hint">${ico("sparkles")}<span>AI 读取最近消息起草回复，由你审阅确认后再发送到${esc(chName)}</span></div>
          <div class="conv-quick-row">
            <button type="button" class="conv-chip-btn" data-conv-ai-quick="先回复收到，告知我会跟进">先回收到</button>
            <button type="button" class="conv-chip-btn" data-conv-ai-quick="说明正在排查，有进展同步">同步进展</button>
            <button type="button" class="conv-chip-btn" data-conv-ai-quick="礼貌说明改期到明天下午">礼貌改期</button>
          </div>
          <div class="cbox conv-cbox" data-send-media="draft">
            <div class="files" hidden></div>
            <div class="conv-media-warn" data-conv-media-warn hidden></div>
            <textarea class="conv-input" rows="1" data-conv-input placeholder="输入回复意图（可点 + 附截图让 AI 参考，留空则直接起草）…">${esc(inputText)}</textarea>
            <div class="cbar conv-cbar">
              ${attachBtn}
              <span class="spacer"></span>
              <button type="button" class="btn primary small conv-act-btn" data-conv-draft-gen ${draft?.loading ? "disabled" : ""}>${ico("sparkles")}<span>${draft?.loading ? "起草中…" : "生成草稿"}</span></button>
            </div>
            <input type="file" multiple hidden>
          </div>`;
      }
    } else {
      dockBody = `<div class="conv-dock-hint">${ico("bot")}<span>附带「${esc(c.name || c.chat)}」最近 ${ctxN || 1} 条消息上下文 · 仅发给 Collie，不发回${esc(chName)}</span></div>
        <div class="conv-quick-row">
          <button type="button" class="conv-chip-btn" data-conv-collie-quick="帮我总结这个会话的重点结论和待办">总结要点</button>
          <button type="button" class="conv-chip-btn" data-conv-collie-quick="根据会话里提到的报错和需求，帮我查一下相关代码">查相关代码</button>
          <button type="button" class="conv-chip-btn" data-conv-collie-quick="把会话里需要我处理的事情列个清单">提取待办</button>
        </div>
        <div class="cbox conv-cbox" data-send-media="all">
          <div class="files" hidden></div>
          <textarea class="conv-input" rows="1" data-conv-input placeholder="让 Collie 总结、查代码或派活处理…">${esc(inputText)}</textarea>
          <div class="cbar conv-cbar">
            ${attachBtn}
            <span class="spacer"></span>
            <button type="button" class="btn primary small conv-act-btn" data-conv-send-collie>${ico("bot")}<span>发给 Collie</span></button>
          </div>
          <input type="file" multiple hidden>
        </div>`;
    }

    return `<div class="card conv-card">
      <div class="card-h conv-head">
        <button type="button" class="icon-btn conv-back" data-conv-close aria-label="返回会话列表" title="返回会话列表">${ico("chevron-left")}</button>
        <div class="conv-title">
          ${avatar(c)}
          <div class="conv-title-meta">
            <div class="conv-title-name"${c.name && c.name !== c.chat ? ` title="${esc(c.chat)}"` : ""}>${esc(c.name || c.chat)}</div>
            <div class="conv-title-sub">${esc(subParts.join(" · "))}</div>
          </div>
        </div>
        <div class="conv-head-acts">
          <div class="conv-ai-pill" title="处理这个会话：打开后自动响应 ${esc(trigger || "/ai")}">
            <span class="conv-ai-lab">AI</span>
            <button type="button" class="switch" role="switch" aria-checked="${!!c.on}" aria-label="处理这个会话" data-conv-on></button>
          </div>
          <button type="button" class="icon-btn conv-cfg-btn${opts.settingsOpen ? " on" : ""}" data-conv-cfg aria-expanded="${!!opts.settingsOpen}" aria-label="会话设置" title="会话处理设置">${ico("sliders-horizontal")}</button>
        </div>
      </div>
      ${opts.settingsOpen ? `<div class="form conv-settings">
        <div class="conv-rule"><button type="button" class="switch" role="switch" aria-checked="${!!c.on}" aria-label="处理这个会话" data-conv-on></button><span><b>处理这个会话</b><small class="muted">打开后，以 ${esc(trigger || "/ai")} 开头的消息交给 agent，并保存会话上下文</small></span></div>
        ${c.on ? `${c.owner ? "" : `<div class="conv-rule"><span class="tm-lab">谁能唤起</span>${seg("anyone", !!c.anyone, [[false, "只有我"], [true, "任何人"]])}</div>
          ${c.anyone ? (c.ext === "larkx" ? `<div class="conv-rule"><span class="tm-lab conv-danger-label">允许危险操作</span>
            <button type="button" class="switch" role="switch" aria-checked="${!!c.allow_dangerous}" aria-label="允许危险操作" aria-describedby="conv-danger-desc" data-conv-danger></button></div>
            <p class="ext-warn" id="conv-danger-desc">关时，其他成员唤起的 Agent 不读私人资料、不执行高风险操作。<br>这是模型行为限制。开启后，本会话的任何成员都可以提出此类请求。</p>`
            : `<p class="ext-warn">任何人都能唤起时，专属 agent 在隔离目录里工作，但那不是沙箱：只对信任的群打开。</p>`) : ""}`}
          <div class="conv-rule"><span class="tm-lab">交给</span>${seg("handler", c.handler, [["collie", "Collie"], ["agent", "专属 agent"]])}${c.handler === "agent" ? `<span class="muted">${worker || "第一次唤起时启动"}</span>` : ""}</div>` : ""}
      </div>` : ""}
      ${outAlert}${sentBanner}
      <div class="conv-stage">
        ${msgs.length ? `<ul class="conv-msgs" aria-label="最近消息">${msgs.map(msg).join("")}</ul>`
          : `<div class="conv-msgs conv-msgs-empty"><div class="empty-row">${esc(c.ext === "larkx" ? collieInboxMessage.legacyPreview(c.preview) : c.preview || "")
            || (c.on ? "打开以后的消息会出现在这里" : "暂无消息")}</div></div>`}
        ${d.history_unavailable ? `<div class="conv-banner warn">暂时无法从飞书读取最近消息</div>` : ""}
        <div class="conv-jumps" aria-label="消息快速跳转">
          <button type="button" class="conv-jump conv-jump-unread" data-conv-jump-unread title="跳转到第一条未读消息"${showUnreadBtn ? "" : " hidden"}>${ico("arrow-up")}<span>${unreadCount} 条未读</span></button>
          <button type="button" class="conv-jump conv-jump-bottom" data-conv-jump-bottom aria-label="跳到最新消息" title="跳到最新消息" hidden>${ico("arrow-down")}</button>
        </div>
      </div>
      <div class="conv-dock" data-mode="${esc(mode)}">
        <div class="conv-modes${hasAI ? "" : " two"}" role="tablist" aria-label="回复或处理方式">
          <button type="button" role="tab" class="conv-mode${mode === "reply" ? " on" : ""}" aria-selected="${mode === "reply"}" data-conv-mode="reply" ${canSend ? "" : "disabled title=\"该扩展不支持直接发送\""}>${ico("send")}<span>${esc(replyTabLabel)}</span></button>
          ${hasAI ? `<button type="button" role="tab" class="conv-mode${mode === "ai" ? " on" : ""}" aria-selected="${mode === "ai"}" data-conv-mode="ai">${ico("sparkles")}<span>AI 帮回</span></button>` : ""}
          <button type="button" role="tab" class="conv-mode${mode === "collie" ? " on" : ""}" aria-selected="${mode === "collie"}" data-conv-mode="collie">${ico("bot")}<span>发给 Collie</span></button>
        </div>
        <div class="conv-dock-body">${dockBody}</div>
      </div>
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

  window.collieExt = { init, hooksList, extPage, replyPage, marketsHTML, pageHead, inboxHTML, inboxListRowsHTML, inboxShown, inboxPlace, convHTML, detailHTML, timerChannels, wechatAvatarField, label, logo, known, learn, stateOf };
})();

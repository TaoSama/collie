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

  // What the card says about where an extension is: off, running, signed in.
  function stateOf(v) {
    if (!v.setting.enabled) return { dot: "asleep", text: "已关闭" };
    if (v.problem && !v.proc) return { dot: "blocked", text: "连不上它的电脑：" + v.problem };
    const p = v.proc || { state: "starting" };
    if (p.state !== "running") return { dot: p.state === "error" ? "blocked" : "starting", text: (PROC[p.state] || p.state) + (p.error ? "：" + p.error : "") };
    const s = v.status;
    if (!s) return { dot: "starting", text: "运行中" };
    if (s.state === "ok") return { dot: "idle", text: "已登录" + (s.account ? " " + s.account : ""), ok: true };
    if (s.state === "not_signed_in") return { dot: "blocked", text: "未登录" + (s.error ? "：" + s.error : ""), login: true };
    return { dot: "blocked", text: "出错：" + (s.error || "") };
  }

  function machineOptions(ms, cur, pick) {
    return (pick ? `<option value="" disabled selected>选一台电脑</option>` : "") +
      ms.map((m) => `<option value="${esc(m.id)}" ${!pick && m.id === cur ? "selected" : ""}>${esc(m.name)}${m.online ? "" : "（离线）"}</option>`).join("");
  }

  // The actions it offers agents (collie ext do), and the switch for the
  // ones that change something.
  function actionsHTML(v) {
    const acts = v.manifest.contributes?.connector?.actions || [];
    if (!acts.length) return "";
    const writes = acts.some((a) => !a.readonly);
    return `<div class="ext-actions"><div class="tm-lab">给 agent 的动作（<code>collie ext do ${esc(v.manifest.id)} &lt;动作&gt;</code>）</div>
      <ul>${acts.map((a) => `<li><code>${esc(a.name)}</code>${a.readonly ? "" : ` <span class="tag attn">会改动</span>`}${a.untrusted ? ` <span class="tag">第三方内容</span>` : ""} <span class="muted">${esc(a.description)}</span></li>`).join("")}</ul>
      ${writes ? `<div class="conv-rule"><button type="button" class="switch" role="switch" aria-checked="${!v.setting.no_writes}" aria-label="允许会改动的动作" data-ext-writes></button><span>允许会改动的动作</span></div>` : ""}</div>`;
  }

  function configFields(v) {
    const props = v.manifest.configuration?.properties || {};
    return Object.entries(props).map(([k, p]) => {
      const val = v.setting.config?.[k] ?? p.default;
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

  function card(v, ms, login) {
    const m = v.manifest, st = stateOf(v), on = v.setting.enabled;
    const qr = login && login.id === m.id ? login : null;
    return `<div class="card ext-card" data-ext="${esc(m.id)}">
      <div class="card-h"><span class="ext-title"><span class="dot ${esc(st.dot)}"></span>${esc(m.name)} <span class="muted">${esc(m.version)}${m.runtime.builtin ? " · 内置" : ""}</span></span>
        <span class="switch-row"><span class="muted">${on ? "已打开" : "已关闭"}</span><button type="button" class="switch" role="switch" aria-checked="${on}" aria-label="打开 ${esc(m.name)}" data-ext-toggle></button></span></div>
      <div class="form">
        <p class="ext-desc">${esc(m.description)}</p>
        ${m.local ? `<p class="ext-warn">来自本地目录（${esc(m.id)}/pkg/${esc(m.version)}），Collie 不校验其内容。</p>` : ""}
        ${m.warning ? `<p class="ext-warn">${esc(m.warning)}</p>` : ""}
        <div class="ext-state">${esc(st.text)}</div>
        ${detailLine(v.status)}
        <div class="set-grid">
          ${m.local ? `<label>运行在<input value="${esc((ms.find((x) => x.id === (m.local_on || "")) || {}).name || m.local_on || "hub")}" disabled><small>本地扩展，在找到它的那台电脑上运行</small></label>`
            : `<label>运行在<select data-ext-machine data-pick="compact">${machineOptions(ms, v.setting.machine || "", m.defaults?.machine === "ask" && !v.setting.chosen)}</select><small>${m.defaults?.machine === "ask" ? "它的后端只在某一台电脑上，先选好再打开。" : ""}换电脑会先停掉原来的，再在新电脑上启动；登录状态留在原来那台</small></label>`}
          ${configFields(v)}
        </div>
        ${actionsHTML(v)}
        ${qr && qr.kind === "secret" ? `<form class="ext-secret" data-ext-secret="${esc(qr.step || "")}"><label>${esc(qr.label || "密钥")}<input type="password" name="secret" autocomplete="off" required><small>${esc(qr.hint || "")} 提交后交给扩展自己保存，Collie 不显示也不记录。</small></label>${qr.error ? `<p class="ext-warn">${esc(qr.error)}</p>` : ""}<div class="form-actions"><button class="btn primary small">提交</button></div></form>` : ""}
        ${qr && qr.kind !== "secret" ? `<div class="ext-qr">${qr.img ? `<img src="${esc(qr.img)}" alt="登录二维码">` : ""}<div><b>${qr.done ? "登录成功" : "用手机扫码登录"}</b><p class="muted">${esc(qr.hint || (qr.done ? "" : "二维码会自动刷新，扫完在手机上确认。"))}</p>${qr.error ? `<p class="ext-warn">${esc(qr.error)}</p>` : ""}</div></div>` : ""}
        <div class="form-actions">
          ${on && st.login ? `<button class="btn primary small" data-ext-login>${m.contributes?.connector?.login === "secret" ? "填写密钥" : "扫码登录"}</button>` : ""}
          ${on && st.ok ? `<button class="btn small" data-ext-logout>退出登录</button>` : ""}
          ${on && m.configuration ? `<button class="btn small" data-ext-save>保存设置</button>` : ""}
          ${v.proc?.logs?.length ? `<details class="ext-logs"><summary class="muted">最近日志</summary><pre>${esc(v.proc.logs.slice(-15).join("\n"))}</pre></details>` : ""}
        </div>
      </div></div>`;
  }

  // The top of 接入: each extension, then the trigger word and the footer.
  function hooksTop(r, login) {
    if (!r) return "";
    const list = r.extensions || [];
    return `<p class="panel-intro">扩展让 Collie 收发飞书、微信消息，默认都是关闭的：打开、扫码登录后才生效。消息先进 <a href="#/auto" data-auto-tab="inbox">收件箱</a>，你打开某个会话后，发触发词的消息才交给 agent，回复带上尾标发回原会话。</p>
      ${list.map((v) => card(v, r.machines || [], login)).join("") || `<div class="card"><div class="empty-row">没有可用的扩展</div></div>`}
      <div class="card"><div class="card-h">唤起与回复</div>
        <form class="form" id="ext-settings-form">
          <div class="set-grid">
            <label>触发词<input name="trigger" value="${esc(r.trigger || "")}" placeholder="${esc(r.default_trigger)}" autocorrect="off" autocapitalize="off" spellcheck="false"><small>打开处理的会话里，以它开头的消息才交给 agent；留空用 ${esc(r.default_trigger)}。微信的绑定人不需要</small></label>
            <label class="span2">回复尾标<input name="footer" value="${esc(r.footer || "")}" placeholder="${esc(r.default_footer)}" autocorrect="off" autocapitalize="off" spellcheck="false">
              <small>留空用默认。可用：{agent} {model} {effort} {elapsed} 耗时 {machine} 电脑 {worker} {ctx} 上下文 {time} {by} 谁唤起的。尾标也用来认出 Collie 自己发的消息</small></label>
          </div>
          <div class="form-actions"><button class="btn primary small">保存</button></div>
        </form></div>`;
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
    const intro = `<p class="panel-intro">打开的扩展收到的会话都在这里，默认都不处理。打开某个会话后，以触发词 <b>${esc(r?.trigger || "/ai")}</b> 开头的消息会交给 Collie 或这个会话的专属 agent，回复带尾标发回原会话。没打开的会话只留最后一条预览。</p>`;
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

  window.collieExt = { init, hooksTop, inboxHTML, convHTML, detailHTML, timerChannels, label, learn, stateOf };
})();

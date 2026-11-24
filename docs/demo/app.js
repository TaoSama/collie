/* Collie — web console + super-agent for herdr. No build step, no framework. */
(() => {
  "use strict";

  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const store = {
    get(k, d) { try { const v = localStorage.getItem("collie." + k); return v == null ? d : JSON.parse(v); } catch { return d; } },
    set(k, v) { try { localStorage.setItem("collie." + k, JSON.stringify(v)); } catch {} },
  };
  const narrow = () => matchMedia("(max-width: 760px)").matches;
  const AGENT_LABEL = { claude: "Claude Code", codex: "Codex", agy: "Antigravity" };
  // "上下文 12% · 120k / 1000k": how full a session's context window is.
  const fmtTokens = (n) => Math.round(n / 1000) + "k";
  const ctxText = (pct, used, window) => `上下文 ${pct}%` + (used && window ? ` · ${fmtTokens(used)} / ${fmtTokens(window)}` : "");
  // An Enter or Escape that belongs to the input method, not the page.
  const composing = (e) => e.isComposing || e.keyCode === 229;
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const tilde = (p) => (p || "").replace(/^\/Users\/[^/]+|^\/home\/[^/]+/, "~");
  // Built once: toLocale*String with options makes a formatter per call.
  const HHMM = new Intl.DateTimeFormat([], { hour: "2-digit", minute: "2-digit" });
  const FULL_TIME = new Intl.DateTimeFormat([], { year: "numeric", month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
  const MONTH_DAY = new Intl.DateTimeFormat([], { month: "numeric", day: "numeric" });
  const hhmm = (t) => { const d = new Date(t); return isNaN(d) ? "" : HHMM.format(d); };
  // 21:52 today, 昨天 18:30, 11月18日 18:30 before that.
  const sepTime = (t) => {
    const d = new Date(t), now = new Date(), y = new Date(now - 86400e3);
    if (d.toDateString() === now.toDateString()) return hhmm(t);
    if (d.toDateString() === y.toDateString()) return "昨天 " + hhmm(t);
    return `${d.getMonth() + 1}月${d.getDate()}日 ${hhmm(t)}`;
  };
  const fullTime = (t) => { const d = new Date(t); return isNaN(d) ? "" : FULL_TIME.format(d); };
  const when = (t) => {
    const d = new Date(t); if (isNaN(d)) return "";
    const today = new Date().toDateString() === d.toDateString();
    return today ? hhmm(t) : MONTH_DAY.format(d) + " " + hhmm(t);
  };

  // ───────────────────────────── icons & labels
  const GLYPHS = {
    claude: '<svg viewBox="0 0 32 32"><path d="M16 5v22M5 16h22M8.2 8.2l15.6 15.6M23.8 8.2L8.2 23.8"/></svg>',
    codex: '<svg viewBox="0 0 32 32"><path d="M16 4l10.4 6v12L16 28 5.6 22V10z"/><path d="M13 13l-3 3 3 3M19 13l3 3-3 3"/></svg>',
    agy: '<svg viewBox="0 0 32 32"><ellipse cx="16" cy="16" rx="11" ry="5" transform="rotate(-30 16 16)"/><path d="M16 21V9M12 13l4-4 4 4"/></svg>',
    agent: '<svg viewBox="0 0 32 32"><circle cx="16" cy="16" r="10"/><path d="M12 15h.01M20 15h.01M12 20c2.4 1.8 5.6 1.8 8 0"/></svg>',
    shell: '<svg viewBox="0 0 32 32"><rect x="4" y="6" width="24" height="20" rx="3"/><path d="M9 13l4 3-4 3M15 20h7"/></svg>',
    collie: '<img src="icon.svg" alt="" style="width:20px;height:20px">',
  };
  // The agents Collie starts, as the pickers name them; settings call agy
  // "antigravity".
  const AGENT_KINDS = [["claude", "Claude"], ["codex", "Codex"], ["agy", "Antigravity"]];
  const shortKind = (k) => (k === "antigravity" ? "agy" : k || "");
  const KIND_LABEL = { claude: "claude", codex: "codex", agy: "antigravity", antigravity: "antigravity" };
  const STATUS_LABEL = {
    idle: "空闲", working: "工作中", waiting: "在等后台任务", blocked: "等你确认", done: "已完成", unknown: "运行中", shell: "终端",
    asleep: "休眠中", starting: "启动中", gone: "已退出", failed: "启动失败",
  };
  const ROUTER = "collie";
  const isLocalRouter = (a) => a.is_router && !a.hub;
  const isRouterHistory = (h) => h.is_router ?? (h.name === ROUTER);
  // Collie is one conversation to the human; the session behind it is an
  // implementation detail. "done" just means a turn ended: it is idle.
  const COLLIE_STATUS = { asleep: "休眠中", starting: "正在唤醒", working: "在想…", blocked: "等你确认", failed: "出错了", dreaming: "在做梦…" };
  const SOURCE_LABEL = { worker: "派出的 agent", chat: "对话", timer: "定时", system: "Collie" };
  const collieStatus = (s) => COLLIE_STATUS[s] || "空闲";
  const collieDot = (s) => (s === "done" || s === "unknown" ? "idle" : s === "dreaming" ? "starting" : s || "asleep");
  const glyph = (v) => isLocalRouter(v) ? GLYPHS.collie : !v.is_agent ? GLYPHS.shell : (GLYPHS[v.kind] || GLYPHS.agent);

  // ───────────────────────────── api
  // A page left open keeps its old code after the hub is updated: when the
  // hub reports a different web app, offer a reload (once).
  let webBuild = null;
  function noteBuild(v) {
    if (!v) return;
    if (webBuild === null) { webBuild = v; return; }
    if (v === webBuild || document.getElementById("update-bar")) return;
    const bar = document.createElement("button");
    bar.id = "update-bar"; bar.type = "button";
    bar.textContent = "Collie 有新版本，点这里刷新";
    bar.onclick = () => location.reload();
    document.body.append(bar);
  }
  // ───────────────────────────── other hubs
  // Hubs on networks that can't reach each other (a devbox, a cloud
  // computer, a Mac mini at home) can all be reached from this browser.
  // Their sessions join this sidebar with every id tagged "!<key>"; any
  // request that names a tagged id goes straight to that hub, with its token.
  let hubs = [];              // [{ key, name, url, token }], kept on this hub
  const remote = new Map();   // key -> { state, ext, hist, online, es }
  const tagged = (v, k) => (v || "") + "!" + k;
  const hubMarks = (h) => ["!" + h.key, "%21" + h.key];
  const hubOf = (text) => hubs.find((h) => hubMarks(h).some((m) => text.includes(m))) || null;
  const untag = (text, h) => hubMarks(h).reduce((t, m) => t.split(m).join(""), text);
  const hubName = (k) => hubs.find((h) => h.key === k)?.name || "";
  function routeReq(path, body, hub) {
    const b = body === undefined ? undefined : JSON.stringify(body);
    const h = hub || hubOf(path + (b || ""));
    if (!h) return { url: path, body: b, hub: null };
    return { url: h.url + untag(path, h), body: b && untag(b, h), hub: h };
  }
  // Ids in a remote hub's answers get its tag, so follow-up requests route back.
  function tagIds(v, k, depth = 0) {
    if (depth > 3 || !v || typeof v !== "object") return v;
    if (Array.isArray(v)) return v.map((x) => tagIds(x, k, depth + 1));
    const out = { ...v };
    for (const f of ["pane_id", "workspace_id"]) if (typeof out[f] === "string" && out[f]) out[f] = tagged(out[f], k);
    if (typeof out.machine === "string") out.machine = tagged(out.machine, k);
    for (const [f, x] of Object.entries(out)) if (x && typeof x === "object") out[f] = tagIds(x, k, depth + 1);
    return out;
  }
  async function api(path, opts = {}) {
    const r = routeReq(path, opts.body, opts.hub);
    const headers = opts.body ? { "Content-Type": "application/json" } : {};
    if (r.hub) headers.Authorization = "Bearer " + r.hub.token;
    const res = await fetch(r.url, {
      method: opts.body ? "POST" : (opts.method || "GET"),
      headers, body: r.body, signal: opts.signal,
      credentials: r.hub ? "omit" : "same-origin",
    });
    if (!r.hub) noteBuild(res.headers.get("X-Collie-Web"));
    let data = {};
    try { data = await res.json(); } catch {}
    if (res.status === 401) {
      if (r.hub) throw new Error(`${r.hub.name} 不认这个浏览器了，在「设备」里重新添加它`);
      showLogin(); throw new Error("unauthorized");
    }
    if (!res.ok) throw new Error(data.error || res.statusText);
    return r.hub ? tagIds(data, r.hub.key) : data;
  }
  // EventSource can't set headers: a remote hub takes its token in the URL.
  function openES(path, hub) {
    const r = routeReq(path, undefined, hub);
    if (!r.hub) return new EventSource(path);
    return new EventSource(r.url + (r.url.includes("?") ? "&" : "?") + "access_token=" + encodeURIComponent(r.hub.token));
  }
  const paneURL = (id, rest) => `/api/panes/${encodeURIComponent(id)}/${rest}`;
  // ───────────────────────────── pickers
  // Every <select> keeps its value (forms read it as before) but shows a quiet
  // button and a menu that matches the app; on a phone the menu is a sheet.
  // Options may carry data-glyph (agent icon), data-dot (status) and data-sub.
  const pickPop = document.createElement("div");
  pickPop.className = "pick-pop"; pickPop.hidden = true; pickPop.setAttribute("role", "listbox");
  const pickBack = document.createElement("div");
  pickBack.className = "pick-back"; pickBack.hidden = true;
  document.body.append(pickBack, pickPop);
  let pickOpen = null;
  const optIcon = (o) => o.dataset.glyph ? `<span class="pi">${GLYPHS[o.dataset.glyph] || ""}</span>`
    : o.dataset.dot ? `<span class="pi"><span class="dot ${esc(o.dataset.dot)}"></span></span>` : "";
  function syncPick(sel) {
    const b = sel._pick;
    if (!b) return;
    const o = sel.selectedOptions[0];
    b.querySelector(".pv").innerHTML = o ? optIcon(o) + `<span class="pt">${esc(o.dataset.short || o.textContent)}</span>` : "";
    b.disabled = sel.disabled;
    b.hidden = sel.hidden;
    if (sel.title) b.title = sel.title;
  }
  function enhance(sel) {
    if (sel._pick || "native" in sel.dataset) return;
    const b = document.createElement("button");
    b.type = "button";
    b.className = "pick" + (sel.dataset.pick ? " " + sel.dataset.pick : "");
    if (sel.title) b.title = sel.title;
    b.setAttribute("aria-haspopup", "listbox");
    b.setAttribute("aria-expanded", "false");
    b.innerHTML = `<span class="pv"></span><svg class="pc" viewBox="0 0 24 24"><path d="M7 10l5 5 5-5"/></svg>`;
    sel._pick = b;
    sel.classList.add("picked");
    sel.tabIndex = -1;
    sel.setAttribute("aria-hidden", "true");
    sel.after(b);
    sel.addEventListener("focus", () => b.focus({ preventScroll: true }));
    b.addEventListener("click", (e) => { e.preventDefault(); e.stopPropagation(); pickOpen === sel ? closePick() : openPick(sel); });
    b.addEventListener("keydown", (e) => {
      if ((e.key === "ArrowDown" || e.key === "ArrowUp") && pickOpen !== sel) { e.preventDefault(); openPick(sel); }
    });
    syncPick(sel);
  }
  function openPick(sel) {
    closePick();
    pickOpen = sel;
    // A modal <dialog> sits in the top layer, above any z-index: the menu has
    // to live inside it to show.
    const host = sel.closest("dialog[open]") || document.body;
    if (pickPop.parentNode !== host) host.append(pickBack, pickPop);
    // Options without an icon keep its place when others have one, so the names line up.
    const iconGap = [...sel.options].some((o) => o.dataset.glyph || o.dataset.dot) ? `<span class="pi"></span>` : "";
    const row = (o) => `<button type="button" class="po${o.selected ? " on" : ""}" role="option" aria-selected="${o.selected}" data-v="${esc(o.value)}" ${o.disabled ? "disabled" : ""}>
      ${optIcon(o) || iconGap}<span class="pl"><span>${esc(o.textContent)}</span>${o.dataset.sub ? `<small>${esc(o.dataset.sub)}</small>` : ""}</span>
      <svg class="ck" viewBox="0 0 24 24"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg></button>`;
    let html = sel.dataset.title ? `<div class="ph">${esc(sel.dataset.title)}</div>` : "";
    for (const c of sel.children) {
      html += c.tagName === "OPTGROUP" ? `<div class="ph">${esc(c.label)}</div>` + [...c.children].map(row).join("") : row(c);
    }
    pickPop.innerHTML = html;
    const sheet = narrow();
    pickPop.classList.toggle("sheet", sheet);
    pickBack.hidden = !sheet;
    pickPop.hidden = false;
    sel._pick.setAttribute("aria-expanded", "true");
    if (sheet) {
      pickPop.style.cssText = "";
    } else {
      const r = sel._pick.getBoundingClientRect();
      const rich = [...sel.options].some((o) => o.dataset.sub);
      pickPop.style.minWidth = Math.max(r.width, rich ? 180 : 108) + "px";
      const w = pickPop.offsetWidth, h = pickPop.offsetHeight;
      const left = Math.min(Math.max(8, r.left), innerWidth - w - 8);
      const below = innerHeight - r.bottom - 8;
      const top = below >= h + 6 || below > r.top ? r.bottom + 6 : r.top - h - 6;
      pickPop.style.left = left + "px";
      pickPop.style.top = Math.max(8, top) + "px";
    }
    ($(".po.on", pickPop) || $(".po", pickPop))?.focus({ preventScroll: true });
    $(".po.on", pickPop)?.scrollIntoView({ block: "nearest" });
  }
  function closePick(refocus) {
    if (!pickOpen) return;
    const sel = pickOpen;
    pickOpen = null;
    pickPop.hidden = true;
    pickBack.hidden = true;
    sel._pick.setAttribute("aria-expanded", "false");
    if (refocus) sel._pick.focus({ preventScroll: true });
  }
  pickPop.addEventListener("click", (e) => {
    const o = e.target.closest(".po");
    if (!o || !pickOpen) return;
    const sel = pickOpen;
    closePick(true);
    if (sel.value === o.dataset.v) return;
    sel.value = o.dataset.v;
    syncPick(sel);
    sel.dispatchEvent(new Event("input", { bubbles: true }));
    sel.dispatchEvent(new Event("change", { bubbles: true }));
  });
  pickPop.addEventListener("keydown", (e) => {
    const items = $$(".po:not([disabled])", pickPop);
    const i = items.indexOf(document.activeElement);
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      items[(i + (e.key === "ArrowDown" ? 1 : -1) + items.length) % items.length]?.focus();
    } else if (e.key === "Escape" || e.key === "Tab") { e.preventDefault(); closePick(true); }
  });
  pickBack.addEventListener("click", () => closePick());
  document.addEventListener("mousedown", (e) => {
    if (pickOpen && !pickPop.contains(e.target) && !pickOpen._pick.contains(e.target)) closePick();
  });
  addEventListener("resize", () => closePick());
  document.addEventListener("scroll", (e) => { if (pickOpen && !pickPop.contains(e.target)) closePick(); }, true);
  document.addEventListener("change", (e) => { if (e.target.tagName === "SELECT") syncPick(e.target); });
  new MutationObserver((ms) => {
    for (const m of ms) {
      for (const n of m.addedNodes) {
        if (n.nodeType !== 1) continue;
        if (n.tagName === "SELECT") enhance(n); else n.querySelectorAll?.("select").forEach(enhance);
      }
      const s = m.target.closest?.("select");
      if (s) syncPick(s);
    }
  }).observe(document.body, { childList: true, subtree: true });
  $$("select").forEach(enhance);


  function toast(msg, kind = "") {
    const t = document.createElement("div");
    t.className = "toast " + kind;
    t.textContent = msg;
    $("#toasts").append(t);
    setTimeout(() => t.remove(), kind === "attn" ? 5000 : 2600);
  }

  // ───────────────────────────── auth
  function showLogin() {
    $("#app").hidden = true;
    $("#login").hidden = false;
    setTimeout(() => $("#login-token").focus(), 50);
  }
  async function login(token) {
    const res = await fetch("/api/login", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ token }) });
    if (!res.ok) throw new Error("bad token");
  }
  // A name the hub shows in its device list, e.g. "iPhone · Safari".
  function deviceName() {
    const ua = navigator.userAgent;
    const dev = /iPhone/.test(ua) ? "iPhone" : /iPad/.test(ua) ? "iPad" : /Android/.test(ua) ? "Android"
      : /Macintosh/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "设备";
    const br = /EdgA?\//.test(ua) ? "Edge" : /Chrome\//.test(ua) ? "Chrome" : /Firefox\//.test(ua) ? "Firefox" : /Safari\//.test(ua) ? "Safari" : "浏览器";
    return `${dev} · ${br}`;
  }
  async function pair(code) {
    const res = await fetch("/api/pair", { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code, kind: "browser", name: deviceName() }) });
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || "配对失败");
  }
  $("#login-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#login-error").textContent = "";
    const v = $("#login-token").value.trim();
    try {
      if (/^[A-Z2-7]{8}$/i.test(v)) await pair(v); else await login(v);
      boot();
    } catch (err) { $("#login-error").textContent = /^[A-Z2-7]{8}$/i.test(v) ? err.message : "令牌不对"; }
  });
  $("#logout").addEventListener("click", async () => { await fetch("/api/logout", { method: "POST" }); location.reload(); });

  // ───────────────────────────── live state
  let state = { ok: false, agents: [], shells: [] };
  let localState = state;     // this hub's own; state adds the other hubs'
  function tagState(s, h, online, routerPane) {
    const k = h.key;
    const fix = (a) => ({ ...a, hub: k, pane_id: tagged(a.pane_id, k), machine: tagged(a.machine, k),
      workspace_id: a.workspace_id ? tagged(a.workspace_id, k) : a.workspace_id,
      // Each hub has its own Collie; only this hub's is "Collie" here.
      ...((a.is_router ?? a.pane_id === routerPane) ? { name: "Collie · " + h.name, topic: "Collie · " + h.name } : {}) });
    const ms = s.machines && s.machines.length ? s.machines : [{ id: "", name: h.name, local: true, online: s.ok !== false }];
    return {
      agents: (s.agents || []).map(fix), shells: (s.shells || []).map(fix),
      machines: ms.map((m) => ({ ...m, hub: k, id: tagged(m.id, k), local: false,
        name: m.local ? h.name : h.name + " · " + m.name,
        online: online && (m.local ? s.ok !== false : m.online) })),
    };
  }
  function mergedState() {
    const parts = [...remote.values()].map((r) => r.state).filter(Boolean);
    if (!parts.length) return localState;
    return { ...localState,
      agents: [...(localState.agents || []), ...parts.flatMap((p) => p.agents)],
      shells: [...(localState.shells || []), ...parts.flatMap((p) => p.shells)],
      machines: [...(localState.machines && localState.machines.length ? localState.machines : [{ id: "", name: "本机", local: true, online: localState.ok }]),
        ...parts.flatMap((p) => p.machines)] };
  }
  function setLocalState(next) { localState = next; state = mergedState(); }
  function remoteChanged() {
    const m = mergedState();
    notifyTransitions(m);
    state = m;
    renderSide();
    if (view === "pane") renderHead();
  }
  function connectHub(h) {
    let r = remote.get(h.key);
    if (!r) { r = { state: tagState({}, h, false) }; remote.set(h.key, r); }
    r.es?.close();
    const es = r.es = openES("/api/events", h);
    let gone;
    // Older hubs identify their Router by pane ID in the super snapshot.
    es.addEventListener("super", (e) => {
      const routerPane = JSON.parse(e.data).router?.pane_id;
      if (r.routerPane === routerPane) return;
      r.routerPane = routerPane;
      if (r.online && r.raw?.agents?.some((a) => a.is_router == null)) {
        r.state = tagState(r.raw, h, true, routerPane);
        remoteChanged();
      }
    });
    es.addEventListener("state", (e) => {
      clearTimeout(gone);
      r.online = true; r.raw = JSON.parse(e.data);
      r.state = tagState(r.raw, h, true, r.routerPane);
      remoteChanged();
    });
    es.onerror = () => {
      // A blip reconnects by itself; only a hub that stays away goes grey.
      clearTimeout(gone);
      gone = setTimeout(() => { r.online = false; r.state = tagState({ ...(r.raw || {}), agents: [], shells: [] }, h, false); remoteChanged(); }, 8000);
      if (es.readyState === EventSource.CLOSED && r.es === es) setTimeout(() => { if (r.es === es) connectHub(h); }, 5000);
    };
  }
  async function loadHubs() {
    try { hubs = (await api("/api/hubs")).hubs || []; } catch { return; }
    for (const [k, r] of remote) if (!hubs.some((h) => h.key === k)) { r.es?.close(); remote.delete(k); }
    for (const h of hubs) if (!remote.get(h.key)?.es || remote.get(h.key).url !== h.url) { connectHub(h); remote.get(h.key).url = h.url; }
    state = mergedState();
    renderSide();
  }
  let sup = { router: { status: "asleep" }, entries: [], workers: [], queued: 0 };
  let prevStatus = new Map();
  let view = null;     // "collie" | "auto" | "pane" | null
  let current = null;  // pane id when view === "pane"

  function connect() {
    const ev = new EventSource("/api/events");
    ev.addEventListener("super", (e) => {
      sup = normSup(JSON.parse(e.data));
      renderSide();
      if (view === "collie") renderSuper();
    });
    ev.addEventListener("state", (e) => {
      localState = JSON.parse(e.data);
      const next = mergedState();
      notifyTransitions(next);
      state = next;
      renderSide();
      if (view === "pane") renderHead();
      if (view === "collie") renderSuper();
    });
    ev.onopen = () => $("#conn").className = "conn on";
    ev.onerror = async () => {
      $("#conn").className = "conn off";
      try { await api("/api/session"); } catch {}
      // A restart can answer with an error status, which closes the stream
      // for good; open a new one or the page silently goes stale.
      if (ev.readyState === EventSource.CLOSED && liveEv === ev) setTimeout(() => { if (liveEv === ev) connect(); }, 2000);
    };
    liveEv = ev;
  }
  let liveEv = null;
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && liveEv?.readyState === EventSource.CLOSED) connect();
  });

  // A finished agent you have open here has been seen: the hub then shows
  // it as idle everywhere (herdr itself only counts its own terminal).
  const seenAt = new Map();
  function markSeen(v) {
    if (v.status !== "done" || document.hidden || Date.now() - (seenAt.get(v.pane_id) || 0) < 3000) return;
    seenAt.set(v.pane_id, Date.now());
    api("/api/seen", { body: { pane: v.pane_id } }).catch(() => {});
  }
  document.addEventListener("visibilitychange", () => { if (view === "pane" && find(current)) markSeen(find(current)); });

  function notifyTransitions(next) {
    for (const a of next.agents) {
      const before = prevStatus.get(a.pane_id);
      if (before && before !== a.status) {
        if (a.status === "blocked") ping(`${title(a)} 需要你确认`, a, "attn");
        else if (before === "working" && (a.status === "done" || a.status === "idle") && !isWorker(a.name) && !isLocalRouter(a)) {
          ping(`${title(a)} 完成了`, a);
        }
      }
    }
    prevStatus = new Map(next.agents.map((a) => [a.pane_id, a.status]));
  }

  function ping(msg, a, kind) {
    if (a.pane_id !== current || document.hidden) {
      toast(msg, kind);
      if (document.hidden && prefs.notify() && "Notification" in window && Notification.permission === "granted") {
        const n = new Notification("Collie", { body: msg, icon: "/icon.svg", tag: a.pane_id });
        n.onclick = () => { focus(); location.hash = "#/p/" + encodeURIComponent(a.pane_id); };
      }
    }
  }

  // Tell the hub a page of Collie is in front of you: it holds back
  // notifications while you can see them here.
  const seen = () => { if (!document.hidden && document.hasFocus()) api("/api/super/seen", { body: { url: location.origin } }).catch(() => {}); };
  setInterval(seen, 30e3);
  document.addEventListener("visibilitychange", seen);
  addEventListener("focus", seen);

  // Optimistic pin toggles before the hub's next stream frame lands: id -> { pinned, item }.
  const pinnedLocal = new Map();
  const AT_BOTTOM_PX = 36;
  function normSup(s) {
    s.hasPending = Array.isArray(s.pending);
    s.entries ||= []; s.workers ||= []; s.pending ||= []; s.pinned ||= []; s.router ||= { status: "asleep" };
    // Preserve entries sliding off the front of the hub's 200-entry window when
    // earlier pages are loaded or the viewer is reading above the bottom, so an
    // anchored historical message never vanishes from the DOM on SSE updates.
    if (sup.entries?.length && s.entries.length) {
      const sc = $("#super-thread")?.parentElement;
      const keepSpilled = older.length > 0 || (view === "collie"
        ? !!(sc && sc.clientHeight > 0 && sc.scrollHeight - sc.scrollTop - sc.clientHeight > AT_BOTTOM_PX)
        : scrollByRoute.get("collie")?.atBottom === false);
      if (keepSpilled) {
        const nextIds = new Set(s.entries.map((e) => e?.id));
        const oldIds = new Set(older.map((e) => e?.id));
        const spilled = sup.entries.filter((e) => e?.id && !nextIds.has(e.id) && !oldIds.has(e.id));
        if (spilled.length) older = [...older, ...spilled];
      }
    }
    for (const [id, loc] of pinnedLocal) {
      if (s.pinned.some((p) => p?.id === id) === loc.pinned) pinnedLocal.delete(id);
    }
    return s;
  }
  const isWorker = (name) => sup.workers.some((w) => w.name === name && !w.ended);
  const routerPane = () => state.agents.find(isLocalRouter)?.pane_id;
  const paneOf = (name) => name === ROUTER ? routerPane() : state.agents.find((a) => a.name === name)?.pane_id;
  // A worker's status as the sidebar shows it (which knows when it waits on
  // background work), else as Collie last saw it.
  const workerStatus = (w) => (!w.ended && state.agents.find((a) => a.name === w.name)?.status) || w.status;
  const workerBusy = (w) => !w.ended && ["working", "starting", "waiting"].includes(workerStatus(w));
  const find = (id) => state.agents.find((a) => a.pane_id === id) || state.shells.find((a) => a.pane_id === id);
  // What to call an agent: the session's own title; a handle like "agy-1" says
  // nothing, so until there's a title use the folder it works in.
  const autoName = (n) => /^(claude|codex|agy|antigravity)-\d+$/.test(n || "");
  const base = (p) => (p || "").replace(/\/+$/, "").split("/").pop() || "~";
  const termName = (v) => v.workspace_label || base(tilde(v.cwd));
  function title(v) {
    if (!v) return "";
    if (isLocalRouter(v)) return "Collie";
    if (isWorker(v.name)) return v.name; // Collie talks about its agents by name
    return v.topic || (v.name && !autoName(v.name) ? v.name : base(tilde(v.cwd))) || v.pane_id;
  }

  // ───────────────────────────── sidebar
  function itemHTML(v, attrs = "") {
    const where = (state.machines || []).length > 1 ? machineName(v.machine) + " · " : "";
    const named = v.is_agent && !isLocalRouter(v) && !isWorker(v.name) && v.name && !autoName(v.name) && v.topic && v.topic !== v.name ? v.name + " · " : "";
    const sub = isLocalRouter(v) ? "Collie 停在一个对话框上"
      : v.is_agent ? `${named}${KIND_LABEL[v.kind] || v.kind} · ${where}${base(tilde(v.cwd))}` : where + tilde(v.cwd);
    return `<li><a class="item ${view === "pane" && v.pane_id === current ? "active" : ""}"${attrs} href="#/p/${encodeURIComponent(v.pane_id)}">
      <span class="glyph">${glyph(v)}</span>
      <span class="body"><div class="name">${isCollieOwned(v) && !isLocalRouter(v) ? `<span class="nm">${esc(title(v))}</span><span class="by-collie">Collie 派出</span>` : esc(v.is_agent ? title(v) : termName(v))}</div><div class="sub">${esc(sub)}</div></span>
      <span class="dot ${esc(v.status)}" title="${esc(STATUS_LABEL[v.status] || v.status)}"></span></a></li>`;
  }

  const machineName = (id) => (state.machines || []).find((m) => m.id === (id || ""))?.name || id || "本机";
  const isCollieOwned = (v) => isLocalRouter(v) || isWorker(v.name);

  // "09:00", "明天 09:00", "11月24日 09:00".
  function whenShort(at) {
    const d = new Date(at), hm = d.toTimeString().slice(0, 5);
    const days = Math.round((new Date(d).setHours(0, 0, 0, 0) - new Date().setHours(0, 0, 0, 0)) / 864e5);
    return days === 0 ? hm : days === 1 ? "明天 " + hm : `${d.getMonth() + 1}月${d.getDate()}日 ${hm}`;
  }
  function renderSide() {
    const blocked = state.agents.filter((a) => a.status === "blocked");
    const machines = (state.machines || [{ id: "", name: "本机", local: true, online: state.ok }]);
    const multi = machines.length > 1;

    renderSessions();

    const online = machines.filter((m) => m.local ? state.ok : m.online).length;
    $("#conn").className = "conn " + (online ? "on" : "off");
    $("#herdr-status").textContent = !online ? "herdr 不可用" : multi ? `${online}/${machines.length} 台电脑在线` : `${state.agents.length} 个 agent`;
    if (!state.ok && state.error) $("#herdr-status").title = state.error;
    if (view !== "file") document.title = blocked.length ? `(${blocked.length}) Collie` : "Collie";

    const r = sup.router || {};
    const active = sup.workers.filter((w) => !w.ended);
    const running = active.filter(workerBusy).length;
    $("#nav-collie-sub").textContent = collieStatus(r.status) + (running ? ` · ${running} 个任务在跑` : active.length ? ` · 派出 ${active.length} 个` : "");
    // The next time Collie set itself to look again, under 自动化 where its
    // timers are: "明天 09:00 再看：复查 paper_df…".
    const f = sup.follow;
    $("#nav-auto-sub").textContent = f?.next ? `${whenShort(f.next)} 再看：${f.next_text || ""}` : "收件箱 · 定时 · 接入";
    $("#nav-auto-sub").title = f?.next ? `Collie 设的下一次跟进：${f.next_text || ""}（点开看进行中的任务）` : "";
    $("#nav-auto-sub").classList.toggle("follow", !!f?.next);
    $("#nav-collie-dot").className = "dot " + collieDot(r.status);
    const pend = activePending();
    const np = $("#nav-pending");
    if (np) {
      np.hidden = pend.length === 0;
      np.textContent = pend.length || "";
      np.title = pend.length ? `${pend.length} 条待你确认` : "";
    }
    $("#nav-queue").hidden = !sup.queued;
    $("#nav-queue").textContent = sup.queued;
    $("#nav-collie").classList.toggle("active", view === "collie");
    $("#nav-auto").classList.toggle("active", view === "auto");
    $("#nav-devices").classList.toggle("active", view === "devices");
    $("#nav-prefs").classList.toggle("active", view === "prefs");
  }

  // A column edge dragged with mouse, finger or pen. onStart measures the
  // layout once (reading it while the width changes would reflow on every
  // move); onMove gets x and that, at most once a frame; onEnd runs once the
  // pointer lets go (or the system takes it away).
  function dragGrip(grip, { onStart, onMove, onEnd }) {
    grip.addEventListener("pointerdown", (e) => {
      if (e.button !== 0 || grip.classList.contains("on")) return;
      const bounds = onStart();
      if (bounds === null) return;
      e.preventDefault();
      grip.setPointerCapture(e.pointerId);
      document.body.classList.add("dragging"); grip.classList.add("on");
      let x = null, raf = 0, moved = false;
      const apply = () => { raf = 0; if (x !== null) { moved = true; onMove(x, bounds); } };
      const move = (ev) => { if (ev.pointerId !== e.pointerId) return; x = ev.clientX; raf ||= requestAnimationFrame(apply); };
      grip.addEventListener("pointermove", move);
      grip.addEventListener("lostpointercapture", () => {
        grip.removeEventListener("pointermove", move);
        if (raf) cancelAnimationFrame(raf), apply();
        document.body.classList.remove("dragging"); grip.classList.remove("on");
        if (moved) onEnd();
      }, { once: true });
    });
  }
  // The conversation keeps at least this much beside the sidebar and a file.
  const MAIN_MIN = 420;
  // The sidebar's edge drags wider or narrower (not on a phone, where the
  // sidebar is the whole screen); a double-click puts it back.
  const SIDE_MIN = 220, SIDE_MAX = 520;
  const sideMax = () => {
    const file = $("#app").classList.contains("has-file") ? $("#file-view").offsetWidth : 0;
    return Math.max(SIDE_MIN, Math.min(SIDE_MAX, innerWidth - MAIN_MIN - file));
  };
  function setSideW(w, max = sideMax()) {
    const root = document.documentElement.style;
    if (!w) return root.removeProperty("--side-w");
    root.setProperty("--side-w", Math.round(Math.max(SIDE_MIN, Math.min(w, max))) + "px");
  }
  setSideW(store.get("sideW", 0));
  addEventListener("resize", () => setSideW(store.get("sideW", 0)));
  let sideDragW = 0;
  dragGrip($("#side-grip"), {
    onStart: () => narrow() ? null : sideMax(),
    onMove: (x, max) => { sideDragW = Math.round(Math.max(SIDE_MIN, Math.min(x, max))); setSideW(sideDragW, max); },
    onEnd: () => { store.set("sideW", sideDragW); fitTerm(); },
  });
  $("#side-grip").addEventListener("dblclick", () => { store.set("sideW", 0); setSideW(0); fitTerm(); });

  // ───────────────────────────── routing & scroll restoration
  // Track in-app history depth on history.state so the header back button pops
  // back to the exact caller view when available, and falls back inside Collie
  // on deep links or fresh standalone PWA launches without exiting the app.
  let navCur = typeof history.state?.collieIdx === "number" ? history.state.collieIdx : 0;
  let booted = false;
  // Saved reading position per history entry (collieIdx) and per logical route.
  const scrollByIdx = new Map();
  const scrollByRoute = new Map();
  let programmaticScrollUntil = 0;
  let collieRestoreRAF = 0;
  let activeCollieRestore = null;
  let scrollTrackRAF = 0;

  const markProgrammaticScroll = (ms = 90) => {
    programmaticScrollUntil = Math.max(programmaticScrollUntil, performance.now() + ms);
  };
  function routeKeyFor(v, id) {
    if (v === "collie") return "collie";
    if (v === "auto") return "auto:" + autoTab;
    if (v === "devices") return "devices:" + devTab;
    if (v === "prefs") return "prefs";
    if (v === "file") return "file:" + (id || "");
    if (v === "ext") return "ext:" + (id || "");
    if (v === "pane") return "pane:" + (id || "") + ":" + ($("#viewer")?.dataset.mode || curMode());
    return "";
  }
  function viewScroller(v = view) {
    if (v === "collie") return $("#super-thread")?.parentElement || null;
    if (v === "auto") return $("#auto-body");
    if (v === "devices") return $("#devices-body");
    if (v === "prefs") return $("#prefs-body");
    if (v === "file") return $("#rv-scroller");
    if (v === "ext") return $("#x-thread")?.parentElement || null;
    if (v === "pane") {
      const m = $("#viewer")?.dataset.mode || curMode();
      if (m === "screen") return $("#screen");
      if (m === "log") return $("#log");
      return scroller($("#thread"));
    }
    return null;
  }
  function captureCollieAnchor(sc) {
    const box = $("#super-thread");
    if (!box || !sc || sc.clientHeight <= 0) return null;
    const msgs = box.querySelectorAll(".msg[data-id]");
    if (!msgs.length) return null;
    const sr = sc.getBoundingClientRect();
    const edge = sr.top + 4;
    let lo = 0, hi = msgs.length - 1, pick = msgs[msgs.length - 1];
    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      const mr = msgs[mid].getBoundingClientRect();
      if (mr.bottom > edge) {
        pick = msgs[mid];
        hi = mid - 1;
      } else {
        lo = mid + 1;
      }
    }
    const anchorId = pick?.dataset.id || "";
    if (!anchorId) return null;
    return { anchorId, anchorOffset: pick.getBoundingClientRect().top - sr.top };
  }
  function applyCollieAnchor(sc, snap) {
    if (!sc || !snap) return false;
    const box = $("#super-thread");
    if (snap.anchorId && box) {
      const el = box.querySelector(`.msg[data-id="${CSS.escape(snap.anchorId)}"]`);
      if (el) {
        const sr = sc.getBoundingClientRect();
        const mr = el.getBoundingClientRect();
        markProgrammaticScroll();
        sc.scrollTop += (mr.top - sr.top) - snap.anchorOffset;
        return true;
      }
    }
    markProgrammaticScroll();
    sc.scrollTop = snap.scrollTop;
    return false;
  }
  function captureViewScroll(v = view, id = (v === "ext" ? extCur : current), idx = navCur) {
    if (!v) return null;
    const sc = viewScroller(v);
    if (!sc || sc.clientHeight <= 0) return null;
    const dist = sc.scrollHeight - sc.scrollTop - sc.clientHeight;
    const atBottom = dist <= AT_BOTTOM_PX;
    const routeKey = routeKeyFor(v, id);
    const snap = { routeKey, atBottom, scrollTop: sc.scrollTop, anchorId: "", anchorOffset: 0 };
    if (v === "collie" && !atBottom) {
      const anc = captureCollieAnchor(sc);
      if (anc) {
        snap.anchorId = anc.anchorId;
        snap.anchorOffset = anc.anchorOffset;
      }
    }
    if (typeof idx === "number") scrollByIdx.set(idx, snap);
    if (routeKey) scrollByRoute.set(routeKey, snap);
    return snap;
  }
  function restoreSimpleScroll(el, snap) {
    if (!el || !snap) return;
    const apply = () => {
      if (el.clientHeight <= 0) return;
      markProgrammaticScroll();
      el.scrollTop = snap.atBottom ? el.scrollHeight : snap.scrollTop;
    };
    apply();
    if (typeof requestAnimationFrame === "function") requestAnimationFrame(apply);
  }
  document.addEventListener("scroll", (ev) => {
    if (!booted || !view || performance.now() < programmaticScrollUntil) return;
    const sc = viewScroller(view);
    if (!sc || ev.target !== sc || sc.clientHeight <= 0) return;
    if (scrollTrackRAF) return;
    scrollTrackRAF = requestAnimationFrame(() => {
      scrollTrackRAF = 0;
      if (!view || performance.now() < programmaticScrollUntil) return;
      captureViewScroll(view, view === "ext" ? extCur : current, navCur);
    });
  }, { capture: true, passive: true });

  function syncHistoryIndex() {
    if (typeof history.state?.collieIdx === "number") {
      navCur = history.state.collieIdx;
    } else {
      navCur = booted ? navCur + 1 : 0;
      history.replaceState({ ...(history.state || {}), collieIdx: navCur }, "");
    }
  }
  function replaceHash(hash) {
    history.replaceState({ ...(history.state || {}), collieIdx: navCur }, "", hash);
  }
  function fallbackHash() {
    if (!narrow()) return "#/collie";
    if (view === "pane") {
      const v = find(current);
      if (v && isCollieOwned(v)) return "#/collie";
    } else if (view === "ext" && extCur?.startsWith("h:")) {
      const h = hist.find((x) => x.id === extCur.slice(2));
      if (h && (isRouterHistory(h) || sup.workers.some((w) => w.name === h.name || (h.session && w.session === h.session)))) return "#/collie";
    }
    return "#/";
  }
  function goBack(replaceAtRoot = false) {
    if (navCur > 0) {
      history.back();
      return;
    }
    const dest = fallbackHash();
    if (location.hash === dest || (!location.hash && dest === "#/")) return;
    if (replaceAtRoot) { replaceHash(dest); route(); }
    else location.hash = dest;
  }
  document.addEventListener("click", (e) => {
    if (e.target.closest(".back")) { e.preventDefault(); goBack(); return; }
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target.closest('a[href^="#/"]');
    if (a && a.getAttribute("href") === location.hash) e.preventDefault();
  });

  function route() {
    const prevView = view, prevId = view === "ext" ? extCur : current, prevIdx = navCur;
    const wasExistingIdx = typeof history.state?.collieIdx === "number";
    syncHistoryIndex();
    let h = location.hash;
    let next = null, id = null;
    if (h.startsWith("#/p/")) { next = "pane"; id = decodeURIComponent(h.slice(4)); }
    else if (h.startsWith("#/x/")) { next = "ext"; id = decodeURIComponent(h.slice(4)); }
    else if (h.startsWith("#/h/")) { next = "ext"; id = "h:" + decodeURIComponent(h.slice(4)); }
    else if (h === "#/file" || h.startsWith("#/file?")) { next = "file"; id = h.slice(6); }
    else if (h === "#/collie") next = "collie";
    else if (h === "#/auto") next = "auto";
    else if (h === "#/devices") next = "devices";
    else if (h === "#/prefs") next = "prefs";
    else if (!narrow()) {
      // Desktop has no empty home: reopen where you were on fresh launch, else Collie.
      let dest = "#/collie";
      if (!booted && !h) {
        const last = store.get("last", "#/collie");
        const ok = last.startsWith("#/p/") ? !!find(decodeURIComponent(last.slice(4))) : /^#\/(collie|auto|devices|prefs)$/.test(last);
        if (ok) dest = last;
      }
      replaceHash(dest);
      h = dest;
      if (h.startsWith("#/p/")) { next = "pane"; id = decodeURIComponent(h.slice(4)); }
      else next = h.slice(2);
    }
    if (next && next !== "file") store.set("last", h);
    if (next === view && id === (next === "ext" ? extCur : current)) return;
    // Snapshot the departing view right before its DOM is hidden.
    if (prevView && performance.now() >= programmaticScrollUntil) {
      captureViewScroll(prevView, prevId, prevIdx);
    }
    if ($("#app").classList.contains("has-file") && (next === "file" || matchMedia("(max-width: 1000px)").matches)) closeFilePane();
    if (view === "ext" && (next !== "ext" || id !== extCur)) { closeExt(); extCur = null; }
    if (view === "pane" && (next !== "pane" || id !== current)) closeStream();
    if (view === "auto" && next !== "auto") clearInterval(autoTimer);
    if (view === "devices" && next !== "devices") clearInterval(devTimer);
    if (next !== "collie") {
      pendingOpen = false;
      hideSearchPanel();
    }
    view = next; current = next === "ext" ? null : id;
    const nextKey = routeKeyFor(next, id);
    const byIdx = wasExistingIdx ? scrollByIdx.get(navCur) : null;
    const saved = (byIdx && byIdx.routeKey === nextKey ? byIdx : null)
      || (next === "collie" || next === "auto" || next === "devices" || next === "prefs" || next === "file" ? scrollByRoute.get(nextKey) : null);

    document.body.classList.toggle("reading-file", next === "file");
    document.body.classList.toggle("show-main", !!next);
    $("#v-collie").hidden = next !== "collie";
    $("#v-auto").hidden = next !== "auto";
    $("#v-devices").hidden = next !== "devices";
    $("#v-prefs").hidden = next !== "prefs";
    $("#v-ext").hidden = next !== "ext";
    $("#detail").hidden = next !== "pane";
    $("#v-file").hidden = next !== "file";
    renderSide();
    if (next === "collie") {
      if (saved && !saved.atBottom) renderSuper({ force: true, restore: saved });
      else renderSuper({ force: true, scrollBottom: true });
      renderTarget(true);
      if (!narrow() && ($("#super-search")?.hidden ?? true)) $("#super-input").focus({ preventScroll: true });
    }
    if (next === "auto") openAuto(saved);
    if (next === "devices") openDevices(saved);
    if (next === "prefs") renderPrefs(saved);
    if (next === "file") openWideFile(id, saved);
    if (next === "ext") openExt(id, saved);
    if (next === "pane") {
      $("#gone").hidden = true;
      $("#log").textContent = "";
      resetChat();
      resetScreen();
      renderHead();
      openStream(saved);
      if (!narrow()) $("#input").focus({ preventScroll: true });
    }
  }
  addEventListener("popstate", route);
  addEventListener("hashchange", route);
  addEventListener("resize", () => { if (booted && !view && !narrow()) route(); });

  // ───────────────────────────── Collie (super-agent) view
  let superSig = "";
  // Which model Collie itself runs with, next to its status.
  async function renderRouterModel(r) {
    const el = $("#router-model");
    const key = [r.kind, r.model, r.effort, r.pending, r.machine, r.alias].join("|");
    if (el._key === key) return;
    el._key = key;
    let model = r.model, effort = r.effort;
    if ((!model || !effort) && r.kind) {
      const info = await modelsFor(shortKind(r.kind), r.machine || "");
      model ||= info.default || "";
      effort ||= info.default_effort || "";
    }
    if (el._key !== key) return;
    const parts = [];
    if (model) parts.push(esc(shortModel(model)) + (r.model ? "" : "（默认）"));
    if (effort) parts.push("思考 " + esc(EFFORT_LABEL[effort] || effort) + (r.effort ? "" : "（默认）"));
    el.innerHTML = parts.join(" · ") + (r.pending ? ` <button type="button" class="pending" data-act="router-restart" title="Collie 的模型设置改过了，点这里重启 Collie">重启后生效</button>` : "");
    el.title = `Collie 自己用的模型${model ? "：" + model : ""}${effort ? "，思考强度 " + effort : ""}（自动化 → 设置 → Collie 自己）`;
  }
  // A worker's comings and goings: its spawn line shows where it stands, and
  // a dialog waiting on the human shows as a card below. Reports and errors stay.
  const CHATTER = /\s(完成了一轮|等你确认|已停止|已退出|回来了（herdr 恢复了它的窗格）)$/;
  const workerChatter = (e) => e.ref && e.ref !== ROUTER && (e.tone === "done" || e.tone === "info" || e.tone === "blocked") && CHATTER.test(e.text);
  // Messages waiting for Collie itself (an older hub only counts them all).
  const routerQueued = () => sup.queued_router ?? sup.queued;
  // Messages you sent that the hub's stream hasn't shown yet.
  const pendingSays = [];
  // Items dismissed locally via × before the hub's next stream frame lands.
  const dismissedLocal = new Set();
  const ICON_X = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 7l10 10M17 7L7 17"/></svg>`;
  const ICON_PIN = `<svg viewBox="0 0 24 24" aria-hidden="true"><path class="pin-body" d="M9.5 3.5h5v5l3 3v2h-11v-2l3-3z"/><path d="M8.5 3.5h7M12 13.5v7"/></svg>`;
  let pendingOpen = false, pendingBarSig = "", pendingListSig = "", flashEntry = { id: "", until: 0 }, jumpSeq = 0, superJumpHiding = false;
  const SUPER_JUMP_DIST = 160;

  function scrollToAsync(sc, top, behavior = "smooth") {
    const res = sc.scrollTo({ top, behavior });
    if (res instanceof Promise) return res;
    if (behavior !== "smooth" || Math.abs(sc.scrollTop - top) < 2) return Promise.resolve();
    return new Promise((resolve) => {
      let tid;
      const done = () => {
        clearTimeout(tid);
        sc.removeEventListener("scrollend", done);
        sc.removeEventListener("scroll", onScroll);
        resolve();
      };
      const onScroll = () => { clearTimeout(tid); tid = setTimeout(done, 110); };
      sc.addEventListener("scrollend", done, { once: true });
      sc.addEventListener("scroll", onScroll, { passive: true });
      tid = setTimeout(done, 120);
    });
  }
  function updateSuperJump() {
    const btn = $("#super-jump"), sc = $("#super-thread")?.parentElement;
    if (!btn || !sc) return;
    const dist = sc.scrollHeight - sc.scrollTop - sc.clientHeight;
    if (dist <= 24) superJumpHiding = false;
    const show = view === "collie" && !pendingOpen && !superJumpHiding && dist > SUPER_JUMP_DIST;
    if (btn.hidden === !show) return;
    const hadFocus = !show && document.activeElement === btn;
    btn.hidden = !show;
    if (hadFocus) sc.focus({ preventScroll: true });
  }

  function optimisticallyResolved(id, list = chatEntries()) {
    if (dismissedLocal.has(id)) return true;
    if (!pendingSays.length) return false;
    if (pendingSays.some((m) => m.re?.entry === id)) return true;
    if (!pendingSays.some((m) => !m.re?.entry)) return false;
    const idx = list.findIndex((e) => e.id === id);
    if (idx < 0) return false;
    for (let j = idx + 1; j < list.length; j++) {
      if (list[j].role === "collie") return false;
    }
    return true;
  }
  function activePending() {
    const list = sup.pending || [];
    if (!list.length) return [];
    const entries = pendingSays.length ? chatEntries() : sup.entries;
    return list.filter((p) => p?.id && !optimisticallyResolved(p.id, entries));
  }
  function clientPinSummary(e) {
    if (!e) return "";
    let t = String(e.text || "");
    if (e.role === "user") {
      const cut = t.indexOf("\n\n[The human picked");
      if (cut >= 0) t = t.slice(0, cut);
      const att = t.lastIndexOf("附件：\n");
      if (att >= 0) t = (t.slice(0, att).trim() + " 📎").trim();
    }
    return plainLine(t.replace(/```[\s\S]*?```/g, " ").replace(/\[image:[^\]]+\]/g, " ")).slice(0, 120) || plainLine(t).slice(0, 120);
  }
  function activePinned() {
    const base = (sup.pinned || []).filter((p) => p?.id && pinnedLocal.get(p.id)?.pinned !== false);
    const have = new Set(base.map((p) => p.id));
    const added = [];
    for (const [id, loc] of pinnedLocal) {
      if (loc.pinned && !have.has(id) && loc.item) added.push(loc.item);
    }
    return added.length ? [...base, ...added] : base;
  }
  const pinDisplaySummary = (p) => (p.role === "user" && p.summary && !p.summary.startsWith("你：") ? "你：" + p.summary : p.summary || "");
  async function togglePin(id, forceVal) {
    if (!id) return;
    const cur = activePinned().some((p) => p.id === id);
    const next = typeof forceVal === "boolean" ? forceVal : !cur;
    if (next === cur) return;
    const e = chatEntries().find((x) => x.id === id);
    pinnedLocal.set(id, {
      pinned: next,
      item: next ? { id, at: e?.at || new Date().toISOString(), role: e?.role || "collie", summary: clientPinSummary(e) } : null,
    });
    if (view === "collie") renderSuper();
    try {
      await api("/api/super/pin", { body: { id, pinned: next } });
    } catch (err) {
      pinnedLocal.delete(id);
      if (view === "collie") renderSuper();
      toast(err.message, "attn");
    }
  }
  function renderPendingBar() {
    const wrap = $("#pending-wrap"), bar = $("#pending-bar"), listEl = $("#pending-list");
    if (!wrap) return;
    const pend = activePending(), pinsAll = activePinned();
    const pendIds = new Set(pend.map((p) => p.id));
    const pinIds = new Set(pinsAll.map((p) => p.id));
    const pinsOnly = pinsAll.filter((p) => !pendIds.has(p.id));
    const total = pend.length + pinsOnly.length;
    if (total < 2) pendingOpen = false;
    if (!total) {
      wrap.hidden = true;
      wrap.classList.remove("open", "pin-only");
      listEl.hidden = true;
      pendingBarSig = pendingListSig = "";
      updateSuperJump();
      return;
    }
    wrap.hidden = false;
    wrap.classList.toggle("open", pendingOpen);
    wrap.classList.toggle("pin-only", pend.length === 0);
    const P = pend.length, M = pinsAll.length;
    const lastPend = P ? pend[P - 1] : null, lastPin = M ? pinsAll[M - 1] : null;
    const sum = lastPend ? lastPend.summary : pinDisplaySummary(lastPin);
    const label = P && M ? (total === 1 ? "待你确认 · 已钉住" : `待你确认 (${P}) · 已钉住 (${M})`)
      : P ? (total === 1 ? "待你确认" : `待你确认 (${P})`)
      : (total === 1 ? "已钉住" : `已钉住 (${M})`);
    const barSig = `${total}:${P}:${M}:${lastPend?.id || ""}:${lastPin?.id || ""}:${sum}`;
    if (barSig !== pendingBarSig) {
      pendingBarSig = barSig;
      const lead = P ? `<span class="pending-dot"></span>` : `<span class="pending-pin-ico">${ICON_PIN}</span>`;
      if (total === 1) {
        const onlyId = lastPend ? lastPend.id : lastPin.id;
        const xBtn = lastPend
          ? `<button type="button" class="pending-x" data-dismiss="${esc(onlyId)}" title="忽略" aria-label="忽略">${ICON_X}</button>`
          : `<button type="button" class="pending-x" data-unpin="${esc(onlyId)}" title="取消钉住" aria-label="取消钉住">${ICON_X}</button>`;
        bar.innerHTML = `<button type="button" class="pending-main" data-goto-pending="${esc(onlyId)}" title="${esc(sum)}">
            ${lead}<span class="pending-label">${esc(label)}</span><span class="pending-sep">·</span><span class="pending-sum">${esc(sum)}</span>
          </button>${xBtn}`;
      } else {
        bar.innerHTML = `<button type="button" class="pending-main" data-toggle-pending aria-expanded="${pendingOpen}" title="${esc(sum)}">
            ${lead}<span class="pending-label">${esc(label)}</span><span class="pending-sep">·</span><span class="pending-sum">${esc(sum)}</span>
            <svg class="pending-chev" viewBox="0 0 24 24" aria-hidden="true"><path d="M7 14l5-5 5 5"/></svg>
          </button>`;
      }
    } else {
      bar.querySelector("[data-toggle-pending]")?.setAttribute("aria-expanded", String(pendingOpen));
    }
    if (total >= 2 && pendingOpen) {
      const listSig = JSON.stringify([
        pend.map((p) => [p.id, p.summary, p.at, pinIds.has(p.id) ? 1 : 0]),
        pinsOnly.map((p) => [p.id, p.summary, p.at, p.role]),
      ]);
      if (listSig !== pendingListSig) {
        pendingListSig = listSig;
        const pendRows = [...pend].reverse().map((p) => `<div class="pending-item">
          <button type="button" class="pending-jump" data-goto-pending="${esc(p.id)}" title="${esc(p.summary)}">
            <span class="pending-dot"></span>${pinIds.has(p.id) ? `<span class="pending-both-pin" title="已钉住">${ICON_PIN}</span>` : ""}<span class="pending-sum">${esc(p.summary)}</span><span class="pending-time">${esc(sepTime(p.at))}</span>
          </button><button type="button" class="pending-x" data-dismiss="${esc(p.id)}" title="忽略" aria-label="忽略">${ICON_X}</button>
        </div>`);
        const pinRows = [...pinsOnly].reverse().map((p) => {
          const ps = pinDisplaySummary(p);
          return `<div class="pending-item is-pin">
          <button type="button" class="pending-jump" data-goto-pending="${esc(p.id)}" title="${esc(ps)}">
            <span class="pending-pin-ico">${ICON_PIN}</span><span class="pending-sum">${esc(ps)}</span><span class="pending-time">${esc(sepTime(p.at))}</span>
          </button><button type="button" class="pending-x" data-unpin="${esc(p.id)}" title="取消钉住" aria-label="取消钉住">${ICON_X}</button>
        </div>`;
        });
        listEl.innerHTML = [...pendRows, ...pinRows].join("");
      }
      listEl.hidden = false;
    } else {
      listEl.hidden = true;
      listEl.innerHTML = "";
      pendingListSig = "";
    }
    updateSuperJump();
  }
  async function dismissPending(id) {
    if (!id || dismissedLocal.has(id)) return;
    dismissedLocal.add(id);
    if (replyTo?.entry === id) setReply(null);
    renderSide();
    if (view === "collie") renderSuper();
    try {
      await api("/api/super/dismiss", { body: { id } });
    } catch (err) {
      dismissedLocal.delete(id);
      renderSide();
      if (view === "collie") renderSuper();
      toast(err.message, "attn");
    }
  }

  // Older chat, loaded a page at a time when you ask: the hub only sends the
  // latest 200 lines. olderMore: whether there is more before that.
  let older = [], olderMore = null, olderBusy = false, superScrollRAF = 0;
  function chatEntries() {
    const have = new Set(sup.entries.map((e) => e.id));
    for (let i = pendingSays.length - 1; i >= 0; i--) if (pendingSays[i].id && have.has(pendingSays[i].id)) pendingSays.splice(i, 1);
    const old = older.length ? older.filter((e) => !have.has(e.id)) : [];
    return old.length || pendingSays.length ? [...old, ...sup.entries, ...pendingSays] : sup.entries;
  }
  // More before what is shown? Until asked, guess from a full page.
  const canLoadOlder = () => olderMore ?? !!sup.earlier;
  async function loadOlder(keepPos = true, untilId = "") {
    const first = chatEntries().find((e) => e.id);
    if (olderBusy || !first) return false;
    olderBusy = true;
    renderSuper();
    const scroller = $("#super-thread").parentElement, fromBottom = scroller.scrollHeight - scroller.scrollTop;
    let added = false;
    try {
      let url = `/api/super/earlier?before=${encodeURIComponent(first.id)}`;
      if (untilId) url += `&until=${encodeURIComponent(untilId)}`;
      const r = await api(url);
      const batch = r.entries || [];
      if (batch.length) {
        const seen = new Set(older.map((x) => x?.id));
        const fresh = batch.filter((x) => x?.id && !seen.has(x.id));
        if (fresh.length) {
          older = [...fresh, ...older];
          added = true;
        }
      }
      olderMore = batch.length ? !!r.more : false;
    } catch (e) { olderMore = false; toast(e.message, "attn"); }
    olderBusy = false;
    renderSuper();
    if (keepPos) {
      if (typeof cancelAnimationFrame === "function") cancelAnimationFrame(superScrollRAF);
      markProgrammaticScroll();
      scroller.scrollTop = scroller.scrollHeight - fromBottom; // stay where you were reading
    }
    return added;
  }
  async function jumpToEntry(id) {
    if (!id) return;
    const seq = ++jumpSeq;
    activeCollieRestore = null;
    pendingOpen = false;
    renderPendingBar();
    const sel = `#v-collie .msg[data-id="${CSS.escape(id)}"]`;
    let m = document.querySelector(sel);
    let guards = 0;
    while (!m && canLoadOlder() && guards++ < 40) {
      while (olderBusy) await new Promise((r) => setTimeout(r, 35));
      if (seq !== jumpSeq) return;
      m = document.querySelector(sel);
      if (m) break;
      const ok = await loadOlder(false, id);
      if (seq !== jumpSeq) return;
      m = document.querySelector(sel);
      if (!ok) break;
    }
    if (!m) { toast("这条消息太早了，已经不在这里"); return; }
    if (typeof cancelAnimationFrame === "function") cancelAnimationFrame(superScrollRAF);
    const sc = $("#super-thread").parentElement;
    const sr = sc.getBoundingClientRect(), mr = m.getBoundingClientRect();
    const want = Math.max(0, Math.min(sc.scrollHeight - sc.clientHeight, sc.scrollTop + (mr.top - sr.top) - Math.max(24, (sc.clientHeight - mr.height) / 2)));
    const dist = want - sc.scrollTop;
    const smooth = !matchMedia("(prefers-reduced-motion: reduce)").matches;
    if (smooth && Math.abs(dist) > sc.clientHeight * 2.2) {
      sc.scrollTop = want - Math.sign(dist) * sc.clientHeight * 0.9;
    }
    flashEntry = { id, until: Date.now() + 2200 };
    await scrollToAsync(sc, want, smooth ? "smooth" : "auto");
    captureViewScroll("collie", null, navCur);
    updateSuperJump();
    if (seq !== jumpSeq) return;
    flashEntry = { id, until: Date.now() + 1550 };
    const hit = document.querySelector(sel);
    if (hit) {
      hit.classList.remove("flash");
      void hit.offsetWidth;
      hit.classList.add("flash");
    }
    setTimeout(() => {
      if (flashEntry.id === id && Date.now() >= flashEntry.until) {
        document.querySelector(sel)?.classList.remove("flash");
      }
    }, 1600);
  }

  // ───────────────────────────── Collie conversation search
  let searchTimer = 0, searchAbort = null, searchSeq = 0;
  let searchQuery = "", searchHits = [], searchTotal = 0, searchHasMore = false, searchIdx = -1;
  {
    const btn = $("#super-search-btn"), wrap = $("#super-search"), input = $("#super-search-input");
    const clearBtn = $("#super-search-clear"), closeBtn = $("#super-search-close");
    const metaEl = $("#super-search-meta"), panel = $("#super-search-panel");
    const headEl = $("#super-search-head"), listEl = $("#super-search-list");
    const isMac = /Mac|iPhone|iPad/.test(navigator.platform || "");
    if (btn && matchMedia("(pointer: fine)").matches) {
      btn.title = `搜索对话 (${isMac ? "⌘K" : "Ctrl+K"})`;
    }
    const highlightSnippet = (snippet, q) => {
      const text = String(snippet || "");
      const terms = [...new Set(String(q || "").trim().split(/\s+/).filter(Boolean))].sort((a, b) => b.length - a.length);
      if (!terms.length || !text) return esc(text);
      const re = new RegExp("(" + terms.map((t) => t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|") + ")", "gi");
      let out = "", pos = 0;
      for (const m of text.matchAll(re)) {
        if (m.index > pos) out += esc(text.slice(pos, m.index));
        if (!m[0]) continue;
        out += `<mark>${esc(m[0])}</mark>`;
        pos = m.index + m[0].length;
      }
      if (pos < text.length) out += esc(text.slice(pos));
      return out;
    };
    const syncSearchActive = (scrollInto = true) => {
      if (!listEl || !input) return;
      const items = $$(".ss-item", listEl);
      items.forEach((el, idx) => {
        const on = idx === searchIdx;
        el.classList.toggle("active", on);
        el.setAttribute("aria-selected", String(on));
        if (on && scrollInto) el.scrollIntoView({ block: "nearest" });
      });
      const activeId = searchIdx >= 0 && items[searchIdx] ? items[searchIdx].id : "";
      if (activeId) input.setAttribute("aria-activedescendant", activeId);
      else input.removeAttribute("aria-activedescendant");
    };
    function showSearchPanel() {
      if (!panel || !input) return;
      panel.hidden = false;
      input.setAttribute("aria-expanded", "true");
    }
    window.__collieHideSearchPanel = () => {
      if (!panel || !input) return;
      panel.hidden = true;
      input.setAttribute("aria-expanded", "false");
      input.removeAttribute("aria-activedescendant");
    };
    const renderSearchResults = (q, hits, total, hasMore) => {
      searchQuery = q;
      searchHits = hits;
      searchTotal = total;
      searchHasMore = hasMore;
      if (!q) {
        metaEl.hidden = true;
        metaEl.textContent = "";
        headEl.hidden = true;
        listEl.innerHTML = "";
        searchIdx = -1;
        window.__collieHideSearchPanel();
        return;
      }
      metaEl.hidden = false;
      metaEl.textContent = hasMore ? `${hits.length}/${total} 条` : `${total} 条`;
      if (!hits.length) {
        headEl.hidden = true;
        listEl.innerHTML = `<div class="ss-empty">没有找到包含「${esc(q)}」的对话</div>`;
        searchIdx = -1;
        showSearchPanel();
        input.removeAttribute("aria-activedescendant");
        return;
      }
      headEl.hidden = false;
      headEl.innerHTML = `<span>${hasMore ? `找到 ${total} 条，显示最近 ${hits.length} 条` : `找到 ${total} 条相关消息`}</span><span class="ss-hint">↑↓ 选择 · ↵ 定位</span>`;
      searchIdx = 0;
      listEl.innerHTML = hits.map((h, i) => {
        const who = h.role === "user" ? "你" : "Collie";
        const at = +new Date(h.at) || 0;
        return `<button type="button" class="ss-item${i === 0 ? " active" : ""}" id="ss-opt-${i}" role="option" aria-selected="${i === 0}" data-hit-id="${esc(h.id)}">
          <div class="ss-top"><span class="ss-who ${h.role === "user" ? "user" : "collie"}">${who}</span><span class="ss-when"${at ? ` title="${esc(fullTime(at))}"` : ""}>${esc(at ? sepTime(at) : "")}</span></div>
          <div class="ss-snip">${highlightSnippet(h.snippet, q)}</div>
        </button>`;
      }).join("");
      showSearchPanel();
      syncSearchActive(false);
    };
    const runSuperSearch = async (raw) => {
      const q = String(raw || "").trim();
      const seq = ++searchSeq;
      searchAbort?.abort();
      if (!q) {
        renderSearchResults("", [], 0, false);
        return;
      }
      const ctrl = new AbortController();
      searchAbort = ctrl;
      try {
        const res = await fetch(`/api/super/search?q=${encodeURIComponent(q)}&limit=30`, {
          credentials: "same-origin",
          signal: ctrl.signal,
        });
        noteBuild(res.headers.get("X-Collie-Web"));
        if (res.status === 401) { showLogin(); return; }
        if (!res.ok) throw new Error(res.statusText);
        const data = await res.json();
        if (seq !== searchSeq || input.value.trim() !== q) return;
        renderSearchResults(q, data.results || [], data.total || 0, !!data.has_more);
      } catch (err) {
        if (err?.name === "AbortError" || seq !== searchSeq) return;
      }
    };
    const openSuperSearch = (selectExisting = false) => {
      if (!wrap || !input || !btn) return;
      const wasHidden = wrap.hidden;
      wrap.hidden = false;
      btn.classList.add("on");
      btn.setAttribute("aria-expanded", "true");
      input.focus({ preventScroll: true });
      if (selectExisting && !wasHidden && input.value) input.select();
      const q = input.value.trim();
      if (q) {
        if (q === searchQuery && (searchHits.length || !panel.hidden)) showSearchPanel();
        else runSuperSearch(q);
      }
    };
    const closeSuperSearch = (refocus = false) => {
      if (!wrap || !input || !btn) return;
      clearTimeout(searchTimer);
      searchAbort?.abort();
      searchSeq++;
      window.__collieHideSearchPanel();
      wrap.hidden = true;
      btn.classList.remove("on");
      btn.setAttribute("aria-expanded", "false");
      if (refocus && view === "collie") {
        if (!narrow()) $("#super-input")?.focus({ preventScroll: true });
        else btn.focus({ preventScroll: true });
      }
    };
    const selectSearchHit = (id) => {
      if (!id) return;
      window.__collieHideSearchPanel();
      if (narrow()) input?.blur();
      jumpToEntry(id);
    };
    btn?.addEventListener("click", (e) => {
      e.preventDefault();
      if (wrap.hidden) openSuperSearch(false);
      else closeSuperSearch(true);
    });
    clearBtn?.addEventListener("click", () => {
      clearTimeout(searchTimer);
      searchAbort?.abort();
      searchSeq++;
      input.value = "";
      clearBtn.hidden = true;
      renderSearchResults("", [], 0, false);
      input.focus({ preventScroll: true });
    });
    closeBtn?.addEventListener("click", () => closeSuperSearch(true));
    input?.addEventListener("input", () => {
      const raw = input.value;
      clearBtn.hidden = !raw.length;
      clearTimeout(searchTimer);
      if (!raw.trim()) {
        searchAbort?.abort();
        searchSeq++;
        renderSearchResults("", [], 0, false);
        return;
      }
      searchTimer = setTimeout(() => runSuperSearch(raw), 180);
    });
    input?.addEventListener("focus", () => {
      const q = input.value.trim();
      if (q && q === searchQuery && panel.hidden) showSearchPanel();
    });
    input?.addEventListener("keydown", (e) => {
      if (composing(e)) return;
      if (e.key === "ArrowDown" && searchHits.length) {
        e.preventDefault();
        if (panel.hidden) showSearchPanel();
        searchIdx = (searchIdx + 1) % searchHits.length;
        syncSearchActive(true);
      } else if (e.key === "ArrowUp" && searchHits.length) {
        e.preventDefault();
        if (panel.hidden) showSearchPanel();
        searchIdx = (searchIdx - 1 + searchHits.length) % searchHits.length;
        syncSearchActive(true);
      } else if (e.key === "Enter") {
        e.preventDefault();
        const q = input.value.trim();
        if (!q) return;
        if (q === searchQuery && searchHits.length) {
          const hit = searchHits[Math.max(0, searchIdx)];
          if (hit?.id) selectSearchHit(hit.id);
        } else {
          clearTimeout(searchTimer);
          runSuperSearch(q);
        }
      }
    });
    listEl?.addEventListener("mousemove", (e) => {
      const item = e.target.closest(".ss-item");
      if (!item) return;
      const idx = $$(".ss-item", listEl).indexOf(item);
      if (idx >= 0 && idx !== searchIdx) {
        searchIdx = idx;
        syncSearchActive(false);
      }
    });
    listEl?.addEventListener("click", (e) => {
      const item = e.target.closest(".ss-item[data-hit-id]");
      if (item) {
        e.preventDefault();
        selectSearchHit(item.dataset.hitId);
      }
    });
    document.addEventListener("mousedown", (e) => {
      if (!wrap || wrap.hidden) return;
      if (wrap.contains(e.target) || btn?.contains(e.target)) return;
      if (!input.value.trim()) closeSuperSearch(false);
      else window.__collieHideSearchPanel();
    });
    document.addEventListener("keydown", (e) => {
      if (view !== "collie" || document.querySelector("dialog[open]")) return;
      if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === "k") {
        e.preventDefault();
        openSuperSearch(true);
        return;
      }
      if (e.key === "Escape" && wrap && !wrap.hidden && !composing(e)) {
        if (pickOpen || pendingOpen || !$("#file-view")?.hidden) return;
        e.preventDefault();
        closeSuperSearch(true);
      }
    });
  }
  function hideSearchPanel() {
    window.__collieHideSearchPanel?.();
  }

  function renderSuper(opts) {
    const force = opts === true || !!opts?.force;
    const forceBottom = !!opts?.scrollBottom;
    const restore = opts?.restore || null;
    const box = $("#super-thread"), wk = $("#super-working"), crew = $("#crew");
    const scroller = box.parentElement;
    const holdRestore = !forceBottom && (restore || (activeCollieRestore && performance.now() < activeCollieRestore.until ? activeCollieRestore : null));
    const distBefore = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight;
    const atBottom = forceBottom ? true : holdRestore ? !!holdRestore.atBottom : distBefore <= AT_BOTTOM_PX;
    const jumping = flashEntry.until > Date.now();
    const liveAnchor = !atBottom && !holdRestore && !jumping && !olderBusy ? captureCollieAnchor(scroller) : null;
    const prevScrollTop = scroller.scrollTop;
    const wasWorking = !wk.hidden, prevBusy = wasWorking ? wk.textContent : "", prevCrew = crew.innerHTML;
    const r = sup.router || {};
    $("#router-kind").textContent = KIND_LABEL[r.kind] || r.kind || "";
    // Where it runs, once there is more than one computer to run on.
    const where = $("#router-where"), on = (state.machines || []).length > 1 ? machineName(r.machine) : "";
    const tags = [on, r.alias && "alias " + r.alias].filter(Boolean), whereText = tags.join("|");
    if (where._key !== whereText) {
      where._key = whereText;
      where.hidden = !whereText;
      where.innerHTML = tags.map((t) => `<span class="kind">${esc(t)}</span>`).join("");
      where.title = whereText ? `Collie 自己${[on && "跑在 " + on + " 上", r.alias && "用 alias " + r.alias + " 启动"].filter(Boolean).join("，")}（自动化 → 设置 → Collie 自己）` : "";
    }
    const st = $("#router-status");
    st.className = "status " + collieDot(r.status);
    st.innerHTML = `<span class="dot ${esc(collieDot(r.status))}"></span>${esc(r.replied ? "在收尾" : collieStatus(r.status))}`;
    // While Collie is busy, say so where the answer will appear, as the
    // other agents' sessions do; the header keeps the resting states.
    const busy = r.replied ? null : { working: "Collie 在想…", starting: "Collie 正在醒来…", blocked: "Collie 在等你在电脑上确认", dreaming: "Collie 在做梦，整理完记忆会自己醒来" }[r.status];
    st.hidden = !!busy;
    if (busy) {
      // In Collie's own column, where its next message will appear.
      wk.className = "working-row say " + (r.status === "working" ? "working" : collieDot(r.status));
      if (busy !== prevBusy) wk.innerHTML = `<span class="avatar"><img src="icon.svg" alt=""></span><span class="wk-t">${esc(busy)}</span>`;
    }
    wk.hidden = !busy;
    $("#router-queue").textContent = sup.paused ? "已暂停投递" : routerQueued() ? `${routerQueued()} 条事件排队中` : "";
    const cx = $("#router-ctx");
    cx.textContent = r.handover ? "正在交接记忆…" : r.alive && r.context ? ctxText(r.context, r.used, r.window) : "";
    cx.classList.toggle("warn", !!r.handover || (r.rotate_at > 0 && r.context >= r.rotate_at - 10));
    cx.title = r.rotate_at > 0 ? `用到 ${r.rotate_at}% 时，Collie 会把要记住的事写进 memory/，然后换一个新会话` : "自动换会话已关闭（自动化 → 设置）";
    renderRouterModel(r);
    $$('[data-act="pause"]').forEach((b) => b.textContent = sup.paused ? "恢复投递" : "暂停投递");

    const active = sup.workers.filter((w) => !w.ended);
    if (view === "collie") renderTarget();
    const crewHTML = active.map((w) => {
      const pane = paneOf(w.name);
      return `<a class="chip" ${pane ? `href="#/p/${encodeURIComponent(pane)}"` : ""}><span class="dot ${esc(workerStatus(w))}" title="${esc(STATUS_LABEL[workerStatus(w)] || "")}"></span>${esc(w.name)}<span class="k">${esc(KIND_LABEL[w.kind] || w.kind)}</span></a>`;
    }).join("");
    if (crewHTML !== prevCrew) crew.innerHTML = crewHTML;
    let html = "", prevRole = "";
    if (!sup.entries.length && !pendingSays.length) {
      html = `<div class="hello"><img src="icon.svg" alt="">
        <h3>一个入口，管一群 agent</h3>
        <p>把事情告诉 Collie。它会理解上下文、拆好活，交给 Claude、Codex 或 Antigravity 在你的机器上并行去做，只在需要你决定或验收时出声。</p>
        <div class="ideas">
          <button data-idea="看一下 ~/agent-workspace 里最近改动最多的三个项目，各派一个 agent 检查有没有失败的测试，最后给我一句话总结。">并行检查三个项目的测试</button>
          <button data-idea="每天早上 9 点帮我看一下各个任务的进展，有卡住的就告诉我。">每天 9 点汇报进展</button>
          <button data-idea="以后 GitHub 上有人评论 @collie 的 issue，就派一个 agent 去复现，确认后再告诉我。">把 GitHub 评论变成任务</button>
        </div></div>`;
    } else {
      let prev = "", lastAt = 0;
      // What a reply is about shows as a quoted line, unless it is plain
      // from what is right above: the first answer to your last message, or
      // a word on the worker whose line it follows.
      let lastUser = "", sinceUser = 0, lastEvent = "", lastCollie = "", sinceCollie = 0;
      const allEntries = chatEntries();
      const pendIds = new Set(activePending().map((p) => p.id));
      const pinIds = new Set(activePinned().map((p) => p.id));
      const entryPending = (idx, id) => {
        if (!id) return false;
        if (sup.hasPending) return pendIds.has(id);
        if (dismissedLocal.has(id)) return false;
        let otherCollie = false;
        for (let j = idx + 1; j < allEntries.length; j++) {
          const next = allEntries[j];
          if (next.role === "user") {
            const reId = next.re?.entry || "";
            if (reId === id || (!reId && !otherCollie)) return false;
          } else if (next.role === "collie") {
            otherCollie = true;
          }
        }
        return true;
      };
      const replyOpts = (e, idx) => {
        const opts = Array.isArray(e.options) ? e.options.filter((o) => typeof o === "string" && o.trim()) : [];
        if (opts.length) {
          if (!entryPending(idx, e.id)) return "";
          return `<div class="reply-opts" data-entry="${esc(e.id)}">${opts.map((o) =>
            `<button type="button" class="reply-opt" data-opt="${esc(o)}">${esc(o)}</button>`).join("")}</div>`;
        }
        if (e.check && entryPending(idx, e.id)) {
          return `<div class="reply-opts reply-check" data-entry="${esc(e.id)}"><button type="button" class="reply-ack" data-ack="${esc(e.id)}"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg>已看</button></div>`;
        }
        return "";
      };
      const quote = (e) => {
        const q = e.re;
        if (!q) return "";
        if (q.entry && q.entry === lastUser && !sinceUser) return "";
        if (q.entry && q.entry === lastCollie && !sinceCollie) return "";
        if (q.worker && q.worker === lastEvent) return "";
        const pane = q.worker && paneOf(q.worker), past = q.worker && !pane ? workerHist(q.worker, e.at) : null;
        const attr = q.entry ? `href="#" data-goto="${esc(q.entry)}"` : pane ? `href="#/p/${encodeURIComponent(pane)}"` : past ? `href="#/h/${encodeURIComponent(past.id)}"` : "";
        return `<a class="re" ${attr} title="${esc(q.text)}"><span class="re-who">${esc(q.who)}</span>${esc(plainLine(q.text))}</a>`;
      };
      for (let ei = 0; ei < allEntries.length; ei++) {
        const e = allEntries[ei];
        if (e.ref === ROUTER && e.tone === "info" && e.role !== "user" && e.role !== "collie") continue;
        if (e.role === "event" && workerChatter(e)) continue;
        // A quiet line with the time when the talk picks up again after a
        // while (or on a new day); hovering a message shows its exact time.
        const at = +new Date(e.at) || 0;
        // A new day always gets its line, even before an event (whose own
        // time has no date); talk after a pause gets one too.
        const talk = e.role === "user" || e.role === "collie";
        const newDay = at && lastAt && new Date(at).toDateString() !== new Date(lastAt).toDateString();
        if (at && (newDay || (talk && (!lastAt || at - lastAt > 10 * 60e3)))) {
          html += `<div class="time-sep">${esc(sepTime(at))}</div>`;
          prev = "";
        }
        if (at) lastAt = at;
        const cont = e.role === "collie" && prev === "collie";
        prev = prevRole = e.role;
        const tip = at ? ` title="${esc(fullTime(at))}" data-time="${esc(sepTime(at))}"` : "";
        const pinned = !!(e.id && pinIds.has(e.id));
        const pinBadge = pinned ? `<div class="pin-ind">${ICON_PIN}<span>已钉住</span></div>` : "";
        if (e.role === "user") {
          html += `<div class="mine${pinned ? " is-pinned" : ""}">${pinBadge}<div class="msg user" data-id="${esc(e.id)}"${at ? ` title="${esc(fullTime(at))}"` : ""}>${quote(e)}${userText(e.text)}</div>${mineActs(e, at, pinned)}</div>`;
          lastUser = e.id; sinceUser = 0; lastEvent = ""; sinceCollie++;
        } else if (e.role === "collie") {
          html += `<div class="say${cont ? " cont" : ""}${pinned ? " is-pinned" : ""}"><span class="avatar">${cont ? "" : `<img src="icon.svg" alt="">`}</span><div class="msg assistant" data-id="${esc(e.id)}"${at ? ` title="${esc(fullTime(at))}"` : ""}>${pinBadge}${quote(e)}${md(e.text)}${imagesIn(e.text)}${replyOpts(e, ei)}${msgActs(e, at, pinned)}</div></div>`;
          sinceUser++; lastEvent = ""; lastCollie = e.id; sinceCollie = 0;
        } else {
          lastEvent = e.ref || "";
          // A handover/dream line opens the session Collie rotated out of
          // (explicit h:<id>, or matched by time for older rows); a worker's
          // line opens its ended record once gone, else its live pane.
          const hid = e.ref?.startsWith("h:") ? e.ref.slice(2)
            : (!e.ref && (e.text?.includes("换了一个新会话") || (e.text?.includes("睡一会儿") && e.text?.includes("做梦"))) ? routerHist(e.at)?.id : "");
          const past = !hid && e.ref && e.ref !== ROUTER ? workerHist(e.ref, e.at) : null;
          const pane = !hid && !past && e.ref ? paneOf(e.ref) : null;
          const hTitle = e.text?.includes("做梦") ? "翻看做梦前的对话" : "翻看交接前的对话";
          const tag = hid ? `a href="#/h/${encodeURIComponent(hid)}" title="${hTitle}"`
            : past ? `a href="#/h/${encodeURIComponent(past.id)}" title="看它的完整记录"`
            : pane ? `a href="#/p/${encodeURIComponent(pane)}"` : "div";
          // A spawn line spins only while that worker is still at it; history stays still.
          const w = e.tone === "spawn" ? sup.workers.find((x) => x.name === e.ref) : null;
          const busy = w && workerBusy(w);
          const stuck = w && !w.ended && w.status === "blocked";
          const tone = e.tone === "blocked" ? "blocked" : e.tone === "done" ? "done" : e.tone === "spawn" ? (busy ? "working" : stuck ? "blocked" : "unknown") : e.tone === "error" ? "failed" : "unknown";
          // A spawn line carries its worker's state, in place of a line per change.
          const state = w ? (w.ended ? "已结束" : busy ? "还在跑" : stuck ? "等你确认" : "") : "";
          html += `<${tag} class="event ${e.tone === "error" ? "error" : ""}"><span class="dot ${tone}"></span><span class="when">${hhmm(e.at)}</span><span>${esc(e.text)}</span>${state ? `<span class="wstate">${state}</span>` : ""}</${tag.split(" ")[0]}>`;
        }
      }
    }
    // Everything waiting on the human, in one place, answerable right here.
    const needs = state.agents.filter((a) => a.status === "blocked");
    if (needs.length) {
      html += `<div class="needs">` + needs.map((a) => {
        const got = superAsks.get(a.pane_id);
        if (!got || Date.now() - got.at > 4000) loadSuperAsk(a.pane_id);
        if (got?.a) return superAskHTML(got.a, a);
        return `<a class="need" href="#/p/${encodeURIComponent(a.pane_id)}">
        <span class="dot blocked"></span>
        <span class="body"><div class="name">${esc(title(a))} 等你确认</div><div class="sub">${esc(isLocalRouter(a) ? "它停在一个对话框上，处理后才能继续" : (KIND_LABEL[a.kind] || a.kind) + " · " + tilde(a.cwd))}</div></span>
        <span class="go">处理 →</span></a>`;
      }).join("") + `</div>`;
    }
    renderPendingBar();
    if (olderBusy) html = `<div class="earlier-row">加载中…</div>` + html;
    const busyChanged = (busy || "") !== prevBusy, crewChanged = crewHTML !== prevCrew;
    if (html !== superSig || force) {
      superSig = html;
      box.innerHTML = html;
      if (flashEntry.until > Date.now() && flashEntry.id) {
        box.querySelector(`.msg[data-id="${CSS.escape(flashEntry.id)}"]`)?.classList.add("flash");
      }
    } else if (!busyChanged && !crewChanged) {
      return;
    }
    if (holdRestore && !holdRestore.atBottom) {
      activeCollieRestore = { ...holdRestore, until: performance.now() + 340 };
      if (typeof cancelAnimationFrame === "function") {
        cancelAnimationFrame(superScrollRAF);
        cancelAnimationFrame(collieRestoreRAF);
      }
      applyCollieAnchor(scroller, holdRestore);
      updateSuperJump();
      if (typeof requestAnimationFrame === "function") {
        collieRestoreRAF = requestAnimationFrame(() => {
          if (view !== "collie" || !activeCollieRestore) return;
          applyCollieAnchor(scroller, activeCollieRestore);
          captureViewScroll("collie", null, navCur);
          updateSuperJump();
        });
      }
      return;
    }
    if ((atBottom && !jumping) || forceBottom) {
      activeCollieRestore = null;
      markProgrammaticScroll();
      scroller.scrollTop = scroller.scrollHeight;
      const set = scroller.scrollTop;
      if (typeof requestAnimationFrame === "function") {
        if (typeof cancelAnimationFrame === "function") cancelAnimationFrame(superScrollRAF);
        superScrollRAF = requestAnimationFrame(() => {
          if (scroller.scrollTop === set) {
            markProgrammaticScroll();
            scroller.scrollTop = scroller.scrollHeight;
          }
          captureViewScroll("collie", null, navCur);
          updateSuperJump();
        });
      }
    } else {
      if (typeof cancelAnimationFrame === "function") cancelAnimationFrame(superScrollRAF);
      if (!jumping && !olderBusy && liveAnchor) {
        applyCollieAnchor(scroller, { anchorId: liveAnchor.anchorId, anchorOffset: liveAnchor.anchorOffset, scrollTop: prevScrollTop });
      }
    }
    updateSuperJump();
  }
  $("#super-thread")._loadMore = () => canLoadOlder() && loadOlder();
  {
    const sc = $("#super-thread")?.parentElement;
    sc?.addEventListener("scroll", updateSuperJump, { passive: true });
    sc?.addEventListener("wheel", (e) => {
      activeCollieRestore = null;
      if (e.deltaY < 0 && superJumpHiding) { superJumpHiding = false; updateSuperJump(); }
    }, { passive: true });
    sc?.addEventListener("touchmove", () => {
      activeCollieRestore = null;
      if (superJumpHiding) superJumpHiding = false;
    }, { passive: true });
    sc?.addEventListener("pointerdown", () => { activeCollieRestore = null; }, { passive: true });
    addEventListener("resize", updateSuperJump);
    $("#super-jump")?.addEventListener("click", async () => {
      if (!sc) return;
      jumpSeq++;
      activeCollieRestore = null;
      if (typeof cancelAnimationFrame === "function") cancelAnimationFrame(superScrollRAF);
      superJumpHiding = true;
      updateSuperJump();
      const smooth = !matchMedia("(prefers-reduced-motion: reduce)").matches;
      if (smooth && sc.scrollHeight - sc.scrollTop > sc.clientHeight * 2.5) {
        sc.scrollTop = sc.scrollHeight - sc.clientHeight * 1.8;
      }
      await scrollToAsync(sc, sc.scrollHeight, smooth ? "smooth" : "auto");
      markProgrammaticScroll();
      sc.scrollTop = sc.scrollHeight;
      captureViewScroll("collie", null, navCur);
      superJumpHiding = false;
      updateSuperJump();
    });
  }

  // The dialogs agents are stuck on, read from their screens (pane → {at, a}).
  const superAsks = new Map();
  async function loadSuperAsk(pane, force) {
    const prev = superAsks.get(pane);
    if ((prev?.loading || prev?.sending) && !force) return;
    superAsks.set(pane, { ...prev, at: Date.now(), loading: true, sending: false });
    let a = null;
    try { const r = await api(paneURL(pane, "ask")); a = r.asking ? r.approval : null; } catch { a = prev?.a || null; }
    if (superAsks.get(pane)?.sending && !force) return;
    superAsks.set(pane, { at: Date.now(), a });
    if ((force || JSON.stringify(a) !== JSON.stringify(prev?.a ?? null)) && view === "collie") renderSuper(force ? { force: true } : undefined);
  }
  setInterval(() => { if (view === "collie" && state.agents.some((x) => x.status === "blocked")) renderSuper(); }, 4000);
  function superAskHTML(a, agent) {
    const { q, blocks } = readDialog(a.question);
    const body = blocks.map((b) => b.code ? `<pre class="ctx-code">${esc(b.text)}</pre>` : `<p>${esc(b.text)}</p>`).join("");
    const choices = a.choices || [];
    return `<div class="ask" data-pane="${esc(agent.pane_id)}">
      <div class="ask-head"><span class="dot blocked"></span><span class="who">${esc(title(agent))}</span><span>需要你确认</span><span class="spacer"></span>
        <a href="#/p/${encodeURIComponent(agent.pane_id)}">打开 →</a></div>
      <div class="ask-q"><div class="q">${esc(q)}</div>${body ? `<div class="ctx">${body}</div>` : ""}</div>
      ${choices.length ? `<div class="choices">${choices.map((c, i) => `<button type="button" class="choice ${c.current ? "current" : ""}" data-choice="${i}">
          <span class="n">${esc(c.text || i + 1)}</span><span class="l">${esc(c.label)}${c.desc ? `<span class="d">${esc(c.desc)}</span>` : ""}</span></button>`).join("")}</div>
        <div class="ask-keys"><button type="button" data-k="esc">Esc 取消</button></div>`
      : `<div class="ask-keys"><button type="button" data-k="up">↑</button><button type="button" data-k="down">↓</button><button type="button" data-k="esc">Esc</button><button type="button" class="go" data-k="enter">确认 ↵</button></div>`}
    </div>`;
  }

  $("#super-thread").addEventListener("mousedown", (e) => { if (e.target.closest(".ask button")) e.preventDefault(); });
  $("#super-thread").addEventListener("click", async (e) => {
    const idea = e.target.closest("[data-idea]")?.dataset.idea;
    if (idea) { $("#super-input").value = idea; grow($("#super-input")); $("#super-input").focus(); }
    // Answering a stuck agent from here.
    const b = e.target.closest("button"), card = b?.closest(".ask[data-pane]");
    if (!card || card.dataset.busy) return;
    const pane = card.dataset.pane;
    let kind, body;
    if (b.dataset.choice != null) {
      const c = superAsks.get(pane)?.a?.choices?.[+b.dataset.choice];
      if (!c) return;
      if (c.keys?.length) { kind = "keys"; body = { keys: c.keys }; }
      else if (c.text) { kind = "text"; body = { text: c.text }; }
      else { kind = "keys"; body = { keys: ["enter"] }; }
    } else if (b.dataset.k) {
      kind = "keys"; body = { keys: [b.dataset.k] };
    } else return;
    card.dataset.busy = "1";
    card.style.opacity = ".5";
    const cur = superAsks.get(pane);
    if (cur) cur.sending = true;
    try {
      await api(paneURL(pane, kind), { body });
      await new Promise((r) => setTimeout(r, 350));
      await loadSuperAsk(pane, true);
    } catch (err) {
      if (cur) cur.sending = false;
      delete card.dataset.busy;
      card.style.opacity = "";
      toast(err.message, "attn");
    }
  });

  // Models and thinking efforts each agent offers, per computer (asked once per page).
  const EFFORT_LABEL = { none: "关闭", minimal: "最低", low: "低", medium: "中", high: "高", xhigh: "很高", max: "最高", ultra: "极致" };
  const modelInfo = {};
  async function modelsFor(kind, machine) {
    const key = kind + "@" + (machine || "");
    if (!modelInfo[key]) {
      const q = new URLSearchParams({ kind });
      if (machine) q.set("machine", machine);
      modelInfo[key] = api("/api/models?" + q).catch(() => ({ models: [], efforts: ["low", "medium", "high", "xhigh", "max"] }));
    }
    return modelInfo[key];
  }
  // The levels a model offers, else the agent's.
  const effortsOf = (info, m) => m?.efforts?.length ? m.efforts : (info.efforts || []);
  // Launcher aliases from each computer's shell rc file, as the hub last read them.
  let aliasInfo = null;
  function loadAliases(fresh) {
    if (!aliasInfo || fresh) aliasInfo = api("/api/super/aliases" + (fresh ? "?refresh=1" : "")).catch(() => { aliasInfo = null; return { machines: [], aliases: [] }; });
    return aliasInfo;
  }
  async function aliasesOn(machine) {
    const info = await loadAliases();
    return info.machines.find((m) => m.id === (machine || ""))?.aliases || [];
  }
  // Settings and the launcher call antigravity by its long name.
  const aliasKind = (k) => (k === "agy" ? "antigravity" : k);
  // A model id without its provider ("openrouter/x" → "x").
  // "claude-opus-5-5[1m]" reads as "Opus 5.5"; other ids lose their provider prefix.
  const shortModel = (id) => {
    const m = /^claude-([a-z]+)-(\d+)-(\d+)(?:-\d{8})?(?:\[1m\])?$/.exec(id);
    return m ? `${m[1][0].toUpperCase() + m[1].slice(1)} ${m[2]}.${m[3]}` : id.split("/").pop();
  };
  const aliasPins = (a) => [a.model, a.effort && (EFFORT_LABEL[a.effort] || a.effort)];
  const aliasSub = (a) => [...aliasPins(a), a.base_url].filter(Boolean).join(" · ");
  const aliasesOfKind = (list, kind) => list.filter((a) => aliasKind(a.kind) === kind);
  // The alias to keep selected: one of this agent's, or one this computer
  // didn't report (still shown as such); another agent's alias no longer applies.
  const keptAlias = (all, list, want) => (want && (list.some((a) => a.name === want) || !all.some((a) => a.name === want)) ? want : "");
  // Options for an alias picker; want stays listed when that computer doesn't have it.
  const aliasOpts = (list, want) => `<option value="">-</option>`
    + (want && !list.some((a) => a.name === want) ? `<option value="${esc(want)}" data-sub="这台电脑上没读到" selected>${esc(want)}</option>` : "")
    + list.map((a) => `<option value="${esc(a.name)}" data-glyph="${esc(a.kind)}" data-sub="${esc(aliasSub(a) || KIND_LABEL[a.kind] || a.kind)}" ${a.name === want ? "selected" : ""}>${esc(a.name)}</option>`).join("");
  // "派到": where this message's work runs. Shows the defaults from
  // 自动化 → 设置 (read again whenever this view opens); target holds only
  // what was picked here, so a message never carries a stale default.
  let target = null, targetDefault = null;
  async function renderTarget(fresh) {
    if (!targetDefault || fresh) {
      try {
        const s = (await api("/api/super/settings")).settings;
        targetDefault = { machine: s.worker_machine || "", kind: shortKind(s.worker_kind) || "claude", model: s.worker_model || "", effort: s.worker_effort || "", alias: s.worker_alias || "" };
      } catch { targetDefault ||= { machine: "", kind: "claude", model: "", effort: "", alias: "" }; }
    }
    // What was picked here over the defaults: picking only an agent leaves the
    // computer at its default, not unset.
    const t = targetNow();
    const ms = (state.machines || [{ id: "", name: "本机", local: true }]).filter((m) => !m.hub && (m.local ? state.ok !== false : m.online || m.id === t.machine));
    const fill = (sel, html) => { if (sel._html !== html) { sel._html = html; sel.innerHTML = html; } };
    const mSel = $("#t-machine"), kSel = $("#t-kind");
    fill(mSel, ms.map((m) => `<option value="${esc(m.id)}" data-dot="${m.local || m.online ? "online" : "offline"}"
      data-sub="${m.local ? "hub" : m.os || ""}${m.id === targetDefault.machine ? " · 默认" : ""}" ${m.id === t.machine ? "selected" : ""}>${esc(m.name)}</option>`).join(""));
    const have = ms.find((m) => m.id === t.machine)?.agents;
    fill(kSel, AGENT_KINDS.map(([k, l]) => {
      const missing = Array.isArray(have) && !have.includes(k);
      const sub = [missing ? "这台电脑没装" : "", k === targetDefault.kind ? "默认" : ""].filter(Boolean).join(" · ");
      return `<option value="${k}" data-glyph="${k}" data-short="${KIND_LABEL[k]}" ${sub ? `data-sub="${sub}"` : ""} ${missing && k !== t.kind ? "disabled" : ""} ${k === t.kind ? "selected" : ""}>${l}</option>`;
    }).join(""));
    mSel.value = t.machine; kSel.value = t.kind;
    mSel.hidden = ms.length < 2;
    syncPick(mSel); syncPick(kSel);
    mSel._pick?.classList.toggle("changed", t.machine !== targetDefault.machine);
    kSel._pick?.classList.toggle("changed", t.kind !== targetDefault.kind);
    renderAliasChip();
    renderModelChips();
  }
  // This computer's aliases for this agent; none listed, no chip. The default
  // alias goes with the default kind, as the model does.
  let aliasSeq = 0;
  async function renderAliasChip() {
    const seq = ++aliasSeq, t = targetNow(), sel = $("#t-alias");
    const all = await aliasesOn(t.machine);
    if (seq !== aliasSeq) return;
    const list = aliasesOfKind(all, aliasKind(t.kind)), want = keptAlias(all, list, t.alias);
    // What is sent is what shows: an alias this agent doesn't have is dropped.
    if (target?.alias !== undefined && target.alias !== want) target.alias = want;
    sel._list = list;
    const html = aliasOpts(list, want);
    if (sel._html !== html) { sel._html = html; sel.innerHTML = html; }
    sel.value = want;
    sel.hidden = !list.length && !want;
    syncPick(sel);
    const def = keptAlias(all, list, t.kind === targetDefault.kind ? targetDefault.alias : "");
    sel._pick?.classList.toggle("changed", want !== def);
  }
  // The default model and effort belong to the default kind of agent; any
  // other kind starts from that agent's own settings.
  const modelDefaults = (t) => t.kind === targetDefault.kind ? { model: targetDefault.model, effort: targetDefault.effort } : { model: "", effort: "" };
  const modelNow = (t) => { const d = modelDefaults(t); return { model: t.model ?? d.model, effort: t.effort ?? d.effort, d }; };
  let chipSeq = 0;
  async function renderModelChips() {
    const seq = ++chipSeq, t = targetNow(), { model, effort, d } = modelNow(t);
    const mSel = $("#t-model"), eSel = $("#t-effort");
    const info = await modelsFor(t.kind, t.machine);
    if (seq !== chipSeq) return;
    const list = [...(info.models || [])];
    if (model && !list.some((m) => m.id === model)) list.unshift({ id: model });
    const mark = (v, dv) => (v === dv ? " · 默认" : "");
    let html = d.model ? "" : `<option value="" data-short="${esc(info.default ? shortModel(info.default) : "默认模型")}" data-sub="agent 自己的设置 · 默认">${esc(info.default ? "默认（" + shortModel(info.default) + "）" : "默认")}</option>`;
    html += list.map((m) => `<option value="${esc(m.id)}" data-short="${esc(m.label && m.label.length <= 24 ? m.label : shortModel(m.id))}" data-sub="${esc((m.label && m.label !== m.id ? m.id : "") + mark(m.id, d.model))}" ${m.id === model ? "selected" : ""}>${esc(m.label || m.id)}</option>`).join("");
    if (mSel._html !== html) { mSel._html = html; mSel.innerHTML = html; }
    mSel.value = model;
    const cur = list.find((m) => m.id === model);
    const effs = effortsOf(info, cur);
    const eff = effs.includes(effort) ? effort : "";
    const defE = d.effort || info.default_effort || "";
    let ehtml = `<option value="" data-short="思考 · ${esc(EFFORT_LABEL[defE] || defE || "默认")}" data-sub="${esc(d.effort ? "派活默认" : "agent 自己的设置")}">默认（${esc(EFFORT_LABEL[defE] || defE || "agent 自己定")}）</option>`;
    ehtml += effs.map((e) => `<option value="${esc(e)}" data-short="思考 · ${esc(EFFORT_LABEL[e] || e)}" data-sub="${esc(e)}" ${e === eff ? "selected" : ""}>${esc(EFFORT_LABEL[e] || e)}</option>`).join("");
    if (eSel._html !== ehtml) { eSel._html = ehtml; eSel.innerHTML = ehtml; }
    eSel.value = eff === d.effort ? "" : eff;
    eSel.hidden = effs.length === 0;
    syncPick(mSel); syncPick(eSel);
    mSel._pick?.classList.toggle("changed", model !== d.model);
    eSel._pick?.classList.toggle("changed", eff !== d.effort && eff !== "");
  }
  const pick = (o) => { target = { ...(target || {}), ...o }; };
  $("#t-machine").addEventListener("change", (e) => { pick({ machine: e.target.value }); renderTarget(); });
  $("#t-kind").addEventListener("change", (e) => { pick({ kind: e.target.value, model: undefined, effort: undefined, alias: undefined }); renderTarget(); });
  // An alias brings the model and effort it pins; one that pins none (or
  // none at all) goes back to the defaults.
  $("#t-alias").addEventListener("change", (e) => {
    const name = e.target.value, a = (e.target._list || []).find((x) => x.name === name);
    pick({ alias: name, model: a?.model || undefined, effort: a?.effort || undefined });
    renderAliasChip(); renderModelChips();
  });
  $("#t-model").addEventListener("change", (e) => { pick({ model: e.target.value }); renderModelChips(); });
  $("#t-effort").addEventListener("change", (e) => { pick({ effort: e.target.value || modelDefaults(targetNow()).effort }); renderModelChips(); });
  const targetNow = () => {
    const d = targetDefault || { machine: "", kind: "claude", model: "", effort: "", alias: "" };
    const t = { ...d };
    for (const [k, v] of Object.entries(target || {})) if (v !== undefined) t[k] = v;
    if (target && "kind" in target && t.kind !== d.kind) { t.model = target.model; t.effort = target.effort; t.alias = target.alias ?? ""; }
    return t;
  };

  // Quoting one of Collie's messages: it rides along with what you send next.
  let replyTo = null;
  const setReply = (e) => {
    replyTo = e ? { entry: e.id, who: "Collie", text: e.text } : null;
    const box = $("#re-draft");
    box.hidden = !e;
    if (e) { $(".re-t", box).textContent = reLine(e.text); $("#super-input").focus(); }
  };
  const reLine = (t) => plainLine(t);
  $("#re-draft button").addEventListener("click", () => setReply(null));
  document.addEventListener("click", (ev) => {
    const b = ev.target.closest(".re-btn[data-re]");
    if (!b) return;
    const e = chatEntries().find((x) => x.id === b.dataset.re);
    if (e) setReply(e);
  });

  async function sendSuper(text, re, restoreRe, ready = text) {
    // Only what was picked here goes along; the hub fills in its current defaults.
    const t = targetNow(), p = target || {};
    const { model, effort } = modelNow(t);
    // Your message shows at once; the hub's copy takes its place when it arrives.
    const mine = { id: "", role: "user", text, at: new Date().toISOString(), pending: true, re };
    pendingSays.push(mine);
    activeCollieRestore = null;
    renderSide();
    renderSuper({ force: true, scrollBottom: true });
    let r;
    try {
      text = mine.text = await ready;
      r = await api("/api/super/say", { body: { text, re: re?.entry || "",
      machine: "machine" in p ? (t.machine || "local") : "",
      kind: "kind" in p ? t.kind : "",
      model: "model" in p || "kind" in p ? model : "",
      effort: "effort" in p || "kind" in p ? effort : "",
      ...("alias" in p || "kind" in p ? { alias: t.alias || "" } : {}) } });
    } catch (err) {
      const idx = pendingSays.indexOf(mine);
      if (idx >= 0) pendingSays.splice(idx, 1);
      if (restoreRe && re && !replyTo) setReply(chatEntries().find((x) => x.id === re.entry));
      renderSide();
      renderSuper({ force: true });
      throw err;
    }
    // The hub's copy may already be here (its push beat the reply): swap now.
    mine.id = r?.entry || "";
    if (!mine.id || sup.entries.some((e) => e.id === mine.id)) {
      const idx = pendingSays.indexOf(mine);
      if (idx >= 0) pendingSays.splice(idx, 1);
      renderSide();
      renderSuper({ force: true });
    }
    if (!sup.router?.alive) toast("正在唤醒 Collie…");
  }

  $("#super-thread").addEventListener("click", async (ev) => {
    const ack = ev.target.closest(".reply-ack[data-ack]");
    if (ack) { ev.preventDefault(); dismissPending(ack.dataset.ack); return; }
    const b = ev.target.closest(".reply-opt[data-opt]");
    if (!b) return;
    const grp = b.closest(".reply-opts[data-entry]");
    if (!grp || grp.dataset.busy) return;
    grp.dataset.busy = "1";
    grp.querySelectorAll("button").forEach((x) => { x.disabled = true; });
    const id = grp.dataset.entry, text = b.dataset.opt;
    if (replyTo?.entry === id) setReply(null);
    const e = chatEntries().find((x) => x.id === id);
    try {
      await sendSuper(text, e ? { entry: e.id, who: "Collie", text: e.text } : { entry: id, who: "Collie", text: "" }, false);
    } catch (err) {
      toast(err.message, "attn");
    }
  });

  $("#pending-wrap").addEventListener("click", (ev) => {
    const dis = ev.target.closest("[data-dismiss]");
    if (dis) { ev.preventDefault(); ev.stopPropagation(); dismissPending(dis.dataset.dismiss); return; }
    const unpin = ev.target.closest("[data-unpin]");
    if (unpin) { ev.preventDefault(); ev.stopPropagation(); togglePin(unpin.dataset.unpin, false); return; }
    if (ev.target.closest("[data-toggle-pending]")) { ev.preventDefault(); pendingOpen = !pendingOpen; renderPendingBar(); return; }
    const go = ev.target.closest("[data-goto-pending]");
    if (go) { ev.preventDefault(); jumpToEntry(go.dataset.gotoPending); }
  });
  document.addEventListener("click", (ev) => {
    if (pendingOpen && !ev.composedPath().includes($("#pending-wrap"))) { pendingOpen = false; renderPendingBar(); }
  });
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape" && pendingOpen && !document.querySelector("dialog[open]")) { pendingOpen = false; renderPendingBar(); }
  });

  bindComposer($("#super-composer"), $("#super-input"), async (draft, ready) => {
    const re = replyTo;
    setReply(null);
    await sendSuper(draft, re, true, ready);
  }, () => targetNow().machine);

  // ───────────────────────────── tabs in a page
  // Picking a tab keeps where the last one was scrolled and opens the new one
  // where it was left; get/set hold which tab is open.
  function bindTabs(bar, page, body, get, set, open) {
    $(bar).addEventListener("click", (e) => {
      const t = e.target.closest("button")?.dataset.tab;
      if (!t || t === get()) return;
      if (view === page) captureViewScroll(page, null, navCur);
      set(t);
      const saved = scrollByRoute.get(page + ":" + t) || null;
      if (!saved) { markProgrammaticScroll(); $(body).scrollTop = 0; }
      open(saved);
    });
  }
  const markTab = (bar, cur) => $$(bar + " button").forEach((b) => b.classList.toggle("on", b.dataset.tab === cur));

  // ───────────────────────────── automation view
  let autoTab = store.get("autoTab", "inbox");
  let autoTimer;
  bindTabs("#auto-tabs", "auto", "#auto-body", () => autoTab, (t) => { autoTab = t; store.set("autoTab", t); }, (saved) => openAuto(saved));

  // The follow-up line under 自动化 opens the open tasks (top of 记忆).
  $("#nav-auto-sub").addEventListener("click", () => {
    if (sup.follow?.next) {
      autoTab = "memory"; store.set("autoTab", autoTab);
      scrollByRoute.delete("auto:memory");
      if (view === "auto") { markProgrammaticScroll(); $("#auto-body").scrollTop = 0; openAuto(); }
    }
  });

  function openAuto(restore = null) {
    memEditing = false;
    $("#auto-body")._memSig = "";
    markTab("#auto-tabs", autoTab);
    if (restore) restoreSimpleScroll($("#auto-body"), restore);
    clearInterval(autoTimer);
    loadAuto(restore);
    if (autoTab === "inbox" || autoTab === "threads") autoTimer = setInterval(() => loadAuto(), 4000);
    if (autoTab === "memory") autoTimer = setInterval(() => { if (!memEditing) loadAuto(); }, 5000);
  }

  async function loadAuto(restore = null) {
    const body = $("#auto-body");
    const keepTop = !restore && body.clientHeight > 0 ? { scrollTop: body.scrollTop, atBottom: body.scrollHeight - body.scrollTop - body.clientHeight <= AT_BOTTOM_PX } : null;
    const applyAfter = () => {
      if (restore) restoreSimpleScroll(body, restore);
      else if (keepTop && !keepTop.atBottom) { markProgrammaticScroll(); body.scrollTop = keepTop.scrollTop; }
    };
    try {
      if (autoTab === "inbox" || autoTab === "threads") {
        const r = await api("/api/super/triggers");
        if (view !== "auto") return;
        body.innerHTML = autoTab === "inbox" ? inboxHTML(r.triggers || []) : threadsHTML(r.threads || []);
        applyAfter();
      } else if (autoTab === "memory") {
        const r = await api("/api/super/memory");
        if (view !== "auto" || autoTab !== "memory" || memEditing) return;
        // Workers' states count too: the task cards show them.
        const sig = JSON.stringify(r) + sup.workers.map((w) => w.name + ":" + workerStatus(w)).join();
        if (sig !== body._memSig) {
          const open = new Set([...body.querySelectorAll("details[open]")].map((d) => d.dataset.path || d.querySelector("summary")?.textContent));
          const first = !body._memSig;
          body._memSig = sig; body.innerHTML = memoryHTML(r);
          if (!first) body.querySelectorAll("details").forEach((d) => d.open = open.has(d.dataset.path || d.querySelector("summary")?.textContent));
          applyAfter();
        } else if (restore) {
          restoreSimpleScroll(body, restore);
        }
        return;
      } else if (autoTab === "timers") {
        const r = await api("/api/super/timers");
        if (view !== "auto" || autoTab !== "timers") return;
        const endedOpen = !!body.querySelector("details[data-ended-timers][open]");
        body.innerHTML = timersHTML(r.timers || []);
        if (endedOpen) { const d = body.querySelector("details[data-ended-timers]"); if (d) d.open = true; }
        applyAfter();
      } else {
        const r = await api("/api/super/settings");
        body.innerHTML = autoTab === "hooks" ? hooksHTML(r) : settingsHTML(r);
        applyAfter();
        if (autoTab !== "hooks") api("/api/super/lark?fresh").then((l) => {
          const el = $("#lark-who");
          if (el) el.textContent = l.ready ? `发给 ${l.who || "lark-cli 登录的人"}（以机器人身份）` : `${l.problem}，所以不会发`;
        }).catch(() => {});
      }
    } catch (err) { body.innerHTML = `<p class="panel-intro">${esc(err.message)}</p>`; }
  }

  // ───────────────────────────── memory: what Collie keeps between sessions
  let memEditing = false;
  const kb = (n) => (n / 1024).toFixed(1) + " KB";
  const kbNum = (n) => (n / 1024).toFixed(1);
  const kbCap = (n) => (n % 1024 === 0 ? String(n / 1024) : (n / 1024).toFixed(1)) + " KB";
  const dayTime = (t) => {
    const d = new Date(t), today = new Date().toDateString(), tmr = new Date(Date.now() + 864e5).toDateString();
    const hm = d.toTimeString().slice(0, 5);
    return d.toDateString() === today ? "今天 " + hm : d.toDateString() === tmr ? "明天 " + hm : `${d.getMonth() + 1}月${d.getDate()}日 ${hm}`;
  };
  function memoryHTML(r) {
    const files = r.files || [];
    const by = (f) => files.filter(f);
    const dream = r.dreaming ? `<b>Collie 正在做梦</b>：回顾最近的对话，整理记忆。整理完会自己醒来，期间的消息会先攒着。`
      : `上次做梦：${r.last_dream ? dayTime(r.last_dream) : "还没有"}　·　下次：${r.next_dream ? dayTime(r.next_dream) + "（当天有新对话才会做）" : "已关闭"}`;
    const firstLine = (t, dflt) => t.split("\n").filter((l) => !/^\s*#/.test(l)).map((l) => l.replace(/^[>*\-\s]+/, "").replace(/`|\*\*/g, "").trim()).find(Boolean) || dflt;
    // A task names the workers on it: show them live, and open their session.
    const crew = (f) => {
      const latest = new Map(); // a name can be reused: the newest one
      for (const w of sup.workers) if (!latest.has(w.name) || new Date(w.created_at) > new Date(latest.get(w.name).created_at)) latest.set(w.name, w);
      const ws = [...latest.values()].filter((w) => new RegExp(`(^|[^\\w-])${w.name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?![\\w-])`).test(f.text));
      if (!ws.length) return "";
      return `<span class="mem-crew">${ws.map((w) => {
        const pane = !w.ended && paneOf(w.name), past = !pane && workerHist(w.name, w.created_at);
        const href = pane ? `#/p/${encodeURIComponent(pane)}` : past ? `#/h/${encodeURIComponent(past.id)}` : "";
        const st = workerStatus(w);
        return `<a ${href ? `href="${href}"` : ""} title="${esc((KIND_LABEL[w.kind] || w.kind) + " · " + (STATUS_LABEL[st] || st))}"><span class="dot ${esc(st)}"></span>${esc(w.name)}</a>`;
      }).join("")}</span>`;
    };
    const card = (f, o = {}) => {
      const edit = !f.legacy && o.edit !== false;
      const sub = o.sub ?? firstLine(f.text, "");
      return `<details class="mem-fold" ${o.open ? "open" : ""} data-path="${esc(f.name)}"><summary>
          <span class="mono mem-name">${esc(o.label || f.name)}</span><span class="grow mem-sub">${esc(sub)}</span><span class="muted mem-size">${kb(f.size)}</span></summary>
        <div class="mem-body msg assistant" data-view>${md(f.text)}</div>
        ${edit ? `<div class="mem-tools">${o.crew ? crew(f) : ""}<span class="muted grow">${esc(dayTime(f.mod))} 更新</span><button class="btn small" data-act="mem-edit">编辑</button></div>
        <form class="mem-edit" hidden><textarea name="text" spellcheck="false">${esc(f.text)}</textarea>
          <div class="form-actions"><span class="muted grow">Collie 下次读这个文件时生效。</span><button type="button" class="btn small" data-act="mem-cancel">取消</button><button class="btn primary small">保存</button></div></form>` : ""}
      </details>`;
    };
    const group = (title, note, list, o = {}) => list.length || o.always ? `<h4 class="mem-h">${title}<span class="muted">${note}</span></h4>
      <div class="card">${list.length ? list.map((f, i) => card(f, typeof o.card === "function" ? o.card(f, i) : o.card || {})).join("") : `<div class="empty-row">${o.empty || "空"}</div>`}</div>` : "";
    const ins = by((f) => f.name === "instructions.md");
    const tasks = by((f) => /^tasks\/[^/]+\.md$/.test(f.name));
    const notes = by((f) => f.name.startsWith("notes/")).sort((a, b) => (a.name === "notes/index.md" ? -1 : b.name === "notes/index.md" ? 1 : a.name.localeCompare(b.name)));
    const done = by((f) => f.name.startsWith("tasks/done/")).sort((a, b) => new Date(b.mod) - new Date(a.mod));
    const msgs = by((f) => f.name === "messages.md");
    const legacy = by((f) => f.legacy);
    const msgBytes = r.messages ?? msgs.reduce((s, f) => s + (f.size || 0), 0);
    const upFront = r.up_front ?? Math.max(0, (r.loaded || 0) - msgBytes);
    const budget = r.budget || (10 << 10);
    const over = upFront > budget;
    return `<p class="panel-intro">Collie 的长期记忆在 <code>${esc(r.dir)}/memory</code>，按用途分开：每次新会话只先读<b>指令、笔记索引、进行中的任务和最近说过的话</b>，其余需要时再看。
      「做梦」时，Collie 回顾最近的会话来整理记忆：替换过时的条目，发现只有跨多次对话才看得出的习惯（标记为 inferred）。</p>
      <div class="card"><div class="mem-dream"><span class="dot ${r.dreaming ? "starting" : "asleep"}"></span><span class="grow">${dream}</span>
        <button class="btn small" data-act="dream-now" ${r.dreaming ? "disabled" : ""}>${r.dreaming ? "做梦中…" : "现在整理"}</button></div>
        <div class="mem-load">
          <div class="mem-load-row">
            <span class="mem-load-main" id="mem-front-label">前置记忆 <b class="${over ? "warn" : ""}">${kbNum(upFront)}</b> / ${kbCap(budget)}${over ? `<span class="mem-warn-note">超出预算，做梦时会精简</span>` : ""}</span>
            <span class="mem-load-msg">近期消息 <b>${kb(msgBytes)}</b><span class="muted"> · 自动记录</span></span>
          </div>
          <progress class="mem-meter${over ? " warn" : ""}" max="${budget}" value="${Math.min(upFront, budget)}" aria-labelledby="mem-front-label"></progress>
          <div class="mem-load-sub muted">仅指令、笔记索引与进行中任务计入 ${kbCap(budget)} 预算；<code>messages.md</code> 由 hub 自动维护，不占预算</div>
        </div></div>
      ${legacy.length ? group("旧版记忆", "等下次做梦或换会话时拆进 memory/", legacy, { card: { open: true } }) : ""}
      ${group("进行中的任务", "一件事一个文件，每次都读", tasks, { card: { label: undefined, crew: true }, always: true, empty: "没有进行中的任务" })}
      ${group("指令", "你的长期要求和偏好，每次都读", ins, { card: { open: true, label: "instructions.md", sub: "" }, always: true, empty: "还没有" })}
      ${group("笔记", "只先读索引，其余需要时读", notes, { card: (f) => ({ open: f.name === "notes/index.md", label: f.name.replace(/^notes\//, "") }) })}
      ${group("最近说过的话", "hub 自动记录最近 10 条，每次都读，避免重复", msgs, { card: { edit: false, label: "messages.md" } })}
      ${r.dreams.length ? `<h4 class="mem-h">做梦日记</h4><div class="card">${r.dreams.map((d, i) => `<details class="mem-fold" ${i === 0 ? "open" : ""}><summary><span class="muted mono mem-name">${esc(d.name.replace(/\.md$/, ""))}</span><span class="grow mem-sub">${esc(firstLine(d.text, d.name))}</span></summary><div class="mem-body msg assistant">${md(d.text)}</div></details>`).join("")}</div>` : ""}
      ${done.length ? `<details class="mem-done" data-path="tasks/done/"><summary class="mem-h">已完成<span class="muted">${done.length} 件 · 做完或放弃的任务，不再先读</span></summary>
        <div class="card">${done.map((f) => card(f, { label: f.name.replace(/^tasks\/done\//, ""), crew: true })).join("")}</div></details>` : ""}`;
  }

  function inboxHTML(ts) {
    return `<p class="panel-intro">你的消息、外部事件、定时器，以及派出的 agent 完成或卡住，都会进这里。先按来源策略过滤；订阅了线程的 agent 直接收到，其余交给 Collie 判断。对方正忙时事件会先攒着，空闲后一次送过去。</p>
      <div class="card"><ul class="rows">${ts.length ? ts.map((t) => `<li class="row">
        <span class="badge ${esc(t.status === "queued" ? "queued" : t.status === "ignored" ? "ignored" : t.action === "candidate" ? "candidate" : "")}">${esc({ queued: "排队", delivered: "已投递", ignored: "已忽略" }[t.status] || t.status)}</span>
        <div><div class="meta"><b>#${esc(t.id)}</b><span>${esc(SOURCE_LABEL[t.source] || t.source)}</span><span>${esc(t.thread)}</span>${t.from ? `<span>from ${esc(t.from === ROUTER ? "Collie" : t.from)}</span>` : ""}${t.targets?.length ? `<span>→ ${esc(t.targets.map((x) => x === ROUTER ? "Collie" : x).join(", "))}</span>` : ""}${t.action === "candidate" ? "<span>candidate</span>" : ""}</div>
        <div class="text">${esc(t.text)}</div></div>
        <span class="right">${when(t.at)}</span></li>`).join("") : `<li class="empty-row">收件箱是空的</li>`}</ul></div>`;
  }

  function threadsHTML(ths) {
    return `<p class="panel-intro">同一个对话、Issue/PR 或群聊话题里的连续事件构成一条线程。Collie 派出的 agent 订阅线程后，后续事件直达它，不再经过 Collie。chat:main 始终由 Collie 自己处理。</p>
      <div class="card"><ul class="rows">${ths.length ? ths.map((th) => `<li class="row">
        <span class="badge">${th.triggers?.length || 0}</span>
        <div><div class="meta"><b>${esc(th.id)}</b>${th.subscribers?.length ? `<span>订阅：${esc(th.subscribers.join(", "))}</span>` : "<span>→ Collie</span>"}</div></div>
        <span class="right">${when(th.last_at)}</span></li>`).join("") : `<li class="empty-row">还没有线程</li>`}</ul></div>`;
  }

  // ───────────────────────────── timers: picked, not typed
  const WEEK = [["mon", "一"], ["tue", "二"], ["wed", "三"], ["thu", "四"], ["fri", "五"], ["sat", "六"], ["sun", "日"]];
  const UNIT = { m: "分钟", h: "小时" };
  // describeSpec turns "weekly mon,thu 09:00" into "每周一、四 09:00".
  function describeSpec(spec) {
    const f = String(spec || "").trim().split(/\s+/);
    const dur = (d) => {
      const m = /^(\d+(?:\.\d+)?)([mhs])$/.exec(d || "");
      if (!m) return d;
      const n = +m[1];
      if (m[2] === "h" && n >= 24 && n % 24 === 0) return `${n / 24} 天`;
      if (m[2] === "m" && n >= 60 && n % 60 === 0) return `${n / 60} 小时`;
      return `${n} ${UNIT[m[2]] || "秒"}`;
    };
    switch (f[0]) {
      case "daily": return `每天 ${f[1]}`;
      case "weekdays": return `工作日 ${f[1]}`;
      case "weekly": return `每周${(f[1] || "").split(",").map((d) => (WEEK.find((w) => w[0] === d) || [, d])[1]).join("、")} ${f[2] || ""}`;
      case "every": return `每隔 ${dur(f[1])}`;
      case "in": return `${dur(f[1])}后（一次）`;
      case "at": return `${f[1]} ${f[2] || ""}（一次）`;
    }
    return spec;
  }
  const pad2 = (n) => String(n).padStart(2, "0");
  const localStamp = (d) => `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
  // The spec the timer form describes, or an error to show.
  function timerSpec(f) {
    const mode = f.dataset.mode, clock = f.clock.value;
    if (["daily", "weekdays", "weekly"].includes(mode) && !/^\d\d:\d\d$/.test(clock)) return { err: "选一个时间" };
    if (mode === "daily") return { spec: `daily ${clock}` };
    if (mode === "weekdays") return { spec: `weekdays ${clock}` };
    if (mode === "weekly") {
      const days = [...f.querySelectorAll(".tm-days button.on")].map((b) => b.dataset.day);
      return days.length ? { spec: `weekly ${days.join(",")} ${clock}` } : { err: "选至少一天" };
    }
    if (mode === "every") {
      const n = Math.floor(+f.every_n.value);
      if (!(n >= 1)) return { err: "间隔至少 1" };
      const u = f.every_u.value;
      return { spec: u === "d" ? `every ${n * 24}h` : `every ${n}${u}` };
    }
    const at = f.at.value;
    if (!at) return { err: "选一个日期和时间" };
    if (new Date(at) <= new Date()) return { err: "这个时间已经过去了" };
    return { spec: `at ${at.replace("T", " ")}` };
  }
  function syncTimerForm(f) {
    const mode = f.dataset.mode;
    f.querySelectorAll(".tm-mode button").forEach((b) => b.classList.toggle("on", b.dataset.mode === mode));
    f.querySelectorAll("[data-show]").forEach((el) => { el.hidden = !el.dataset.show.split(" ").includes(mode); });
    const r = timerSpec(f), target = f.target.selectedOptions[0]?.textContent || "Collie";
    const sum = f.querySelector(".tm-sum");
    sum.textContent = r.err ? r.err : `${describeSpec(r.spec)} · 交给 ${target.replace(/（.*）/, "")}`;
    sum.classList.toggle("err", !!r.err);
  }

  function timersHTML(ts = []) {
    const soon = new Date(Date.now() + 60 * 60 * 1000); soon.setMinutes(0);
    setTimeout(() => { const f = $("#timer-form"); if (f) syncTimerForm(f); });
    const idN = (id) => +(/\d+$/.exec(id || "")?.[0] || 0);
    const ms = (s) => { const n = s ? Date.parse(s) : NaN; return n > 0 ? n : NaN; };
    const endMs = (t) => { const l = ms(t.last_run); return isNaN(l) ? ms(t.next_run) : l; };
    const active = ts.filter((t) => t.enabled).sort((a, b) => {
      const ta = ms(a.next_run), tb = ms(b.next_run);
      if (isNaN(ta) !== isNaN(tb)) return isNaN(ta) ? 1 : -1;
      return (ta - tb) || (idN(a.id) - idN(b.id));
    });
    const ended = ts.filter((t) => !t.enabled).sort((a, b) => {
      const ta = endMs(a), tb = endMs(b);
      if (isNaN(ta) !== isNaN(tb)) return isNaN(ta) ? 1 : -1;
      return (tb - ta) || (idN(b.id) - idN(a.id));
    });
    const timerRow = (t) => `<li class="row">
        <span class="badge ${t.enabled ? "queued" : "ignored"}">${t.enabled ? "启用" : "已结束"}</span>
        <div><div class="meta"><b title="${esc(t.spec)}">${esc(describeSpec(t.spec))}</b><span>→ ${esc(t.target || "Collie")}</span>${t.enabled ? `<span>下次 ${when(t.next_run)}</span>` : t.last_run ? `<span>上次 ${when(t.last_run)}</span>` : ""}${t.owner ? `<span>${t.owner === "you" ? "你设的" : esc(t.owner) + " 设的"}</span>` : ""}</div>
        <div class="text">${esc(t.text)}</div></div>
        <button class="btn ghost small" data-del-timer="${esc(t.id)}">删除</button></li>`;
    return `<p class="panel-intro">到点会给 Collie（或某个 agent）发一条消息：每天的例行检查、过一会儿的跟进提醒。Collie 自己也会设定时来跟进。</p>
      <div class="card"><div class="card-h">新建定时</div>
        <form class="form tm-form" id="timer-form" data-mode="weekdays">
          <div class="tm-field">
            <span class="tm-lab">什么时候</span>
            <div class="tm-when">
              <div class="seg tm-mode">${[["daily", "每天"], ["weekdays", "工作日"], ["weekly", "每周"], ["every", "每隔"], ["once", "一次"]].map(([m, l]) => `<button type="button" data-mode="${m}">${l}</button>`).join("")}</div>
              <div class="tm-days" data-show="weekly">${WEEK.map(([d, l], i) => `<button type="button" data-day="${d}" class="${i === 0 ? "on" : ""}">${l}</button>`).join("")}</div>
              <input type="time" name="clock" value="09:00" data-show="daily weekdays weekly">
              <span class="tm-every" data-show="every"><input type="number" name="every_n" min="1" value="2"><select name="every_u" data-pick="compact" data-title="单位"><option value="m">分钟</option><option value="h" selected>小时</option><option value="d">天</option></select></span>
              <span class="tm-once" data-show="once"><input type="datetime-local" name="at" value="${localStamp(soon)}"><span class="tm-chips">${[["30 分钟后", 30], ["1 小时后", 60], ["明早 9 点", "am"]].map(([l, v]) => `<button type="button" class="chip-btn" data-at="${v}">${l}</button>`).join("")}</span></span>
            </div>
          </div>
          <label class="tm-text"><span class="tm-lab">说什么</span><textarea name="text" rows="2" placeholder="例如：检查各个任务的进展，有卡住的告诉我" required></textarea></label>
          <div class="tm-foot">
            <label class="tm-target"><span class="tm-lab">交给</span><select name="target" data-pick="compact" data-title="到点交给谁"><option value="" data-sub="由它决定怎么处理">Collie</option>${sup.workers.filter((w) => !w.ended).map((w) => `<option value="${esc(w.name)}" data-sub="${esc(KIND_LABEL[w.kind] || w.kind)}">${esc(w.name)}</option>`).join("")}</select></label>
            <span class="tm-sum muted"></span>
            <button class="btn primary small">添加</button>
          </div>
        </form></div>
      <div class="card"><ul class="rows">${active.length ? active.map(timerRow).join("") : `<li class="empty-row">${ended.length ? "没有启用中的定时" : "还没有定时"}</li>`}</ul></div>
      ${ended.length ? `<details class="mem-done" data-ended-timers><summary class="mem-h">已结束<span class="muted">${ended.length} 个</span></summary>
        <div class="card"><ul class="rows">${ended.map(timerRow).join("")}</ul></div></details>` : ""}`;
  }
  // Timer form: mode, days and quick times are buttons; everything re-describes the spec.
  $("#auto-body").addEventListener("click", (e) => {
    const f = e.target.closest("#timer-form");
    if (!f) return;
    const b = e.target.closest("button[type=button]");
    if (!b) return;
    if (b.dataset.mode) f.dataset.mode = b.dataset.mode;
    if (b.dataset.day) {
      b.classList.toggle("on");
    }
    if (b.dataset.at) {
      let d = new Date();
      if (b.dataset.at === "am") { d.setDate(d.getDate() + 1); d.setHours(9, 0, 0, 0); }
      else d = new Date(d.getTime() + +b.dataset.at * 60000);
      f.at.value = localStamp(d);
    }
    syncTimerForm(f);
  });
  $("#auto-body").addEventListener("input", (e) => { const f = e.target.closest("#timer-form"); if (f) syncTimerForm(f); });
  $("#auto-body").addEventListener("change", (e) => { const f = e.target.closest("#timer-form"); if (f) syncTimerForm(f); });

  function hooksHTML(r) {
    const base = location.origin + "/hooks/";
    const url = (src) => `${base}${src}?key=${r.hook_key}`;
    return `<p class="panel-intro">把外部事件接进来。Webhook 立即返回 202，过滤和路由在收件箱里异步完成。未经请求的事件默认是 <b>candidate</b>：只有 Collie 的记忆或你的规则覆盖到时才会行动，否则保持安静。</p>
      <div class="card"><div class="card-h">Webhook 地址</div><div class="form">
        <label>GitHub <small>Settings → Webhooks，Content type 选 application/json</small><div class="code"><span>${esc(url("github"))}</span><button class="icon-btn" data-copy="${esc(url("github"))}" title="复制"><svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg></button></div></label>
        <label>飞书 / Lark <small>事件订阅的请求地址，已支持 url_verification</small><div class="code"><span>${esc(url("feishu"))}</span><button class="icon-btn" data-copy="${esc(url("feishu"))}" title="复制"><svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg></button></div></label>
        <label>通用 <small>POST JSON：{"text","thread","from","id"}，source 名可自取</small><div class="code"><span>curl -X POST '${esc(url("ci"))}' -H 'Content-Type: application/json' -d '{"thread":"build-42","text":"nightly build failed"}'</span></div></label>
      </div></div>
      <div class="card"><div class="card-h">来源策略（快路径）<button class="btn small" id="add-policy">添加</button></div>
        <div id="policies">${(r.policies || []).map(policyRow).join("")}</div>
        <div class="form"><div class="form-actions"><button class="btn primary small" id="save-policies">保存策略</button></div></div></div>`;
  }

  function policyRow(p = { source: "webhook:", action: "candidate", allow: [] }) {
    return `<div class="policy">
      <input data-f="source" class="mono" value="${esc(p.source)}" placeholder="webhook:github" autocorrect="off" autocapitalize="off" spellcheck="false">
      <select data-f="action" data-pick="compact">${["respond", "candidate", "ignore"].map((a) => `<option value="${a}" ${p.action === a ? "selected" : ""}>${{ respond: "直接响应", candidate: "候选（Collie 判断）", ignore: "忽略" }[a]}</option>`).join("")}</select>
      <input data-f="allow" value="${esc((p.allow || []).join(", "))}" placeholder="白名单（可选，逗号分隔）" autocorrect="off" autocapitalize="off" spellcheck="false">
      <button type="button" class="icon-btn" data-del-policy title="删除"><svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6L6 18"/></svg></button></div>`;
  }

  // ───────────────────────────── settings: grouped by what they are about
  // Fills a model picker and its effort picker from what that agent offers.
  async function fillModels(form, who) {
    const kindSel = form[who + "_kind"], modelSel = form[who + "_model"], effSel = form[who + "_effort"];
    const machine = form[who + "_machine"] ? form[who + "_machine"].value : "";
    const want = modelSel.dataset.value ?? modelSel.value, wantEff = effSel.dataset.value ?? effSel.value;
    modelSel.innerHTML = `<option value="">读取中…</option>`;
    const info = await modelsFor(kindSel.value, machine);
    const list = [...(info.models || [])];
    if (want && !list.some((m) => m.id === want)) list.unshift({ id: want });
    const sub = (m) => [m.label && m.label !== m.id ? m.id : "", m.note && m.note !== m.id && !(m.label && m.note.startsWith(m.label)) ? m.note : ""].filter(Boolean).join(" · ");
    modelSel.innerHTML = `<option value="" data-sub="${esc(info.default ? "现在是 " + info.default : "用 agent 自己的设置")}">默认</option>`
      + list.map((m) => `<option value="${esc(m.id)}" data-sub="${esc(sub(m))}" ${m.id === want ? "selected" : ""}>${esc(m.label || m.id)}</option>`).join("")
      + `<option value="__custom" data-sub="输入列表里没有的模型名">其他…</option>`;
    delete modelSel.dataset.value;
    const fillEffort = () => {
      const m = list.find((x) => x.id === modelSel.value);
      const effs = effortsOf(info, m);
      const cur = effSel.dataset.value ?? effSel.value;
      effSel.innerHTML = `<option value="" data-sub="${esc(info.default_effort ? "现在是 " + (EFFORT_LABEL[info.default_effort] || info.default_effort) : "用 agent 自己的设置")}">默认</option>`
        + effs.map((e) => `<option value="${esc(e)}" data-sub="${esc(e)}" ${e === cur ? "selected" : ""}>${esc(EFFORT_LABEL[e] || e)}</option>`).join("");
      delete effSel.dataset.value;
      effSel.disabled = effs.length === 0;
    };
    fillEffort();
    let prev = modelSel.value;
    const onchange = modelSel.onchange = () => {
      if (modelSel.value !== "__custom") { prev = modelSel.value; fillEffort(); return; }
      modelSel.value = prev;
      customModel(modelSel, (v) => {
        // The list may have been refilled (agent or computer changed) meanwhile.
        if (!v || modelSel.onchange !== onchange) return;
        if (![...modelSel.options].some((o) => o.value === v)) modelSel.add(new Option(v, v), 1);
        modelSel.value = prev = v;
        syncPick(modelSel);
        fillEffort();
      });
    };
  }
  // A model name typed in place of the picker, styled like it: Enter keeps
  // it, Esc or leaving it empty goes back to the list.
  function customModel(sel, done) {
    const b = sel._pick, input = document.createElement("input");
    input.className = "pick-input";
    input.placeholder = "模型名，照 --model 参数写";
    input.autocomplete = "off"; input.spellcheck = false;
    input.setAttribute("autocapitalize", "off");
    b.classList.add("editing");
    b.after(input);
    input.focus();
    input.addEventListener("keydown", (e) => {
      if (e.key !== "Enter" && e.key !== "Escape") return;
      e.preventDefault(); e.stopPropagation();
      if (e.key === "Escape") input.value = "";
      input.blur();
    });
    input.addEventListener("blur", () => {
      input.remove();
      b.classList.remove("editing");
      done(input.value.trim());
      b.focus({ preventScroll: true });
    });
  }
  const formMachine = (form, who) => (form[who + "_machine"] ? form[who + "_machine"].value : "");
  async function fillAliases(form, who) {
    const sel = form[who + "_alias"];
    const want = sel.dataset.value ?? sel.value;
    const all = await aliasesOn(formMachine(form, who));
    const list = aliasesOfKind(all, form[who + "_kind"].value);
    sel.innerHTML = aliasOpts(list, keptAlias(all, list, want));
    delete sel.dataset.value;
    syncPick(sel);
    showRouterArgs(form);
  }
  // An alias brings its own arguments: Collie's extra ones apply only without one.
  const showRouterArgs = (form) => { const adv = form.querySelector(".set-adv"); if (adv) adv.hidden = !!form.router_alias.value; };
  // Picking an alias picks its agent, model and effort; another agent drops it.
  async function pickAlias(form, who) {
    const a = (await aliasesOn(formMachine(form, who))).find((x) => x.name === form[who + "_alias"].value);
    if (!a) return;
    const k = form[who + "_kind"];
    k.value = aliasKind(a.kind);
    syncPick(k);
    form[who + "_model"].dataset.value = a.model || "";
    form[who + "_effort"].dataset.value = a.effort || "";
    fillModels(form, who);
  }
  function bindSettings(form) {
    if (!form || form._bound) return;
    form._bound = true;
    for (const who of ["router", "worker"]) {
      fillModels(form, who);
      fillAliases(form, who);
      form[who + "_kind"].addEventListener("change", () => {
        form[who + "_model"].value = ""; form[who + "_effort"].value = "";
        fillAliases(form, who);
        fillModels(form, who);
      });
      form[who + "_alias"].addEventListener("change", () => { pickAlias(form, who); showRouterArgs(form); });
      form[who + "_machine"]?.addEventListener("change", () => { fillModels(form, who); fillAliases(form, who); });
    }
    renderAliasList();
  }
  // Every alias found, by the rc file it came from.
  async function renderAliasList(fresh) {
    const info = await loadAliases(fresh), box = $("#alias-list");
    if (!box) return;
    const name = (id) => info.machines.find((m) => m.id === id)?.name || id;
    const stale = (rc) => info.machines.filter((m) => m.stale && (m.rc || "") === rc)
      .map((m) => `<span class="tag attn" title="${esc(m.error || "")}">${esc(m.name)} 未更新</span>`).join("");
    const groups = new Map();
    for (const a of info.aliases) groups.set(a.rc, [...(groups.get(a.rc) || []), a]);
    for (const m of info.machines) if (m.stale && m.rc && !groups.has(m.rc)) groups.set(m.rc, []);
    const row = (a) => `<li class="row alias-row" title="${esc(a.command)}">
      <span class="pi">${GLYPHS[a.kind] || ""}</span>
      <div><div class="meta"><b>${esc(a.name)}</b><span class="mono">${esc([KIND_LABEL[a.kind] || a.kind, ...aliasPins(a)].filter(Boolean).join(" · "))}</span></div>
        ${a.base_url ? `<div class="text mono">${esc(a.base_url)}</div>` : ""}</div>
      <span class="right">${a.machines.map((id) => esc(name(id))).join(" · ")}</span></li>`;
    const orphan = stale("");
    box.innerHTML = (orphan ? `<div class="alias-stale">${orphan}</div>` : "")
      + ([...groups].map(([rc, list]) => `<div class="alias-group"><h5>来自 ${esc(rc)} <span class="count">${list.length}</span>${stale(rc)}</h5>
        <ul class="rows">${list.map(row).join("") || `<li class="empty-row">上次没读到</li>`}</ul></div>`).join("")
      || `<p class="muted alias-empty">没在任何电脑的 shell 配置里找到启动 claude、codex 或 agy 的 alias。</p>`);
  }

  function settingsHTML(r) {
    const s = r.settings;
    const kindOpts = (v) => [["claude", "Claude", "claude"], ["codex", "Codex", "codex"], ["antigravity", "Antigravity", "agy"]].map(([k, l, g]) => `<option value="${k}" data-glyph="${g}" ${v === k ? "selected" : ""}>${l}</option>`).join("");
    const modelPick = (who, model, effort, alias) => `
          <label>Alias<select name="${who}_alias" data-title="用哪个 alias 启动" data-value="${esc(alias || "")}"><option value="${esc(alias || "")}" selected>${esc(alias || "-")}</option></select></label>
          <label>模型<select name="${who}_model" data-title="模型" data-value="${esc(model || "")}"><option value="${esc(model || "")}" selected>${esc(model || "默认")}</option></select></label>
          <label>思考强度<select name="${who}_effort" data-pick="compact" data-title="思考强度" data-value="${esc(effort || "")}"><option value="${esc(effort || "")}" selected>${esc(EFFORT_LABEL[effort] || effort || "默认")}</option></select></label>`;
    const many = (state.machines || []).length > 1;
    const machinePick = (name, cur, title) => `<select name="${name}" data-title="${title}">${state.machines.map((m) => `<option value="${esc(m.id)}" data-dot="${m.local || m.online ? "online" : "offline"}" ${m.id === (cur || "") ? "selected" : ""}>${esc(m.name)}${m.local ? "（hub）" : m.online ? "" : "（离线）"}</option>`).join("")}</select>`;
    setTimeout(() => bindSettings($("#settings-form")));
    return `<form class="settings" id="settings-form">
      <section class="card set-sec">
        <header><h4>Collie 自己</h4><p>只负责理解、分派和回复，轻量快速的模型就够。它在 <code>${esc(tilde(r.router_dir))}</code> 里工作：ROUTER.md 是操作手册，memory/ 是长期记忆。</p></header>
        <div class="set-grid four">
          <label>Agent<select name="router_kind" data-title="Collie 自己用的 agent">${kindOpts(s.router_kind)}</select></label>
          ${modelPick("router", s.router_model, s.router_effort, s.router_alias)}
        </div>
        ${many || s.router_machine ? `<div class="set-grid three"><label>电脑${machinePick("router_machine", s.router_machine, "Collie 自己在哪台电脑上运行")}<small>hub 连不上模型时换一台；记忆仍存在 hub 上，自动同步过去</small></label></div>` : ""}
        <details class="set-adv" ${(s.router_args || []).length ? "open" : ""} ${s.router_alias ? "hidden" : ""}><summary>更多启动参数</summary>
          <label><input name="router_args" class="mono" value="${esc((s.router_args || []).join(" "))}" placeholder="例如 --permission-mode plan" autocorrect="off" autocapitalize="off" spellcheck="false"><small>原样加在启动命令后面，一般不用填</small></label>
        </details>
      </section>

      <section class="card set-sec">
        <header><h4>派活默认</h4><p>Collie 派 agent 干活时默认这样启动；你说「用 Codex…」「在 Mac 上…」时它会自己换。</p></header>
        <div class="set-grid four">
          <label>Agent<select name="worker_kind" data-title="派活默认用">${kindOpts(s.worker_kind)}</select></label>
          ${modelPick("worker", s.worker_model, s.worker_effort, s.worker_alias)}
        </div>
        <div class="set-grid three">
          ${many ? `<label>电脑${machinePick("worker_machine", s.worker_machine, "派活默认在哪台电脑")}</label>` : ""}
          <label class="${many ? "span2" : "span3"}">目录<input name="worker_cwd" class="mono" value="${esc(tilde(s.worker_cwd))}" autocorrect="off" autocapitalize="off" spellcheck="false"></label>
        </div>
      </section>

      <section class="card set-sec">
        <header class="set-head"><div><h4>启动 alias</h4><p>每台电脑 shell 配置里启动 claude、codex、agy 的 alias。上面选了 alias，就在那台电脑的 shell 里用它启动，带上它设的地址、密钥和模型。</p></div>
          <button type="button" class="btn small" data-act="alias-refresh">重新读取</button></header>
        <div id="alias-list" class="alias-list"><p class="muted alias-empty">正在读取各台电脑的 alias…</p></div>
      </section>

      <section class="card set-sec">
        <header><h4>记忆与会话</h4><p>Collie 的对话变长后会换新会话，要记住的事通过 memory/ 交接；每晚回顾当天的对话，整理记忆。</p></header>
        <div class="set-grid">
          <label>上下文用到多少时换新会话<select name="rotate_pct" data-pick="compact" data-title="上下文用到多少时换新会话">${[50, 60, 70, 80, -1].map((p) => `<option value="${p}" ${(s.rotate_pct || 60) === p ? "selected" : ""}>${p < 0 ? "从不" : p + "%"}</option>`).join("")}</select><small>窗口大小按 agent 自己的设置算</small></label>
          <label>每晚做梦<select name="dream_at" data-pick="compact" data-title="每晚做梦">${["02:00", "04:00", "06:00", "off"].map((t) => `<option value="${t}" ${(s.dream_at || "04:00") === t ? "selected" : ""}>${t === "off" ? "不做" : t}</option>`).join("")}</select><small>当天没有新对话就跳过</small></label>
        </div>
      </section>

      <section class="card set-sec">
        <header><h4>通知</h4></header>
        <div class="set-grid">
          <label class="check span3"><input type="checkbox" name="notify_lark" ${s.notify_lark ? "checked" : ""}> 用 hub 上的 lark-cli 发飞书消息给你<small id="lark-who" class="muted">· 正在看 hub 上的 lark-cli…</small></label>
          <label>Webhook<input name="notify_webhook" value="${esc(s.notify_webhook || "")}" placeholder="https://open.feishu.cn/open-apis/bot/v2/hook/…" autocorrect="off" autocapitalize="off" spellcheck="false"><small>飞书 / Lark 机器人或 Slack</small></label>
          <label>什么时候推送<select name="notify_level" data-pick="compact" data-title="什么时候推送"><option value="quiet" ${s.notify_level === "quiet" ? "selected" : ""} data-sub="卡住、要你确认、出错">只在需要你时</option><option value="normal" ${s.notify_level === "normal" ? "selected" : ""} data-sub="外加 Collie 的回复，比如做完事的汇报">也推送 Collie 的回复</option></select><small>你正开着 Collie 的网页时不推</small></label>
        </div>
      </section>

      <section class="card set-sec">
        <header><h4>其他</h4></header>
        <div class="set-grid">
          <label>聊天回复长度<input name="reply_chars" type="number" min="40" step="20" value="${s.reply_chars || 200}"><small>Collie 平时每条回复大约多少字（英文按词算）；你要详细时它会写长</small></label>
          <label>多久没进展算卡住<input name="stall_minutes" type="number" min="0" value="${s.stall_minutes}"><small>分钟；0 表示不检查</small></label>
        </div>
        <label class="check"><input type="checkbox" name="bypass" ${s.ask_permissions ? "" : "checked"}> 跳过权限确认：Collie 和它派出的 agent 直接跑命令、改文件，不停下来问你（重启 Collie、新派的 agent 生效）</label>
        <label class="check"><input type="checkbox" name="paused" ${s.paused ? "checked" : ""}> 暂停投递：事件照收，先不交给 agent</label>
      </section>

      <div class="set-actions"><button type="button" class="btn small" data-act="router-restart">重启 Collie</button><span class="grow"></span><button class="btn primary small">保存</button></div>
    </form>`;
  }

  $("#auto-body").addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    try {
      if (f.id === "timer-form") {
        const r = timerSpec(f);
        if (r.err) { toast(r.err, "attn"); return; }
        await api("/api/super/timers", { body: { spec: r.spec, text: f.text.value, target: f.target.value, owner: "you" } });
        toast(`已添加：${describeSpec(r.spec)}`);
        loadAuto();
      } else if (f.classList.contains("mem-edit")) {
        await api("/api/super/memory", { body: { path: f.closest("[data-path]").dataset.path, text: f.text.value } });
        memEditing = false;
        toast("已保存");
        loadAuto();
      } else if (f.id === "settings-form") {
        const cur = (await api("/api/super/settings")).settings;
        const s = {
          ...cur,
          router_kind: f.router_kind.value, router_args: f.router_alias.value ? [] : f.router_args.value.split(/\s+/).filter(Boolean),
          router_machine: f.router_machine ? f.router_machine.value : (cur.router_machine || ""),
          router_model: f.router_model.value === "__custom" ? "" : f.router_model.value, router_effort: f.router_effort.value,
          router_alias: f.router_alias.value, worker_alias: f.worker_alias.value,
          worker_kind: f.worker_kind.value, worker_cwd: f.worker_cwd.value,
          worker_model: f.worker_model.value === "__custom" ? "" : f.worker_model.value, worker_effort: f.worker_effort.value,
          worker_machine: f.worker_machine ? f.worker_machine.value : (cur.worker_machine || ""),
          reply_chars: +f.reply_chars.value, stall_minutes: +f.stall_minutes.value,
          rotate_pct: +f.rotate_pct.value, dream_at: f.dream_at.value,
          notify_webhook: f.notify_webhook.value.trim(), notify_lark: f.notify_lark.checked, notify_level: f.notify_level.value, paused: f.paused.checked,
          ask_permissions: !f.bypass.checked,
        };
        await api("/api/super/settings", { body: { settings: s } });
        renderTarget(true);
        const relaunch = s.router_kind !== cur.router_kind || s.router_machine !== (cur.router_machine || "") || s.router_model !== (cur.router_model || "") || s.router_effort !== (cur.router_effort || "") || s.router_alias !== (cur.router_alias || "") || s.router_args.join(" ") !== (cur.router_args || []).join(" ") || s.ask_permissions !== !!cur.ask_permissions;
        toast(relaunch ? "已保存。Collie 自己的启动设置在重启 Collie 后生效" : "已保存");
      }
    } catch (err) { toast(err.message, "attn"); }
  });

  $("#auto-body").addEventListener("click", async (e) => {
    const t = e.target.closest("button");
    if (!t) return;
    if (t.dataset.act === "mem-edit" || t.dataset.act === "mem-cancel") {
      const box = t.closest("[data-path]");
      memEditing = t.dataset.act === "mem-edit";
      box.querySelector(".mem-edit").hidden = !memEditing;
      box.querySelector("[data-view]").hidden = memEditing;
      box.querySelector(".mem-tools").hidden = memEditing;
      if (memEditing) box.querySelector("textarea").focus();
      else { $("#auto-body")._memSig = ""; loadAuto(); }
      return;
    }
    if (t.dataset.act === "alias-refresh") {
      t.disabled = true; t.textContent = "读取中…";
      await renderAliasList(true);
      const f = $("#settings-form");
      if (f) for (const who of ["router", "worker"]) fillAliases(f, who);
      t.disabled = false; t.textContent = "重新读取";
      return;
    }
    if (t.dataset.act === "dream-now") {
      t.disabled = true; t.textContent = "开始中…";
      try { await api("/api/super/dream", { body: {} }); toast("Collie 去做梦了"); }
      catch (err) { toast(err.message, "attn"); }
      loadAuto();
      return;
    }
    if (t.dataset.delTimer) {
      await api(`/api/super/timers/${t.dataset.delTimer}/delete`, { method: "POST", body: {} }).catch((err) => toast(err.message));
      loadAuto();
    } else if (t.dataset.copy) {
      await copy(t.dataset.copy); toast("已复制");
    } else if (t.id === "add-policy") {
      $("#policies").insertAdjacentHTML("beforeend", policyRow());
    } else if (t.hasAttribute("data-del-policy")) {
      t.closest(".policy").remove();
    } else if (t.id === "save-policies") {
      const policies = $$(".policy").map((row) => ({
        source: $("[data-f=source]", row).value.trim(), action: $("[data-f=action]", row).value,
        allow: $("[data-f=allow]", row).value.split(",").map((x) => x.trim()).filter(Boolean),
      })).filter((p) => p.source);
      try { await api("/api/super/settings", { body: { policies } }); toast("策略已保存"); } catch (err) { toast(err.message, "attn"); }
    }
  });

  // ───────────────────────────── agents in other terminals (outside herdr)
  let ext = [], extTimer, extStream = null, extCur = null;
  async function loadExternal() {
    if (document.hidden && ext.length) return;
    try { ext = (await api("/api/external")).sessions || []; } catch { return; }
    ext = ext.concat(...await fromHubs("/api/external", "ext"));
    renderExt();
    if (view === "ext") renderExtHead();
  }

  // The same list from every other hub, ids tagged; a hub that doesn't
  // answer keeps what it showed last until it is marked offline.
  async function fromHubs(path, field) {
    return Promise.all(hubs.map(async (h) => {
      const r = remote.get(h.key);
      if (!r) return [];
      if (!r.online) return (r[field] = []);
      try {
        const d = await api(path, { hub: h });
        r[field] = (d.sessions || []).map((e) => ({ ...tagIds(e, h.key), id: tagged(e.id, h.key), hub: h.key }));
      } catch {}
      return r[field] || [];
    }));
  }

  // ───────────────────────────── ended sessions: read them, continue them
  let hist = [];
  // workerHist: the ended session named name that a line at time at was
  // about: the first of that name to end after it.
  const workerHist = (name, at) => {
    const t = new Date(at);
    let best = null;
    for (const h of hist) if (h.name === name && new Date(h.ended) >= t && (!best || new Date(h.ended) < new Date(best.ended))) best = h;
    return best;
  };
  // routerHist: the ended Collie session closest in time to an older
  // handover/dream line written before explicit "h:<id>" refs.
  const routerHist = (at) => {
    const t = +new Date(at);
    if (!t) return null;
    let best = null, bestDiff = 10 * 60e3;
    for (const h of hist) {
      if (!isRouterHistory(h)) continue;
      const d = Math.abs(+new Date(h.ended) - t);
      if (d < bestDiff) { best = h; bestDiff = d; }
    }
    return best;
  };
  async function loadHistory() {
    loadMarks();
    try { hist = (await api("/api/history")).sessions || []; } catch { return; }
    hist = hist.concat(...await fromHubs("/api/history", "hist"));
    renderHist();
    if (view === "collie") renderSuper();
    if (view === "ext" && extCur?.startsWith("h:")) renderHistHead(extCur.slice(2));
  }
  const ago = (t) => {
    const s = (Date.now() - new Date(t)) / 1000;
    return s < 90 ? "刚刚" : s < 3600 ? Math.round(s / 60) + " 分钟前" : s < 86400 ? Math.round(s / 3600) + " 小时前" : Math.round(s / 86400) + " 天前";
  };
  const histName = (h) => h.title || (h.name && !autoName(h.name) ? h.name : base(tilde(h.cwd)));
  const histOnline = (h) => !h.machine || (state.machines || []).some((m) => m.id === h.machine && m.online);
  const renderHist = () => renderSessions();
  function renderHistHead(hid) {
    const h = hist.find((x) => x.id === hid);
    const on = h && histOnline(h);
    $("#x-name").textContent = h ? histName(h) : "已结束的会话";
    $("#x-kind").textContent = h ? KIND_LABEL[h.kind] || h.kind : "";
    $("#x-tag").textContent = "已结束";
    $("#x-status").className = "status ended";
    $("#x-status").textContent = h ? "结束于 " + ago(h.ended) : "";
    $("#x-cwd").textContent = h ? (h.machine ? machineName(h.machine) + " · " : "") + tilde(h.cwd) : "";
    $("#x-note").textContent = !h ? "这条记录已经不在了。"
      : on ? `继续会${h.machine ? "在 " + machineName(h.machine) + " 上" : ""}用同一个会话重新打开 ${KIND_LABEL[h.kind] || h.kind}，之前的上下文都还在。`
      : `${machineName(h.machine)} 现在不在线，连上后才能继续。`;
    const b = $("#x-takeover");
    b.textContent = "继续这个会话";
    b.hidden = false;
    b.disabled = !on;
    $("#x-forget").hidden = !h;
  }
  async function openHist(hid, restore = null) {
    const el = $("#x-thread");
    resetEarlier(el, null); el._msgs = null;
    el._restore = restore && !restore.atBottom ? restore : null;
    primeThread(el, "h:" + hid);
    if (!hist.some((h) => h.id === hid)) await loadHistory();
    renderHistHead(hid);
    const h = hist.find((x) => x.id === hid);
    if (!h) { el.innerHTML = ""; el._restore = null; return; }
    try {
      const q = new URLSearchParams({ kind: h.kind, session: h.session, cwd: h.cwd });
      if (h.machine) q.set("machine", h.machine);
      const r = await api("/api/transcript?" + q);
      if (view === "ext" && extCur === "h:" + hid) {
        resetEarlier(el, (c) => `/api/transcript?${q}&before=${c}`);
        el._msgs = r.messages || []; el._liveEarlier = r.earlier || 0;
        keepThread("h:" + hid, el._msgs);
        el._redraw = () => { if (view === "ext" && extCur === "h:" + hid) drawThread(threadMsgs(el), el, "这个会话没有消息。"); };
        el._redraw();
      }
    } catch (err) {
      el._restore = null;
      if (view === "ext" && extCur === "h:" + hid) el.innerHTML = `<div class="thread-empty">${esc(err.message)}</div>`;
    }
  }
  const RESUME = { claude: (s) => ["--resume", s], codex: (s) => ["resume", s], agy: (s) => ["--conversation", s] };
  async function continueHist(hid) {
    const h = hist.find((x) => x.id === hid);
    if (!h || !RESUME[h.kind]) return;
    const b = $("#x-takeover");
    b.disabled = true; b.textContent = "正在打开…";
    try {
      const r = await api("/api/agents", { body: {
        kind: h.kind, cwd: h.cwd, machine: h.machine || "", name: "", label: histName(h).slice(0, 40), args: RESUME[h.kind](h.session),
      } });
      if (r.warning) toast(r.warning);
      await api(`/api/history/${encodeURIComponent(h.id)}/delete`, { body: {} }).catch(() => {});
      hist = hist.filter((x) => x.id !== h.id);
      renderHist();
      replaceHash("#/p/" + encodeURIComponent(r.pane_id));
      route();
    } catch (err) {
      toast(err.message, "attn");
      renderHistHead(hid);
    }
  }
  $("#x-forget").addEventListener("click", async () => {
    const hid = extCur?.startsWith("h:") && extCur.slice(2);
    if (!hid) return;
    try { await api(`/api/history/${encodeURIComponent(hid)}/delete`, { body: {} }); } catch (err) { toast(err.message, "attn"); return; }
    hist = hist.filter((x) => x.id !== hid);
    renderHist();
    if (store.get("last") === location.hash) store.set("last", "#/collie");
    goBack(true);
  });
  const extName = (e) => e.title || (e.cwd ? e.cwd.split("/").filter(Boolean).pop() : "") || KIND_LABEL[e.kind] || e.kind;
  // The sidebar lists sessions by project (the repository they run in), what
  // needs you on top. The view shows them all, one computer's, or by what
  // you need: what needs you, what just finished, what is going on now
  // (herdr agents, yours and Collie's, and other apps' running sessions),
  // what rests, then what you did lately (other apps' resting conversations
  // and ended herdr sessions, merged). A kind narrows to one agent or the
  // terminals.
  const RECENT_N = 8;
  const FOLDER_SVG = '<svg class="gico" viewBox="0 0 24 24"><path d="M3.5 7.5a2 2 0 0 1 2-2h4l2 2h7a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z"/></svg>';
  const SIDE_KINDS = [...AGENT_KINDS, ["term", "终端"]];
  let extAll = store.get("extAll", false);
  let sideView = store.get("sideView", "all"), sideKind = store.get("sideKind", "");
  if (!(sideView === "all" || sideView === "status" || sideView.startsWith("m:"))) sideView = "all";
  if (!SIDE_KINDS.some(([k]) => k === sideKind)) sideKind = "";
  // Folds of groupings that are gone are dropped.
  const sideFold = Object.fromEntries(Object.entries(store.get("sideFold", {})).filter(([k]) => /^(status|p):/.test(k)));
  const EXT_STATE = { working: "在干活", waiting: "在等后台任务", idle: "开着，在等你", blocked: "等电脑那边确认" };
  const RANK = { working: 0, waiting: 0.5, starting: 1, blocked: 2, done: 3, unknown: 4, idle: 4 };
  const ENDED_RANK = 9; // ended and resting sessions sort after anything open
  const STATUS_GROUPS = [["attn", "需要你"], ["done", "完成未看"], ["going", "进行中"], ["idle", "空闲"], ["recent", "最近"], ["term", "终端"]];
  // Worktrees live inside their repository: list them under it.
  const WORKTREE_RE = /^(.+?)\/(?:\.worktrees|\.claude\/worktrees|\.herdr\/worktrees)\/./;
  const repoOf = (cwd) => { const p = (cwd || "").replace(/\/+$/, ""); return p.match(WORKTREE_RE)?.[1] || p; };
  // Another app's session Collie can only read (no way to send to it). The
  // one open now stays, so it doesn't vanish under you.
  const readOnlyHidden = (e) => !e.can_send && !prefs.readOnly() && !(view === "ext" && extCur === e.id);
  function renderExt() { renderSessions(); }
  // What the hub keeps about a session for you (marked unread, hidden, the
  // name you gave it), by computer, agent and session. A remote hub's tag is
  // spelled differently in the key so the request stays on this hub.
  let marks = {}, sideByRow = new Map();
  // Ids from another hub may carry its tag more than once; the key keeps one.
  const machineParts = (machine) => String(machine || "").split("!");
  const markKey = (machine, kind, session) => {
    if (!session) return "";
    const [m, hub] = machineParts(machine);
    return [m + (hub ? "~" + hub : ""), shortKind(kind), session].join("|");
  };
  async function loadMarks() {
    try { marks = (await api("/api/marks")).marks || {}; } catch { return; }
    renderSessions();
  }
  async function setMark(key, change) {
    marks = (await api("/api/marks", { body: { key, ...change } })).marks || {};
    renderSessions();
  }
  function sideRows() {
    const many = (state.machines || []).length > 1;
    const rowAttrs = (id, m) => ` data-row="${esc(id)}"${m?.unread ? " data-unread" : ""}`;
    const unreadDot = `<span class="dot done" title="未读"></span>`;
    const extRow = (e, key, m) => {
      const open = e.live && e.status === "idle";
      const where = [e.app || KIND_LABEL[e.kind] || e.kind, many && e.machine ? machineName(e.machine) : "", e.live && !open ? "" : ago(e.updated || e.started)].filter(Boolean).join(" · ");
      const dot = e.live ? `<span class="dot ${esc(e.status || "unknown")}" title="${esc(EXT_STATE[e.status] || "正在运行")}"></span>` : m?.unread ? unreadDot : "";
      return `<li><a class="item ${e.live ? "" : "ended"} ${view === "ext" && extCur === e.id ? "active" : ""}"${rowAttrs(e.id, m)} href="#/x/${encodeURIComponent(e.id)}">
        <span class="glyph">${GLYPHS[e.kind] || GLYPHS.agent}</span>
        <span class="body"><div class="name">${esc(m?.title || extName(e))}</div><div class="sub">${esc(where)}</div></span>${dot}</a></li>`;
    };
    const histRow = (h, key, m) => {
      const sub = [KIND_LABEL[h.kind] || h.kind, many ? machineName(h.machine) : "", ago(h.ended)].filter(Boolean).join(" · ");
      return `<li><a class="item ended ${view === "ext" && extCur === "h:" + h.id ? "active" : ""}"${rowAttrs("h:" + h.id, m)} href="#/h/${encodeURIComponent(h.id)}">
        <span class="glyph">${GLYPHS[h.kind] || GLYPHS.agent}</span>
        <span class="body"><div class="name">${esc(m?.title || histName(h))}</div><div class="sub">${esc(sub)}</div></span>${m?.unread ? unreadDot : ""}</a></li>`;
    };
    const pane = (a, term) => {
      const key = term ? "" : markKey(a.machine, a.kind, a.session), m = marks[key];
      // Marked unread, a resting agent reads as finished and not looked at.
      const v = m?.unread && a.status === "idle" ? { ...a, status: "done" } : a;
      const rank = RANK[v.status] ?? 3;
      const bucket = term ? "term" : v.status === "blocked" ? "attn" : v.status === "done" ? "done" : rank <= 1 ? "going" : "idle";
      return { id: a.pane_id, src: "pane", html: itemHTML(v, rowAttrs(a.pane_id, m)), bucket, kind: term ? "" : shortKind(a.kind), rank, t: 0, machine: a.machine || "", cwd: a.cwd,
        key, mark: m, session: a.session || "", name: term ? "" : title(a), term: term ? termName(a) : "", status: a.status, ws: a.workspace_id, live: true };
    };
    // Another app's session that is only open (kept alive in the background,
    // nothing running) is listed under 最近 by its time, its dot saying it is
    // still open. Collie's own ended sessions are reached from its conversation.
    const going = (e) => e.live && e.status !== "idle" && e.status !== "unknown";
    const workers = new Set(sup.workers.map((w) => w.name));
    // A worker's session is Collie's by its id: its title can change (Codex
    // names threads, anyone can rename them).
    const workerSessions = new Set(sup.workers.map((w) => w.session).filter(Boolean));
    const collieRan = (h) => isRouterHistory(h) || workers.has(h.name) || workerSessions.has(h.session);
    // Another hub's own computer has no machine id: it is that hub.
    const machineOf = (x) => x.hub ? tagged(x.machine || "", x.hub) : x.machine || "";
    const rest = (r) => {
      // Marked unread, it waits under 完成未看 until opened.
      if (r.mark?.unread && r.rank >= ENDED_RANK) r.bucket = "done";
      return r;
    };
    return [
      ...state.agents.filter((a) => !isLocalRouter(a) || a.status === "blocked").map((a) => pane(a, false)),
      ...state.shells.map((a) => pane(a, true)),
      ...ext.filter((e) => !readOnlyHidden(e) && !workerSessions.has(e.session)).map((e) => {
        const machine = machineOf(e), key = markKey(machine, e.kind, e.session), m = marks[key];
        return rest({ id: e.id, src: "ext", html: extRow(e, key, m), kind: shortKind(e.kind), bucket: going(e) ? "going" : "recent", rank: going(e) ? RANK[e.status] ?? 3 : ENDED_RANK,
          t: new Date(e.updated || e.started), machine, cwd: e.cwd, key, mark: m, session: e.session || "", name: m?.title || extName(e), status: e.status, live: !!e.live });
      }),
      ...hist.filter((h) => !collieRan(h)).map((h) => {
        const machine = machineOf(h), key = markKey(machine, h.kind, h.session), m = marks[key];
        return rest({ id: "h:" + h.id, src: "hist", html: histRow(h, key, m), kind: shortKind(h.kind), bucket: "recent", rank: ENDED_RANK, t: new Date(h.ended), machine, cwd: h.cwd,
          key, mark: m, session: h.session || "", name: m?.title || histName(h), live: false });
      }),
    // Archived where its agent has no archive of its own (or not yet listed
    // as archived): hidden while it rests.
    ].filter((r) => !(r.mark?.archived && r.rank >= ENDED_RANK));
  }
  // [{ key, label, on, title, rows }], in the order they are listed.
  function sideGroups(rows) {
    if (sideView === "status") return STATUS_GROUPS.map(([key, label]) => ({ key, label, rows: rows.filter((r) => r.bucket === key) }));
    // A project is its folder's name on one computer: a repository and its
    // worktrees (wherever they live) are one project.
    const by = new Map();
    for (const r of rows) {
      if (r.bucket === "attn") continue;
      const path = repoOf(r.cwd), label = path ? base(path) : "其他", key = r.machine + "\n" + label;
      if (!by.has(key)) by.set(key, { key, machine: r.machine, label, paths: new Set(), project: true, rows: [], best: Infinity, last: 0 });
      const g = by.get(key);
      if (path) g.paths.add(tilde(path));
      g.rows.push(r); g.best = Math.min(g.best, r.rank); g.last = Math.max(g.last, +r.t || 0);
    }
    const groups = [...by.values()].sort((x, y) => x.best - y.best || y.last - x.last || x.label.localeCompare(y.label));
    for (const g of groups) {
      g.title = [...g.paths].join("\n");
      // The same project on another computer says which.
      if (groups.some((o) => o !== g && o.label === g.label)) g.on = machineName(g.machine);
      g.rows.sort((x, y) => x.rank - y.rank || y.t - x.t);
    }
    return [{ key: "attn", label: "需要你", rows: rows.filter((r) => r.bucket === "attn") }, ...groups];
  }
  const sideHere = (r) => view === "ext" ? r.id === extCur : view === "pane" && r.id === current;
  // Opening a session marked unread reads it, once per opening: marking the
  // open one unread keeps it so.
  let hereSeen = "";
  function readOnOpen(all) {
    const here = all.find(sideHere);
    if (here?.id === hereSeen) return;
    hereSeen = here?.id || "";
    if (here?.mark?.unread) setMark(here.key, { unread: false }).catch((err) => toast(err.message, "attn"));
  }
  // Narrowed to a kind, the status groups show open.
  const sideFoldable = () => !(sideView === "status" && sideKind);
  function renderSessions() {
    const all = sideRows();
    readOnOpen(all);
    sideByRow = new Map(all.filter((r) => r.kind || r.term).map((r) => [r.id, r]));
    const kindOf = (r) => r.bucket === "term" ? "term" : r.kind;
    const nOn = {};
    for (const r of all) nOn[r.machine] = (nOn[r.machine] || 0) + 1;
    const ms = state.machines || [];
    const online = (id) => { const m = ms.find((x) => x.id === id); return m ? (m.local ? state.ok : m.online) : undefined; };
    let on = sideView.startsWith("m:") ? sideView.slice(2) : null;
    const ids = [...new Set([...ms.filter((m) => !m.hub).map((m) => m.id), ...all.map((r) => r.machine)])]
      .filter((id) => nOn[id] || id === on);
    // A computer that is gone (not even offline) leaves the view on all.
    if (on !== null && !ids.includes(on)) { on = null; sideView = "all"; }
    const inView = (r) => on === null || r.machine === on;
    // A filter never hides what needs you.
    const viewRows = all.filter((r) => inView(r) || r.bucket === "attn");
    const rows = sideKind ? viewRows.filter((r) => kindOf(r) === sideKind || r.bucket === "attn") : viewRows;
    const nKind = {};
    for (const r of all) if (inView(r)) nKind[kindOf(r)] = (nKind[kindOf(r)] || 0) + 1;
    const viewList = [["all", "全部"], ["status", "状态"], ...(ids.length > 1 || on !== null ? ids.map((id) => ["m:" + id, machineName(id), online(id)]) : [])];
    const viewHTML = viewList.map(([v, label, up]) =>
      `<button type="button" data-view="${esc(v)}" class="${v === sideView ? "on" : ""}">${up === undefined ? "" : `<span class="dot ${up ? "online" : "offline"}"></span>`}${esc(label)}</button>`).join("");
    const kinds = SIDE_KINDS.filter(([v]) => nKind[v] || v === sideKind);
    const chipsHTML = kinds.length < 2 && !sideKind ? "" : kinds.map(([v, label]) =>
      `<button type="button" data-kind="${v}" class="${v === sideKind ? "on" : ""}">${label} <span class="k">${nKind[v] || 0}</span></button>`).join("");

    const groupHTML = (g) => {
      const open = g.rows.filter((r) => r.rank < ENDED_RANK), ended = g.rows.filter((r) => r.rank >= ENDED_RANK);
      let shown = extAll ? ended : ended.slice(0, RECENT_N);
      const cur = view === "ext" && extCur && ended.find((r) => r.id === extCur && !shown.includes(r));
      if (cur) shown = [...shown, cur];
      const fk = (g.project ? "p:" : "status:") + g.key, dflt = !g.project && g.key === "term";
      const folded = sideFoldable() && (fk in sideFold ? sideFold[fk] : dflt);
      const busy = g.key === "going" ? g.rows.filter((r) => r.rank <= 1).length : 0;
      const count = busy ? `${busy} 个在跑` : g.rows.length || "";
      const empty = g.key === "going" && !g.rows.length
        ? `<li class="empty-hint">没有在跑的会话。点右上角 + 新建，或让 Collie 派一个。</li>` : "";
      return `<details class="group ${g.project ? "proj" : ""}" data-fold="${esc(fk)}" data-dflt="${dflt ? 1 : 0}" ${folded ? "" : "open"}>
        <summary${g.title ? ` title="${esc(g.title)}"` : ""}><h2>${g.project ? FOLDER_SVG : ""}<span class="gname">${esc(g.label)}</span>${g.on ? `<span class="gon">${esc(g.on)}</span>` : ""}<span class="count">${esc(count)}</span></h2></summary>
        <ul class="list ${g.key === "attn" ? "attn" : ""}">${[...open, ...shown].map((r) => r.html).join("")}${empty}${ended.length > RECENT_N
          ? `<li><button type="button" class="more-row" data-act="ext-more">${extAll ? "收起" : `还有 ${ended.length - RECENT_N} 个`}</button></li>` : ""}</ul>
      </details>`;
    };
    // 进行中 is always there when nothing runs, to say how to start one.
    const groups = sideGroups(rows).filter((g) => g.rows.length || (g.key === "going" && !sideKind
      && !rows.some((r) => ["done", "going", "idle"].includes(r.bucket))));
    const html = groups.map(groupHTML).join("") || `<p class="empty-hint">${sideKind === "term" ? "没有开着的终端。"
      : sideKind ? "没有这个 agent 的会话。" : on !== null ? "这台电脑上没有会话。" : "没有会话。点右上角 + 新建，或让 Collie 派一个。"}</p>`;
    const box = $("#side-groups"), views = $("#side-view"), bar = $("#side-only");
    if (box._html !== html) box.innerHTML = box._html = html;
    if (views._html !== viewHTML) views.innerHTML = views._html = viewHTML;
    if (bar._html !== chipsHTML) bar.innerHTML = bar._html = chipsHTML;
    bar.hidden = !chipsHTML;
  }
  // Look up the open session again in a moment (a few times at most).
  let extSoonT = null, extSoonN = 0;
  function extSoon() {
    if (extSoonT || extSoonN >= 15) return;
    extSoonN++;
    extSoonT = setTimeout(() => { extSoonT = null; if (view === "ext") loadExternal(); }, 1500);
  }
  function extWorking(gone) {
    const box = $("#x-working");
    const e = !gone && !extCur?.startsWith("h:") && ext.find((x) => x.id === extCur);
    const st = e && e.live ? e.status : "";
    const text = { working: `${e && e.app || "那边"}里还在跑…`, blocked: "在等你在电脑上确认" }[st] || "";
    const prev = box.hidden ? "" : box.textContent;
    if (!text) { box.hidden = true; return; }
    if (text === prev) return;
    const was = nearBottom($("#x-thread"));
    box.className = "working-row " + st;
    box.innerHTML = `<span class="dot ${st}"></span>${esc(text)}`;
    box.hidden = false;
    stick($("#x-thread"), was);
  }
  function renderExtHead(gone) {
    extWorking(gone);
    if (extCur?.startsWith("h:")) return renderHistHead(extCur.slice(2));
    $("#x-tag").textContent = "只读";
    $("#x-forget").hidden = true;
    $("#x-takeover").textContent = "在 herdr 里接管";
    $("#x-takeover").className = "btn primary small";
    $("#x-composer").hidden = true;
    const e = ext.find((x) => x.id === extCur);
    if (!e) {
      $("#x-status").textContent = gone ? "已退出" : "正在连接那台电脑…";
      $("#x-status").className = "status " + (gone ? "ended" : "unknown");
      $("#x-takeover").disabled = true;
      if (!gone) extSoon();
      return;
    }
    $("#x-name").textContent = extName(e);
    $("#x-kind").textContent = KIND_LABEL[e.kind] || e.kind;
    $("#x-status").textContent = gone ? "已退出" : `正在运行 · ${e.app || "另一个终端"}`;
    $("#x-status").className = "status " + (gone ? "ended" : "unknown");
    $("#x-cwd").textContent = (e.machine ? machineName(e.machine) + " · " : "") + tilde(e.cwd);
    const app = e.app || "另一个终端";
    if (!e.live && !gone) {
      // A desktop app's conversation that is not running right now.
      $("#x-tag").textContent = e.can_send ? "可对话" : "只读";
      $("#x-status").textContent = `上次活动 ${ago(e.updated || e.started)} · ${app}`;
      $("#x-status").className = "status ended";
      $("#x-takeover").hidden = !e.can_take_over;
      $("#x-takeover").disabled = false;
      $("#x-takeover").textContent = "在 herdr 里继续";
      $("#x-takeover").className = "btn small";
      $("#x-composer").hidden = !e.can_send;
      if (e.can_send) $("#x-hint").textContent = e.kind === "agy" ? "经 Antigravity 送达" : "经 Codex App 送达";
      $("#x-note").textContent = e.can_send ? ""
        : (e.read_only || "") + (e.can_take_over ? "。也可以在 herdr 里用同一个会话继续，之后就能在这里对话。" : "");
      return;
    }
    $("#x-takeover").hidden = !e.can_take_over;
    $("#x-note").textContent = gone ? "这个进程已经退出了。"
      : !e.session ? `它跑在 ${app} 里，还没有会话记录，所以没法接管。`
      : `它跑在 ${app} 里，Collie 只能看，不能往里面打字。接管会先结束那边的进程（相当于按 Ctrl-C），再在 herdr 里接着同一个会话继续，之后就能在这里和它对话。`;
    $("#x-takeover").disabled = !!gone || !e.session;
    if (e.status && !gone) {
      $("#x-status").textContent = `${{ working: "正在跑", waiting: "在等后台任务", idle: "空闲，等你发话", blocked: "等电脑那边确认" }[e.status] || "正在运行"} · ${app}`;
      $("#x-status").className = "status " + e.status;
    }
    if (e.can_send && !gone) {
      // It takes messages where it runs: no need to take it over.
      $("#x-tag").textContent = "可对话";
      $("#x-composer").hidden = false;
      $("#x-takeover").className = "btn small";
      $("#x-hint").textContent = e.kind === "agy" ? "经 Antigravity 送达" : e.app === "Codex 桌面版" ? "经 Codex App 送达" : e.kind === "codex" ? "经 Codex 后台送达" : "经会话收件箱送达";
      $("#x-note").textContent = "";
    } else if (!gone && e.read_only && e.session) {
      $("#x-note").textContent = e.read_only + "。接管会先结束那边的进程（相当于按 Ctrl-C），再在 herdr 里接着同一个会话继续。";
    }
  }
  bindComposer($("#x-composer"), $("#x-input"), (draft, ready) => {
    const e = ext.find((x) => x.id === extCur);
    if (!e) throw new Error("这个会话已经结束了");
    return trackSend("x:" + e.id, draft, ready, async (text) => {
      const r = await api(`/api/external/${encodeURIComponent(e.id)}/send`, { body: { text } });
      if (r.note) toast(r.note, "attn");
    });
  }, () => { // files go to the computer it runs on (through its hub, if another)
    const e = ext.find((x) => x.id === extCur);
    return e?.hub ? tagged(e.machine || "", e.hub) : e?.machine || "";
  });
  function closeExt() { extStream?.close(); extStream = null; }
  $(".side-scroll").addEventListener("click", (ev) => {
    const v = ev.target.closest("[data-view]"), k = ev.target.closest("[data-kind]");
    const det = ev.target.closest("summary")?.parentElement, fold = det?.dataset.fold;
    if (v) { sideView = v.dataset.view; store.set("sideView", sideView); }
    // A kind is on or off: tapping it again shows every kind.
    else if (k) { sideKind = k.dataset.kind === sideKind ? "" : k.dataset.kind; store.set("sideKind", sideKind); }
    else if (fold) {
      // Re-rendered on every update: the open state lives here, not in the
      // DOM; only what differs from the default is kept.
      ev.preventDefault();
      if (!sideFoldable()) return;
      const folded = det.open;
      if (folded === (det.dataset.dflt === "1")) delete sideFold[fold]; else sideFold[fold] = folded;
      store.set("sideFold", sideFold);
    } else if (ev.target.closest("[data-act=ext-more]")) {
      ev.preventDefault();
      extAll = !extAll; store.set("extAll", extAll);
    } else return;
    renderSessions();
  });
  // ───────────────────────────── the app's own confirm and prompt
  // ask() resolves true (or the text typed, when given a value to edit) on
  // OK, null when cancelled; warn colours OK for what can't be taken back.
  const askDlg = $("#ask");
  // askEnd answers the ask on screen; replacing it answers the old one null.
  let askEnd = null;
  function ask({ title, text = "", value, ok = "确定", warn = false }) {
    askEnd?.("");
    if (askDlg.open) askDlg.close("");
    $("#ask-title").textContent = title;
    $("#ask-text").textContent = text;
    $("#ask-text").hidden = !text;
    const input = $("#ask-input"), okBtn = $("#ask-ok");
    $("#ask-field").hidden = value === undefined;
    input.value = value ?? "";
    okBtn.textContent = ok;
    okBtn.classList.toggle("warn", warn);
    askDlg.returnValue = "";
    askDlg.showModal();
    if (value === undefined) okBtn.focus(); else { input.focus(); input.select(); }
    return new Promise((resolve) => askEnd = (rv) => { askEnd = null; resolve(rv !== "ok" ? null : value === undefined ? true : input.value); });
  }
  // A close queued by replacing an ask arrives once the new one is open: not its answer.
  askDlg.addEventListener("close", () => { if (!askDlg.open) askEnd?.(askDlg.returnValue); });
  askDlg.addEventListener("click", (e) => { if (e.target.closest("[data-ask-cancel]") || e.target === askDlg) askDlg.close(""); });
  // Enter that picks an input-method word must not submit.
  $("#ask-input").addEventListener("keydown", (e) => { if (e.key === "Enter" && composing(e)) e.preventDefault(); });

  // ───────────────────────────── a session's menu
  // Right-click a session (or hold it on a touch screen): rename it, mark it
  // unread, copy its id, archive it. Renaming and archiving go to its own
  // agent (session_menu.go); Claude Code has no archive, so Collie hides it.
  // A terminal's menu only closes it.
  const ctxPop = document.createElement("div");
  ctxPop.className = "pick-pop ctx-menu"; ctxPop.hidden = true; ctxPop.setAttribute("role", "menu");
  const ctxBack = document.createElement("div");
  ctxBack.className = "pick-back"; ctxBack.hidden = true;
  document.body.append(ctxBack, ctxPop);
  let ctxRow = null;
  const CTX_ICONS = {
    rename: '<svg viewBox="0 0 24 24"><path d="M4 20h4L19 9l-4-4L4 16z"/><path d="M13.5 6.5l4 4"/></svg>',
    unread: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="8"/></svg>',
    read: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="8"/><path d="M8.5 12.5l2.5 2.5 4.5-5"/></svg>',
    copy: '<svg viewBox="0 0 24 24"><rect x="8" y="8" width="12" height="12" rx="2.5"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/></svg>',
    archive: '<svg viewBox="0 0 24 24"><rect x="3.5" y="4.5" width="17" height="4.5" rx="1.5"/><path d="M5 9v9a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V9M10 13h4"/></svg>',
    close: '<svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6L6 18"/></svg>',
  };
  const CTX_DANGER = new Set(["archive", "close"]);
  // A running Claude Code renames itself by its /rename; the others by their own API.
  const typesRename = (r) => r.src === "pane" && r.kind === "claude";
  function openCtx(row, x, y) {
    closePick();
    ctxRow = row;
    const unread = !!row.mark?.unread;
    const items = row.term ? [["close", "关闭", false]] : [
      ["rename", "重命名", !typesRename(row) && !row.session],
      [unread ? "read" : "unread", unread ? "标记为已读" : "标记为未读", !row.key],
      ["copy", "复制对话 ID", !row.session],
      ["archive", "归档", !row.session],
    ];
    ctxPop.innerHTML = `<div class="ph">${esc(row.term || row.name)}</div>` + items.map(([act, label, off]) =>
      `<button type="button" class="po${CTX_DANGER.has(act) ? " danger" : ""}" role="menuitem" data-ctx="${act}" ${off ? "disabled" : ""}><span class="pi">${CTX_ICONS[act]}</span><span class="pl"><span>${label}</span></span></button>`).join("");
    const sheet = narrow();
    ctxPop.classList.toggle("sheet", sheet);
    ctxBack.hidden = !sheet;
    ctxPop.hidden = false;
    if (sheet) ctxPop.style.cssText = "";
    else {
      ctxPop.style.minWidth = "180px";
      const w = ctxPop.offsetWidth, h = ctxPop.offsetHeight;
      ctxPop.style.left = Math.min(Math.max(8, x), innerWidth - w - 8) + "px";
      ctxPop.style.top = Math.min(Math.max(8, y), innerHeight - h - 8) + "px";
    }
    $(".po:not([disabled])", ctxPop)?.focus({ preventScroll: true });
  }
  function closeCtx() { ctxPop.hidden = true; ctxBack.hidden = true; ctxRow = null; }
  const sideBox = $("#side-groups");
  // Holding a session on a touch screen opens the menu too (iOS sends no
  // contextmenu for a link); the tap that ends the hold doesn't open it.
  const HOLD_MS = 500, HOLD_SLOP = 10;
  let hold = null;
  const dropHold = () => { if (hold) clearTimeout(hold.t); };
  sideBox.addEventListener("contextmenu", (e) => {
    const row = sideByRow.get(e.target.closest(".item[data-row]")?.dataset.row);
    if (!row) return;
    e.preventDefault();
    dropHold();
    if (hold) hold.fired = true;
    openCtx(row, e.clientX, e.clientY);
  });
  sideBox.addEventListener("pointerdown", (e) => {
    hold = null;
    const a = e.pointerType === "touch" && e.target.closest(".item[data-row]");
    if (!a) return;
    hold = { x: e.clientX, y: e.clientY, fired: false };
    hold.t = setTimeout(() => {
      const row = sideByRow.get(a.dataset.row);
      if (!hold || !row) return;
      hold.fired = true;
      openCtx(row, hold.x, hold.y);
    }, HOLD_MS);
  });
  sideBox.addEventListener("pointermove", (e) => {
    if (hold && !hold.fired && Math.hypot(e.clientX - hold.x, e.clientY - hold.y) > HOLD_SLOP) { dropHold(); hold = null; }
  });
  sideBox.addEventListener("pointerup", dropHold);
  sideBox.addEventListener("pointercancel", () => { dropHold(); hold = null; });
  sideBox.addEventListener("click", (e) => {
    if (hold?.fired) { e.preventDefault(); e.stopPropagation(); }
    hold = null;
  }, true);
  ctxBack.addEventListener("click", closeCtx);
  document.addEventListener("mousedown", (e) => { if (!ctxPop.hidden && !ctxPop.contains(e.target)) closeCtx(); });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !ctxPop.hidden) closeCtx(); });
  addEventListener("resize", () => { if (!ctxPop.hidden) closeCtx(); });
  ctxPop.addEventListener("click", async (e) => {
    const b = e.target.closest("[data-ctx]");
    if (!b || !ctxRow) return;
    const row = ctxRow, act = b.dataset.ctx;
    closeCtx();
    try { await ctxAct(row, act); } catch (err) { toast(err.message, "attn"); }
  });
  // The session's id with its computer (and hub), so the request goes there.
  const sessionPath = (r) => {
    const [m, hub] = machineParts(r.machine);
    return encodeURIComponent(r.session + (m ? "@" + m : "") + (hub ? "!" + hub : ""));
  };
  async function ctxAct(r, act) {
    if (act === "copy") { await copy(r.session); toast("已复制 " + r.session); }
    else if (act === "unread" || act === "read") await setMark(r.key, { unread: act === "unread" });
    else if (act === "rename") await renameRow(r);
    else if (act === "archive") await archiveRow(r);
    else if (act === "close") await closeTerm(r);
  }
  async function renameRow(r) {
    const name = (await ask({ title: "重命名", value: r.name, ok: "改名" }))?.replace(/\s+/g, " ").trim();
    if (!name || name === r.name) return;
    if (typesRename(r)) {
      // Typed into it: only while it waits for you, not mid-task or on a question.
      if (!["idle", "done"].includes(r.status)) throw new Error("它正在干活或在等你确认：等它停下来再改名");
      await api(paneURL(r.id, "text"), { body: { text: "/rename " + name, enter: true } });
    } else {
      await api(`/api/sessions/${sessionPath(r)}/rename`, { body: { kind: r.kind, name } });
      if (r.src !== "pane") await setMark(r.key, { title: name });
      if (r.src === "ext") loadExternal();
    }
    toast("已改名为「" + name + "」");
  }
  async function archiveRow(r) {
    const own = r.kind !== "claude", running = r.src === "pane";
    const text = [running ? "会先结束这个 agent（关闭它的工作区）。" : "",
      own ? `用 ${AGENT_LABEL[r.kind] || r.kind} 自己的归档，在它那边也会归档。` : "Claude Code 没有自己的归档：Collie 只把它从列表里藏起来，会话文件不动。"].filter(Boolean).join("\n");
    if (!await ask({ title: `归档「${r.name}」？`, text, ok: "归档", warn: true })) return;
    // Hidden first, so a closed agent whose archive then fails doesn't come
    // back as an ended session; the error says so.
    await setMark(r.key, { archived: true });
    if (running) {
      await api(`/api/workspaces/${encodeURIComponent(r.ws)}/close`, { body: {} });
      state = { ...state, agents: state.agents.filter((a) => a.workspace_id !== r.ws), shells: state.shells.filter((a) => a.workspace_id !== r.ws) };
      if (view === "pane" && current === r.id) goBack(true);
    }
    if (own) await api(`/api/sessions/${sessionPath(r)}/archive`, { body: { kind: r.kind, just_closed: running } });
    if (r.src === "ext") loadExternal();
    toast("已归档");
  }
  async function closeTerm(r) {
    if (!await ask({ title: `关闭终端「${r.term}」？`, text: "里面在跑的进程会被结束。", ok: "关闭", warn: true })) return;
    await api(paneURL(r.id, "close"), { body: {} });
    state = { ...state, shells: state.shells.filter((a) => a.pane_id !== r.id) };
    if (store.get("last") === "#/p/" + encodeURIComponent(r.id)) store.set("last", "#/collie");
    if (view === "pane" && current === r.id) goBack(true);
    renderSessions();
    toast("已关闭");
  }
  async function openExt(id, restore = null) {
    closeExt();
    extCur = id;
    if (id.startsWith("h:")) return openHist(id.slice(2), restore);
    const el = $("#x-thread");
    el._msgs = null;
    el._restore = restore && !restore.atBottom ? restore : null;
    primeThread(el, "x:" + id);
    resetEarlier(el, (c) => `/api/external/${encodeURIComponent(id)}/earlier?before=${c}`);
    extSoonN = 0;
    if (!ext.some((e) => e.id === id)) await loadExternal();
    renderExtHead();
    const mine = () => view === "ext" && extCur === id;
    // The stream drops when the hub restarts or the computer reconnects:
    // try again in a moment, keeping what is on screen.
    let tries = 0;
    const connect = () => {
      const es = extStream = openES(`/api/external/${encodeURIComponent(id)}/chat`);
      es.addEventListener("meta", (ev) => {
        tries = 0; extSoonN = 0;
        if (!ext.some((e) => e.id === id)) loadExternal();
        if (mine() && !JSON.parse(ev.data).structured) el.innerHTML = `<div class="thread-empty">还没有找到这个会话的记录。</div>`;
      });
      const show = (msgs) => { keepThread("x:" + id, msgs); if (mine()) { drawThread(withSending("x:" + id, msgs), el, "这个会话还没有消息。"); extWorking(); } };
      el._redraw = () => show(threadMsgs(el));
      es.addEventListener("patch", (ev) => mine() && show(applyPatch(el, JSON.parse(ev.data))));
      es.addEventListener("messages", (ev) => { el._msgs = null; el._older = null; show(JSON.parse(ev.data)); }); // an older computer
      es.addEventListener("gone", () => { closeExt(); if (mine()) renderExtHead(true); });
      es.onerror = () => {
        if (es.readyState !== EventSource.CLOSED || extStream !== es || !mine()) return;
        if (++tries > 20) { renderExtHead(true); return; }
        if (!el.querySelector(".msg")) el.innerHTML = `<div class="thread-empty">连不上那台电脑，正在重试…</div>`;
        setTimeout(() => { if (extStream === es && mine()) connect(); }, Math.min(1000 * tries, 5000));
      };
    };
    connect();
  }
  $("#x-takeover").addEventListener("click", async () => {
    if (extCur?.startsWith("h:")) return continueHist(extCur.slice(2));
    const e = ext.find((x) => x.id === extCur);
    if (!e) return;
    const app = e.app || "另一个终端";
    if (e.live && !(await ask({ title: `接管「${extName(e)}」？`, ok: "接管", text: `会先结束 ${app} 里的这个 ${KIND_LABEL[e.kind] || e.kind} 进程（相当于按 Ctrl-C），然后在 herdr 里用同一个会话继续。${app} 那边会回到命令行。` }))) return;
    const b = $("#x-takeover");
    b.disabled = true; b.textContent = "接管中…";
    try {
      const r = await api(`/api/external/${encodeURIComponent(e.id)}/takeover`, { body: {} });
      toast("已在 herdr 里接管");
      ext = ext.filter((x) => x.id !== e.id);
      renderExt();
      replaceHash("#/p/" + encodeURIComponent(r.pane_id));
      route();
    } catch (err) {
      toast(err.message, "attn");
      b.disabled = false;
    } finally { b.textContent = "在 herdr 里接管"; }
  });

  // ───────────────────────────── preferences (per device, in localStorage)
  const prefs = {
    theme: () => store.get("theme", "system"),
    text: () => store.get("text", "normal"),
    sendKey: () => store.get("sendKey", "enter"),
    notify: () => store.get("notify", true),
    fold: () => store.get("fold", true),
    readOnly: () => store.get("showReadOnly", false),
  };
  const darkMQ = matchMedia("(prefers-color-scheme: dark)");
  function applyLook() {
    const root = document.documentElement;
    const t = prefs.theme(), x = prefs.text();
    if (t === "system") delete root.dataset.theme; else root.dataset.theme = t;
    if (x === "normal") delete root.dataset.text; else root.dataset.text = x;
    const dark = t === "dark" || (t === "system" && darkMQ.matches);
    $$('meta[name="theme-color"]').forEach((m) => { m.removeAttribute("media"); m.content = dark ? "#0b0b0c" : "#ffffff"; });
    dispatchEvent(new Event("collie:theme"));
  }
  darkMQ.addEventListener("change", applyLook);
  addEventListener("storage", (ev) => {
    if (!ev.key || ev.key === "collie.theme" || ev.key === "collie.text") applyLook();
  });
  applyLook();

  function renderPrefs(restore = null) {
    const seg = (key, cur, opts) => `<div class="seg" data-pref="${key}">${opts.map(([v, label]) =>
      `<button type="button" data-v="${v}" class="${v === cur ? "on" : ""}">${label}</button>`).join("")}</div>`;
    const sw = (key, on) => `<button type="button" class="switch" role="switch" aria-checked="${on}" data-pref="${key}"></button>`;
    const row = (title, sub, ctl) => `<div class="pref"><div class="pl"><b>${title}</b>${sub ? `<small>${sub}</small>` : ""}</div>${ctl}</div>`;
    const perm = !("Notification" in window) ? "这个浏览器不支持通知（手机上通常要先添加到主屏幕）"
      : Notification.permission === "denied" ? "浏览器已拒绝通知，需要在浏览器的网站设置里打开"
      : "页面在后台时，agent 需要你确认或完成任务会弹通知";
    const mac = /Mac|iPhone|iPad/.test(navigator.platform);
    $("#prefs-body").innerHTML = `<div class="prefs">
      <div class="card"><div class="card-h">外观</div>
        ${row("主题", "跟随系统时，设备切到深色模式这里也跟着变", seg("theme", prefs.theme(), [["system", '<span class="theme-swatch system"></span>跟随系统'], ["light", '<span class="theme-swatch light"></span>浅色'], ["dark", '<span class="theme-swatch dark"></span>深色']]))}
        ${row("对话文字", "", seg("text", prefs.text(), [["small", "小"], ["normal", "标准"], ["large", "大"]]))}
      </div>
      <div class="card"><div class="card-h">对话</div>
        ${row("打开 agent 时显示", "对话：从会话记录整理的消息；屏幕：终端原样；日志：自动换行的滚动输出", seg("mode2", mode, [["chat", "对话"], ["screen", "屏幕"], ["log", "日志"]]))}
        ${row("发送消息", "另一种按法用来换行；Shift+Enter 总是换行", seg("sendKey", prefs.sendKey(), [["enter", "Enter"], ["mod", mac ? "⌘ Enter" : "Ctrl Enter"]]))}
        ${row("折叠连续的工具步骤", "超过 3 步时收成一行，只露出最后一步", sw("fold", prefs.fold()))}
      </div>
      <div class="card"><div class="card-h">侧边栏</div>
        ${row("显示其他终端里的只读会话", "在 iTerm、VS Code、tmux 或桌面版里开的会话，Collie 发不了消息、只能看的那些", sw("showReadOnly", prefs.readOnly()))}
      </div>
      <div class="card"><div class="card-h">通知</div>
        ${row("浏览器通知", esc(perm), sw("notify", prefs.notify() && (!("Notification" in window) || Notification.permission !== "denied")))}
      </div>
      <p class="panel-intro">Collie 本身的设置（它自己用哪个 agent、派活默认在哪台电脑、通知机器人）在 <a href="#/auto" data-auto-tab="settings">自动化 → 设置</a>；电脑和手机的连接在 <a href="#/devices">设备</a>。</p>
    </div>`;
    if (restore) restoreSimpleScroll($("#prefs-body"), restore);
  }
  $("#prefs-body").addEventListener("click", async (e) => {
    const link = e.target.closest("a[data-auto-tab]");
    if (link) autoTab = link.dataset.autoTab;
    const b = e.target.closest(".seg[data-pref] button, button.switch[data-pref]");
    if (!b) return;
    if (b.classList.contains("switch")) {
      const key = b.dataset.pref, on = b.getAttribute("aria-checked") !== "true";
      if (key === "notify" && on && "Notification" in window && Notification.permission !== "granted") {
        const p = await Notification.requestPermission();
        if (p !== "granted") { toast("浏览器没有允许通知", "attn"); renderPrefs(); return; }
      }
      store.set(key, on);
      if (key === "showReadOnly") renderSessions();
    } else {
      const key = b.closest("[data-pref]").dataset.pref, v = b.dataset.v;
      if (key === "mode2") setMode(v); else store.set(key, v);
      if (key === "theme" || key === "text") applyLook();
    }
    renderPrefs();
  });

  // ───────────────────────────── devices
  let devTimer;
  let devAddr = store.get("pairAddr", "");
  let pairing = { browser: null, machine: null }; // {code, expires}

  // The devices page by what you came for: connecting (pairing a phone or a
  // computer, the other hubs, the phones and browsers in), or the computers
  // and how they are doing.
  let devTab = store.get("devTab", "machines");
  bindTabs("#dev-tabs", "devices", "#devices-body", () => devTab, (t) => { devTab = t; store.set("devTab", t); }, (saved) => openDevices(saved));

  function openDevices(restore = null) {
    if (!(devTab in DEV_VIEW)) store.set("devTab", devTab = "connect"); // a tab merged into 连接
    markTab("#dev-tabs", devTab);
    clearInterval(devTimer);
    if (restore) restoreSimpleScroll($("#devices-body"), restore);
    loadDevices(restore);
    devTimer = setInterval(() => loadDevices(), 5000);
  }

  async function newCode(kind) {
    const r = await api("/api/pair/new", { body: { kind } });
    pairing[kind] = { code: r.code, expires: new Date(r.expires).getTime() };
    return r;
  }

  const left = (p) => { const s = Math.max(0, Math.round((p.expires - Date.now()) / 1000)); return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`; };

  // A newer release: the footer says so, the devices page says where and how.
  const INSTALL = "curl -fsSL https://taosama.github.io/collie/install.sh | sh -s --";
  // cmpVer orders dotted version numbers: <0, 0 or >0.
  const cmpVer = (x, y) => {
    const a = String(x).split(".").map(Number), b = String(y).split(".").map(Number);
    for (let i = 0; i < Math.max(a.length, b.length); i++) if ((a[i] || 0) !== (b[i] || 0)) return (a[i] || 0) - (b[i] || 0);
    return 0;
  };
  const RELEASE = /^v\d+\.\d+\.\d+$/;
  const isOld = (have, latest) => RELEASE.test(have || "") && RELEASE.test(latest || "") && cmpVer(have.slice(1), latest.slice(1)) < 0;
  async function checkUpdates() {
    try {
      const r = await api("/api/version");
      const n = $("#update-note");
      n.hidden = !r.outdated?.length;
      n.title = r.outdated?.length ? `Collie ${r.latest} 已发布，可以更新：${r.outdated.join("、")}（看「设备」页）` : "";
    } catch {}
  }
  checkUpdates();
  setInterval(checkUpdates, 30 * 60 * 1000);
  const verTag = (have, latest, cmd) => !have ? "" : isOld(have, latest)
    ? `<span class="tag attn" title="${esc("更新：" + cmd)}">${esc(have)} → ${esc(latest)}</span>` : `<span>${esc(have)}</span>`;
  const updateRow = (have, latest, cmd, note, id, name) => isOld(have, latest)
    ? `<div class="update-cmd"><p class="pair-hint">有新版本 ${esc(latest)}。<button class="btn small primary" data-update="${esc(id)}" data-name="${esc(name)}">更新到 ${esc(latest)}</button></p><p class="pair-hint muted">点了没反应的话，在这台电脑上运行${note ? "（" + esc(note) + "）" : ""}：</p><div class="code"><span>${esc(cmd)}</span><button class="icon-btn" data-copy="${esc(cmd)}" title="复制"><svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg></button></div></div>` : "";

  // ───────────────────────────── computer cards
  // Each computer's CPU, memory, network, tokens and disk over the last hour,
  // as its collie samples them (GET /api/sysmon, routed to the computer).
  // Only the samples after the last one held are asked for again.
  const SYS_SPAN = 3600; // seconds a card draws
  const SYS_ASK_MS = 8000; // a computer slower than this keeps its last report
  const sysm = new Map(); // machine id ("" for the hub's own) → { r, samples, key, html }
  const mcOpen = new Set(); // cards with their details open
  let sysmAt = 0, lastDevices = null;
  const MACHINE_ICON = '<svg viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/></svg>';
  const num = (v, d) => (v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(Math.min(d, 1)) : v.toFixed(d));
  const UNITS = ["B", "KB", "MB", "GB", "TB", "PB"];
  const unitOf = (n) => Math.max(0, Math.min(UNITS.length - 1, Math.floor(Math.log(Math.max(n, 1)) / Math.log(1024))));
  const inUnit = (n, u) => num(n / 1024 ** u, 2);
  const bytesText = (n) => n == null ? "—" : `${inUnit(n, unitOf(n))} ${UNITS[unitOf(n)]}`;
  // "8.48 / 124 GB"; units apart when they differ ("757 GB / 5.04 TB").
  const ofText = (used, total) => {
    if (!total) return "";
    const u = unitOf(total);
    return unitOf(used) === u ? `${inUnit(used, u)} / ${inUnit(total, u)} ${UNITS[u]}` : `${bytesText(used)} / ${bytesText(total)}`;
  };
  const rateText = (n) => n == null ? "—" : bytesText(n) + "/s";
  const pctText = (v) => v == null ? "—" : v.toFixed(1) + "%";
  const tokText = (n) => n < 1e3 ? String(Math.round(n)) : n < 1e6 ? num(n / 1e3, 1) + "K" : n < 1e9 ? num(n / 1e6, 1) + "M" : num(n / 1e9, 2) + "B";
  const level = (v, warn, bad) => v == null ? "" : v >= bad ? "bad" : v >= warn ? "warn" : "";

  // A line per series over the last hour; a gap in the samples breaks it.
  let sparkSeq = 0;
  function spark(points, series, top, end, every) {
    const W = 160, H = 40;
    const runs = series.map((sr) => {
      const out = []; let run = [], prev = null;
      for (const p of points) {
        const v = sr.read(p);
        if (v == null || (prev != null && p.t - prev > every * 2.5)) { if (run.length) out.push(run); run = []; }
        if (v != null) run.push({ t: p.t, v });
        prev = v == null ? null : p.t;
      }
      if (run.length) out.push(run);
      return out.filter((r) => r.length > 1);
    });
    if (!runs.some((r) => r.length)) return `<div class="mc-wait">采样中…</div>`;
    const max = Math.max(top, ...runs.flat(2).map((p) => p.v));
    const x = (t) => (((t - (end - SYS_SPAN)) / SYS_SPAN) * W).toFixed(1);
    const y = (v) => (H - 2 - (Math.max(0, v) / max) * (H - 4)).toFixed(1);
    const id = "mcf" + ++sparkSeq, wash = series.length === 1;
    const defs = wash ? `<defs><linearGradient id="${id}" x1="0" x2="0" y1="0" y2="1"><stop offset="0" stop-color="${series[0].color}" stop-opacity=".14"/><stop offset="1" stop-color="${series[0].color}" stop-opacity="0"/></linearGradient></defs>` : "";
    const body = runs.map((rs, i) => rs.map((r) => {
      const line = r.map((p) => `${x(p.t)},${y(p.v)}`).join(" ");
      return (wash ? `<polygon points="${x(r[0].t)},${H} ${line} ${x(r[r.length - 1].t)},${H}" fill="url(#${id})"/>` : "")
        + `<polyline points="${line}" fill="none" stroke="${series[i].color}"/>`;
    }).join("")).join("");
    return `<svg class="mc-spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" aria-hidden="true">${defs}${body}</svg>`;
  }

  // Asked at most every 9 s (each ask ends within SYS_ASK_MS, so they never
  // pile up); a computer that doesn't answer keeps what it said last, and one
  // whose collie is too old to say anything shows no figures.
  async function pollSysmon(ids) {
    sysmAt = Date.now();
    for (const id of sysm.keys()) if (!ids.includes(id)) sysm.delete(id);
    await Promise.all(ids.map(async (id) => {
      const cur = sysm.get(id) || { samples: [] };
      sysm.set(id, cur);
      try {
        const r = await api(`/api/sysmon?machine=${encodeURIComponent(id)}&since=${cur.samples.at(-1)?.t || 0}`, { signal: AbortSignal.timeout(SYS_ASK_MS) });
        const end = r.samples.at(-1)?.t || cur.samples.at(-1)?.t || 0;
        cur.samples = [...cur.samples, ...r.samples].filter((p) => p.t > end - SYS_SPAN);
        cur.r = r;
      } catch (err) {
        if (err.name !== "TimeoutError") cur.r = null;
      }
    }));
    const box = $("#dev-machines");
    if (box && lastDevices && view === "devices") box.innerHTML = machineCards(lastDevices);
  }

  // Drawn again only when a new sample or minute of tokens came.
  function metricsHTML(m) {
    const st = sysm.get(m.id), r = st?.r;
    if (!r) return "";
    const key = `${st.samples.at(-1)?.t}:${r.tokens?.minute}:${r.tokens?.today}`;
    if (st.key !== key) st.key = key, st.html = drawMetrics(st, r);
    return st.html;
  }
  function drawMetrics(st, r) {
    const pts = st.samples, last = pts.at(-1) || {}, end = last.t || Date.now() / 1000, every = r.every || 10;
    const mem = last.mem_total ? (last.mem_used / last.mem_total) * 100 : null;
    const disk = last.disk_total ? (last.disk_used / last.disk_total) * 100 : null;
    const memPct = (p) => (p.mem_total ? (p.mem_used / p.mem_total) * 100 : null);
    const tk = r.tokens || { recent: [] };
    const tokPts = (tk.recent || []).map((v, i, a) => ({ t: tk.minute - (a.length - 1 - i) * 60, v }));
    const cell = (label, value, detail, chart, cls = "") => `<div class="mc-cell"><div class="mc-l">${label}</div>
      <div class="mc-v ${cls}">${value}</div><div class="mc-d">${detail}</div>${chart}</div>`;
    const disks = (r.disks || []).map((d) => `${d.mount} ${ofText(d.used, d.total)}`).join("\n");
    return `<div class="mc-grid">
      ${cell("CPU", pctText(last.cpu), last.load == null ? "" : "负载 " + num(last.load, 2), spark(pts, [{ color: "var(--c-cpu)", read: (p) => p.cpu }], 100, end, every), level(last.cpu, 75, 90))}
      ${cell("内存", pctText(mem), ofText(last.mem_used, last.mem_total), spark(pts, [{ color: "var(--c-mem)", read: memPct }], 100, end, every), level(mem, 80, 90))}
      ${cell("网络", `<span class="c-rx">↓</span> ${rateText(last.rx)}`, `<span class="c-tx">↑</span> ${rateText(last.tx)}`,
        spark(pts, [{ color: "var(--c-rx)", read: (p) => p.rx }, { color: "var(--c-tx)", read: (p) => p.tx }], 1, end, every))}
      ${cell("Token", `${tokText(tk.per_minute || 0)}/分`, `今天 ${tokText(tk.today || 0)}`, spark(tokPts, [{ color: "var(--c-cpu)", read: (p) => p.v }], 1, tk.minute + 60, 60))}
    </div>
    ${disk == null ? "" : `<div class="mc-disk" title="${esc(disks)}"><span class="mc-l">磁盘</span>
      <span class="mc-bar"><i class="${level(disk, 80, 90)}" style="width:${disk.toFixed(1)}%"></i></span>
      <span class="mc-dv ${level(disk, 80, 90)}">${pctText(disk)}</span><span class="mc-d">${ofText(last.disk_used, last.disk_total)}</span></div>`}`;
  }

  // What a computer is and has: the details under its card.
  function detailsHTML(m, latest) {
    const r = sysm.get(m.id)?.r, sys = r?.system || {}, ver = r?.versions || {};
    const row = (k, v) => v ? `<div class="mc-kv"><span>${k}</span><span>${v}</span></div>` : "";
    const up = sys.boot ? Math.floor((Date.now() / 1000 - sys.boot) / 86400) : null;
    const agents = AGENT_KINDS.map(([k, label]) => !Array.isArray(m.agents) ? "" : m.agents.includes(k)
      ? `<span>${esc(label)} ${esc(ver[k] || "")}</span>` : `<span class="tag attn" title="找不到 ${k} 命令：装一个，再重连这台电脑">没装 ${esc(label)}</span>`).join("");
    const cmd = INSTALL + (m.hub ? " hub" : " node");
    return `<div class="mc-more">
      ${row("系统", esc([sys.os || m.os, sys.kernel].filter(Boolean).join(" · ")))}
      ${row("型号", esc(sys.model || ""))}
      ${row("CPU", esc([sys.cpu, sys.cores ? sys.cores + " 核" : ""].filter(Boolean).join(" · ")))}
      ${row("开机", up == null ? "" : `${when(sys.boot * 1000)}（已运行 ${up} 天）`)}
      ${row("磁盘", (r?.disks || []).map((d) => `<span class="mono">${esc(d.mount)}</span> ${ofText(d.used, d.total)}`).join("<br>"))}
      ${row("Agent", `<span class="mc-tags">${agents}</span>`)}
      ${row("herdr", m.online ? esc(ver.herdr || "没检测到") : "")}
      ${row("Collie", `<span class="mc-tags">${verTag(m.version, latest, cmd)}</span>`)}
      ${row("目录", m.home ? `<span class="mono">${esc(tilde(m.home))}</span>` : "")}
      ${updateRow(m.version, latest, cmd, m.hub ? "装 hub 时加过 --url 等参数的话照样带上" : "", m.id, m.name)}
      ${m.hub ? "" : `<div class="mc-acts"><button class="btn ghost small" data-del-dev="${esc(m.id)}" data-name="${esc(m.name)}">移除这台电脑</button></div>`}
    </div>`;
  }

  function machineCard(m, latest) {
    const r = sysm.get(m.id)?.r, sys = r?.system || {}, ver = r?.versions || {};
    const sub = [sys.os || m.os, sys.cpu].filter(Boolean).join(" · ");
    const mine = state.agents.filter((a) => (a.machine || "") === m.id && !isLocalRouter(a));
    const busy = mine.filter((a) => (RANK[a.status] ?? 3) <= 1).length;
    const said = !m.online ? `离线 · 上次 ${when(m.last_seen)}` : mine.length ? `${mine.length} 个 Agent${busy ? `，${busy} 个在工作` : ""}` : "没有 Agent";
    const vers = ["claude", "herdr"].filter((k) => ver[k]).map((k) => `<span>${k} ${esc(ver[k])}</span>`).join("");
    const open = mcOpen.has(m.id);
    return `<section class="mc ${m.online ? "" : "off"}">
      <header class="mc-h"><span class="mc-ic">${MACHINE_ICON}</span><div class="mc-t">
        <div class="mc-n"><b>${esc(m.name)}</b>${m.hub ? '<span class="tag">hub</span>' : ""}<span class="mc-on"><i></i>${m.online ? "在线" : "离线"}</span>${isOld(m.version, latest) ? '<span class="tag attn">可更新</span>' : ""}</div>
        <p class="mc-sub" title="${esc(sub)}">${esc(sub)}</p></div></header>
      ${m.online ? metricsHTML(m) : ""}
      ${open ? detailsHTML(m, latest) : ""}
      <footer class="mc-f"><span>${said}</span><span class="mc-ver">${vers}</span>
        <button type="button" class="mc-tg" data-mc="${esc(m.id)}" aria-expanded="${open}">${open ? "收起" : "详情"}<svg viewBox="0 0 24 24"><path d="M9 6l6 6-6 6"/></svg></button></footer>
    </section>`;
  }

  // The hub's own computer first, then the paired ones.
  function machineCards(r) {
    for (const id of mcOpen) if (id && !r.devices.some((d) => d.id === id)) mcOpen.delete(id);
    // The computers by name, the hub after them.
    const list = [...r.devices.filter((d) => d.kind === "machine").sort((a, b) => a.name.localeCompare(b.name)),
      { id: "", name: r.hub.name, hub: true, online: true, version: r.hub.version, agents: r.hub.agents, home: r.hub.home }];
    return list.map((m) => machineCard(m, r.latest)).join("");
  }

  // What each tab shows, from /api/devices (the hubs tab needs none: other
  // hubs are this browser's own list).
  const DEV_VIEW = {
    machines: (r) => `<div class="mc-list" id="dev-machines">${machineCards(r)}</div>`,
    connect: (r) => devPairing(r) + devHubs() + devBrowsers(r),
  };
  function devBrowsers(r) {
      const browsers = r.devices.filter((d) => d.kind !== "machine");
      return `
      <div class="card"><div class="card-h">手机和浏览器</div><ul class="rows">
        ${browsers.length ? browsers.map((d) => `<li class="row dev-row">
          <span class="dot unknown"></span>
          <div><div class="meta"><b>${esc(d.name)}</b>${d.current ? '<span class="tag">当前</span>' : ""}<span>最近 ${when(d.last_seen)}</span></div></div>
          <button class="btn ghost small" data-del-dev="${esc(d.id)}" data-name="${esc(d.name)}">移除</button></li>`).join("")
        : `<li class="empty-row">还没有配对过的手机。用访问令牌登录的浏览器不在这里列出。</li>`}
      </ul></div>`;
  }
  const devHubs = () => `
      <div class="card"><div class="card-h">其他 hub</div><p class="pair-hint">别的网络里的 Collie hub（云电脑、家里的电脑）。和这台 hub 不通也没关系：只要这个浏览器能打开它，它的会话就会出现在侧栏里。</p>
        <ul class="rows">${hubs.map((h) => { const on = remote.get(h.key)?.online; return `<li class="row dev-row">
          <span class="dot ${on ? "online" : "offline"}"></span>
          <div><div class="meta"><b>${esc(h.name)}</b><span>${on ? "在线" : "连不上"}</span></div><div class="text mono">${esc(h.url)}</div></div>
          <button class="btn ghost small" data-del-hub="${esc(h.key)}" data-name="${esc(h.name)}">移除</button></li>`; }).join("")}
        </ul>
        <form id="add-hub" class="hub-add">
          <input name="link" required autocomplete="off" autocorrect="off" autocapitalize="off" spellcheck="false" placeholder="粘贴那个 hub「设备 → 连接」里手机的链接">
          <input name="name" autocomplete="off" placeholder="叫它什么（可选）">
          <button class="btn primary small">添加</button>
        </form>
        <p class="pair-hint muted">那个 hub 也要是 v0.2.4 以后的版本。</p>
      </div>`;
  function devPairing(r) {
      const addrs = r.addresses || [];
      if (!addrs.includes(devAddr)) devAddr = addrs[0] || location.origin;
      const fresh = (k) => pairing[k] && pairing[k].expires - Date.now() > 15000;
      const pb = pairing.browser;
      const phoneURL = pb ? `${devAddr}/?pair=${pb.code}` : "";
      const pm = fresh("machine") ? pairing.machine : null;
      const nodeCmd = pm ? `collie node --hub ${devAddr} --code ${pm.code}` : "";
      const installCmd = pm ? `curl -fsSL https://taosama.github.io/collie/install.sh | sh -s -- node --hub ${devAddr} --code ${pm.code}` : "";
      const addrSel = addrs.length > 1 ? `<label class="addr-pick">用这个地址<select id="dev-addr" data-pick="compact">${addrs.map((a) => `<option ${a === devAddr ? "selected" : ""}>${esc(a)}</option>`).join("")}</select></label>` : "";
      return `
      <p class="panel-intro">Collie 适合放在一台常开的开发机上当 hub：手机、平板和浏览器只连 hub；你的 Mac 等电脑运行 <code>collie node</code> 主动连上来，hub 就能在上面开 agent、看对话、处理确认。不用开端口，也不需要 SSH。</p>

      <div class="pair-grid">
        <div class="card pair-card">
          <div class="card-h">连接手机或平板</div>
          <div class="qr-wrap">${pb ? `<img class="qr" alt="配对二维码" src="demo-qr.png?text=${encodeURIComponent(phoneURL)}">` : ""}</div>
          <p class="pair-hint">用相机扫一下，在浏览器里打开即可，之后可「添加到主屏幕」。<b>${pb ? left(pb) : ""}</b> 内有效，只能用一次。</p>
          <div class="code"><span>${esc(phoneURL)}</span><button class="icon-btn" data-copy="${esc(phoneURL)}" title="复制"><svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg></button></div>
          ${addrSel}
        </div>

        <div class="card pair-card">
          <div class="card-h">连接一台电脑</div>
          <p class="pair-hint">在那台电脑上装好 herdr、git 和 Go 1.25+，然后运行（从源码编译 Collie，并作为后台服务常驻）：</p>
          ${pm ? `<div class="code big"><span>${esc(installCmd)}</span><button class="icon-btn" data-copy="${esc(installCmd)}" title="复制"><svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg></button></div>
               <p class="pair-hint">已经装了 collie 的话：</p>
               <div class="code"><span>${esc(nodeCmd)}</span><button class="icon-btn" data-copy="${esc(nodeCmd)}" title="复制"><svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg></button></div>
               <p class="pair-hint"><b>${left(pm)}</b> 内有效，只能用一次。之后它会自动重连。</p>`
             : `<button class="btn primary small" id="new-node-code">生成连接命令</button>`}
          <p class="pair-hint muted">需要这台电脑能访问上面的地址（同一内网或 VPN）。</p>
        </div>
      </div>`;
  }

  let devSeq = 0;
  async function loadDevices(restore = null) {
    const seq = ++devSeq, tab = devTab, body = $("#devices-body");
    const keepTop = !restore && body.clientHeight > 0 ? { scrollTop: body.scrollTop, atBottom: body.scrollHeight - body.scrollTop - body.clientHeight <= AT_BOTTOM_PX } : null;
    let r = null;
    try {
      r = await api("/api/devices");
      const pb = pairing.browser;
      if (tab === "connect" && !(pb && pb.expires - Date.now() > 15000)) await newCode("browser").catch(() => {});
    } catch (err) {
      if (seq === devSeq) body.innerHTML = body._html = `<p class="panel-intro">${esc(err.message)}</p>`;
      return;
    }
    // An answer for a tab since left, or a page since closed, draws nothing.
    if (seq !== devSeq || view !== "devices") return;
    if (r) lastDevices = r;
    if (tab === "machines" && Date.now() - sysmAt > 9000) pollSysmon(["", ...r.devices.filter((d) => d.kind === "machine" && d.online).map((d) => d.id)]);
    const keepOpen = (pickOpen && pickOpen.id === "dev-addr") || document.activeElement?.id === "dev-addr" || document.activeElement?.closest?.("#add-hub");
    if (keepOpen) return; // don't redraw under an open select or a half-typed form
    const typed = [...document.querySelectorAll("#add-hub input")].map((i) => i.value);
    const html = DEV_VIEW[tab](r);
    if (body._html !== html) body.innerHTML = body._html = html;
    document.querySelectorAll("#add-hub input").forEach((i, n) => { i.value = typed[n] || ""; });
    if (restore) restoreSimpleScroll(body, restore);
    else if (keepTop && !keepTop.atBottom) { markProgrammaticScroll(); body.scrollTop = keepTop.scrollTop; }
  }

  $("#devices-body").addEventListener("change", (e) => {
    if (e.target.id === "dev-addr") { devAddr = e.target.value; store.set("pairAddr", devAddr); e.target.blur(); loadDevices(); }
  });
  // Adding another hub: its phone link carries a one-time pairing code; trade
  // it for a token of its own (the cookie that link would set can't be used
  // from this page) and keep the token here, for every browser of this hub.
  $("#devices-body").addEventListener("submit", async (e) => {
    if (e.target.id !== "add-hub") return;
    e.preventDefault();
    const f = e.target, btn = f.querySelector("button");
    let u;
    try { u = new URL(f.link.value.trim()); } catch { toast("链接不对：要以 http:// 或 https:// 开头", "attn"); return; }
    const code = u.searchParams.get("pair");
    if (!code) { toast("链接里没有配对码（?pair=…），在那个 hub 的「设备 → 连接」里复制手机的链接", "attn"); return; }
    const url = (u.origin + u.pathname).replace(/\/+$/, "");
    if (url === location.origin) { toast("这就是当前这个 hub", "attn"); return; }
    if (location.protocol === "https:" && u.protocol === "http:") { toast("这个页面是 https，浏览器不让它连 http 的 hub", "attn"); return; }
    btn.disabled = true;
    try {
      let res;
      try {
        res = await fetch(url + "/api/pair", { method: "POST", headers: { "Content-Type": "application/json" }, credentials: "omit",
          body: JSON.stringify({ code, kind: "browser", name: "hub " + location.host, want_token: true }) });
      } catch { throw new Error("这个浏览器连不上 " + url + "（或者它还是旧版本，不允许跨站访问）"); }
      const d = await res.json().catch(() => ({}));
      if (!res.ok || !d.token) throw new Error(d.error || (res.ok ? "那个 hub 是旧版本，先升级到 v0.2.4" : res.statusText));
      const key = "h" + Math.random().toString(36).slice(2, 8);
      const name = f.name.value.trim().slice(0, 30) || u.hostname.split(".")[0];
      const next = [...hubs, { key, name, url, token: d.token }];
      hubs = (await api("/api/hubs", { body: { hubs: next } })).hubs;
      f.reset();
      toast(`已添加 ${name}`);
      await loadHubs();
      setTimeout(loadExternal, 1500);
      loadDevices();
    } catch (err) { toast(err.message, "attn"); }
    finally { btn.disabled = false; }
  });
  $("#devices-body").addEventListener("click", async (e) => {
    const b = e.target.closest("button");
    if (!b) return;
    try {
      if (b.dataset.copy) { await copy(b.dataset.copy); toast("已复制"); }
      else if (b.dataset.mc !== undefined) {
        const id = b.dataset.mc;
        if (!mcOpen.delete(id)) mcOpen.add(id);
        if (lastDevices) $("#dev-machines").innerHTML = machineCards(lastDevices);
      } else if (b.id === "new-node-code") { await newCode("machine"); loadDevices(); }
      else if (b.dataset.update !== undefined) {
        const id = b.dataset.update;
        if (!(await ask({ title: `更新「${b.dataset.name}」上的 Collie？`, ok: "更新", text: `它会下载新版并重启，大约十几秒连不上${id ? "" : "（这是 hub，页面会短暂断开）"}；里面的 agent 不受影响。` }))) return;
        b.disabled = true; b.textContent = "更新中…";
        const r = await api("/api/update?" + new URLSearchParams(id ? { machine: id } : {}), { body: {} });
        toast(r.latest ? "已经是最新版" : `已更新到 ${r.version}，正在重启`);
        setTimeout(() => { loadDevices(); checkUpdates(); }, 12000);
      }
      else if (b.dataset.delHub) {
        if (!(await ask({ title: `不再显示「${b.dataset.name}」的会话？`, text: "那个 hub 本身不受影响。", ok: "不再显示" }))) return;
        hubs = (await api("/api/hubs", { body: { hubs: hubs.filter((h) => h.key !== b.dataset.delHub) } })).hubs;
        await loadHubs(); loadExternal(); loadHistory(); loadDevices();
      }
      else if (b.dataset.delDev) {
        if (!(await ask({ title: `移除「${b.dataset.name}」？`, text: "它需要重新配对才能再连进来。", ok: "移除", warn: true }))) return;
        await api(`/api/devices/${encodeURIComponent(b.dataset.delDev)}/delete`, { body: {} });
        loadDevices();
      }
    } catch (err) { toast(err.message, "attn"); }
  });

  // Clipboard API needs a secure context; plain-http LAN addresses fall back.
  async function copy(text) {
    if (!await copyText(text)) throw new Error("复制失败");
  }

  // ───────────────────────────── one agent: header
  // CTX_WARN_PCT: a session this full is close to compacting.
  const CTX_WARN_PCT = 80;
  function renderHead() {
    const v = find(current);
    if (!v) return;
    markSeen(v);
    $("#d-name").textContent = isLocalRouter(v) ? "Collie 的终端" : v.is_agent ? title(v) : termName(v);
    $("#d-name").title = v.is_agent && v.name && !isLocalRouter(v) ? `名字：${v.name}` : "";
    $("#d-kind").textContent = v.is_agent ? (KIND_LABEL[v.kind] || v.kind) : "";
    const role = isWorker(v.name) ? "Collie 派出" : "";
    $("#d-worker").hidden = !role;
    $("#d-worker").textContent = role;
    const st = $("#d-status");
    st.className = "status " + v.status;
    st.innerHTML = `<span class="dot ${esc(v.status)}"></span>${esc(STATUS_LABEL[v.status] || v.status)}`;
    // With more than one computer, say which one it runs on; its own home
    // folder shortens to ~ (big Linux boxes keep homes outside /home).
    const ms = state.machines || [];
    const home = ms.find((m) => m.id === (v.machine || ""))?.home;
    const cwd = home && (v.cwd || "").startsWith(home) ? "~" + v.cwd.slice(home.length) : tilde(v.cwd);
    $("#d-cwd").textContent = (ms.length > 1 ? machineName(v.machine) + " · " : "") + cwd;
    const u = v.context, pct = u?.window ? Math.floor(u.used * 100 / u.window) : 0;
    $("#d-ctx").textContent = pct ? ctxText(pct, u.used, u.window) : "";
    $("#d-ctx").classList.toggle("warn", pct >= CTX_WARN_PCT);
    renderSessionChips(v);
    // The pane may have shown up before the page knew what it is (a fresh
    // terminal): once it does, switch to the view that fits.
    if (view === "pane" && $("#viewer").dataset.mode && $("#viewer").dataset.mode !== curMode()) { closeStream(); resetChat(); resetScreen(); openStream(); }
    $("#input").placeholder = isLocalRouter(v) ? "直接在 Collie 的终端里输入…" : v.is_agent ? `给 ${KIND_LABEL[v.kind] || v.kind} 发消息…` : "输入命令…";
  }

  // Every agent session shows its agent, model and thinking level under the
  // input. The agent is fixed; Codex's and Antigravity's model and level
  // change in place, by their own slash commands. Claude Code's would also
  // become the default of every new session, so it only shows them. What a
  // session was started with is known when Collie started it; otherwise the
  // agent's configured default stands in.
  const CODEX_INLINE_MODEL = "0.164"; // codex's /model takes a model and --reasoning from here on
  const SWITCH_OK = new Set(["idle", "done", "unknown"]); // a command typed mid-turn would be read as a message
  const VERSIONS_KEEP_MS = 10 * 60e3;
  const agentVers = new Map(); // machine → { t, p: its agents' versions }
  function agentVersions(machine) {
    const have = sysm.get(machine)?.r?.versions;
    if (have) return have;
    const at = agentVers.get(machine);
    if (at && Date.now() - at.t < VERSIONS_KEEP_MS) return at.p;
    const p = api(`/api/sysmon?machine=${encodeURIComponent(machine)}&since=${Math.ceil(Date.now() / 1000)}`)
      .then((r) => r.versions || {}, () => { agentVers.delete(machine); return {}; });
    agentVers.set(machine, { t: Date.now(), p });
    return p;
  }
  // sess: what the chips show and for which pane; fixed says why they can't
  // change whatever the agent is doing.
  let sess = null, sessSeq = 0, sessBusy = false;
  async function renderSessionChips(v) {
    const kSel = $("#p-kind"), aSel = $("#p-alias"), mSel = $("#p-model"), eSel = $("#p-effort");
    const w = isWorker(v.name) ? sup.workers.find((x) => x.name === v.name && !x.ended) : null;
    const run = v.run || w?.run || ["", ""];
    const kind = shortKind(v.kind), machine = v.machine || "", remote = !!hubOf(v.pane_id);
    const show = v.is_agent && !isLocalRouter(v) && AGENT_KINDS.some(([k]) => k === kind);
    const alias = show ? w?.alias || "" : ""; // a task Collie started through an alias
    const key = show ? [v.pane_id, kind, machine, alias, ...run].join("|") : "";
    if (sess?.key === key) return lockSession();
    const seq = ++sessSeq;
    sess = { key };
    kSel.hidden = mSel.hidden = eSel.hidden = !show;
    aSel.hidden = !alias;
    aSel.innerHTML = alias ? `<span class="pv"><span class="pt">${esc(alias)}</span></span>` : "";
    mSel.disabled = eSel.disabled = true;
    for (const s of [mSel, eSel]) syncPick(s);
    if (!show) return;
    kSel.innerHTML = `<span class="pv"><span class="pi">${GLYPHS[kind] || GLYPHS.agent}</span><span class="pt">${esc(KIND_LABEL[kind] || kind)}</span></span>`;
    const [info, vers] = await Promise.all([remote ? {} : modelsFor(kind, machine), kind === "codex" && !remote ? agentVersions(machine) : {}]);
    if (seq !== sessSeq) return;
    const own = !run[0], model = run[0] || info.default || "", effort = run[1] || info.default_effort || "";
    const list = [...(info.models || [])];
    if (model && !list.some((m) => m.id === model)) list.unshift({ id: model });
    const label = (m) => m.label && m.label.length <= 24 ? m.label : shortModel(m.id);
    mSel.innerHTML = (model ? "" : `<option value="" data-short="默认模型" disabled>agent 自己的设置</option>`)
      + list.map((m) => `<option value="${esc(m.id)}" data-short="${esc(label(m))}" data-sub="${esc([m.label && m.label !== m.id ? m.id : "", m.id === model && own ? "agent 自己的设置" : ""].filter(Boolean).join(" · "))}">${esc(m.label || m.id)}</option>`).join("");
    const effs = effortsOf(info, list.find((m) => m.id === model));
    const effOpt = (e, extra = "") => `<option value="${esc(e)}" data-short="思考 · ${esc(EFFORT_LABEL[e] || e || "默认")}" ${extra}>${esc(EFFORT_LABEL[e] || e || "agent 自己定")}</option>`;
    eSel.innerHTML = (effs.includes(effort) ? "" : effOpt(effort, effort ? "" : "disabled")) + effs.map((e) => effOpt(e, `data-sub="${esc(e)}"`)).join("");
    mSel.value = model; eSel.value = effort;
    eSel.hidden = !effs.length;
    sess = { key, pane: v.pane_id, kind, machine, model, effort: run[1], tip: own ? "agent 自己的设置" : shortModel(run[0]) + (run[1] ? "，思考 " + (EFFORT_LABEL[run[1]] || run[1]) : ""),
      fixed: kind === "claude" ? "Claude Code 在会话里换模型会顺带改掉以后所有新会话的默认，这里只显示"
        : remote ? "另一个 hub 上的会话，到那边去换"
        : !list.length ? "没读到这台电脑上这个 agent 的模型列表"
        : kind === "codex" && cmpVer(vers.codex || "0", CODEX_INLINE_MODEL) < 0 ? `Codex ${CODEX_INLINE_MODEL} 起才能在会话里直接换，这台是 ${vers.codex || "未知版本"}` : "" };
    lockSession();
  }
  // The chips change only while the agent rests and no switch is under way.
  function lockSession() {
    const mSel = $("#p-model"), eSel = $("#p-effort");
    if (!sess?.pane) return;
    const why = sess.fixed || (sessBusy ? "正在换…" : SWITCH_OK.has(find(sess.pane)?.status) ? "" : "agent 正在忙，停下来再换");
    const title = why || `换这个会话的模型（现在用的：${sess.tip}）`;
    if (mSel.disabled === !!why && mSel.title === title) return;
    mSel.disabled = eSel.disabled = !!why;
    mSel.title = title;
    eSel.title = why || "换这个会话的思考强度";
    for (const s of [mSel, eSel]) syncPick(s);
  }
  async function switchSession(change) {
    const s = sess;
    if (!s?.pane || sessBusy) return;
    const to = { model: change.model ?? s.model, effort: change.effort ?? s.effort };
    // A new model may not have the old level; its own default then.
    if (change.model && to.effort) {
      const info = await modelsFor(s.kind, s.machine);
      const effs = effortsOf(info, (info.models || []).find((m) => m.id === to.model));
      if (effs.length && !effs.includes(to.effort)) to.effort = effs.includes(info.default_effort) ? info.default_effort : effs[0];
    }
    sessBusy = true; lockSession();
    try {
      await api(paneURL(s.pane, "model"), { body: { kind: s.kind, ...to, from: [s.model, s.effort || ""] } });
      const v = find(s.pane);
      if (v) v.run = [to.model, to.effort];
      toast(`已在会话里切到 ${shortModel(to.model)}${to.effort ? " · 思考 " + (EFFORT_LABEL[to.effort] || to.effort) : ""}`);
    } catch (err) {
      toast(err.message, "attn");
      if (sess === s) sess = { key: "" }; // show what it was again
    } finally {
      sessBusy = false;
      const v = find(current);
      if (v) renderSessionChips(v);
    }
  }
  $("#p-model").addEventListener("change", (e) => switchSession({ model: e.target.value }));
  $("#p-effort").addEventListener("change", (e) => switchSession({ effort: e.target.value }));

  // ───────────────────────────── one agent: streams
  let mode = store.get("mode2", "chat");
  let stream = null;
  let term = null;
  let size = { cols: 0, rows: 0 };
  let prevScreenLines = null;
  let pendingScreen = null;
  let screenFresh = true;

  // A plain shell has no conversation to show, so it opens on the screen.
  const isShell = () => { const v = find(current); return !!v && !v.is_agent; };
  let shellMode = null; // a shell's own pick; never overwrites the agent default
  const curMode = () => isShell() ? (shellMode || (mode === "chat" ? "screen" : mode)) : mode;
  function applyMode() {
    const m = curMode();
    $("#viewer").dataset.mode = m;
    $("#detail").classList.toggle("chat-mode", m === "chat");
    $("#detail").classList.toggle("shell", isShell());
    if (m === "chat") hideJump();
    $$("#mode-seg button").forEach((b) => {
      b.classList.toggle("on", b.dataset.mode === m);
      if (b.dataset.mode === "chat") b.hidden = isShell();
    });
  }
  function setMode(m) {
    if (view === "pane" && isShell()) shellMode = m; else { mode = m; store.set("mode2", m); }
    applyMode();
    if (view === "pane" && current) { closeStream(); resetChat(); resetScreen(); openStream(); }
  }
  $$("#mode-seg button").forEach((b) => b.addEventListener("click", () => setMode(b.dataset.mode)));

  const screenNearBottom = () => {
    const el = $("#screen");
    return el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  };
  function hideJump() {
    const j = $("#jump");
    if (j.hidden) return;
    const hadFocus = document.activeElement === j;
    j.hidden = true;
    if (hadFocus) (curMode() === "screen" ? $("#screen") : $("#log"))?.focus({ preventScroll: true });
  }
  function resetScreen() {
    prevScreenLines = null;
    pendingScreen = null;
    screenFresh = true;
    size = { cols: 0, rows: 0 };
    hideJump();
    if (term) {
      term.reset();
      $("#screen").scrollTop = 0;
      $("#screen").scrollLeft = 0;
    }
  }

  function ensureTerm() {
    if (term || !window.Terminal) return;
    const css = getComputedStyle(document.documentElement);
    term = new Terminal({
      cols: 120, rows: 40, fontFamily: css.getPropertyValue("--mono"), fontSize: 13, lineHeight: 1.15,
      cursorBlink: false, scrollback: 0, allowTransparency: true, theme: termTheme(),
    });
    // Native #screen handles 2D scrolling: never translate wheel into Up/Down
    // arrow keys (\x1b[A/\x1b[B) or let xterm cancel horizontal touch pans.
    term.attachCustomWheelEventHandler(() => false);
    $("#screen").addEventListener("touchmove", (e) => e.stopPropagation(), { capture: true, passive: true });
    term.open($("#screen"));
    term.onData(typeIntoPane);
    term.onSelectionChange(() => {
      if (!term.hasSelection() && pendingScreen && curMode() === "screen") {
        const p = pendingScreen;
        pendingScreen = null;
        drawScreen(p);
      }
    });
    // The terminal is the input: tap or click it and type. (A drag that
    // selected text, or scrolled the screen sideways, isn't a tap.)
    $("#screen").addEventListener("click", () => { if (!term.hasSelection()) term.focus(); });
    term.textarea?.setAttribute("enterkeyhint", "enter");
    term.textarea?.addEventListener("focus", () => $("#viewer").classList.add("typing"));
    term.textarea?.addEventListener("blur", () => $("#viewer").classList.remove("typing"));
    matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => { term.options.theme = termTheme(); });
    addEventListener("collie:theme", () => { term.options.theme = termTheme(); });
  }
  function termTheme() {
    const fg = getComputedStyle(document.documentElement).getPropertyValue("--term-fg").trim();
    return { background: "rgba(0,0,0,0)", foreground: fg, cursor: fg, selectionBackground: "rgba(127,127,127,.3)" };
  }
  // The pane keeps its own width (herdr can't resize it for us), so a phone
  // can't fit 120 columns legibly: there it shows the screen at a readable
  // size and scrolls sideways, or, with 全览, the whole width at once.
  const narrowScreen = matchMedia("(max-width: 760px)");
  function fitTerm() {
    if (!term || !size.cols) return;
    const el = $("#screen"), was = screenNearBottom();
    const pad = parseFloat(getComputedStyle(el).paddingLeft) * 2;
    const fitW = Math.floor(((el.clientWidth - pad) / size.cols / 0.602) * 2) / 2;
    const overview = store.get("term-overview", false);
    const fs = narrowScreen.matches && !overview ? Math.min(14, Math.max(fitW, 11)) : Math.max(4.5, Math.min(14, fitW));
    if (term.options.fontSize !== fs) {
      term.options.fontSize = fs;
      if (was) requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; hideJump(); });
    }
    $("#term-fit").textContent = overview ? "放大" : "全览";
    $("#term-fit").hidden = !narrowScreen.matches || fitW >= 11;
  }
  addEventListener("resize", () => fitTerm());
  $("#term-fit").addEventListener("click", () => { store.set("term-overview", !store.get("term-overview", false)); fitTerm(); });
  $("#term-kbd").addEventListener("click", () => term?.focus());

  function openStream(restore = null) {
    const id = current;
    applyMode();
    const m = curMode();
    if (m === "chat") return openChat(id, restore);
    if (m === "screen") ensureTerm();
    let pendingRestore = restore && !restore.atBottom ? restore : null;
    stream = openES(paneURL(id, "stream") + "?mode=" + m);
    stream.addEventListener("frame", (e) => {
      if (id !== current) return;
      const f = JSON.parse(e.data);
      if (m === "screen") drawScreen(f, pendingRestore);
      else drawLog(f.text, pendingRestore);
      pendingRestore = null;
    });
    stream.addEventListener("gone", () => { $("#gone").hidden = false; closeStream(); });
  }
  function closeStream() { stream?.close(); stream = null; term?.blur(); }

  function drawScreen(f, restore = null) {
    if (!term) return;
    const el = $("#screen");
    const stick = restore ? !!restore.atBottom : (screenFresh || screenNearBottom());
    const raw = (f.text || "").replace(/\r\n/g, "\n").replace(/\r/g, "");
    const lines = raw ? raw.split("\n") : [""];
    if (!screenFresh && term.hasSelection()) {
      pendingScreen = f;
      return;
    }
    if (!stick && prevScreenLines) {
      const check = Math.min(6, prevScreenLines.length);
      const sameHead = lines.length >= prevScreenLines.length &&
        prevScreenLines.slice(0, check).every((l, i) => lines[i] === l);
      if (!sameHead) {
        pendingScreen = f;
        $("#jump").hidden = false;
        return;
      }
    }
    pendingScreen = null;
    const savedTop = restore && !restore.atBottom ? restore.scrollTop : el.scrollTop;
    const cols = f.cols || size.cols || 120;
    const paneRows = f.rows || size.rows || 40;
    const rows = Math.max(1, lines.length);
    const dimChanged = cols !== size.cols || paneRows !== size.rows;
    if (dimChanged) size = { cols, rows: paneRows };
    const full = !prevScreenLines || cols !== term.cols || rows < term.rows;
    if (cols !== term.cols || rows !== term.rows) {
      if (full) term.reset();
      term.resize(cols, rows);
    }
    if (dimChanged) fitTerm();
    let out = "\x1b[?25l\x1b[?7l";
    if (full) {
      for (let r = 0; r < rows; r++) out += `\x1b[${r + 1};1H\x1b[0m\x1b[2K${lines[r]}`;
    } else {
      for (let r = 0; r < rows; r++) {
        if (prevScreenLines[r] !== lines[r]) out += `\x1b[${r + 1};1H\x1b[0m\x1b[2K${lines[r]}`;
      }
    }
    prevScreenLines = lines;
    screenFresh = false;
    const finishScroll = () => {
      if (stick) {
        el.scrollTop = el.scrollHeight;
        hideJump();
      } else {
        el.scrollTop = savedTop;
        if (screenNearBottom()) hideJump(); else $("#jump").hidden = false;
      }
    };
    if (out === "\x1b[?25l\x1b[?7l") { finishScroll(); return; }
    term.write(out, () => {
      finishScroll();
      requestAnimationFrame(finishScroll);
    });
  }
  function cleanLog(text) {
    return text.split("\n").map((l) => l.replace(/\s+$/, ""))
      .map((l) => (/^\s*[─━═\-]{16,}\s*$/.test(l) ? "────────────────" : l))
      .join("\n").replace(/\n{3,}/g, "\n\n").trim() + "\n";
  }
  function drawLog(text, restore = null) {
    const el = $("#log");
    const atBottom = restore ? !!restore.atBottom : (el.scrollHeight - el.scrollTop - el.clientHeight < 40);
    el.textContent = cleanLog(text);
    if (restore && !restore.atBottom) {
      restoreSimpleScroll(el, restore);
      if (el.scrollHeight - el.scrollTop - el.clientHeight < 40) hideJump(); else $("#jump").hidden = false;
      return;
    }
    if (atBottom) { el.scrollTop = el.scrollHeight; hideJump(); } else $("#jump").hidden = false;
  }
  $("#screen").addEventListener("scroll", () => {
    if (curMode() !== "screen") return;
    if (screenNearBottom()) {
      hideJump();
      if (pendingScreen) { const p = pendingScreen; pendingScreen = null; drawScreen(p); }
    } else {
      $("#jump").hidden = false;
    }
  }, { passive: true });
  $("#log").addEventListener("scroll", () => {
    if (curMode() !== "log") return;
    const el = $("#log");
    if (el.scrollHeight - el.scrollTop - el.clientHeight < 40) hideJump(); else $("#jump").hidden = false;
  }, { passive: true });
  $("#jump").addEventListener("click", () => {
    if (curMode() === "screen") {
      const el = $("#screen");
      el.scrollTop = el.scrollHeight;
      hideJump();
      if (pendingScreen) { const p = pendingScreen; pendingScreen = null; drawScreen(p); }
    } else {
      const el = $("#log");
      el.scrollTop = el.scrollHeight;
      hideJump();
    }
  });

  // ───────────────────────────── one agent: conversation
  let showRaw = false;
  function resetChat() {
    $("#thread").innerHTML = "";
    $("#thread").classList.remove("asking");
    $("#live").hidden = true;
    $("#live").innerHTML = "";
    $("#thread")._sig = ""; $("#thread")._msgs = null; $("#thread")._blocks = null; $("#thread")._older = null; $("#thread")._restore = null; showRaw = false;
  }
  function openChat(id, restore = null) {
    stream = openES(paneURL(id, "chat"));
    const alive = () => id === current && curMode() === "chat";
    stream.addEventListener("reset", () => alive() && resetChat());
    const th = $("#thread");
    resetEarlier(th, (c) => paneURL(id, "earlier") + "?before=" + c);
    th._restore = restore && !restore.atBottom ? restore : null;
    th._redraw = () => alive() && drawThread(withSending("p:" + id, threadMsgs(th)));
    if (!th._msgs) primeThread(th, "p:" + id);
    const showPane = (msgs) => { keepThread("p:" + id, msgs); drawThread(withSending("p:" + id, msgs)); };
    stream.addEventListener("patch", (e) => alive() && showPane(applyPatch(th, JSON.parse(e.data))));
    stream.addEventListener("messages", (e) => alive() && drawThread(withSending("p:" + id, JSON.parse(e.data)))); // an older computer
    stream.addEventListener("log", (e) => alive() && drawFallback(JSON.parse(e.data).text));
    stream.addEventListener("live", (e) => alive() && drawLive(JSON.parse(e.data)));
    stream.addEventListener("gone", () => { $("#gone").hidden = false; closeStream(); });
  }
  // Threads on a .viewer.chatbg scroll the viewer around them.
  const scroller = (el) => el.closest(".viewer.chatbg") || el;
  const nearBottom = (el) => { const s = scroller(el); return s.scrollHeight - s.scrollTop - s.clientHeight < 80; };
  const stick = (el, was) => {
    if (!was) return;
    const s = scroller(el);
    s.scrollTop = s.scrollHeight;
    requestAnimationFrame(() => { s.scrollTop = s.scrollHeight; }); // after images and code lay out
  };

  // A stream sends {base, from, msgs}: the first (and every reconnect) has
  // from === base and carries the whole view; later ones replace the
  // messages from position `from` on, and `base` moves when old ones fall off.
  function applyPatch(el, p) {
    let list = el._msgs, base = el._base;
    if (!list || p.from === p.base || p.from < base || p.from > base + list.length) {
      list = []; base = p.from;
      el._older = null; // a fresh view: what was loaded before may not join it
    }
    list = list.slice(0, p.from - base).concat(p.msgs);
    if (p.base > base) {
      if (el._older) el._older = el._older.concat(list.slice(0, p.base - base));
      list = list.slice(p.base - base); base = p.base;
    }
    el._msgs = list; el._base = base; el._liveEarlier = p.earlier || 0;
    return threadMsgs(el);
  }
  // A thread's messages: the older ones loaded on request, then the live view.
  const threadMsgs = (el) => (el._older || []).concat(el._msgs || []);

  // What you sent shows at the end of its thread at once, dimmed, until the
  // agent's own record of it arrives (keyed "p:<pane>" or "x:<session>").
  // One whose record never matches (the agent rewrote it) goes once sent
  // and given time to show up.
  const SENDING_STALE_MS = 20000;
  const sendingByKey = new Map();
  const normMsg = (t) => String(t || "").replace(/\s+/g, " ").trim();
  const userCount = (msgs, t) => msgs.reduce((n, m) => n + (m.kind === "user" && normMsg(m.text) === t ? 1 : 0), 0);
  function sendingThread(key) {
    if (view === "pane" && key === "p:" + current && curMode() === "chat") return $("#thread");
    if (view === "ext" && key === "x:" + extCur) return $("#x-thread");
    return null;
  }
  function redrawSending(key, bottom) {
    const el = sendingThread(key);
    if (!el?._redraw) return;
    if (bottom) { el._restore = null; el._fresh = true; }
    el._redraw();
  }
  // The record it waits for is one more than those already there, and
  // than the same text still on its way.
  function setSendingText(mine, text) {
    const t = normMsg(text), el = sendingThread(mine.key);
    mine.text = text;
    mine.base = userCount(el ? threadMsgs(el) : threadCache.get(mine.key) || [], t)
      + (sendingByKey.get(mine.key) || []).filter((m) => m !== mine && normMsg(m.text) === t).length;
  }
  function dropSending(mine) {
    const left = (sendingByKey.get(mine.key) || []).filter((m) => m !== mine);
    if (left.length) sendingByKey.set(mine.key, left); else sendingByKey.delete(mine.key);
    redrawSending(mine.key);
  }
  // Shows a message in thread `key` at once, then posts its final text.
  async function trackSend(key, draft, ready, post) {
    const mine = { key, sent: 0 };
    setSendingText(mine, draft);
    sendingByKey.set(key, (sendingByKey.get(key) || []).concat(mine));
    redrawSending(key, true);
    try {
      const text = await ready;
      setSendingText(mine, text);
      await post(text);
    } catch (err) {
      dropSending(mine);
      throw err;
    }
    mine.sent = Date.now();
    setTimeout(() => redrawSending(key), SENDING_STALE_MS + 100);
  }
  function withSending(key, msgs) {
    const list = sendingByKey.get(key);
    if (!list) return msgs;
    const now = Date.now();
    const left = list.filter((m) => userCount(msgs, normMsg(m.text)) <= m.base && !(m.sent && now - m.sent > SENDING_STALE_MS));
    if (left.length) sendingByKey.set(key, left); else sendingByKey.delete(key);
    return left.length ? msgs.concat(left.map((m) => ({ kind: "user", text: m.text, sending: true }))) : msgs;
  }
  const earlierCursor = (el) => (el._older ? el._olderEarlier : el._liveEarlier) || 0;
  function resetEarlier(el, url) {
    el._older = null; el._olderEarlier = 0; el._liveEarlier = 0; el._earlierURL = url;
    el._loadMore = url && (() => { el._earlierFailed = false; if (earlierCursor(el) > 0) loadEarlier(el); });
  }

  // Scrolled to the top of a conversation: fetch the page before the oldest
  // message shown, keeping the reader's place.
  async function loadEarlier(el) {
    if (el._loadingEarlier || !el._earlierURL) return;
    const cursor = earlierCursor(el);
    if (!cursor) return;
    el._loadingEarlier = true;
    el._redraw?.();
    try {
      const r = await api(el._earlierURL(cursor));
      if (el._earlierURL && earlierCursor(el) === cursor) {
        el._older = (r.messages || []).concat(el._older || []);
        el._olderEarlier = r.earlier || 0;
      }
    } catch (err) {
      el._earlierFailed = true; // scrolling up tries again; nothing loops on its own
      toast(err.message, "attn");
    }
    el._loadingEarlier = false;
    const s = scroller(el), fromBottom = s.scrollHeight - s.scrollTop;
    el._redraw?.();
    s.scrollTop = s.scrollHeight - fromBottom;
  }
  // Any conversation scrolled to its top brings in the page before: each
  // thread says how (_loadMore), this one listener notices.
  document.addEventListener("scroll", (ev) => {
    if (performance.now() < programmaticScrollUntil) return;
    const s = ev.target;
    if (!(s instanceof Element) || s.scrollTop >= 40 || s.scrollHeight <= s.clientHeight + 100) return;
    const el = s._loadMore ? s : [...s.querySelectorAll(".thread")].find((t) => t._loadMore && (t.parentElement === s || scroller(t) === s));
    el?._loadMore();
  }, { capture: true, passive: true });

  // Opening a thread: what was on screen last time shows at once and the live
  // stream updates it; a thread never opened shows a skeleton of a
  // conversation, so nothing jumps when the messages land.
  const threadCache = new Map();
  const SKELETON = `<div class="thread-skel" aria-label="读取中">${["user", "a", "a short", "tools", "user short", "a", "a short"].map((c) => `<div class="sk ${c}"></div>`).join("")}</div>`;
  function primeThread(el, key) {
    el._sig = ""; el._blocks = null; el._fresh = true;
    const got = key && threadCache.get(key);
    if (got) { drawThread(got, el); el._fresh = true; } else el.innerHTML = SKELETON;
  }
  function keepThread(key, msgs) {
    if (!key || !msgs || !msgs.length) return;
    threadCache.delete(key);
    threadCache.set(key, msgs.slice(-120));
    while (threadCache.size > 20) threadCache.delete(threadCache.keys().next().value);
  }

  document.addEventListener("click", async (ev) => {
    const b = ev.target.closest("[data-act=ask-reveal]");
    if (!b || !extCur) return;
    b.disabled = true;
    try { await api(`/api/external/${encodeURIComponent(extCur)}/reveal`, { body: {} }); toast("已在电脑上的 Claude 桌面版打开这个会话"); }
    catch (err) { toast(err.message, "attn"); }
    finally { b.disabled = false; }
  });
  // An end-of-turn button answers by sending its label from this view's composer.
  document.addEventListener("click", (ev) => {
    const b = ev.target.closest("[data-act=ask-reply]");
    if (!b) return;
    const form = b.closest("section.view")?.querySelector("form.composer:not([hidden])");
    const input = form?.querySelector("textarea");
    if (!form || !input) { toast("这里没法发消息，去电脑上回答", "attn"); return; }
    input.value = b.dataset.text;
    form.requestSubmit();
  });
  function drawThread(msgs, el = $("#thread"), empty = "还没有对话。发一条消息开始吧。") {
    const rest = el._restore;
    // Just opened: start at the latest message, unless restoring a scroll position.
    const was = rest ? !!rest.atBottom : (el._fresh || nearBottom(el) || !el.childElementCount);
    if (msgs.length) el._fresh = false;
    const open = new Set($$("details.tool[open], details.toolrun[open]", el).map((d) => d.dataset.i));
    if (!msgs.length) { el.innerHTML = `<div class="thread-empty">${empty}</div>`; el._sig = ""; el._blocks = null; return; }
    const blocks = [];
    let html = "", i = 0;
    const cut = () => { blocks.push(html); html = ""; };
    while (i < msgs.length) {
      const m = msgs[i];
      if (m.kind === "tool") {
        // A run of tool calls reads as one quiet line; long runs fold away,
        // keeping only the latest step in view.
        const start = i;
        while (i < msgs.length && msgs[i].kind === "tool") i++;
        const run = msgs.slice(start, i);
        const row = (t, k) => {
          const cls = t.error ? "err" : (t.output ? "" : "pending");
          return `<details class="tool ${cls}" data-i="${k}" ${open.has(String(k)) ? "open" : ""}>`
            + `<summary><span class="tname">${esc(t.name)}</span><span class="targ">${esc(t.input)}</span></summary>`
            + (t.output ? `<pre>${esc(t.output)}</pre>` : "") + `</details>`;
        };
        if (run.length <= 3 || !prefs.fold()) {
          html += `<div class="tools">${run.map((t, k) => row(t, start + k)).join("")}</div>`;
          cut();
          continue;
        }
        const key = "run" + start;
        const names = [...new Set(run.map((t) => t.name))].slice(0, 4).join("、");
        const errs = run.filter((t) => t.error).length;
        html += `<div class="tools"><details class="toolrun" data-i="${key}" ${open.has(key) ? "open" : ""}>
          <summary><span class="n">${run.length} 个步骤</span><span class="targ">${esc(names)}${errs ? ` · <b class="errs">${errs} 个出错</b>` : ""}</span></summary>
          <div class="toolrun-body">${run.slice(0, -1).map((t, k) => row(t, start + k)).join("")}</div>
        </details>${row(run[run.length - 1], i - 1)}</div>`;
        cut();
        continue;
      }
      if (m.kind === "ask" && m.name === "recap") {
        html += `<div class="recap-note">${esc(m.input)}</div>`;
        cut();
        i++;
        continue;
      }
      if (m.kind === "ask") {
        // A question to you: AskUserQuestion is answered in the app it runs
        // in; the Claude app's end-of-turn buttons just send their label.
        const later = msgs.slice(i + 1).some((x) => x.kind === "user");
        const box = m.name === "wrap_up";
        const open = box ? !later : !m.output;
        const opts = (m.options || []).map((o) => box && open
          ? `<button type="button" class="btn small" data-act="ask-reply" data-text="${esc(o)}">${esc(o)}</button>`
          : `<span class="opt">${esc(o)}</span>`).join("");
        html += `<div class="ask-card ${open ? "open" : ""}">`
          + (m.input ? `<div class="recap">${esc(m.input)}</div>` : "")
          + `<div class="q"><span class="dot ${open ? "blocked" : "unknown"}"></span>${esc(m.text)}</div>`
          + (opts ? `<div class="opts">${opts}</div>` : "")
          + (m.output ? `<div class="answer">${esc(m.output)}</div>`
            : open && !box ? (el.id === "x-thread" && /^cd-/.test(extCur || "")
              ? `<div class="where"><button type="button" class="btn small" data-act="ask-reveal">去 Claude 桌面版回答</button><span>这个对话框只能在电脑上答</span></div>`
              : el.id === "thread" ? "" : `<div class="where">在电脑上的会话里回答</div>`) : "")
          + `</div>`;
        cut();
        i++;
        continue;
      }
      html += m.kind === "user" ? `<div class="msg user${m.sending ? " sending" : ""}">${userText(m.text)}</div>` : `<div class="msg assistant">${md(m.text)}</div>`;
      cut();
      i++;
    }
    if (el._loadingEarlier) blocks.unshift(`<div class="earlier-row">加载中…</div>`);
    const sig = blocks.join("");
    if (sig === el._sig) return;
    el._sig = sig;
    const old = el._blocks;
    if (old && el.children.length === old.length && el.lastElementChild === el._last) {
      // Keep what did not change (and its open/closed state); redraw from
      // the first block that did.
      let k = 0;
      while (k < old.length && k < blocks.length && old[k] === blocks[k]) k++;
      while (el.children.length > k) el.lastElementChild.remove();
      el.insertAdjacentHTML("beforeend", blocks.slice(k).join(""));
    } else {
      el.innerHTML = sig;
    }
    el._blocks = blocks; el._last = el.lastElementChild;
    if (rest && !rest.atBottom) {
      restoreSimpleScroll(scroller(el), rest);
      if (msgs.length && !el.querySelector(".thread-skel")) el._restore = null;
    } else {
      stick(el, was);
    }
    // Too short to scroll, with more before it: there is no top to scroll
    // to, so bring the earlier page in now.
    if (el._earlierURL && !el._loadingEarlier && !el._earlierFailed && earlierCursor(el) > 0) {
      const s = scroller(el);
      if (s.scrollHeight <= s.clientHeight + 100) setTimeout(() => loadEarlier(el));
    }
  }

  function drawFallback(text) {
    const el = $("#thread");
    const was = nearBottom(el) || !el.childElementCount;
    const v = find(current);
    const note = v && v.is_agent ? `${esc(KIND_LABEL[v.kind] || v.kind)} 还没有可读取的会话记录，先显示终端输出` : "普通终端，这里显示终端输出";
    el.innerHTML = `<div class="fallback-note">${note}</div><pre class="termcard">${esc(cleanLog(text))}</pre>`;
    stick(el, was);
  }

  let lastLive = null;
  function drawLive(l) {
    lastLive = l;
    const box = $("#live");
    const th = $("#thread");
    const was = nearBottom(th);
    th.classList.toggle("asking", l.status === "blocked" && !!l.approval);
    let html = "";
    if (l.status === "working" && l.tail) {
      html = `<div class="live-working"><span class="dot working"></span><pre>${esc(l.tail)}</pre><span class="meta"></span></div>`;
    } else if (l.status === "blocked" && l.approval) {
      const v = find(current);
      html = askHTML(l.approval, v ? title(v) : "agent");
    }
    // Live updates arrive every second (the agent's clock ticks); redrawing
    // an unchanged card would replay its entrance and make it jump.
    if (html !== box._html) {
      box._html = html;
      box.innerHTML = html;
    }
    // The counters tick every second: only their text changes.
    const meta = box.querySelector(".live-working .meta");
    if (meta && meta.textContent !== (l.meta || "")) meta.textContent = l.meta || "";
    box.hidden = !html;
    stick(th, was);
  }

  // Terminal dialogs arrive hard-wrapped at the terminal's width. Rebuild the
  // paragraphs so the card can use its own width, and find the real question.
  const CJK = /[　-鿿＀-￯]/;
  function readDialog(text) {
    const raw = (text || "").split("\n");
    const ind = (l) => l.match(/^ */)[0].length;
    const filled = raw.filter((l) => l.trim());
    const base = filled.length ? Math.min(...filled.map(ind)) : 0;
    const paras = [];
    let cur = [];
    for (const l of raw) {
      if (l.trim()) cur.push(l);
      else if (cur.length) { paras.push(cur); cur = []; }
    }
    if (cur.length) paras.push(cur);
    // A line that ends mid-word and is followed by a capitalised line is a real
    // line break ("This command requires approval" / "Do you want…"), not a wrap.
    const split = [];
    for (const p of paras) {
      let part = [p[0]];
      for (let k = 1; k < p.length; k++) {
        const prev = p[k - 1].trim(), next = p[k].trim();
        if (ind(p[k]) === ind(p[k - 1]) && /[A-Za-z0-9]$/.test(prev) && /^[A-Z]/.test(next)) { split.push(part); part = []; }
        part.push(p[k]);
      }
      split.push(part);
    }
    let blocks = split.map((p) => {
      const t0 = p[0].trim();
      // Indented deeper than the dialog's text, or a lone path: show as-is.
      if (p.every((l) => ind(l) > base) || (p.length === 1 && /^[~/.][^\s]*$/.test(t0))) {
        const cut = Math.min(...p.map(ind));
        return { code: true, text: p.map((l) => l.slice(cut)).join("\n") };
      }
      let t = "";
      for (const l of p.map((x) => x.trim())) t += !t ? l : (CJK.test(t.slice(-1)) && CJK.test(l[0]) ? "" : " ") + l;
      return { code: false, text: t };
    });
    // Terminal chrome that means nothing in a card.
    // Claude's question box heads itself with tabs ("☐ Color  ✔ Submit").
    blocks = blocks.filter((b) => !/^(←\s*)?[☐☒✔]\s/.test(b.text.trim()));
    blocks = blocks.filter((b, i) => b.code || !(/^tip:/i.test(b.text) ||
      (i === blocks.length - 1 && b.text.split(/\s+/).length <= 3 && !/[.:?？。：]/.test(b.text))));
    // The question is the last sentence ending in "?"; it becomes the title.
    for (let i = blocks.length - 1; i >= 0; i--) {
      const b = blocks[i];
      if (b.code) continue;
      const ms = [...b.text.matchAll(/[^.!?。！？]*[?？]/g)];
      if (!ms.length) continue;
      const m = ms[ms.length - 1];
      const rest = (b.text.slice(0, m.index) + b.text.slice(m.index + m[0].length)).trim();
      const out = blocks.slice();
      if (rest) out[i] = { code: false, text: rest }; else out.splice(i, 1);
      // "Quick safety check: Is this…" → the label adds nothing to a title.
      const q = m[0].trim().replace(/^[^:：?？]{1,24}[:：]\s*(?=\S)/, "");
      return { q, blocks: out };
    }
    const last = blocks.map((b) => !b.code).lastIndexOf(true);
    if (last < 0) return { q: "要继续吗？", blocks };
    return { q: blocks[last].text, blocks: blocks.filter((_, k) => k !== last) };
  }

  // A calm decision sheet: the question as the title, the dialog's text
  // reflowed under it, the choices as a list.
  function askHTML(a, who) {
    const { q, blocks } = readDialog(a.question);
    const choices = a.choices || [];
    const body = blocks.map((b) => b.code ? `<pre class="ctx-code">${esc(b.text)}</pre>` : `<p>${esc(b.text)}</p>`).join("");
    return `<div class="ask">
      <div class="ask-head"><span class="dot blocked"></span><span class="who">${esc(who)}</span><span>需要你确认</span><span class="spacer"></span>
        <button data-raw>${showRaw ? "收起终端" : "查看终端"}</button></div>
      <div class="ask-q"><div class="q">${esc(q)}</div>${body ? `<div class="ctx">${body}</div>` : ""}</div>
      ${showRaw ? `<pre class="ask-raw">${esc(a.screen)}</pre>` : ""}
      ${choices.length ? `<div class="choices">${choices.map((c, i) => `<button class="choice ${c.current ? "current" : ""}" data-choice="${i}">
          <span class="n">${esc(c.text || i + 1)}</span><span class="l">${esc(c.label)}${c.desc ? `<span class="d">${esc(c.desc)}</span>` : ""}</span></button>`).join("")}</div>
        <div class="ask-keys"><button data-k="esc">Esc 取消</button></div>`
      : `<div class="ask-keys"><button data-k="up">↑</button><button data-k="down">↓</button><button data-k="esc">Esc</button><button class="go" data-k="enter">确认 ↵</button></div>`}
    </div>`;
  }

  $("#live").addEventListener("mousedown", (e) => { if (e.target.closest(".ask button")) e.preventDefault(); });
  $("#live").addEventListener("click", (e) => {
    const b = e.target.closest("button");
    if (!b || !current) return;
    if (b.hasAttribute("data-raw")) { showRaw = !showRaw; if (lastLive) drawLive(lastLive); return; }
    const ask = b.closest(".ask");
    if (ask?.dataset.busy) return;
    let p;
    if (b.dataset.choice != null) {
      const c = lastLive?.approval?.choices?.[+b.dataset.choice];
      if (!c) return;
      if (c.keys?.length) p = sendKeys(c.keys);
      else if (c.text) p = sendText(c.text);
      else p = sendKeys(["enter"]);
    } else if (b.dataset.k) {
      p = sendKeys([b.dataset.k]);
    } else return;
    if (ask) {
      ask.dataset.busy = "1";
      ask.style.opacity = ".5";
      Promise.resolve(p).finally(() => setTimeout(() => { $("#live")._html = ""; }, 600));
    }
  });

  $("#kbd-toggle").addEventListener("click", () => {
    const on = $("#keys").classList.toggle("open");
    $("#kbd-toggle").classList.toggle("on", on);
  });

  // A file path in a message: ~/… or /…, ending in a name with an extension.
  // It runs on escaped text, so it never meets a raw quote or tag.
  const FILE_RE = /(^|[\s`(（：，、；>;])((?:~|\/)[\w.\-+@%~\/]*\/[\w.\-+@%~]+\.[A-Za-z0-9]{1,8})(?![\w\/])/g;

  // ───────────────────────────── offline syntax highlighter
  // Pure local single-pass lexical scanner: zero external dependencies, O(N)
  // non-backtracking rules, and every slice passes through esc() so output is
  // XSS-safe and preserves exact textContent when copying selected lines.
  const HL_MAX_BYTES = 200 * 1024, HL_MAX_LINES = 5000;
  const LANG_LABELS = {
    go: "Go", py: "Python", js: "JavaScript", ts: "TypeScript", jsx: "JSX", tsx: "TSX",
    sql: "SQL", json: "JSON", yaml: "YAML", sh: "Shell", html: "HTML", xml: "XML",
    css: "CSS", md: "Markdown", diff: "Diff", rust: "Rust", java: "Java",
    c: "C", cpp: "C++", toml: "TOML", dockerfile: "Dockerfile", makefile: "Makefile",
  };
  const EXT_LANG = {
    go: "go", py: "py", pyi: "py", pyw: "py",
    js: "js", mjs: "js", cjs: "js", ts: "ts", mts: "ts", cts: "ts", jsx: "jsx", tsx: "tsx",
    sql: "sql", json: "json", jsonl: "json", jsonc: "json", webmanifest: "json",
    yaml: "yaml", yml: "yaml", sh: "sh", bash: "sh", zsh: "sh",
    html: "html", htm: "html", vue: "html", svelte: "html",
    xml: "xml", svg: "xml", plist: "xml", xhtml: "xml",
    css: "css", scss: "css", less: "css", md: "md", markdown: "md", mdx: "md",
    diff: "diff", patch: "diff", rs: "rust", java: "java", kt: "java", kts: "java",
    c: "c", h: "c", cc: "cpp", cpp: "cpp", cxx: "cpp", hpp: "cpp", hh: "cpp", proto: "cpp",
    toml: "toml", ini: "toml", cfg: "toml", conf: "toml",
    dockerfile: "dockerfile", mk: "makefile",
  };
  const FENCE_LANG = {
    ...EXT_LANG,
    golang: "go", gomod: "go", python: "py", python3: "py",
    javascript: "js", node: "js", typescript: "ts",
    mysql: "sql", postgres: "sql", postgresql: "sql", sqlite: "sql", hive: "sql", sparksql: "sql", plsql: "sql",
    json5: "json", shell: "sh", console: "sh", env: "sh",
    rust: "rust", kotlin: "java", "c++": "cpp", protobuf: "cpp",
    docker: "dockerfile", makefile: "makefile", make: "makefile",
  };
  function detectCodeLang(raw, byPath = false) {
    if (!raw) return null;
    let id = "";
    if (byPath) {
      const name = String(raw).split("/").pop() || "", low = name.toLowerCase();
      if (low === "dockerfile" || low.startsWith("dockerfile.") || low === "containerfile") id = "dockerfile";
      else if (low === "makefile" || low === "gnumakefile" || low === "justfile") id = "makefile";
      else if (low === "go.mod" || low === "go.sum") id = "go";
      else if (/^\.(?:bashrc|bash_profile|zshrc|profile|env)(?:\..+)?$/.test(low) || low === ".gitignore" || low === ".dockerignore") id = "sh";
      else if (low.includes(".")) id = EXT_LANG[low.split(".").pop()] || "";
    } else {
      const tag = String(raw).trim().split(/[\s{:]/)[0].toLowerCase().replace(/^language-/, "");
      id = FENCE_LANG[tag] || "";
    }
    return id && LANG_LABELS[id] ? { id, label: LANG_LABELS[id] } : null;
  }
  const wset = (s) => new Set(s.trim().split(/\s+/));
  function wordCls(kw, ty, cn, { ci = false, pascal = true, calls = true } = {}) {
    const kS = wset(kw), tS = ty ? wset(ty) : null, cS = cn ? wset(cn) : null;
    return (tok, src, idx) => {
      const w = ci ? tok.toLowerCase() : tok;
      if (kS.has(w)) return "k";
      if (cS?.has(w)) return "n";
      if (tS?.has(w)) return "ty";
      if (calls) {
        let j = idx + tok.length;
        while (j < src.length && (src[j] === " " || src[j] === "\t")) j++;
        if (src[j] === "(") return "fn";
      }
      if (pascal && /^[A-Z][A-Za-z0-9_]*$/.test(tok)) return "ty";
      return "";
    };
  }
  function compileRules(rules, flags = "gm") {
    return { re: new RegExp(rules.map((r) => "(" + r.pat + ")").join("|"), flags), rules };
  }
  function scanCompiled(src, { re, rules }) {
    let out = "", pos = 0;
    for (const m of src.matchAll(re)) {
      if (m.index > pos) out += esc(src.slice(pos, m.index));
      const tok = m[0];
      if (!tok) continue;
      let g = 0;
      while (g < rules.length && m[g + 1] === undefined) g++;
      const r = rules[g];
      if (r?.html) {
        out += r.html(tok, src, m.index);
      } else {
        const cls = typeof r?.cls === "function" ? r.cls(tok, src, m.index) : r?.cls;
        out += cls ? `<span class="hl-${cls}">${esc(tok)}</span>` : esc(tok);
      }
      pos = m.index + tok.length;
    }
    if (pos < src.length) out += esc(src.slice(pos));
    return out;
  }
  const P_CL = "//[^\\n]*", P_CB = "\\/\\*[\\s\\S]*?(?:\\*\\/|$)", P_HASH = "#[^\\n]*";
  const P_DQ = '"(?:\\\\[\\s\\S]|[^"\\\\\\n])*"?', P_SQ = "'(?:\\\\[\\s\\S]|[^'\\\\\\n])*'?", P_TPL = "`(?:\\\\[\\s\\S]|[^`\\\\])*`?";
  const P_NUM = "-?\\b(?:0[xX][0-9a-fA-F_]+|0[bBoO][0-9_]+|\\d[\\d_]*(?:\\.[\\d_]+)?(?:[eE][+-]?\\d+)?i?)\\b";
  const P_ID = "[A-Za-z_$][\\w$]*";
  const leadHash = (t) => (t[0] === "#" ? `<span class="hl-c">${esc(t)}</span>` : esc(t[0]) + `<span class="hl-c">${esc(t.slice(1))}</span>`);
  const TAG_SUB = compileRules([
    { pat: "^<\\/?[A-Za-z][\\w:-]*", cls: "ty" },
    { pat: "\\/?>|<\\?[\\w:-]*|\\?>", cls: "ty" },
    { pat: '"[^"]*"|\'[^\']*\'', cls: "s" },
    { pat: "[\\w:-]+(?=\\s*=)", cls: "pr" },
    { pat: "[\\w:-]+", cls: "pr" },
  ]);
  const hlTag = (t) => scanCompiled(t, TAG_SUB);
  const GRAMMARS = {};
  function getGrammar(id) {
    if (GRAMMARS[id]) return GRAMMARS[id];
    let rules = null, flags = "gm";
    const jsWords = wordCls(
      "as async await break case catch class const continue debugger default delete do else enum export extends finally for from function get if implements import in instanceof interface keyof let namespace new of private protected public readonly return satisfies set static super switch throw try type typeof var void while yield",
      "any bigint boolean never number object string symbol unknown Promise Map Set Array Record Partial Omit Pick Readonly",
      "true false null undefined NaN Infinity this"
    );
    switch (id) {
      case "go":
        rules = [
          { pat: P_CL, cls: "c" }, { pat: P_CB, cls: "c" },
          { pat: "`[^`]*`?", cls: "s" }, { pat: P_DQ, cls: "s" }, { pat: P_SQ, cls: "s" },
          { pat: P_NUM, cls: "n" },
          { pat: P_ID, cls: wordCls(
            "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var module require replace exclude",
            "any bool byte comparable complex64 complex128 error float32 float64 int int8 int16 int32 int64 rune string uint uint8 uint16 uint32 uint64 uintptr",
            "true false nil iota"
          ) },
        ];
        break;
      case "py":
        rules = [
          { pat: P_HASH, cls: "c" },
          { pat: '(?:[fFrRbBuU]{1,2})?(?:"""[\\s\\S]*?(?:"""|$)|\'\'\'[\\s\\S]*?(?:\'\'\'|$))', cls: "s" },
          { pat: "(?:[fFrRbBuU]{1,2})?(?:" + P_DQ + "|" + P_SQ + ")", cls: "s" },
          { pat: "@[A-Za-z_][\\w.]*", cls: "m" },
          { pat: P_NUM, cls: "n" },
          { pat: "[A-Za-z_][\\w]*", cls: wordCls(
            "and as assert async await break case class continue def del elif else except finally for from global if import in is lambda match nonlocal not or pass raise return try while with yield",
            "bool bytes callable dict float frozenset int list object set slice str tuple type Exception ValueError TypeError KeyError RuntimeError OSError",
            "True False None self cls"
          ) },
        ];
        break;
      case "js": case "ts": case "jsx": case "tsx":
        rules = [
          { pat: P_CL, cls: "c" }, { pat: P_CB, cls: "c" },
          { pat: P_TPL, cls: "s" }, { pat: P_DQ, cls: "s" }, { pat: P_SQ, cls: "s" },
          ...(id === "jsx" || id === "tsx" ? [{ pat: "<\\/?[A-Za-z][\\w.]*(?=\\s|\\/|\\>)", cls: "ty" }] : []),
          { pat: "@[A-Za-z_$][\\w$.]*", cls: "m" },
          { pat: P_NUM, cls: "n" },
          { pat: P_ID, cls: jsWords },
        ];
        break;
      case "sql":
        rules = [
          { pat: "--[^\\n]*", cls: "c" }, { pat: P_CB, cls: "c" },
          { pat: "'(?:''|\\\\[\\s\\S]|[^'])*'?", cls: "s" }, { pat: P_DQ, cls: "pr" }, { pat: "`[^`]*`?", cls: "pr" },
          { pat: "\\$\\d+|:[A-Za-z_]\\w*", cls: "m" },
          { pat: P_NUM, cls: "n" },
          { pat: "[A-Za-z_]\\w*", cls: wordCls(
            "select from where and or not in exists is as join left right inner outer full cross lateral on using group by order asc desc nulls first last having limit offset union all except intersect insert into values update set delete create alter drop replace truncate table view index database schema temp temporary if case when then else end with recursive distinct over partition rows range between preceding following unbounded current row primary key foreign references default unique check constraint begin commit rollback pragma explain analyze unnest",
            "int integer bigint smallint tinyint text varchar char nvarchar boolean bool date time timestamp timestamptz datetime float double real decimal numeric json jsonb blob bytea uuid serial array map struct string",
            "null true false",
            { ci: true, pascal: false, calls: true }
          ) },
        ];
        break;
      case "json":
        rules = [
          { pat: P_CL, cls: "c" }, { pat: P_CB, cls: "c" },
          { pat: P_DQ, cls: (tok, src, idx) => {
            let j = idx + tok.length;
            while (j < src.length && (src[j] === " " || src[j] === "\t" || src[j] === "\n" || src[j] === "\r")) j++;
            return src[j] === ":" ? "pr" : "s";
          } },
          { pat: "\\b(?:true|false|null)\\b", cls: "n" },
          { pat: "-?\\b\\d+(?:\\.\\d+)?(?:[eE][+-]?\\d+)?\\b", cls: "n" },
        ];
        break;
      case "yaml":
        rules = [
          { pat: "(?:^|[ \\t])#[^\\n]*", html: leadHash },
          { pat: "^[ \\t]*(?:-[ \\t]+)?[A-Za-z0-9_.\\-]+(?=\\s*:(?:\\s|$))", html: (t) => {
            const m = /^([ \t]*(?:-[ \t]+)?)(.*)$/.exec(t);
            return esc(m[1]) + `<span class="hl-pr">${esc(m[2])}</span>`;
          } },
          { pat: P_DQ, cls: "s" }, { pat: "'(?:''|[^'\\n])*'?", cls: "s" },
          { pat: "[&*][\\w-]+|![\\w/-]+", cls: "m" },
          { pat: "\\b(?:true|false|null|yes|no|on|off)\\b", cls: "n" },
          { pat: "-?\\b\\d+(?:\\.\\d+)?\\b", cls: "n" },
        ];
        break;
      case "sh":
        rules = [
          { pat: "^#![^\\n]*", cls: "m" },
          { pat: "(?:^|[ \\t;&|()])#[^\\n]*", html: leadHash },
          { pat: '"(?:\\\\[\\s\\S]|[^"\\\\])*"?', cls: "s" }, { pat: "'[^']*'", cls: "s" },
          { pat: "\\$(?:\\{[^}\\n]+\\}|\\([^)\\n]*\\)|[A-Za-z_]\\w*|[0-9?!@#$*_-])", cls: "m" },
          { pat: "(?:^|[ \\t])--?[A-Za-z0-9][\\w-]*", html: (t) => (t[0] === "-" ? `<span class="hl-pr">${esc(t)}</span>` : esc(t[0]) + `<span class="hl-pr">${esc(t.slice(1))}</span>`) },
          { pat: "\\b(?:if|then|else|elif|fi|for|in|do|done|while|until|case|esac|function|select|return|exit|break|continue|export|local|readonly|declare|typeset|set|unset|shift|source|trap|exec|eval)\\b", cls: "k" },
          { pat: "\\b(?:echo|printf|cd|pwd|test|read|kill|wait|true|false)\\b", cls: "fn" },
          { pat: "-?\\b\\d+\\b", cls: "n" },
        ];
        break;
      case "html": case "xml":
        rules = [
          { pat: "<!--[\\s\\S]*?(?:-->|$)", cls: "c" },
          { pat: "<![^>]*>", cls: "m" },
          ...(id === "html" ? [
            { pat: "<script\\b[^>]*>[\\s\\S]*?<\\/script>", html: (t) => {
              const m = /^(<script\b[^>]*>)([\s\S]*)(<\/script>)$/i.exec(t);
              return m ? hlTag(m[1]) + highlightCode(m[2], "js") + hlTag(m[3]) : esc(t);
            } },
            { pat: "<style\\b[^>]*>[\\s\\S]*?<\\/style>", html: (t) => {
              const m = /^(<style\b[^>]*>)([\s\S]*)(<\/style>)$/i.exec(t);
              return m ? hlTag(m[1]) + highlightCode(m[2], "css") + hlTag(m[3]) : esc(t);
            } },
          ] : []),
          { pat: "<\\/?[A-Za-z?][\\w:-]*(?:\\s+(?:\"[^\"]*\"|'[^']*'|[^\"'<>])*)?\\s*[\\/?]?>", html: hlTag },
          { pat: "&[A-Za-z0-9#]+;", cls: "n" },
        ];
        break;
      case "css":
        rules = [
          { pat: P_CB, cls: "c" }, { pat: P_CL, cls: "c" },
          { pat: P_DQ, cls: "s" }, { pat: P_SQ, cls: "s" },
          { pat: "@[\\w-]+", cls: "k" },
          { pat: "--[\\w-]+", cls: "m" },
          { pat: "#[0-9a-fA-F]{3,8}\\b", cls: "n" },
          { pat: "[a-zA-Z-]+(?=\\s*:)", cls: "pr" },
          { pat: "-?\\b\\d+(?:\\.\\d+)?(?:px|em|rem|dvh|svh|vh|vw|%|s|ms|deg|fr|ch)?\\b", cls: "n" },
          { pat: "\\b(?:var|calc|min|max|clamp|color-mix|light-dark|linear-gradient|radial-gradient|url|rgb|rgba|hsl|hsla)(?=\\()", cls: "fn" },
        ];
        break;
      case "diff":
        rules = [
          { pat: "^(?:diff |index |--- |\\+\\+\\+ )[^\\n]*", cls: "m" },
          { pat: "^@@[^\\n]*", cls: "fn" },
          { pat: "^\\+[^\\n]*", cls: "add" },
          { pat: "^-[^\\n]*", cls: "del" },
        ];
        break;
      case "md":
        rules = [
          { pat: "^#{1,6}\\s[^\\n]*", cls: "fn" },
          { pat: "```[\\s\\S]*?(?:```|$)", cls: "s" },
          { pat: "`[^`\\n]+`", cls: "s" },
          { pat: "\\*\\*[^*\\n]+\\*\\*", cls: "k" },
          { pat: "\\[[^\\]\\n]+\\]\\([^)\\n]+\\)", cls: "ty" },
          { pat: "^>\\s[^\\n]*", cls: "c" },
        ];
        break;
      case "rust":
        rules = [
          { pat: P_CL, cls: "c" }, { pat: P_CB, cls: "c" },
          { pat: "#!?\\[[^\\]\\n]*\\]", cls: "m" },
          { pat: 'r#*"[\\s\\S]*?"#*', cls: "s" }, { pat: P_DQ, cls: "s" },
          { pat: "'(?:\\\\.|[^'\\\\\\n])'", cls: "s" },
          { pat: "'[a-zA-Z_]\\w*\\b", cls: "m" },
          { pat: "\\b[a-z_]\\w*!", cls: "m" },
          { pat: P_NUM, cls: "n" },
          { pat: "[A-Za-z_]\\w*", cls: wordCls(
            "as async await break const continue crate dyn else enum extern fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait type unsafe use where while",
            "bool char f32 f64 i8 i16 i32 i64 i128 isize str u8 u16 u32 u64 u128 usize String Vec Option Result Box Rc Arc Cow",
            "true false None Some Ok Err"
          ) },
        ];
        break;
      case "java":
        rules = [
          { pat: P_CL, cls: "c" }, { pat: P_CB, cls: "c" },
          { pat: P_DQ, cls: "s" }, { pat: P_SQ, cls: "s" },
          { pat: "@[A-Za-z_][\\w.]*", cls: "m" },
          { pat: P_NUM, cls: "n" },
          { pat: P_ID, cls: wordCls(
            "abstract assert break case catch class const continue default do else enum extends final finally for fun goto if implements import in instanceof interface internal native new object open override package private protected public record return sealed static strictfp super suspend switch synchronized throw throws transient try val var volatile when while yield",
            "boolean byte char double float int long short void String List Map Set Optional Object Integer Long Double Boolean",
            "true false null this"
          ) },
        ];
        break;
      case "c": case "cpp":
        rules = [
          { pat: P_CL, cls: "c" }, { pat: P_CB, cls: "c" },
          { pat: "^[ \\t]*#[ \\t]*[a-zA-Z_]\\w*[^\\n]*", cls: "m" },
          { pat: P_DQ, cls: "s" }, { pat: P_SQ, cls: "s" },
          { pat: P_NUM, cls: "n" },
          { pat: "[A-Za-z_]\\w*", cls: wordCls(
            "alignas alignof auto break case catch class co_await co_return co_yield constexpr consteval constinit const const_cast continue decltype default delete do dynamic_cast else enum explicit export extern final for friend goto if inline mutable namespace new noexcept operator override private protected public register reinterpret_cast requires return sizeof static static_assert static_cast struct switch template thread_local throw try typedef typeid typename union using virtual volatile while message service rpc repeated optional required",
            "bool char char8_t char16_t char32_t double float int long short signed size_t ssize_t int8_t int16_t int32_t int64_t uint8_t uint16_t uint32_t uint64_t uintptr_t ptrdiff_t void wchar_t string vector map unordered_map set unique_ptr shared_ptr bytes",
            "true false nullptr NULL this"
          ) },
        ];
        break;
      case "toml":
        rules = [
          { pat: P_HASH, cls: "c" },
          { pat: "^[ \\t]*\\[\\[?[^\\]\\n]+\\]\\]?", cls: "ty" },
          { pat: '"""[\\s\\S]*?(?:"""|$)|\'\'\'[\\s\\S]*?(?:\'\'\'|$)', cls: "s" },
          { pat: P_DQ, cls: "s" }, { pat: "'[^'\\n]*'?", cls: "s" },
          { pat: "^[ \\t]*[A-Za-z0-9_.-]+(?=\\s*=)", cls: "pr" },
          { pat: "\\b(?:true|false)\\b|\\b\\d{4}-\\d{2}-\\d{2}(?:[T ]\\d{2}:\\d{2}:\\d{2}(?:\\.\\d+)?(?:Z|[+-]\\d{2}:\\d{2})?)?\\b", cls: "n" },
          { pat: P_NUM, cls: "n" },
        ];
        break;
      case "dockerfile":
        rules = [
          { pat: P_HASH, cls: "c" },
          { pat: "^[ \\t]*(?:FROM|RUN|CMD|LABEL|EXPOSE|ENV|ADD|COPY|ENTRYPOINT|VOLUME|USER|WORKDIR|ARG|ONBUILD|STOPSIGNAL|HEALTHCHECK|SHELL|AS)\\b", cls: "k" },
          { pat: P_DQ, cls: "s" }, { pat: P_SQ, cls: "s" },
          { pat: "\\$(?:\\{[^}\\n]+\\}|[A-Za-z_]\\w*)", cls: "m" },
          { pat: "(?:^|[ \\t])--[A-Za-z0-9][\\w=-]*", html: (t) => (t[0] === "-" ? `<span class="hl-pr">${esc(t)}</span>` : esc(t[0]) + `<span class="hl-pr">${esc(t.slice(1))}</span>`) },
          { pat: P_NUM, cls: "n" },
        ];
        flags = "gim";
        break;
      case "makefile":
        rules = [
          { pat: P_HASH, cls: "c" },
          { pat: "^[A-Za-z0-9_.\\-/]+(?=\\s*:[^=])", cls: "fn" },
          { pat: "^[ \\t]*(?:include|-include|ifeq|ifneq|ifdef|ifndef|else|endif|export|unexport|override|define|endef|\\.PHONY|\\.DEFAULT_GOAL)\\b", cls: "k" },
          { pat: "^[ \\t]*[A-Za-z_]\\w*(?=\\s*[:+?]?=)", cls: "pr" },
          { pat: "\\$(?:\\([^)\\n]+\\)|\\{[^}\\n]+\\}|[@<^?*+%|])", cls: "m" },
          { pat: P_DQ, cls: "s" }, { pat: P_SQ, cls: "s" },
        ];
        break;
    }
    return rules ? (GRAMMARS[id] = compileRules(rules, flags)) : null;
  }
  function highlightCode(code, langId) {
    if (!langId || code.length > HL_MAX_BYTES) return esc(code);
    const g = getGrammar(langId);
    return g ? scanCompiled(code, g) : esc(code);
  }

  // ───────────────────────────── file viewer
  // A file an agent mentions opens here, read-only, from the computer it is on.
  const IMG_EXT = /\.(png|jpe?g|gif|webp|svg|bmp|ico)$/i;
  const MD_EXT = /\.(md|markdown|mdx)$/i;
  const HTML_EXT = /\.html?$/i;
  const htmlFileURL = (path, machine = "") => {
    if (typeof window.__collieDemoHtmlUrl === "function") {
      const u = window.__collieDemoHtmlUrl(path);
      if (u) return u;
    }
    return `/api/file?path=${encodeURIComponent(path)}${machine ? "&machine=" + encodeURIComponent(machine) : ""}&as=html`;
  };
  const fileLinkAttrs = (rawPath, escapedPath, titleText) => HTML_EXT.test(rawPath)
    ? `class="fpath html-link" href="${esc(htmlFileURL(rawPath))}" target="_blank" rel="noopener" data-path="${escapedPath}" title="${esc("在新标签页打开 " + rawPath)}"`
    : IMG_EXT.test(rawPath)
    ? `class="fpath img-link" href="#" data-path="${escapedPath}" title="${esc("查看大图 " + (rawPath.split("/").pop() || rawPath))}"`
    : `class="fpath" href="#" data-path="${escapedPath}" title="${titleText}"`;
  const IMG_TYPE = { svg: "image/svg+xml", jpg: "image/jpeg", jpeg: "image/jpeg", ico: "image/x-icon" };
  const imgMime = (p) => {
    const ext = String(p || "").split(".").pop().toLowerCase();
    return IMG_TYPE[ext] || "image/" + ext;
  };
  function fileContextMachine() {
    if (view === "pane") return find(current)?.machine;
    if (view === "ext") {
      const id = extCur || "";
      return (id.startsWith("h:") ? hist.find((h) => h.id === id.slice(2)) : ext.find((x) => x.id === id))?.machine;
    }
  }
  async function fetchFile(path, machine) {
    const q = `/api/file?path=${encodeURIComponent(path)}${machine ? "&machine=" + encodeURIComponent(machine) : ""}${IMG_EXT.test(path) ? "&as=image" : ""}`;
    const r = routeReq(q);
    const res = await fetch(r.url, { headers: r.hub ? { Authorization: "Bearer " + r.hub.token } : {}, credentials: r.hub ? "omit" : "same-origin" });
    if (res.status === 404) return null;
    if (!res.ok) { let e = {}; try { e = await res.json(); } catch {} throw new Error(e.error || "打不开这个文件"); }
    return { kind: res.headers.get("X-File-Kind"), size: +res.headers.get("X-File-Size") || 0,
      path: decodeURIComponent(res.headers.get("X-File-Path") || path), blob: await res.blob(), machine };
  }
  // Where the message came from first, then the hub, then every other computer.
  const fileTries = () => {
    const ms = (state.machines || []).filter((m) => !m.hub && (m.local || m.online)).map((m) => m.local ? "" : m.id);
    return [...new Set([fileContextMachine() ?? "", "", ...ms])];
  };
  async function findFile(path, tries = fileTries()) {
    for (const m of tries) { const f = await fetchFile(path, m); if (f) return f; }
    return null;
  }
  let fileUrl = "", wideFileUrl = "", fileSeq = 0, wideSeq = 0;
  let fvCur = null, rvCur = null;
  const FILE_DL_MAX = 100 * (1 << 20), FILE_COPY_MAX = 20 * (1 << 20);
  const wideFileHash = (path, machine = "") => {
    const q = new URLSearchParams({ path });
    if (machine) q.set("machine", machine);
    return "#/file?" + q.toString();
  };
  const baseFileName = (raw) => {
    const s = String(raw || "").split(/[?#]/)[0].replace(/\/+$/, "");
    const n = s.split("/").pop() || "";
    try { return decodeURIComponent(n) || "download"; } catch { return n || "download"; }
  };
  function triggerBlobDownload(blob, fileName) {
    const u = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = u;
    a.download = fileName;
    a.style.display = "none";
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(u), 60000);
  }
  async function downloadCurFile(cur) {
    if (!cur?.reqPath) return;
    if (cur.size > FILE_DL_MAX) {
      toast("文件超过 100 MB，无法通过网页直接下载", "attn");
      return;
    }
    const fileName = baseFileName(cur.path || cur.reqPath);
    const machine = cur.machine || "";
    const q = `/api/file?path=${encodeURIComponent(cur.reqPath)}${machine ? "&machine=" + encodeURIComponent(machine) : ""}&download=1`;
    if (window.__collieDemo && !cur.truncated && cur.blob) {
      triggerBlobDownload(cur.blob, fileName);
      return;
    }
    const r = routeReq(q);
    if (window.__collieDemo || r.hub) {
      try {
        const res = await fetch(r.url, {
          headers: r.hub ? { Authorization: "Bearer " + r.hub.token } : {},
          credentials: r.hub ? "omit" : "same-origin",
        });
        if (!res.ok) {
          let e = {};
          try { e = await res.json(); } catch {}
          throw new Error(e.error || "下载失败");
        }
        triggerBlobDownload(await res.blob(), fileName);
      } catch (err) {
        toast(err.message || "下载失败", "attn");
      }
      return;
    }
    const a = document.createElement("a");
    a.href = q;
    a.download = fileName;
    a.style.display = "none";
    document.body.append(a);
    a.click();
    a.remove();
  }
  async function fetchFullTextForCopy(cur) {
    const q = `/api/file?path=${encodeURIComponent(cur.reqPath)}${cur.machine ? "&machine=" + encodeURIComponent(cur.machine) : ""}&download=1`;
    const r = routeReq(q);
    const res = await fetch(r.url, {
      headers: r.hub ? { Authorization: "Bearer " + r.hub.token } : {},
      credentials: r.hub ? "omit" : "same-origin",
    });
    if (!res.ok) {
      let e = {};
      try { e = await res.json(); } catch {}
      throw new Error(e.error || "拉取完整原文失败");
    }
    const full = await res.text();
    cur.rawText = full;
    cur.truncated = false;
    return full;
  }
  function syncFileBar(prefix, { langLabel = "", copyable = false, copyTitle = "复制全文", downloadable = false, wideHref = "" }) {
    const lEl = $("#" + prefix + "-lang"), cWrap = $("#" + prefix + "-copy-wrap"), cBtn = $("#" + prefix + "-copy");
    const dlBtn = $("#" + prefix + "-dl"), wideBtn = prefix === "fv" ? $("#fv-wide") : null;
    if (lEl) { lEl.hidden = !langLabel; lEl.textContent = langLabel; }
    if (dlBtn) dlBtn.hidden = !downloadable;
    if (cWrap) cWrap.hidden = !copyable;
    if (cBtn) {
      cBtn.title = copyTitle;
      cBtn.setAttribute("aria-label", copyTitle);
      cBtn.removeAttribute("data-copied");
      cBtn.removeAttribute("data-failed");
      if (cBtn.nextElementSibling) cBtn.nextElementSibling.textContent = "";
    }
    if (wideBtn) {
      wideBtn.hidden = !wideHref;
      if (wideHref) wideBtn.href = wideHref;
    }
  }
  function renderCodeBody(body, path, text, cut, srcId, wrapCard = false) {
    const lang = detectCodeLang(path, true);
    const norm = text.replace(/\r\n/g, "\n").replace(/\n$/, "");
    let lines = 1;
    for (let i = 0; i < norm.length && lines <= HL_MAX_LINES + 1; i++) if (norm.charCodeAt(i) === 10) lines++;
    let inner = "";
    if (lang && norm.length <= HL_MAX_BYTES && lines <= HL_MAX_LINES) {
      const nums = Array.from({ length: lines }, (_, i) => i + 1).join("\n");
      body.classList.add("is-code");
      inner = `<div class="fv-code"><div class="fv-ln" aria-hidden="true">${nums}</div><pre class="fv-pre"><code id="${srcId}">${highlightCode(norm, lang.id)}</code></pre></div>`;
    } else if (lang) {
      body.classList.add("is-code");
      inner = `<pre class="fv-pre fv-large" id="${srcId}">${esc(norm)}</pre>`;
    } else if (wrapCard) {
      body.classList.add("is-code");
      inner = `<pre class="fv-pre fv-large" id="${srcId}">${esc(text)}</pre>`;
    } else {
      inner = `<pre class="fv-pre" id="${srcId}">${esc(text)}</pre>`;
    }
    body.innerHTML = wrapCard ? `<div class="rv-code-card">${inner}</div>${cut}` : `${inner}${cut}`;
    return lang;
  }
  async function openFile(path) {
    if (IMG_EXT.test(path)) { openLightbox([{ path }], 0); return; }
    const body = $("#fv-body"), seq = ++fileSeq;
    fvCur = null;
    body.classList.remove("is-code");
    const initMachine = fileContextMachine() || "";
    syncFileBar("fv", { wideHref: HTML_EXT.test(path) ? "" : wideFileHash(path, initMachine) });
    $("#fv-name").textContent = path.split("/").pop();
    $("#fv-path").textContent = path;
    body.innerHTML = `<p class="fv-note muted">读取中…</p>`;
    showFilePane();
    const tries = fileTries();
    let f = null;
    try {
      f = await findFile(path, tries);
    } catch (e) { syncFileBar("fv", {}); body.innerHTML = `<p class="fv-note">${esc(e.message)}</p>`; return; }
    if (seq !== fileSeq) return;
    if (!f) { syncFileBar("fv", {}); body.innerHTML = `<p class="fv-note">在${tries.length > 1 ? "各台电脑" : "这台电脑"}上都没找到这个文件。</p>`; return; }
    const where = (state.machines || []).length > 1 || f.machine ? machineName(f.machine) + " · " : "";
    $("#fv-path").textContent = where + f.path + " · " + fmtSize(f.size);
    if (fileUrl) URL.revokeObjectURL(fileUrl), fileUrl = "";
    const canDl = f.size <= FILE_DL_MAX;
    const wHref = HTML_EXT.test(path) ? "" : wideFileHash(path, f.machine || "");
    fvCur = { reqPath: path, path: f.path, machine: f.machine || "", kind: f.kind, size: f.size, blob: f.blob, rawText: "", truncated: false };
    if (f.kind === "image") {
      syncFileBar("fv", { downloadable: canDl, wideHref: wHref });
      const ext = path.split(".").pop().toLowerCase();
      fileUrl = URL.createObjectURL(new Blob([f.blob], { type: IMG_TYPE[ext] || "image/" + ext }));
      body.innerHTML = `<div class="fv-img"><img src="${fileUrl}" alt=""></div>`;
      return;
    }
    if (f.kind !== "text") {
      syncFileBar("fv", { downloadable: canDl });
      body.innerHTML = `<p class="fv-note">这个文件不是文本${f.size > 20 << 20 ? "，也太大了" : ""}，没法在这里预览${canDl ? "（可点右上角下载原件）" : ""}。</p>`;
      return;
    }
    const text = await f.blob.text();
    if (seq !== fileSeq) return;
    const truncated = f.size > f.blob.size;
    fvCur.rawText = text;
    fvCur.truncated = truncated;
    const cut = truncated ? `<p class="fv-note muted">文件有 ${fmtSize(f.size)}，这里只预览前 1 MB（点右上角下载可获取完整原件）。</p>` : "";
    if (MD_EXT.test(path)) {
      syncFileBar("fv", { langLabel: "Markdown", copyable: true, copyTitle: "复制 Markdown 源码", downloadable: canDl, wideHref: wHref });
      body.innerHTML = `<div class="msg assistant fv-md" id="fv-code-src">${md(text)}</div>${cut}`;
      body.scrollTop = body.scrollLeft = 0;
      return;
    }
    const lang = renderCodeBody(body, path, text, cut, "fv-code-src", false);
    syncFileBar("fv", { langLabel: lang ? lang.label : "", copyable: true, copyTitle: "复制全文", downloadable: canDl, wideHref: wHref });
    body.scrollTop = body.scrollLeft = 0;
  }
  async function openWideFile(rawQuery, restore = null) {
    const q = new URLSearchParams(String(rawQuery || "").replace(/^\?/, ""));
    const path = (q.get("path") || "").trim();
    const machineHint = (q.get("machine") || "").trim();
    const body = $("#rv-body"), sc = $("#rv-scroller"), seq = ++wideSeq;
    rvCur = null;
    body.classList.remove("is-code");
    syncFileBar("rv", {});
    if (!path) {
      $("#rv-name").textContent = "文件阅读";
      $("#rv-path").textContent = "";
      document.title = "文件阅读 · Collie";
      body.innerHTML = `<p class="fv-note muted">缺少文件路径参数。</p>`;
      return;
    }
    const name = path.split("/").pop() || path;
    $("#rv-name").textContent = name;
    $("#rv-path").textContent = (machineHint ? machineName(machineHint) + " · " : "") + path;
    document.title = `${name} · Collie`;
    body.innerHTML = `<p class="fv-note muted">读取中…</p>`;
    const tries = [...new Set([machineHint, ...fileTries()])];
    let f = null;
    try {
      f = await findFile(path, tries);
    } catch (e) { body.innerHTML = `<p class="fv-note">${esc(e.message)}</p>`; return; }
    if (seq !== wideSeq || view !== "file") return;
    if (!f) { body.innerHTML = `<p class="fv-note">在${tries.length > 1 ? "各台电脑" : "这台电脑"}上都没找到这个文件。</p>`; return; }
    const where = (state.machines || []).length > 1 || f.machine ? machineName(f.machine) + " · " : "";
    $("#rv-path").textContent = where + f.path + " · " + fmtSize(f.size);
    if (wideFileUrl) URL.revokeObjectURL(wideFileUrl), wideFileUrl = "";
    const canDl = f.size <= FILE_DL_MAX;
    rvCur = { reqPath: path, path: f.path, machine: f.machine || "", kind: f.kind, size: f.size, blob: f.blob, rawText: "", truncated: false };
    if (f.kind === "image") {
      syncFileBar("rv", { downloadable: canDl });
      const ext = path.split(".").pop().toLowerCase();
      wideFileUrl = URL.createObjectURL(new Blob([f.blob], { type: IMG_TYPE[ext] || "image/" + ext }));
      body.innerHTML = `<div class="rv-img"><img src="${wideFileUrl}" alt="${esc(name)}" data-wide-img="${esc(path)}"></div>`;
      if (restore) restoreSimpleScroll(sc, restore); else sc.scrollTop = 0;
      return;
    }
    if (f.kind !== "text") {
      syncFileBar("rv", { downloadable: canDl });
      body.innerHTML = `<p class="fv-note">这个文件不是文本${f.size > 20 << 20 ? "，也太大了" : ""}，没法在这里预览${canDl ? "（可点右上角下载原件）" : ""}。</p>`;
      return;
    }
    const text = await f.blob.text();
    if (seq !== wideSeq || view !== "file") return;
    const truncated = f.size > f.blob.size;
    rvCur.rawText = text;
    rvCur.truncated = truncated;
    const cut = truncated ? `<p class="fv-note muted">文件有 ${fmtSize(f.size)}，这里只显示前 1 MB（点右上角下载可获取完整原件）。</p>` : "";
    if (MD_EXT.test(path)) {
      syncFileBar("rv", { langLabel: "Markdown", copyable: true, copyTitle: "复制 Markdown 源码", downloadable: canDl });
      body.innerHTML = `<article class="msg assistant rv-md" id="rv-code-src">${md(text)}</article>${cut}`;
    } else {
      const lang = renderCodeBody(body, path, text, cut, "rv-code-src", true);
      syncFileBar("rv", { langLabel: lang ? lang.label : "", copyable: true, copyTitle: "复制全文", downloadable: canDl });
    }
    if (restore) restoreSimpleScroll(sc, restore);
    else { markProgrammaticScroll(); sc.scrollTop = sc.scrollLeft = 0; }
  }
  $("#fv-dl")?.addEventListener("click", () => downloadCurFile(fvCur));
  $("#rv-dl")?.addEventListener("click", () => downloadCurFile(rvCur));
  $("#rv-home")?.addEventListener("click", (ev) => {
    if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    if (navCur > 0) {
      ev.preventDefault();
      goBack();
    }
  });
  // Images a message carries ("[image:~/.collie/images/….png]", saved from the
  // agent's transcript on its computer) show as thumbnails; tap for the lightbox.
  const IMG_TOKEN = /\[image:(~\/\.collie\/images\/[0-9a-f]+\.(?:png|jpe?g|gif|webp|svg))\]\n?/gi;
  const thumbs = new Map();    // path -> object URL, kept so redraws don't refetch
  const thumbMeta = new Map(); // path -> { size, machine, path }
  const IMG_RUN = /(?:\[image:~\/\.collie\/images\/[0-9a-f]+\.(?:png|jpe?g|gif|webp|svg)\]\n?)+/gi;
  const thumbBtn = (p) => `<button type="button" class="thumb" data-img="${esc(p)}" aria-label="${esc("查看大图 " + (p.split("/").pop() || "图片"))}"><img alt=""${thumbs.get(p) ? ` src="${thumbs.get(p)}"` : ""}></button>`;
  const withImages = (html) => html.includes("[image:") ? html.replace(IMG_RUN, (run) => `<span class="thumbs">${run.replace(IMG_TOKEN, (_, p) => thumbBtn(p))}</span>`) : html;
  async function loadThumb(img) {
    const p = img.parentElement.dataset.img;
    if (thumbs.get(p)) { img.src = thumbs.get(p); return; }
    if (thumbs.has(p)) return; // on its way; it fills every copy when it lands
    thumbs.set(p, "");
    try {
      const f = await findFile(p);
      if (!f || f.kind !== "image") throw 0;
      const u = URL.createObjectURL(new Blob([f.blob], { type: imgMime(p) }));
      thumbs.set(p, u);
      thumbMeta.set(p, { size: f.size, machine: f.machine, path: f.path });
      for (const el of document.querySelectorAll(".thumb[data-img] img")) {
        if (el.parentElement.dataset.img === p) { el.src = u; el.parentElement.classList.remove("broken"); }
      }
    } catch { thumbs.delete(p); img.parentElement.classList.add("broken"); }
  }
  // Under each Collie message, on hover: copy, quote, pin, and when it was said.
  const ICON_COPY = `<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/></svg>`;
  const ICON_QUOTE = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M16 3a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2 1 1 0 0 1 1 1v1a2 2 0 0 1-2 2 1 1 0 0 0-1 1v2a1 1 0 0 0 1 1 6 6 0 0 0 6-6V5a2 2 0 0 0-2-2z"/><path d="M5 3a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2 1 1 0 0 1 1 1v1a2 2 0 0 1-2 2 1 1 0 0 0-1 1v2a1 1 0 0 0 1 1 6 6 0 0 0 6-6V5a2 2 0 0 0-2-2z"/></svg>`;
  const pinActBtn = (id, pinned) => `<button type="button" class="pin-btn${pinned ? " on" : ""}" data-pin="${esc(id)}" aria-pressed="${pinned}" title="${pinned ? "取消钉住" : "钉住"}" aria-label="${pinned ? "取消钉住" : "钉住"}">${ICON_PIN}</button>`;
  const msgActs = (e, at, pinned = false) => e.id ? `<div class="msg-acts">
      <button type="button" data-copy="${esc(e.id)}" title="复制" aria-label="复制">${ICON_COPY}</button>
      <button type="button" class="re-btn" data-re="${esc(e.id)}" title="引用回复" aria-label="引用回复">${ICON_QUOTE}</button>
      ${pinActBtn(e.id, pinned)}
      ${at ? `<span class="t">${esc(sepTime(at))}</span>` : ""}</div>` : "";
  // Under your own message, at its right edge: when you said it, copy, and pin.
  const mineActs = (e, at, pinned = false) => e.id ? `<div class="msg-acts">${at ? `<span class="t">${esc(sepTime(at))}</span>` : ""}
      <button type="button" data-copy="${esc(e.id)}" title="复制" aria-label="复制">${ICON_COPY}</button>
      ${pinActBtn(e.id, pinned)}</div>` : "";
  async function copyText(t) {
    if (navigator.clipboard && window.isSecureContext) {
      try { await navigator.clipboard.writeText(t); return true; } catch {}
    }
    // Plain-HTTP LAN addresses have no Async Clipboard API: synchronous textarea fallback.
    const ta = Object.assign(document.createElement("textarea"), { value: t });
    ta.setAttribute("readonly", "");
    ta.style.cssText = "position:fixed;inset-block-start:0;opacity:0";
    document.body.append(ta);
    ta.select();
    try { return document.execCommand("copy"); } catch { return false; } finally { ta.remove(); }
  }
  const copyTimers = new WeakMap();
  let lastCopyMsg = "";
  function reportCopy(button, ok) {
    const tip = button.nextElementSibling;
    clearTimeout(copyTimers.get(button));
    button.removeAttribute("data-copied");
    button.removeAttribute("data-failed");
    let msg = ok ? "已复制" : "复制失败";
    if (msg === lastCopyMsg) msg += "\u00A0";
    lastCopyMsg = msg;
    if (tip) tip.textContent = "";
    void button.offsetWidth;
    button.setAttribute(ok ? "data-copied" : "data-failed", "");
    if (tip) requestAnimationFrame(() => { tip.textContent = msg; });
    copyTimers.set(button, setTimeout(() => {
      button.removeAttribute("data-copied");
      button.removeAttribute("data-failed");
      if (tip) tip.textContent = "";
    }, 1500));
  }
  async function invokeCopy(button, target) {
    const cur = target?.id === "fv-code-src" ? fvCur : target?.id === "rv-code-src" ? rvCur : null;
    if (cur) {
      if (cur.truncated) {
        if (cur.size > FILE_COPY_MAX) {
          reportCopy(button, false);
          toast("文件超过 20 MB，请点下载按钮获取完整原件", "attn");
          return;
        }
        try {
          const full = await fetchFullTextForCopy(cur);
          const ok = await copyText(full);
          reportCopy(button, ok);
          if (!ok) toast("完整全文已加载，再点一次即可复制");
        } catch (err) {
          reportCopy(button, false);
          toast(err.message || "复制失败", "attn");
        }
        return;
      }
      if (!cur.rawText) return;
      reportCopy(button, await copyText(cur.rawText));
      return;
    }
    const text = target?.textContent || "";
    if (!text) return;
    reportCopy(button, await copyText(text));
  }
  if ("CommandEvent" in window) {
    document.addEventListener("command", (ev) => {
      if (ev.command === "--copy") invokeCopy(ev.source, ev.target);
    }, true);
  } else {
    document.addEventListener("click", (ev) => {
      const btn = ev.target.closest('button[command="--copy"]');
      if (!btn) return;
      const target = document.getElementById(btn.getAttribute("commandfor"));
      if (target) invokeCopy(btn, target);
    });
  }
  document.addEventListener("click", async (ev) => {
    const pb = ev.target.closest(".pin-btn[data-pin]");
    if (pb) { ev.preventDefault(); togglePin(pb.dataset.pin); return; }
    const b = ev.target.closest("[data-copy]");
    if (!b) return;
    const e = chatEntries().find((x) => x.id === b.dataset.copy);
    if (e) toast(await copyText(e.text) ? "已复制" : "复制失败", "");
  });
  // A reply's quote of your message: take you to it.
  document.addEventListener("click", (e) => {
    const a = e.target.closest("a.re[data-goto]");
    if (!a) return;
    e.preventDefault();
    jumpToEntry(a.dataset.goto);
  });
  // No hover on a phone: tap a Collie chat message to see when it was said.
  document.addEventListener("click", (e) => {
    if (!matchMedia("(hover: none)").matches) return;
    const m = e.target.closest("#v-collie .msg[data-time], #v-collie .say .msg, #v-collie .mine .msg");
    if (m && e.target.closest("a, button, .thumb, img")) return;
    for (const x of document.querySelectorAll("#v-collie .msg.show-time")) if (x !== m) x.classList.remove("show-time");
    m?.classList.toggle("show-time");
  });
  new MutationObserver(() => {
    for (const img of document.querySelectorAll(".thumb img:not([src])")) if (!img.dataset.asked) { img.dataset.asked = 1; loadThumb(img); }
  }).observe(document.body, { childList: true, subtree: true });

  // ───────────────────────────── fullscreen image lightbox
  const lightbox = $("#lightbox"), lbStage = $("#lb-stage"), lbImg = $("#lb-img");
  const lbName = $("#lb-name"), lbCount = $("#lb-count"), lbMeta = $("#lb-meta"), lbStatus = $("#lb-status");
  const lbZoomBtn = $("#lb-zoom"), lbZoomPct = $("#lb-zoom-pct"), lbOpenLink = $("#lb-open"), lbDlLink = $("#lb-dl");
  const lbPrevBtn = $("#lb-prev"), lbNextBtn = $("#lb-next"), lbCloseBtn = $("#lb-close");
  let lbItems = [], lbIdx = 0, lbSeq = 0, lbTempUrl = "", lbCurMeta = null;
  let lbScale = 1, lbX = 0, lbY = 0, lbSavedScroll = null;
  const lbTabProtected = new Set();

  let supportsDisplayTransition;
  function canTransitionDisplay() {
    if (supportsDisplayTransition !== undefined) return supportsDisplayTransition;
    if (!window.CSS?.supports?.("transition-behavior", "allow-discrete") || !document.body) return false;
    const probe = document.createElement("div");
    probe.style.cssText = "transition: display 1s allow-discrete !important; display: block;";
    document.body.appendChild(probe);
    getComputedStyle(probe).display;
    probe.style.display = "none";
    supportsDisplayTransition = getComputedStyle(probe).display === "block";
    probe.remove();
    return supportsDisplayTransition;
  }
  const supportsTopLayerExit = () => !!(window.CSS?.supports?.("overlay", "auto") && canTransitionDisplay());

  function releaseLbTempUrl() {
    if (!lbTempUrl) return;
    const u = lbTempUrl;
    lbTempUrl = "";
    // Never revoke an ObjectURL currently shared with the `thumbs` cache.
    for (const v of thumbs.values()) if (v === u) return;
    if (lbTabProtected.has(u)) {
      setTimeout(() => { lbTabProtected.delete(u); URL.revokeObjectURL(u); }, 60000);
    } else {
      URL.revokeObjectURL(u);
    }
  }
  const cleanImgName = (raw, fallback = "图片") => {
    const s = String(raw || "").split(/[?#]/)[0].replace(/\/+$/, "");
    const n = s.split("/").pop() || "";
    try { return decodeURIComponent(n) || fallback; } catch { return n || fallback; }
  };
  function updateLbMeta() {
    if (!lbMeta) return;
    const parts = [];
    if (lbCurMeta) {
      const multi = (state.machines || []).length > 1;
      if (multi || lbCurMeta.machine) parts.push(machineName(lbCurMeta.machine));
      if (lbCurMeta.isSvg) parts.push("SVG");
      else if (lbCurMeta.width > 0 && lbCurMeta.height > 0) parts.push(`${lbCurMeta.width} × ${lbCurMeta.height}`);
      if (lbCurMeta.size > 0) parts.push(fmtSize(lbCurMeta.size));
    }
    lbMeta.textContent = parts.join(" · ");
  }
  function setLbReady(ready, url = "", dlName = "") {
    if (lbZoomBtn) {
      if (!ready && document.activeElement === lbZoomBtn) lbCloseBtn?.focus({ preventScroll: true });
      lbZoomBtn.disabled = !ready;
    }
    for (const a of [lbOpenLink, lbDlLink]) {
      if (!a) continue;
      if (ready && url) {
        a.href = url;
        a.removeAttribute("aria-disabled");
        a.tabIndex = 0;
      } else {
        a.href = "#";
        a.setAttribute("aria-disabled", "true");
        a.tabIndex = -1;
      }
    }
    if (lbDlLink) {
      if (ready && dlName) lbDlLink.setAttribute("download", dlName);
      else lbDlLink.removeAttribute("download");
    }
  }
  function clampLbPan() {
    if (lbScale <= 1.005) { lbScale = 1; lbX = 0; lbY = 0; return; }
    const st = lbStage.getBoundingClientRect();
    const bw = lbImg.offsetWidth || 0, bh = lbImg.offsetHeight || 0;
    const maxX = Math.max(0, (bw * lbScale - st.width) / 2 + 24);
    const maxY = Math.max(0, (bh * lbScale - st.height) / 2 + 24);
    lbX = Math.min(maxX, Math.max(-maxX, lbX));
    lbY = Math.min(maxY, Math.max(-maxY, lbY));
  }
  function applyLbTransform(immediate = false) {
    clampLbPan();
    const zoomed = lbScale > 1.005;
    lightbox.classList.toggle("is-zoomed", zoomed);
    if (immediate) lightbox.classList.add("no-anim");
    lbImg.style.transform = zoomed ? `translate3d(${lbX.toFixed(1)}px, ${lbY.toFixed(1)}px, 0) scale(${lbScale.toFixed(3)})` : "";
    if (immediate) requestAnimationFrame(() => lightbox.classList.remove("no-anim"));
    if (lbZoomBtn) {
      lbZoomBtn.classList.toggle("zoomed", zoomed);
      lbZoomBtn.setAttribute("aria-pressed", String(zoomed));
      const label = zoomed ? "缩回适屏 (1x)" : "放大查看细节";
      lbZoomBtn.title = label;
      lbZoomBtn.setAttribute("aria-label", label);
    }
    if (lbZoomPct) {
      lbZoomPct.textContent = !zoomed ? "1x" : (Math.abs(lbScale - Math.round(lbScale)) < 0.06 ? `${Math.round(lbScale)}x` : `${lbScale.toFixed(1)}x`);
    }
  }
  function targetLbZoomScale() {
    const bw = lbImg.offsetWidth || 0, nw = lbImg.naturalWidth || 0;
    if (bw > 0 && nw > 0 && !lbImg.classList.contains("is-svg")) {
      const ratio = nw / bw;
      if (narrow()) return Math.max(2, Math.min(ratio > 1.2 ? ratio : 2.4, 3));
      return ratio >= 1.35 ? Math.min(ratio, 4) : 2;
    }
    return 2;
  }
  function zoomLbTo(nextScale, clientX, clientY, immediate = false) {
    const clamped = Math.min(6, Math.max(1, nextScale));
    if (clamped <= 1.005) {
      lbScale = 1; lbX = 0; lbY = 0;
      applyLbTransform(immediate);
      return;
    }
    if (typeof clientX === "number" && typeof clientY === "number") {
      const r = lbStage.getBoundingClientRect();
      const ox = clientX - (r.left + r.width / 2);
      const oy = clientY - (r.top + r.height / 2);
      const factor = clamped / lbScale;
      lbX = ox - (ox - lbX) * factor;
      lbY = oy - (oy - lbY) * factor;
    }
    lbScale = clamped;
    applyLbTransform(immediate);
  }
  function toggleLbZoom(clientX, clientY) {
    if (lbImg.hidden || lbZoomBtn?.disabled) return;
    if (lbScale > 1.005) zoomLbTo(1);
    else zoomLbTo(targetLbZoomScale(), clientX, clientY);
  }
  function collectGallery(triggerEl, fallbackItem) {
    const scope = triggerEl?.closest(".rv-md") || triggerEl?.closest(".fv-md") || triggerEl?.closest(".mem-body") || triggerEl?.closest(".msg") || triggerEl?.closest(".thumbs");
    if (!scope) return { items: [fallbackItem], index: 0 };
    const seen = new Set(), items = [];
    const add = (it) => {
      const key = it.path ? "p:" + it.path : (it.url ? "u:" + it.url : "");
      if (!key || seen.has(key)) return;
      seen.add(key);
      items.push(it);
    };
    for (const el of scope.querySelectorAll(".thumb[data-img], .md-img-btn[data-img-url], a.fpath[data-path], img")) {
      if (el.classList.contains("thumb") && el.dataset.img) {
        add({ path: el.dataset.img, alt: el.dataset.imgAlt || "" });
      } else if (el.classList.contains("md-img-btn") && el.dataset.imgUrl) {
        add({ url: el.dataset.imgUrl, alt: el.dataset.imgAlt || "" });
      } else if (el.matches("a.fpath[data-path]")) {
        const p = el.dataset.path || "";
        if (IMG_EXT.test(p)) add({ path: p, alt: "" });
      } else if (el.tagName === "IMG" && !el.closest(".thumb, .md-img-btn, .avatar")) {
        const u = el.currentSrc || el.getAttribute("src") || "";
        if (u) add({ url: u, alt: el.getAttribute("alt") || "" });
      }
    }
    const wantKey = fallbackItem.path ? "p:" + fallbackItem.path : "u:" + (fallbackItem.url || "");
    let index = items.findIndex((it) => (it.path ? "p:" + it.path : "u:" + (it.url || "")) === wantKey);
    if (index < 0) { items.push(fallbackItem); index = items.length - 1; }
    return { items, index };
  }
  async function showLbSlide(idx) {
    if (!lbItems.length) return;
    lbIdx = ((idx % lbItems.length) + lbItems.length) % lbItems.length;
    const item = lbItems[lbIdx], seq = ++lbSeq;
    releaseLbTempUrl();
    lbScale = 1; lbX = 0; lbY = 0;
    applyLbTransform(true);
    const multi = lbItems.length > 1;
    lbPrevBtn.hidden = !multi;
    lbNextBtn.hidden = !multi;
    lbCount.hidden = !multi;
    if (multi) lbCount.textContent = `${lbIdx + 1} / ${lbItems.length}`;

    const name = cleanImgName(item.path || item.url, item.alt || "图片");
    lbName.textContent = item.alt && !item.path ? item.alt : name;
    lbCurMeta = null;
    updateLbMeta();
    setLbReady(false);

    const isSvg = /\.svg(?:$|[?#])/i.test(item.path || item.url || "");
    lbImg.classList.toggle("is-svg", isSvg);
    lbImg.alt = item.alt || name;

    const showErr = (msg) => {
      if (seq !== lbSeq) return;
      lbImg.hidden = true;
      lbImg.removeAttribute("src");
      lbStatus.className = "lb-status";
      lbStatus.textContent = msg;
      lbStatus.hidden = false;
      setLbReady(false);
    };
    const showUrl = (u, meta) => {
      if (seq !== lbSeq) return;
      lbCurMeta = { ...meta, isSvg, width: 0, height: 0 };
      updateLbMeta();
      lbImg.onload = () => {
        if (seq !== lbSeq) return;
        lbStatus.hidden = true;
        lbImg.hidden = false;
        if (lbCurMeta) {
          lbCurMeta.width = lbImg.naturalWidth || 0;
          lbCurMeta.height = lbImg.naturalHeight || 0;
          updateLbMeta();
        }
        setLbReady(true, u, name);
      };
      lbImg.onerror = () => showErr("这张图片加载失败了。");
      lbImg.src = u;
    };

    // 1. Direct remote/static URL (e.g. Markdown ![alt](https://...))
    if (!item.path && item.url) {
      lbImg.hidden = true;
      lbStatus.className = "lb-status loading";
      lbStatus.textContent = "加载图片中…";
      lbStatus.hidden = false;
      showUrl(item.url, {});
      return;
    }

    // 2. Cached in `thumbs`: instant display, zero refetch, never revoked on close.
    const cached = thumbs.get(item.path);
    if (cached) {
      lbStatus.hidden = true;
      const m = thumbMeta.get(item.path) || {};
      showUrl(cached, { size: m.size || 0, machine: m.machine ?? fileContextMachine() ?? "" });
      return;
    }

    // 3. Cross-machine fetch via `findFile` (hub / node / remote hub Bearer).
    lbImg.hidden = true;
    lbImg.removeAttribute("src");
    lbStatus.className = "lb-status loading";
    lbStatus.textContent = "读取原图中…";
    lbStatus.hidden = false;
    const tries = fileTries();
    let f = null;
    try {
      f = await findFile(item.path, tries);
    } catch (err) {
      showErr(err.message || "打不开这张图片。");
      return;
    }
    if (seq !== lbSeq) return;
    if (!f) {
      showErr(`在${tries.length > 1 ? "各台电脑" : "这台电脑"}上都没找到这张图片。`);
      return;
    }
    if (f.kind !== "image") {
      showErr(f.size > 20 << 20 ? `这张图片有 ${fmtSize(f.size)}（超过 20 MB），没法直接预览。` : "这个文件不是受支持的图片格式。");
      return;
    }
    const blobUrl = URL.createObjectURL(new Blob([f.blob], { type: imgMime(item.path) }));
    // If a `.thumb` in the DOM is waiting on this exact path, share it into `thumbs`.
    if (document.querySelector(`.thumb[data-img="${CSS.escape(item.path)}"]`)) {
      thumbs.set(item.path, blobUrl);
      thumbMeta.set(item.path, { size: f.size, machine: f.machine, path: f.path });
      for (const el of document.querySelectorAll(".thumb[data-img] img")) {
        if (el.parentElement.dataset.img === item.path) { el.src = blobUrl; el.parentElement.classList.remove("broken"); }
      }
    } else {
      lbTempUrl = blobUrl;
    }
    showUrl(blobUrl, { size: f.size, machine: f.machine });
  }
  function openLightbox(items, index = 0) {
    if (!lightbox || !items?.length) return;
    const sc = viewScroller(view);
    lbSavedScroll = sc ? { sc, top: sc.scrollTop } : null;
    lbItems = items;
    lightbox.removeAttribute("data-closing");
    if (!lightbox.open) {
      markProgrammaticScroll(150);
      lightbox.showModal();
      if (lbSavedScroll?.sc && lbSavedScroll.sc.scrollTop !== lbSavedScroll.top) {
        lbSavedScroll.sc.scrollTop = lbSavedScroll.top;
      }
    }
    showLbSlide(index);
  }
  async function closeLightbox() {
    if (!lightbox?.open || lightbox.hasAttribute("data-closing")) return;
    markProgrammaticScroll(180);
    if (!supportsTopLayerExit()) {
      lightbox.setAttribute("data-closing", "");
      const anims = lightbox.getAnimations({ subtree: true });
      if (anims.length) {
        await Promise.race([
          Promise.allSettled(anims.map((a) => a.finished)),
          new Promise((r) => setTimeout(r, 240)),
        ]);
      }
      if (!lightbox.hasAttribute("data-closing")) return;
      lightbox.removeAttribute("data-closing");
    }
    if (lightbox.open) lightbox.close();
  }
  if (lightbox) {
    // Fallback for browsers without native <dialog closedby="any"> support.
    if (!("closedBy" in HTMLDialogElement.prototype)) {
      lightbox.addEventListener("click", (ev) => {
        if (ev.target !== lightbox) return;
        const r = lightbox.getBoundingClientRect();
        const inside = r.top <= ev.clientY && ev.clientY <= r.top + r.height && r.left <= ev.clientX && ev.clientX <= r.left + r.width;
        if (!inside) closeLightbox();
      });
    }
    lightbox.addEventListener("cancel", (ev) => {
      markProgrammaticScroll(180);
      if (!supportsTopLayerExit()) {
        ev.preventDefault();
        closeLightbox();
      }
    });
    lightbox.addEventListener("close", () => {
      lightbox.returnValue = "";
      lightbox.removeAttribute("data-closing");
      lightbox.classList.remove("is-zoomed", "is-dragging", "no-anim");
      lbSeq++;
      releaseLbTempUrl();
      lbImg.onload = lbImg.onerror = null;
      lbImg.removeAttribute("src");
      lbImg.hidden = true;
      lbStatus.hidden = true;
      lbScale = 1; lbX = 0; lbY = 0;
      if (lbSavedScroll?.sc && lbSavedScroll.sc.scrollTop !== lbSavedScroll.top) {
        markProgrammaticScroll(120);
        lbSavedScroll.sc.scrollTop = lbSavedScroll.top;
      }
      lbSavedScroll = null;
    });
    lbCloseBtn?.addEventListener("click", () => closeLightbox());
    lbZoomBtn?.addEventListener("click", () => toggleLbZoom());
    lbOpenLink?.addEventListener("click", (ev) => {
      if (lbOpenLink.getAttribute("aria-disabled") === "true") { ev.preventDefault(); return; }
      if (lbTempUrl && lbOpenLink.href === lbTempUrl) lbTabProtected.add(lbTempUrl);
    });
    lbDlLink?.addEventListener("click", (ev) => {
      if (lbDlLink.getAttribute("aria-disabled") === "true") ev.preventDefault();
    });
    lbPrevBtn?.addEventListener("click", () => showLbSlide(lbIdx - 1));
    lbNextBtn?.addEventListener("click", () => showLbSlide(lbIdx + 1));
    document.addEventListener("keydown", (ev) => {
      if (!lightbox.open || lightbox.hasAttribute("data-closing") || ev.metaKey || ev.ctrlKey || ev.altKey) return;
      if (ev.key === "ArrowLeft" && lbItems.length > 1) { ev.preventDefault(); showLbSlide(lbIdx - 1); }
      else if (ev.key === "ArrowRight" && lbItems.length > 1) { ev.preventDefault(); showLbSlide(lbIdx + 1); }
      else if (ev.key === "+" || ev.key === "=") { ev.preventDefault(); if (!lbImg.hidden) zoomLbTo(lbScale * 1.35); }
      else if (ev.key === "-") { ev.preventDefault(); if (!lbImg.hidden) zoomLbTo(lbScale / 1.35); }
      else if (ev.key === "0") { ev.preventDefault(); zoomLbTo(1); }
    });
    lightbox.addEventListener("wheel", (ev) => {
      if (lbImg.hidden || lbZoomBtn?.disabled) return;
      ev.preventDefault();
      const factor = Math.exp(-ev.deltaY * 0.0022);
      zoomLbTo(lbScale * factor, ev.clientX, ev.clientY, true);
    }, { passive: false });

    // Pointer gestures on stage: backdrop light-dismiss, click-to-zoom, 1-finger pan, 2-finger pinch & 1x swipe.
    const ptrs = new Map();
    let gesture = null;
    lbStage.addEventListener("pointerdown", (ev) => {
      if (ev.button !== 0 && ev.pointerType === "mouse") return;
      ptrs.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
      if (ptrs.size === 2) {
        const [a, b] = [...ptrs.values()];
        gesture = {
          kind: "pinch", moved: true,
          dist: Math.hypot(b.x - a.x, b.y - a.y) || 1,
          scale0: lbScale,
        };
        lightbox.classList.add("is-dragging");
        return;
      }
      gesture = {
        kind: "single", pointerId: ev.pointerId,
        startX: ev.clientX, startY: ev.clientY,
        panX0: lbX, panY0: lbY,
        target: ev.target, moved: false,
      };
      if (lbScale > 1.005 && ev.target === lbImg) {
        try { lbStage.setPointerCapture(ev.pointerId); } catch {}
      }
    });
    lbStage.addEventListener("pointermove", (ev) => {
      if (!ptrs.has(ev.pointerId)) return;
      ptrs.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
      if (ptrs.size === 2 && gesture?.kind === "pinch" && !lbImg.hidden) {
        const [a, b] = [...ptrs.values()];
        const dist = Math.hypot(b.x - a.x, b.y - a.y) || 1;
        const midX = (a.x + b.x) / 2, midY = (a.y + b.y) / 2;
        zoomLbTo(gesture.scale0 * (dist / gesture.dist), midX, midY, true);
        return;
      }
      if (!gesture || gesture.kind !== "single" || gesture.pointerId !== ev.pointerId) return;
      const dx = ev.clientX - gesture.startX, dy = ev.clientY - gesture.startY;
      if (!gesture.moved && Math.hypot(dx, dy) > 5) {
        gesture.moved = true;
        if (lbScale > 1.005) lightbox.classList.add("is-dragging");
      }
      if (gesture.moved && lbScale > 1.005) {
        lbX = gesture.panX0 + dx;
        lbY = gesture.panY0 + dy;
        applyLbTransform(true);
      }
    });
    const endPtr = (ev, cancelled = false) => {
      ptrs.delete(ev.pointerId);
      if (ptrs.size === 0) lightbox.classList.remove("is-dragging");
      if (!gesture || cancelled) { if (!ptrs.size) gesture = null; return; }
      if (gesture.kind === "single" && gesture.pointerId === ev.pointerId) {
        const g = gesture;
        gesture = null;
        const dx = ev.clientX - g.startX, dy = ev.clientY - g.startY;
        if (!g.moved) {
          if (g.target === lbImg && !lbImg.hidden) {
            toggleLbZoom(ev.clientX, ev.clientY);
          } else if (g.target === lbStage && ev.target === lbStage) {
            closeLightbox();
          }
        } else if (lbScale <= 1.005 && lbItems.length > 1 && Math.abs(dx) > 52 && Math.abs(dx) > Math.abs(dy) * 1.6) {
          showLbSlide(lbIdx + (dx < 0 ? 1 : -1));
        }
      }
    };
    lbStage.addEventListener("pointerup", (ev) => endPtr(ev, false));
    lbStage.addEventListener("pointercancel", (ev) => endPtr(ev, true));
  }

  async function openSandboxedHtmlRemote(path, win) {
    try {
      const tries = fileTries();
      let htmlText = null;
      for (const m of tries) {
        const q = `/api/file?path=${encodeURIComponent(path)}${m ? "&machine=" + encodeURIComponent(m) : ""}&as=html`;
        const r = routeReq(q);
        const res = await fetch(r.url, { headers: r.hub ? { Authorization: "Bearer " + r.hub.token } : {}, credentials: r.hub ? "omit" : "same-origin" });
        if (res.status === 404) continue;
        if (!res.ok) { let d = {}; try { d = await res.json(); } catch {} throw new Error(d.error || "打不开这个文件"); }
        htmlText = await res.text();
        break;
      }
      if (htmlText === null) throw new Error("没找到这个 HTML 文件");
      const sb = "allow-scripts allow-forms allow-popups allow-modals allow-downloads";
      if (win && !win.closed) {
        const doc = win.document;
        doc.open();
        doc.write(`<!doctype html><meta charset="utf-8"><title>${esc(path.split("/").pop())}</title><style>html,body,iframe{margin:0;width:100%;height:100%;border:0;display:block}</style><iframe sandbox="${sb}"></iframe>`);
        doc.close();
        const frame = doc.querySelector("iframe");
        if (frame) frame.srcdoc = htmlText;
        return;
      }
      syncFileBar("fv", { langLabel: "HTML" });
      $("#fv-name").textContent = path.split("/").pop();
      $("#fv-path").textContent = path;
      const body = $("#fv-body");
      body.classList.remove("is-code");
      body.innerHTML = `<iframe class="fv-html-sandbox" sandbox="${sb}"></iframe>`;
      body.querySelector("iframe").srcdoc = htmlText;
      showFilePane();
    } catch (err) {
      if (win && !win.closed) try { win.close(); } catch {}
      toast(err.message, "attn");
    }
  }
  document.addEventListener("click", (e) => {
    const t = e.target.closest(".thumb[data-img]");
    if (t) {
      e.preventDefault();
      const { items, index } = collectGallery(t, { path: t.dataset.img, alt: t.dataset.imgAlt || "" });
      openLightbox(items, index);
      return;
    }
    const mb = e.target.closest(".md-img-btn[data-img-url]");
    if (mb) {
      e.preventDefault();
      const { items, index } = collectGallery(mb, { url: mb.dataset.imgUrl, alt: mb.dataset.imgAlt || "" });
      openLightbox(items, index);
      return;
    }
    const a = e.target.closest("a.fpath");
    if (a) {
      const path = a.dataset.path || "";
      if (HTML_EXT.test(path)) {
        const ctxMachine = (view === "file" ? rvCur?.machine : fileContextMachine()) || "";
        const hub = hubOf(ctxMachine);
        if (!hub && (!window.__collieDemo || window.__collieDemoHtmlUrl?.(path))) {
          a.href = htmlFileURL(path, ctxMachine);
          a.target = "_blank";
          a.rel = "noopener";
          return;
        }
        e.preventDefault();
        const win = window.open("", "_blank");
        if (win) { try { win.opener = null; } catch {} }
        openSandboxedHtmlRemote(path, win);
        return;
      }
      e.preventDefault();
      if (IMG_EXT.test(path)) {
        const { items, index } = collectGallery(a, { path });
        openLightbox(items, index);
        return;
      }
      if (view === "file") {
        location.hash = wideFileHash(path, rvCur?.machine || "");
        return;
      }
      openFile(path);
      return;
    }
    const wideImg = e.target.closest(".rv-img img[data-wide-img]");
    if (wideImg) {
      e.preventDefault();
      openLightbox([{ path: wideImg.dataset.wideImg, alt: wideImg.getAttribute("alt") || "" }], 0);
      return;
    }
    const rawImg = e.target.closest(".msg img, .mem-body img, .fv-md img, .rv-md img");
    if (rawImg && !rawImg.closest(".avatar, a[href]")) {
      const u = rawImg.currentSrc || rawImg.getAttribute("src") || "";
      if (u) {
        e.preventDefault();
        const { items, index } = collectGallery(rawImg, { url: u, alt: rawImg.getAttribute("alt") || "" });
        openLightbox(items, index);
      }
    }
  });
  // Wide screens: a panel to the right of the conversation, which stays
  // usable; its width is dragged and remembered. Narrow ones: full screen.
  const pane = $("#file-view");
  const setFileW = (w) => document.documentElement.style.setProperty("--file-w", Math.round(w) + "px");
  // The widest the panel may be.
  const fileMax = () => innerWidth - $(".side").offsetWidth - MAIN_MIN;
  let paneTimer;
  function showFilePane() {
    clearTimeout(paneTimer);
    // Until it has been dragged: a little under half the window.
    setFileW(Math.max(Math.min(store.get("fileW", 0) || innerWidth * 0.45, fileMax()), 360));
    if (pane.hidden) { pane.hidden = false; pane.offsetWidth; }
    $("#app").classList.add("has-file");
  }
  function closeFilePane() {
    $("#app").classList.remove("has-file");
    fileSeq++;
    clearTimeout(paneTimer);
    paneTimer = setTimeout(() => {
      pane.hidden = true;
      if (fileUrl) URL.revokeObjectURL(fileUrl), fileUrl = "";
    }, 360);
  }
  pane.addEventListener("click", (e) => { if (e.target.closest("[data-fv-close]")) closeFilePane(); });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !pane.hidden && !document.querySelector("dialog[open]")) closeFilePane(); });
  let fileDragW = 0;
  dragGrip($("#fv-grip"), {
    onStart: fileMax,
    onMove: (x, max) => setFileW(fileDragW = Math.max(Math.min(innerWidth - x, max), 360)),
    onEnd: () => store.set("fileW", Math.round(fileDragW)),
  });

  // Minimal, safe Markdown: escape first, then add a little structure.
  const mdCache = new Map();
  function md(src) {
    const key = String(src || "");
    let out = mdCache.get(key);
    if (out === undefined) {
      out = mdRender(key);
      if (mdCache.size > 8000) mdCache.delete(mdCache.keys().next().value);
      mdCache.set(key, out);
    }
    return out;
  }
  // A bare link: ASCII URL characters up to whitespace, tags or non-ASCII text
  // and punctuation, shown whole without its scheme ([name](url) is better
  // still: the name shows).
  const URL_RE = /(?<![A-Za-z0-9_\/])https?:\/\/[^\x00-\x20<>\x7f-\uffff]+/g;
  const unesc = (s) => s.replace(/&(amp|lt|gt|quot|#39);/g, (m) => ({ "&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": '"', "&#39;": "'" }[m]));
  const urlLabel = (u) => u.replace(/^https?:\/\//, "").replace(/\/$/, "");
  function parseInlineCodePath(raw) {
    if (!raw || raw.includes("://") || raw.includes("//") || /[\s<>"'`*?;$&|()\\]/.test(raw)) return "";
    const base = raw.replace(/(?:#L\d+(?:-L?\d+)?|:\d+(?:[-:]\d+)?)$/i, "");
    if (!/^(?:~\/(?:[\w.\-+@%~\u4e00-\u9fff]+\/)*|\/(?:[\w.\-+@%~\u4e00-\u9fff]+\/)+)[\w.\-+@%~\u4e00-\u9fff]*[A-Za-z0-9_\u4e00-\u9fff][\w.\-+@%~\u4e00-\u9fff]*\.[A-Za-z][A-Za-z0-9]{0,9}$/.test(base)) {
      return "";
    }
    const segs = (base.startsWith("~/") ? base.slice(2) : base.slice(1)).split("/");
    if (segs.some((seg) => !seg || seg === "." || /^\.{2,}$/.test(seg))) return "";
    return base;
  }
  function isPureHttpUrl(raw) {
    if (!/^https?:\/\/[^\x00-\x20<>"'`]+$/i.test(raw)) return false;
    try {
      const u = new URL(raw);
      if (u.protocol !== "http:" && u.protocol !== "https:") return false;
      const h = u.hostname;
      if (!h || h.startsWith(".") || h.endsWith(".")) return false;
      return h === "localhost" || h.includes(".") || h.startsWith("[") || Boolean(u.port);
    } catch {
      return false;
    }
  }
  // CommonMark ASCII punctuation characters that may be backslash-escaped.
  // inline() runs on HTML-escaped text, so the five escaped entities are
  // matched alongside the remaining 27 literal ASCII punctuation characters.
  const CM_ESC_ENTITY_RE = /^(?:&amp;|&lt;|&gt;|&quot;|&#39;)/;
  const CM_ESC_PUNCT_RE = /^[!#$%()*+,\-.\/:;=?@\[\\\]^_`{|}~]/;
  const isEscapedAt = (s, idx) => {
    let bs = 0;
    for (let k = idx - 1; k >= 0 && s[k] === "\\"; k--) bs++;
    return (bs & 1) === 1;
  };
  const hasUnescapedToken = (s, token) => {
    for (let p = s.indexOf(token); p >= 0; p = s.indexOf(token, p + token.length)) {
      if (!isEscapedAt(s, p)) return true;
    }
    return false;
  };
  // Heuristic for \(...\) and \[...\] so genuine LaTeX formulas render while
  // CommonMark-escaped prose like \(parens\) or \[brackets\] unescapes cleanly.
  function looksLikeLatexMath(raw) {
    const s = String(raw || "").trim();
    if (!s || /https?:\/\//i.test(s)) return false;
    if (/\\(?:[A-Za-z]+|[,;:!{}|%#$&_])/.test(s)) return true;
    if (/[=^_+<>~±×÷≈≠≤≥∞∑∫√∂∇∈∉⊂⊆∪∩∀∃→←↔⇒⇔α-ωΑ-Ω]/.test(s)) return true;
    if (/(?:^|\s)-\s*[A-Za-z0-9(\\]|(?<![A-Za-z])[A-Za-z0-9)\]]\s*-\s*(?:\d+|[A-Za-z](?![A-Za-z])|[(\\]|[A-Za-z]\()/.test(s)) return true;
    if (/^(?:\d+(?:\.\d+)?|[A-Za-z])(?:\s*[\/\*]\s*(?:\d+(?:\.\d+)?|[A-Za-z]))+$/.test(s)) return true;
    if (/^(?:\d+[A-Za-z]['′]?|[A-Za-z]['′]?(?:\s*,\s*[A-Za-z]['′]?)*|[A-Za-z]\([^()\n]+\)|\|[^|\n]+\|)$/.test(s)) return true;
    return false;
  }
  function renderMath(tex, displayMode, inlineWrap = false) {
    const src = String(tex || "").trim();
    if (!src) return "";
    const k = typeof katex !== "undefined" ? katex : (typeof globalThis !== "undefined" ? globalThis.katex : null);
    if (k && typeof k.renderToString === "function") {
      try {
        const html = k.renderToString(src, {
          displayMode: !!displayMode,
          throwOnError: false,
          errorColor: "var(--attn)",
          strict: "ignore",
          trust: false,
          maxSize: 20,
          maxExpand: 1000,
        });
        if (displayMode) {
          return inlineWrap
            ? `<span class="md-math-block" role="region" aria-label="数学公式">${html}</span>`
            : `<div class="md-math-block" tabindex="0" role="region" aria-label="数学公式">${html}</div>`;
        }
        return `<span class="md-math">${html}</span>`;
      } catch {}
    }
    return displayMode
      ? (inlineWrap
        ? `<span class="md-math-block md-math-fallback"><code>${esc(src)}</code></span>`
        : `<div class="md-math-block md-math-fallback"><code>${esc(src)}</code></div>`)
      : `<span class="md-math md-math-fallback"><code>${esc(src)}</code></span>`;
  }
  function extractMathBlocks(text, blocks) {
    const rawLines = text.split("\n");
    const outLines = [];
    for (let i = 0; i < rawLines.length; i++) {
      const line = rawLines[i];
      const trimmed = line.trim();
      if (trimmed.startsWith("$$")) {
        if (trimmed.length > 4 && trimmed.endsWith("$$") && !isEscapedAt(trimmed, trimmed.length - 2)) {
          const inner = trimmed.slice(2, -2);
          if (inner.trim() && !hasUnescapedToken(inner, "$$")) {
            blocks.push(renderMath(inner, true));
            outLines.push(`\u0000${blocks.length - 1}\u0000`);
            continue;
          }
        }
        const afterOpen = trimmed.slice(2);
        if (!hasUnescapedToken(afterOpen, "$$")) {
          let matched = false;
          for (let j = i + 1; j < rawLines.length; j++) {
            const nextTrim = rawLines[j].trim();
            if (!nextTrim || /^\u0000\d+\u0000$/.test(nextTrim)) break;
            const rt = rawLines[j].trimEnd();
            if (rt.endsWith("$$") && !isEscapedAt(rt, rt.length - 2)) {
              const beforeClose = rt.slice(0, -2);
              if (!hasUnescapedToken(beforeClose, "$$")) {
                const expr = [afterOpen, ...rawLines.slice(i + 1, j), beforeClose].join("\n").trim();
                if (expr) {
                  blocks.push(renderMath(expr, true));
                  outLines.push(`\u0000${blocks.length - 1}\u0000`);
                  i = j;
                  matched = true;
                }
              }
              break;
            }
            if (hasUnescapedToken(rawLines[j], "$$")) break;
          }
          if (matched) continue;
        }
      } else if (trimmed.startsWith("\\[")) {
        if (trimmed.length > 4 && trimmed.endsWith("\\]") && !isEscapedAt(trimmed, trimmed.length - 2)) {
          const inner = trimmed.slice(2, -2);
          if (!hasUnescapedToken(inner, "\\]") && looksLikeLatexMath(inner)) {
            blocks.push(renderMath(inner, true));
            outLines.push(`\u0000${blocks.length - 1}\u0000`);
            continue;
          }
        }
        const afterOpen = trimmed.slice(2);
        if (!hasUnescapedToken(afterOpen, "\\]")) {
          let matched = false;
          for (let j = i + 1; j < rawLines.length; j++) {
            const nextTrim = rawLines[j].trim();
            if (!nextTrim || /^\u0000\d+\u0000$/.test(nextTrim)) break;
            const rt = rawLines[j].trimEnd();
            if (rt.endsWith("\\]") && !isEscapedAt(rt, rt.length - 2)) {
              const beforeClose = rt.slice(0, -2);
              if (!hasUnescapedToken(beforeClose, "\\]")) {
                const expr = [afterOpen, ...rawLines.slice(i + 1, j), beforeClose].join("\n").trim();
                if (looksLikeLatexMath(expr)) {
                  blocks.push(renderMath(expr, true));
                  outLines.push(`\u0000${blocks.length - 1}\u0000`);
                  i = j;
                  matched = true;
                }
              }
              break;
            }
            if (hasUnescapedToken(rawLines[j], "\\]")) break;
          }
          if (matched) continue;
        }
      }
      outLines.push(line);
    }
    return outLines.join("\n");
  }
  const findLatexClose = (s, start, closeCh) => {
    for (let j = start; j < s.length; j++) {
      const ch = s[j];
      if (ch === "`") return -1;
      if (ch === "\\") {
        if (s[j + 1] === closeCh) return j;
        j++;
      }
    }
    return -1;
  };
  const findDoubleDollarClose = (s, start) => {
    for (let j = start; j < s.length - 1; j++) {
      if (s[j] === "`") return -1;
      if (s[j] === "\\") { j++; continue; }
      if (s[j] === "$" && s[j + 1] === "$") return j;
    }
    return -1;
  };
  const findDollarClose = (s, start) => {
    let braces = 0;
    for (let j = start; j < s.length; j++) {
      const ch = s[j];
      if (ch === "`") return -1;
      if (ch === "\\") { j++; continue; }
      if (ch === "{") { braces++; continue; }
      if (ch === "}") { if (--braces < 0) return -1; continue; }
      if (ch === "$") {
        if (j === start || braces !== 0) return -1;
        if (s[j + 1] === "$") return -1;
        if (/\s/.test(s[j - 1])) return -1;
        if (j + 1 < s.length && /[A-Za-z0-9_${]/.test(s[j + 1])) return -1;
        return j;
      }
    }
    return -1;
  };
  function mdRender(src) {
    const blocks = [];
    let text = String(src || "").replace(/\r\n?/g, "\n").replace(/[\u0000\u0001]/g, "").replace(/```([^\n`]*)\n([\s\S]*?)(```|$)/g, (_, info, code) => {
      const raw = code.replace(/\n$/, "");
      const lang = detectCodeLang(info, false);
      blocks.push(`<pre><code${lang ? ` data-lang="${lang.id}"` : ""}>${lang ? highlightCode(raw, lang.id) : esc(raw)}</code></pre>`);
      return `\u0000${blocks.length - 1}\u0000`;
    });
    text = extractMathBlocks(text, blocks);
    text = esc(text);
    const inline = (s) => {
      const stash = [];
      const keep = (h) => `\u0001${stash.push(h) - 1}\u0001`;
      let scanned = "";
      for (let i = 0; i < s.length; ) {
        const ch = s[i];
        if (ch === "`") {
          let ticks = 1;
          while (s[i + ticks] === "`") ticks++;
          let close = -1;
          for (let j = i + ticks; j < s.length; j++) {
            if (s[j] !== "`") continue;
            let n = 1;
            while (s[j + n] === "`") n++;
            if (n === ticks) { close = j; break; }
            j += n - 1;
          }
          if (close > i + ticks) {
            let c = s.slice(i + ticks, close);
            if (ticks > 1 && c.length >= 2 && c.startsWith(" ") && c.endsWith(" ") && c.trim()) {
              c = c.slice(1, -1);
            }
            const raw = unesc(c).trim();
            const cleanP = parseInlineCodePath(raw);
            if (cleanP) {
              const ep = esc(cleanP);
              scanned += keep(`<a ${fileLinkAttrs(cleanP, ep, "打开这个文件")}><code class="code-link">${c}</code></a>`);
            } else if (isPureHttpUrl(raw)) {
              const eu = esc(raw);
              scanned += keep(`<a class="url" href="${eu}" target="_blank" rel="noopener noreferrer" title="${eu}"><code class="code-link">${c}</code></a>`);
            } else {
              scanned += keep(`<code>${c}</code>`);
            }
            i = close + ticks;
            continue;
          }
          scanned += s.slice(i, i + ticks);
          i += ticks;
          continue;
        }
        if (ch === "\\" && i + 1 < s.length) {
          const next = s[i + 1];
          if (next === "[" && !(i > 0 && /[A-Za-z0-9_]/.test(s[i - 1]))) {
            const close = findLatexClose(s, i + 2, "]");
            if (close > i + 2 && s[close + 2] !== "(") {
              const raw = unesc(s.slice(i + 2, close));
              if (looksLikeLatexMath(raw)) {
                scanned += keep(renderMath(raw, true, true));
                i = close + 2;
                continue;
              }
            }
          }
          if (next === "(" && !(i > 0 && s[i - 1] === "]")) {
            const close = findLatexClose(s, i + 2, ")");
            if (close > i + 2) {
              const raw = unesc(s.slice(i + 2, close));
              if (looksLikeLatexMath(raw)) {
                scanned += keep(renderMath(raw, false));
                i = close + 2;
                continue;
              }
            }
          }
          const rest = s.slice(i + 1);
          const ent = rest.match(CM_ESC_ENTITY_RE);
          if (ent) {
            scanned += keep(ent[0]);
            i += 1 + ent[0].length;
            continue;
          }
          if (CM_ESC_PUNCT_RE.test(next)) {
            scanned += keep(next);
            i += 2;
            continue;
          }
        }
        if (ch === "$") {
          if (s[i + 1] === "$") {
            const close = findDoubleDollarClose(s, i + 2);
            if (close > i + 2) {
              const raw = unesc(s.slice(i + 2, close));
              if (raw.trim()) {
                scanned += keep(renderMath(raw, true, true));
                i = close + 2;
                continue;
              }
            }
            scanned += "$$";
            i += 2;
            continue;
          }
          if (!(i > 0 && /[A-Za-z0-9_]/.test(s[i - 1])) && i + 1 < s.length && !/\s/.test(s[i + 1])) {
            const close = findDollarClose(s, i + 1);
            if (close > i + 1) {
              const raw = unesc(s.slice(i + 1, close));
              const looksLikeCurrencyProse = !raw.includes("\\") && /^\d[\d,]*(?:\.\d+)?\s+[A-Za-z\u4e00-\u9fff]{2,}/.test(raw);
              if (!looksLikeCurrencyProse) {
                scanned += keep(renderMath(raw, false));
                i = close + 1;
                continue;
              }
            }
          }
        }
        scanned += ch;
        i++;
      }
      let r = scanned
        .replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>")
        .replace(/!\[([^\]\n]*)\]\((https?:\/\/(?:[^()\s"]|\([^()\s"]*\))+)\)/g, (_, alt, u) => {
          const rawU = unesc(u), eu = esc(rawU);
          return keep(`<button type="button" class="md-img-btn" data-img-url="${eu}" data-img-alt="${alt}" aria-label="${alt || "查看大图"}"><img class="md-img" src="${eu}" alt="${alt}" loading="lazy"></button>`);
        })
        .replace(/!\[([^\]\n]*)\]\((?:file:\/\/)?((?:\/|~\/)[^)\s"#]+)(?:#[^)\s"]*)?\)/g, (_, alt, p) => {
          p = unesc(p).replace(/:\d+(?:[-:]\d+)?$/, "");
          try { p = decodeURI(p); } catch {}
          const ep = esc(p);
          if (!IMG_EXT.test(p)) return keep(`<a ${fileLinkAttrs(p, ep, ep)}>${alt || ep}</a>`);
          return keep(`<button type="button" class="thumb md-thumb" data-img="${ep}" data-img-alt="${alt}" aria-label="${alt || esc("查看大图 " + (p.split("/").pop() || "图片"))}"><img class="md-img" alt="${alt}"${thumbs.get(p) ? ` src="${thumbs.get(p)}"` : ""}></button>`);
        })
        .replace(/\[([^\]\n]+)\]\((https?:\/\/(?:[^()\s"]|\([^()\s"]*\))+)\)/g, (_, l, u) => keep(`<a href="${u}" target="_blank" rel="noopener noreferrer">${l}</a>`))
        // [MergeStatus.tsx](file:///…/MergeStatus.tsx#L67-L84): the name, opening the file.
        .replace(/\[([^\]\n]+)\]\((?:file:\/\/)?((?:\/|~\/)[^)\s"#]+)(?:#[^)\s"]*)?\)/g, (_, label, p) => {
          p = unesc(p).replace(/:\d+(?:[-:]\d+)?$/, "");
          try { p = decodeURI(p); } catch {}
          const ep = esc(p);
          return keep(`<a ${fileLinkAttrs(p, ep, ep)}>${label}</a>`);
        })
        .replace(URL_RE, (m) => {
          const raw = unesc(m);
          const cut = raw.search(/[<>"`]/);
          let u = cut < 0 ? raw : raw.slice(0, cut);
          const rest = cut < 0 ? "" : raw.slice(cut);
          let tail = "";
          const count = (x, ch) => x.split(ch).length - 1;
          while (u.length) {
            if (/[.,;:!?']$/.test(u) ||
                (u.endsWith(")") && count(u, "(") < count(u, ")")) ||
                (u.endsWith("]") && count(u, "[") < count(u, "]"))) {
              tail = u.slice(-1) + tail;
              u = u.slice(0, -1);
            } else break;
          }
          if (!/^https?:\/\/[^\/.\s]/.test(u)) return m;
          const eu = esc(u);
          return keep(`<a class="url" href="${eu}" target="_blank" rel="noopener noreferrer" title="${eu}">${esc(urlLabel(u))}</a>`) + esc(tail + rest);
        })
        .replace(FILE_RE, (_, pre, p) => {
          const rawP = unesc(p);
          return `${pre}${keep(`<a ${fileLinkAttrs(rawP, p, "打开这个文件")}>${p}</a>`)}`;
        });
      while (r.includes("\u0001")) r = r.replace(/\u0001(\d+)\u0001/g, (_, n) => stash[+n]);
      return r;
    };
    const out = [];
    let list = null, para = [];
    const flushPara = () => { if (para.length) { out.push(`<p>${para.map(inline).join("<br>")}</p>`); para = []; } };
    const flushList = () => { if (list) { out.push(`<${list.tag}${list.start > 1 ? ` start="${list.start}"` : ""}>${list.items.map((x) => `<li>${inline(x)}</li>`).join("")}</${list.tag}>`); list = null; } };
    // A GFM table: a | row, a |---| row, then more | rows.
    const cells = (row) => row.trim().replace(/^\|/, "").replace(/\|$/, "").split(/(?<!\\)\|/).map((c) => c.trim().replace(/\\\|/g, "|"));
    const isSep = (row) => /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$/.test(row) && row.includes("-");
    const lines = text.split("\n");
    for (let li = 0; li < lines.length; li++) {
      const line = lines[li];
      let m;
      if (line.includes("|") && li + 1 < lines.length && isSep(lines[li + 1]) && lines[li + 1].includes("|")) {
        flushPara(); flushList();
        const head = cells(line);
        const align = cells(lines[li + 1]).map((c) => c.endsWith(":") ? (c.startsWith(":") ? "center" : "right") : "");
        const td = (tag, c, k) => `<${tag}${align[k] ? ` style="text-align:${align[k]}"` : ""}>${inline(c)}</${tag}>`;
        let body = "";
        li += 2;
        while (li < lines.length && lines[li].includes("|") && lines[li].trim()) {
          const r = cells(lines[li]);
          body += `<tr>${head.map((_, k) => td("td", r[k] || "", k)).join("")}</tr>`;
          li++;
        }
        li--;
        out.push(`<div class="md-table" tabindex="0" role="region" aria-label="数据表格"><table><thead><tr>${head.map((c, k) => td("th", c, k)).join("")}</tr></thead><tbody>${body}</tbody></table></div>`);
        continue;
      }
      if (/^\u0000\d+\u0000$/.test(line.trim())) { flushPara(); flushList(); out.push(line.trim()); continue; }
      if (/^\s{0,3}([-*_])(\s*\1){2,}\s*$/.test(line)) { flushPara(); flushList(); out.push("<hr>"); continue; }
      if ((m = line.match(/^(#{1,4})\s+(.*)$/))) { flushPara(); flushList(); const h = Math.min(m[1].length + 1, 4); out.push(`<h${h}>${inline(m[2])}</h${h}>`); continue; }
      if ((m = line.match(/^\s*[-*•]\s+(.*)$/)) || (m = line.match(/^\s*\d+[.)]\s+(.*)$/))) {
        const tag = /^\s*\d/.test(line) ? "ol" : "ul";
        flushPara();
        // "2." after a few bullets carries on the numbering, as the agent wrote it.
        if (!list || list.tag !== tag) { flushList(); list = { tag, items: [], start: parseInt(line) || 1 }; }
        list.items.push(m[1]);
        continue;
      }
      if ((m = line.match(/^&gt;\s?(.*)$/))) { flushPara(); flushList(); out.push(`<blockquote>${inline(m[1])}</blockquote>`); continue; }
      if (!line.trim()) { flushPara(); flushList(); continue; }
      flushList();
      para.push(line);
    }
    flushPara(); flushList();
    return out.join("").replace(/\u0000(\d+)\u0000/g, (_, n) => blocks[+n]);
  }

  // ───────────────────────────── input
  const SEQ = {
    "\r": "enter", "\x7f": "backspace", "\b": "backspace", "\t": "tab", "\x1b": "esc",
    "\x1b[A": "up", "\x1b[B": "down", "\x1b[C": "right", "\x1b[D": "left", "\x1b[Z": "shift+tab",
    "\x1b[H": "home", "\x1b[F": "end", "\x1b[3~": "delete", "\x1b[5~": "pageup", "\x1b[6~": "pagedown",
    "\x1bOA": "up", "\x1bOB": "down", "\x1bOC": "right", "\x1bOD": "left",
  };
  function typeIntoPane(data) {
    if (!current) return;
    if (SEQ[data]) return sendKeys([SEQ[data]]);
    if (data.length === 1 && data.charCodeAt(0) < 27) return sendKeys(["ctrl+" + String.fromCharCode(data.charCodeAt(0) + 96)]);
    sendText(data);
  }
  // Keystrokes go out one request at a time, in order; text or keys typed
  // while one is on its way ride along in the next (a request a round trip
  // each would trail far behind fast typing on a slow link).
  const MAX_KEYS = 64; // the server's limit (sendKeys)
  let sendChain = Promise.resolve(), sendTail = null;
  function sendToPane(kind, body) {
    if (!current) return;
    const url = paneURL(current, kind);
    if (sendTail && sendTail.url === url && !sendTail.started) {
      if (kind === "text") { sendTail.body.text += body.text; return sendTail.done; }
      if (sendTail.body.keys.length + body.keys.length <= MAX_KEYS) { sendTail.body.keys.push(...body.keys); return sendTail.done; }
    }
    const op = { url, body, started: false };
    op.done = sendChain = sendChain.then(() => {
      op.started = true;
      if (sendTail === op) sendTail = null;
      return api(url, { body: op.body });
    }).catch((e) => toast(e.message));
    sendTail = op;
    return op.done;
  }
  const sendKeys = (keys) => sendToPane("keys", { keys });
  const sendText = (text) => sendToPane("text", { text });

  $("#keys").addEventListener("click", (e) => {
    const b = e.target.closest("button");
    if (!b || !current) return;
    if (b.dataset.k) sendKeys([b.dataset.k]); else if (b.dataset.t) sendText(b.dataset.t);
  });
  $("#keys").addEventListener("mousedown", (e) => e.preventDefault());

  // Sizing a box from scratch (height auto, then measure) lays out the whole
  // page around it. Typing that stays within the lines it has, or grows
  // past the cap, needs none of that; only text that overflows or went away.
  function grow(el) {
    const max = innerHeight * 0.4, len = el.value.length, longer = len >= (el._len ?? 0);
    el._len = len;
    if (longer && (el.scrollHeight <= el.clientHeight || el.offsetHeight >= Math.floor(max))) return;
    el.style.height = "auto";
    el.style.height = Math.min(el.scrollHeight, max) + "px";
  }
  // Files go to the machine where the agent runs, then their paths ride
  // along in the message (agents read images and files by path).
  const RETRY_SVG = `<svg viewBox="0 0 24 24"><path d="M20 12a8 8 0 1 1-2.34-5.66"/><path d="M20 4v5h-5"/></svg>`;
  const fmtSize = (n) => n < 1024 ? n + " B" : n < 1 << 20 ? Math.round(n / 1024) + " KB" : (n / 1048576).toFixed(1) + " MB";
  function bindFiles(form, machine) {
    const box = $(".files", form), picker = $('input[type="file"]', form);
    form._files = [];
    const draw = () => {
      box.hidden = !form._files.length;
      box.innerHTML = form._files.map((f, i) => `<span class="file ${f.err ? "err" : f.path ? "" : "busy"}" title="${esc(f.err || f.path || "上传中…")}">
        ${f.thumb ? `<img src="${f.thumb}" alt="">` : `<svg viewBox="0 0 24 24"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg>`}
        <span class="fn">${esc(f.name)}</span><span class="fs">${f.err ? "失败" : f.path ? fmtSize(f.size) : "上传中"}</span>
        ${f.err ? `<button type="button" data-retry="${i}" title="重新上传">${RETRY_SVG}</button>` : ""}<button type="button" data-rm="${i}" title="移除">×</button></span>`).join("");
    };
    const upload = (f) => {
      f.err = "";
      const m = machine();
      const up = routeReq(`/api/uploads?name=${encodeURIComponent(f.name)}${m ? "&machine=" + encodeURIComponent(m) : ""}`);
      f.done = fetch(up.url, {
        method: "POST", body: f.file, credentials: up.hub ? "omit" : "same-origin",
        headers: up.hub ? { Authorization: "Bearer " + up.hub.token } : {},
      }).then(async (res) => {
        const d = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(d.error || res.statusText);
        f.path = d.path;
      }).catch((err) => { f.err = err.message; toast(`${f.name}：${err.message}`, "attn"); }).finally(draw);
    };
    // ready: every file in a message uploaded, the failed ones tried once
    // more; a retry clicked meanwhile is waited for too.
    form._filesReady = async (files) => {
      files.filter((f) => f.err).forEach(upload);
      do await Promise.all(files.map((f) => f.done));
      while (files.some((f) => !f.path && !f.err));
    };
    const add = (list) => {
      for (const file of list) {
        const f = { file, name: file.name || "粘贴的图片.png", size: file.size, thumb: file.type.startsWith("image/") ? URL.createObjectURL(file) : "" };
        upload(f);
        form._files.push(f);
      }
      draw();
    };
    $(".attach", form).addEventListener("click", () => picker.click());
    picker.addEventListener("change", () => { add([...picker.files]); picker.value = ""; });
    box.addEventListener("click", (e) => {
      const again = form._files[e.target.closest("[data-retry]")?.dataset.retry];
      if (again) { upload(again); draw(); return; }
      const i = e.target.closest("[data-rm]")?.dataset.rm;
      if (i == null) return;
      const [f] = form._files.splice(+i, 1);
      if (f?.thumb) URL.revokeObjectURL(f.thumb);
      draw();
    });
    const ta = $("textarea", form);
    ta.addEventListener("paste", (e) => {
      const files = [...(e.clipboardData?.files || [])];
      if (files.length) { e.preventDefault(); add(files); }
    });
    form.addEventListener("dragover", (e) => { if (e.dataTransfer?.types?.includes("Files")) { e.preventDefault(); form.classList.add("drop"); } });
    form.addEventListener("dragleave", () => form.classList.remove("drop"));
    form.addEventListener("drop", (e) => {
      form.classList.remove("drop");
      if (!e.dataTransfer?.files?.length) return;
      e.preventDefault(); add([...e.dataTransfer.files]);
    });
    // A message being sent takes its files along; a failed one puts them back.
    form._takeFiles = () => { const t = form._files; form._files = []; draw(); return t; };
    form._restoreFiles = (files) => { form._files = files.concat(form._files); draw(); };
    form._dropFiles = (files) => files.forEach((f) => f.thumb && URL.revokeObjectURL(f.thumb));
  }
  async function withFiles(form, files, text) {
    if (!files.length) return text;
    await form._filesReady(files);
    if (files.some((f) => f.err)) throw new Error("有文件没传上去，点 ↻ 重传或移除后再发");
    return (text.trim() ? text.trim() + "\n\n" : "") + "附件：\n" + files.map((f) => f.path).join("\n");
  }
  // Images a Collie message points at (a screenshot a worker took) show
  // under it, so you need not open each one.
  const IMG_PATH = /(?:^|[\s(（`"'：:，、,;；>])((?:~|\/)[^\s`"'()（）<>，、,;；]*\.(?:png|jpe?g|gif|webp|svg))(?=$|[\s)）`"'，。、,;；!?！？])/gim;
  function imagesIn(t) {
    const clean = String(t || "").replace(/```[\s\S]*?```/g, " ").replace(/!\[[^\]\n]*\]\([^)\n]+\)/g, " ");
    const ps = [...new Set([...clean.matchAll(IMG_PATH)].map((m) => m[1]))].slice(0, 6);
    return ps.length ? `<span class="thumbs">${ps.map(thumbBtn).join("")}</span>` : "";
  }
  // Markdown as one plain line, for quotes: no marks, list dashes or breaks.
  const plainLine = (t) => (t || "").split("\n").map((l) => l.replace(/^\s*(?:[-*+]|\d+\.|#+|>)\s+/, "").trim()).filter(Boolean).join(" ")
    .replace(/\\([!"#$%&'()*+,\-.\/:;<=>?@\[\\\]^_`{|}~])/g, "$1")
    .replace(/\*\*|__|`/g, "").replace(/\[([^\]]*)\]\([^)]*\)/g, "$1");
  // A user message's attachments: images as thumbnails, other files by name
  // (not server paths).
  const linkUserFiles = (escaped) => escaped.replace(FILE_RE, (_, pre, p) => `${pre}<a ${fileLinkAttrs(unesc(p), p, "打开这个文件")}>${p}</a>`);
  function userText(t) {
    const i = t.lastIndexOf("附件：\n");
    if (i < 0) return withImages(linkUserFiles(esc(t)));
    const paths = t.slice(i + 4).split("\n").map((p) => p.trim()).filter(Boolean);
    const imgs = paths.filter((p) => IMG_EXT.test(p)), rest = paths.filter((p) => !IMG_EXT.test(p));
    const name = (p) => p.split("/").pop().replace(/^[0-9a-f]{6}-/, "");
    return withImages(linkUserFiles(esc(t.slice(0, i).trimEnd())))
      + (imgs.length ? `<span class="thumbs">${imgs.map(thumbBtn).join("")}</span>` : "")
      + (rest.length ? `<span class="att">${rest.map((p) => `<a ${fileLinkAttrs(p, esc(p), esc(p))}>📎 ${esc(name(p))}</a>`).join("")}</span>` : "");
  }

  function bindComposer(form, input, send, machine, onEmpty) {
    if (machine) bindFiles(form, machine);
    input.addEventListener("input", () => grow(input));
    input.addEventListener("keydown", (e) => {
      if (e.key !== "Enter" || e.shiftKey || composing(e)) return;
      const mod = e.metaKey || e.ctrlKey;
      // "mod": Enter makes a new line and ⌘/Ctrl+Enter sends.
      if (prefs.sendKey() === "mod" ? mod : !mod) { e.preventDefault(); form.requestSubmit(); }
    });
    // Sending never waits on the network: the box empties at once and
    // send(draft, ready) shows the message; ready gives its final text (with
    // the files' paths) once the ones before it are out. A failed message
    // goes back into the box, in the order sent, ahead of anything typed since.
    let last = Promise.resolve(), restored = 0;
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      if (!input.value.trim() && !form._files?.length) { onEmpty?.(); return; }
      const draft = input.value, files = form._takeFiles?.() || [];
      input.value = ""; grow(input); restored = 0;
      const prev = last;
      const ready = prev.then(() => withFiles(form, files, draft));
      last = Promise.resolve(send(draft, ready)).then(() => form._dropFiles?.(files), (err) => {
        const v = input.value, head = v.slice(0, restored), tail = v.slice(restored).replace(/^\n/, "");
        const front = head ? head + "\n" + draft : draft;
        input.value = tail.trim() ? front + "\n" + tail : front; grow(input);
        restored = front.length;
        if (files.length) form._restoreFiles(files);
        toast(err.message, "attn");
      });
    });
  }

  bindComposer($("#composer"), $("#input"), (draft, ready) => {
    const id = current;
    if (!id) throw new Error("没有选中会话");
    return trackSend("p:" + id, draft, ready, async (text) => {
      let v = find(id);
      if (!v) {
        // The page may be behind (e.g. the hub restarted); look again before giving up.
        try { setLocalState(await api("/api/state")); renderSide(); } catch {}
        v = find(id) || { pane_id: id, is_agent: false };
      }
      try {
        if (v.is_agent) await api(`/api/agents/${encodeURIComponent(id)}/prompt`, { body: { text } });
        else await api(paneURL(id, "text"), { body: { text, enter: true } });
      } catch (err) {
        throw new Error(err.message.includes("blocked") ? "agent 正在等你确认，先处理上面的确认卡片" : err.message);
      }
    });
  }, () => (current || "").includes("@") ? current.slice(current.lastIndexOf("@") + 1) : "", () => sendKeys(["enter"]));

  // ───────────────────────────── menus
  document.addEventListener("click", (e) => {
    const opener = e.target.closest("[data-menu]");
    $$(".menu").forEach((m) => { if (!opener || m.id !== opener.dataset.menu) m.hidden = true; });
    if (opener) { const m = $("#" + opener.dataset.menu); m.hidden = !m.hidden; }
  });

  document.addEventListener("click", async (e) => {
    const act = e.target.closest("[data-act]")?.dataset.act;
    if (!act) return;
    const v = find(current);
    try {
      if (act === "interrupt") sendKeys(["ctrl+c"]);
      if (act === "copy" && v) { await copy(v.pane_id); toast("已复制 " + v.pane_id); }
      if (act === "shell-here" && v) {
        const r = await api("/api/agents", { body: { kind: "shell", cwd: v.cwd, machine: v.machine || "" } });
        location.hash = "#/p/" + encodeURIComponent(r.pane_id);
      }
      if (act === "close" && v) {
        if (!(await ask({ title: `关闭工作区「${v.workspace_label || v.workspace_id}」？`, text: "里面的进程会被结束。", ok: "关闭", warn: true }))) return;
        await api(`/api/workspaces/${encodeURIComponent(v.workspace_id)}/close`, { method: "POST", body: {} });
        state = { ...state, agents: state.agents.filter((a) => a.workspace_id !== v.workspace_id), shells: state.shells.filter((a) => a.workspace_id !== v.workspace_id) };
        if (store.get("last") === location.hash) store.set("last", "#/collie");
        goBack(true);
      }
      if (act === "router-open") {
        const p = routerPane();
        if (p) location.hash = "#/p/" + encodeURIComponent(p); else toast("Collie 还在休眠；发一条消息就会唤醒它");
      }
      if (act === "router-restart") {
        if (!(await ask({ title: "重启 Collie？", text: "它会读自己的记忆恢复上下文，排队的消息不会丢。", ok: "重启" }))) return;
        await api("/api/super/router/restart", { method: "POST", body: {} });
        toast("Collie 正在重启");
      }
      if (act === "pause") {
        const cur = (await api("/api/super/settings")).settings;
        await api("/api/super/settings", { body: { settings: { ...cur, paused: !cur.paused } } });
        toast(cur.paused ? "已恢复投递" : "已暂停投递");
      }
      if (act === "auto") location.hash = "#/auto";
    } catch (err) { toast(err.message, "attn"); }
  });

  // ───────────────────────────── launcher
  const sheet = $("#launch");
  const cwdInput = $("#l-cwd");
  const nameInput = $("#l-name");
  let nameTouched = false;

  function suggestName() {
    if (!nameTouched) nameInput.value = "";
    // A terminal needs only a computer and a folder.
    const shell = $("input[name=kind]:checked")?.value === "shell";
    $$("#launch .agent-only").forEach((f) => f.hidden = shell);
    $("#launch-go").textContent = shell ? "打开" : "启动";
    if (!shell) fillLaunchModels();
  }
  // Model and effort for a new agent: "" leaves it to the agent's own settings
  // on that computer (what it would run if started by hand).
  let launchSeq = 0;
  let launchWant = null; // the model and effort an alias just picked
  async function fillLaunchModels() {
    const seq = ++launchSeq, kind = $("input[name=kind]:checked")?.value;
    const mSel = $("#l-model");
    const keep = { model: mSel.value, effort: $("#l-effort").value, key: mSel._key };
    const info = await modelsFor(kind, pickMachine);
    if (seq !== launchSeq) return;
    const want = launchWant;
    launchWant = null;
    const same = keep.key === kind + "@" + pickMachine;
    mSel._key = kind + "@" + pickMachine;
    const list = [...(info.models || [])];
    if (want?.model && !list.some((m) => m.id === want.model)) list.unshift({ id: want.model });
    const def = info.default ? shortModel(info.default) : "";
    mSel.innerHTML = `<option value="" data-short="默认模型" data-sub="${esc(info.default ? "现在是 " + info.default : "agent 自己的设置")}">默认${def ? "（" + esc(def) + "）" : ""}</option>` +
      list.map((m) => `<option value="${esc(m.id)}" data-sub="${esc(m.label && m.label !== m.id ? m.id : "")}">${esc(m.label || m.id)}</option>`).join("");
    mSel.value = want ? want.model || "" : same && list.some((m) => m.id === keep.model) ? keep.model : "";
    fillLaunchEfforts(info, want ? want.effort || "" : same ? keep.effort : "");
    syncPick(mSel);
  }
  async function fillLaunchAliases() {
    const sel = $("#l-alias"), want = sel.value, machine = pickMachine;
    const kind = $("input[name=kind]:checked")?.value;
    const list = aliasesOfKind(await aliasesOn(machine), kind);
    if (machine !== pickMachine || kind !== $("input[name=kind]:checked")?.value) return;
    sel.innerHTML = aliasOpts(list, list.some((a) => a.name === want) ? want : "");
    sel.hidden = list.length === 0;
    syncPick(sel);
  }
  $("#l-alias").addEventListener("change", async () => {
    const a = (await aliasesOn(pickMachine)).find((x) => x.name === $("#l-alias").value);
    if (!a) return;
    const r = $(`input[name=kind][value="${aliasKind(a.kind)}"]`);
    if (!r || r.disabled) return;
    r.checked = true;
    launchWant = { model: a.model || "", effort: a.effort || "" };
    suggestName();
  });
  function fillLaunchEfforts(info, want) {
    const eSel = $("#l-effort");
    const cur = (info.models || []).find((m) => m.id === $("#l-model").value);
    const effs = effortsOf(info, cur);
    const defE = info.default_effort || "";
    eSel.innerHTML = `<option value="" data-short="思考 · ${esc(EFFORT_LABEL[defE] || defE || "默认")}">默认（${esc(EFFORT_LABEL[defE] || defE || "agent 自己定")}）</option>` +
      effs.map((e) => `<option value="${esc(e)}" data-short="思考 · ${esc(EFFORT_LABEL[e] || e)}">${esc(EFFORT_LABEL[e] || e)}</option>`).join("");
    eSel.value = effs.includes(want) ? want : "";
    eSel.hidden = effs.length === 0;
    syncPick(eSel);
  }
  $("#l-model").addEventListener("change", async () => {
    fillLaunchEfforts(await modelsFor($("input[name=kind]:checked").value, pickMachine), $("#l-effort").value);
  });
  let pickMachine = "";
  const recentKey = () => "recentDirs" + (pickMachine ? ":" + pickMachine : "");
  function renderMachinePick() {
    const ms = (state.machines || []).filter((m) => m.local ? state.ok : m.online);
    $("#l-machine-field").hidden = ms.length < 2;
    if (!ms.some((m) => m.id === pickMachine)) pickMachine = ms[0]?.id || "";
    $("#l-machines").innerHTML = ms.map((m) => `<button type="button" class="${m.id === pickMachine ? "on" : ""}" data-m="${esc(m.id)}">
      <span class="dot online"></span>${esc(m.name)}${m.local ? "<small>hub</small>" : ""}</button>`).join("");
    markKinds();
    fillLaunchAliases();
  }
  // Agents the chosen computer doesn't have can't be picked.
  function markKinds() {
    const have = (state.machines || []).find((m) => m.id === pickMachine)?.agents;
    for (const r of $$("input[name=kind]")) {
      const k = shortKind(r.value);
      const card = r.closest(".kind-card"), small = card.querySelector("small");
      small.dataset.cmd ||= small.textContent;
      const missing = Array.isArray(have) && k in AGENT_LABEL && !have.includes(k);
      r.disabled = missing;
      card.classList.toggle("missing", missing);
      card.title = missing ? "这台电脑上没装 " + AGENT_LABEL[k] : "";
      small.textContent = missing ? "没装" : small.dataset.cmd;
    }
    if ($("input[name=kind]:checked")?.disabled) {
      const first = $$("input[name=kind]").find((r) => !r.disabled);
      if (first) { first.checked = true; first.dispatchEvent(new Event("change")); }
    }
  }
  function resetDir() {
    const recent = store.get(recentKey(), []);
    cwdInput.value = recent[0] || "~";
    $("#recent").innerHTML = recent.slice(0, 6).map((d) => `<button type="button" data-d="${esc(d)}">${esc(d)}</button>`).join("");
    browse(cwdInput.value);
  }
  $("#l-machines").addEventListener("click", (e) => {
    const b = e.target.closest("[data-m]");
    if (!b) return;
    pickMachine = b.dataset.m;
    store.set("machine", pickMachine);
    renderMachinePick();
    resetDir();
    suggestName();
  });
  function openLauncher() {
    nameTouched = false;
    $("#launch-error").textContent = "";
    $("#l-prompt").value = "";
    pickMachine = store.get("machine", "");
    renderMachinePick();
    const recent = store.get(recentKey(), []);
    cwdInput.value = recent[0] || "~";
    $("#recent").innerHTML = recent.slice(0, 6).map((d) => `<button type="button" data-d="${esc(d)}">${esc(d)}</button>`).join("");
    const radio = $(`input[name=kind][value="${store.get("kind", "claude")}"]`);
    if (radio && !radio.disabled) radio.checked = true;
    markKinds();
    suggestName();
    browse(cwdInput.value);
    sheet.showModal();
    if (prefs.notify() && "Notification" in window && Notification.permission === "default") Notification.requestPermission();
  }
  $("#new-btn").addEventListener("click", openLauncher);
  $$("input[name=kind]").forEach((r) => r.addEventListener("change", () => {
    fillLaunchAliases();
    suggestName();
  }));
  nameInput.addEventListener("input", () => { nameTouched = true; });
  $("#recent").addEventListener("click", (e) => { const d = e.target.closest("button")?.dataset.d; if (d) { cwdInput.value = d; browse(d); } });

  let browseTimer;
  async function browse(p) {
    try {
      const r = await api("/api/dirs?path=" + encodeURIComponent(p) + (pickMachine ? "&machine=" + encodeURIComponent(pickMachine) : ""));
      $("#dirs").innerHTML = r.dirs.map((d) => `<li data-d="${esc(d)}">${esc(d)}</li>`).join("");
    } catch { $("#dirs").innerHTML = ""; }
  }
  cwdInput.addEventListener("input", () => { clearTimeout(browseTimer); browseTimer = setTimeout(() => browse(cwdInput.value), 250); });
  $("#dirs").addEventListener("click", (e) => {
    const d = e.target.closest("li")?.dataset.d;
    if (!d) return;
    cwdInput.value = (cwdInput.value.replace(/\/+$/, "") || "~") + "/" + d;
    browse(cwdInput.value);
  });
  $("#dir-up").addEventListener("click", () => {
    const v = cwdInput.value.replace(/\/+$/, "");
    const up = v.includes("/") ? v.slice(0, v.lastIndexOf("/")) || "/" : "~";
    cwdInput.value = up;
    browse(up);
  });
  $("#launch-form").addEventListener("submit", async (e) => {
    if (e.submitter?.value !== "go") return;
    e.preventDefault();
    const kind = $("input[name=kind]:checked").value;
    const shell = kind === "shell";
    const body = shell ? { kind, cwd: cwdInput.value.trim(), machine: pickMachine }
      : { kind, name: nameInput.value.trim(), cwd: cwdInput.value.trim(), prompt: $("#l-prompt").value, machine: pickMachine,
          model: $("#l-model").value, effort: $("#l-effort").value, alias: $("#l-alias").value };
    const go = $("#launch-go");
    go.disabled = true; go.textContent = shell ? "打开中…" : "启动中…";
    $("#launch-error").textContent = "";
    try {
      const r = await api("/api/agents", { body });
      store.set("kind", body.kind);
      store.set(recentKey(), [body.cwd, ...store.get(recentKey(), []).filter((d) => d !== body.cwd)].slice(0, 8));
      sheet.close();
      if (r.warning) toast(r.warning, "attn");
      location.hash = "#/p/" + encodeURIComponent(r.pane_id);
    } catch (err) { $("#launch-error").textContent = err.message; }
    finally { go.disabled = false; go.textContent = shell ? "打开" : "启动"; }
  });

  // ───────────────────────────── viewport (mobile keyboard)
  // The app fills the visible area: its height shrinks above the keyboard, and
  // it follows the visible area when the browser scrolls the page for it.
  function setVH() {
    const v = window.visualViewport;
    const root = document.documentElement.style;
    // Chats and terminals that were showing their latest lines keep showing them as the keyboard takes space.
    const pinned = [...document.querySelectorAll(".viewer, #log, #screen")].filter((el) => el.offsetParent && el.scrollHeight - el.scrollTop - el.clientHeight <= AT_BOTTOM_PX);
    root.setProperty("--vh", (v ? v.height : innerHeight) + "px");
    pinned.forEach((el) => { el.scrollTop = el.scrollHeight; });
    root.setProperty("--vtop", (v ? Math.max(0, v.offsetTop) : 0) + "px");
  }
  window.visualViewport?.addEventListener("resize", setVH);
  window.visualViewport?.addEventListener("scroll", setVH);
  addEventListener("resize", setVH);
  setVH();

  // ───────────────────────────── boot
  function stripLarkJumpParams() {
    if (!location.search) return;
    const q = new URLSearchParams(location.search);
    let dirty = false;
    for (const k of ["lk_mobile_jump_to_browser", "lk_jump_to_browser"]) {
      if (q.has(k)) { q.delete(k); dirty = true; }
    }
    if (!dirty) return;
    const rest = q.toString();
    history.replaceState(history.state, "", location.pathname + (rest ? "?" + rest : "") + location.hash);
  }
  async function boot() {
    stripLarkJumpParams();
    const q = new URLSearchParams(location.search);
    const t = q.get("t"), code = q.get("pair");
    if (t || code) {
      try { if (code) await pair(code); else await login(t); }
      catch (err) { if (code) { $("#login-error").textContent = err.message; } }
      q.delete("t"); q.delete("pair");
      const rest = q.toString();
      history.replaceState(history.state, "", location.pathname + (rest ? "?" + rest : "") + location.hash);
    }
    try { await api("/api/session"); } catch { return; }
    try { sup = normSup(await api("/api/super")); } catch {}
    try { setLocalState(await api("/api/state")); } catch {}
    await loadHubs();
    $("#login").hidden = true;
    $("#app").hidden = false;
    setMode(mode);
    connect();
    booted = false;
    view = undefined;
    route();
    booted = true;
    loadExternal();
    clearInterval(extTimer);
    loadHistory();
    // The hub answers the list from its own copy, so this is cheap.
    let tick = 0;
    extTimer = setInterval(() => { loadExternal(); if (++tick % 3 === 0) loadHistory(); }, 3000);
    offerLocalApp();
  }

  // On a computer that serves Collie at 127.0.0.1 (a node, or the hub's own
  // machine), offer to switch there: Chrome installs and opens it as an app
  // only on a secure origin, which the LAN address is not.
  async function offerLocalApp() {
    const local = ["127.0.0.1", "localhost", "[::1]"].includes(location.hostname);
    if (local || !matchMedia("(pointer: fine)").matches || store.get("local-app-off", false)) return;
    let r;
    try { r = await api("/api/local-app"); } catch { return; }
    if (!r.url) return;
    $("#local-app").hidden = false;
    $("#local-app-open").title = r.url;
    $("#local-app-open").onclick = async (e) => {
      e.preventDefault();
      try {
        // A one-time code signs the new address in, like scanning a QR code.
        const p = await api("/api/pair/new", { body: { kind: "browser" } });
        location.href = `${r.url}/?pair=${encodeURIComponent(p.code)}${location.hash}`;
      } catch (err) { toast(err.message, "attn"); }
    };
    $("#local-app-x").onclick = () => { store.set("local-app-off", true); $("#local-app").hidden = true; };
  }
  boot();
})();

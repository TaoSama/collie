// The workbench (工作台): pages the human pins, grouped in a navigation of
// its own. A web page is embedded when its site allows it, else it is a card
// that opens a new tab; a shortcut opens one of Collie's own pages. Each
// module says what it gives agents: nothing, a read on demand, or a daily
// snapshot the dream turns into memory (workbench.go).
(() => {
  // Shortcut targets → [view, tab, label]; the hub keeps the same list (wbLinks).
  const LINKS = {
    "auto:inbox": ["auto", "inbox", "自动化 · 收件箱"],
    "auto:events": ["auto", "events", "自动化 · 事件"],
    "auto:timers": ["auto", "timers", "自动化 · 定时"],
    "auto:memory": ["auto", "memory", "自动化 · 记忆"],
    "auto:hooks": ["auto", "hooks", "自动化 · 接入"],
    "devices:machines": ["devices", "machines", "设备 · 电脑"],
    "devices:connect": ["devices", "connect", "设备 · 连接"],
  };
  const SHARE = { off: "不给", read: "按需读", keep: "沉淀" };
  const SHARE_HINT = {
    off: "Agent 看不到这个模块",
    read: "Collie 和它派出的 worker 可以用 collie wb read 现取",
    keep: "另外每天存一份快照，做梦时提炼进 Collie 的记忆",
  };
  const ID_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;
  const FRAME_SANDBOX = "allow-scripts allow-same-origin allow-forms allow-popups allow-popups-to-escape-sandbox allow-downloads";
  const FRAME_ALLOW = "geolocation; clipboard-write; fullscreen";
  const DEFAULT_ICON = { web: "🌐", link: "↗" };

  // arrange lists the groups in order, each with its modules in order.
  function arrange(cfg) {
    const groups = (cfg.groups || []).map((g) => ({ ...g, modules: [] }));
    const byId = new Map(groups.map((g) => [g.id, g]));
    for (const m of cfg.modules || []) (byId.get(m.group) || groups[0])?.modules.push(m);
    return groups;
  }

  // freeId makes an id for a new module or group from a URL's host or a
  // title, unused among taken.
  function freeId(seed, taken, fallback = "m") {
    let base = "";
    try { base = new URL(seed).hostname.split(".")[0]; } catch { base = String(seed || ""); }
    base = base.toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 24);
    if (!ID_RE.test(base)) base = fallback;
    let id = base, n = 2;
    while (taken.has(id)) id = `${base}-${n++}`;
    return id;
  }

  // shift moves module id one place within its group (dir -1 or 1).
  function shift(cfg, id, dir) {
    const mods = cfg.modules, i = mods.findIndex((m) => m.id === id);
    if (i < 0) return cfg;
    for (let j = i + dir; j >= 0 && j < mods.length; j += dir) {
      if (mods[j].group === mods[i].group) { [mods[i], mods[j]] = [mods[j], mods[i]]; break; }
    }
    return cfg;
  }

  const host = (u) => { try { return new URL(u).host; } catch { return ""; } };

  function mount(app) {
    const { api, toast, esc, ask, openLink } = app;
    const $ = (s) => document.querySelector(s);
    let cfg = null, cur = "", seq = 0;

    async function load() {
      cfg = await api("/api/wb");
      return cfg;
    }
    const mod = (id) => cfg?.modules.find((m) => m.id === id);
    const icon = (m) => esc(m.icon || DEFAULT_ICON[m.kind] || "•");
    const href = (m) => (m.kind === "link" ? "#" : "#/wb/" + encodeURIComponent(m.id));
    const linkAttr = (m) => (m.kind === "link" ? ` data-wb-link="${esc(m.url)}"` : "");

    function navHTML() {
      return `<div class="wb-nav-home"><a class="wb-item ${cur ? "" : "on"}" href="#/wb"><span class="wb-ico">${window.collieIcons?.svg("layout-dashboard") || "▦"}</span><span>工作台首页</span></a></div>` +
        arrange(cfg).map((g) => `<div class="wb-group">${esc(g.title)}</div>` + g.modules.map((m) =>
          `<a class="wb-item ${cur === m.id ? "on" : ""}" href="${href(m)}"${linkAttr(m)} title="${esc(m.title)}"><span class="wb-ico">${icon(m)}</span><span>${esc(m.title)}</span>${m.share !== "off" ? `<span class="wb-dot" title="给 Agent：${SHARE[m.share]}"></span>` : ""}${m.kind === "web" && m.embed !== "iframe" ? '<span class="wb-out">↗</span>' : ""}</a>`).join("")).join("") +
        `<button type="button" class="wb-item wb-add" data-wb-add><span class="wb-ico">＋</span><span>添加网页</span></button>`;
    }

    function cardMeta(m) {
      if (m.kind === "link") return esc(LINKS[m.url]?.[2] || "Collie");
      const bits = [esc(host(m.url))];
      if (m.share !== "off") bits.push(`给 Agent：${SHARE[m.share]}`);
      if (m.snap_err) bits.push(`<span class="wb-err">快照失败</span>`);
      return bits.join(" · ");
    }

    function overviewHTML() {
      return arrange(cfg).map((g) => `<section class="wb-sec">
        <div class="wb-sec-h"><span>${esc(g.title)}</span>
          <button type="button" class="icon-btn" data-wb-group="${esc(g.id)}" title="分组：改名、排序、删除"><svg viewBox="0 0 24 24"><circle cx="5" cy="12" r="1.4"/><circle cx="12" cy="12" r="1.4"/><circle cx="19" cy="12" r="1.4"/></svg></button></div>
        <div class="wb-cards">${g.modules.map((m) => `<a class="wb-card" href="${href(m)}"${linkAttr(m)}>
          <span class="wb-card-ico">${icon(m)}</span><span class="wb-card-t">${esc(m.title)}</span>
          <span class="wb-card-m">${cardMeta(m)}</span></a>`).join("") || '<p class="empty-hint">还没有模块</p>'}</div></section>`).join("") +
        `<div class="wb-foot"><button type="button" class="btn small" data-wb-add>添加网页</button>
         <button type="button" class="btn small ghost" data-wb-newgroup>新建分组</button></div>`;
    }

    // A module kept by the hub opens through a ticket (workbench_proxy.go):
    // the frame, or a new tab, gets its address when it opens.
    async function enterURL(m) { return (await api("/api/wb/enter", { body: { id: m.id } })).url; }
    async function enterFrame(m) {
      const f = $(".wb-frame");
      try { const url = await enterURL(m); if (f?.isConnected) f.src = url; } catch (err) { toast(err.message, "attn"); }
    }
    async function openTab(m) {
      if (!m.hub_login) { window.open(m.url, "_blank", "noopener,noreferrer"); return; }
      const w = window.open("", "_blank"); // before the await, or it counts as a popup
      if (w) w.opener = null;
      try { const url = await enterURL(m); if (w) w.location = url; } catch (err) { w?.close(); toast(err.message, "attn"); }
    }

    function pageHTML(m) {
      if (m.embed === "iframe") {
        return `<iframe class="wb-frame" ${m.hub_login ? "" : `src="${esc(m.url)}" `}sandbox="${FRAME_SANDBOX}" allow="${FRAME_ALLOW}" referrerpolicy="no-referrer" title="${esc(m.title)}"></iframe>`;
      }
      const snap = m.snap_at ? `最近一次快照 ${new Date(m.snap_at).toLocaleString()}` : "";
      return `<div class="wb-link-card">
        <div class="wb-card-ico big">${icon(m)}</div><h3>${esc(m.title)}</h3>
        <p class="muted">${esc(host(m.url))} 在新标签里打开（网站不允许被嵌入时用这种方式）。</p>
        <button type="button" class="btn primary" data-wb-open>在新标签打开</button>
        <p class="muted wb-share-line">给 Agent：${SHARE[m.share]}${snap ? " · " + esc(snap) : ""}${m.snap_err ? ` · <span class="wb-err">${esc(m.snap_err)}</span>` : ""}</p></div>`;
    }

    function headHTML(m) {
      $("#wb-name").innerHTML = m
        ? `<a class="wb-crumb" href="#/wb" title="回到工作台首页">${window.collieIcons?.svg("layout-dashboard") || ""}<span>工作台</span></a><span class="wb-sep" aria-hidden="true">/</span><span class="wb-cur">${esc(m.title)}</span>`
        : "工作台";
      $("#wb-meta").textContent = m ? [host(m.url), m.embed === "iframe" ? "嵌入" : "新标签打开", ...(m.hub_login ? ["hub 保管登录"] : []), "给 Agent：" + SHARE[m.share]].join(" · ") : "把你关心的网页和页面放在这里，选哪些交给 Agent";
      $("#wb-acts").innerHTML = m ? `
        <button type="button" class="icon-btn" data-wb-open title="在新标签页打开">${window.collieIcons?.svg("maximize-2") || ""}</button>
        ${m.embed === "iframe" ? '<button type="button" class="icon-btn" data-wb-reload title="刷新"><svg viewBox="0 0 24 24"><path d="M20 12a8 8 0 1 1-2.3-5.7M20 4v5h-5"/></svg></button>' : ""}
        ${m.feed ? '<button type="button" class="icon-btn" data-wb-send title="把它现在的数据发给 Collie"><svg viewBox="0 0 24 24"><path d="M12 3l2.2 6.8H21l-5.5 4 2.1 6.7L12 16.4l-5.6 4.1 2.1-6.7L3 9.8h6.8z"/></svg></button>' : ""}
        <button type="button" class="icon-btn" data-wb-edit="${esc(m.id)}" title="设置"><svg viewBox="0 0 24 24"><circle cx="5" cy="12" r="1.4"/><circle cx="12" cy="12" r="1.4"/><circle cx="19" cy="12" r="1.4"/></svg></button>`
        : `<button type="button" class="btn small" data-wb-add>添加网页</button>`;
    }

    function render() {
      const m = mod(cur);
      if (cur && (!m || m.kind === "link")) { location.replace("#/wb"); return; }
      $("#v-wb").classList.toggle("wb-on-page", !!m);
      headHTML(m);
      $("#wb-nav").innerHTML = navHTML();
      const main = $("#wb-main");
      const key = m ? `${m.id}|${m.url}|${m.embed}|${m.hub_login}|${m.share}|${m.snap_at || ""}|${m.snap_err || ""}` : "overview:" + JSON.stringify(cfg);
      if (main._key === key) return;
      main._key = key;
      main.innerHTML = m ? pageHTML(m) : overviewHTML();
      if (m?.hub_login && m.embed === "iframe") enterFrame(m);
    }

    async function open(id) {
      cur = id || "";
      const mine = ++seq;
      if (cfg) render();
      try { await load(); } catch (err) { if (mine === seq) $("#wb-main").innerHTML = `<p class="panel-intro">${esc(err.message)}</p>`; return; }
      if (mine === seq) render();
    }
    function close() {
      seq++;
      const main = $("#wb-main");
      main.innerHTML = ""; main._key = ""; // let an embedded page go
    }

    async function save(next, purge = [], redraw = true) {
      cfg = await api("/api/wb", { body: { groups: next.groups, modules: next.modules, purge } });
      $("#wb-main")._key = "";
      if (redraw) render();
    }
    const clone = () => JSON.parse(JSON.stringify({ groups: cfg.groups, modules: cfg.modules.map(({ has_token, cookies, snap_at, snap_err, ...m }) => m) }));

    // ── the module sheet: add or edit one module
    const dlg = $("#wb-edit");
    let editing = null; // the module being edited, or null for a new one
    function field(name) { return dlg.querySelector(`[name="${name}"]`); }
    function setSeg(name, v) { dlg.querySelectorAll(`[data-seg="${name}"] button`).forEach((b) => b.classList.toggle("on", b.dataset.v === v)); }
    const segVal = (name) => dlg.querySelector(`[data-seg="${name}"] button.on`)?.dataset.v;
    function syncForm() {
      const web = (editing?.kind || "web") === "web";
      dlg.querySelectorAll(".wb-web").forEach((el) => (el.hidden = !web));
      const share = segVal("share") || "off";
      $("#wb-share-hint").textContent = SHARE_HINT[share];
      $("#wb-keep-every").hidden = share !== "keep";
      $("#wb-hub-f").hidden = !web || !cfg?.proxy;
    }
    function editModule(m) {
      editing = m || null;
      $("#wb-edit-title").textContent = m ? "设置 · " + m.title : "添加网页";
      field("title").value = m?.title || "";
      field("icon").value = m?.icon || "";
      field("url").value = m?.url || "";
      field("feed").value = m?.feed || "";
      field("token").value = "";
      field("token").placeholder = m?.has_token ? "已设置，留空不改" : "可选：网站给的只读 token";
      field("clear-token").checked = false;
      $("#wb-clear-row").hidden = !m?.has_token;
      field("sensitive").checked = !!m?.sensitive;
      field("hub-login").checked = !!m?.hub_login;
      field("logout").checked = false;
      $("#wb-logout-row").hidden = !m?.cookies;
      $("#wb-cookies").textContent = m?.cookies || 0;
      field("every").value = m?.every || "";
      field("group").innerHTML = cfg.groups.map((g) => `<option value="${esc(g.id)}">${esc(g.title)}</option>`).join("");
      field("group").value = m?.group || cfg.groups[0]?.id || "";
      field("group").dispatchEvent(new Event("change", { bubbles: true })); // the picker shows it
      setSeg("embed", m?.embed || "tab");
      setSeg("share", m?.share || "off");
      $("#wb-probe-note").textContent = "";
      $("#wb-edit-error").textContent = "";
      $("#wb-del").hidden = $("#wb-up").hidden = $("#wb-down").hidden = !m;
      syncForm();
      dlg.showModal();
    }
    let probeSeq = 0;
    async function probe() {
      const url = field("url").value.trim();
      if (!url) return;
      const mine = ++probeSeq;
      $("#wb-probe-note").textContent = "检测中…";
      try {
        const p = await api("/api/wb/probe", { body: { url } });
        if (mine !== probeSeq || !dlg.open) return; // another URL or module since
        setSeg("embed", p.embed);
        $("#wb-probe-note").textContent = p.embed === "iframe" ? "可以嵌入"
          : `不能嵌入（${p.reason}），会在新标签打开` + (p.same ? "。这像是你自己的站点：让它的响应头带上 frame-ancestors " + location.origin + " 就能嵌进来" : "");
      } catch (err) { if (mine === probeSeq) $("#wb-probe-note").textContent = err.message; }
    }
    async function submit() {
      const next = clone();
      const v = { title: field("title").value.trim(), icon: field("icon").value.trim(), group: field("group").value };
      let id = editing?.id;
      if (!editing || editing.kind === "web") {
        Object.assign(v, {
          kind: "web", url: field("url").value.trim(), embed: segVal("embed") || "tab", feed: field("feed").value.trim(),
          share: segVal("share") || "off", sensitive: field("sensitive").checked, every: Number(field("every").value) || 0,
          hub_login: !!cfg.proxy && field("hub-login").checked,
        });
        if (!v.title) v.title = host(v.url) || "网页";
      }
      const purge = [];
      if (editing) {
        const i = next.modules.findIndex((m) => m.id === id);
        if (editing.share !== "off" && v.share === "off" && await ask({ title: "同时删除它的快照和记忆？", text: "不删的话，已经沉淀的内容会留在 Collie 的记忆里", ok: "删除" })) purge.push(id);
        next.modules[i] = { ...next.modules[i], ...v };
      } else {
        id = freeId(v.url || v.title, new Set(next.modules.map((m) => m.id)));
        next.modules.push({ id, ...v });
      }
      const added = !editing;
      const logout = field("logout").checked;
      await save(next, purge, !logout);
      editing = mod(id); // saved: trying again after a failed token edits it, not a copy
      if (logout) {
        await api("/api/wb/logout", { body: { id } });
        await load();
        render();
      }
      const token = field("token").value.trim(), clear = field("clear-token").checked;
      if (token || clear) {
        await api("/api/wb/token", { body: { id, token: clear ? "" : token } });
        await load();
        render();
      }
      dlg.close();
      if (added) location.hash = "#/wb/" + encodeURIComponent(id);
    }

    async function remove() {
      if (!editing) return;
      if (!(await ask({ title: `移除「${editing.title}」？`, ok: "移除", warn: true }))) return;
      const purge = editing.share !== "off" && await ask({ title: "同时删除它的快照和记忆？", ok: "删除" }) ? [editing.id] : [];
      const next = clone();
      next.modules = next.modules.filter((m) => m.id !== editing.id);
      await save(next, purge);
      dlg.close();
      if (cur === editing.id) location.hash = "#/wb";
    }

    async function groupMenu(gid) {
      const g = cfg.groups.find((x) => x.id === gid);
      const name = await ask({ title: "分组名（清空则删除这个空分组）", value: g.title, ok: "保存" });
      if (name === null) return;
      const next = clone();
      if (!name.trim()) {
        if (next.modules.some((m) => m.group === gid)) { toast("分组里还有模块，先移走它们", "attn"); return; }
        next.groups = next.groups.filter((x) => x.id !== gid);
      } else next.groups.find((x) => x.id === gid).title = name.trim();
      await save(next);
    }
    async function newGroup() {
      const name = await ask({ title: "新分组", value: "", ok: "创建" });
      if (!name?.trim()) return;
      const next = clone();
      next.groups.push({ id: freeId(name, new Set(next.groups.map((g) => g.id)), "g"), title: name.trim() });
      await save(next);
    }

    async function sendToCollie() {
      const m = mod(cur);
      if (!m) return;
      try {
        const r = await api(`/api/super/wb/read/${encodeURIComponent(m.id)}?human=1`);
        await api("/api/super/say", { body: { text: `来自工作台「${m.title}」的数据：\n\n${r.text}` } });
        toast("已发给 Collie");
      } catch (err) { toast(err.message, "attn"); }
    }

    const guard = (fn) => async (...a) => {
      try { await fn(...a); } catch (err) {
        if (dlg.open) $("#wb-edit-error").textContent = err.message;
        toast(err.message, "attn");
      }
    };
    $("#v-wb").addEventListener("click", (e) => {
      const t = e.target.closest("[data-wb-link],[data-wb-add],[data-wb-edit],[data-wb-group],[data-wb-newgroup],[data-wb-send],[data-wb-reload],[data-wb-open]");
      if (!t) return;
      e.preventDefault();
      if (t.dataset.wbLink) openLink(LINKS[t.dataset.wbLink]);
      else if (t.hasAttribute("data-wb-add")) editModule(null);
      else if (t.dataset.wbEdit) editModule(mod(t.dataset.wbEdit));
      else if (t.dataset.wbGroup) guard(groupMenu)(t.dataset.wbGroup);
      else if (t.hasAttribute("data-wb-newgroup")) guard(newGroup)();
      else if (t.hasAttribute("data-wb-send")) sendToCollie();
      else if (t.hasAttribute("data-wb-open")) { const m = mod(cur); if (m) openTab(m); }
      else if (t.hasAttribute("data-wb-reload")) {
        const m = mod(cur), f = $(".wb-frame");
        if (m?.hub_login) enterFrame(m); // the old address was a ticket, used up
        else if (f) f.src = f.src;
      }
    });
    dlg.addEventListener("click", (e) => {
      const b = e.target.closest("[data-seg] button");
      if (b) { b.parentElement.querySelectorAll("button").forEach((x) => x.classList.toggle("on", x === b)); syncForm(); return; }
      if (e.target.closest("#wb-probe")) probe();
      else if (e.target.closest("#wb-del")) guard(remove)();
      else if (e.target.closest("#wb-up, #wb-down")) guard(async () => { await save(shift(clone(), editing.id, e.target.closest("#wb-up") ? -1 : 1)); dlg.close(); })();
      else if (e.target === dlg) dlg.close();
    });
    field("url").addEventListener("change", probe);
    // Through the hub any page can be embedded: its frame headers are Collie's.
    field("hub-login").addEventListener("change", (e) => { if (e.target.checked) setSeg("embed", "iframe"); });
    dlg.querySelector("form").addEventListener("submit", (e) => {
      if (e.submitter?.value !== "save") return;
      e.preventDefault();
      guard(submit)();
    });

    return { open, close, current: () => cur };
  }

  window.collieWorkbench = { arrange, freeId, shift, mount, LINKS, SHARE };
})();

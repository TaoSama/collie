// Generic header status slot renderer for extensions with the header_status
// capability. Pure string rendering (no DOM coupling) so it can be unit-tested
// directly and shared across every session view header.
(() => {
  const TONES = new Set(["ok", "info", "warn", "hot", "danger", "muted"]);
  const ACCENTS = new Set(["claude", "codex", "agy", "gemini"]);
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const toneCls = (t) => (TONES.has(t) ? " hs-" + t : "");
  const accentCls = (a) => (ACCENTS.has(a) ? " hs-brand-" + a : "");
  const clampPct = (v) => (typeof v === "number" && Number.isFinite(v) ? Math.max(0, Math.min(100, Math.round(v))) : null);
  const CHEV_SVG = `<svg class="hs-chev" viewBox="0 0 24 24" aria-hidden="true"><path d="M6 9l6 6 6-6"/></svg>`;
  const CHECK_SVG = `<svg class="hs-check" viewBox="0 0 24 24" aria-hidden="true"><path d="M20 6L9 17l-5-5"/></svg>`;

  function renderIcon(key, opts, cls) {
    if (!key || !opts || !opts.icons || !opts.icons[key]) return "";
    return `<span class="${cls}" aria-hidden="true">${opts.icons[key]}</span>`;
  }

  function actionAttrs(slotId, action) {
    if (!action || !action.name) return "";
    const args = action.args ? JSON.stringify(action.args) : "{}";
    const tip = action.label ? ` title="${esc(action.label)}" aria-label="${esc(action.label)}"` : "";
    return ` data-hs-action="${esc(action.name)}" data-hs-slot="${esc(slotId)}" data-hs-args="${esc(args)}"${tip}`;
  }

  function renderItem(it, showSub, opts) {
    const pct = clampPct(it.percent);
    const tip = it.tooltip ? ` title="${esc(it.tooltip)}"` : "";
    const bar = pct !== null ? `<span class="hs-bar" aria-hidden="true"><span class="hs-fill" style="width:${pct}%"></span></span>` : "";
    const ico = renderIcon(it.icon, opts, "hs-item-ico");
    return `<span class="hs-item${toneCls(it.tone)}${accentCls(it.accent)}"${tip}>`
      + ico
      + (it.label ? `<span class="hs-lab">${esc(it.label)}</span>` : "")
      + bar
      + (it.value ? `<span class="hs-val">${esc(it.value)}</span>` : "")
      + (showSub && it.sub ? `<span class="hs-isub">${esc(it.sub)}</span>` : "")
      + `</span>`;
  }

  function compactFallback(slot) {
    const st = slot.status;
    const title = st?.title || slot.label || slot.name || slot.id;
    if (slot.state === "starting" && !st) return `${title} · 启动中…`;
    if (!st) return `${title} · ${slot.error || "暂不可用"}`;
    if (st.compact) return st.compact;
    const parts = (st.items || []).slice(0, 3).map((it) => [it.label, it.value].filter(Boolean).join(" ")).filter(Boolean);
    if (parts.length) return parts.join(" · ");
    if (st.error) return `${title} · ${st.error}`;
    return title;
  }

  function renderBadges(r) {
    const list = Array.isArray(r.badges) && r.badges.length
      ? r.badges.filter((b) => b && b.text)
      : r.badge ? [{ text: r.badge, tone: "" }] : [];
    if (!list.length) return "";
    return list.map((b) => `<span class="hs-badge${toneCls(b.tone)}">${esc(b.text)}</span>`).join("");
  }

  function renderRow(r, slotId, foldKey, opts) {
    const folded = Array.isArray(r.folded_rows) && r.folded_rows.length ? r.folded_rows : null;
    if (folded) {
      const expanded = !!(opts.expandedFolds && opts.expandedFolds[foldKey]);
      const head = `<button type="button" class="hs-fold-row${expanded ? " is-expanded" : ""}" data-hs-fold-row="${esc(foldKey)}" aria-expanded="${expanded}">`
        + `<span class="hs-fold-row-l">${CHEV_SVG}<span class="hs-healthy-dot" aria-hidden="true"></span><span class="hs-fold-row-t">${esc(r.label)}</span>${r.detail ? `<span class="hs-fold-row-d">${esc(r.detail)}</span>` : ""}</span>`
        + `<span class="hs-fold-row-a">${expanded ? "收起" : "展开"}</span>`
        + `</button>`;
      const children = expanded
        ? `<div class="hs-fold-children">${folded.map((c, idx) => renderRow(c, slotId, `${foldKey}.${idx}`, opts)).join("")}</div>`
        : "";
      return `<div class="hs-fold-group">${head}${children}</div>`;
    }
    const right = r.metric
      ? `<span class="hs-row-metric">${renderItem(r.metric, false, opts)}</span>`
      : r.value ? `<span class="hs-row-v${toneCls(r.tone)}">${esc(r.value)}</span>` : "";
    const selTag = r.selected
      ? `<span class="hs-sel-tag">${CHECK_SVG}<span>顶栏</span></span>`
      : "";
    const badges = renderBadges(r);
    const ico = renderIcon(r.icon, opts, "hs-row-ico");
    const titleBlock = `<span class="hs-row-id">`
      + `<span class="hs-row-title-line">${r.current && !r.selected ? `<span class="hs-cur" aria-hidden="true">▸</span>` : ""}${ico}<span class="hs-row-name" title="${esc(r.label)}">${esc(r.label)}</span>${badges}${selTag}</span>`
      + (r.sublabel ? `<span class="hs-row-sub" title="${esc(r.sublabel)}">${esc(r.sublabel)}</span>` : "")
      + `</span>`;
    const top = `<div class="hs-row-top">${titleBlock}${right}</div>`;
    const meters = (r.meters || []).length
      ? `<div class="hs-meters">${r.meters.map((m) => renderItem(m, true, opts)).join("")}</div>`
      : "";
    const det = r.detail ? `<div class="hs-row-d${toneCls(r.tone)}">${esc(r.detail)}</div>` : "";
    const clickable = !!(r.action && r.action.name);
    const cls = `hs-row${r.current ? " is-cur" : ""}${r.selected ? " is-selected" : ""}${clickable ? " is-action" : ""}${accentCls(r.accent)}`;
    if (clickable) {
      return `<div class="${cls}" role="button" tabindex="0" aria-pressed="${!!r.selected}"${actionAttrs(slotId, r.action)}>${top}${meters}${det}</div>`;
    }
    return `<div class="${cls}">${top}${meters}${det}</div>`;
  }

  function renderChips(chips, slotId) {
    if (!Array.isArray(chips) || !chips.length) return "";
    return `<span class="hs-chips">${chips.map((c) => {
      const attrs = c.action ? actionAttrs(slotId, c.action) : "";
      return `<button type="button" class="hs-chip${c.selected ? " is-selected" : ""}${toneCls(c.tone)}" aria-pressed="${!!c.selected}"${attrs}>${c.selected ? CHECK_SVG : ""}${esc(c.label)}</button>`;
    }).join("")}</span>`;
  }

  function renderSection(sec, slotId, idx, opts) {
    const secKey = `${slotId}:${sec.title || idx}`;
    const collapsible = !!sec.collapsible;
    const override = opts.secState && Object.prototype.hasOwnProperty.call(opts.secState, secKey)
      ? !!opts.secState[secKey]
      : null;
    const collapsed = collapsible ? (override !== null ? override : !!sec.collapsed) : false;
    const icoHtml = renderIcon(sec.icon, opts, "hs-sec-ico");
    const chipsHtml = renderChips(sec.chips, slotId);
    let head = "";
    if (sec.title || sec.meta || chipsHtml) {
      if (collapsible) {
        head = `<button type="button" class="hs-sec-h is-toggle${collapsed ? " is-collapsed" : ""}${accentCls(sec.accent)}" data-hs-sec="${esc(secKey)}" data-hs-sec-default="${sec.collapsed ? "1" : "0"}" aria-expanded="${!collapsed}">`
          + `<span class="hs-sec-l">${CHEV_SVG}${icoHtml}<span class="hs-sec-t">${esc(sec.title || "")}</span></span>`
          + (sec.meta ? `<span class="hs-sec-m">${esc(sec.meta)}</span>` : "")
          + `</button>`;
      } else {
        head = `<div class="hs-sec-h${accentCls(sec.accent)}"><span class="hs-sec-l">${icoHtml}<span class="hs-sec-t">${esc(sec.title || "")}</span></span>${chipsHtml || (sec.meta ? `<span class="hs-sec-m">${esc(sec.meta)}</span>` : "")}</div>`;
      }
    }
    const rows = collapsed
      ? ""
      : `<div class="hs-rows">${(sec.rows || []).map((r, rIdx) => renderRow(r, slotId, `${secKey}:${rIdx}`, opts)).join("")}</div>`;
    return `<div class="hs-sec${collapsed ? " is-collapsed" : ""}">${head}${rows}</div>`;
  }

  function renderPanel(slot, opts) {
    const st = slot.status;
    const title = st?.title || slot.label || slot.name || slot.id;
    const when = opts.when || ((t) => String(t || ""));
    const refreshIcon = opts.refreshIcon || '<svg class="ico" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/></svg>';
    const closeIcon = '<svg class="ico" viewBox="0 0 24 24" aria-hidden="true"><path d="M18 6L6 18M6 6l12 12"/></svg>';
    const errText = st?.error || slot.error || "";
    const updated = st?.updated_at ? when(st.updated_at) : "";
    const secs = (st?.sections || []).map((sec, idx) => renderSection(sec, slot.id, idx, opts)).join("");
    const fallbackItems = !secs && (st?.items || []).length
      ? `<div class="hs-sec"><div class="hs-rows"><div class="hs-row"><div class="hs-meters">${st.items.map((m) => renderItem(m, true, opts)).join("")}</div></div></div></div>`
      : "";
    const empty = !secs && !fallbackItems && !errText
      ? `<div class="hs-empty">${slot.state === "starting" ? "正在启动扩展…" : "暂无状态数据"}</div>`
      : "";
    return `<div class="hs-scrim" data-hs-close${opts.open ? "" : " hidden"}></div>`
      + `<div class="hs-panel${opts.pinned ? " is-pinned" : ""}" role="dialog" aria-label="${esc(title)}"${opts.open ? "" : " hidden"}>`
      + `<div class="hs-ph">`
      + `<span class="hs-ph-t">${slot.icon_url ? `<img class="src-logo" src="${esc(slot.icon_url)}" alt="">` : ""}<b>${esc(title)}</b>${opts.pinned ? `<span class="hs-pin-tag">已固定</span>` : ""}</span>`
      + `<span class="hs-ph-r">${updated ? `<span class="hs-ph-at">${esc(updated)}</span>` : ""}<button type="button" class="icon-btn hs-ref${opts.loading ? " spinning" : ""}" data-hs-refresh="${esc(slot.id)}" aria-label="刷新" title="刷新">${refreshIcon}</button><button type="button" class="icon-btn hs-close" data-hs-close aria-label="关闭" title="关闭">${closeIcon}</button></span>`
      + `</div>`
      + `<div class="hs-body">${errText ? `<div class="hs-err">${esc(errText)}</div>` : ""}${secs}${fallbackItems}${empty}</div>`
      + `</div>`;
  }

  function renderSlot(slot, opts = {}) {
    const st = slot.status;
    const title = st?.title || slot.label || slot.name || slot.id;
    const items = st?.items || [];
    const hasErr = slot.state === "error" || (!!st?.error && !items.length);
    const tone = st?.tone || (hasErr ? "warn" : "");
    const compact = compactFallback(slot);
    const open = opts.openId === slot.id;
    const pinned = opts.pinnedId === slot.id;
    let desktopMain = "";
    if (items.length) {
      desktopMain = `<span class="hs-line">${items.map((it) => renderItem(it, false, opts)).join(`<span class="hs-sep" aria-hidden="true">·</span>`)}</span>`
        + (st.subline ? `<span class="hs-sub">${esc(st.subline)}</span>` : "");
    } else if (slot.state === "starting" && !st) {
      desktopMain = `<span class="hs-line"><span class="dot starting"></span><span class="hs-lab">${esc(title)}</span><span class="hs-val hs-muted">启动中…</span></span>`;
    } else {
      desktopMain = `<span class="hs-line"><span class="hs-lab">${esc(title)}</span><span class="hs-val hs-warn">${esc(st?.error || slot.error || "暂不可用")}</span></span>`;
    }
    const mobileIco = (items[0] && renderIcon(items[0].icon, opts, "hs-item-ico"))
      || (slot.icon_url ? `<img class="src-logo" src="${esc(slot.icon_url)}" alt="">` : "");
    return `<div class="hs-wrap${open ? " is-open" : ""}${pinned ? " is-pinned" : ""}" data-hs-id="${esc(slot.id)}">`
      + `<button type="button" class="hs-trigger${toneCls(tone)}" data-hs-toggle="${esc(slot.id)}" aria-expanded="${open}" aria-haspopup="dialog" title="${esc(title)}">`
      + `<span class="hs-desktop">${desktopMain}</span>`
      + `<span class="hs-mobile">${mobileIco}<span class="hs-mobile-t">${esc(compact)}</span>${CHEV_SVG}</span>`
      + `</button>`
      + renderPanel(slot, {
        open,
        pinned,
        loading: !!opts.loading,
        when: opts.when,
        refreshIcon: opts.refreshIcon,
        icons: opts.icons,
        secState: opts.secState,
        expandedFolds: opts.expandedFolds,
      })
      + `</div>`;
  }

  function render(slots, opts = {}) {
    if (!Array.isArray(slots) || !slots.length) return "";
    return slots.map((s) => renderSlot(s, opts)).join("");
  }

  window.collieHeaderStatus = { render, renderSlot, compactFallback };
})();

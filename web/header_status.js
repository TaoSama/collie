// Generic header status slot renderer for extensions with the header_status
// capability. Pure string rendering (no DOM coupling) so it can be unit-tested
// directly and shared across every session view header.
(() => {
  const TONES = new Set(["ok", "warn", "danger", "muted"]);
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const toneCls = (t) => (TONES.has(t) ? " hs-" + t : "");
  const clampPct = (v) => (typeof v === "number" && Number.isFinite(v) ? Math.max(0, Math.min(100, Math.round(v))) : null);

  function renderItem(it, showSub) {
    const pct = clampPct(it.percent);
    const tip = it.tooltip ? ` title="${esc(it.tooltip)}"` : "";
    const bar = pct !== null ? `<span class="hs-bar" aria-hidden="true"><span class="hs-fill" style="width:${pct}%"></span></span>` : "";
    return `<span class="hs-item${toneCls(it.tone)}"${tip}>`
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

  function renderSection(sec) {
    const head = sec.title || sec.meta
      ? `<div class="hs-sec-h"><span>${esc(sec.title || "")}</span>${sec.meta ? `<span class="hs-sec-m">${esc(sec.meta)}</span>` : ""}</div>`
      : "";
    const rows = (sec.rows || []).map((r) => {
      const top = `<div class="hs-row-top">`
        + `<span class="hs-row-l">${r.current ? `<span class="hs-cur" aria-hidden="true">▸</span>` : ""}<span class="hs-row-name">${esc(r.label)}</span>${r.badge ? `<span class="hs-badge">${esc(r.badge)}</span>` : ""}</span>`
        + (r.value ? `<span class="hs-row-v${toneCls(r.tone)}">${esc(r.value)}</span>` : "")
        + `</div>`;
      const meters = (r.meters || []).length
        ? `<div class="hs-meters">${r.meters.map((m) => renderItem(m, true)).join("")}</div>`
        : "";
      const det = r.detail ? `<div class="hs-row-d">${esc(r.detail)}</div>` : "";
      return `<div class="hs-row${r.current ? " is-cur" : ""}">${top}${meters}${det}</div>`;
    }).join("");
    return `<div class="hs-sec">${head}<div class="hs-rows">${rows}</div></div>`;
  }

  function renderPanel(slot, opts) {
    const st = slot.status;
    const title = st?.title || slot.label || slot.name || slot.id;
    const when = opts.when || ((t) => String(t || ""));
    const refreshIcon = opts.refreshIcon || '<svg class="ico" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/></svg>';
    const errText = st?.error || slot.error || "";
    const updated = st?.updated_at ? when(st.updated_at) : "";
    const secs = (st?.sections || []).map(renderSection).join("");
    const fallbackItems = !secs && (st?.items || []).length
      ? `<div class="hs-sec"><div class="hs-rows"><div class="hs-row"><div class="hs-meters">${st.items.map((m) => renderItem(m, true)).join("")}</div></div></div></div>`
      : "";
    const empty = !secs && !fallbackItems && !errText
      ? `<div class="hs-empty">${slot.state === "starting" ? "正在启动扩展…" : "暂无状态数据"}</div>`
      : "";
    return `<div class="hs-panel" role="dialog" aria-label="${esc(title)}"${opts.open ? "" : " hidden"}>`
      + `<div class="hs-ph">`
      + `<span class="hs-ph-t">${slot.icon_url ? `<img class="src-logo" src="${esc(slot.icon_url)}" alt="">` : ""}<b>${esc(title)}</b></span>`
      + `<span class="hs-ph-r">${updated ? `<span class="hs-ph-at">${esc(updated)}</span>` : ""}<button type="button" class="icon-btn hs-ref${opts.loading ? " spinning" : ""}" data-hs-refresh="${esc(slot.id)}" aria-label="刷新" title="刷新">${refreshIcon}</button></span>`
      + `</div>`
      + (errText ? `<div class="hs-err">${esc(errText)}</div>` : "")
      + secs + fallbackItems + empty
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
    const folded = !!opts.folded;
    let desktopMain = "";
    if (items.length) {
      desktopMain = `<span class="hs-line">${items.map((it) => renderItem(it, false)).join(`<span class="hs-sep" aria-hidden="true">·</span>`)}</span>`
        + (st.subline ? `<span class="hs-sub">${esc(st.subline)}</span>` : "");
    } else if (slot.state === "starting" && !st) {
      desktopMain = `<span class="hs-line"><span class="dot starting"></span><span class="hs-lab">${esc(title)}</span><span class="hs-val hs-muted">启动中…</span></span>`;
    } else {
      desktopMain = `<span class="hs-line"><span class="hs-lab">${esc(title)}</span><span class="hs-val hs-warn">${esc(st?.error || slot.error || "暂不可用")}</span></span>`;
    }
    const chev = `<svg class="hs-chev" viewBox="0 0 24 24" aria-hidden="true"><path d="M6 9l6 6 6-6"/></svg>`;
    const itemText = (it) => [it.label, it.value].filter(Boolean).join(" ");
    const firstVal = items.length >= 2 && items[0].percent == null && items[1].percent != null
      ? `${itemText(items[0])} · ${itemText(items[1])}`
      : items[0] ? itemText(items[0]) : title;
    const mobileText = folded ? firstVal : compact;
    return `<div class="hs-wrap${open ? " is-open" : ""}${folded ? " is-folded" : ""}" data-hs-id="${esc(slot.id)}">`
      + `<button type="button" class="hs-trigger${toneCls(tone)}" data-hs-toggle="${esc(slot.id)}" aria-expanded="${open}" aria-haspopup="dialog" title="${esc(title)}">`
      + `<span class="hs-desktop">${desktopMain}</span>`
      + `<span class="hs-mobile">${slot.icon_url ? `<img class="src-logo" src="${esc(slot.icon_url)}" alt="">` : ""}<span class="hs-mobile-t">${esc(mobileText)}</span>${chev}</span>`
      + `</button>`
      + `<button type="button" class="hs-fold-btn" data-hs-fold aria-label="${folded ? "展开状态栏" : "收起状态栏"}" title="${folded ? "展开状态栏" : "收起状态栏"}">${folded ? "展开" : "收起"}</button>`
      + renderPanel(slot, { open, loading: !!opts.loading, when: opts.when, refreshIcon: opts.refreshIcon })
      + `</div>`;
  }

  function render(slots, opts = {}) {
    if (!Array.isArray(slots) || !slots.length) return "";
    return slots.map((s) => renderSlot(s, opts)).join("");
  }

  window.collieHeaderStatus = { render, renderSlot, compactFallback };
})();

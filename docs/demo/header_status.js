// Generic header status slot renderer & surgical in-place DOM patcher for
// extensions with the header_status capability. Pure string rendering stays
// side-effect-free for unit tests, while patch() updates mounted DOM nodes in
// place so polling refreshes and fold/section toggles never tear down the
// popover/drawer, replay open animations, or reset scroll position.
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
      return `<div class="hs-fold-group" data-hs-key="${esc(foldKey)}">${head}${children}</div>`;
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
      return `<div class="${cls}" data-hs-key="${esc(foldKey)}" role="button" tabindex="0" aria-pressed="${!!r.selected}"${actionAttrs(slotId, r.action)}>${top}${meters}${det}</div>`;
    }
    return `<div class="${cls}" data-hs-key="${esc(foldKey)}">${top}${meters}${det}</div>`;
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
    return `<div class="hs-sec${collapsed ? " is-collapsed" : ""}" data-hs-sec-box="${esc(secKey)}">${head}${rows}</div>`;
  }

  function renderPanelBodyInner(slot, opts) {
    const st = slot.status;
    const errText = st?.error || slot.error || "";
    const secs = (st?.sections || []).map((sec, idx) => renderSection(sec, slot.id, idx, opts)).join("");
    const fallbackItems = !secs && (st?.items || []).length
      ? `<div class="hs-sec"><div class="hs-rows"><div class="hs-row"><div class="hs-meters">${st.items.map((m) => renderItem(m, true, opts)).join("")}</div></div></div></div>`
      : "";
    const empty = !secs && !fallbackItems && !errText
      ? `<div class="hs-empty">${slot.state === "starting" ? "正在启动扩展…" : "暂无状态数据"}</div>`
      : "";
    return `${errText ? `<div class="hs-err">${esc(errText)}</div>` : ""}${secs}${fallbackItems}${empty}`;
  }

  function renderPanel(slot, opts) {
    const st = slot.status;
    const title = st?.title || slot.label || slot.name || slot.id;
    const when = opts.when || ((t) => String(t || ""));
    const refreshIcon = opts.refreshIcon || '<svg class="ico" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/></svg>';
    const closeIcon = '<svg class="ico" viewBox="0 0 24 24" aria-hidden="true"><path d="M18 6L6 18M6 6l12 12"/></svg>';
    const updated = st?.updated_at ? when(st.updated_at) : "";
    return `<div class="hs-scrim" data-hs-close${opts.open ? "" : " hidden"}></div>`
      + `<div class="hs-panel${opts.pinned ? " is-pinned" : ""}" role="dialog" aria-label="${esc(title)}"${opts.open ? "" : " hidden"}>`
      + `<div class="hs-ph">`
      + `<span class="hs-ph-t">${slot.icon_url ? `<img class="src-logo" src="${esc(slot.icon_url)}" alt="">` : ""}<b>${esc(title)}</b><span class="hs-pin-tag"${opts.pinned ? "" : " hidden"}>已固定</span></span>`
      + `<span class="hs-ph-r"><span class="hs-ph-at"${updated ? "" : " hidden"}>${esc(updated)}</span><button type="button" class="icon-btn hs-ref${opts.loading ? " spinning" : ""}" data-hs-refresh="${esc(slot.id)}" aria-label="刷新" title="刷新">${refreshIcon}</button><button type="button" class="icon-btn hs-close" data-hs-close aria-label="关闭" title="关闭">${closeIcon}</button></span>`
      + `</div>`
      + `<div class="hs-body">${renderPanelBodyInner(slot, opts)}</div>`
      + `</div>`;
  }

  function renderDesktopMain(slot, opts) {
    const st = slot.status;
    const title = st?.title || slot.label || slot.name || slot.id;
    const items = st?.items || [];
    if (items.length) {
      return `<span class="hs-line">${items.map((it) => renderItem(it, false, opts)).join(`<span class="hs-sep" aria-hidden="true">·</span>`)}</span>`
        + (st.subline ? `<span class="hs-sub">${esc(st.subline)}</span>` : "");
    }
    if (slot.state === "starting" && !st) {
      return `<span class="hs-line"><span class="dot starting"></span><span class="hs-lab">${esc(title)}</span><span class="hs-val hs-muted">启动中…</span></span>`;
    }
    return `<span class="hs-line"><span class="hs-lab">${esc(title)}</span><span class="hs-val hs-warn">${esc(st?.error || slot.error || "暂不可用")}</span></span>`;
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
    const desktopMain = renderDesktopMain(slot, opts);
    const mobileIco = (items[0] && renderIcon(items[0].icon, opts, "hs-item-ico"))
      || (slot.icon_url ? `<img class="src-logo" src="${esc(slot.icon_url)}" alt="">` : "");
    return `<div class="hs-wrap${open ? " is-open" : ""}${pinned ? " is-pinned" : ""}" data-hs-id="${esc(slot.id)}">`
      + `<button type="button" class="hs-trigger${toneCls(tone)}" data-hs-toggle="${esc(slot.id)}" aria-expanded="${open}" aria-haspopup="dialog" title="${esc(title)}">`
      + `<span class="hs-desktop">${desktopMain}</span>`
      + `<span class="hs-mobile"><span class="hs-mobile-ico">${mobileIco}</span><span class="hs-mobile-t">${esc(compact)}</span>${CHEV_SVG}</span>`
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

  // Lightweight in-place DOM morphing so updating changed numbers, progress bar
  // widths, or toggling fold/section state preserves existing elements, focus,
  // open animations, and scroll positions without flashing.
  function syncAttrs(el, tpl) {
    const nextNames = new Set();
    for (const a of tpl.attributes) {
      nextNames.add(a.name);
      if (el.getAttribute(a.name) !== a.value) el.setAttribute(a.name, a.value);
    }
    for (let i = el.attributes.length - 1; i >= 0; i--) {
      const name = el.attributes[i].name;
      if (!nextNames.has(name)) el.removeAttribute(name);
    }
  }

  function morphChildren(curParent, tplParent) {
    const curNodes = Array.from(curParent.childNodes);
    const tplNodes = Array.from(tplParent.childNodes);
    const keyed = new Map();
    for (const n of curNodes) {
      if (n.nodeType === 1) {
        const k = n.getAttribute("data-hs-sec-box") || n.getAttribute("data-hs-key");
        if (k) keyed.set(k, n);
      }
    }
    let idx = 0;
    for (const tpl of tplNodes) {
      const key = tpl.nodeType === 1 ? (tpl.getAttribute("data-hs-sec-box") || tpl.getAttribute("data-hs-key")) : null;
      let cur = key ? keyed.get(key) : curParent.childNodes[idx];
      if (cur && key && cur !== curParent.childNodes[idx]) {
        curParent.insertBefore(cur, curParent.childNodes[idx] || null);
      }
      if (!cur) {
        curParent.appendChild(tpl.cloneNode(true));
      } else if (cur.nodeType !== tpl.nodeType || cur.nodeName !== tpl.nodeName) {
        curParent.replaceChild(tpl.cloneNode(true), cur);
      } else if (cur.nodeType === 3) {
        if (cur.nodeValue !== tpl.nodeValue) cur.nodeValue = tpl.nodeValue;
      } else if (cur.nodeType === 1) {
        // Skip re-morphing identical SVG/icon subtrees to avoid image decode flashes.
        if (cur.tagName === "svg" || cur.classList?.contains("hs-item-ico") || cur.classList?.contains("hs-sec-ico") || cur.classList?.contains("hs-row-ico")) {
          if (cur.outerHTML !== tpl.outerHTML) {
            curParent.replaceChild(tpl.cloneNode(true), cur);
          }
        } else {
          syncAttrs(cur, tpl);
          morphChildren(cur, tpl);
        }
      }
      idx++;
    }
    while (curParent.childNodes.length > tplNodes.length) {
      curParent.removeChild(curParent.lastChild);
    }
  }

  // patch(container, slots, opts) mounts once and updates every slot in place.
  // opts.anchorSelector optionally preserves the viewport Y offset of a clicked
  // element (e.g. a fold/section toggle button) inside .hs-body across the update.
  function patch(container, slots, opts = {}) {
    if (!container) return;
    if (!Array.isArray(slots) || !slots.length) {
      if (container.childNodes.length) container.textContent = "";
      container.hidden = true;
      return;
    }
    container.hidden = false;

    // Remove wraps for slots that no longer exist.
    const wantIds = new Set(slots.map((s) => s.id));
    for (const child of Array.from(container.children)) {
      const id = child.getAttribute("data-hs-id");
      if (!wantIds.has(id)) child.remove();
    }

    const doc = container.ownerDocument;
    const when = opts.when || ((t) => String(t || ""));
    const parseFrag = (html) => {
      const t = doc.createElement("template");
      t.innerHTML = html;
      return t.content;
    };

    for (let i = 0; i < slots.length; i++) {
      const slot = slots[i];
      let wrap = container.querySelector(`:scope > .hs-wrap[data-hs-id="${slot.id.replace(/"/g, '\\"')}"]`);
      const open = opts.openId === slot.id;
      const pinned = opts.pinnedId === slot.id;
      const slotOpts = {
        open,
        pinned,
        loading: !!opts.loading,
        when,
        refreshIcon: opts.refreshIcon,
        icons: opts.icons,
        secState: opts.secState,
        expandedFolds: opts.expandedFolds,
      };

      const st = slot.status;
      const title = st?.title || slot.label || slot.name || slot.id;
      const items = st?.items || [];
      const hasErr = slot.state === "error" || (!!st?.error && !items.length);
      const tone = st?.tone || (hasErr ? "warn" : "");
      const compact = compactFallback(slot);
      const deskHtml = renderDesktopMain(slot, opts);
      const mobIcoHtml = (items[0] && renderIcon(items[0].icon, opts, "hs-item-ico"))
        || (slot.icon_url ? `<img class="src-logo" src="${esc(slot.icon_url)}" alt="">` : "");
      const bodyHtml = renderPanelBodyInner(slot, slotOpts);

      if (!wrap) {
        const frag = parseFrag(renderSlot(slot, opts));
        wrap = frag.firstElementChild;
        const dEl = wrap.querySelector(".hs-desktop");
        if (dEl) dEl._hsHtml = deskHtml;
        const mEl = wrap.querySelector(".hs-mobile-ico");
        if (mEl) mEl._hsHtml = mobIcoHtml;
        const bEl = wrap.querySelector(".hs-body");
        if (bEl) bEl._hsHtml = bodyHtml;
        if (open) {
          const sc = wrap.querySelector(":scope > .hs-scrim");
          const pn = wrap.querySelector(":scope > .hs-panel");
          if (sc) {
            sc.classList.add("is-entering");
            sc.addEventListener("animationend", () => sc.classList.remove("is-entering"), { once: true });
          }
          if (pn) {
            pn.classList.add("is-entering");
            pn.addEventListener("animationend", () => pn.classList.remove("is-entering"), { once: true });
          }
        }
        container.insertBefore(wrap, container.children[i] || null);
        continue;
      }

      if (container.children[i] !== wrap) {
        container.insertBefore(wrap, container.children[i] || null);
      }

      const wasOpen = wrap.classList.contains("is-open");
      wrap.classList.toggle("is-open", open);
      wrap.classList.toggle("is-pinned", pinned);

      // 1. Update trigger text/classes/bars in place (no teardown).
      const trig = wrap.querySelector(":scope > .hs-trigger");
      if (trig) {
        const trigCls = `hs-trigger${toneCls(tone)}`;
        if (trig.className !== trigCls) trig.className = trigCls;
        const expStr = String(open);
        if (trig.getAttribute("aria-expanded") !== expStr) trig.setAttribute("aria-expanded", expStr);
        if (trig.getAttribute("title") !== title) trig.setAttribute("title", title);

        const deskEl = trig.querySelector(".hs-desktop");
        if (deskEl && deskEl._hsHtml !== deskHtml) {
          morphChildren(deskEl, parseFrag(deskHtml));
          deskEl._hsHtml = deskHtml;
        }
        const mobIcoEl = trig.querySelector(".hs-mobile-ico");
        if (mobIcoEl && mobIcoEl._hsHtml !== mobIcoHtml) {
          mobIcoEl.innerHTML = mobIcoEl._hsHtml = mobIcoHtml;
        }
        const mobTextEl = trig.querySelector(".hs-mobile-t");
        if (mobTextEl && mobTextEl.textContent !== compact) {
          mobTextEl.textContent = compact;
        }
      }

      // 2. Patch panel content BEFORE unhiding on open so the very first paint
      //    already has final layout and content (no empty/flash frame).
      const scrim = wrap.querySelector(":scope > .hs-scrim");
      const panel = wrap.querySelector(":scope > .hs-panel");
      if (!panel) continue;
      panel.classList.toggle("is-pinned", pinned);
      if (panel.getAttribute("aria-label") !== title) panel.setAttribute("aria-label", title);

      // 3. Patch panel header metadata/spinner in place.
      const phTitle = panel.querySelector(".hs-ph-t b");
      if (phTitle && phTitle.textContent !== title) phTitle.textContent = title;
      const pinTag = panel.querySelector(".hs-pin-tag");
      if (pinTag && pinTag.hidden !== !pinned) pinTag.hidden = !pinned;
      const atEl = panel.querySelector(".hs-ph-at");
      if (atEl) {
        const updated = st?.updated_at ? when(st.updated_at) : "";
        if (atEl.textContent !== updated) atEl.textContent = updated;
        if (atEl.hidden !== !updated) atEl.hidden = !updated;
      }
      const refBtn = panel.querySelector(".hs-ref");
      if (refBtn) refBtn.classList.toggle("spinning", !!opts.loading);

      // 4. Patch panel body in place while preserving scrollTop / clicked anchor.
      const body = panel.querySelector(".hs-body");
      if (body && body._hsHtml !== bodyHtml) {
        const prevScroll = body.scrollTop;
        let anchorOffset = null;
        if (wasOpen && open && opts.anchorSelector) {
          const anchorEl = body.querySelector(opts.anchorSelector);
          if (anchorEl) {
            anchorOffset = anchorEl.getBoundingClientRect().top - body.getBoundingClientRect().top;
          }
        }
        morphChildren(body, parseFrag(bodyHtml));
        body._hsHtml = bodyHtml;
        if (wasOpen && open) {
          if (anchorOffset !== null && opts.anchorSelector) {
            const nextAnchor = body.querySelector(opts.anchorSelector);
            if (nextAnchor) {
              const nextOffset = nextAnchor.getBoundingClientRect().top - body.getBoundingClientRect().top;
              const target = prevScroll + (nextOffset - anchorOffset);
              body.scrollTop = Math.max(0, Math.min(body.scrollHeight - body.clientHeight, target));
            } else {
              body.scrollTop = prevScroll;
            }
          } else {
            body.scrollTop = prevScroll;
          }
        }
      }

      // 5. Toggle scrim & panel visibility; add .is-entering BEFORE unhiding so
      //    the enter animation starts on frame 0 without a pre-animation flash.
      if (!wasOpen && open) {
        if (scrim) {
          scrim.classList.add("is-entering");
          scrim.hidden = false;
          scrim.addEventListener("animationend", () => scrim.classList.remove("is-entering"), { once: true });
        }
        panel.classList.add("is-entering");
        panel.hidden = false;
        panel.addEventListener("animationend", () => panel.classList.remove("is-entering"), { once: true });
      } else if (wasOpen && !open) {
        if (scrim) {
          scrim.classList.remove("is-entering");
          scrim.hidden = true;
        }
        panel.classList.remove("is-entering");
        panel.hidden = true;
      } else {
        if (scrim && scrim.hidden !== !open) scrim.hidden = !open;
        if (panel.hidden !== !open) panel.hidden = !open;
      }
    }
  }

  window.collieHeaderStatus = { render, renderSlot, patch, compactFallback };
})();

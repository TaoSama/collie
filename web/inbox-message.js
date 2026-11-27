(() => {
  const { esc, createRenderer } = window.collieMarkdown;
  const markdown = createRenderer({ externalContent: true, detectCodeLang: () => null });
  const marks = { bold: "strong", italic: "em", underline: "u", strike: "s" };

  function httpURL(value) {
    if (typeof value !== "string" || !/^https?:\/\//i.test(value) || /[\x00-\x20\x7f]/.test(value)) return "";
    try {
      const u = new URL(value);
      return !u.username && !u.password ? u.href : "";
    } catch {
      return "";
    }
  }

  const external = (url, body, cls = "") =>
    `<a class="${cls}" href="${esc(url)}" target="_blank" rel="noopener noreferrer">${body}</a>`;

  function media(n) {
    const url = typeof n.url === "string" && n.url.startsWith("/api/inbox/media?") ? esc(n.url) : "";
    if (n.kind === "image") {
      const alt = esc(n.text || "飞书图片");
      const cls = n.tone === "sticker" ? "im-image im-sticker" : "im-image";
      return url ? `<img class="${cls}" src="${url}" alt="${alt}" loading="lazy">` : `<span class="im-note">图片暂不可用</span>`;
    }
    const name = esc(n.text || "飞书文件");
    const bytes = Number(n.tone);
    const size = bytes > 0 && Number.isFinite(bytes)
      ? bytes >= 1048576 ? `${(bytes / 1048576).toFixed(1)} MB` : bytes >= 1024 ? `${(bytes / 1024).toFixed(1)} KB` : `${bytes} B`
      : "";
    const icon = window.collieIcons.svg("file-text");
    const inner = `${icon}<span><strong>${name}</strong><small>${size || (url ? "下载文件" : "文件暂不可用")}</small></span>`;
    return url ? `<a class="im-file" href="${url}" download>${inner}</a>` : `<span class="im-file">${inner}</span>`;
  }

  const containers = {
    group: ["div", "im-group"], columns: ["div", "im-columns"], fields: ["div", "im-fields"],
    actions: ["div", "im-actions"], note: ["div", "im-note"], paragraph: ["p", ""],
    quote: ["blockquote", ""], code: ["code", ""], heading: ["h4", ""],
    list: ["ul", ""], ordered: ["ol", ""], item: ["li", ""],
    row: ["tr", ""], cell: ["td", ""],
  };

  function actionAttrs(n, kind = "card") {
    if (!n.action || !n.sig || n.disabled) return "";
    return ` data-im-act="${esc(kind)}" data-im-id="${esc(n.action)}" data-im-sig="${esc(n.sig)}"` +
      (n.name ? ` data-im-name="${esc(n.name)}"` : "") +
      (n.value ? ` data-im-val="${esc(n.value)}"` : "");
  }

  function renderSelect(n) {
    const opts = Array.isArray(n.options) ? n.options : [];
    const ph = esc(n.placeholder || (n.tone === "person" ? "选择成员" : "请选择"));
    const act = actionAttrs(n);
    const title = n.tone === "person" ? ' title="选人组件请在飞书中使用"' : (!act && !n.name ? ' title="在飞书中使用此操作"' : "");
    const disabled = n.disabled || (!act && !n.name) ? " disabled" : "";
    const nameAttr = n.name ? ` name="${esc(n.name)}"` : "";
    const cur = String(n.value || "");
    const hasCur = cur && opts.some((o) => String(o?.value ?? "") === cur);
    const items = [
      `<option value=""${hasCur ? "" : " selected"} disabled>${ph}</option>`,
      ...opts.map((o) => {
        const v = String(o?.value ?? ""), l = esc(o?.text || v);
        return `<option value="${esc(v)}"${v === cur && hasCur ? " selected" : ""}>${l}</option>`;
      }),
    ].join("");
    return `<select class="im-select"${act}${nameAttr}${disabled}${title} aria-label="${ph}">${items}</select>`;
  }

  function renderOverflow(n) {
    const opts = (Array.isArray(n.options) ? n.options : []).filter((o) => o && (o.text || o.value));
    if (!opts.length) return "";
    const anyLive = opts.some((o) => httpURL(o.url) || (o.action && o.sig));
    const items = [
      `<option value="" selected disabled>更多操作…</option>`,
      ...opts.map((o, i) => {
        const u = httpURL(o.url);
        const label = esc(o.text || o.value);
        if (u) return `<option value="${i}" data-url="${esc(u)}">${label}</option>`;
        if (o.action && o.sig) {
          return `<option value="${i}" data-im-id="${esc(o.action)}" data-im-sig="${esc(o.sig)}" data-im-val="${esc(o.value || "")}">${label}</option>`;
        }
        return `<option value="${i}" disabled>${label}</option>`;
      }),
    ].join("");
    return `<select class="im-select im-overflow"${anyLive ? "" : ' disabled title="在飞书中使用此操作"'} aria-label="更多操作">${items}</select>`;
  }

  function renderPicker(n) {
    const type = n.kind === "time" ? "time" : n.kind === "datetime" ? "datetime-local" : "date";
    const act = actionAttrs(n);
    const disabled = n.disabled || (!act && !n.name) ? " disabled" : "";
    const nameAttr = n.name ? ` name="${esc(n.name)}"` : "";
    const val = n.kind === "datetime" ? String(n.value || "").replace(" ", "T").slice(0, 16) : String(n.value || "");
    const ph = esc(n.placeholder || (type === "time" ? "选择时间" : type === "datetime-local" ? "选择日期时间" : "选择日期"));
    return `<input type="${type}" class="im-picker" value="${esc(val)}"${act}${nameAttr}${disabled} aria-label="${ph}"${disabled ? ' title="在飞书中使用此操作"' : ""}>`;
  }

  function nodes(values, depth = 0) {
    if (!Array.isArray(values) || depth > 20) return "";
    return values.slice(0, 400).map((n) => {
      if (!n || typeof n.kind !== "string") return "";
      const text = esc(n.text), inner = text + nodes(n.children, depth + 1);
      if (Object.hasOwn(containers, n.kind)) {
        const [tag, cls] = containers[n.kind];
        return `<${tag}${cls ? ` class="${cls}"` : ""}>${inner}</${tag}>`;
      }
      switch (n.kind) {
        case "text":
          return String(n.tone || "").split(" ").reduce((body, mark) =>
            Object.hasOwn(marks, mark) ? `<${marks[mark]}>${body}</${marks[mark]}>` : body, inner);
        case "mention": return `<span class="im-mention">${inner}</span>`;
        case "emoji": {
          const url = typeof n.url === "string" && n.url.startsWith("/api/inbox/avatar?") ? esc(n.url) : "";
          const label = text || "表情";
          return url ? `<img class="im-emoji" src="${url}" alt="[${label}]" title="${label}" loading="lazy" decoding="async">`
            : `<span class="im-emoji-alt">[${label}]</span>`;
        }
        case "markdown": return `<div class="im-markdown">${markdown(n.text)}</div>`;
        case "field": return `<div class="im-field${n.tone === "short" ? " short" : ""}">${inner}</div>`;
        case "table": return `<div class="im-table"><table><tbody>${inner}</tbody></table></div>`;
        case "code-block": return `<pre><code>${text}</code></pre>`;
        case "rule": return "<hr>";
        case "link": {
          const url = httpURL(n.url);
          return url ? external(url, inner || esc(url)) : inner;
        }
        case "button": {
          const url = httpURL(n.url), label = inner || "打开";
          const tone = /^primary(?:_|$)/.test(n.tone) ? " primary" : /^danger(?:_|$)/.test(n.tone) ? " danger" : "";
          const cls = `btn small im-action${tone}`;
          if (url && !n.disabled) return external(url, label, cls);
          const act = actionAttrs(n);
          if (act) {
            const btype = n.name === "submit" ? "submit" : "button";
            return `<button type="${btype}" class="${cls}"${act}>${label}</button>`;
          }
          return `<button type="button" class="${cls}" disabled title="在飞书中使用此操作">${label}</button>`;
        }
        case "select": return renderSelect(n);
        case "overflow": return renderOverflow(n);
        case "date":
        case "time":
        case "datetime": return renderPicker(n);
        case "input": {
          const ph = esc(n.placeholder || ""), val = esc(n.value || "");
          const nameAttr = n.name ? ` name="${esc(n.name)}"` : "";
          const lab = text ? `<span>${text}</span>` : "";
          return `<label class="im-field-input">${lab}<input type="text" class="im-input"${nameAttr} value="${val}" placeholder="${ph}"${n.disabled ? " disabled" : ""}></label>`;
        }
        case "form": return `<form class="im-form" data-im-form>${nodes(n.children, depth + 1)}</form>`;
        case "image":
        case "file": return media(n);
        default: return inner;
      }
    }).join("");
  }

  function eventTime(view) {
    if (!view.start) return "";
    const format = (value, allDay, withWeekday) => {
      if (allDay && /^\d{4}-\d{2}-\d{2}/.test(value)) return value.slice(0, 10);
      const date = new Date(value);
      if (!Number.isFinite(+date)) return "";
      const opts = withWeekday
        ? { month: "numeric", day: "numeric", weekday: "short", hour: "2-digit", minute: "2-digit" }
        : { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" };
      return new Intl.DateTimeFormat("zh-CN", opts).format(date);
    };
    const start = format(view.start, view.all_day, true);
    if (!start) return "";
    const sDate = new Date(view.start), eDate = view.end ? new Date(view.end) : null;
    const sameDay = eDate && Number.isFinite(+sDate) && Number.isFinite(+eDate) &&
      sDate.getFullYear() === eDate.getFullYear() && sDate.getMonth() === eDate.getMonth() && sDate.getDate() === eDate.getDate();
    const end = view.end
      ? (sameDay && !view.all_day
        ? new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit" }).format(eDate)
        : format(view.end, view.all_day, false))
      : "";
    const clock = window.collieIcons?.svg ? window.collieIcons.svg("clock") : "";
    const conflict = view.conflict ? `<span class="im-conflict">与已有日程冲突</span>` : "";
    return `<div class="im-event-row"><span class="im-event-time">${clock}<time datetime="${esc(view.start)}">${esc(start)}</time>${end && end !== start
      ? ` – <time datetime="${esc(view.end)}">${esc(end)}</time>` : ""}${view.all_day ? " · 全天" : ""}</span>${conflict}</div>`;
  }

  const RSVP_OPTS = [["accept", "接受"], ["decline", "拒绝"], ["tentative", "待定"]];
  const RSVP_LABELS = Object.fromEntries(RSVP_OPTS);

  function calendarFooter(view) {
    const st = String(view.status || "");
    if (st === "cancelled" || st === "removed") {
      return `<div class="im-cal-foot"><span class="im-note">日程已取消</span></div>`;
    }
    const interactive = !!(view.action && view.sig);
    if (Object.hasOwn(RSVP_LABELS, st)) {
      if (!interactive) {
        return `<div class="im-cal-foot"><span class="im-rsvp-lab">已回复</span><span class="im-rsvp-val">${RSVP_LABELS[st]}</span></div>`;
      }
      const opts = RSVP_OPTS.map(([v, l]) => `<option value="${v}"${v === st ? " selected" : ""}>${l}</option>`).join("");
      return `<div class="im-cal-foot"><span class="im-rsvp-lab">已回复</span><select class="im-select im-rsvp-select" data-pick="compact" data-im-act="calendar" data-im-id="${esc(view.action)}" data-im-sig="${esc(view.sig)}" aria-label="修改日程回复">${opts}</select></div>`;
    }
    if (!interactive) return "";
    const btns = RSVP_OPTS.map(([v, l]) =>
      `<button type="button" class="btn small im-action${v === "accept" ? " primary" : ""}" data-im-act="calendar" data-im-id="${esc(view.action)}" data-im-sig="${esc(view.sig)}" data-im-val="${v}">${l}</button>`
    ).join("");
    return `<div class="im-cal-foot"><div class="im-actions">${btns}</div></div>`;
  }

  function render(view, ctx = {}) {
    const body = nodes(view.nodes);
    if (view.kind === "system") return `<div class="im-system">${body}</div>`;
    if (view.kind === "media") return `<div class="im-content im-attachments">${body}</div>`;
    const isCard = view.kind === "card", isEvent = view.kind === "calendar";
    const tone = /^(red|orange|yellow)$/.test(view.tone) ? "attn" : /^green$/.test(view.tone) ? "ok" : "info";
    const title = esc(view.title || (isEvent ? "日程" : isCard && !body ? "飞书卡片" : ""));
    const url = httpURL(view.url);
    const header = title || view.subtitle ? `<header class="im-heading"><strong>${url && title ? external(url, title) : title}</strong>${
      view.subtitle ? `<span>${esc(view.subtitle)}</span>` : ""}</header>` : "";
    const time = isEvent ? eventTime(view) : "";
    const foot = isEvent ? calendarFooter(view) : "";
    const cls = isCard ? "im-card" : isEvent ? "im-card im-calendar" : "im-post";
    const attrs = (isCard || isEvent) && ctx.convId && ctx.msgId
      ? ` data-im-conv="${esc(ctx.convId)}" data-im-msg="${esc(ctx.msgId)}"` : "";
    return `<div class="im-content ${cls}" data-tone="${tone}"${attrs}>${header}${body || time || foot
      ? `<div class="im-body">${time}${body}${foot}</div>` : ""}</div>`;
  }

  // Older hub rows may contain a serialized payload clipped halfway through.
  const legacyPreview = (text) => String(text || "").replace(
    /^(.*?\[(?:日历|分享日程|卡片|飞书消息)\])\s+[\[{].*$/s, "$1",
  );
  window.collieInboxMessage = { render, legacyPreview };
})();

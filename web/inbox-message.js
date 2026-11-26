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
      return url ? `<img class="im-image" src="${url}" alt="${alt}" loading="lazy">` : `<span class="im-note">图片暂不可用</span>`;
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
          const cls = `btn small im-action${/^primary(?:_|$)/.test(n.tone) ? " primary" : ""}`;
          return url ? external(url, label, cls)
            : `<button type="button" class="${cls}" disabled title="在飞书中使用此操作">${label}</button>`;
        }
        case "image":
        case "file": return media(n);
        default: return inner;
      }
    }).join("");
  }

  function eventTime(view) {
    if (!view.start) return "";
    const format = (value, allDay) => {
      if (allDay && /^\d{4}-\d{2}-\d{2}/.test(value)) return value.slice(0, 10);
      const date = new Date(value);
      return Number.isFinite(+date) ? new Intl.DateTimeFormat("zh-CN", {
        month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit",
      }).format(date) : "";
    };
    const start = format(view.start, view.all_day), end = format(view.end || "", view.all_day);
    if (!start) return "";
    return `<div class="im-event-time"><time datetime="${esc(view.start)}">${esc(start)}</time>${end && end !== start
      ? ` – <time datetime="${esc(view.end)}">${esc(end)}</time>` : ""}${view.all_day ? " · 全天" : ""}</div>`;
  }

  function render(view) {
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
    const cls = isCard ? "im-card" : isEvent ? "im-card im-calendar" : "im-post";
    return `<div class="im-content ${cls}" data-tone="${tone}">${header}${body || time
      ? `<div class="im-body">${time}${body}</div>` : ""}</div>`;
  }

  // Older hub rows may contain a serialized payload clipped halfway through.
  const legacyPreview = (text) => String(text || "").replace(
    /^(.*?\[(?:日历|分享日程|卡片|飞书消息)\])\s+[\[{].*$/s, "$1",
  );
  window.collieInboxMessage = { render, legacyPreview };
})();

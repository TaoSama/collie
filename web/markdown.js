// Render Markdown used in chat, memory, and file previews.
(() => {
  "use strict";

  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const unesc = (s) => s.replace(/&(amp|lt|gt|quot|#39);/g, (m) => ({ "&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": '"', "&#39;": "'" }[m]));
  const TABLE_NUMBER = /^[+−-]?(?:\d+|\d{1,3}(?:,\d{3})+)(?:\.\d+)?[%％]?$/;
  const QUOTE_LINE = /^ {0,3}&gt;[ \t]?(.*)$/;

  function createRenderer({ fileLinkAttrs, filePattern = /(?!)/g, imagePattern, thumbnailURL, detectCodeLang, highlightCode, externalContent = false }) {
    // Minimal, safe Markdown: escape first, then add a little structure.
    const previewCache = new Map(), sessionCache = new Map();
    const CODE_CHIP_MAX = 24; // inline code up to this many characters is kept on one line in a table cell
    function md(src, { session = false } = {}) {
      const mdCache = session ? sessionCache : previewCache;
      const key = String(src || "");
      let out = mdCache.get(key);
      if (out === undefined) {
        out = mdRender(key, session);
        if (mdCache.size > 8000) mdCache.delete(mdCache.keys().next().value);
        mdCache.set(key, out);
      }
      return out;
    }
    // A bare link: ASCII URL characters up to whitespace, tags or non-ASCII text
    // and punctuation, shown whole without its scheme ([name](url) is better
    // still: the name shows).
    const URL_RE = /(?<![A-Za-z0-9_\/])https?:\/\/[^\x00-\x20<>\x7f-\uffff]+/g;
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
    function extractSessionFences(text, storeCode) {
      const openFence = /^( {0,3})(`{3,}|~{3,})([^\n]*)$/gm;
      let result = "", cursor = 0, open;
      while ((open = openFence.exec(text))) {
        const [line, indent, fence, info] = open;
        if (fence[0] === "`" && info.includes("`")) continue;
        const contentStart = open.index + line.length + 1;
        const closeFence = new RegExp("^ {0,3}" + fence[0] + "{" + fence.length + ",}[ \\t]*$", "gm");
        closeFence.lastIndex = contentStart;
        const close = closeFence.exec(text);
        let code = text.slice(contentStart, close ? close.index : text.length);
        if (indent) code = code.replace(new RegExp("^ {1," + indent.length + "}", "gm"), "");
        result += text.slice(cursor, open.index) + storeCode(info, code);
        if (!close) return result;
        cursor = openFence.lastIndex = close.index + close[0].length;
      }
      return result + text.slice(cursor);
    }
    function mdRender(src, session) {
      const blocks = [];
      const storeCode = (info, code) => {
        const raw = code.replace(/\n$/, "");
        const lang = detectCodeLang(info, false);
        const pre = `<pre><code${lang ? ` data-lang="${lang.id}"` : ""}>${lang ? highlightCode(raw, lang.id) : esc(raw)}</code></pre>`;
        const label = lang?.label || info.trim().split(/\s+/, 1)[0] || "纯文本";
        blocks.push(session ? `<div class="md-code"><div class="md-code-bar"><span>${esc(label)}</span><button type="button" class="md-code-copy" aria-label="复制代码">复制</button></div>${pre}</div>` : pre);
        return `\u0000${blocks.length - 1}\u0000`;
      };
      let text = src.replace(/\r\n?/g, "\n").replace(/[\u0000\u0001]/g, "");
      text = session ? extractSessionFences(text, storeCode)
        : text.replace(/```([^\n`]*)\n([\s\S]*?)(```|$)/g, (_, info, code) => storeCode(info, code));
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
              const chip = raw.length <= CODE_CHIP_MAX ? " data-chip" : "";
              if (cleanP && !externalContent) {
                const ep = esc(cleanP);
                scanned += keep(`<a ${fileLinkAttrs(cleanP, ep, "打开这个文件")}><code class="code-link"${chip}>${c}</code></a>`);
              } else if (isPureHttpUrl(raw)) {
                const eu = esc(raw);
                scanned += keep(`<a class="url" href="${eu}" target="_blank" rel="noopener noreferrer" title="${eu}"><code class="code-link"${chip}>${c}</code></a>`);
              } else {
                scanned += keep(`<code${chip}>${c}</code>`);
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
            if (externalContent) return alt || "图片";
            const rawU = unesc(u), eu = esc(rawU);
            return keep(`<button type="button" class="md-img-btn" data-img-url="${eu}" data-img-alt="${alt}" aria-label="${alt || "查看大图"}"><img class="md-img" src="${eu}" alt="${alt}" loading="lazy"></button>`);
          })
          .replace(/!\[([^\]\n]*)\]\((?:file:\/\/)?((?:\/|~\/)[^)\s"#]+)(?:#[^)\s"]*)?\)/g, (_, alt, p) => {
            if (externalContent) return alt || "图片";
            p = unesc(p).replace(/:\d+(?:[-:]\d+)?$/, "");
            try { p = decodeURI(p); } catch {}
            const ep = esc(p);
            if (!imagePattern.test(p)) return keep(`<a ${fileLinkAttrs(p, ep, ep)}>${alt || ep}</a>`);
            return keep(`<button type="button" class="thumb md-thumb" data-img="${ep}" data-img-alt="${alt}" aria-label="${alt || esc("查看大图 " + (p.split("/").pop() || "图片"))}"><img class="md-img" alt="${alt}"${thumbnailURL(p) ? ` src="${thumbnailURL(p)}"` : ""}></button>`);
          })
          .replace(/\[([^\]\n]+)\]\((https?:\/\/(?:[^()\s"]|\([^()\s"]*\))+)\)/g, (_, l, u) => keep(`<a href="${u}" target="_blank" rel="noopener noreferrer">${l}</a>`))
          // [MergeStatus.tsx](file:///…/MergeStatus.tsx#L67-L84): the name, opening the file.
          .replace(/\[([^\]\n]+)\]\((?:file:\/\/)?((?:\/|~\/)[^)\s"#]+)(?:#[^)\s"]*)?\)/g, (_, label, p) => {
            if (externalContent) return label;
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
          .replace(filePattern, (_, pre, p) => {
            if (externalContent) return pre + p;
            const rawP = unesc(p);
            return `${pre}${keep(`<a ${fileLinkAttrs(rawP, p, "打开这个文件")}>${p}</a>`)}`;
          })
          // *italic* and _italic_, after links so a URL's or path's * and _ stay
          // as they are; not inside a word (2*3*4, snake_case) or around spaces (a * b).
          .replace(/(?<![A-Za-z0-9*])\*(?![\s*])([^*\n]*?[^\s*])\*(?![A-Za-z0-9*])/g, "<em>$1</em>")
          .replace(/(?<![\p{L}\p{N}_])_(?![\s_])([^_\n]*?[^\s_])_(?![\p{L}\p{N}_])/gu, "<em>$1</em>");
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
          const align = cells(lines[li + 1]).map((c) => {
            if (c.endsWith(":")) return c.startsWith(":") ? "center" : "right";
            return session && c.startsWith(":") ? "left" : "";
          });
          const rows = [];
          li += 2;
          while (li < lines.length && lines[li].includes("|") && lines[li].trim()) {
            rows.push(cells(lines[li]));
            li++;
          }
          li--;
          const numeric = session ? head.map((_, k) => {
            const values = rows.map((row) => row[k]).filter(Boolean);
            return values.length > 0 && values.every((value) => TABLE_NUMBER.test(value));
          }) : [];
          const td = (tag, c, k) => {
            const direction = align[k] || (numeric[k] ? "right" : "");
            return `<${tag}${numeric[k] ? ' class="md-number"' : ""}${direction ? ` style="text-align:${direction}"` : ""}>${inline(c)}</${tag}>`;
          };
          const body = rows.map((row) => `<tr>${head.map((_, k) => td("td", row[k] || "", k)).join("")}</tr>`).join("");
          out.push(`<div class="md-table" tabindex="0" role="region" aria-label="数据表格"><table><thead><tr>${head.map((c, k) => td("th", c, k)).join("")}</tr></thead><tbody>${body}</tbody></table></div>`);
          continue;
        }
        if (/^\u0000\d+\u0000$/.test(line.trim())) { flushPara(); flushList(); out.push(line.trim()); continue; }
        if (/^\s{0,3}([-*_])(\s*\1){2,}\s*$/.test(line)) { flushPara(); flushList(); out.push("<hr>"); continue; }
        if ((m = line.match(/^(#{1,4})\s+(.*)$/))) { flushPara(); flushList(); const h = session ? m[1].length : Math.min(m[1].length + 1, 4); out.push(`<h${h}>${inline(m[2])}</h${h}>`); continue; }
        if ((m = line.match(/^\s*[-*•]\s+(.*)$/)) || (m = line.match(/^\s*\d+[.)]\s+(.*)$/))) {
          const tag = /^\s*\d/.test(line) ? "ol" : "ul";
          flushPara();
          // "2." after a few bullets carries on the numbering, as the agent wrote it.
          if (!list || list.tag !== tag) { flushList(); list = { tag, items: [], start: parseInt(line) || 1 }; }
          list.items.push(m[1]);
          continue;
        }
        if ((m = line.match(session ? QUOTE_LINE : /^&gt;\s?(.*)$/))) {
          flushPara(); flushList();
          const quote = [inline(m[1])];
          if (session) {
            while ((m = lines[li + 1]?.match(QUOTE_LINE))) {
              quote.push(inline(m[1]));
              li++;
            }
          }
          out.push(`<blockquote>${quote.join("<br>")}</blockquote>`);
          continue;
        }
        if (!line.trim()) { flushPara(); flushList(); continue; }
        flushList();
        para.push(line);
      }
      flushPara(); flushList();
      return out.join("").replace(/\u0000(\d+)\u0000/g, (_, n) => blocks[+n]);
    }
    return md;
  }

  window.collieMarkdown = { createRenderer, esc, unesc };
})();

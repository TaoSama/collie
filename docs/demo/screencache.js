// Local scroll cache, delta frame decoder, and viewport region segmenter for
// fullscreen agents in screen mode (Claude Code, Codex, agy).
//
// Fullscreen agents keep scrollback inside their own alt-screen TUI. Caching
// visited transcript lines locally lets scrolling back (and predictive
// prefetching ahead) render in < 5ms with zero network round trips, while
// separating sticky top headers and bottom input chrome from the scrollable
// body so nothing ghosts or gets stuck at row 0.
(() => {
  const ANSI_RE = /\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[78])/g;
  const RULE_RE = /^\s*[─━═╌╍\-]{16,}\s*$/;
  const OVERLAY_RE = /\s*(?:Jump to bottom \(ctrl\+End\)\s*↓|↓\s*Back to bottom\s*·\s*esc|↓\s*\d+\s+lines?|\d+\s+new messages?\s*↓)\s*$/;
  const SOLO_OVERLAY_RE = /^\s*(?:Jump to bottom \(ctrl\+End\)\s*↓|↓\s*Back to bottom\s*·\s*esc|↓\s*\d+\s+lines?|\d+\s+new messages?\s*↓)\s*$/;
  const TOP_BANNER_RE = /(?:Claude Code v\d|OpenAI Codex|Antigravity CLI \d)/;
  const CLAUDE_STICKY_BG = "48;2;55;55;55";
  const CLAUDE_STICKY_DIM_FG = "38;2;80;80;80";
  const CLAUDE_STICKY_READABLE_FG = "38;2;225;225;225";

  const stripAnsi = (s) => (s || "").replace(ANSI_RE, "");
  const hasOverlayPill = (line) => OVERLAY_RE.test(stripAnsi(line));
  const isSoloOverlay = (line) => SOLO_OVERLAY_RE.test(stripAnsi(line));
  const lineKey = (line) => stripAnsi(line).replace(OVERLAY_RE, "").trimEnd();

  // Remove Claude's floating right-side "Jump to bottom (ctrl+End) ↓" pill from
  // a transcript line before storing it in the cache so scrolling never leaves
  // ghost pills baked into the middle of the transcript.
  function cleanCachedLine(line) {
    if (!hasOverlayPill(line)) return line;
    const plain = stripAnsi(line);
    const m = OVERLAY_RE.exec(plain);
    if (!m) return line;
    const keepChars = m.index;
    let vis = 0, i = 0, out = "";
    while (i < line.length && vis < keepChars) {
      if (line[i] === "\x1b") {
        ANSI_RE.lastIndex = i;
        const em = ANSI_RE.exec(line);
        if (em && em.index === i) { out += em[0]; i = ANSI_RE.lastIndex; continue; }
      }
      out += line[i++];
      vis++;
    }
    return out.replace(/\s+$/, "") + "\x1b[0m";
  }

  // Claude Code 2.1 fullscreen draws its scrolled sticky prompt header on row 0
  // with fg rgb(80,80,80) on bg rgb(55,55,55) (contrast 1.37:1), which looks
  // like a broken dark bar stuck to the top row. Lift only that dimmed fg so
  // the sticky header stays legible without touching any other agent or colour.
  function polishClaudeRow0(line, kind) {
    if (kind !== "claude" || !line) return line;
    if (!line.includes(CLAUDE_STICKY_BG) || !line.includes(CLAUDE_STICKY_DIM_FG)) return line;
    const plain = stripAnsi(line).trimStart();
    if (!plain.startsWith("❯")) return line;
    return line.replaceAll(CLAUDE_STICKY_DIM_FG, CLAUDE_STICKY_READABLE_FG);
  }

  // Reconstruct full line array from either a full text frame or a row-shift
  // delta patch ({ shift, patch, n }).
  function applyFramePatch(prevLines, f) {
    if (typeof f.text === "string") {
      const raw = f.text.replace(/\r\n/g, "\n").replace(/\r/g, "");
      return raw ? raw.split("\n") : [""];
    }
    if (Array.isArray(f.patch) && prevLines && Number.isInteger(f.n) && f.n > 0) {
      const sh = f.shift || 0;
      const next = new Array(f.n);
      for (let r = 0; r < f.n; r++) {
        const pr = r + sh;
        next[r] = pr >= 0 && pr < prevLines.length ? prevLines[pr] : "";
      }
      for (const entry of f.patch) {
        const r = entry[0], txt = entry[1];
        if (r >= 0 && r < next.length) next[r] = txt;
      }
      return next;
    }
    return prevLines ? prevLines.slice() : [""];
  }

  // Segment a fullscreen agent frame into:
  //   topPin : sticky prompt header rows at the top (0..2)
  //   body   : scrollable transcript rows in the middle
  //   botPin : fixed composer + status bar rows at the bottom
  function segmentFrame(lines, kind) {
    const n = lines.length;
    if (n < 8 || (kind !== "claude" && kind !== "codex" && kind !== "agy")) {
      return { topCount: 0, botStart: n, topPin: [], body: lines.slice(), botPin: [] };
    }
    const plain = lines.map(stripAnsi);
    let topCount = 0;
    let botStart = n;

    if (kind === "claude") {
      // Bottom chrome: horizontal rule above `❯` prompt line near the bottom,
      // plus the 1-row blank margin Claude leaves directly above the rule.
      for (let i = n - 1; i >= Math.max(3, n - 12); i--) {
        if (/^\s*❯[\s\u00a0]/.test(plain[i]) || plain[i].trim() === "❯") {
          botStart = i > 0 && RULE_RE.test(plain[i - 1]) ? i - 1 : i;
          if (botStart > 2 && plain[botStart - 1].trim() === "" && !lines[botStart - 1].includes("48;2;")) {
            botStart--;
          }
          break;
        }
      }
      // Top sticky prompt: single dark-background bar at row 0 starting with `❯`.
      if (botStart > 2 && lines[0].includes(CLAUDE_STICKY_BG) && /^\s*❯/.test(plain[0]) && !lines[1].includes(CLAUDE_STICKY_BG)) {
        topCount = 1;
      }
    } else if (kind === "codex") {
      // Bottom chrome: `› ` composer line (plus optional dark-padded blank row above it).
      for (let i = n - 1; i >= Math.max(3, n - 10); i--) {
        if (/^\s*›[\s\u00a0]/.test(plain[i]) || plain[i].trim() === "›") {
          botStart = i;
          // Codex pads its composer box with a dark background row right above `› `.
          if (botStart > 1 && plain[botStart - 1].trim() === "" && lines[botStart - 1].includes("48;2;")) {
            botStart--;
          }
          break;
        }
      }
      // Top sticky prompt: row 0 with dark bar background starting with `› `.
      if (botStart > 2 && lines[0].includes("48;2;") && /^\s*›[\s\u00a0]/.test(plain[0]) && !lines[1].includes("48;2;")) {
        topCount = 1;
      }
    } else if (kind === "agy") {
      // Bottom chrome: horizontal rule above `>` prompt line.
      for (let i = n - 1; i >= Math.max(3, n - 10); i--) {
        if (/^\s*>[\s\u00a0]*$/.test(plain[i]) || /^\s*>\s/.test(plain[i])) {
          if (i > 0 && RULE_RE.test(plain[i - 1])) {
            botStart = i - 1;
            break;
          }
        }
      }
      // Top sticky prompt: row 0 starts with `> ` and row 1 is a horizontal rule.
      if (botStart > 3 && /^\s*>\s/.test(plain[0]) && RULE_RE.test(plain[1])) {
        topCount = 2;
      }
    }

    // Dedicated bottom scroll-indicator rows (Codex's "↓ Back to bottom · esc",
    // agy's "↓ 44 lines", Claude's standalone "Jump to bottom") belong to botPin,
    // never to the stitched transcript body.
    let scrolledIndicator = false;
    let belowLines = 0;
    while (botStart > topCount + 2 && isSoloOverlay(lines[botStart - 1])) {
      const m = stripAnsi(lines[botStart - 1]).match(/↓\s*(\d+)\s+lines?/i);
      if (m) belowLines = Math.max(belowLines, parseInt(m[1], 10) || 0);
      botStart--;
      scrolledIndicator = true;
    }
    if (botStart > topCount && hasOverlayPill(lines[botStart - 1])) {
      scrolledIndicator = true;
    }

    if (botStart - topCount < 4) {
      return { topCount: 0, botStart: n, topPin: [], body: lines.slice(), botPin: [], scrolledIndicator, belowLines };
    }
    return {
      topCount,
      botStart,
      topPin: lines.slice(0, topCount),
      body: lines.slice(topCount, botStart),
      botPin: lines.slice(botStart),
      scrolledIndicator,
      belowLines,
    };
  }

  // Find best index `top` in `cacheKeys` where `bodyKeys` aligns (can be negative
  // when older lines need to be prepended, or extend past cacheKeys.length).
  function findOverlap(cacheKeys, bodyKeys, expectedTop) {
    const L = cacheKeys.length, H = bodyKeys.length;
    if (!L || !H) return null;
    const minNonEmpty = Math.min(2, bodyKeys.filter((k) => k !== "").length);
    let best = null;

    for (let top = -H + 2; top <= L - 2; top++) {
      const ovStart = Math.max(0, top);
      const ovEnd = Math.min(L, top + H);
      const ovLen = ovEnd - ovStart;
      if (ovLen < 2) continue;

      let matches = 0, nonEmptyMatches = 0, nonEmptyTotal = 0;
      for (let i = ovStart; i < ovEnd; i++) {
        const ck = cacheKeys[i], bk = bodyKeys[i - top];
        const nonEmpty = ck !== "" || bk !== "";
        if (nonEmpty) nonEmptyTotal++;
        if (ck === bk) {
          matches++;
          if (nonEmpty) nonEmptyMatches++;
        }
      }
      if (nonEmptyTotal > 0 && nonEmptyMatches < Math.min(minNonEmpty, nonEmptyTotal)) continue;
      const ratio = matches / ovLen;
      if (ratio < 0.8) continue;

      const dist = Math.abs(top - expectedTop);
      if (
        !best ||
        nonEmptyMatches > best.nonEmptyMatches ||
        (nonEmptyMatches === best.nonEmptyMatches && matches > best.matches) ||
        (nonEmptyMatches === best.nonEmptyMatches && matches === best.matches && dist < best.dist)
      ) {
        best = { top, matches, nonEmptyMatches, ovLen, dist };
      }
    }
    return best;
  }

  const MAX_CACHE_LINES = 6000;
  const PREFETCH_MARGIN = 14;
  const DEFAULT_TICKS_SPEED = { claude: 1, codex: 1, agy: 2 };

  function createScreenCache() {
    let pane = "";
    let kind = "";
    let cols = 0;
    let paneRows = 0;

    let lines = [];       // stitched scrollable transcript ANSI lines (oldest -> newest)
    let keys = [];        // normalized match keys parallel to `lines`
    let clean = [];       // true if stored line has no floating "Jump to bottom" overlay
    let topPin = [];      // latest sticky prompt header (when scrolled mid-turn)
    let topPinAtBottom = []; // topPin seen when at bottom (e.g. Codex keeps row 0 even at bottom)
    let botPin = [];      // latest bottom chrome (clean, without transient scrolled indicator when at bottom)
    let botPinLive = [];  // bottom chrome seen at live bottom
    let bodyRows = 0;     // visible body slot count when scrolled (paneRows - topPin.length - botPin.length)
    let baseBodyRows = 0; // visible body slot count at bottom

    let viewTop = 0;      // currently displayed index in `lines`
    let agentTop = 0;     // authoritative agent viewport index in `lines`
    let hitTop = false;   // true once scrolling up reaches real top of history
    let atBottom = true;  // true when user is following live bottom
    let seenBottom = true; // false if pane opened while already scrolled up
    let estBelow = 0;     // estimated transcript lines below `lines` when !seenBottom
    let pendingPageUpLines = 0;
    let totalHint = 0;    // server-estimated total transcript lines (optional)
    let linesPerTick = 1; // observed lines scrolled per single SGR wheel report

    // User browsing / pending fetch state
    let lastUserScrollAt = 0;
    let pendingUpDemand = 0;   // lines the user wanted above index 0 when a miss fired
    let pendingUpSentAt = 0;
    let prefetchInFlight = false;
    let noOpUpCount = 0;       // consecutive confirmed no-op upward fetches at agentTop === 0
    let lastAgentBodySig = "";

    function reset(newPane = "", newKind = "", newCols = 0, newRows = 0) {
      pane = newPane;
      kind = newKind;
      cols = newCols;
      paneRows = newRows;
      lines = [];
      keys = [];
      clean = [];
      topPin = [];
      topPinAtBottom = [];
      botPin = [];
      botPinLive = [];
      bodyRows = 0;
      baseBodyRows = 0;
      viewTop = 0;
      agentTop = 0;
      hitTop = false;
      atBottom = true;
      seenBottom = true;
      estBelow = 0;
      pendingPageUpLines = 0;
      totalHint = 0;
      linesPerTick = DEFAULT_TICKS_SPEED[newKind] || 1;
      lastUserScrollAt = 0;
      pendingUpDemand = 0;
      pendingUpSentAt = 0;
      prefetchInFlight = false;
      noOpUpCount = 0;
      lastAgentBodySig = "";
    }

    function activeTopPinFor(pos, isBottom) {
      if (isBottom) return topPinAtBottom;
      if (hitTop && pos <= 1) return [];
      return topPin;
    }

    function activeBotPinFor(isBottom) {
      return isBottom && botPinLive.length ? botPinLive : botPin;
    }

    function visibleBodyHeight(isBottom, pos = viewTop) {
      const tp = activeTopPinFor(pos, isBottom);
      const bp = activeBotPinFor(isBottom);
      const h = paneRows - tp.length - bp.length;
      return h > 0 ? h : (bodyRows || baseBodyRows || paneRows);
    }

    function maxViewTopFor(isBottom = false) {
      const h = visibleBodyHeight(isBottom, lines.length);
      return Math.max(0, lines.length - h);
    }

    function compose(pos = viewTop, isBottom = atBottom) {
      const tp = activeTopPinFor(pos, isBottom);
      const bp = activeBotPinFor(isBottom);
      const h = Math.max(1, paneRows - tp.length - bp.length);
      const maxT = Math.max(0, lines.length - h);
      const clamped = Math.max(0, Math.min(maxT, Math.round(pos)));
      const out = [];
      for (const l of tp) out.push(polishClaudeRow0(l, kind));
      for (let i = 0; i < h; i++) {
        out.push(lines[clamped + i] ?? "");
      }
      for (const l of bp) out.push(l);
      while (out.length < paneRows) out.push("");
      return out.slice(0, paneRows);
    }

    function trimCap() {
      if (lines.length <= MAX_CACHE_LINES) return;
      const drop = lines.length - MAX_CACHE_LINES;
      // Prefer dropping from whichever end is farthest from viewTop.
      if (viewTop > lines.length / 2) {
        lines.splice(0, drop);
        keys.splice(0, drop);
        clean.splice(0, drop);
        viewTop = Math.max(0, viewTop - drop);
        agentTop = Math.max(0, agentTop - drop);
        hitTop = false;
      } else {
        lines.length = MAX_CACHE_LINES;
        keys.length = MAX_CACHE_LINES;
        clean.length = MAX_CACHE_LINES;
      }
    }

    function writeSlot(idx, rawLine, key) {
      const isClean = !hasOverlayPill(rawLine);
      if (idx >= 0 && idx < lines.length && clean[idx] && !isClean && keys[idx] === key) {
        return; // keep cleaner version without floating pill
      }
      lines[idx] = isClean ? rawLine : cleanCachedLine(rawLine);
      keys[idx] = key;
      clean[idx] = true;
    }

    // Ingest an authoritative frame from SSE.
    // Returns { renderLines, usedCache, hitTop, atBottom, changed }.
    function ingest(rawFrameLines, meta = {}) {
      const fPane = meta.pane || pane;
      const fKind = meta.kind || kind;
      const fCols = meta.cols || cols;
      const fRows = Math.max(meta.rows || 0, rawFrameLines.length);
      const now = meta.now ?? performance.now();

      if (fPane !== pane || fKind !== kind || (cols && fCols !== cols) || (paneRows && fRows !== paneRows)) {
        reset(fPane, fKind, fCols, fRows);
      }
      if (meta.hist && meta.hist > 0) {
        totalHint = Math.max(totalHint, meta.hist);
      }

      // Pad frame to paneRows so term.rows never shrinks and triggers term.reset().
      const padded = rawFrameLines.slice();
      while (padded.length < paneRows) padded.push("");
      if (padded.length > 0) padded[0] = polishClaudeRow0(padded[0], kind);

      const seg = segmentFrame(padded, kind);
      const bodyKeys = seg.body.map(lineKey);
      const bodySig = bodyKeys.join("\n");
      const hasBanner = bodyKeys.slice(0, 5).some((k) => TOP_BANNER_RE.test(k));

      // First frame or unsegmented screen (dialog/menu/startup): initialize directly.
      if (!lines.length || seg.botStart === padded.length) {
        lines = seg.body.map(cleanCachedLine);
        keys = bodyKeys;
        clean = seg.body.map(() => true);
        topPin = seg.topPin.slice();
        const fullToBottom = bodyKeys.length >= 16 && bodyKeys.slice(-2).some((k) => k !== "");
        const topScrolled = hasBanner && (seg.scrolledIndicator || totalHint > seg.body.length + 2 || fullToBottom);
        const claudeStickyScrolled = kind === "claude" && seg.topPin.length > 0;
        const scrolled = seg.scrolledIndicator || topScrolled || claudeStickyScrolled;
        topPinAtBottom = scrolled ? [] : seg.topPin.slice();
        botPin = seg.botPin.slice();
        if (!scrolled) botPinLive = seg.botPin.slice();
        bodyRows = seg.body.length;
        baseBodyRows = seg.body.length;
        viewTop = 0;
        agentTop = 0;
        atBottom = !scrolled;
        seenBottom = !scrolled;
        if (seg.belowLines > 0) estBelow = seg.belowLines;
        else if (!seenBottom) estBelow = Math.max(seg.body.length, totalHint > seg.body.length ? totalHint - seg.body.length : seg.body.length);
        else estBelow = 0;
        if (hasBanner) hitTop = true;
        lastAgentBodySig = bodySig;
        prefetchInFlight = false;
        pendingUpDemand = 0;
        return {
          renderLines: padded,
          usedCache: false,
          hitTop,
          atBottom,
          changed: true,
          scrolledIndicator: seg.scrolledIndicator,
          belowLines: seg.belowLines,
        };
      }

      // Always keep bottom chrome fresh (status/composer/cursor live here).
      const claudeStickyScrolled = kind === "claude" && seg.topPin.length > 0 && topPinAtBottom.length === 0;
      const scrolledFromBottom = seg.scrolledIndicator || claudeStickyScrolled;
      botPin = seg.botPin.slice();
      if (!scrolledFromBottom && seenBottom) {
        botPinLive = seg.botPin.slice();
      }
      if (seg.topPin.length > 0) topPin = seg.topPin.slice();
      if (!scrolledFromBottom && atBottom && seenBottom && agentTop >= maxViewTopFor(true) - 1) {
        topPinAtBottom = seg.topPin.slice();
        baseBodyRows = seg.body.length;
      }
      bodyRows = seg.body.length;

      // If we thought we were at live bottom (e.g. right after returnToLive on
      // agy) yet the authoritative frame still shows a scroll-down indicator
      // ("↓ 27 lines"), revoke atBottom/seenBottom so the UI stays honest and
      // triggers follow-up PageDowns. During normal stitched upward scrolling
      // (atBottom is already false), keep seenBottom intact since `lines` still
      // anchors to the real bottom.
      if (seg.scrolledIndicator && atBottom) {
        atBottom = false;
        seenBottom = false;
        estBelow = Math.max(seg.belowLines || 0, bodyRows || seg.body.length);
      }

      // Unchanged body (e.g. cursor blink / status bar update while a remote
      // wheel POST is still in flight): never falsely trip hitTop here! Only
      // refresh chrome/cursor and let post-completion top confirmation decide.
      if (bodySig === lastAgentBodySig) {
        const browsing = !atBottom && (viewTop !== agentTop || now - lastUserScrollAt < 250);
        return {
          renderLines: browsing ? compose(viewTop, false) : padded,
          usedCache: browsing,
          hitTop,
          atBottom,
          changed: false,
          prepended: 0,
          scrolledIndicator: seg.scrolledIndicator,
          belowLines: seg.belowLines,
        };
      }
      lastAgentBodySig = bodySig;

      // Align incoming body against our stitched line buffer.
      const prevAgentTop = agentTop;
      const ov = findOverlap(keys, bodyKeys, agentTop);
      let prepended = 0;

      let disjointUp = false;
      if (ov) {
        let top = ov.top;
        if (top < 0) {
          const prepend = -top;
          prepended = prepend;
          hitTop = false;
          noOpUpCount = 0;
          pendingUpSentAt = 0;
          const newLines = new Array(prepend);
          const newKeys = new Array(prepend);
          const newClean = new Array(prepend);
          for (let i = 0; i < prepend; i++) {
            newLines[i] = cleanCachedLine(seg.body[i]);
            newKeys[i] = bodyKeys[i];
            newClean[i] = true;
          }
          lines = newLines.concat(lines);
          keys = newKeys.concat(keys);
          clean = newClean.concat(clean);
          viewTop += prepend;
          agentTop = 0;
          top = 0;

          // Fulfill pending upward demand smoothly: land on the exact lines the
          // user asked for while keeping the extra prefetched lines above!
          if (pendingUpDemand > 0) {
            const move = Math.min(prepend, pendingUpDemand);
            viewTop = Math.max(0, viewTop - move);
            pendingUpDemand = Math.max(0, pendingUpDemand - move);
            atBottom = false;
          }
        } else {
          agentTop = top;
        }

        // Write overlapping & newly appended lines.
        const end = top + seg.body.length;
        if (end > lines.length) {
          const extra = end - lines.length;
          for (let i = 0; i < extra; i++) {
            lines.push("");
            keys.push("");
            clean.push(false);
          }
        }
        for (let i = 0; i < seg.body.length; i++) {
          writeSlot(top + i, seg.body[i], bodyKeys[i]);
        }
        trimCap();

        if (atBottom) {
          viewTop = Math.max(0, lines.length - seg.body.length);
        }
      } else {
        // Disjoint jump (e.g. PageUp / ctrl+End jump far outside cached window,
        // or a brand-new screen after /clear).
        if (atBottom) {
          // If live bottom jumped far ahead, reset cache cleanly to the new live view.
          lines = seg.body.slice();
          keys = bodyKeys;
          clean = seg.body.map((l) => !hasOverlayPill(l));
          topPinAtBottom = seg.topPin.slice();
          viewTop = 0;
          agentTop = 0;
          hitTop = hasBanner;
        } else {
          // User jumped upward into an uncached distant page: anchor at top of a
          // fresh stitched window (keep totalHint and estimated lines below).
          disjointUp = true;
          const prevLen = lines.length;
          seenBottom = false;
          if (seg.belowLines > 0) estBelow = seg.belowLines;
          else estBelow = Math.max(estBelow + prevLen, pendingPageUpLines || seg.body.length);
          pendingPageUpLines = 0;
          totalHint = Math.max(totalHint, estBelow + seg.body.length);
          lines = seg.body.map(cleanCachedLine);
          keys = bodyKeys;
          clean = seg.body.map(() => true);
          viewTop = 0;
          agentTop = 0;
          pendingUpDemand = 0;
        }
      }

      if (hasBanner && agentTop === 0) hitTop = true;
      prefetchInFlight = false;
      if (agentTop < prevAgentTop) pendingUpSentAt = 0;

      // Decide whether to render from local cache (user is browsing at viewTop
      // while agent catches up or prefetched ahead) or verbatim authoritative frame.
      const browsing = !atBottom && (Math.abs(viewTop - agentTop) > 0 || now - lastUserScrollAt < 180);
      if (!browsing) {
        viewTop = agentTop;
        if (!disjointUp && !scrolledFromBottom && seenBottom && agentTop >= Math.max(0, lines.length - seg.body.length) && seg.topPin.length === topPinAtBottom.length) {
          atBottom = true;
        }
      }
      return {
        renderLines: browsing ? compose(viewTop, false) : padded,
        usedCache: browsing,
        hitTop,
        atBottom,
        changed: true,
        prepended,
        scrolledIndicator: seg.scrolledIndicator,
        belowLines: seg.belowLines,
      };
    }

    // Handle a user scroll of `deltaLines` (< 0 upward toward older history, > 0 downward).
    // Returns:
    //   hit          : true if answered immediately from local cache (0 network wait!)
    //   renderLines  : frame to paint immediately (or null if pure cold miss)
    //   sendTicks    : wheel ticks to send to the real agent right now (0 if deferred/none)
    //   syncDelta    : desired agent offset delta (viewTop - agentTop) for background sync
    //   goHome       : true if user scrolled back to live bottom while agent is scrolled up
    //   needPrefetch : true if approaching unseen top edge and should prefetch ahead
    function scroll(deltaLines, now = performance.now()) {
      if (!lines.length || !deltaLines) {
        return { hit: false, renderLines: null, sendTicks: deltaLines, syncDelta: 0, goHome: false, needPrefetch: false };
      }
      lastUserScrollAt = now;
      const h = visibleBodyHeight(false, viewTop);
      const maxT = Math.max(0, lines.length - h);
      const want = viewTop + deltaLines;

      // Scrolling DOWN to or past live bottom
      if (deltaLines > 0 && want >= maxT) {
        if (!seenBottom) {
          // Pane opened while already scrolled up: forward downward ticks to
          // discover unseen newer lines below our cached window.
          const ticks = Math.min(8, Math.max(1, Math.ceil(Math.max(deltaLines, 4) / linesPerTick)));
          return {
            hit: false,
            renderLines: null,
            sendTicks: ticks,
            syncDelta: 0,
            goHome: false,
            needPrefetch: false,
          };
        }
        const wasAtBottom = atBottom;
        viewTop = maxT;
        atBottom = true;
        pendingUpDemand = 0;
        const needHome = agentTop < maxT || !wasAtBottom;
        return {
          hit: true,
          renderLines: compose(viewTop, true),
          sendTicks: 0,
          syncDelta: 0,
          goHome: needHome,
          needPrefetch: false,
        };
      }

      // Scrolling within cached range [0 .. maxT]: INSTANT CACHE HIT!
      if (want >= 0 && want <= maxT) {
        viewTop = want;
        atBottom = want >= maxT;
        const needPrefetch = deltaLines < 0 && !hitTop && !prefetchInFlight && viewTop <= PREFETCH_MARGIN;
        if (needPrefetch) {
          prefetchInFlight = true;
          pendingUpSentAt = now;
        }
        return {
          hit: true,
          renderLines: compose(viewTop, atBottom),
          sendTicks: needPrefetch ? -Math.max(4, Math.ceil(8 / linesPerTick)) : 0,
          syncDelta: viewTop - agentTop,
          goHome: false,
          needPrefetch,
        };
      }

      // Scrolling UP past top of cache (want < 0)
      if (hitTop) {
        viewTop = 0;
        atBottom = maxT === 0;
        return {
          hit: true,
          renderLines: compose(0, atBottom),
          sendTicks: 0,
          syncDelta: 0 - agentTop,
          goHome: false,
          needPrefetch: false,
        };
      }

      // Cold cache miss above index 0: show top of cache immediately (if not
      // already there) AND immediately fetch a burst (with prefetch headroom).
      const alreadyAtZero = viewTop === 0;
      const shortfall = Math.abs(want);
      viewTop = 0;
      atBottom = false;
      pendingUpDemand = Math.min(32, pendingUpDemand + shortfall);
      pendingUpSentAt = now;
      prefetchInFlight = true;
      // Fetch requested lines + prefetch headroom (bounded by safe BURST = 8).
      const wantFetchLines = Math.max(shortfall + 6, 8);
      const ticks = -Math.min(8, Math.max(1, Math.ceil(wantFetchLines / linesPerTick)));
      return {
        hit: !alreadyAtZero,
        renderLines: alreadyAtZero ? null : compose(0, false),
        sendTicks: ticks,
        syncDelta: 0,
        goHome: false,
        needPrefetch: true,
      };
    }

    // Real scrollbar position & proportion metadata.
    // Uses a consistent viewport height (`baseBodyRows || bodyRows`) so ratio
    // never jumps or stops short of 1.0 when sticky headers appear/disappear.
    function scrollbar() {
      const h = Math.max(1, baseBodyRows || bodyRows || visibleBodyHeight(atBottom, viewTop));
      const cached = lines.length;
      const unknownBelow = seenBottom ? 0 : Math.max(h, estBelow);
      const unknownAbove = hitTop
        ? 0
        : seenBottom
          ? Math.max(0, Math.max(totalHint, cached + (cached > h ? Math.ceil(h * 0.5) : h)) - cached)
          : Math.max(0, totalHint - cached - unknownBelow);
      const total = Math.max(h, cached + unknownAbove + unknownBelow);
      const maxOffset = Math.max(0, total - h);
      const maxCacheTop = Math.max(0, cached - h);
      const clampedView = Math.min(viewTop, maxCacheTop);
      const offset = atBottom ? maxOffset : Math.max(0, Math.min(maxOffset, unknownAbove + clampedView));
      const ratio = maxOffset > 0 ? offset / maxOffset : 1;
      const thumbRatio = Math.min(1, h / total);
      return {
        visible: total > h || !hitTop || !seenBottom,
        total,
        viewport: h,
        offset,
        maxOffset,
        ratio,
        thumbRatio,
        hitTop,
        atBottom,
        cached,
        unknownAbove,
        unknownBelow,
      };
    }

    function setHistHint(n) {
      if (n > 0 && n > totalHint) {
        totalHint = n;
        // If we opened on a confirmed top banner (hitTop) and server history
        // proves the transcript extends past the visible viewport, expose the
        // scrollbar immediately at ratio=0 instead of hiding it.
        if (hitTop && viewTop === 0 && lines.length > 0 && lines.length < n - 2 && seenBottom && atBottom) {
          seenBottom = false;
          atBottom = false;
          estBelow = Math.max(estBelow, n - lines.length);
        }
        return true;
      }
      return false;
    }

    // Drag or click the real scrollbar to `ratio` (0 = top, 1 = bottom).
    function scrollToRatio(ratio, now = performance.now()) {
      const r = Math.max(0, Math.min(1, ratio));
      const sb = scrollbar();
      lastUserScrollAt = now;
      const maxBottomTop = maxViewTopFor(true);
      if (r >= 0.96 || sb.maxOffset === 0) {
        const wasAtBottom = atBottom;
        const wasSeenBottom = seenBottom;
        viewTop = maxBottomTop;
        atBottom = true;
        seenBottom = true;
        estBelow = 0;
        pendingPageUpLines = 0;
        pendingUpDemand = 0;
        return {
          hit: true,
          renderLines: compose(viewTop, true),
          goHome: !wasSeenBottom || agentTop < viewTop || !wasAtBottom,
          pageUp: 0,
          sendTicks: 0,
          syncDelta: 0,
        };
      }
      const targetVirtual = Math.round(r * sb.maxOffset);
      const cacheIdx = targetVirtual - sb.unknownAbove;
      const maxT = maxViewTopFor(false);

      if (seenBottom && cacheIdx >= maxBottomTop) {
        const wasAtBottom = atBottom;
        viewTop = maxBottomTop;
        atBottom = true;
        pendingUpDemand = 0;
        return {
          hit: true,
          renderLines: compose(viewTop, true),
          goHome: agentTop < viewTop || !wasAtBottom,
          pageUp: 0,
          sendTicks: 0,
          syncDelta: 0,
        };
      }

      if (cacheIdx >= 0 && cacheIdx <= maxT) {
        viewTop = cacheIdx;
        atBottom = false;
        return {
          hit: true,
          renderLines: compose(viewTop, false),
          goHome: false,
          pageUp: 0,
          sendTicks: 0,
          syncDelta: viewTop - agentTop,
        };
      }

      // Target is in unseen history above our cached window!
      const needLines = Math.max(1, -cacheIdx);
      viewTop = 0;
      atBottom = false;
      pendingUpSentAt = now;
      prefetchInFlight = true;
      const pageUp = needLines >= sb.viewport ? Math.min(6, Math.max(1, Math.ceil(needLines / sb.viewport))) : 0;
      if (pageUp) pendingPageUpLines = Math.max(pendingPageUpLines, Math.max(sb.viewport, sb.maxOffset - targetVirtual));
      const sendTicks = pageUp ? 0 : -Math.min(8, Math.max(2, Math.ceil(needLines / linesPerTick)));
      return {
        hit: true,
        renderLines: compose(0, false),
        goHome: false,
        pageUp,
        sendTicks,
        syncDelta: 0,
      };
    }

    function noteNoChangeAfterUp() {
      if (!prefetchInFlight && !pendingUpDemand) return false;
      prefetchInFlight = false;
      pendingUpDemand = 0;
      pendingUpSentAt = 0;
      if (agentTop === 0) {
        noOpUpCount++;
        // If server history hint indicates substantially more unseen lines
        // above, require two confirmed no-op POSTs before collapsing to hitTop.
        const likelyMoreAbove = totalHint > lines.length + Math.max(8, bodyRows);
        if (!likelyMoreAbove || noOpUpCount >= 2) {
          hitTop = true;
          return true;
        }
      }
      return false;
    }

    function returnToLive() {
      const h = visibleBodyHeight(true, lines.length);
      viewTop = Math.max(0, lines.length - h);
      atBottom = true;
      seenBottom = true;
      estBelow = 0;
      pendingPageUpLines = 0;
      pendingUpDemand = 0;
      prefetchInFlight = false;
      lastUserScrollAt = 0;
    }

    function noteAgentSync(deltaTicks) {
      // Called when background sync sends ticks toward viewTop.
      if (!deltaTicks) return;
      agentTop = Math.max(0, Math.min(maxViewTopFor(false), agentTop + Math.round(deltaTicks * linesPerTick)));
    }

    function state() {
      return { pane, kind, cols, paneRows, cached: lines.length, viewTop, agentTop, hitTop, atBottom, totalHint, bodyRows, linesPerTick };
    }

    return {
      reset,
      ingest,
      scroll,
      scrollToRatio,
      scrollbar,
      setHistHint,
      noteNoChangeAfterUp,
      returnToLive,
      noteAgentSync,
      state,
    };
  }

  window.collieScreenCache = {
    applyFramePatch,
    polishClaudeRow0,
    segmentFrame,
    findOverlap,
    lineKey,
    hasOverlayPill,
    createScreenCache,
  };
})();

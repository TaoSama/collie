// Reading Collie's task files (memory/tasks/<slug>.md) for the task panel.
// Collie writes them loosely: "# slug" or "# slug · title (t123)", a 目标
// line, a 下一步 line and timestamped "- HH:MM …" entries, newest last.
// Only what a file says plainly is read out of it; the rest stays unsaid.
(() => {
  // A trigger or event id in brackets, or bare: (t2609), （#t4109 实际已送达）.
  const REF_RE = /[（(][^（）()]*#?\bt\d+\b[^（）()]*[）)]|#t\d+\b/g;
  const field = (lines, key) => {
    const re = new RegExp(`^[-*>\\s]*(?:\\*\\*)?${key}(?:\\*\\*)?\\s*[:：]\\s*(.+)$`);
    for (const l of lines) { const m = l.match(re); if (m) return m[1].trim(); }
    return "";
  };
  const tidy = (s) => s.replace(REF_RE, "").replace(/`|\*\*/g, "").replace(/\s{2,}/g, " ").trim();
  // The latest timestamped entry: what happened, not what is planned.
  const latest = (lines) => {
    const stamped = lines.filter((l) => /^[-*]\s*\d{1,2}:\d{2}\s/.test(l));
    return stamped.length ? stamped[stamped.length - 1] : "";
  };
  // What the newest entry says it waits on: someone has been asked
  // (问人, 待人看图), or it is merged and goes out with a coming release.
  const ASK_RE = /问人|等人|待人|人在场|给人选|待你|问你/;
  const SHIP_RE = /合\s*main|排队发版|待发版|随\s*\.\d+|随\s*v[\d.]+\s*上线/;

  function parse(name, text) {
    const slug = name.replace(/^tasks\//, "").replace(/\.md$/, "");
    const lines = String(text || "").split("\n").map((l) => l.trim());
    const h1 = (lines.find((l) => /^#\s/.test(l)) || "").replace(/^#+\s*/, "");
    const goal = tidy(field(lines, "目标"));
    const next = tidy(field(lines, "下一步"));
    // The heading less the slug it repeats; a bare slug says nothing more,
    // so the goal's first sentence names the task instead (and the goal is
    // not repeated under it).
    let title = tidy(h1.startsWith(slug) ? h1.slice(slug.length).replace(/^\s*[·:：\-—|]\s*/, "") : h1);
    if (!title && goal) title = goal.split(/[。；;]|→/)[0].trim();
    const last = latest(lines);
    const phase = ASK_RE.test(last) ? "ask" : SHIP_RE.test(last) ? "ship" : "";
    return { slug, title: title || slug, next, goal: goal.startsWith(title) ? "" : goal, phase };
  }

  window.collieTasks = { parse };
})();

// Runs of replies in the chat with Collie: messages joined by their quotes
// (re.entry), you answering Collie or Collie answering you. Three or more
// messages make a run. The main chat keeps its first and latest messages
// and folds the rest; the whole run reads in a panel of its own.
(() => {
  const MIN_RUN = 3;
  const talk = (e) => e && (e.role === "user" || e.role === "collie");

  // entries in the order they were said; keyOf gives each message a key
  // (its id, or a stand-in while it is on its way).
  function group(entries, keyOf) {
    const byKey = new Map(), other = new Set();
    for (const e of entries) {
      if (talk(e)) byKey.set(keyOf(e), e);
      else if (e?.id) other.add(e.id); // a worker's line, an event: no reply
    }
    // What a message answers: another message in the chat, or one too early
    // to be loaded (then only its quote is known).
    const parentOf = (e) => {
      const id = e.re?.entry;
      return !id || e.re.worker || other.has(id) ? "" : id;
    };
    const rootCache = new Map(), stubs = new Map();
    // Every message on the way up shares the root: one walk per run.
    const rootOf = (k) => {
      const seen = [];
      let cur = k;
      while (!rootCache.has(cur)) {
        seen.push(cur);
        const e = byKey.get(cur);
        const p = e ? parentOf(e) : "";
        if (!p || seen.includes(p)) break;
        if (!byKey.has(p)) stubs.set(p, { id: p, who: e.re.who || "", text: e.re.text || "", stub: true });
        cur = p;
      }
      const root = rootCache.get(cur) ?? cur;
      for (const x of seen) rootCache.set(x, root);
      return root;
    };
    const runs = new Map();
    for (const e of entries) {
      if (!talk(e)) continue;
      const k = keyOf(e), r = rootOf(k);
      if (!runs.has(r)) runs.set(r, { key: r, root: byKey.get(r) || stubs.get(r), items: [] });
      if (k !== r) runs.get(r).items.push(e);
    }
    const threads = new Map(), member = new Map(), folded = new Set(), lastOf = new Map();
    for (const [r, t] of runs) {
      if (!t.items.length || t.items.length + 1 < MIN_RUN) continue;
      t.latest = t.items[t.items.length - 1];
      t.parentOf = parentOf;
      threads.set(r, t);
      member.set(r, r);
      for (const e of t.items) {
        const k = keyOf(e);
        member.set(k, r);
        if (e !== t.latest) folded.add(k);
      }
      lastOf.set(keyOf(t.latest), r);
    }
    return { threads, member, folded, lastOf };
  }

  window.collieThreads = { group };
})();

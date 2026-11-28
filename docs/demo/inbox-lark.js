// Lark inbox message actions and topic navigation.
(() => {
  const { svg: ico } = window.collieIcons;
  const LIKE = "THUMBSUP";
  const HOLD_MS = 470;
  const HOLD_SLOP = 10;
  const EMOJI_FALLBACK = {
    THUMBSUP: "👍", HEART: "❤️", LAUGH: "😂", THANKS: "🙏", EYES: "👀", PARTY: "🎉",
    OK: "👌", SMILE: "🙂", OnIt: "👀", SOB: "😭", CLAP: "👏", FIRE: "🔥",
  };
  let esc;
  const rawID = (m) => m.source_id || m.id;
  const availableMessage = (m) => m?.id && (!m.id.startsWith("out:") || m.source_id) && !["system", "recalled"].includes(m.view?.kind);
  const summary = (m) => collieInboxMessage.legacyPreview(m?.text).replace(/\s+/g, " ").trim();
  const title = (m) => m?.view?.title || [...summary(m)].slice(0, 48).join("") || "话题";
  const catalog = (lark) => new Map((lark?.emoji || []).map((e) => [e.key, e]));
  const emojiLabel = (e, key) => e?.label || EMOJI_FALLBACK[key] || key;
  const emojiIcon = (e, key) => {
    const text = esc(EMOJI_FALLBACK[key] || e?.label || key);
    const url = e?.url || "";
    return `<span class="lark-inbox-emoji" aria-hidden="true">${url.startsWith("/api/inbox/avatar?") ? `<img src="${esc(url)}" alt=""><span>${text}</span>` : text}</span>`;
  };
  function reactions(m, lark, entries = m.reactions) {
    if (!entries?.length) return "";
    const emojis = catalog(lark);
    return `<div class="lark-inbox-reactions">${entries.map((r) => {
      const names = (r.names || []).filter(Boolean);
      const count = Math.max(r.count || 0, names.length);
      const label = names.length === count && count > 0 && count <= 2 ? names.join("、") : `${count} 人`;
      const emoji = emojis.get(r.key);
      const name = emojiLabel(emoji, r.key);
      return `<button type="button" class="lark-inbox-reaction" data-lark-reaction="${esc(r.key)}"
        aria-pressed="${!!r.mine}" aria-label="${r.mine ? "撤销" : "回应"}${esc(name)}，${esc(label)}" title="${esc(names.join("、") || name)}">
        ${emojiIcon(emoji, r.key)}<span>${esc(label)}</span></button>`;
    }).join("")}</div>`;
  }
  const messageAttrs = (m, lark) => lark && availableMessage(m) ? ' data-lark-message tabindex="0" aria-keyshortcuts="Shift+F10 ContextMenu"' : "";
  const button = (attr, icon, text, attrs = "") => `<button type="button" ${attr} ${attrs}>${ico(icon)}<span>${esc(text)}</span></button>`;
  const openButton = (m) => m.thread_id && (!m.root_id || m.root_id === rawID(m))
    ? button("data-lark-open-topic", "message-square-text", `${m.reply_count || 0} 条评论`, 'class="conv-chip-btn lark-topic-link"')
    : "";
  const subscribeButton = (m, head = false) => button("data-lark-subscribe", m.subscribed ? "check" : "bookmark-plus", m.subscribed ? "已订阅" : "订阅",
    `aria-pressed="${!!m.subscribed}"${head ? ` class="btn small lark-topic-follow" data-msg-id="${esc(m.id)}"` : ""}`);
  const isRoot = (m) => availableMessage(m) && m.thread_id && (!m.root_id || m.root_id === rawID(m));
  const commentsIn = (msgs, root) => msgs.filter((m) => rawID(m) !== rawID(root) && (m.thread_id === root.thread_id || m.root_id === rawID(root)));

  function topicCard(m, c, lark, clock, detail = false, unreadAnchor = false) {
    const like = m.reactions?.find((r) => r.key === LIKE);
    const heading = title(m);
    let body = "";
    if (detail) {
      const view = m.view?.kind === "post" ? { ...m.view, title: "" } : m.view;
      body = view ? collieInboxMessage.render(view, { convId: c.id, msgId: m.id }) : esc(m.text);
    } else {
      const text = summary(m);
      const preview = text === heading ? "" : text.startsWith(heading) ? text.slice(heading.length).trim() : text;
      body = esc(preview.length > 240 ? preview.slice(0, 240).trimEnd() + "…" : preview);
    }
    return `<li class="lark-topic-card" data-msg-id="${esc(m.id)}"${unreadAnchor ? " data-unread-anchor" : ""}>
      <div class="lark-topic-body${detail ? " im-rich" : ""}"${messageAttrs(m, lark)}>
        <div class="lark-topic-meta"><span class="conv-ava" aria-hidden="true">${esc((m.sender_name || "?").slice(0, 1))}${m.avatar ? `<img class="inbox-pic" src="${esc(m.avatar)}" alt="" loading="lazy" decoding="async">` : ""}</span>
          <b>${esc(m.mine ? "你" : m.sender_name || "")}</b><time>${esc(clock(m.at))}</time></div>
        ${detail ? `<h3>${esc(heading)}</h3>` : `<h3><button type="button" data-lark-open-topic>${esc(heading)}</button></h3>`}
        ${body ? `<div class="lark-topic-content">${body}</div>` : ""}
        ${reactions(m, lark, m.reactions?.filter((r) => r.key !== LIKE))}
      </div>
      <div class="lark-topic-acts">
        ${button("data-lark-like", "thumbs-up", `${like?.mine ? "已赞" : "点赞"}${like?.count ? ` ${like.count}` : ""}`, `aria-pressed="${!!like?.mine}"`)}
        ${button("data-lark-open-topic data-lark-comment", "message-square-text", `评论${m.reply_count ? ` ${m.reply_count}` : ""}`)}
        ${button("data-lark-forward data-lark-topic-forward", "forward", "转发")}
        ${subscribeButton(m)}
      </div>
    </li>`;
  }

  function topicFeed(msgs, c, lark, clock, renderMessage, filter, unreadID, unreadCount) {
    const roots = msgs.filter(isRoot);
    const rootIDs = new Set(roots.map(rawID));
    const threadIDs = new Set(roots.map((m) => m.thread_id));
    const orphan = (m) => !rootIDs.has(m.root_id) && !threadIDs.has(m.thread_id);
    const records = filter === "subscribed" ? roots.filter((m) => m.subscribed) : msgs.filter((m) => isRoot(m) || orphan(m));
    const tabs = [["all", "全部"], ["subscribed", "我订阅的"]].map(([key, text]) =>
      `<button type="button" data-lark-filter="${key}" aria-pressed="${filter === key}">${text}</button>`).join("");
    const unread = msgs.find((m) => m.id === unreadID);
    const cards = [...records].reverse().map((m, i, list) => {
      const anchor = unread && unreadCount > 0 && (m === unread ||
        (isRoot(m) && (m.thread_id === unread.thread_id || rawID(m) === unread.root_id)));
      return isRoot(m) ? topicCard(m, c, lark, clock, false, anchor) : renderMessage(m, i, list, anchor);
    }).join("");
    return `<div class="lark-topic-tabs" aria-label="筛选话题">${tabs}</div>
      <ul class="conv-msgs lark-topic-list" aria-label="话题列表">${cards || `<li class="empty-row">${filter === "subscribed" ? "暂无订阅的话题" : "暂无话题"}</li>`}</ul>`;
  }

  function topicDetail(root, msgs, c, lark, clock, renderMessage) {
    const comments = commentsIn(msgs, root);
    return `<ul class="conv-msgs lark-topic-list" aria-label="话题详情">
      ${topicCard(root, c, lark, clock, true)}
      <li class="lark-topic-comments">评论 <span>${root.reply_count ?? comments.length}</span></li>
      ${comments.map(renderMessage).join("")}
      ${comments.length ? "" : '<li class="empty-row">暂无评论</li>'}
    </ul>`;
  }

  function quoteHTML(reply) {
    return reply?.message ? `<div class="lark-inbox-quote"><div><b>${reply.mode === "create_topic" ? "创建话题" : "回复"}${reply.message.sender_name ? ` · ${esc(reply.message.mine ? "你" : reply.message.sender_name)}` : ""}</b>
      <span>${esc(summary(reply.message) || title(reply.message))}</span></div>
      <button type="button" class="icon-btn" data-lark-cancel-reply aria-label="取消引用" title="取消引用">${ico("x")}</button></div>` : "";
  }

  function bind(root, hooks) {
    const dialog = document.createElement("dialog");
    dialog.className = "lark-inbox-menu";
    dialog.setAttribute("aria-label", "消息操作");
    document.body.append(dialog);

    let convID = "", target = null, trigger = null, down = null, heldTouch = false, ignoreClick = 0;
    let topic = null, filter = "all", reply = null, publish = false, topicRequest = null;
    const pending = new Set();
    const getDetail = () => hooks.detail();
    const isCurrent = () => !!convID && getDetail()?.conv?.id === convID;
    const allMessages = () => [...(topic?.msgs || []), ...(getDetail()?.msgs || [])];
    const getMessage = (id) => allMessages().find((m) => m.id === id || rawID(m) === id);
    const rootFor = () => topic && (getMessage(rawID(topic.root)) || topic.root);
    const focusInput = () => root.querySelector("#inbox-detail [data-conv-input]")?.focus({ preventScroll: true });
    const redraw = () => hooks.draw();
    const fixed = (d, rect) => {
      const width = d.offsetWidth, height = d.offsetHeight;
      const x = Math.min(Math.max(8, rect.left), window.innerWidth - width - 8);
      const below = rect.bottom + 7;
      const y = below + height <= window.innerHeight - 8 ? below : Math.max(8, rect.top - height - 7);
      d.style.setProperty("--lark-menu-x", `${x}px`);
      d.style.setProperty("--lark-menu-y", `${y}px`);
    };
    const clearHold = () => {
      if (down) clearTimeout(down.timer);
      down = null;
    };
    const close = () => { if (dialog.open) dialog.close(); };
    function reset(id = "") {
      close();
      clearHold();
      convID = id;
      topic = reply = topicRequest = null;
      filter = "all";
      publish = false;
      ignoreClick = 0;
    }

    function show(kind, el = trigger) {
      if (!target || !el || !isCurrent()) return;
      const d = getDetail(), emojis = catalog(d.lark);
      const quick = [...new Set([...(d.lark.recent_emoji || []), ...emojis.keys()])].filter((key) => emojis.has(key)).slice(0, 6);
      const pick = (key) => `<button type="button" data-lark-emoji="${esc(key)}" aria-label="${esc(emojiLabel(emojis.get(key), key))}"
        title="${esc(emojiLabel(emojis.get(key), key))}" aria-pressed="${!!target.reactions?.some((r) => r.key === key && r.mine)}">${emojiIcon(emojis.get(key), key)}</button>`;
      const heading = (text) => `<div class="lark-menu-head"><b>${text}</b><button type="button" class="icon-btn" data-lark-menu-close aria-label="关闭">${ico("x")}</button></div>`;
      const forwardTitle = target.forwardThread ? "转发话题" : "转发消息";
      dialog.classList.toggle("forward", kind === "forward");
      dialog.setAttribute("aria-label", kind === "menu" ? "消息操作" : kind === "emoji" ? "表情回应" : forwardTitle);
      if (kind === "menu") {
        const topicHome = d.lark.topic_mode && !topic;
        const actions = [
          ["quote", "message-square-reply", "回复"],
          ["forward", "forward", "转发"],
          ["create_topic", "file-text", "创建话题"],
          ["copy", "copy", "复制"],
        ].map(([action, icon, text]) => {
          const disabled = (action === "create_topic" && (d.conv.kind !== "group" || d.lark.topic_mode || target.thread_id)) ||
            (action === "quote" && topicHome && !isRoot(target));
          return button(`data-lark-menu-action="${action}"`, icon, text, disabled ? "disabled" : "");
        }).join("");
        dialog.innerHTML = `<div class="lark-menu-quick">${quick.map(pick).join("")}
          <button type="button" data-lark-more aria-label="更多表情" title="更多表情">${ico("ellipsis")}</button></div>
          <div class="lark-menu-actions">${actions}</div>`;
      } else if (kind === "emoji") {
        dialog.innerHTML = `${heading("表情回应")}${quick.length ? `<h3>常用表情</h3><div class="lark-menu-grid">${quick.map(pick).join("")}</div>` : ""}
          <h3>全部表情</h3><div class="lark-menu-grid">${[...emojis.keys()].map(pick).join("")}</div>
          ${emojis.size ? "" : '<p class="muted">暂时无法读取表情</p>'}`;
      } else {
        const items = hooks.convs().filter((c) => c.ext === "larkx" && c.kind !== "doc" && c.send_as !== "none");
        dialog.innerHTML = `${heading(forwardTitle)}
          <label for="lark-forward-target">转发到</label>
          <select id="lark-forward-target" data-lark-forward-target><option value="">选择飞书会话</option>
          ${items.map((c) => `<option value="${esc(c.id)}">${esc(c.name || c.chat)}</option>`).join("")}</select>
          <div class="lark-forward-acts"><button type="button" class="btn ghost" data-lark-menu-close>取消</button>
          <button type="button" class="btn primary" data-lark-forward-confirm disabled>确认转发</button></div>`;
      }
      trigger = el;
      if (!dialog.open) dialog.showModal();
      fixed(dialog, el.getBoundingClientRect());
      dialog.querySelector("button:not([disabled])")?.focus({ preventScroll: true });
    }

    function openMessage(el) {
      const msg = getMessage(el.closest("[data-msg-id]")?.dataset.msgId);
      if (!availableMessage(msg) || !getDetail()?.lark) return;
      target = msg;
      show("menu", el);
      return dialog.open;
    }
    function openHeldMessage(el) {
      if (!openMessage(el)) return;
      heldTouch = true;
      ignoreClick = performance.now() + 350;
    }
    function findBubble(e) {
      if (!e.target.closest || !getDetail()?.lark ||
          e.target.closest("a, button, select, input, textarea, [contenteditable]") || window.getSelection()?.toString()) return null;
      return e.target.closest("[data-lark-message]");
    }
    root.addEventListener("contextmenu", (e) => {
      const el = findBubble(e);
      if (!el) return;
      e.preventDefault();
      if (!dialog.open) {
        const isTouch = !!down;
        clearHold();
        if (isTouch) openHeldMessage(el);
        else if (openMessage(el) && e.pointerType === "touch") ignoreClick = performance.now() + 350;
      }
    });
    root.addEventListener("keydown", (e) => {
      if (e.key !== "ContextMenu" && !(e.key === "F10" && e.shiftKey) && e.key !== "Enter") return;
      const el = findBubble(e);
      if (!el || e.target !== el) return;
      e.preventDefault();
      openMessage(el);
    });
    root.addEventListener("pointerdown", (e) => {
      if (e.pointerType !== "touch" || !e.isPrimary) return;
      const el = findBubble(e);
      if (!el) return;
      clearHold();
      heldTouch = false;
      down = { x: e.clientX, y: e.clientY, timer: setTimeout(() => {
        if (el.isConnected) openHeldMessage(el);
      }, HOLD_MS) };
    }, { passive: true });
    root.addEventListener("pointermove", (e) => {
      if (down && Math.hypot(e.clientX - down.x, e.clientY - down.y) > HOLD_SLOP) clearHold();
    }, { passive: true });
    root.addEventListener("scroll", clearHold, { capture: true, passive: true });
    for (const type of ["pointerup", "pointercancel"]) document.addEventListener(type, clearHold, { passive: true });
    document.addEventListener("touchend", (e) => {
      if (!heldTouch) return;
      e.preventDefault();
      heldTouch = false;
      ignoreClick = performance.now() + 350;
    }, { capture: true, passive: false });
    document.addEventListener("touchcancel", () => { heldTouch = false; }, { capture: true, passive: true });
    dialog.addEventListener("pointerdown", () => { if (!heldTouch) ignoreClick = 0; });
    dialog.addEventListener("click", (e) => {
      if (heldTouch || performance.now() < ignoreClick) { e.preventDefault(); return; }
      if (e.target === dialog) { close(); return; }
      const b = e.target.closest("button");
      if (!b || b.disabled) return;
      if (b.hasAttribute("data-lark-menu-close")) { close(); return; }
      if (b.hasAttribute("data-lark-more")) { show("emoji"); return; }
      if (b.dataset.larkEmoji) { react(b, target, b.dataset.larkEmoji); return; }
      if (b.hasAttribute("data-lark-forward-confirm")) {
        const dest = dialog.querySelector("[data-lark-forward-target]")?.value;
        if (dest) write(b, target, { action: "forward", target: dest, thread_id: target.forwardThread || undefined });
        return;
      }
      dispatchMenu(b.dataset.larkMenuAction);
    });
    dialog.addEventListener("change", (e) => {
      if (e.target.hasAttribute("data-lark-forward-target")) dialog.querySelector("[data-lark-forward-confirm]").disabled = !e.target.value;
    });
    dialog.addEventListener("close", () => {
      if (dialog.open || !isCurrent()) return;
      redraw();
      if (trigger?.isConnected && document.activeElement === document.body) trigger.focus({ preventScroll: true });
    });
    document.addEventListener("error", (e) => {
      if (e.target.matches?.(".lark-inbox-emoji > img")) e.target.classList.add("failed");
    }, true);

    async function dispatchMenu(action) {
      if (!action || !target || !isCurrent()) return;
      if (action === "forward") { show("forward"); return; }
      const msg = target;
      close();
      if (action === "copy") {
        const text = collieInboxMessage.legacyPreview(msg.text) || trigger?.textContent?.trim() || "";
        if (!text) { hooks.toast("没有可以复制的文字", "attn"); return; }
        const ok = await hooks.copy(text);
        hooks.toast(ok ? "已复制" : "复制失败", ok ? "" : "attn");
      } else if (action === "quote" && getDetail()?.lark?.topic_mode && !topic) {
        if (isRoot(msg)) await openTopic(msg, true);
      } else {
        reply = { message: msg, mode: action === "quote" && topic ? "comment" : action };
        publish = false;
        hooks.replyMode();
        redraw();
        focusInput();
      }
    }

    async function refreshTopic(notify = false) {
      if (!topic || !isCurrent()) return;
      if (topicRequest?.topic === topic) return topicRequest.promise;
      const query = topic, id = convID;
      const request = { topic: query };
      topicRequest = request;
      request.promise = hooks.action({ id, action: "topic", thread_id: query.root.thread_id, message_id: query.root.id })
        .then((res) => { if (topicRequest === request && topic === query && isCurrent()) query.msgs = res.msgs || []; })
        .catch((err) => { if (notify && topicRequest === request && topic === query && isCurrent()) hooks.toast(err.message, "attn"); })
        .finally(() => { if (topicRequest === request) topicRequest = null; });
      return request.promise;
    }

    async function openTopic(msg, comment, fromHistory = false) {
      if (!msg.thread_id) return;
      if (topic?.root.thread_id !== msg.thread_id) {
        topic = { root: msg, msgs: commentsIn(getDetail().msgs || [], msg) };
        reply = null;
        publish = false;
        hooks.topicChanged(msg.thread_id, fromHistory);
      }
      if (comment) hooks.replyMode();
      const wanted = topic;
      redraw();
      if (comment) focusInput();
      await refreshTopic(true);
      if (topic !== wanted || !isCurrent()) return;
      redraw();
      if (comment) focusInput();
    }

    async function write(b, msg, body) {
      if (!msg || !isCurrent()) return;
      const id = convID, key = `${id}:${msg.id}:${body.action}:${body.key || ""}`;
      if (pending.has(key)) return;
      pending.add(key);
      const inMenu = dialog.open && dialog.contains(b);
      hooks.busy(b);
      let sent = false;
      try {
        await hooks.action({ id, message_id: msg.id, ...body });
        sent = true;
        if (body.action === "forward") hooks.toast("已转发");
        if (convID === id && isCurrent()) {
          if (inMenu && dialog.contains(b)) close();
          topicRequest = null;
          await hooks.reload();
        }
      } catch (err) {
        const transportFailure = err instanceof TypeError && err.status == null;
        const uncertain = !sent && body.action === "forward" && (err.code === "uncertain" || transportFailure);
        if (uncertain && inMenu && dialog.contains(b)) close();
        hooks.toast(uncertain && transportFailure ? "没有收到转发回执，消息可能已经转发，请到飞书确认" : err.message, "attn");
      } finally {
        pending.delete(key);
        hooks.busy(b, false);
      }
    }
    function react(b, msg, key) {
      return write(b, msg, { action: "reaction", key, active: !msg?.reactions?.some((r) => r.key === key && r.mine) });
    }

    function leaveTopic() {
      topic = reply = null;
      topicRequest = null;
      hooks.topicChanged("", true);
      redraw();
    }
    function syncTopic(threadID) {
      if (!isCurrent() || threadID === (topic?.root.thread_id || "")) return;
      if (!threadID) { leaveTopic(); return; }
      const m = getDetail().msgs?.find((m) => isRoot(m) && m.thread_id === threadID);
      if (m) openTopic(m, false, true);
    }

    function click(b) {
      if (!isCurrent()) return false;
      const data = b.dataset;
      const msg = getMessage(b.closest("[data-msg-id]")?.dataset.msgId);
      if (data.larkFilter) {
        filter = data.larkFilter;
        hooks.topicScroll();
        redraw();
      } else if (Object.hasOwn(data, "larkBack")) {
        if (!hooks.topicBack(topic?.root.thread_id)) leaveTopic();
      } else if (Object.hasOwn(data, "larkPublish")) {
        publish = true;
        reply = null;
        hooks.replyMode();
        redraw();
        focusInput();
      } else if (Object.hasOwn(data, "larkCancelReply")) {
        reply = null;
        publish = false;
        redraw();
        focusInput();
      } else if (msg && Object.hasOwn(data, "larkOpenTopic")) {
        openTopic(msg, Object.hasOwn(data, "larkComment"));
      } else if (msg && Object.hasOwn(data, "larkForward")) {
        target = { ...msg, forwardThread: Object.hasOwn(data, "larkTopicForward") ? msg.thread_id : "" };
        show("forward", b);
      } else if (msg && Object.hasOwn(data, "larkSubscribe")) {
        write(b, msg, { action: "subscribe", thread_id: msg.thread_id, active: !msg.subscribed });
      } else if (msg && Object.hasOwn(data, "larkLike")) {
        react(b, msg, LIKE);
      } else if (msg && data.larkReaction) {
        react(b, msg, data.larkReaction);
      } else return false;
      return true;
    }
    function sendOpts() {
      if (!isCurrent()) return null;
      if (reply) return { reply_to: reply.message.id, reply_mode: reply.mode };
      if (topic) return { reply_to: topic.root.id, reply_mode: "comment" };
      return getDetail()?.lark?.topic_mode ? { reply_mode: "publish" } : null;
    }
    function sent(opts) {
      if (!opts || !isCurrent()) return;
      if (reply?.message.id === opts.reply_to) reply = null;
      if (opts.reply_mode === "publish") publish = false;
      topicRequest = null;
    }
    const opts = () => isCurrent() ? { topicRoot: rootFor(), topicMsgs: topic?.msgs, topicFilter: filter, larkReply: reply, larkPublish: publish } : {};
    return { reset, close, refreshTopic, click, sendOpts, sent, opts, syncTopic, interacting: () => dialog.open };
  }

  window.collieInboxLark = { init: (escape) => { esc = escape; }, bind, reactions, messageAttrs, openButton, isRoot, topicFeed, topicDetail, quoteHTML, title, subscribeButton };
})();

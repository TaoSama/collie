// Picking a computer. Every place that asks which computer (派活, 新建会话,
// 设置, an extension's 运行在) offers the same list, in the same order,
// drawn the same way: <option>s the app's picker shows with a status dot,
// "hub" or the OS underneath, and which one is the default.
(() => {
  let esc = (s) => String(s ?? "");
  function init(h) { esc = h.esc; }

  const up = (m) => !!(m.local || m.online);

  // Computers by name, with the hub's first when it has any agent CLI and last when it has none.
  function order(list, isHub = (m) => m.local) {
    const hub = list.filter(isHub), rest = list.filter((m) => !isHub(m)).sort((a, b) => a.name.localeCompare(b.name));
    return hub.some((m) => m.agents?.length) ? [...hub, ...rest] : [...rest, ...hub];
  }

  // What a picker offers: this hub's computers that are up, and the one
  // picked (keep) even when it is not. others: other hubs' computers too.
  function choices(list, { keep, others = false, isUp = up } = {}) {
    return order(list.filter((m) => (others || !m.hub) && (isUp(m) || m.id === keep)));
  }

  function options(list, { cur, def, isUp = up } = {}) {
    return list.map((m) => {
      const on = isUp(m);
      const sub = [m.local ? "hub" : m.os || "", on ? "" : "离线", def !== undefined && m.id === def ? "默认" : ""].filter(Boolean).join(" · ");
      return `<option value="${esc(m.id)}" data-dot="${on ? "online" : "offline"}"${sub ? ` data-sub="${esc(sub)}"` : ""}${m.id === cur ? " selected" : ""}>${esc(m.name)}</option>`;
    }).join("");
  }

  window.collieMachines = { init, order, choices, options };
})();

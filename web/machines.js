// Picking a computer. Every place that asks which computer (派活, 新建会话,
// 设置, an extension's 运行在) offers the same list, in the same order,
// drawn the same way: <option>s the app's picker shows with a status dot,
// the name Collie knows it by (the hub is "hub", its host name on hover),
// what kind of computer it is underneath, and which one is the default.
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

  // "linux/amd64" as people say it: Linux · x86_64, macOS · Apple 芯片.
  const OS = { linux: "Linux", darwin: "macOS", windows: "Windows", freebsd: "FreeBSD" };
  const ARCH = { amd64: "x86_64", arm64: "ARM64", "386": "x86" };
  function kind(os) {
    const [o, a] = String(os || "").split("/");
    if (!o) return "";
    const arch = o === "darwin" ? { arm64: "Apple 芯片", amd64: "Intel" }[a] : ARCH[a];
    return [OS[o] || o, arch || a].filter(Boolean).join(" · ");
  }

  function options(list, { cur, def, isUp = up } = {}) {
    return list.map((m) => {
      const on = isUp(m);
      const sub = [kind(m.os), on ? "" : "离线", def !== undefined && m.id === def ? "默认" : ""].filter(Boolean).join(" · ");
      const name = m.local ? "hub" : m.name;
      return `<option value="${esc(m.id)}" data-dot="${on ? "online" : "offline"}"${sub ? ` data-sub="${esc(sub)}"` : ""}${name !== m.name ? ` title="${esc(m.name)}"` : ""}${m.id === cur ? " selected" : ""}>${esc(name)}</option>`;
    }).join("");
  }

  window.collieMachines = { init, order, choices, options };
})();

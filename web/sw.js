/* Collie's service worker: keeps the app shell so an installed app opens
   without waiting on the network. The hub stamps BUILD and SHELL when it
   serves this file (serviceWorker in server.go), so every release is a new
   script: the browser installs it with a fresh copy of the shell and drops the
   old copy. The page then offers a reload, as for any open page that sees a
   newer hub. API calls never go through here. */
"use strict";
const BUILD = "__BUILD__";
const SHELL = ["__SHELL__"];
const PREFIX = "collie-shell-";
const CACHE = PREFIX + BUILD;

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE)
    .then((c) => c.addAll(SHELL.map((u) => new Request(u, { cache: "reload" }))))
    .then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(caches.keys()
    .then((ks) => Promise.all(ks.filter((k) => k.startsWith(PREFIX) && k !== CACHE).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});

// The cache key a request is answered from, or null to leave it to the
// network: the page itself (with or without ?pair=…) and the shell's files.
function shellKey(req) {
  if (req.method !== "GET") return null;
  const u = new URL(req.url);
  if (u.origin !== location.origin) return null;
  if (req.mode === "navigate") return u.pathname === "/" || u.pathname === "/index.html" ? "/" : null;
  return SHELL.includes(u.pathname) || u.pathname.startsWith("/vendor/") ? u.pathname : null;
}

self.addEventListener("fetch", (e) => {
  const key = shellKey(e.request);
  if (key) e.respondWith(fromShell(key, e.request));
});

async function fromShell(key, req) {
  const c = await caches.open(CACHE);
  const hit = await c.match(key);
  if (hit) return hit;
  // Not kept yet (KaTeX fonts, or the cache was cleared on sign-out).
  const res = await fetch(req);
  if (res.ok) await c.put(key, res.clone());
  return res;
}

// A push from the hub (push.go): a decision that needs you, shown even when
// every page is closed. A page in front of you already shows it. Buttons
// answer it as on the page's own notifications, when all its choices fit.
self.addEventListener("push", (e) => {
  let n = {};
  try { n = e.data ? e.data.json() : {}; } catch (err) { console.warn("push payload:", err); }
  e.waitUntil(pushed(n));
});

const NOTE_CHOICE = 24; // a choice's length on a button (clipNote in app.js)
const clipNote = (s) => (s.length > NOTE_CHOICE ? s.slice(0, NOTE_CHOICE - 1) + "…" : s);

async function pushed(n) {
  const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  if (!n.always && wins.some((c) => c.visibilityState === "visible" && c.focused)) return;
  const choices = n.choices || [];
  const max = (self.Notification && Notification.maxActions) || 0;
  const fit = !!n.answer && choices.length > 0 && choices.length <= max;
  await self.registration.showNotification(n.title || "Collie", {
    body: n.body || "", icon: "/icon.svg", tag: n.tag || "", renotify: !!n.tag,
    data: { url: location.origin + "/" + (n.hash || ""), answer: fit ? n.answer : null, choices: fit ? choices : [] },
    actions: fit ? choices.map((c, i) => ({ action: String(i), title: clipNote(c) })) : [],
  });
}

// A notification opens what it is about (data.url, a #/d/… decision link):
// in the open page if there is one, else a new window (an installed app
// starts cold). One of its buttons answers the decision from here; when that
// fails (the dialog changed, already answered, signed out), the page opens.
self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  e.waitUntil(notificationClicked(e.notification.data || {}, e.action));
});

async function notificationClicked(d, action) {
  const choice = action && d.answer ? (d.choices || [])[+action] : undefined;
  if (choice !== undefined) {
    try {
      const r = await fetch("/api/super/decide", {
        method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...d.answer, choice }),
      });
      if (r.ok) return;
    } catch (err) { console.warn("answering from a notification:", err); }
  }
  await openPage(d.url || "/");
}

async function openPage(url) {
  const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  const w = wins.find((c) => c.frameType === "top-level" && new URL(c.url).origin === location.origin);
  if (w) {
    w.postMessage({ type: "open", url });
    await w.focus();
    return;
  }
  await self.clients.openWindow(url);
}

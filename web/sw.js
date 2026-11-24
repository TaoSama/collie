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

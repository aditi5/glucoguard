// Offline support. BUILD is replaced with the commit SHA at deploy time, so
// every release gets a fresh cache and old engine versions are dropped.
const CACHE = "glucoguard-__BUILD__";
const ASSETS = ["./", "index.html", "styles.css", "app.js", "wasm_exec.js", "engine.wasm",
  "manifest.webmanifest", "icon.svg", "icon-192.png", "icon-512.png"];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(ASSETS)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(caches.keys()
    .then((keys) => Promise.all(keys.filter((k) => k.startsWith("glucoguard-") && k !== CACHE).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});

// Network first, so a new engine version is used as soon as it is deployed;
// fall back to the cache when offline.
self.addEventListener("fetch", (e) => {
  if (e.request.method !== "GET" || new URL(e.request.url).origin !== location.origin) return;
  // cache: "no-cache" revalidates with the server instead of trusting the
  // browser's HTTP cache (GitHub Pages sets max-age=600), so a safety-rule
  // update reaches phones on the next load. Navigation requests can't be
  // re-constructed with an init, so fetch those by URL.
  const req = e.request.mode === "navigate"
    ? fetch(e.request.url, { cache: "no-cache", credentials: "same-origin" })
    : fetch(e.request, { cache: "no-cache" });
  e.respondWith(
    req
      .then((res) => {
        if (res.ok) { const copy = res.clone(); caches.open(CACHE).then((c) => c.put(e.request, copy)); }
        return res;
      })
      .catch(() => caches.match(e.request, { ignoreSearch: true }))
  );
});

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  e.waitUntil(self.clients.matchAll({ type: "window" }).then((cs) => (cs[0] ? cs[0].focus() : self.clients.openWindow("./"))));
});

// millivolt dashboard service worker. Cache name is stamped with the
// dashboard content identity so a rebuild drops the previous shell.
// The worker never caches live HTML or operator/inference APIs: / is
// operator-gated (login vs bootstrap) and /metrics /admin /v1 stream.
const CACHE = 'millivolt-shell-__DASHBOARD_VERSION__';
const SHELL = [
  '/favicon.ico',
  '/favicon.svg',
  '/apple-touch-icon.png',
  '/icon-192.png',
  '/icon-512.png',
  '/icon-192-maskable.png',
  '/icon-512-maskable.png',
  '/manifest.webmanifest',
];

function livePath(pathname) {
  return pathname === '/' || pathname === '/index.html' ||
    pathname === '/healthz' || pathname === '/models' ||
    pathname === '/sw.js' ||
    pathname === '/admin' || pathname.startsWith('/admin/') ||
    pathname === '/metrics' || pathname.startsWith('/metrics/') ||
    pathname === '/v1' || pathname.startsWith('/v1/');
}

function shellPath(pathname) {
  return pathname.startsWith('/dash/') ||
    pathname === '/favicon.ico' || pathname === '/favicon.svg' ||
    pathname === '/apple-touch-icon.png' || pathname === '/manifest.webmanifest' ||
    pathname.startsWith('/icon-');
}

function offlinePage() {
  return new Response(
    '<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="theme-color" content="#1b1826"><title>millivolt</title><body style="margin:0;min-height:100dvh;display:grid;place-items:center;background:#141210;color:#e8e4de;font:14px/1.5 sans-serif"><p style="max-width:22rem;padding:20px;color:#97907e">millivolt is a live instrument and needs a network to open.</p></body></html>',
    { headers: { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' } }
  );
}

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(CACHE);
    await cache.addAll(SHELL);
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const keys = await caches.keys();
    await Promise.all(keys.filter((k) => k.startsWith('millivolt-shell-') && k !== CACHE).map((k) => caches.delete(k)));
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;
  let url;
  try { url = new URL(req.url); } catch { return; }
  if (url.origin !== self.location.origin) return;
  // Cover start_url for installability without storing login or bootstrap HTML.
  if (req.mode === 'navigate' && (url.pathname === '/' || url.pathname === '/index.html')) {
    event.respondWith((async () => {
      try { return await fetch(req); } catch { return offlinePage(); }
    })());
    return;
  }
  if (livePath(url.pathname)) return;
  event.respondWith((async () => {
    const cache = await caches.open(CACHE);
    try {
      const fresh = await fetch(req);
      if (fresh.ok && shellPath(url.pathname)) cache.put(req, fresh.clone());
      return fresh;
    } catch (err) {
      const hit = await cache.match(req);
      if (hit) return hit;
      throw err;
    }
  })());
});

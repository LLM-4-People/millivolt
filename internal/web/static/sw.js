// millivolt dashboard service worker. Cache name is stamped with the
// dashboard content identity so a rebuild drops the previous shell.
// The worker never caches live HTML or operator/inference APIs: / is
// operator-gated (login vs bootstrap) and /metrics /admin /session /v1
// stream.
// Cached shell keys share CACHE_PREFIX; the stamped version completes it.
const CACHE_PREFIX = 'millivolt-shell-';
const CACHE = CACHE_PREFIX + '__DASHBOARD_VERSION__';
// The shell stamps every local /dash/ URL with the same version. Derive it
// from CACHE, the single owner, instead of repeating the literal.
const CACHE_VERSION = CACHE.slice(CACHE_PREFIX.length);
// One list of the exact shell assets: the install-time precache array and
// shellPath's exact-name check both derive from it, so the two cannot drift.
// The prefix rules stay separate (shellPath accepts /dash/ paths and any
// /icon- size the precache does not enumerate).
const SHELL_ASSETS = [
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
    pathname === '/session' || pathname.startsWith('/session/') ||
    pathname === '/metrics' || pathname.startsWith('/metrics/') ||
    pathname === '/v1' || pathname.startsWith('/v1/');
}

function shellPath(pathname) {
  return pathname.startsWith('/dash/') ||
    SHELL_ASSETS.includes(pathname) ||
    pathname.startsWith('/icon-');
}

// A /dash/ URL carrying exactly the stamped version names immutable bytes:
// the server serves it private/immutable, and a rebuild changes the version
// before it can change the bytes under that URL. Any missing, stale or
// duplicate v stays network-first.
function exactVersionedDash(url) {
  if (!url.pathname.startsWith('/dash/')) return false;
  const versions = url.searchParams.getAll('v');
  return versions.length === 1 && versions[0] === CACHE_VERSION;
}

function offlinePage() {
  return new Response(
    '<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="theme-color" content="#100e0b"><title>millivolt</title><body style="margin:0;min-height:100dvh;display:grid;place-items:center;background:#141210;color:#e8e4de;font:14px/1.5 sans-serif"><p style="max-width:22rem;padding:20px;color:#97907e">millivolt is a live instrument and needs a network to open.</p></body></html>',
    { headers: { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' } }
  );
}

// CacheStorage is an optional accelerator, never a dependency. An open,
// match, or put rejection is a cache miss: the network path stays
// authoritative, and a failed offline fallback preserves the original
// network error for the caller.
async function cacheBestEffort(operation) {
  try { return await operation(); } catch { return undefined; }
}

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(CACHE);
    await cache.addAll(SHELL_ASSETS);
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const keys = await caches.keys();
    await Promise.all(keys.filter((k) => k.startsWith(CACHE_PREFIX) && k !== CACHE).map((k) => caches.delete(k)));
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
  // Exact-version /dash/ assets are content-addressed by the stamped v, so
  // answer from the stamped cache first and store the first successful fetch.
  // Unversioned shell and brand paths remain network-first with cache fallback.
  if (exactVersionedDash(url)) {
    event.respondWith((async () => {
      const cache = await cacheBestEffort(() => caches.open(CACHE));
      const hit = cache ? await cacheBestEffort(() => cache.match(req)) : undefined;
      if (hit) return hit;
      const fresh = await fetch(req);
      if (fresh.ok && cache) {
        await cacheBestEffort(() => cache.put(req, fresh.clone()));
      }
      return fresh;
    })());
    return;
  }
  event.respondWith((async () => {
    try {
      const fresh = await fetch(req);
      if (fresh.ok && shellPath(url.pathname)) {
        const cache = await cacheBestEffort(() => caches.open(CACHE));
        if (cache) await cacheBestEffort(() => cache.put(req, fresh.clone()));
      }
      return fresh;
    } catch (err) {
      const cache = await cacheBestEffort(() => caches.open(CACHE));
      const hit = cache ? await cacheBestEffort(() => cache.match(req)) : undefined;
      if (hit) return hit;
      throw err;
    }
  })());
});

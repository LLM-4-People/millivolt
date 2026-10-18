// Deterministic service worker test. Loads internal/web/static/sw.js into a
// vm sandbox with fake CacheStorage, fetch and Response implementations, then
// drives the captured fetch listener. No browser, server, network or real
// CacheStorage is used.
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SW_PATH = path.join(__dirname, '..', 'internal/web/static/sw.js');
const ORIGIN = 'https://proxy.example';
const MARKER = '__DASHBOARD_VERSION__';
const CACHE_PREFIX = 'millivolt-shell-';
const TEST_VERSION = 'test-version-0123456789abcdef';
const CACHE_NAME = CACHE_PREFIX + TEST_VERSION;

const failures = [];
const unhandled = [];
process.on('unhandledRejection', reason => {
  unhandled.push(reason && reason.message ? reason.message : String(reason));
});

function check(label, ok, detail) {
  console.log((ok ? 'PASS ' : 'FAIL ') + label);
  if (ok) return;
  failures.push(label);
  if (detail !== undefined) console.log('  detail: ' + detail);
}

class FakeResponse {
  constructor(body = '', init = {}) {
    this.body = String(body);
    this.status = init.status === undefined ? 200 : init.status;
    this.ok = init.ok === undefined ? this.status >= 200 && this.status < 300 : init.ok;
    this.headers = init.headers || {};
  }
  clone() {
    return new FakeResponse(this.body, { status: this.status, ok: this.ok, headers: { ...this.headers } });
  }
}

class FakeCache {
  constructor(name) {
    this.name = name;
    this.entries = new Map();
    this.added = [];
    this.puts = [];
    this.failPut = new Set();
  }
  match(req) {
    return Promise.resolve(this.entries.has(req.url) ? this.entries.get(req.url).clone() : undefined);
  }
  put(req, res) {
    this.puts.push(req.url);
    if (this.failPut.has(req.url)) return Promise.reject(new Error('quota'));
    this.entries.set(req.url, res.clone());
    return Promise.resolve();
  }
  addAll(assets) {
    this.added.push([...assets]);
    return Promise.resolve();
  }
}

function loadWorker() {
  const source = fs.readFileSync(SW_PATH, 'utf8');
  const markers = source.split(MARKER).length - 1;
  check('sw.js carries exactly one dashboard version marker', markers === 1, 'found ' + markers);
  if (markers !== 1) throw new Error('cannot stamp the service worker');

  const listeners = {};
  const stores = new Map();
  const calls = { opens: [], deletes: [], skipWaiting: 0, claim: 0 };
  const store = name => {
    if (!stores.has(name)) stores.set(name, new FakeCache(name));
    return stores.get(name);
  };
  const caches = {
    open(name) {
      calls.opens.push(name);
      return Promise.resolve(store(name));
    },
    keys() {
      return Promise.resolve([...stores.keys()]);
    },
    delete(name) {
      calls.deletes.push(name);
      return Promise.resolve(stores.delete(name));
    },
  };

  const defaultRespond = (req, index) => new FakeResponse('fresh:' + index);
  let respond = defaultRespond;
  let fetches = [];
  const fetch = req => {
    fetches.push(req.url);
    return Promise.resolve().then(() => respond(req, fetches.length));
  };

  const self = {
    location: { origin: ORIGIN },
    addEventListener(type, listener) {
      listeners[type] = listener;
    },
    skipWaiting() {
      calls.skipWaiting++;
      return Promise.resolve();
    },
    clients: {
      claim() {
        calls.claim++;
        return Promise.resolve();
      },
    },
  };

  const context = vm.createContext({ self, caches, fetch, Response: FakeResponse, URL });
  vm.runInContext(source.replace(MARKER, TEST_VERSION), context, { filename: SW_PATH });

  return {
    listeners,
    calls,
    stores,
    store,
    fetches: () => fetches,
    resetFetches() {
      fetches = [];
    },
    onFetch(handler) {
      respond = handler;
    },
    resetFetchHandler() {
      respond = defaultRespond;
    },
    request(pathname, overrides = {}) {
      return { url: new URL(pathname, ORIGIN).href, method: 'GET', mode: 'cors', ...overrides };
    },
    dispatch(req) {
      let promise;
      listeners.fetch({
        request: req,
        respondWith(value) {
          promise = value;
        },
      });
      return promise;
    },
    async lifecycle(type) {
      let promise;
      listeners[type]({
        waitUntil(value) {
          promise = value;
        },
      });
      if (!promise) throw new Error(type + ' listener did not call waitUntil');
      await promise;
    },
  };
}

async function main() {
  const worker = loadWorker();
  for (const type of ['install', 'activate', 'fetch']) {
    check(type + ' listener remains registered', typeof worker.listeners[type] === 'function');
  }
  if (typeof worker.listeners.fetch !== 'function') throw new Error('no fetch listener');

  await worker.lifecycle('install');
  const installed = worker.store(CACHE_NAME);
  check('install precaches the shell assets under the stamped cache',
    worker.calls.opens.filter(name => name === CACHE_NAME).length === 1 &&
      installed.added.length === 1 && worker.calls.skipWaiting === 1,
    JSON.stringify({ opens: worker.calls.opens, added: installed.added, skip: worker.calls.skipWaiting }));
  const shellAssets = installed.added[0] || [];
  check('install precache keeps the brand/PWA list and never the live shell',
    shellAssets.includes('/manifest.webmanifest') &&
      shellAssets.includes('/favicon.ico') &&
      shellAssets.includes('/icon-512.png') &&
      !shellAssets.includes('/') && !shellAssets.includes('/index.html') && !shellAssets.includes('/sw.js') &&
      !shellAssets.some(p => p.startsWith('/dash/')),
    JSON.stringify(shellAssets));

  const staleCache = CACHE_PREFIX + 'stale-version';
  const nestedCache = CACHE_NAME + '-old';
  worker.store(staleCache);
  worker.store(nestedCache);
  worker.store('unrelated');
  await worker.lifecycle('activate');
  const deleted = [...worker.calls.deletes].sort();
  check('activate deletes only older shell caches and claims clients',
    deleted.join() === [staleCache, nestedCache].sort().join() &&
      worker.calls.claim === 1 &&
      worker.stores.has(CACHE_NAME) && worker.stores.has('unrelated') &&
      !worker.stores.has(staleCache) && !worker.stores.has(nestedCache),
    JSON.stringify({ deleted, stores: [...worker.stores.keys()], claim: worker.calls.claim }));

  // Exact-version cache-first path.
  {
    const cache = worker.store(CACHE_NAME);
    const req = worker.request('/dash/js/core.js?v=' + TEST_VERSION);
    cache.entries.set(req.url, new FakeResponse('cached-exact-version'));
    worker.resetFetches();
    const res = await worker.dispatch(req);
    check('an exact-version cache hit is served without a network request',
      !!res && res.body === 'cached-exact-version' && worker.fetches().length === 0,
      JSON.stringify({ body: res && res.body, fetches: worker.fetches() }));
  }

  {
    const cache = worker.store(CACHE_NAME);
    const req = worker.request('/dash/js/store.js?v=' + TEST_VERSION);
    worker.resetFetches();
    const first = await worker.dispatch(req);
    check('an exact-version miss fetches once and stores the response',
      !!first && first.body === 'fresh:1' && worker.fetches().length === 1 &&
        cache.puts.filter(u => u === req.url).length === 1 && cache.entries.has(req.url),
      JSON.stringify({ body: first && first.body, fetches: worker.fetches(), puts: cache.puts }));
    const second = await worker.dispatch(req);
    check('a second identical request uses the stored response without another fetch',
      !!second && second.body === 'fresh:1' && worker.fetches().length === 1 &&
        cache.puts.filter(u => u === req.url).length === 1,
      JSON.stringify({ body: second && second.body, fetches: worker.fetches(), puts: cache.puts }));
  }

  {
    const cache = worker.store(CACHE_NAME);
    const req = worker.request('/dash/js/extra.js?v=' + TEST_VERSION + '&x=1');
    cache.entries.set(req.url, new FakeResponse('cached-extra-query'));
    worker.resetFetches();
    const res = await worker.dispatch(req);
    check('an unrelated query keeps the exact-version cache-first path',
      !!res && res.body === 'cached-extra-query' && worker.fetches().length === 0,
      JSON.stringify({ body: res && res.body, fetches: worker.fetches() }));
  }

  // Unversioned, stale and duplicate versions stay network-first even with a
  // stale cache entry present.
  for (const [label, url, cached] of [
    ['a missing v', '/dash/js/fence.js', 'cached-missing'],
    ['a stale v', '/dash/js/fence.js?v=stale', 'cached-stale'],
    ['a duplicate v', '/dash/js/fence.js?v=' + TEST_VERSION + '&v=' + TEST_VERSION, 'cached-duplicate'],
  ]) {
    const cache = worker.store(CACHE_NAME);
    const req = worker.request(url);
    cache.entries.set(req.url, new FakeResponse(cached));
    worker.resetFetches();
    const res = await worker.dispatch(req);
    check(label + ' stays network-first even with a cache hit',
      !!res && res.body === 'fresh:1' && worker.fetches().length === 1 &&
        worker.fetches()[0] === req.url,
      JSON.stringify({ body: res && res.body, fetches: worker.fetches() }));
  }

  {
    const cache = worker.store(CACHE_NAME);
    const req = worker.request('/dash/js/broken.js?v=' + TEST_VERSION);
    const before = cache.puts.length;
    worker.resetFetches();
    worker.onFetch(() => new FakeResponse('missing', { status: 404 }));
    const res = await worker.dispatch(req);
    check('a non-ok exact-version response is returned but never cached',
      !!res && res.status === 404 && worker.fetches().length === 1 &&
        cache.puts.length === before && !cache.entries.has(req.url),
      JSON.stringify({ status: res && res.status, fetches: worker.fetches(), puts: cache.puts }));
    worker.resetFetchHandler();
  }

  // Navigation stays network-first with the offline page and never caches
  // login or bootstrap HTML.
  {
    const cache = worker.store(CACHE_NAME);
    const before = cache.puts.length;
    worker.onFetch(() => Promise.reject(new Error('offline')));
    for (const pathname of ['/', '/index.html']) {
      worker.resetFetches();
      const res = await worker.dispatch(worker.request(pathname, { mode: 'navigate' }));
      check('an offline navigation to ' + pathname + ' gets the offline page and never caches HTML',
        !!res && res.body.includes('needs a network') &&
          res.headers['Cache-Control'] === 'no-store' &&
          worker.fetches().length === 1 && cache.puts.length === before,
        JSON.stringify({ body: res && res.body, headers: res && res.headers, fetches: worker.fetches() }));
    }
    worker.resetFetchHandler();
  }

  {
    const cache = worker.store(CACHE_NAME);
    const before = cache.puts.length;
    for (const pathname of ['/', '/index.html']) {
      worker.resetFetches();
      const res = await worker.dispatch(worker.request(pathname, { mode: 'navigate' }));
      check('a live navigation to ' + pathname + ' returns the network response without caching it',
        !!res && res.body === 'fresh:1' && worker.fetches().length === 1 &&
          cache.puts.length === before,
        JSON.stringify({ body: res && res.body, fetches: worker.fetches() }));
    }
  }

  // Live/operator, cross-origin and non-GET requests get no interception.
  {
    const cache = worker.store(CACHE_NAME);
    const before = cache.puts.length;
    worker.resetFetches();
    const ignored = [
      ['a live metrics route', worker.request('/metrics/agg/chart?v=' + TEST_VERSION)],
      ['an operator config route', worker.request('/admin/config')],
      ['an operator session route', worker.request('/session')],
      ['an inference route', worker.request('/v1/models')],
      ['a cross-origin asset', worker.request('https://other.example/dash/js/core.js?v=' + TEST_VERSION)],
      ['a non-GET asset', worker.request('/dash/js/core.js?v=' + TEST_VERSION, { method: 'POST' })],
    ];
    for (const [label, req] of ignored) {
      const res = await worker.dispatch(req);
      check(label + ' is neither intercepted nor cached', res === undefined);
    }
    check('ignored requests never reach fetch or the shell cache',
      worker.fetches().length === 0 && cache.puts.length === before,
      JSON.stringify({ fetches: worker.fetches(), puts: cache.puts }));
  }

  {
    const cache = worker.store(CACHE_NAME);
    const req = worker.request('/dash/js/quota.js?v=' + TEST_VERSION);
    cache.failPut.add(req.url);
    worker.resetFetches();
    const res = await worker.dispatch(req);
    check('a cache write failure never breaks the fetched response',
      !!res && res.body === 'fresh:1' && worker.fetches().length === 1,
      JSON.stringify({ body: res && res.body, fetches: worker.fetches() }));
    cache.failPut.delete(req.url);
  }

  await new Promise(resolve => setImmediate(resolve));
  check('the service worker leaves no unhandled cache rejection', unhandled.length === 0, unhandled.join('; '));

  if (failures.length) {
    console.log('\nFAILURES: ' + failures.join(' | '));
    process.exitCode = 1;
  } else {
    console.log('\nALL SERVICE WORKER CHECKS PASSED');
  }
}

main().catch(err => {
  console.error(err);
  if (failures.length) console.log('\nFAILURES: ' + failures.join(' | '));
  else console.log('\nSERVICE WORKER CHECK CRASHED BEFORE ANY CHECK FAILED');
  process.exitCode = 1;
});

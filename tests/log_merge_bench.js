// DOM-free benchmark and regression probe for the request log's archive merge
// (G6). It runs the production log modules in the page's own deferred-script
// order with inert document stubs, then drives the live-frame owner directly: a
// 20k-row durable archive below a 10k-row live ring, one counter-only frame and
// one one-record update.
//
// Timing alone is not the contract: the archive scan counter and the live
// identity state prove that the unchanged archive is never rescanned. The
// legacy full merge is kept here only as the measured baseline (a merge with
// no memo hit) and as the output-equality oracle.
//
// Run: node tests/log_merge_bench.js
//      node --expose-gc tests/log_merge_bench.js   (adds heap deltas)
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const ROOT = path.join(__dirname, '..');
const STATIC_JS = path.join(ROOT, 'internal/web/static/js');
// index.html's deferred script order is the one owner of the log module list;
// this bench loads the same sources in that order instead of keeping a second
// copy that could drift from the page.
function logModules() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/web/static/index.html'), 'utf8');
  const names = [...html.matchAll(/<script\b[^>]*\bsrc="\/dash\/js\/(log(?:\.js|_[a-z0-9_]+\.js))[^"]*"[^>]*>/g)]
    .map(m => m[1]);
  if (!names.length) throw new Error('index.html references no log module');
  if (!names.includes('log.js')) throw new Error('index.html references no log.js entry module');
  return names;
}
const LOG_MODULES = logModules();
const LOG_SOURCES = LOG_MODULES.map(name => fs.readFileSync(path.join(STATIC_JS, name), 'utf8'));
const ARCHIVE_LEN = 20000;
const RING_LEN = 10000;
const FRAMES = 250;
const VISIBLE_CAP = 60;

const inert = {addEventListener() {}, querySelector() { return null; }, querySelectorAll() { return []; }, scrollTop: 0};
const sandbox = {
  console,
  $: () => inert,
  document: {createElement: () => ({content: null, set innerHTML(v) {}})},
  ENTITY_TYPES: {
    client: {color: 'client'}, model: {color: 'model'}, tool: {color: 'tool'},
    status: {color: 'status'},
  },
  dashCfg: {history_size: RING_LEN, log_rows: VISIBLE_CAP},
  lastData: null,
  requestAnimationFrame: () => 1,
  setTimeout,
  clearTimeout,
};
sandbox.globalThis = sandbox;
const context = vm.createContext(sandbox);
vm.runInContext('let logLimit = 0; let logArchive = []; let recordsRevision = 0;', context);
// Each module is its own script, exactly as the page runs them; the vm context
// shares one global lexical scope across scripts, like the browser.
for (const src of LOG_SOURCES) vm.runInContext(src, context);
vm.runInContext(`
  globalThis.logApi = {
    mergeLogArchive, logWindowLength, logWindowRows, visibleLog, logVisible,
    logArchivePrepend, logArchiveClear, logArchiveIndex,
  };
`, context);
const api = sandbox.logApi;

function fail(message) {
  console.error('FAIL ' + message);
  process.exitCode = 1;
}
function check(label, cond) {
  console.log((cond ? 'PASS ' : 'FAIL ') + label);
  if (!cond) fail(label);
}
const run = src => vm.runInContext(src, context);

function rec(prefix, i, start) { return {id: prefix + i, start, status_code: 200}; }
const archive = Array.from({length: ARCHIVE_LEN}, (_, i) => rec('a', i, i * 1000));
const ring = Array.from({length: RING_LEN}, (_, i) => rec('r', i, 50000000 + i * 1000));
const data = {records: ring, counters: {}, _rev: 0};
sandbox.__archive = archive;
sandbox.__data = data;
run('logArchive = globalThis.__archive; lastData = globalThis.__data; logLimit = 0;');
api.logWindowLength(ring);

// The legacy full merge: exactly the pre-G6 work, and the equality oracle for
// the visible window below the ring.
function legacyMerge(scopedRing) {
  const seen = new Set();
  for (const r of scopedRing) { if (r && r.id) seen.add(r.id); }
  const extra = [];
  for (const r of archive) {
    if (r && r.id && !seen.has(r.id)) { extra.push(r); seen.add(r.id); }
  }
  return extra.concat(scopedRing);
}
function legacyVisible(merged) {
  const n = Math.min(merged.length, VISIBLE_CAP);
  return merged.slice(-n).reverse();
}
const oracle = legacyVisible(legacyMerge(ring));
const baselineChecks = api.logWindowRows(ring);
check('visible window matches the legacy merge byte-for-byte (ids and object identity)',
  baselineChecks.length === oracle.length && baselineChecks.every((r, i) => r === oracle[i]));
check('merged length matches the legacy archive plus ring',
  api.logWindowLength(ring) === legacyMerge(ring).length);

function heap() { return process.memoryUsage().heapUsed; }
function gc() { if (global.gc) global.gc(); }

function bench(label, frames, frame) {
  gc();
  const before = heap();
  const scans = run('_logArchiveScans');
  const t0 = process.hrtime.bigint();
  for (let i = 0; i < frames; i++) frame(i);
  const ns = Number(process.hrtime.bigint() - t0);
  gc();
  const bytes = Math.max(0, heap() - before);
  const scansAfter = run('_logArchiveScans');
  const perFrame = ns / frames / 1e6;
  console.log(`${label}: ${perFrame.toFixed(3)} ms/frame, ${(perFrame * 60).toFixed(1)} ms/s at 60fps, ` +
    `${scansAfter - scans} archive scans` + (global.gc ? `, ${Math.round(bytes / frames)} heap B/frame` : ''));
  return {perFrame, scans: scansAfter - scans};
}

// Counter-only live frame: the derivation (_rev) may advance and the ring
// object is unchanged, so neither membership nor the archive index may move.
const counter = bench('counter-only frame', FRAMES, () => {
  run('lastData._rev++');
  api.logWindowLength(ring);
  api.logWindowRows(ring);
});
check('a counter-only frame never rescans the archive', counter.scans === 0);

// One-record update: a live row is replaced in place (same id, same ring
// length) and the record revision advances. Only that record's content
// changes; membership and archive shadowing stay put.
let updateIndex = RING_LEN - 1;
const updated = [];
const oneRecord = bench('one-record update frame', FRAMES, i => {
  updateIndex = RING_LEN - 1 - (i % VISIBLE_CAP);
  const next = {...ring[updateIndex], status_code: 500 + (i % 2)};
  ring[updateIndex] = next;
  updated.push(next);
  run('recordsRevision++');
  api.logWindowLength(ring);
  api.logWindowRows(ring);
});
check('a one-record update never rescans the archive', oneRecord.scans === 0);
check('the one-record update is materialized without a full merge',
  api.logWindowRows(ring)[VISIBLE_CAP - 1 - (updateIndex - (RING_LEN - VISIBLE_CAP))] === ring[updateIndex]);

// Append + archive shadow: one new live id per frame. A live id present in the
// archive must suppress its archived copy rather than grow the merged length.
const shadow = rec('shadow', 0, 90000000);
archive.push(shadow);
run('logArchive = globalThis.__archive;');
api.logArchiveIndex();
api.logWindowLength(ring);
const preAppend = api.logWindowLength(ring);
const append = bench('live append frame (appended id shadows an archive row)', FRAMES, i => {
  const next = rec('shadow', i, 91000000 + i);
  ring.push(next);
  run('recordsRevision++');
  api.logWindowLength(ring);
  api.logWindowRows(ring);
});
check('appended live ids reconcile without an archive scan', append.scans === 0);
check('a live id that shadows an archive row does not grow the merged length',
  api.logWindowLength(ring) === preAppend + (FRAMES - 1));

// Archive mutation owns the revision: one prepend and one clear rebuild once.
const scansBefore = run('_logArchiveScans');
api.logArchivePrepend([rec('p', 0, 1000)]);
api.logWindowLength(ring);
check('an archive prepend rebuilds the identity set exactly once',
  run('_logArchiveScans') === scansBefore + 1);
const afterPrepend = run('_logArchiveScans');
api.logArchiveClear();
api.logWindowLength(ring);
check('a reset clears the archive without a scan', run('_logArchiveScans') === afterPrepend &&
  run('logArchive.length') === 0 && api.logWindowLength(ring) === ring.length);

// Legacy baseline: every frame misses (the old memo keyed on the global
// revision) and rebuilds the whole merge.
sandbox.__archive = archive;
run('logArchive = globalThis.__archive; logLimit = 0;');
api.logWindowLength(ring);
const legacy = bench('legacy full merge per frame', 100, () => {
  legacyMerge(ring);
});

const ratio = legacy.perFrame / Math.max(oneRecord.perFrame, 1e-9);
console.log(`one-record improvement: ${(legacy.perFrame - oneRecord.perFrame).toFixed(3)} ms/frame ` +
  `(${ratio.toFixed(1)}x faster), ${((legacy.perFrame * 60) - (oneRecord.perFrame * 60)).toFixed(1)} ms/s saved at 60fps`);
check('the live frame is materially cheaper than the legacy full merge', ratio >= 5);

if (process.exitCode) {
  console.error('\nBENCH FAILED');
} else {
  console.log('\nALL LOG MERGE BENCH CHECKS PASSED');
}

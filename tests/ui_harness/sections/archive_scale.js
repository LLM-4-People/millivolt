// test 10c-ter: archive revision and incremental live reconciliation
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function archiveScale(H) {
  const {check, d, fire, mkRec, rows, sleep, w} = H;
  const state = H.state;

  // ---- test 10c-ter: archive revision and incremental live reconciliation ----
  // The durable archive owns one persistent identity set and a revision that
  // advances only when older pages actually change. A live frame reconciles
  // ring membership and materializes only the visible cap: counter-only and
  // one-record frames must not call the full merge or rescan the archive.
  {
    const savedData = w.eval('lastData');
    const savedArchive = w.eval('logArchive');
    const ring = [
      mkRec('memo-ring0', 200, 1700001000000),
      mkRec('memo-ring1', 200, 1700001001000),
    ];
    const archive = [
      mkRec('memo-old1', 200, 1700000900000),
      mkRec('memo-old0', 200, 1700000800000),
      { ...ring[0], status_code: 500 }, // a stale archive copy of a live id
    ];
    const data = { records: ring, counters: {}, _rev: 3 };
    w.__memoRing = ring; w.__memoArchive = archive; w.__memoData = data;
    w.eval('lastData = window.__memoData; logArchive = window.__memoArchive; logLimit = 0;');
    const ids = list => list.map(r => r.id).join(',');
    const first = w.mergeLogArchive(ring);
    check('the merged window keeps durable order below the live ring and live wins duplicate ids',
      ids(first) === 'memo-old1,memo-old0,memo-ring0,memo-ring1' &&
      first[2] === ring[0] && first[3] === ring[1]);
    check('an unchanged archive merge reuses the prior merged array',
      w.mergeLogArchive(ring) === first);
    check('a merged window passed back through the merge reuses itself',
      w.mergeLogArchive(first) === first);
    // The global derivation revision moves for counters and KPIs too, so it is
    // no longer a merge identity; only record changes may rebuild the window.
    data._rev = 4;
    check('a counter-only revision bump reuses the merged window',
      w.mergeLogArchive(ring) === first);
    const nextData = { records: ring, counters: {}, _rev: 4 };
    w.__memoData = nextData;
    w.eval('lastData = window.__memoData');
    check('a new data object with unchanged records reuses the merged window',
      w.mergeLogArchive(ring) === first);
    const archiveRev = w.eval('_logArchiveIndex.rev');
    w.mergeLogArchive(ring);
    w.logWindowLength(ring);
    check('counter-only frames do not advance the archive revision',
      w.eval('_logArchiveIndex.rev') === archiveRev);
    // A one-record content change rebuilds the live row without invalidating
    // the archive identity set: the materialized merge walks the archive once,
    // but the cached id set and revision stay put.
    const updated = { ...ring[1], status_code: 201 };
    ring[1] = updated;
    const scansBeforeContent = w.eval('_logArchiveScans');
    w.eval('bumpRecords()');
    const content = w.mergeLogArchive(ring);
    check('a one-record update rebuilds merged content without rebuilding the archive identity',
      content !== first && content[3] === updated &&
      w.eval('_logArchiveIndex.rev') === archiveRev &&
      w.eval('_logArchiveScans') === scansBeforeContent + 1);
    const nextArchive = archive.slice();
    w.__memoArchive = nextArchive;
    w.eval('logArchive = window.__memoArchive');
    const archiveSwap = w.mergeLogArchive(ring);
    check('a replaced archive array rebuilds the merged window',
      archiveSwap !== content && ids(archiveSwap) === ids(content));
    check('an archive mutation advances the identity revision exactly once',
      w.eval('_logArchiveIndex.rev') === archiveRev + 1);
    nextArchive.length = 1; // an in-place trim, as the cap owner applies
    const trimmed = w.mergeLogArchive(ring);
    check('an in-place archive trim rebuilds the merged window with the retained page',
      trimmed !== archiveSwap && ids(trimmed) === 'memo-old1,memo-ring0,memo-ring1');
    // The live-frame owner is the hot path: rows and stats must materialize
    // without invoking the full archive merge.
    const calls = [];
    const realMerge = w.mergeLogArchive;
    w.mergeLogArchive = recs => { calls.push(recs); return realMerge(recs); };
    const scansBeforeFrame = w.eval('_logArchiveScans');
    try {
      w.eval('lastData._rev++'); // a counter-only frame still schedules the log
      w.renderRequestsIncremental(ring);
      w.updateLogStats({ scopedRecs: ring });
    } finally {
      w.mergeLogArchive = realMerge;
    }
    check('a counter-only live frame never calls the full archive merge',
      calls.length === 0 && w.eval('_logArchiveScans') === scansBeforeFrame);
    // Evicting a live id unshadows its archived copy in place: the merged count
    // stays coherent (ring -1, shadow -1) and the archive row appears below
    // the ring in its retained keyset order.
    w.__memoArchive = archive;
    w.eval('logArchive = window.__memoArchive');
    const beforeDrop = w.logWindowLength(ring);
    ring.splice(0, 1);
    w.eval('bumpRecords()');
    check('an evicted live id unshadows its archive copy without losing archive order',
      w.logWindowLength(ring) === beforeDrop &&
      ids(w.logWindowRows(ring)) === 'memo-ring1,memo-ring0,memo-old0,memo-old1');
    w.__memoArchive = [];
    w.eval('logArchive = window.__memoArchive');
    check('an empty archive returns the scoped ring array itself',
      w.mergeLogArchive(ring) === ring && w.logWindowLength(ring) === ring.length);
    // Restore the suite's own data and repaint through the product owners.
    w.__memoSavedData = savedData; w.__memoSavedArchive = savedArchive;
    w.eval('lastData = window.__memoSavedData; logArchive = window.__memoSavedArchive; resetLogWindow(); renderRequests(derive(lastData).scopedRecs); updateLogStats();');
    for (const key of ['__memoRing', '__memoArchive', '__memoData', '__memoSavedData', '__memoSavedArchive']) delete w[key];
  }

  // ---- test 10c-quater: live frames at the audited archive scale ----
  // 20k durable rows below a 10k live ring: counter-only frames and one-record
  // updates stay O(visible cap), never rebuilding the archive merge.
  {
    const savedData = w.eval('lastData');
    const savedArchive = w.eval('logArchive');
    const bigArchive = Array.from({length: 20000}, (_, i) =>
      mkRec('biga' + String(i).padStart(5, '0'), 200, 1500000000000 + i * 1000));
    const bigRing = Array.from({length: 10000}, (_, i) =>
      mkRec('bigr' + String(i).padStart(5, '0'), 200, 1520000000000 + i * 1000));
    // An archived copy of a live id is data the live ring must shadow.
    bigArchive.push({ ...bigRing[9999], status_code: 500 });
    w.__bigArchive = bigArchive; w.__bigRing = bigRing;
    w.__bigSavedData = savedData; w.__bigSavedArchive = savedArchive;
    w.__bigData = { records: bigRing, counters: {}, _rev: 0 };
    w.eval('lastData = window.__bigData; logArchive = window.__bigArchive; logLimit = 0;');
    w.logWindowLength(bigRing); // warm the archive identity and live membership
    const scans = w.eval('_logArchiveScans');
    const calls = [];
    const realMerge = w.mergeLogArchive;
    w.mergeLogArchive = recs => { calls.push(recs); return realMerge(recs); };
    try {
      for (let i = 0; i < 10; i++) {
        w.eval('lastData._rev++');
        w.renderRequestsIncremental(bigRing);
        w.updateLogStats({ scopedRecs: bigRing });
      }
      bigRing[9999] = { ...bigRing[9999], status_code: 503 };
      w.eval('bumpRecords()');
      w.renderRequestsIncremental(bigRing);
      w.updateLogStats({ scopedRecs: bigRing });
    } finally {
      w.mergeLogArchive = realMerge;
    }
    check('20k archive + 10k ring: counter-only and one-record frames never merge or rescan',
      calls.length === 0 && w.eval('_logArchiveScans') === scans);
    check('20k archive + 10k ring: only the visible cap materializes',
      w.logWindowRows(bigRing).length === 60 && rows().length === 60 &&
      rows()[0].dataset.id === 'bigr09999');
    check('the archived live duplicate adds no merged row',
      w.logWindowLength(bigRing) === 30000);
    w.eval('logLimit = 10001');
    const deep = w.logWindowRows(bigRing);
    check('the newest archive row sits directly below the live ring at depth',
      deep.length === 10001 && deep[10000].id === 'biga19999' &&
      deep[9999].id === 'bigr00000');
    // Restore and repaint the suite's own state.
    w.eval('lastData = window.__bigSavedData; logArchive = window.__bigSavedArchive; resetLogWindow(); renderRequests(derive(lastData).scopedRecs); updateLogStats();');
    for (const key of ['__bigArchive', '__bigRing', '__bigData', '__bigSavedData', '__bigSavedArchive']) delete w[key];
  }

  // ---- test 10c-quinquies: archive writes must use a supported owner ----
  // The archive identity cache keys on the array object and its length, so a
  // same-length in-place element replacement is intentionally not a supported
  // mutation and is not observed by the cache. Every archive write must use
  // logArchiveClear or logArchivePrepend, or replace the array / change its
  // length. This regression performs the unsupported edit deliberately, then
  // invokes the supported prepend owner: the owner must expose the resulting
  // archive to the merge exactly like a cold (uncached) merge, with no stale
  // duplicate and no lost row.
  {
    const savedData = w.eval('lastData');
    const savedArchive = w.eval('logArchive');
    const ring = [
      mkRec('memoq-ring0', 200, 1700003000000),
      mkRec('memoq-ring1', 200, 1700003001000),
    ];
    const old2 = mkRec('memoq-old2', 200, 1700002700000);
    const archive = [
      mkRec('memoq-old0', 200, 1700002900000),
      mkRec('memoq-old1', 200, 1700002800000),
      old2,
    ];
    const newOld = mkRec('memoq-old0b', 200, 1700002600000);
    const data = { records: ring, counters: {}, _rev: 7 };
    w.__memoqRing = ring; w.__memoqArchive = archive; w.__memoqNew = newOld; w.__memoqData = data;
    w.eval('lastData = window.__memoqData; logArchive = window.__memoqArchive; logLimit = 0;');
    const ids = list => list.map(r => r.id).join(',');
    w.mergeLogArchive(ring); // warm the archive identity and the merge memo
    const warmRev = w.eval('_logArchiveIndex.rev');
    const warmScans = w.eval('_logArchiveScans');
    // Unsupported: replace elements in place without changing the length.
    // old0 becomes an archived duplicate of a live id (it must be suppressed)
    // and old1 becomes a new archive id (it must appear). The identity cache
    // cannot observe either edit on its own.
    archive[0] = { ...ring[0] };
    archive[1] = mkRec('memoq-old1b', 200, 1700002800000);
    w.logArchivePrepend([newOld]);
    const merged = w.mergeLogArchive(ring);
    const ownerRev = w.eval('_logArchiveIndex.rev');
    const ownerScans = w.eval('_logArchiveScans');
    // Cold oracle: a supported array replacement discards every identity and
    // memo, so this merge recomputes the same resulting archive from scratch.
    w.__memoqOracleArchive = w.eval('logArchive').slice();
    w.eval('logArchive = window.__memoqOracleArchive');
    const oracle = w.mergeLogArchive(ring);
    check('a supported archive owner exposes in-place edits and matches the cold merge oracle',
      ids(merged) === 'memoq-old0b,memoq-old1b,memoq-old2,memoq-ring0,memoq-ring1' &&
      merged.length === oracle.length &&
      merged.every((r, i) => r === oracle[i]) &&
      merged[0] === newOld && merged[1] === archive[1] && merged[2] === old2 &&
      merged[3] === ring[0] && merged[4] === ring[1] &&
      !merged.includes(archive[0]) &&
      new Set(merged.map(r => r.id)).size === merged.length);
    check('the supported archive mutation advances the identity revision once with one rebuild and one merge scan',
      ownerRev === warmRev + 1 && ownerScans === warmScans + 2);
    // Restore and repaint the suite's own state.
    w.__memoqSavedData = savedData; w.__memoqSavedArchive = savedArchive;
    w.eval('lastData = window.__memoqSavedData; logArchive = window.__memoqSavedArchive; resetLogWindow(); renderRequests(derive(lastData).scopedRecs); updateLogStats();');
    for (const key of ['__memoqRing', '__memoqArchive', '__memoqNew', '__memoqData', '__memoqOracleArchive', '__memoqSavedData', '__memoqSavedArchive']) delete w[key];
  }

  // ---- test 10d: the request log nests by local calendar day - a divider
  // rides above each day's first row, moves with live inserts, and never
  // duplicates or orphans across full resyncs. Fixtures anchor to local
  // midnights so the days are distinct regardless of the run time.
  const DAY_MS = 86400000;
  const midnight = new Date(); midnight.setHours(0, 0, 0, 0);
  const at = daysAgo => midnight.getTime() - daysAgo * DAY_MS + 3600000; // 01:00 local, N days ago
  const MIXED = [
    mkRec('day2', 200, at(2)),
    mkRec('day1', 200, at(1)),
    mkRec('day0', 200, at(0)),
  ];
  fire('snapshot', { feed_id: 'feedB', seq: 3, incremental: false,
    records: MIXED, counters: { in_flight: 0, total_requests: 3, total_errors: 0 } });
  await sleep(30);
  const seps = () => [...d.querySelectorAll('#tbl-requests tr.day-sep')];
  check('day dividers: exactly one per local day', seps().length === 3);
  check('day dividers span the full request column count',
    seps()[0]?.querySelector('td')?.colSpan === w.eval('REQ_COLS'));
  check('newest day divider is stamped TODAY', seps()[0]?.textContent?.includes('TODAY'));
  check('each divider sits directly above its day opener',
    seps()[0]?.nextElementSibling?.dataset.id === 'day0' &&
    seps()[1]?.nextElementSibling?.dataset.id === 'day1' &&
    seps()[2]?.nextElementSibling?.dataset.id === 'day2');
  fire('record', { ...mkRec('day0b', 200, at(0) + 3600000) }, '4');
  await sleep(20);
  check('same-day live insert moves the divider, never duplicates it',
    seps().length === 3 && seps()[0]?.nextElementSibling?.dataset.id === 'day0b' &&
    seps()[0]?.nextElementSibling?.nextElementSibling?.dataset.id === 'day0');
  fire('record', { ...mkRec('day1b', 200, at(1) + 7200000) }, '5');
  await sleep(20);
  // An out-of-order arrival (a late-finalized older record) opens its own day
  // boundary at its arrival position - the display is arrival-ordered, so the
  // divider tracks the displayed sequence, never a re-sorted one.
  check('late-arriving previous-day record opens its own divider at its arrival position',
    seps().length === 4 && seps()[0]?.nextElementSibling?.dataset.id === 'day1b');
  fire('snapshot', { feed_id: 'feedB', seq: 5, incremental: false,
    records: [ mkRec('day2', 200, at(2)), mkRec('day1', 200, at(1)), mkRec('day0', 200, at(0)),
      mkRec('day0b', 200, at(0) + 3600000), mkRec('day1b', 200, at(1) + 7200000) ],
    counters: { in_flight: 0, total_requests: 5, total_errors: 0 } });
  await sleep(30);
  check('full resync rebuilds the dividers exactly (no orphans)', seps().length === 4);
  check('dividers are not request rows (log stats unchanged)', rows().length === 5);
};

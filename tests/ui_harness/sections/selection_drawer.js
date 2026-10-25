// test 7c: the 5s tick IS the SSE fallback - a cursor-resumed bootstrap
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function selectionDrawer(H) {
  const {check, d, fire, mkRec, rows, settleUntil, sleep, w} = H;
  const state = H.state;

  // ---- test 7c: the 5s tick IS the SSE fallback - a cursor-resumed bootstrap
  // folds missed records exactly like the stream does, and replay-tolerant
  // delivery (the same record twice) must not duplicate the row.
  const origTick = w.fetch;
  w.fetch = (url) => {
    const u = String(url);
    if (u.includes('/metrics/bootstrap')) {
      state.bootURL = u;
      const fp = state.fullPayload;
      const since = new URL(url, 'http://x').searchParams.get('since');
      const recs = since ? fp.records.filter(r => r.__seq > Number(since)) : fp.records;
      return Promise.resolve({ ok: true, json: async () => ({
        records: JSON.parse(JSON.stringify(recs)),
        counters: fp.counters, seq: fp.seq,
        feed_id: fp.feed_id, incremental: !!since,
      }) });
    }
    return origTick(url);
  };
  state.fullPayload = { feed_id: 'feedB', seq: 4,
    records: [{ ...mkRec('fresh4', 200, 1700000103000), __seq: 4 }],
    counters: { in_flight: 0, total_requests: 4, total_errors: 0 } };
  // Await the page's own fetch and then its painted end state instead of a
  // fixed sleep: under load the fold landed after 20 ms and failed the check
  // while the next assertion 20 ms later passed. Capture the URL synchronously
  // so the check pins this call, not whatever fetch ran last.
  const resume = w.eval("fetchBootstrap('resume')");
  const resumeURL = state.bootURL;
  await resume;
  await settleUntil(() => rows().length === 4 && rows()[0].dataset.id === 'fresh4');
  check('tick resume URL carries since + feed', resumeURL.includes('since=3') && resumeURL.includes('feed=feedB'));
  check('tick resume folds the missed record once', rows().length === 4 && rows()[0].dataset.id === 'fresh4');
  await w.eval("fetchBootstrap('resume')"); // already held: the fold must tolerate the replay
  // Drain any live render the replay fold scheduled before asserting: a
  // wrongly re-added record bumps the data revision and paints on the next
  // rAF, which must finish before the no-duplicate check can see it.
  await settleUntil(() => !w.eval('_renderQueued') && !w.eval('_renderDirty'));
  check('tick replay of the same record never duplicates the row', rows().length === 4);
  w.fetch = origTick;
  w.eval('armDashboardTicks()');

  // ---- test 7d: selection identity survives re-render ticks ----
  // The drawer's selection (drawerId) owns both the drawer's content and
  // the row highlight. Two re-render paths must keep the selection on the
  // same request: the lifecycle finalization that replaces the selected
  // row's own DOM node in place, and the full resync (renderAll re-renders
  // drawer AND log together).
  {
    const expandingRows = () => [...d.querySelectorAll('#tbl-requests tr.exp-row')]
      .filter(tr => tr.classList.contains('expanding'));
    const selRec = { ...mkRec('sel-live', 200, 1700000104000) };
    delete selRec.status_code;
    fire('begin', { record: selRec, in_flight: 1 });
    // The begin upsert schedules the coalesced rAF render that paints the
    // row itself; the pending-row template never carries the expanding
    // class (that is the drawer-selection highlight, painted
    // synchronously by openDrawer on the next line and proven by the
    // check after it). Poll for the painted row, not the highlight.
    await settleUntil(() => !!d.querySelector('#tbl-requests tr.exp-row[data-id="sel-live"]'));
    w.openDrawer('sel-live');
    check('opening a request row selects it in the drawer and highlights exactly its row',
      d.getElementById('drawer').classList.contains('open') &&
        d.getElementById('drawer-body').textContent.includes('sel-live') &&
        expandingRows().length === 1 && expandingRows()[0].dataset.id === 'sel-live');
    const nodeBefore = d.querySelector('#tbl-requests tr.exp-row[data-id="sel-live"]');
    fire('end', { record: { ...mkRec('sel-live', 200, 1700000104000), duration_ms: 999 }, in_flight: 0 });
    // The end upsert schedules the coalesced rAF pass that replaces the
    // row's node group in place; poll for the flushed end state (a fresh
    // node that keeps the expanding highlight).
    await settleUntil(() => {
      const n = d.querySelector('#tbl-requests tr.exp-row[data-id="sel-live"]');
      return !!n && n !== nodeBefore && n.classList.contains('expanding');
    });
    const nodeAfter = d.querySelector('#tbl-requests tr.exp-row[data-id="sel-live"]');
    check('the selected row keeps its highlight when its own node is re-rendered',
      nodeBefore && nodeAfter && nodeBefore !== nodeAfter &&
        nodeAfter.classList.contains('expanding') &&
        expandingRows().length === 1 && expandingRows()[0].dataset.id === 'sel-live' &&
        d.getElementById('drawer').classList.contains('open'));
    fire('snapshot', { feed_id: 'feedB', seq: 4, incremental: false,
      records: [mkRec('fresh1', 200, 1700000100000), mkRec('fresh2', 200, 1700000101000),
        mkRec('fresh3', 200, 1700000102000), mkRec('fresh4', 200, 1700000103000),
        { ...mkRec('sel-live', 200, 1700000104000), duration_ms: 999 }],
      counters: { in_flight: 0, total_requests: 5, total_errors: 0 } });
    await sleep(20);
    check('a full resync re-renders drawer and log from the same selection identity',
      d.getElementById('drawer').classList.contains('open') &&
        d.getElementById('drawer-body').textContent.includes('sel-live') &&
        d.getElementById('drawer-body').textContent.includes('999ms') &&
        expandingRows().length === 1 && expandingRows()[0].dataset.id === 'sel-live');
    w.closeDrawer();
  }

  // ---- test 7e: attempt metadata parity in the drawer ----
  // Absorbed attempts store the same upstream information as the final
  // response; the attempt log shows each attempt's own provider request id,
  // the matching key between a provider-side failure report and the exact
  // absorbed attempt. Attempts without one (transport failures) render no
  // empty req span.
  {
    const atts = [
      { status_code: 502, error_type: 'bad_gateway', error_msg: 'upstream overloaded',
        provider_request_id: 'req_meta_1', at: new Date(1700000104500).toISOString() },
      { status_code: 0, error_type: 'transport', error_msg: 'connection reset',
        at: new Date(1700000104800).toISOString() },
    ];
    fire('end', { record: { ...mkRec('att-meta', 200, 1700000104000), retries: 2, attempts: atts }, in_flight: 0 });
    await sleep(20);
    w.openDrawer('att-meta');
    const attRows = [...d.querySelectorAll('.attempt-link')];
    check('the drawer attempt log renders the absorbed attempts with their own provider request ids',
      d.getElementById('drawer').classList.contains('open') &&
        attRows.length === 2 &&
        attRows[0].textContent.includes('502') &&
        attRows[0].textContent.includes('req req_meta_1') &&
        attRows[1].textContent.includes('transport'));
    check('an attempt without a provider request id renders no empty req span',
      !attRows[1].textContent.includes('· req'));
    w.closeDrawer();
  }

  // ---- test 7f: the attempt-detail drawer mode ----
  // Clicking an absorbed attempt (a drawer attempt link or a log retry-sub
  // row) must open THAT attempt in the drawer, not the final record: the
  // attempt view carries only the retained per-attempt truth (Go's
  // RetryAttempt parity) plus parent context - never the record-level
  // usage/cost/TTFT/preview sections that have no attempt twin. drawerId
  // stays the parent record id throughout, so the row highlight and the
  // debug download-status guard keep their record identity.
  {
    const atts = [
      { status_code: 502, error_type: 'bad_gateway', error_code: 'overloaded',
        error_msg: 'upstream overloaded', retry_after_ms: 1260,
        at: new Date(1700000104500).toISOString(),
        provider_request_id: 'req_meta_1', provider_server: 'srv-a.example',
        provider_model: 'm-upstream', processing_ms: 45,
        rate_limit_remaining: 12, rate_limit_limit: 100,
        response_headers: {'x-request-id': ['req_meta_1'], 'retry-after': ['2']} },
      { status_code: 429, error_type: 'rate_limit', error_msg: 'slow down',
        at: new Date(1700000104800).toISOString() },
    ];
    fire('end', { record: { ...mkRec('att-open', 200, 1700000104000), retries: 2, attempts: atts,
      prompt_preview: 'PROMPT-PREVIEW-7F', response_preview: 'RESPONSE-PREVIEW-7F' }, in_flight: 0 });
    await sleep(20);
    w.openDrawer('att-open');
    const title = () => d.getElementById('drawer-title').textContent;
    const bodyText = () => d.getElementById('drawer-body').textContent;
    // typeof guard: pre-fix trees have no drawerAttempt at all, and every row
    // must stay a clean red, never an uncaught ReferenceError.
    const attemptSel = () => w.eval('typeof drawerAttempt === "undefined" ? "absent" : drawerAttempt');
    const links = () => [...d.querySelectorAll('.attempt-link[data-open-req]')];
    const back = () => d.getElementById('drawer-back');
    check('the drawer attempt links carry their attempt index',
      links().length === 2 && links()[0].dataset.attempt === '0' && links()[1].dataset.attempt === '1');
    links()[0].click();
    check('clicking an attempt link opens THAT attempt in the drawer',
      d.getElementById('drawer').classList.contains('open') &&
        title().includes('attempt 1 of 2') && title().includes('502') &&
        title().includes('req_meta_1') && !!back());
    check('the attempt view seats focus on its back affordance', d.activeElement === back());
    const attRow = k => [...d.querySelectorAll('#drawer-body .detail-kv .k')]
      .find(el => el.textContent === k)?.closest('.detail-kv');
    check('the attempt view renders the retained attempt truth',
      attRow('request')?.querySelector('.v')?.textContent === 'att-open' &&
        attRow('attempt')?.querySelector('.v')?.textContent === '1 of 2' &&
        attRow('status')?.querySelector('.v')?.textContent === '502' &&
        !!attRow('when') &&
        attRow('type')?.querySelector('.v')?.textContent === 'bad_gateway' &&
        attRow('code')?.querySelector('.v')?.textContent === 'overloaded' &&
        attRow('message')?.querySelector('.v')?.textContent === 'upstream overloaded' &&
        attRow('retry-after')?.querySelector('.v')?.textContent === '1.26s' &&
        attRow('request id')?.querySelector('.v')?.textContent === 'req_meta_1' &&
        attRow('server')?.querySelector('.v')?.textContent === 'srv-a.example' &&
        attRow('model (upstream)')?.querySelector('.v')?.textContent === 'm-upstream' &&
        attRow('processing')?.querySelector('.v')?.textContent === '45 ms' &&
        attRow('remaining')?.querySelector('.v')?.textContent === '12' &&
        attRow('limit')?.querySelector('.v')?.textContent === '100');
    check('the attempt view renders exactly its own sections',
      JSON.stringify([...d.querySelectorAll('#drawer-body h4')].map(h => h.textContent)) ===
        JSON.stringify(['Attempt', 'Provider', 'Rate limit', 'Response headers']));
    check('the attempt response headers render sorted with the shared row pattern', (() => {
      const sec = [...d.querySelectorAll('#drawer-body .detail-section')]
        .find(s => s.querySelector('h4')?.textContent === 'Response headers');
      return !!sec && [...sec.querySelectorAll('.detail-kv .k')].map(k => k.textContent).join() ===
        'retry-after,x-request-id';
    })());
    check('the record-level sections and previews have no attempt twin and never render',
      !bodyText().includes('PROMPT-PREVIEW-7F') && !bodyText().includes('RESPONSE-PREVIEW-7F') &&
        !bodyText().includes('ttft') && !bodyText().includes('cost') &&
        !bodyText().includes('Tokens'));
    check('the attempt view carries no debug status slot for a stale download to paint',
      !d.getElementById('drawer-debug-status'));
    w.eval("debugDownloadStatus('att-open', 'stale download message')");
    check('a download result cannot paint a stale status into the attempt view',
      !bodyText().includes('stale download message') && attemptSel() === 0);
    // Drivers are guarded so a pre-fix tree stays red row by row, never an
    // uncaught TypeError; each row keeps its own red conjunct.
    const backBtn = back();
    if (backBtn) backBtn.click();
    check('the back affordance returns to the request view with focus on the drawer\'s first control',
      !!backBtn && !back() && !title().includes('attempt') && bodyText().includes('PROMPT-PREVIEW-7F') &&
        d.activeElement === d.querySelector('#drawer .nav-btns button'));
    // The log retry-sub rows open the same attempt mode (their index rides
    // data-attempt).
    d.querySelector('.retry-toggle[data-retry-for="att-open"]').click();
    const sub = () => d.querySelector('tr.retry-sub[data-retry-of="att-open"][data-attempt="1"]');
    check('the retry-sub rows carry their attempt index', !!sub() && !sub().hidden);
    const subRow = sub();
    check('a retry sub-row spans the full request column count',
      !!subRow && [...subRow.cells].reduce((sum, cell) => sum + cell.colSpan, 0) === w.eval('REQ_COLS'));
    if (subRow) subRow.click();
    check('clicking a retry-sub row opens that attempt in the drawer',
      title().includes('attempt 2 of 2') && title().includes('429') && attemptSel() === 1);
    if (subRow) { subRow.setAttribute('data-attempt', 'not-a-number'); subRow.click(); }
    check('a malformed attempt index fails closed to the request view',
      attemptSel() === null && !title().includes('attempt') &&
        bodyText().includes('PROMPT-PREVIEW-7F'));
    // The empty-string spelling: Number('') coerces to 0, which opened
    // attempt mode instead of failing closed - the index must pass the
    // digit-spelling gate first.
    if (subRow) { subRow.setAttribute('data-attempt', ''); subRow.click(); }
    check('an empty-string attempt index fails closed to the request view',
      attemptSel() === null && !title().includes('attempt') &&
        bodyText().includes('PROMPT-PREVIEW-7F'));
    // The remaining spellings the drawer's digit gate names: Number coerces
    // '0x1' and ' 1 ' to 1, so each must fail closed through the same gate.
    if (subRow) { subRow.setAttribute('data-attempt', '0x1'); subRow.click(); }
    check('a 0x1 attempt index fails closed to the request view',
      attemptSel() === null && !title().includes('attempt') &&
        bodyText().includes('PROMPT-PREVIEW-7F'));
    if (subRow) { subRow.setAttribute('data-attempt', ' 1 '); subRow.click(); }
    check('a space-padded attempt index fails closed to the request view',
      attemptSel() === null && !title().includes('attempt') &&
        bodyText().includes('PROMPT-PREVIEW-7F'));
    d.querySelector('#tbl-requests tr.exp-row[data-id="att-open"]:not(.retry-sub)').click();
    check('clicking the parent row still opens the parent detail',
      !back() && !title().includes('attempt') && bodyText().includes('PROMPT-PREVIEW-7F'));
    // The selection must survive a wholesale record replacement through the
    // full-resync render: the (id, index) key re-resolves from the fresh
    // record - attempts are append-only, so the grown array must show in the
    // title (a captured attempt object would still say 2).
    links()[0].click();
    fire('snapshot', { feed_id: 'feedB', seq: 6, incremental: false,
      records: [mkRec('fresh1', 200, 1700000100000), mkRec('fresh2', 200, 1700000101000),
        mkRec('fresh3', 200, 1700000102000), mkRec('fresh4', 200, 1700000103000),
        { ...mkRec('sel-live', 200, 1700000104000), duration_ms: 999 },
        { ...mkRec('att-open', 200, 1700000104000), retries: 3,
          attempts: [...atts.map(a => ({ ...a })),
            { status_code: 500, error_type: 'server_error', at: new Date(1700000105100).toISOString() }],
          prompt_preview: 'PROMPT-PREVIEW-7F', response_preview: 'RESPONSE-PREVIEW-7F' }],
      counters: { in_flight: 0, total_requests: 6, total_errors: 0 } });
    await sleep(20);
    check('the attempt selection re-resolves through a wholesale record replacement',
      title().includes('attempt 1 of 3') && title().includes('502') &&
        bodyText().includes('req_meta_1') && attemptSel() === 0 &&
        d.getElementById('drawer').classList.contains('open'));
    w.eval('drawerAttempt = 99');
    w.renderDrawer();
    check('a dangling attempt key fails closed to the parent view',
      !back() && !title().includes('attempt') && bodyText().includes('PROMPT-PREVIEW-7F') &&
        attemptSel() === null);
    links()[0].click();
    check('back in attempt mode for the lifecycle rows', title().includes('attempt 1 of 3'));
    d.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Escape' }));
    check('Escape closes the drawer and clears the attempt mode',
      d.getElementById('drawer').hidden && w.eval('drawerId') === null && attemptSel() === null);
    d.querySelector('.retry-toggle[data-retry-for="att-open"]').click();
    const sub0 = d.querySelector('tr.retry-sub[data-retry-of="att-open"][data-attempt="0"]');
    if (sub0) sub0.click();
    check('a fresh open from a sub-row click lands in attempt mode',
      d.getElementById('drawer').classList.contains('open') && title().includes('attempt 1 of 3'));
    d.querySelector('#drawer .nav-btns button[onclick="drawerNav(1)"]').click();
    check('drawerNav clears the attempt mode',
      !title().includes('attempt') && attemptSel() === null &&
        bodyText().includes('sel-live') && bodyText().includes('999ms'));
    w.closeDrawer();
    check('closing the drawer clears the attempt mode',
      d.getElementById('drawer').hidden && w.eval('drawerId') === null && attemptSel() === null);
  }

  // ---- test 7g: the attempt pill class unification ----
  // A degenerate absorbed attempt - an HTTP 200 that still carried an
  // error payload and was retried - must wear the same pill class on
  // every surface: the class computation is one owner (attemptPillClass),
  // so the drawer's retry list, the attempt view and the log sub-row can
  // never disagree about what a 1xx-3xx failure looks like.
  {
    const atts = [
      { status_code: 200, error_type: 'upstream_error', error_msg: 'degenerate 200 with an error payload',
        at: new Date(1700000104500).toISOString() },
    ];
    fire('end', { record: { ...mkRec('att-200', 200, 1700000104000), retries: 1, attempts: atts }, in_flight: 0 });
    await sleep(20);
    w.openDrawer('att-200');
    const listPill = d.querySelector('.attempt-link .pill');
    check('the drawer retry list classes the degenerate 200 attempt warn',
      listPill.classList.contains('warn') && !listPill.classList.contains('err'));
    d.querySelector('.attempt-link').click();
    const viewPill = [...d.querySelectorAll('#drawer-body .detail-kv')]
      .find(el => el.querySelector('.k')?.textContent === 'status')?.querySelector('.v .pill');
    check('the attempt view status pill wears the same class',
      viewPill && viewPill.classList.contains('warn') && !viewPill.classList.contains('err'));
    w.closeDrawer();
    d.querySelector('.retry-toggle[data-retry-for="att-200"]').click();
    const subPill = d.querySelector('tr.retry-sub[data-retry-of="att-200"] .pill');
    check('the log sub-row pill wears the same shared class',
      subPill && subPill.classList.contains('warn') && !subPill.classList.contains('err'));
  }


  // ---- test 7h: in-band upstream entries are health evidence, not retries ----
  // A committed keepalive 200 can hide an upstream 429; the record keeps it as
  // an in_band attempt entry so the health predicates see the real status. No
  // retry disclosure may present it as an absorbed retry.
  {
    const atts = [
      { status_code: 502, error_type: 'bad_gateway', error_msg: 'upstream overloaded',
        at: new Date(1700000104500).toISOString() },
      { status_code: 429, error_type: 'rate_limit_error', error_msg: 'slow down', in_band: true,
        at: new Date(1700000104800).toISOString() },
    ];
    fire('end', { record: { ...mkRec('att-inband', 200, 1700000104000),
      error_type: 'rate_limit_error', error_msg: 'slow down', retries: 1, attempts: atts }, in_flight: 0 });
    await sleep(20);
    const row = d.querySelector('#tbl-requests tr.exp-row[data-id="att-inband"]:not(.retry-sub)');
    const toggle = row && row.querySelector('.retry-toggle');
    check('an in-band entry does not inflate the retry badge',
      !!toggle && toggle.textContent.includes('1') && !toggle.textContent.includes('2'));
    w.openDrawer('att-inband');
    const links = [...d.querySelectorAll('.attempt-link[data-open-req]')];
    check('the drawer retry list has one link at the original index',
      links.length === 1 && links[0].dataset.attempt === '0');
    const retrySection = [...d.querySelectorAll('#drawer-body .detail-section')]
      .find(s => s.querySelector('h4')?.textContent.startsWith('Retry attempts'));
    check('the drawer retry section lists only the real retry',
      !!retrySection && retrySection.querySelectorAll('.attempt-link').length === 1);
    w.closeDrawer();
    const toggleAgain = d.querySelector('#tbl-requests tr.exp-row[data-id="att-inband"]:not(.retry-sub) .retry-toggle');
    if (toggleAgain) toggleAgain.click();
    const subs = [...d.querySelectorAll('tr.retry-sub[data-retry-of="att-inband"]')];
    check('the log expands one retry sub-row with its original attempt index',
      subs.length === 1 && subs[0].dataset.attempt === '0' && subs[0].hidden === false);
  }
};

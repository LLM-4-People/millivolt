// test 1: full render painted 30 rows
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function renderLive(H) {
  const {JSDOM, STATIC, check, d, fire, fs, mkRec, path, rows, settleUntil, sleep, w} = H;
  const state = H.state;

  // ---- test 1: full render painted 30 rows ----
  check('full render shows 30 rows', rows().length === 30);
  {
    const table = d.querySelector('#tbl-requests table');
    const cols = [...d.querySelectorAll('#tbl-requests colgroup col')];
    const ths = [...d.querySelectorAll('#tbl-requests thead th')];
    const labels = ths.map(th => th.textContent);
    const expectedLabels = ['time', 'client · provider · model', 'status', 'ttft', 'tps',
      'in/out', 'cache', 'tools', 'finish', 'dur', 'turns', 'queued', 'cost', ''];
    const firstCell = d.querySelector('#tbl-requests tr.exp-row td');
    check('the request table column contract is literal',
      w.eval('REQ_COLS') === 14 &&
      labels.length === 14 && labels.join('|') === expectedLabels.join('|'));
    check('the request table has no hand-tuned column widths',
      cols.length === 0 && !table.style.minWidth && !table.style.width &&
      ths.every(th => !th.style.minWidth));
    check('the request table sizes every column to its content',
      !!table && w.getComputedStyle(table).tableLayout === 'auto');
    check('the request log is a labelled keyboard-scrollable region', (() => {
      const box = d.getElementById('tbl-requests');
      return box.getAttribute('role') === 'region' &&
        box.getAttribute('aria-label') === 'Request log' && box.tabIndex === 0;
    })());
    check('the actions header has an accessible name',
      ths[13]?.getAttribute('aria-label') === 'Request actions');
    check('request cells clip and ellipsize only as a below-floor backstop',
      !!firstCell && w.getComputedStyle(firstCell).overflow === 'hidden' &&
      w.getComputedStyle(firstCell).textOverflow === 'ellipsis');
    check('the column floors ratchet upward and never shrink', (() => {
      const saved = w.eval('REQ_FLOORS.slice()');
      const grew = w.eval('reqRatchetWidths([101, 55, 0, NaN, -4])');
      const first = w.eval('REQ_FLOORS.slice()');
      const again = w.eval('reqRatchetWidths([80, 40])');
      const second = w.eval('REQ_FLOORS.slice()');
      w.eval('REQ_FLOORS.splice(0, REQ_FLOORS.length, ...' + JSON.stringify(saved) + ')');
      return grew === true && again === false &&
        first[0] === 101 && first[1] === 55 &&
        first.slice(2).join(',') === saved.slice(2).join(',') &&
        second.join(',') === first.join(',');
    })());
    check('the header row spans every request column',
      !!table && table.tHead?.rows?.[0]?.cells.length === w.eval('REQ_COLS'));
    check('every live status transition is width-relevant', (() => {
      const live = {client: 'c', provider: 'p', model: 'm', status_code: 0,
        attempts: [], live: true, paused: true, throttled: false, stream: true,
        debug: false};
      const cases = [
        ['pause release', {...live, paused: false}, true],
        ['throttle on', {...live, paused: false, throttled: true}, true],
        ['stream stop', {...live, stream: false}, true],
        ['debug on', {...live, debug: true}, true],
        ['token churn alone', {...live, usage: {input_tokens: 1}}, false],
      ];
      return cases.every(([, next, expected]) =>
        w.eval('reqWidthRelevant(' + JSON.stringify(live) + ', ' +
          JSON.stringify(next) + ')') === expected);
    })());
  }
  check('dimension picker trigger exists', !!d.getElementById('xp-dim-trigger'));
  check('dimension rail still has nine dims', d.querySelectorAll('#xp-rail .xp-rail-item').length === 9);
  {
    // Disclosure contract: the trigger expands the rail group it controls.
    // It is not a listbox popup, so it must not advertise aria-haspopup.
    const trigger = d.getElementById('xp-dim-trigger');
    const rail = d.getElementById('xp-rail');
    check('the dimension trigger is a disclosure for its rail group, not a listbox popup',
      !trigger.hasAttribute('aria-haspopup') &&
      trigger.getAttribute('aria-controls') === 'xp-rail' &&
      trigger.getAttribute('aria-expanded') === 'false' &&
      rail.getAttribute('role') === 'group');
  }
  check('first paint fetches chart and explorer exactly once when model rules arrive',
    state.initialFetches.filter(u => u.includes('/metrics/agg/chart')).length === 1 &&
    state.initialFetches.filter(u => u.includes('/metrics/agg/explorer')).length === 1);
  check('first chart fetch starts its refresh cadence instead of refetching on the first tick', w.eval('_lastChartFetch > 0'));

  // Shared explorer health signals/order and server-authored conversations.
  {
    const order = ['provider','model','client','conversation','tool','time','status','error','key'];
    check('explorer dimension order is the single desktop/mobile order',
      [...d.querySelectorAll('#xp-rail [data-xp-dim]')].map(n=>n.dataset.xpDim).join()===order.join());
    const entity = {name:'neutral.example',n:3,cost:0,in:0,out:0,cache:0,err_final:1,
      rate_limit_requests:2,tools:0,cost_per_mtok:null,ttft_p50:null,ttft_p95:null,tps_p50:null,tps_p95:null,
      err_events:4,code:'500',last_ms:0};
    const renderCard = (dim, overrides={}) => {
      const host=d.createElement('div');
      host.innerHTML=w.xpNodeCard({activeDim:dim,filters:[]},{...entity,...overrides},{total:3,error_total:4});
      return host;
    };
    for (const dim of order) {
      const card=renderCard(dim);
      check(`${dim} has one separate error/429 request tally`,
        card.querySelectorAll('.xp-node-err').length===1 && card.querySelector('.xp-node-err').textContent==='1 err' &&
        card.querySelectorAll('.xp-node-rate').length===1 && card.querySelector('.xp-node-rate').textContent==='2 ×429' &&
        card.querySelector('.xp-node-rate').title==='Requests with final or retried HTTP 429; each request counted once' &&
        card.querySelector('.xp-node-signals').textContent==='1 err · 2 ×429');
      for (const [name, counts, expected] of [
        ['zero', {err_final:0,rate_limit_requests:0}, ''],
        ['error only', {err_final:1,rate_limit_requests:0}, '1 err'],
        ['429 only', {err_final:0,rate_limit_requests:2}, '2 ×429'],
        ['missing', {err_final:undefined,rate_limit_requests:undefined}, ''],
      ]) {
        const only=renderCard(dim,counts);
        check(`${dim} ${name} health signals omit zero badges and stray separators`,
          (only.querySelector('.xp-node-signals')?.textContent || '')===expected &&
          !!only.querySelector('.xp-node-err')===(counts.err_final>0) &&
          !!only.querySelector('.xp-node-rate')===(counts.rate_limit_requests>0) &&
          !only.querySelector('.xp-node-signals [aria-hidden]'));
      }
      if (['status','key','time'].includes(dim)) check(`${dim} grid does not duplicate the error count`,
        !card.querySelector('.xp-node-kpis').textContent.includes('errors'));
    }
    for(const dim of ['status','key']) check(`${dim} retains its error-rate KPI`,renderCard(dim).querySelector('.xp-node-kpis').textContent.includes('err rate'));
    // The conversation card renders the wire's per-group tool-call sum with
    // the tool dimension's exact KPI: a chat-only conversation keeps the real 0
    // (never a fabricated gap), a missing field keeps the shared '-'
    // placeholder, and the slot order stays a stable economics-then-wait row.
    check('conversation card renders the wire tools KPI like the tool dimension',
      renderCard('conversation').querySelector('.xp-node-kpis').textContent.includes('calls 0') &&
      renderCard('conversation',{tools:47}).querySelector('.xp-node-kpis').textContent.includes('calls 47') &&
      renderCard('tool',{tools:47}).querySelector('.xp-node-kpis').textContent.includes('calls 47'));
    check('conversation tools KPI keeps the shared placeholder for an empty field',
      renderCard('conversation',{tools:undefined}).querySelector('.xp-node-kpis').textContent.includes('calls -') &&
      renderCard('conversation',{tools:null}).querySelector('.xp-node-kpis').textContent.includes('calls -'));
    check('conversation KPI slots stay a stable economics-then-wait row', (() => {
      const want = ['cost','tok','calls','ttft'];
      const spans = [...renderCard('conversation',{tools:3}).querySelectorAll('.xp-node-kpis > span')];
      return spans.length === want.length && want.every((w, i) => spans[i].textContent.startsWith(w));
    })());
    // shareBar: the % flows through the pct/pctCap owner, so a
    // strictly-sub-1 fraction can never round up to a false '100%' and a
    // full fraction is a true 100. An unmeasured share (zero denominator)
    // renders no share row at all, never a fabricated 0%.
    check('shareBar never rounds a sub-100 share to a false 100%',
      w.shareBar(0.9998, '#56B4E9').includes('99.9% of total') &&
        !w.shareBar(0.9998, '#56B4E9').includes('100% of total') &&
        w.shareBar(1, '#56B4E9').includes('100.0% of total'));
    check('an unmeasured share renders no fabricated 0% row', (() => {
      const measured = renderCard('provider');
      const zero = d.createElement('div');
      zero.innerHTML = w.xpNodeCard({activeDim:'provider',filters:[]}, {...entity, n: 3}, {total: 0, error_total: 0});
      const zeroErr = d.createElement('div');
      zeroErr.innerHTML = w.xpNodeCard({activeDim:'error',filters:[]}, {...entity, err_events: 2}, {total: 0, error_total: 0});
      return !!measured.querySelector('.xp-node-share') &&
        measured.querySelector('.xp-node-share').title === 'of requests' &&
        !zero.querySelector('.xp-node-share') && !zero.textContent.includes('0%') &&
        !zeroErr.querySelector('.xp-node-share') && !zeroErr.textContent.includes('0%');
    })());
    // Error-dimension shares stay in affected-request units (N/error_total),
    // never failure-event occurrences: a request with three absorbed 500s is
    // still one affected request, so its card can never show more than 100%.
    const errorShare = (n, errEvents, total) => {
      const host = d.createElement('div');
      host.innerHTML = w.xpNodeCard({activeDim:'error',filters:[]},
        {...entity, name:'upstream|500|multi', n, err_events:errEvents},
        {total:10, error_total:total});
      return host.querySelector('.xp-node-share');
    };
    const multiShare = errorShare(1, 3, 2);
    check('error gallery share uses affected requests, never failure-event occurrences',
      multiShare?.title === 'of error requests' &&
      multiShare.querySelector('.share-pct')?.textContent === '50.0%' &&
      errorShare(1, 1, 2).querySelector('.share-pct')?.textContent === '50.0%' &&
      errorShare(2, 4, 2).querySelector('.share-pct')?.textContent === '100.0%');
    // Tool cards use the same affected-request unit for N/share while the
    // calls KPI stays in call units: one request with three same-name calls is
    // 1 req at 100%, never 3 req above the denominator, and calls reads 3.
    const toolShare = d.createElement('div');
    toolShare.innerHTML = w.xpNodeCard({activeDim:'tool',filters:[]},
      {...entity, name:'lookup', n:1, tools:3}, {total:1, error_total:0});
    check('tool gallery share counts affected requests while calls stays call-counted',
      toolShare.querySelector('.xp-node-share')?.title === 'of requests' &&
      toolShare.querySelector('.share-pct')?.textContent === '100.0%' &&
      toolShare.querySelector('.xp-node-kpis').textContent.includes('calls 3'));
    const savedAgg=w.eval('explorerAgg');
    const trigger=d.getElementById('xp-dim-trigger'), originalStyle=w.getComputedStyle;
    try {
      w.eval('explorerAgg={rail:{conversation:4},conversation_summary:{main:1,sub:2,unresolved:1}}');
      w.renderDimRail({activeDim:'conversation',filters:[]});
      const summary='1 main + 2 sub + 1 unresolved';
      check('conversation rail headline uses server main/sub/unresolved counts',d.querySelector('[data-xp-dim="conversation"] .rail-n').textContent===summary);
      check('mobile dimension trigger reuses the same conversation summary',trigger.querySelector('.rail-n').textContent===summary);
      w.getComputedStyle=el=>el===trigger?{display:'flex'}:originalStyle(el);
      w.toggleDimMenu();
      check('mobile picker opens the same ordered dimension nodes',trigger.getAttribute('aria-expanded')==='true' &&
        [...d.querySelectorAll('#xp-rail [data-xp-dim]')].map(n=>n.dataset.xpDim).join()===order.join());
      w.closeDimMenu();
      check('mobile picker closes without losing summary',trigger.getAttribute('aria-expanded')==='false' && trigger.querySelector('.rail-n').textContent===summary);
    } finally {w.getComputedStyle=originalStyle;w.__restoreExplorer=savedAgg;w.eval('explorerAgg=window.__restoreExplorer;delete window.__restoreExplorer');w.renderDimRail(w.explorerState());}
    const parent='s:parent"<neutral>',scope=[{dim:'client',id:'client'},{dim:'key',id:'hashed-key'},{dim:'conversation',id:parent}];
    const child=renderCard('conversation',{name:'s:child',conversation:{role:'sub',parent_id:parent,parent_in_scope:false,parent_scope:scope}});
    const link=child.querySelector('a.xp-conversation-parent');
    check('subconversation role and external parent link are visible',child.querySelector('.xp-conversation-role').textContent==='sub' &&
      !!link && !child.querySelector('button a') && link.textContent.includes('outside view'));
    check('parent pivot retains exact namespace and leaf filter',link?.getAttribute('href')===w.buildHash([...scope,{dim:'by',id:'conversation'}]) && link.title.includes('descendants are not included'));
    check('parent identifiers are escaped as text and a hash URL',!child.querySelector('neutral') && !child.querySelector('[onerror]') && link.textContent.includes(parent.slice(2)));
    check('unobserved parents are distinct from filtered-out parents',
      renderCard('conversation',{conversation:{role:'sub',parent_id:parent,parent_observed:false,parent_in_scope:false}}).querySelector('.xp-conversation-parent').textContent.includes('not observed'));
    for(const badScope of [undefined,[{dim:'client',id:'client'},{dim:'key',id:''},{dim:'conversation',id:parent}]]) {
      const unresolved=renderCard('conversation',{conversation:{role:'sub',parent_id:parent,parent_scope:badScope}});
      check('unfilterable parent remains visible without a broadened link',!unresolved.querySelector('a.xp-conversation-parent') && unresolved.querySelector('.xp-conversation-parent').textContent.includes('parent'));
    }
    check('unknown relationships stay explicitly unresolved',renderCard('conversation',{conversation:{role:'unknown',issue:'multiple_scopes'}}).querySelector('.xp-conversation-role').textContent==='unresolved');
    check('main conversation role is explicit',renderCard('conversation',{conversation:{role:'main'}}).querySelector('.xp-conversation-role').textContent==='main');
    check('main and sub tooltips state only observed facts',
      renderCard('conversation',{conversation:{role:'main'}}).querySelector('.xp-conversation-role').title==='No parent declared in observed history' &&
      child.querySelector('.xp-conversation-role').title==='Client-declared parent conversation');
  }

  // Explorer render gates: unchanged aggregates must not rewrite the rail
  // trigger or re-run gallery layout sizing.
  {
    const probe = d.createElement('div');
    probe.id = 'xp-update-probe';
    d.body.appendChild(probe);
    try {
      check('updateSection returns true for first and changed content',
        w.updateSection('xp-update-probe', 'first') === true &&
        w.updateSection('xp-update-probe', 'changed') === true);
      check('updateSection returns false for identical content and a missing zone',
        w.updateSection('xp-update-probe', 'changed') === false &&
        w.updateSection('xp-update-probe-missing', 'content') === false);
      check('updateSection still writes the latest content exactly', probe.innerHTML === 'changed');
    } finally {
      probe.remove();
      w.eval("delete lastRender['xp-update-probe']");
    }

    const originalSizeGallery = w.sizeGallery;
    const savedAgg = w.eval('explorerAgg');
    const gallery = d.getElementById('xp-gallery');
    const gateGroup = (name, n) => ({name, n, cost:0, in:10, out:5, cache:2, tools:0,
      err_final:0, rate_limit_requests:0, cost_per_mtok:null,
      ttft_p50:null, ttft_p95:null, tps_p50:null, tps_p95:null});
    const galleryAgg = {
      dim: 'provider', total: 10, error_total: 0, rail: {provider: 2},
      scopeKey: w.eval('scopeQS()'),
      groups: [gateGroup('gate-alpha.example', 6), gateGroup('gate-beta.example', 4)],
    };
    let sizeCalls = 0;
    try {
      w.sizeGallery = (...args) => { sizeCalls++; return originalSizeGallery(...args); };
      w.__restoreGalleryAgg = savedAgg;
      w.eval('explorerAgg = ' + JSON.stringify(galleryAgg));
      w.renderGallery({activeDim: 'provider', filters: []});
      const galleryHTML = gallery.innerHTML;
      const firstCalls = sizeCalls;
      check('a changed gallery payload writes its cards and runs sizing',
        firstCalls === 1 && gallery.querySelectorAll('.xp-node').length === 2 &&
        galleryHTML.includes('gate-alpha.example'));
      w.renderGallery({activeDim: 'provider', filters: []});
      check('an unchanged gallery render leaves the node, HTML and sizing untouched',
        sizeCalls === firstCalls && gallery.innerHTML === galleryHTML &&
        d.getElementById('xp-gallery') === gallery);
      w.eval('explorerAgg = ' + JSON.stringify({
        ...galleryAgg,
        groups: [gateGroup('gate-alpha.example', 9), gateGroup('gate-beta.example', 4)],
      }));
      w.renderGallery({activeDim: 'provider', filters: []});
      check('a changed group count repaints the cards and runs sizing again',
        sizeCalls === firstCalls + 1 && gallery.innerHTML !== galleryHTML &&
        gallery.querySelector('[data-xp-key="gate-alpha.example"] .xp-node-n')?.textContent.startsWith('9'));
      // The synthetic overflow card carries an explicit marker and no filter
      // key: a real value named "other" stays independently selectable.
      const originalNavigateTo = w.navigateTo;
      const navCalls = [];
      try {
        w.navigateTo = segments => navCalls.push(segments);
        w.eval('explorerAgg = ' + JSON.stringify({
          ...galleryAgg,
          dim: 'provider',
          total: 3,
          groups: [
            {...gateGroup('other', 2), synthetic: true},
            gateGroup('other', 1),
          ],
        }));
        w.renderGallery({activeDim: 'provider', filters: []});
        const synthetic = gallery.querySelector('.xp-node[data-xp-synthetic]');
        const real = gallery.querySelector('.xp-node[data-xp-key="other"]');
        check('the synthetic overflow card is marked, labelled and not filterable',
          !!synthetic && !synthetic.hasAttribute('data-xp-key') &&
          synthetic.getAttribute('aria-disabled') === 'true' &&
          synthetic.textContent.includes('other (grouped)') && !!real);
        synthetic?.click();
        check('clicking the synthetic overflow card never adds a filter', navCalls.length === 0);
        real?.click();
        check('a real value named other remains independently selectable',
          navCalls.length === 1 && navCalls[0].length === 1 &&
          navCalls[0][0].dim === 'provider' && navCalls[0][0].id === 'other');
      } finally {
        w.navigateTo = originalNavigateTo;
        w.eval('explorerAgg = ' + JSON.stringify(galleryAgg));
      }
    } finally {
      w.sizeGallery = originalSizeGallery;
      w.eval('explorerAgg=window.__restoreGalleryAgg;delete window.__restoreGalleryAgg');
      w.renderGallery(w.explorerState());
    }

    const trigger = d.getElementById('xp-dim-trigger');
    let mutations = [];
    const observer = new w.MutationObserver(records => mutations.push(...records));
    observer.observe(trigger, {attributes: true, childList: true, subtree: true, characterData: true});
    try {
      w.__restoreRailAgg = savedAgg;
      w.eval('explorerAgg = ' + JSON.stringify({dim: 'provider', rail: {provider: 77, model: 3}}));
      w.renderDimRail({activeDim: 'provider', filters: []});
      check('a changed rail payload repaints the dimension trigger',
        await settleUntil(() => mutations.length > 0 && trigger.querySelector('.rail-n')?.textContent === '77'));
      const firstMutations = mutations.length;
      w.renderDimRail({activeDim: 'provider', filters: []});
      // Negative proof: an unchanged render must not touch the observed node.
      await sleep(0);
      check('an unchanged rail render does not touch the dimension trigger',
        mutations.length === firstMutations);

      mutations = [];
      w.renderDimRail({activeDim: 'model', filters: []});
      const expectedHost = d.createElement('div');
      expectedHost.innerHTML = `<span class="xp-dim-by">by</span>${w.entityBadge('model', '', 'Model')}<span class="rail-n">3</span><svg class="hdr-ico xp-dim-caret" viewBox="0 0 16 16" aria-hidden="true"><path d="M4 6.5 8 10.5 12 6.5"/></svg>`;
      check('an active-dimension change repaints the complete trigger presentation',
        await settleUntil(() => mutations.length > 0 && trigger.innerHTML === expectedHost.innerHTML &&
          trigger.style.getPropertyValue('--ent') === '#CC79A7' &&
          trigger.getAttribute('aria-label') === 'break down by Model' &&
          trigger.title === 'break down by Model' &&
          trigger.querySelector('.xp-dim-caret path')?.getAttribute('d') === 'M4 6.5 8 10.5 12 6.5'));

      mutations = [];
      w.eval('explorerAgg = ' + JSON.stringify({dim: 'provider', rail: {provider: 77, model: 4}}));
      w.renderDimRail({activeDim: 'model', filters: []});
      check('a rail count change repaints the dimension trigger again',
        await settleUntil(() => mutations.length > 0 && trigger.querySelector('.rail-n')?.textContent === '4'));

      const staleHost = d.createElement('div');
      staleHost.innerHTML = `<span class="xp-dim-by">by</span>${w.entityBadge('model', '', 'Model')}<span class="rail-n">4</span><svg class="hdr-ico xp-dim-caret" viewBox="0 0 16 16" aria-hidden="true"><path d="M4 6.5 8 10.5 12 6.5"/></svg>`;
      const stale = trigger.cloneNode(false);
      trigger.replaceWith(stale);
      w.renderDimRail({activeDim: 'model', filters: []});
      check('a replaced trigger node is repainted even while the module key matches',
        await settleUntil(() => stale.innerHTML === staleHost.innerHTML &&
          stale.getAttribute('aria-label') === 'break down by Model'));
    } finally {
      observer.disconnect();
      w.eval('explorerAgg=window.__restoreRailAgg;delete window.__restoreRailAgg');
      w.renderDimRail(w.explorerState());
    }
  }

  // ---- test 2: live 'record' event → incremental insertion, stable nodes ----
  const oldFifth = rows()[5];
  fire('record', { ...mkRec('reqnew0', 200, 1700000031000) }, '31');
  check('record event inserts new row at top',
    await settleUntil(() => rows().length === 31 && rows()[0].dataset.id === 'reqnew0'));
  check('existing rows keep the SAME nodes (no table rebuild)', rows().includes(oldFifth));

  // ---- test 3: in-flight begin → final end replaces ONLY that row ----
  const inRec = { ...mkRec('reqnew1', 200, 1700000032000) };
  delete inRec.status_code;
  fire('begin', { record: inRec, in_flight: 1 });
  await settleUntil(() => !!d.querySelector('#tbl-requests tr.live-row[data-id="reqnew1"]'));
  const inRow = d.querySelector('#tbl-requests tr.live-row[data-id="reqnew1"]');
  check('begin event renders the in-flight live-row', !!inRow);
  const stable = rows().find(tr => tr === oldFifth);
  fire('end', { record: mkRec('reqnew1', 200, 1700000032000), in_flight: 0 });
  await settleUntil(() => {
    const row = d.querySelector('#tbl-requests tr.exp-row[data-id="reqnew1"]');
    return !!row && !row.classList.contains('live-row') && row.textContent.includes('200');
  });
  const finRow = d.querySelector('#tbl-requests tr.exp-row[data-id="reqnew1"]');
  check('end event finalizes only that row', finRow && !finRow.classList.contains('live-row') && finRow.textContent.includes('200'));
  check('unrelated rows untouched across lifecycle events', rows().some(tr => tr === stable));
  check('two rows for same id never coexist', d.querySelectorAll('#tbl-requests tr.exp-row[data-id="reqnew1"]').length === 1);

  // ---- test 4: scroll anchoring (user scrolled into the list) ----
  const cont = d.getElementById('tbl-requests');
  cont.scrollTop = 200;
  fire('record', { ...mkRec('reqnew2', 200, 1700000033000) }, '32');
  check('scrollTop compensated for the inserted row height',
    await settleUntil(() => cont.scrollTop === 220));

  // ---- test 5: incremental snapshot merge (SSE reconnect with cursor) ----
  fire('snapshot', { feed_id: 'feedA', seq: 34, incremental: true,
    records: [{ ...mkRec('reqnew3', 200, 1700000034000) }],
    counters: { in_flight: 0, total_requests: 34, total_errors: 0 } });
  check('incremental snapshot merges without full replace',
    await settleUntil(() => rows().includes(oldFifth) && rows().length === 34));

  // ---- test 6: feed change (proxy restart) → full replace ----
  fire('snapshot', { feed_id: 'feedB', seq: 2, incremental: false,
    records: [{ ...mkRec('fresh1', 200, 1700000100000) }, { ...mkRec('fresh2', 200, 1700000101000) }],
    counters: { in_flight: 0, total_requests: 2, total_errors: 0 } });
  await settleUntil(() => rows().length === 2);
  const ids = rows().map(tr => tr.dataset.id);
  check('feed change resets view to the new snapshot', rows().length === 2 && ids.includes('fresh1') && ids.includes('fresh2'));

  // ---- test 7: stale-cursor poll (since > latest) → server full replaces ----
  // (Feed B has seq 2; a poll with the old feedA cursor would ask since=34 -
  // the page's own cursor was invalidated to 2, so the page asks since=2 and
  // the stub returns an in-range delta. Verify the page cursor followed.)
  state.fullPayload = {
    feed_id: 'feedB', seq: 3,
    records: [{ ...mkRec('fresh1', 200, 1700000100000), __seq: 1 },
              { ...mkRec('fresh2', 200, 1700000101000), __seq: 2 },
              { ...mkRec('fresh3', 200, 1700000102000), __seq: 3 }],
    counters: { in_flight: 0, total_requests: 3, total_errors: 0 },
  };
  // Trigger the page's own 5s poll path by simulating a disconnected SSE-less
  // state is awkward; instead assert the fetch the page would make is formed
  // correctly by spying on window.fetch calls once (invoked manually).
  let fetchedSince = null;
  const origFetch = w.fetch;
  // Forward BOTH args - later tests POST via this wrapper, and dropping the
  // options object silently turns their POSTs into GET-shaped calls.
  w.fetch = (url, opts) => { fetchedSince = new URL(url, 'http://x').searchParams.get('since'); return origFetch(url, opts); };
  // run the page's poll tick handler? The interval skips when SSE 'connected'.
  // The page's sse readyState is 1, so polling is dormant - verify cursor by
  // calling the page's own snapshot application with an in-range delta.
  fire('snapshot', { feed_id: 'feedB', seq: 3, incremental: true,
    records: [{ ...mkRec('fresh3', 200, 1700000102000) }],
    counters: { in_flight: 0, total_requests: 3, total_errors: 0 } });
  check('post-restart incremental delta applies on the new feed',
    await settleUntil(() => rows().length === 3 &&
      rows().includes(rows().find(tr => tr.dataset.id === 'fresh3'))));

  // ---- test 7b: a kpiAgg swap with an EMPTY delta must still repaint the
  // KPI band (the derivation is memoized on the record revision -
  // applyBootstrapState bumps _rev when the aggregate changes).
  // 7b/7c drive the resume cursor by hand, so stop the page's own 5s tick for
  // the window: a tick landing while 7b-2's seq-4 stub is installed advances
  // lastSeq and turns 7c's since=3 premise into since=4. Re-armed after 7c.
  w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
  const origBootstrap = w.fetch;
  let bootstrapStateApplied = false;
  w.fetch = (url) => {
    const u = String(url);
    if (u.includes('/metrics/bootstrap')) {
      bootstrapStateApplied = true;
      return Promise.resolve({ ok: true, json: async () => ({
        records: [], counters: { in_flight: 0, total_requests: 3, total_errors: 0 },
        seq: 3, feed_id: 'feedB', incremental: true,
        kpi: { requests: 31, errors: 1, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0 },
        dash: {}, pause: {}, throttle: {},
      }) });
    }
    return origBootstrap(url);
  };
  w.eval("fetchBootstrap('none')");
  await settleUntil(() => bootstrapStateApplied &&
    d.getElementById('kpis').textContent.includes('31'));
  w.fetch = origBootstrap;
  check('bootstrap state-only pull repaints the band from fresh kpiAgg',
    bootstrapStateApplied && d.getElementById('kpis').textContent.includes('31'));

  // ---- test 7b-2: a never-measured aggregate renders derived rate tiles as
  // '-' through the shared pct owner, never a fabricated 0%, while counts
  // stay honest zeros (0 requests, 0 of 0, 0 / 0 in).
  w.fetch = (url) => {
    const u = String(url);
    if (u.includes('/metrics/bootstrap')) {
      return Promise.resolve({ ok: true, json: async () => ({
        records: [], counters: { in_flight: 0, total_requests: 0, total_errors: 0 },
        seq: 4, feed_id: 'feedB', incremental: true,
        kpi: { requests: 0, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0 },
        dash: {}, pause: {}, throttle: {},
      }) });
    }
    return origBootstrap(url);
  };
  w.eval("fetchBootstrap('none')");
  await settleUntil(() => {
    const text = d.getElementById('kpis').textContent;
    return text.includes('0 of 0') && text.includes('0 / 0 in') && !text.includes('%');
  });
  w.fetch = origBootstrap;
  const neverMeasured = d.getElementById('kpis').textContent;
  check('a never-measured band shows - rates and honest zero counts',
    neverMeasured.includes('0 of 0') && neverMeasured.includes('0 / 0 in') &&
    !neverMeasured.includes('%'));

  // ---- test 7b-3: the static KPI skeleton in index.html and the live
  // renderer's first kpiHtml must agree on tile count, headings and class
  // structure, so the first payload only swaps text and nothing below the
  // band moves (the chart-tile skeleton pin is the same idea for totals).
  {
    const skelDoc = new JSDOM(fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8')).window.document;
    const shape = root => [...root.querySelectorAll('.kpi')].map(k => {
      const val = k.querySelector('.val');
      return {
        cls: [...k.classList].sort().join(' '),
        h2: (k.querySelector('h2') || {textContent: ''}).textContent,
        val: val ? [...val.classList].sort().join(' ') : '',
      };
    });
    const skel = shape(skelDoc.getElementById('kpis'));
    const firstRender = w.eval(`(() => {
      const saved = kpiAgg;
      kpiAgg = { requests: 0, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null,
        input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, avg_ttft_ms: null, avg_tps: null };
      const html = derive({ records: [], counters: { in_flight: 0 }, _rev: 1 }).kpiHtml;
      kpiAgg = saved;
      return html;
    })()`);
    const host = d.createElement('div');
    host.innerHTML = firstRender;
    const live = shape(host);
    check('KPI band skeleton matches the first-rendered tile structure',
      skel.length === live.length && JSON.stringify(skel) === JSON.stringify(live) &&
      JSON.stringify(live));
  }

  // The Avg TPS KPI uses the same compact unit as the Overview timing tile.
  // TPS_UNIT is the single owner; the chart axis, legend and tooltips keep the
  // full tok/s label where there is room.
  {
    const kpiHtml = w.eval(`(() => {
      const saved = kpiAgg;
      kpiAgg = { requests: 1, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null,
        input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0,
        avg_ttft_ms: null, avg_tps: 1234.56 };
      const out = derive({ records: [], counters: { in_flight: 0 }, _rev: 1 }).kpiHtml;
      kpiAgg = saved;
      return out;
    })()`);
    const tpsValue = w.eval('fmt(1234.56)');
    const tpsFull = w.eval('fmtFull(1234.56)');
    check('the Avg TPS KPI uses the compact t/s unit with a full-value title',
      kpiHtml.includes('title="' + tpsFull + ' t/s">' + tpsValue +
        '</span> <span class="unit">t/s</span>') &&
      !kpiHtml.includes('tok/s') && w.eval('TPS_UNIT') === 't/s');
    check('compact speed values keep their magnitude suffix apart from the unit', (() => {
      const plain = w.eval('fmt(148.239)');
      const compact = w.eval('fmt(1234.56)');
      return w.eval('fmtTPSCompact(148.239)') === plain + 't/s' &&
        w.eval('fmtTPSCompact(1234.56)') === compact + ' t/s' &&
        w.eval('fmtTPSCompact(null)') === '-';
    })());
  }
};

"""Chart canvas, overview, layout, table and content-fit checks."""

from .support import require


async def chart_checks(page, screenshot, results, layout_checks):
    """Drive the chart, overview, layout and request-table probes."""
    for viewport in ({'width': 1440, 'height': 1000}, {'width': 390, 'height': 844}):
        await page.set_viewport_size(viewport)
        for pct in ('50', '95', '99'):
            await page.select_option('#chart-pct', pct)
            # Sparse gate first: one measured bucket (the old
            # singleton case) must paint the waiting blank, never
            # a stranded lone point. Then a five-bucket measured
            # window - built from one actual server bucket so the
            # case stays independent of calendar-minute
            # boundaries - paints both lines across the plot.
            state = await page.evaluate('''async () => {
                const bucket = chartAgg.buckets.find(b => b.tps.every(Number.isFinite) && b.ttft.every(Number.isFinite) && b.dec.every(Number.isFinite));
                if (!bucket) throw new Error('fixture has no percentile-bearing bucket');
                const bm = chartAgg.bucket_ms;
                chartAgg = {...chartAgg, buckets:[bucket], from_ms:bucket.t, now_ms:bucket.t+bm};
                renderChart();
                await new Promise(requestAnimationFrame);
                const sparseBlank = !_up && !!document.querySelector('#chart-traffic canvas.chart-blank');
                // Layout-shift guard: the blank and the mounted
                // plot must own the exact same reserved space.
                const box = document.getElementById('chart-traffic');
                const pair = document.querySelector('.grid-pair');
                const sparseH = Math.round(box.getBoundingClientRect().height) + '/' + Math.round(pair.getBoundingClientRect().height);
                // One ladder ascends, the other descends:
                // each series is normalized to its own
                // min/max, so two same-slope ladders would
                // sit at identical normalized positions
                // whenever the range snap pads them
                // alike, letting the latency line occlude
                // the speed line exactly. Mirrored slopes
                // can coincide at only the single crossing,
                // so both lines always paint.
                const five = Array.from({length:5}, (_, k) => ({...bucket,
                    t: bucket.t + k*bm,
                    tps: bucket.tps.map(v => v * (1 + k/10)),
                    ttft: bucket.ttft.map(v => v * (1 + (4-k)/10)),
                    dec: bucket.dec.map(v => v * (1 + (4-k)/10))}));
                chartAgg = {...chartAgg, buckets:five, from_ms:bucket.t, now_ms:bucket.t+5*bm};
                renderChart();
                // uPlot commits setData in a microtask; inspect
                // the accepted frame, not its previous canvas.
                await new Promise(requestAnimationFrame);
                const mountedH = Math.round(box.getBoundingClientRect().height) + '/' + Math.round(pair.getBoundingClientRect().height);
                const pixels = _up.ctx.getImageData(0,0,_up.ctx.canvas.width,_up.ctx.canvas.height).data;
                let speed=0, latency=0;
                for (let i=0;i<pixels.length;i+=4) {
                    if (!pixels[i+3]) continue;
                    // cyan speed line: green dominant over blue
                    // (kept loose for thin antialiased strokes at
                    // low DPR); blue latency line: blue dominant.
                    if (pixels[i+1]>pixels[i]*1.3 && pixels[i+1]>pixels[i+2]) speed++;
                    if (pixels[i+2]>pixels[i]*1.3 && pixels[i+2]>pixels[i+1]*1.1) latency++;
                }
                const card=document.querySelector('.traffic-card');
                const overflow=[...card.querySelectorAll('*')].filter(e=>e.clientWidth && e.scrollWidth>e.clientWidth+2).map(e=>e.id||e.className);
                // The line-only domain must be the first and last
                // sample exactly, so the series touches both plot
                // edges instead of stranding a half bucket of dead
                // space on each side.
                const xs = _up.data[0];
                const plotW = _up.bbox.width / (window.devicePixelRatio || 1);
                const xMin = _up.scales.x.min, xMax = _up.scales.x.max;
                const edge = { first: (xs[0] - xMin) / (xMax - xMin) * plotW,
                               last: (xs[xs.length-1] - xMin) / (xMax - xMin) * plotW,
                               width: plotW };
                return {sparseBlank, sparseH, mountedH, points:_up.data[0].length, speed, latency, overflow, edge,
                        legend:document.querySelector('#traffic-legend').textContent};
            }''')
            require(state['sparseBlank'], state)
            require(state['sparseH'] == state['mountedH'], state)
            require(state['points'] == 5 and state['speed'] and state['latency'], state)
            require(state['edge']['first'] <= 1 and
                    state['edge']['width'] - state['edge']['last'] <= 1, state)
            require(not state['overflow'], state)
            require(not any(p in state['legend'] for p in ('p50', 'p95', 'p99')), state)
            results.append({'width': viewport['width'], 'pct': pct, **state})
    # Compaction contract on the real payload: buckets without
    # BOTH measurements at the selected percentile drop out of the
    # timeline (no gap points, no dead space). The kept set must
    # equal the both-measured buckets exactly, every plotted point
    # carries both lines, and the axes note reports the omission.
    # Read the columns, not the mounted plot: sparse windows stay
    # behind the sparse gate, and the data contract holds either
    # way.
    state = await page.evaluate('''async () => {
        const full = window.__fullChart;
        const idx = ['50','95','99'].indexOf(document.getElementById('chart-pct').value);
        const measured = full.buckets.filter(b => Number.isFinite(b.tps?.[idx]) && Number.isFinite(b.ttft?.[idx]) && Number.isFinite(b.dec?.[idx]));
        chartAgg = {...full};
        renderChart();
        await new Promise(requestAnimationFrame);
        const d = chartData();
        const kept = d ? d[0].length : 0;
        const bothFinite = d ? d[0].every((x, i) => Number.isFinite(d[1][i]) && Number.isFinite(d[2][i])) : false;
        const dropped = full.buckets.length - measured.length;
        return {kept, want: measured.length, bothFinite, dropped,
                note: document.getElementById('chart-context').textContent};
    }''')
    require(state['kept'] == state['want'] and state['bothFinite'], state)
    require(state['want'] >= 1, state)
    if state['dropped']:
        require('unmeasured intervals omitted' in state['note'], state)
    # Overview preset: the summary is metrics-only. No plot
    # ever mounts (no uPlot, no blank canvas), the card carries
    # tiles-only, and the bucket-cadence context row is hidden.
    # The recovered-429 fixture proves the health invariant end
    # to end: the health pair reads 0 errors / 2 rate limited in
    # the chart's two health colors, the requests/tokens pair and
    # the in/out/cached triple render their colored halves, and
    # the merged timing tile reads the server's period averages
    # (v-ttft / v-tps halves). Each tile carries ONE sparkline
    # with one self-scaled line per metric half in the half's
    # color, spanning only traffic-bearing buckets - the same
    # empty-bucket removal the plots apply. Every tile keeps one
    # fixed size (uniform heights, no vertical clip, the spark
    # full-height and pinned to the tile's bottom padding,
    # nothing reaching past the card), at every viewport.
    # Every selected tile always renders at every viewport: the
    # band's auto-fit grid wraps the tiles into as many whole
    # rows as the card needs and the page scrolls past them, so
    # the DOM must match summaryVisibleTiles exactly (all 5 in
    # the default selection) and no tile renders narrower than
    # the 160px content-fit floor - no value ever clips. The
    # picker owns what shows: a row flip drops the tile from the
    # band, persists in dash.chart, and a reload restores the
    # selection. No percentile selector, no visible pXX label.
    # Viewports: 1440, 2560 and the 1366x768 laptop (plus the
    # 750px lock floor) lock one screen - the short windows
    # shorten the tile and spark, never the card; 390 stacks
    # and scrolls - all show all 5.
    await page.select_option('#chart-preset', 'overview')
    for viewport in ({'width': 1440, 'height': 1000}, {'width': 2560, 'height': 1440},
                     {'width': 1366, 'height': 768}, {'width': 1366, 'height': 750},
                     {'width': 390, 'height': 844}):
        await page.set_viewport_size(viewport)
        state = await page.evaluate("""async () => {
            const full = window.__fullChart;
            const rlTotal = full.buckets.reduce((s,b)=>s+b.rl,0);
            const errTotal = full.buckets.reduce((s,b)=>s+b.err,0);
            if (rlTotal < 1) throw new Error('fixture produced no rate-limited requests');
            chartAgg = {...full};
            renderChart();
            await new Promise(requestAnimationFrame);
            const card=document.querySelector('.traffic-card');
            const overflow=[...card.querySelectorAll('*')].filter(e=>e.clientWidth && e.scrollWidth>e.clientWidth+2).map(e=>e.id||e.className);
            const errV=document.querySelector('#chart-totals .v-err');
            const rlV=document.querySelector('#chart-totals .v-rl');
            const ttftV=document.querySelector('#chart-totals .v-ttft');
            const tpsV=document.querySelector('#chart-totals .v-tps');
            const reqPair=document.querySelector('#chart-totals .v-req') && document.querySelector('#chart-totals .v-tok');
            const cacheThird=document.querySelector('#chart-totals .v-cache');
            const tokSub=document.querySelector('#chart-totals .chart-total[title^="tokens in/out/cached"] .chart-sub');
            const kept = full.buckets.filter(b => b.req > 0).length;
            const reqPath = document.querySelector('#chart-totals .chart-total[title^="requests / tokens"] svg.spark path');
            const sparkCmds = reqPath ? (reqPath.getAttribute('d').match(/[ML]/g) || []).length : 0;
            const tileList = [...document.querySelectorAll('#chart-totals .chart-total')];
            const cardRect = card.getBoundingClientRect();
            const pairRect = document.querySelector('.grid-pair').getBoundingClientRect();
            const footerRect = document.querySelector('footer').getBoundingClientRect();
            const visible = summaryVisibleTiles(activePreset());
            return {tilesOnly: card.classList.contains('tiles-only'),
                    plot: !!_up, blank: !!document.querySelector('#chart-traffic canvas.chart-blank'),
                    wrapHidden: getComputedStyle(document.getElementById('chart-traffic')).display === 'none',
                    visible,
                    trackFloor: tileList.every(t => t.offsetWidth >= 170),
                    shareLabels: !tokSub || (tokSub.textContent.includes('of tokens') && tokSub.textContent.includes('cache hit') && tokSub.textContent.includes('of input')),
                    overflow, rlTotal, errTotal,
                    uniformTiles: tileList.length >= 1 && tileList.every(t => t.offsetHeight === tileList[0].offsetHeight),
                    noVClip: tileList.every(t => t.scrollHeight <= t.clientHeight + 1),
                    sparkHeights: tileList.map(t => { const s = t.querySelector('svg.spark'); return s ? s.getBoundingClientRect().height : 0; }),
                    sparkPinned: tileList.every(t => { const s = t.querySelector('svg.spark'); return !s || (s.getBoundingClientRect().height >= 13 && t.getBoundingClientRect().bottom - s.getBoundingClientRect().bottom >= 9); }),
                    insideCard: tileList.every(t => t.getBoundingClientRect().bottom <= cardRect.bottom + 1),
                    errors0:errV && errV.textContent === '0',
                    rateLimited2:rlV && rlV.textContent === '2',
                    reqTokensPair:!!reqPair,
                    cacheThird:!!cacheThird && !!document.querySelector('#chart-totals .chart-total[title^="tokens in/out/cached"] .v-cache'),
                    timingPair:!!ttftV && !!tpsV && ttftV.textContent !== '-' && tpsV.textContent !== '-',
                    kept, sparkCmds,
                    tiles:document.querySelectorAll('#chart-totals .chart-total').length,
                    sparks:document.querySelectorAll('#chart-totals svg.spark').length,
                    pctHidden:document.getElementById('chart-pct').hidden,
                    metricsPick:!document.getElementById('chart-metrics').hidden,
                    contextHidden:document.getElementById('chart-context').hidden,
                    cardH: Math.round(cardRect.height),
                    pairH: Math.round(pairRect.height),
                    footerTop: Math.round(footerRect.top),
                    cardSpill: cardRect.height > pairRect.height + 1 ||
                        cardRect.bottom > footerRect.top + 1,
                    legend:document.querySelector('#traffic-legend').textContent};
        }""")
        require(state['tilesOnly'] and not state['plot'] and not state['blank'] and state['wrapHidden'], state)
        require(state['rlTotal'] == 2 and state['errTotal'] == 0, state)
        require(state['reqTokensPair'] and state['cacheThird'], state)
        require(state['shareLabels'], state)
        require(state['contextHidden'], state)
        require(state['tiles'] == len(state['visible']) and state['sparks'] == state['tiles'], state)
        require(state['kept'] < 2 or state['sparkCmds'] == state['kept'], state)
        require(not state['overflow'], state)
        require(state['uniformTiles'] and state['noVClip'], state)
        spark_min = (22 if viewport['height'] == 750 else
                     26 if viewport['width'] == 1366 else 31)
        require(state['sparkHeights'] and
                all(h >= spark_min for h in state['sparkHeights']), state)
        require(state['sparkPinned'] and state['insideCard'] and
                not state['cardSpill'], state)
        require(state['pctHidden'] and state['metricsPick'], state)
        require(state['trackFloor'], state)
        if 'health' in state['visible']:
            require(state['errors0'] and state['rateLimited2'], state)
        if 'timing' in state['visible']:
            require(state['timingPair'], state)
        require(not any(p in state['legend'] for p in ('p50', 'p95', 'p99')), state)
        # Metrics picker contract on the real DOM: the card-head
        # trigger opens the menu, a row flip drops the tile from
        # the band (no stub - the grid reflows), the trigger's
        # count follows, and the choice persists in dash.chart.
        # The count reports the selection itself: every selected
        # tile renders, so it can never disagree with the band.
        toggle = await page.evaluate("""async () => {
            document.getElementById('chart-metrics-btn').click();
            await new Promise(requestAnimationFrame);
            const menu = document.getElementById('chart-metrics-menu');
            const row = [...menu.querySelectorAll('.metrics-row')].find(r => r.dataset.metric === 'tokens');
            row.click();
            await new Promise(requestAnimationFrame);
            const after = document.querySelectorAll('#chart-totals .chart-total').length;
            const want = summaryVisibleTiles(activePreset()).length;
            const saved = JSON.parse(localStorage.getItem('dash.chart') || '{}');
            return {open: !menu.hidden,
                    expanded: document.getElementById('chart-metrics-btn').getAttribute('aria-expanded'),
                    rowUnchecked: row.getAttribute('aria-checked') === 'false',
                    tileGone: !document.querySelector('#chart-totals .chart-total[title^="tokens in/out/cached"]'),
                    after, want,
                    count: document.getElementById('chart-metrics-count').textContent,
                    persisted: (saved.hidden && saved.hidden.overview || []).join() === 'tokens'};
        }""")
        require(toggle['open'] and toggle['expanded'] == 'true' and toggle['rowUnchecked'], toggle)
        require(toggle['tileGone'] and toggle['after'] == toggle['want'] and toggle['count'] == f"{toggle['want']}/5", toggle)
        require(toggle['persisted'], toggle)
        await page.evaluate("""async () => {
            const menu = document.getElementById('chart-metrics-menu');
            const row = [...menu.querySelectorAll('.metrics-row')].find(r => r.dataset.metric === 'tokens');
            row.click();
            await new Promise(requestAnimationFrame);
            document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape'}));
            await new Promise(requestAnimationFrame);
        }""")
        results.append({'width': viewport['width'], 'preset': 'overview', **state})
    # The metrics menu must never be cropped by the window's
    # bottom edge: on a short viewport (the stacked scrolling
    # layout, the card head sits low) opening it flips the menu
    # above its trigger (setMetricsMenuOpen + syncMetricsMenuEdge)
    # so every row stays reachable.
    await page.set_viewport_size({'width': 1280, 'height': 585})
    menu_edge = await page.evaluate("""async () => {
        document.getElementById('chart-metrics-btn').click();
        // The flip is rAF-scheduled (and the viewport switch just
        // queued its own resize rAF): poll frames until the menu
        // settles - within a handful when the mechanism works,
        // never (and the check fails) when it does not.
        let rect = null;
        for (let i = 0; i < 20; i++) {
            await new Promise(requestAnimationFrame);
            rect = document.getElementById('chart-metrics-menu').getBoundingClientRect();
            if (rect.bottom <= innerHeight - 7) break;
        }
        const menu = document.getElementById('chart-metrics-menu');
        const open = !menu.hidden;
        const fits = rect.bottom <= innerHeight - 7;
        const rows = menu.querySelectorAll('.metrics-row').length;
        document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape'}));
        return {open, bottom: Math.round(rect.bottom), innerH: innerHeight, fits, rows};
    }""")
    require(menu_edge['open'] and menu_edge['rows'] == 5 and menu_edge['fits'], menu_edge)
    results.append({'menu_edge': menu_edge})
    # Restore the saved-view expectations the reload check pins.
    await page.select_option('#chart-preset', 'latency')
    await page.select_option('#chart-pct', '99')
    # The layout adapts by viewport size: wide-AND-tall viewports
    # keep the locked one-screen instrument - the ENTIRE page
    # shows without scrolling (inner regions scroll, the page
    # does not) - while any viewport too small to hold it -
    # narrower than 1200px OR shorter than 750px - falls back to
    # the base layout: every section keeps its natural height and
    # the page scrolls. 1366x768 pins the common laptop: it must
    # still lock. 1280x585 is the exact short-tab size that once
    # crammed the whole page into a 147px strip.
    for viewport, want_locked in (({'width': 1440, 'height': 1000}, True),
                                  ({'width': 1706, 'height': 810}, True),
                                  ({'width': 1366, 'height': 768}, True),
                                  ({'width': 1280, 'height': 585}, False),
                                  ({'width': 1024, 'height': 900}, False)):
        await page.set_viewport_size(viewport)
        # Let the locked/unlocked flip settle: the gallery band
        # re-caps through its ResizeObserver within a frame, and
        # measuring inside the transition would read stale boxes.
        await page.evaluate('new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))')
        state = await page.evaluate('''() => {
            const explorer = document.querySelector('#explorer');
            const pair = document.querySelector('.grid-pair');
            const footer = document.querySelector('footer');
            const logCard = document.querySelector('.grid-pair > .card:not(.traffic-card)');
            const er = explorer.getBoundingClientRect();
            const pr = pair.getBoundingClientRect();
            const fr = footer.getBoundingClientRect();
            const lg = logCard.getBoundingClientRect();
            const locked = getComputedStyle(document.documentElement)
                .getPropertyValue('--gallery-locked').trim();
            const explorerMax = parseFloat(getComputedStyle(explorer).maxHeight) || 0;
            const gallery = document.querySelector('#xp-gallery');
            const cards = gallery ? [...gallery.children] : [];
            const rowH = cards.length
                ? Math.max(...cards.filter(c => c.offsetTop === cards[0].offsetTop).map(c => c.offsetHeight))
                : 0;
            const traffic = document.querySelector('.grid-pair .traffic-card');
            const tr = traffic.getBoundingClientRect();
            return {
                locked,
                pageScrollY: document.documentElement.scrollHeight > innerHeight + 1,
                pageScrollX: document.documentElement.scrollWidth > innerWidth + 1,
                explorerH: Math.round(er.height),
                explorerMax,
                // The scrolling-page explorer compresses to ONE
                // whole row of node cards: the band (xp-body)
                // carries the cap, the gallery scrolls internally.
                rowH,
                bandH: gallery ? Math.round(gallery.clientHeight) : 0,
                oneRowBand: !!gallery && gallery.clientHeight <= rowH + 14,
                // Scrolling layouts stack the pair vertically
                // (full-width, one below another); side-by-side
                // exists only in the locked one-screen mode.
                pairStacked: Math.abs(tr.left - lg.left) <= 1 && lg.top >= tr.bottom - 1,
                pairH: Math.round(pr.height),
                footerBottom: Math.round(fr.bottom),
                // Stacked mode: sections run one below another in
                // reading order and the footer sits below the fold.
                order: er.top >= 0 && pr.top >= er.bottom - 1 && fr.top >= pr.bottom - 1,
                inView: er.top >= 0 && pr.top >= 0 && fr.top >= 0
                    && er.bottom <= innerHeight + 1 && pr.bottom <= innerHeight + 1
                    && fr.bottom <= innerHeight + 1,
                logH: Math.round(lg.height),
                // The requests table owns the pair's full column
                // height: the summary card opts out of the stretch
                // (align-self: start), so no summary change may
                // ever borrow the log card's space.
                logFill: Math.abs(lg.height - pr.height) <= 1 && Math.abs(lg.bottom - pr.bottom) <= 1,
            };
        }''')
        require(state['locked'] == ('1' if want_locked else ''), state)
        require(not state['pageScrollX'], state)
        if want_locked:
            require(not state['pageScrollY'], state)
            require(state['explorerMax'] <= 240, state)
            require(state['explorerH'] <= state['explorerMax'] + 1, state)
            require(state['pairH'] > state['explorerH'], state)
            require(state['logFill'], state)
            require(state['inView'], state)
        else:
            # Too small for one screen: the whole UI stacks
            # vertically - the pair included - the page scrolls,
            # and the explorer compresses to a compact ONE-ROW
            # band (the tall dimension rail scrolls inside it,
            # the gallery scrolls its remaining rows).
            require(state['pageScrollY'], state)
            require(state['order'], state)
            require(state['pairStacked'], state)
            require(not state['explorerMax'] or state['explorerMax'] >= 600, state)
            require(state['oneRowBand'], state)
            require(state['explorerH'] <= state['rowH'] + 120, state)
            require(state['logH'] >= 200, state)
        layout_checks.append({'width': viewport['width'], 'height': viewport['height'], **state})
    if screenshot:
        await page.locator('.traffic-card').screenshot(path=screenshot)
    # Request columns are content-sized and monotonic: a column
    # grows to fit its widest value and keeps that width when a
    # shorter row replaces it. The probe saves and restores the
    # real tbody and floors (one id, one owner), then drives the
    # production probe: a long identity must widen the identity
    # column, and swapping the short row back must not shrink it.
    table_shift = []
    for viewport in ({'width': 1440, 'height': 1000}, {'width': 390, 'height': 844}):
        await page.set_viewport_size(viewport)
        state = await page.evaluate("""async () => {
            const box = document.getElementById('tbl-requests');
            const table = box.querySelector('table');
            const body = table.tBodies[0];
            const savedBody = body.innerHTML;
            const savedFloors = REQ_FLOORS.slice();
            const frames = () => new Promise(r =>
                requestAnimationFrame(() => requestAnimationFrame(r)));
            const measure = () => {
                const origin = table.getBoundingClientRect().left;
                return [...table.tHead.rows[0].cells].map(th => {
                    const r = th.getBoundingClientRect();
                    return { left: r.left - origin, width: r.width,
                        minWidth: parseFloat(th.style.minWidth) || 0 };
                });
            };
            // visibleWidth intersects an element rect with every
            // clipping box. A label can report scrollWidth equal to
            // clientWidth while an ancestor still cuts it, so every
            // badge must prove the whole label stays visible.
            const visibleWidth = (el, clips) => {
                if (!el) return 0;
                const r = el.getBoundingClientRect();
                let left = r.left, right = r.right;
                for (const clip of clips) {
                    if (!clip) continue;
                    const c = clip.getBoundingClientRect();
                    left = Math.max(left, c.left);
                    right = Math.min(right, c.right);
                }
                return Math.max(0, right - left);
            };
            const readIdentity = () => {
                const ident = body.querySelector('.req-ident');
                const labels = ident ? [...ident.querySelectorAll('.ent-lb')] : [];
                return {
                    labels: labels.map(label => {
                        const link = label.closest('a.ent-link');
                        return {
                            text: label.textContent.trim(),
                            width: label.getBoundingClientRect().width,
                            clientWidth: label.clientWidth,
                            scrollWidth: label.scrollWidth,
                            visibleWidth: visibleWidth(label, [link]),
                        };
                    }),
                    overflow: ident ? ident.scrollWidth > ident.clientWidth + 1 : true,
                };
            };
            const base = {
                id: 'colshift', start: new Date().toISOString(),
                stream: true, status_code: 200, ttft_ms: 12, gen_tps: 42,
                duration_ms: 34, finish_reason: 'stop', tool_calls: 1,
                cost: 0.001, queue_wait_ms: 0,
                usage: { input_tokens: 10, output_tokens: 5, cache_read_tokens: 0 },
                attempts: [],
            };
            const shortRec = { ...base,
                client: 'opencode/1.18.32', provider: 'narrow-provider',
                model: 'Neutral-Model-With-A-Very-Long-Suffix' };
            const longRec = { ...base,
                client: 'openai/enterprise-client-with-a-very-long-product-suffix',
                provider: 'provider-with-a-very-long-tenant-and-region-label',
                model: 'Neutral-Model-With-A-Very-Long-Suffix' };
            let result;
            try {
                modelNames.set('Neutral-Model-With-A-Very-Long-Suffix',
                    'neutral-model-with-a-very-long-suffix');
                const shortHtml = reqRow(shortRec, null);
                reqMeasureRows([shortHtml]);
                applyReqFloors(box);
                body.innerHTML = shortHtml;
                await frames();
                const shortCols = measure();
                const shortIdentity = readIdentity();
                const shortCell = body.querySelector('tr.exp-row td:nth-child(2)');
                const shortTitle = shortCell ? shortCell.getAttribute('title') : '';
                const modelLabel = body.querySelector('.req-model .ent-lb');
                const modelLabelTitle = modelLabel ? modelLabel.getAttribute('title') : '';
                const modelClosestTitle = modelLabel && modelLabel.closest('[title]')
                    ? modelLabel.closest('[title]').getAttribute('title') : '';
                const clientLink = body.querySelector('.req-ident a.ent-link[data-client]');
                const clientEnt = clientLink ? clientLink.querySelector('.ent') : null;
                const clientTitle = clientLink ? clientLink.getAttribute('title') : null;
                const clientEntTitle = clientEnt ? clientEnt.getAttribute('title') : null;
                const modelLink = body.querySelector('.req-model a.ent-link');
                if (modelLink) modelLink.focus();
                const modelOutline = modelLink ? getComputedStyle(modelLink) : null;
                const modelFocus = modelOutline ? {
                    style: modelOutline.outlineStyle,
                    width: parseFloat(modelOutline.outlineWidth),
                    color: modelOutline.outlineColor,
                    offset: modelOutline.outlineOffset,
                } : {};
                if (modelLink) modelLink.blur();
                const shortFloor = REQ_FLOORS[1];
                // A wider identity must grow the column through the
                // same probe the live path uses.
                const longHtml = reqRow(longRec, null);
                body.innerHTML = longHtml;
                await frames();
                const longCols = measure();
                const longIdentity = readIdentity();
                const longCell = body.querySelector('tr.exp-row td:nth-child(2)');
                const longTitle = longCell ? longCell.getAttribute('title') : '';
                reqMeasureRows([longHtml]);
                applyReqFloors(box);
                const longFloor = REQ_FLOORS[1];
                // Back to the short row: the learned floor must
                // hold the wider column, never shrink it.
                body.innerHTML = shortHtml;
                await frames();
                const heldCols = measure();
                const heldIdentity = readIdentity();
                const savedScroll = box.scrollLeft;
                box.scrollLeft = 40;
                const movedScroll = box.scrollLeft;
                box.scrollLeft = savedScroll;
                result = {
                    reqCols: REQ_COLS,
                    headerColumns: shortCols.length,
                    bodyColumns: body.querySelector('tr.exp-row')
                        ? body.querySelector('tr.exp-row').cells.length : 0,
                    layout: getComputedStyle(table).tableLayout,
                    colgroups: table.querySelectorAll('colgroup').length,
                    floorCount: shortCols.filter(col => col.minWidth > 0).length,
                    shortIdentityWidth: shortCols[1].width,
                    longIdentityWidth: longCols[1].width,
                    heldIdentityWidth: heldCols[1].width,
                    shortFloor, longFloor,
                    shortIdentity, longIdentity, heldIdentity,
                    shortTitle, longTitle,
                    expectedShortTitle: reqIdentityText(shortRec),
                    expectedLongTitle: reqIdentityText(longRec),
                    modelLabelTitle, modelClosestTitle,
                    clientTitle, clientEntTitle, modelFocus,
                    scrollBox: {
                        overflowX: getComputedStyle(box).overflowX,
                        moved: movedScroll,
                        saved: savedScroll,
                    },
                    mobileScroll: box.scrollWidth > box.clientWidth + 1,
                    pageOverflow: document.documentElement.scrollWidth > innerWidth + 1,
                };
                // A retry message is retained up to 512 characters.
                // In auto layout its nowrap colspan cell must not
                // become the table's widest content: opening the
                // sub-row must leave every column where it was, and
                // the message must ellipsize inside its cell.
                const retryRec = { ...base, id: 'retry-width-probe',
                    retries: 1,
                    attempts: [{ at: base.start, status_code: 429,
                        error_type: 'rate_limit',
                        error_msg: 'upstream error: ' + 'x'.repeat(512) }] };
                const retryHtml = reqRow(retryRec, null);
                body.innerHTML = retryHtml;
                await frames();
                const retryBefore = {
                    table: table.getBoundingClientRect().width,
                    cols: measure().map(col => col.width),
                };
                const retrySub = body.querySelector('tr.retry-sub');
                retrySub.hidden = false;
                await frames();
                const retryMsg = retrySub.querySelector('.retry-sub-msg');
                const retryText = retrySub.querySelector('.retry-sub-text');
                const retryAfter = {
                    table: table.getBoundingClientRect().width,
                    cols: measure().map(col => col.width),
                    messageWidth: retryText ? retryText.getBoundingClientRect().width : 0,
                    messageScrollWidth: retryText ? retryText.scrollWidth : 0,
                    title: retryMsg ? retryMsg.getAttribute('title') : '',
                };
                retrySub.hidden = true;
                await frames();
                result.retrySub = {
                    before: retryBefore, after: retryAfter,
                    closedTable: table.getBoundingClientRect().width,
                };
            } finally {
                modelNames.delete('Neutral-Model-With-A-Very-Long-Suffix');
                body.innerHTML = savedBody;
                REQ_FLOORS.splice(0, REQ_FLOORS.length, ...savedFloors);
                applyReqFloors(box);
            }
            return result;
        }""")
        require(state['reqCols'] == 14 and state['headerColumns'] == 14 and
                state['bodyColumns'] == 14, state)
        require(state['layout'] == 'auto' and state['colgroups'] == 0, state)
        require(state['floorCount'] == 14, state)
        require(state['longIdentityWidth'] > state['shortIdentityWidth'] + 1, state)
        require(state['longFloor'] > state['shortFloor'], state)
        require(state['heldIdentityWidth'] >= state['longFloor'] - 0.5, state)
        for ident in (state['shortIdentity'], state['longIdentity'],
                      state['heldIdentity']):
            require(not ident['overflow'], state)
            require(len(ident['labels']) == 3, state)
            require(all(label['scrollWidth'] <= label['clientWidth'] + 1 and
                        label['visibleWidth'] >= label['width'] - 1
                        for label in ident['labels']), state)
        require(state['shortTitle'] == state['expectedShortTitle'] and
                'opencode/1.18.32' in state['shortTitle'] and
                'narrow-provider' in state['shortTitle'], state)
        require(state['longTitle'] == state['expectedLongTitle'] and
                'provider-with-a-very-long-tenant-and-region-label' in
                state['longTitle'], state)
        require(state['modelLabelTitle'].startswith(
                    'original: Neutral-Model-With-A-Very-Long-Suffix'), state)
        require('original: Neutral-Model-With-A-Very-Long-Suffix' in
                state['modelClosestTitle'], state)
        require(state['clientTitle'] == '' and state['clientEntTitle'] == '',
                state)
        focus = state['modelFocus']
        require(focus.get('style') == 'solid' and focus.get('width', 0) >= 1 and
                focus.get('color') not in ('', 'rgba(0, 0, 0, 0)', 'transparent') and
                focus.get('offset') == '-1px', state)
        require(state['scrollBox']['overflowX'] in ('auto', 'scroll'), state)
        require(not state['pageOverflow'], state)
        if viewport['width'] == 390:
            require(state['mobileScroll'], state)
            require(state['scrollBox']['moved'] > 0, state)
        retry = state['retrySub']
        require(retry['after']['table'] <= retry['before']['table'] + 0.5,
                retry)
        require(retry['closedTable'] <= retry['before']['table'] + 0.5,
                retry)
        require(len(retry['after']['cols']) == len(retry['before']['cols']) and
                all(abs(after - before) <= 0.5 for after, before in
                    zip(retry['after']['cols'], retry['before']['cols'])),
                retry)
        require(retry['after']['messageScrollWidth'] >
                retry['after']['messageWidth'] + 1, retry)
        require(len(retry['after']['title']) >= 512, retry)
        table_shift.append({'width': viewport['width'], **state})
    results.append({'table_shift': table_shift})

    # Real formatter outputs must fit their content-sized columns,
    # or the cell must carry the full value in its title. The probe
    # renders through the production reqRow and restores the real
    # tbody.
    await page.set_viewport_size({'width': 1440, 'height': 1000})
    content_fit = await page.evaluate("""async () => {
        const box = document.getElementById('tbl-requests');
        const table = box.querySelector('table');
        const body = table.tBodies[0];
        const saved = body.innerHTML;
        const frames = () => new Promise(r =>
            requestAnimationFrame(() => requestAnimationFrame(r)));
        const at = new Date(2020, 0, 1, 12, 34, 56);
        const base = {
            id: 'fit-probe', start: at.toISOString(), end: at.toISOString(),
            provider: 'neutral-provider', model: 'Neutral-Model',
            client: 'neutral-client', stream: true, status_code: 200,
            ttft_ms: 123, duration_ms: 456, finish_reason: 'content_filter',
            tool_calls: 3, turns_user: 2, turns_assistant: 1,
            last_turn_role: 'assistant', queue_wait_ms: 0,
            usage: {input_tokens: 999990000, output_tokens: 999990000,
                    cache_read_tokens: 999990000},
        };
        const long = {...base, ttft_ms: 987654321, duration_ms: 987654321,
            tool_calls: 123456, turns_user: 123456,
            turns_assistant: 123456, turns_tool: 123456,
            last_turn_role: 'assistant', queue_wait_ms: 987654321,
            images: 123, attachments: 45};
        // Every displayed text cell, including the queued column.
        const probes = [0, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12];
        const rows = [];
        try {
            for (const [fixture, isLong] of [
                [{...base, cost: 0.12346, gen_tps: 99999.9}, false],
                [{...base, cost: 999.999, gen_tps: 99999.9}, false],
                [{...long, cost: 0.5, gen_tps: 99999.9}, true],
            ]) {
                body.innerHTML = reqRow(fixture, null);
                await frames();
                const cells = [...body.querySelector('tr.exp-row').cells];
                const read = index => ({
                    text: cells[index].textContent.trim(),
                    title: cells[index].getAttribute('title') || '',
                    fits: cells[index].scrollWidth <= cells[index].clientWidth + 1,
                });
                rows.push({
                    cost: fixture.cost, long: isLong,
                    time: read(0), ttft: read(3), tps: read(4),
                    inOut: read(5), cache: read(6), tools: read(7),
                    finish: read(8), dur: read(9), turns: read(10),
                    queued: read(11), costCell: read(12),
                    cellsFitOrTitled: probes.every(index =>
                        cells[index].scrollWidth <= cells[index].clientWidth + 1 ||
                        !!cells[index].getAttribute('title')),
                    expectedTime: at.toLocaleTimeString(),
                });
            }
        } finally {
            if (table.isConnected && table.tBodies[0] === body) body.innerHTML = saved;
        }
        return rows;
    }""")
    for row in content_fit:
        require(row['cellsFitOrTitled'], row)
        require(row['time']['text'] == row['expectedTime'] and
                '12:34:56' in row['time']['text'], row)
        require(row['ttft']['text'] == row['ttft']['title'], row)
        require(row['tps']['text'] == '99999.9' and
                row['tps']['title'].startswith('99999.9'), row)
        require(row['inOut']['text'] == '999.99M/999.99M', row)
        require(row['cache']['text'] == '100.00%' and
                '100.00%' in row['cache']['title'], row)
        require(row['tools']['text'] == ('123456' if row['long'] else '3') and
                'tool calls' in row['tools']['title'], row)
        require(row['finish']['text'] == 'content_filter' and
                row['finish']['title'] == 'content_filter', row)
        require(row['dur']['text'] == row['dur']['title'], row)
        require(row['turns']['title'] and
                ('123456 user' in row['turns']['title'] if row['long']
                 else '2 user' in row['turns']['title']), row)
        if row['long']:
            # The queued value uses its full fmtDur text; the title
            # repeats that same value for assistive technology.
            require(row['queued']['text'] == row['queued']['title'] and
                    row['queued']['title'].endswith('d'), row)
        else:
            require(row['queued']['text'] == '-' and
                    row['queued']['title'] == '', row)
        expected_cost = ('12.346¢' if row['cost'] == 0.12346 else
                         '50¢' if row['cost'] == 0.5 else '$999.999')
        require(row['costCell']['text'] == expected_cost and
                row['costCell']['title'].endswith(expected_cost), row)
    results.append({'content_fit': content_fit})


async def focus_checks(page, results):
    """Keep keyboard focus and both scroll axes across a live row update."""
    # Keyboard focus and both scroll axes must survive a live row's
    # incremental replacement with its final values.
    focus_scroll = await page.evaluate("""async () => {
        const box = document.getElementById('tbl-requests');
        const frames = () => new Promise(r =>
            requestAnimationFrame(() => requestAnimationFrame(r)));
        const baseStart = new Date(2026, 0, 2, 12, 0, 0);
        const recs = Array.from({length: 60}, (_, i) => ({
            id: 'keep-' + i,
            start: new Date(baseStart.getTime() - i * 60000).toISOString(),
            end: new Date(baseStart.getTime() - i * 60000 + 500).toISOString(),
            provider: 'p', model: 'm', client: 'c', stream: true,
            status_code: 200, ttft_ms: 100, duration_ms: 500,
            usage: {input_tokens: 10, output_tokens: 5, total_tokens: 15},
            retries: 0, attempts: [],
        }));
        recs[30] = {...recs[30], live: true, status_code: 0, ttft_ms: 0,
                    gen_tps: 0, duration_ms: 0, queue_wait_ms: 0, cost: 0};
        try {
            renderRequests(recs);
            await frames();
            const target = box.querySelector(
                'tr.exp-row[data-id="keep-30"] button[data-open-req]');
            target.focus();
            box.scrollTop = 120;
            box.scrollLeft = 40;
            const overflow = {
                vertical: box.scrollHeight > box.clientHeight + 1,
                horizontal: box.scrollWidth > box.clientWidth + 1,
            };
            const before = {top: box.scrollTop, left: box.scrollLeft,
                focused: document.activeElement === target};
            const next = recs.map(rec => rec.id === 'keep-30'
                ? {...rec, live: false, status_code: 200, ttft_ms: 100,
                   duration_ms: 500, gen_tps: 12.5, cost: 0.01}
                : rec);
            renderRequestsIncremental(next);
            await frames();
            const active = document.activeElement;
            return {overflow, before, after: {
                top: box.scrollTop, left: box.scrollLeft,
                focusId: active?.closest?.('tr.exp-row[data-id]')?.dataset.id || '',
                focusAction: !!active?.hasAttribute?.('data-open-req'),
                focusButton: active?.tagName === 'BUTTON',
            }};
        } finally {
            renderRequests(derive(lastData).scopedRecs);
            await frames();
        }
    }""")
    require(focus_scroll['before']['focused'], focus_scroll)
    require(focus_scroll['overflow']['vertical'] and
            focus_scroll['overflow']['horizontal'], focus_scroll)
    require(focus_scroll['before']['top'] > 0 and
            focus_scroll['before']['left'] > 0, focus_scroll)
    require(focus_scroll['after']['focusId'] == 'keep-30' and
            focus_scroll['after']['focusAction'] and
            focus_scroll['after']['focusButton'], focus_scroll)
    require(abs(focus_scroll['after']['top'] - focus_scroll['before']['top']) <= 1 and
            focus_scroll['after']['left'] == focus_scroll['before']['left'],
            focus_scroll)
    results.append({'focus_scroll': focus_scroll})

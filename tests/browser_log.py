"""Incremental request-table width, day geometry and retry hover checks."""

from .support import require


async def log_checks(page, results):
    """Drive the incremental width, sticky day and retry hover probes."""
    # The incremental path must learn a wider identity when a live
    # row changes or a new live row arrives, then hold that floor
    # when the short window returns. This drives production
    # renderRequestsIncremental and the full reqThead rebuild, not
    # the measurement helper directly: deleting the probe call or
    # dropping the header floors leaves the long row visually wider
    # but lets the revert shrink the column.
    incremental_width = await page.evaluate("""async () => {
        const box = document.getElementById('tbl-requests');
        const frames = () => new Promise(r =>
            requestAnimationFrame(() => requestAnimationFrame(r)));
        const savedFloors = REQ_FLOORS.slice();
        const at = new Date(2026, 0, 2, 12, 0, 0).toISOString();
        const base = {start: at, end: at, provider: 'p', model: 'm',
            client: 'c', stream: true, status_code: 0, live: true,
            ttft_ms: 0, duration_ms: 0, queue_wait_ms: 0, cost: 0,
            usage: {input_tokens: 0, output_tokens: 0},
            retries: 0, attempts: []};
        const recs = Array.from({length: 6}, (_, i) => ({...base,
            id: 'iw-' + i,
            start: new Date(new Date(at).getTime() - i * 60000).toISOString()}));
        const long = {client: 'openai/enterprise-client-with-a-very-long-product-suffix',
            provider: 'provider-with-a-very-long-tenant-and-region-label',
            model: 'neutral-model-with-a-very-long-suffix'};
        const headerFloor = () => parseFloat(
            box.querySelector('thead th:nth-child(2)')?.style.minWidth) || 0;
        const identity = id => {
            const row = box.querySelector('tr.exp-row[data-id="' + id + '"]');
            return {width: row?.cells[1]?.getBoundingClientRect().width || 0,
                    floor: REQ_FLOORS[1], headerFloor: headerFloor(),
                    text: row?.cells[1]?.textContent.trim() || ''};
        };
        try {
            REQ_FLOORS.fill(0);
            renderRequests(recs);
            await frames();
            const short = identity('iw-0');
            const longRecs = recs.map(r => r.id === 'iw-0'
                ? {...r, ...long} : r);
            renderRequestsIncremental(longRecs);
            await frames();
            const replaced = identity('iw-0');
            renderRequestsIncremental(recs);
            await frames();
            const heldReplace = identity('iw-0');

            // Insert path: zero the floors, paint the short window,
            // then add one new live row with the long identity.
            REQ_FLOORS.fill(0);
            renderRequests(recs);
            await frames();
            const insertBase = identity('iw-0');
            renderRequestsIncremental([...recs, {...base, ...long,
                id: 'iw-new'}]);
            await frames();
            const inserted = identity('iw-new');
            renderRequestsIncremental(recs);
            await frames();
            const heldInsert = identity('iw-0');
            return {short, insertBase, replaced, inserted,
                    heldReplace, heldInsert};
        } finally {
            REQ_FLOORS.splice(0, REQ_FLOORS.length, ...savedFloors);
            renderRequests(derive(lastData).scopedRecs);
            await frames();
        }
    }""")
    for before, after in (('short', 'replaced'), ('insertBase', 'inserted')):
        require(incremental_width[after]['floor'] >
                incremental_width[before]['floor'] + 1, incremental_width)
        require(incremental_width[after]['width'] >
                incremental_width[before]['width'] + 1, incremental_width)
        require(abs(incremental_width[after]['headerFloor'] -
                    incremental_width[after]['floor']) <= 0.01,
                incremental_width)
    for held, prior in (('heldReplace', 'replaced'),
                        ('heldInsert', 'inserted')):
        require(incremental_width[held]['floor'] ==
                incremental_width[prior]['floor'], incremental_width)
        require(incremental_width[held]['width'] >=
                incremental_width[prior]['floor'] - 0.5, incremental_width)
        require(abs(incremental_width[held]['headerFloor'] -
                    incremental_width[held]['floor']) <= 0.01,
                incremental_width)
    require(abs(incremental_width['short']['headerFloor'] -
                incremental_width['short']['floor']) <= 0.01,
            incremental_width)
    results.append({'incremental_width': incremental_width})

    # The sticky day divider must sit exactly on the sticky header's
    # bottom edge: no sliver of scrolling rows between them, and no
    # overlap.
    day_geometry = await page.evaluate("""async () => {
        const box = document.getElementById('tbl-requests');
        const frames = () => new Promise(r =>
            requestAnimationFrame(() => requestAnimationFrame(r)));
        const latest = new Date(2026, 0, 2, 12, 0, 0);
        const earlier = new Date(2026, 0, 1, 12, 0, 0);
        const recs = Array.from({length: 40}, (_, i) => {
            const at = i < 20 ? latest : earlier;
            return {
                id: 'day-' + i,
                start: new Date(at.getTime() - (i % 20) * 60000).toISOString(),
                end: new Date(at.getTime() - (i % 20) * 60000 + 500).toISOString(),
                provider: 'p', model: 'm', client: 'c', stream: true,
                status_code: 200, ttft_ms: 100, duration_ms: 500,
                usage: {input_tokens: 10, output_tokens: 5, total_tokens: 15},
                retries: 0, attempts: [],
            };
        });
        try {
            box.scrollIntoView({block: 'center'});
            await frames();
            renderRequests(recs);
            await frames();
            box.scrollTop = box.scrollHeight;
            await frames();
            const headCells = [...box.querySelectorAll('thead th')]
                .map(th => th.getBoundingClientRect());
            const headTop = Math.min(...headCells.map(rect => rect.top));
            const headBottom = Math.max(...headCells.map(rect => rect.bottom));
            const boxRect = box.getBoundingClientRect();
            const boxBottom = boxRect.bottom;
            const separators = [...box.querySelectorAll('tr.day-sep td')]
                .map(td => td.getBoundingClientRect())
                .filter(rect => rect.bottom > headBottom - 1 && rect.top < boxBottom);
            const overflowY = box.scrollHeight > box.clientHeight + 1;
            if (!separators.length) return {separators: 0, headBottom,
                boxTop: boxRect.top, headTop, headHeight: headBottom - headTop,
                scrollTop: box.scrollTop, overflowY, pageScrollY: scrollY};
            const pinned = separators.reduce((a, b) => a.top < b.top ? a : b);
            return {separators: separators.length, headBottom,
                boxTop: boxRect.top, headTop, headHeight: headBottom - headTop,
                scrollTop: box.scrollTop, overflowY, pageScrollY: scrollY,
                separatorTop: pinned.top, gap: pinned.top - headBottom};
        } finally {
            renderRequests(derive(lastData).scopedRecs);
            await frames();
        }
    }""")
    require(day_geometry['separators'] >= 1, day_geometry)
    require(day_geometry['overflowY'] and day_geometry['scrollTop'] > 0,
            day_geometry)
    require(abs(day_geometry['gap']) <= 0.5, day_geometry)
    results.append({'day_geometry': day_geometry})

    # A hovered retry sub-row lifts its muted cells to the surface
    # text token; the warn tint stays the row's state cue.
    await page.set_viewport_size({'width': 1440, 'height': 1000})
    retry_injected = await page.evaluate("""() => {
        const body = document.querySelector('#tbl-requests tbody');
        const rec = { id: 'retry-hover-fixture', provider: 'p', model: 'm',
            client: 'c', key_hash: 'k-retry', conversation_id: 'cv',
            status_code: 200, start: new Date().toISOString(),
            end: new Date().toISOString(), stream: true, time_bucket: 'work',
            ttft_ms: 100, duration_ms: 500, usage: {},
            retries: 1, attempts: [{ status_code: 502,
                error_type: 'bad_gateway', error_msg: 'upstream overloaded',
                at: new Date().toISOString() }] };
        const template = document.createElement('template');
        template.innerHTML = reqRow(rec, null);
        const rows = [...template.content.querySelectorAll('tr')];
        rows.forEach(row => { row.hidden = false; body.append(row); });
        return rows.length;
    }""")
    require(retry_injected == 2, retry_injected)
    try:
        await page.hover('tr.retry-sub[data-retry-of="retry-hover-fixture"] td')
        # The shared color transition must finish before reading the
        # hovered computed values.
        await page.wait_for_timeout(250)
        retry_hover = await page.evaluate("""() => {
            const cell = document.querySelector(
                'tr.retry-sub[data-retry-of="retry-hover-fixture"] td');
            const probe = document.createElement('span');
            probe.style.color = 'var(--text)';
            document.body.appendChild(probe);
            const result = {
                color: getComputedStyle(cell).color,
                expected: getComputedStyle(probe).color,
                background: getComputedStyle(cell).backgroundColor,
            };
            probe.remove();
            return result;
        }""")
        require(retry_hover['color'] == retry_hover['expected'], retry_hover)
        require(retry_hover['background'] != 'rgba(0, 0, 0, 0)', retry_hover)
    finally:
        # The probe rows are not part of lastData, so a failed
        # assertion must not leave them behind for later checks.
        await page.evaluate("""() => document.querySelectorAll(
            'tr[data-id="retry-hover-fixture"], tr[data-retry-of="retry-hover-fixture"]')
            .forEach(row => row.remove())""")

    # The In-flight tile must never clip its queued summary at the narrowest
    # track. Its two ends stay short (active / queued) while the composition
    # rail carries the exact per-state split and its hover title.
    await page.set_viewport_size({'width': 1440, 'height': 1000})
    flight_mix = await page.evaluate("""async () => {
        const frames = () => new Promise(r =>
            requestAnimationFrame(() => requestAnimationFrame(r)));
        const host = document.getElementById('kpis');
        const savedHtml = host.innerHTML;
        const savedKpi = _kpiHtml, savedKey = _kpiHtmlKey;
        const live = [
            { id: 'kpi-flight-1', live: true, stream: true },
            { id: 'kpi-flight-2', live: true, stream: true },
            { id: 'kpi-flight-3', live: true, stream: true, paused: true },
            { id: 'kpi-flight-4', live: true, throttled: true },
        ];
        try {
            host.innerHTML = derive({ records: live, counters: { in_flight: 4 },
                _rev: 424242 }).kpiHtml;
            await frames();
            const tile = [...host.querySelectorAll('.kpi')]
                .find(k => (k.querySelector('h2') || {}).textContent === 'In-flight');
            const sub = tile.querySelector('.flight-sub');
            const spans = [...sub.querySelectorAll('span')].filter(s => s.textContent);
            const rail = tile.querySelector('.flight-mix');
            return {
                texts: spans.map(s => s.textContent),
                clipped: spans.filter(s => s.scrollWidth > s.clientWidth + 1)
                    .map(s => s.textContent),
                segments: [...rail.querySelectorAll('.fm-seg')].map(e => e.className),
                title: rail.getAttribute('title'),
                railWidth: rail.getBoundingClientRect().width,
                subWidth: sub.getBoundingClientRect().width,
            };
        } finally {
            host.innerHTML = savedHtml;
            _kpiHtml = savedKpi;
            _kpiHtmlKey = savedKey;
        }
    }""")
    require(flight_mix['texts'] == ['2 active', '2 queued'], flight_mix)
    require(not flight_mix['clipped'], flight_mix)
    require(flight_mix['segments'] == ['fm-seg fm-streaming', 'fm-seg fm-paused',
                                       'fm-seg fm-throttled'], flight_mix)
    require(flight_mix['title'] == '2 streaming · 1 paused · 1 throttled', flight_mix)
    require(abs(flight_mix['railWidth'] - flight_mix['subWidth']) <= 1, flight_mix)
    results.append({'flight_mix': flight_mix})

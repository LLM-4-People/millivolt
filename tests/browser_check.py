#!/usr/bin/env python3
"""Real-canvas dashboard regression against an existing isolated dev instance.

Requires Python Playwright + Chromium. Does not start, stop, or restart a proxy.
All upstream traffic and the scoped fixture cleanup stay on loopback. Browser
resources reject redirects; SSE is disabled so this checks bootstrap/poll charts.
"""

import argparse
import asyncio
import json
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

from playwright.async_api import async_playwright

from .support import operator_headers, operator_signin, require

DEV_DIR = Path('/tmp/millivolt')  # Reserved dev.sh namespace, never arbitrary data.
# Two content deltas with a flushed gap between them: the first delta stamps
# first_token_at, the second stamps last_token_at, so every fixture request
# contributes a real decode-window sample to the chart. A single-chunk stream
# has first == last and no decode window at all.
BODY_HEAD = b'data: {"choices":[{"delta":{"content":"fixture"},"finish_reason":null}]}\n\n'
BODY_TAIL = (
    b'data: {"choices":[{"delta":{"content":"."},"finish_reason":null}]}\n\n'
    b'data: {"choices":[{"delta":{},"finish_reason":"stop"}],'
    b'"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.001}}\n\n'
    b'data: [DONE]\n\n'
)
# The composed fixture the response-integrity checks compare against.
BODY = BODY_HEAD + BODY_TAIL
# A provider-style 429 body for throttle-marked fixture requests: the upstream
# alternates 429→200 per marked body, so each one recovers on its first retry
# (an absorbed attempt). That exercises the chart's rate-limit fold with the
# health invariant: rate-limited requests are distinct from errors.
BODY_429 = b'{"error":{"message":"rate limited","type":"rate_limit_error","code":429}}'


class Upstream(BaseHTTPRequestHandler):
    _throttle_hits = {}

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        if b'throttle' in body:
            hits = Upstream._throttle_hits[body] = Upstream._throttle_hits.get(body, 0) + 1
            if hits % 2 == 1:
                self.send_response(429)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(BODY_429)))
                self.end_headers()
                self.wfile.write(BODY_429)
                return
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Content-Length', str(len(BODY_HEAD) + len(BODY_TAIL)))
        self.end_headers()
        # Fixture delay guarantees a measurable positive TTFT on fast hosts;
        # the flushed split write guarantees a measurable decode window too.
        time.sleep(0.02)
        self.wfile.write(BODY_HEAD)
        self.wfile.flush()
        time.sleep(0.05)
        self.wfile.write(BODY_TAIL)

    def log_message(self, *args):
        pass


def target(value):
    try:
        url = urlparse(value)
        port = url.port
    except ValueError as error:
        raise argparse.ArgumentTypeError('invalid dev target URL: ' + str(error)) from error
    if (url.scheme != 'http' or url.hostname != '127.0.0.1' or port is None
            or not 1024 <= port <= 65535 or port == 8080
            or url.username is not None or url.password is not None or url.path not in ('', '/')
            or url.query or url.fragment):
        raise argparse.ArgumentTypeError('use an explicit http://127.0.0.1:dev-port URL, never port 8080')
    return value.rstrip('/')


def private_config(doc, base):
    expected = DEV_DIR / f'millivolt-dev-{urlparse(base).port}.yaml'
    if doc.get('path') != str(expected) or expected.resolve() != expected:
        raise RuntimeError('target is not the private dev configuration')
    override = doc.get('overrides', {}).get('db_path')
    effective = doc.get('effective', {}).get('db_path')
    if 'db_path' in doc.get('restart_required', []):
        raise RuntimeError('target has an ambiguous pending database restart')
    if not isinstance(override, str) or not override:
        raise RuntimeError('target requires the explicit dev.sh database override')
    if override == 'none':
        if effective != '':
            raise RuntimeError('disabled database override does not match effective state')
        return
    if effective != override:
        raise RuntimeError('database override does not match effective state')
    raw = Path(override)
    resolved = raw.resolve()
    if (not raw.is_absolute() or resolved.parent != DEV_DIR
            or not resolved.name.startswith('millivolt-dev') or not resolved.name.endswith('.db')):
        raise RuntimeError('target database is outside the private dev namespace')


async def browser_route(route, base, errors):
    requested = urlparse(route.request.url)
    accepted = urlparse(base)
    if (requested.scheme, requested.hostname, requested.port) != (accepted.scheme, accepted.hostname, accepted.port):
        errors.append('browser attempted a foreign origin: ' + route.request.url)
        await route.abort()
        return
    if requested.path == '/metrics/live/stream':
        # Buffered browser fixtures use the real bootstrap and polling paths.
        # route.fetch buffers bodies, so an endless SSE response is excluded.
        await route.abort()
        return
    try:
        response = await route.fetch(max_redirects=0)
        try:
            if 300 <= response.status < 400:
                errors.append('browser resource redirected: ' + route.request.url)
                await route.abort()
            else:
                await route.fulfill(response=response)
        finally:
            await response.dispose()
    except Exception as error:
        errors.append('browser resource failed: ' + str(error))
        await route.abort()


async def check(base, screenshot):
    label = 'browser-fixture-' + uuid.uuid4().hex
    upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    try:
        async with async_playwright() as playwright:
            browser = await playwright.chromium.launch()
            cleanup_needed = False
            page = None
            try:
                context = await browser.new_context(viewport={'width': 1440, 'height': 1000}, service_workers='block')
                errors = []
                await context.route('**/*', lambda route: browser_route(route, base, errors))
                # Sign in before the first page load: the context's cookie jar
                # authenticates the dashboard, its fetches and its EventSource.
                await operator_signin(context, base)
                page = await context.new_page()
                page.on('pageerror', lambda error: errors.append(str(error)))
                config_response = await page.request.get(base + '/admin/config', max_redirects=0)
                if config_response.status != 200:
                    raise RuntimeError('configuration preflight failed: ' + str(config_response.status))
                private_config(await config_response.json(), base)
                # Set before the first attempt: a failed response can still have
                # recorded a finalized request that this run must clean up.
                cleanup_needed = True
                for _ in range(8):
                    response = await page.request.post(base + '/v1/chat/completions', headers={
                        'X-Proxy-Base-URL': f'http://127.0.0.1:{upstream.server_port}',
                        'X-Proxy-Key': 'local-fixture-only', 'X-Proxy-Client': label,
                    }, max_redirects=0, data={'model': 'fixture-model', 'stream': True,
                             'messages': [{'role': 'user', 'content': 'fixture'}]})
                    require(response.ok and await response.body() == BODY, 'fixture response changed')
                for i in range(2):
                    # Recovered 429s: absorbed retry attempts, final 200 - rate
                    # limited but never errors (the chart folds them into rl).
                    response = await page.request.post(base + '/v1/chat/completions', headers={
                        'X-Proxy-Base-URL': f'http://127.0.0.1:{upstream.server_port}',
                        'X-Proxy-Key': 'local-fixture-only', 'X-Proxy-Client': label,
                    }, max_redirects=0, data={'model': 'fixture-model', 'stream': True,
                             'messages': [{'role': 'user', 'content': 'throttle ' + str(i)}]})
                    require(response.ok and await response.body() == BODY, 'throttle fixture recovery changed')
                await page.goto(base + '/#client=' + label, wait_until='domcontentloaded')
                await page.wait_for_function('chartAgg && explorerAgg && lastData')
                # Stash the untouched full chart payload: later canvas checks
                # rewrite chartAgg with trimmed buckets, but the Overview
                # period-total assertions need every request-bearing bucket.
                await page.evaluate('window.__fullChart = chartAgg')
                await page.select_option('#chart-preset', 'latency')
                await page.wait_for_function('chartAgg && chartAgg.tps_p.some(v => v != null)')
                results = []
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
                layout_checks = []
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

                # The published docs capture uses 2200x900 so the request
                # table's learned content widths fit without horizontal
                # clipping. The table is content-sized, not a fixed budget.
                await page.set_viewport_size({'width': 2200, 'height': 900})
                await page.evaluate(
                    'new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))')
                docs_table = await page.evaluate("""() => {
                    const box = document.getElementById('tbl-requests');
                    const table = box.querySelector('table');
                    return {
                        clientWidth: box.clientWidth,
                        scrollWidth: box.scrollWidth,
                        tableWidth: table ? Math.round(table.getBoundingClientRect().width) : 0,
                        layout: table ? getComputedStyle(table).tableLayout : '',
                    };
                }""")
                require(docs_table['layout'] == 'auto' and
                        docs_table['scrollWidth'] <= docs_table['clientWidth'] + 2, docs_table)
                results.append({'docs_table': docs_table})
                await page.set_viewport_size({'width': 1440, 'height': 1000})

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

                # The incremental path must learn a wider identity when a live
                # row changes, then hold that floor when the short record
                # returns. This drives production renderRequestsIncremental,
                # not the measurement helper directly: deleting its probe call
                # leaves the long row visually wider but lets the revert shrink
                # the column.
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
                    const identity = () => {
                        const row = box.querySelector('tr.exp-row[data-id="iw-0"]');
                        return {width: row?.cells[1]?.getBoundingClientRect().width || 0,
                                floor: REQ_FLOORS[1],
                                text: row?.cells[1]?.textContent.trim() || ''};
                    };
                    try {
                        REQ_FLOORS.fill(0);
                        renderRequests(recs);
                        await frames();
                        const short = identity();
                        const longRecs = recs.map(r => r.id === 'iw-0' ? {...r,
                            client: 'openai/enterprise-client-with-a-very-long-product-suffix',
                            provider: 'provider-with-a-very-long-tenant-and-region-label',
                            model: 'neutral-model-with-a-very-long-suffix'} : r);
                        renderRequestsIncremental(longRecs);
                        await frames();
                        const long = identity();
                        renderRequestsIncremental(recs);
                        await frames();
                        const held = identity();
                        return {short, long, held};
                    } finally {
                        REQ_FLOORS.splice(0, REQ_FLOORS.length, ...savedFloors);
                        renderRequests(derive(lastData).scopedRecs);
                        await frames();
                    }
                }""")
                require(incremental_width['long']['floor'] >
                        incremental_width['short']['floor'] + 1, incremental_width)
                require(incremental_width['long']['width'] >
                        incremental_width['short']['width'] + 1, incremental_width)
                require(incremental_width['held']['floor'] ==
                        incremental_width['long']['floor'], incremental_width)
                require(incremental_width['held']['width'] >=
                        incremental_width['long']['floor'] - 0.5, incremental_width)
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

                # The theme tray stays open while values are chosen and the
                # contained preview changes without touching the document root.
                await page.set_viewport_size({'width': 1440, 'height': 1000})
                await page.evaluate('new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))')
                await page.click('#btn-theme')
                require(await page.locator('#theme-menu').is_visible(), 'theme tray did not open')
                tray_state = await page.evaluate("""() => {
                    const menu = document.getElementById('theme-menu');
                    const panes = menu.querySelector('.theme-panes');
                    const list = document.getElementById('theme-list');
                    const box = list.getBoundingClientRect();
                    const rows = [...list.querySelectorAll('.theme-opt')].map(row => {
                        const rect = row.getBoundingClientRect();
                        return rect.top >= box.top - 1 && rect.bottom <= box.bottom + 1 &&
                            rect.top >= 0 && rect.bottom <= innerHeight;
                    });
                    const mode = document.getElementById('theme-mode');
                    const legend = [...menu.querySelectorAll('#theme-current .tp-key')]
                        .map(key => getComputedStyle(key).backgroundColor);
                    return {
                        panes: !!panes && panes.getAttribute('aria-hidden') === 'true',
                        paneCount: panes ? panes.querySelectorAll('.theme-pane').length : 0,
                        currentPane: document.getElementById('theme-current')?.dataset.theme,
                        previewPane: document.getElementById('theme-preview')?.dataset.theme,
                        captionBar: !!mode.closest('.theme-caption-bar'),
                        surfaceLabel: menu.querySelector('.theme-caption-bar .theme-mode-label')
                            ?.textContent,
                        surfaceLabelRef: mode.getAttribute('aria-labelledby'),
                        currentCaption: document.getElementById('theme-current-label').textContent,
                        previewCaption: document.getElementById('theme-preview-label').textContent,
                        previewMuted: !document.getElementById('theme-preview-label')
                            .classList.contains('has-preview'),
                        hint: menu.querySelector('.theme-hintbar').textContent,
                        resetTitle: document.getElementById('theme-reset')?.title,
                        paletteRows: rows.length,
                        allRowsVisible: rows.length === 8 && rows.every(Boolean),
                        listScrolls: list.scrollHeight > list.clientHeight + 1,
                        listOverflow: getComputedStyle(list).overflowY,
                        legend,
                        legendText: document.querySelector('#theme-current .tp-legend').textContent,
                        pills: menu.querySelectorAll('.tp-pill').length,
                        closeText: document.getElementById('theme-close')?.textContent,
                        closeAccent: document.getElementById('theme-close')?.classList.contains('btn-accent'),
                        done: !!document.getElementById('theme-done'),
                    };
                }""")
                require(tray_state['panes'] and tray_state['paneCount'] == 2, tray_state)
                require(tray_state['currentPane'] == 'instrument' and tray_state['previewPane'] == 'instrument', tray_state)
                require(tray_state['captionBar'] and tray_state['surfaceLabel'] == 'Surface' and
                        tray_state['surfaceLabelRef'] == 'theme-mode-label', tray_state)
                require(tray_state['currentCaption'] == 'Current: Instrument / Black' and
                        tray_state['previewCaption'] == 'Preview: same' and
                        tray_state['previewMuted'], tray_state)
                require(tray_state['hint'] ==
                        'Arrows preview. Click or press Enter to apply. Esc closes.', tray_state)
                require(tray_state['resetTitle'] == 'Restore Instrument Black', tray_state)
                require(tray_state['allRowsVisible'] and not tray_state['listScrolls'] and
                        tray_state['listOverflow'] == 'visible', tray_state)
                require(len(tray_state['legend']) == 3 and
                        len(set(tray_state['legend'])) == 3 and
                        tray_state['legendText'] == '' and tray_state['pills'] == 0, tray_state)
                require(tray_state['closeText'] == 'Close' and not tray_state['closeAccent'] and
                        not tray_state['done'], tray_state)
                # A palette hover previews only inside the tray: the preview
                # pane takes the candidate while the current pane, root,
                # storage and canvas palette stay on the committed pair. The
                # polite live region belongs to the keyboard cursor, so hover
                # must not write it.
                committed_baseline = await page.evaluate("""() => {
                    window.themePreviewChart = {
                        calls: 0, refresh: refreshChartColors, reset: resetChart,
                    };
                    window.refreshChartColors = (...args) => {
                        window.themePreviewChart.calls += 1;
                        return window.themePreviewChart.refresh(...args);
                    };
                    window.resetChart = (...args) => {
                        window.themePreviewChart.calls += 1;
                        return window.themePreviewChart.reset(...args);
                    };
                    return {
                        colors: JSON.parse(JSON.stringify(COLORS)),
                        saved: localStorage.getItem('millivolt.theme'),
                        status: document.getElementById('theme-status').textContent,
                    };
                }""")
                await page.hover('[data-theme-id="forest"]')
                preview = await page.evaluate("""() => {
                    const stage = document.getElementById('theme-preview');
                    const current = document.getElementById('theme-current');
                    const style = getComputedStyle(stage);
                    const currentStyle = getComputedStyle(current);
                    return {
                        stage: stage.dataset.theme,
                        stageMode: stage.dataset.mode,
                        current: current.dataset.theme,
                        currentMode: current.dataset.mode,
                        root: document.documentElement.dataset.theme,
                        rootMode: document.documentElement.dataset.mode,
                        caption: document.getElementById('theme-preview-label').textContent,
                        currentCaption: document.getElementById('theme-current-label').textContent,
                        open: !document.getElementById('theme-menu').hidden,
                        saved: localStorage.getItem('millivolt.theme'),
                        status: document.getElementById('theme-status').textContent,
                        previewRows: document.querySelectorAll('#theme-menu .is-preview').length,
                        colors: JSON.parse(JSON.stringify(COLORS)),
                        chartCalls: window.themePreviewChart.calls,
                        bg: style.getPropertyValue('--bg').trim(),
                        accent: style.getPropertyValue('--accent').trim(),
                        currentBg: currentStyle.getPropertyValue('--bg').trim(),
                        color: style.color,
                        colorScheme: style.colorScheme,
                        legend: [...stage.querySelectorAll('.tp-key')].map(key => ({
                            bg: getComputedStyle(key).backgroundColor,
                        })),
                    };
                }""")
                require(preview['stage'] == 'forest' and preview['stageMode'] == 'black' and
                        preview['current'] == 'instrument' and preview['currentMode'] == 'black' and
                        preview['root'] == 'instrument' and preview['rootMode'] == 'black' and
                        preview['open'], preview)
                require(preview['bg'] == '#0f1713' and preview['accent'] == '#34d399', preview)
                require(preview['currentBg'] == '#141210' and
                        preview['currentCaption'] == 'Current: Instrument / Black', preview)
                require(preview['caption'] == 'Preview: Forest / Black', preview)
                require(preview['color'] == 'rgb(232, 228, 222)', preview)
                require(preview['colorScheme'] == 'dark', preview)
                require(len(preview['legend']) == 3 and
                        len({key['bg'] for key in preview['legend']}) == 3 and
                        all(key['bg'] not in ('rgba(0, 0, 0, 0)', 'transparent')
                            for key in preview['legend']), preview)
                require(preview['previewRows'] == 1 and
                        preview['saved'] == committed_baseline['saved'] ==
                        '{"theme":"instrument","mode":"black"}', preview)
                require(preview['colors'] == committed_baseline['colors'] and
                        preview['chartCalls'] == 0, preview)
                require(preview['status'] == committed_baseline['status'] == '', preview)
                await page.evaluate("""() => {
                    const spy = window.themePreviewChart;
                    window.refreshChartColors = spy.refresh;
                    window.resetChart = spy.reset;
                    delete window.themePreviewChart;
                }""")

                # Leaving the tray with the pointer, not a click, drops the
                # candidate: the preview pane returns to the committed pair and
                # says so, while the current pane, root and storage persist.
                await page.mouse.move(8, 8)
                left_tray = await page.evaluate("""() => ({
                    stage: document.getElementById('theme-preview').dataset.theme,
                    stageMode: document.getElementById('theme-preview').dataset.mode,
                    current: document.getElementById('theme-current').dataset.theme,
                    currentMode: document.getElementById('theme-current').dataset.mode,
                    root: document.documentElement.dataset.theme,
                    rootMode: document.documentElement.dataset.mode,
                    caption: document.getElementById('theme-preview-label').textContent,
                    currentCaption: document.getElementById('theme-current-label').textContent,
                    open: !document.getElementById('theme-menu').hidden,
                    saved: localStorage.getItem('millivolt.theme'),
                    previewRows: document.querySelectorAll('#theme-menu .is-preview').length,
                })""")
                require(left_tray['stage'] == 'instrument' and left_tray['stageMode'] == 'black' and
                        left_tray['current'] == 'instrument' and left_tray['currentMode'] == 'black',
                        left_tray)
                require(left_tray['caption'] == 'Preview: same' and
                        left_tray['currentCaption'] == 'Current: Instrument / Black', left_tray)
                require(left_tray['root'] == 'instrument' and left_tray['rootMode'] == 'black' and
                        left_tray['open'] and left_tray['previewRows'] == 0, left_tray)
                require(left_tray['saved'] == '{"theme":"instrument","mode":"black"}', left_tray)

                # Hovering then clicking the same palette commits the shown
                # candidate, updates both panes and leaves the tray open. The
                # pointer-induced focus must not write the keyboard live
                # region: the only announcement is the committed Current line.
                await page.evaluate("""() => {
                    window.themeStatusLog = [];
                    const status = document.getElementById('theme-status');
                    window.themeStatusObserver = new MutationObserver(() => {
                        if (status.textContent) window.themeStatusLog.push(status.textContent);
                    });
                    window.themeStatusObserver.observe(status, {
                        childList: true, characterData: true, subtree: true,
                    });
                }""")
                await page.hover('[data-theme-id="forest"]')
                hovered = await page.evaluate("""() => ({
                    stage: document.getElementById('theme-preview').dataset.theme,
                    root: document.documentElement.dataset.theme,
                    saved: localStorage.getItem('millivolt.theme'),
                    statusLog: window.themeStatusLog.slice(),
                })""")
                require(hovered['stage'] == 'forest' and hovered['root'] == 'instrument' and
                        hovered['saved'] == '{"theme":"instrument","mode":"black"}', hovered)
                require(hovered['statusLog'] == [], hovered)
                await page.click('[data-theme-id="forest"]')
                chosen = await page.evaluate("""() => {
                    const log = window.themeStatusLog.slice();
                    window.themeStatusObserver.disconnect();
                    delete window.themeStatusObserver;
                    delete window.themeStatusLog;
                    return {
                        theme: document.documentElement.dataset.theme,
                        open: !document.getElementById('theme-menu').hidden,
                        current: document.getElementById('theme-current').dataset.theme,
                        stage: document.getElementById('theme-preview').dataset.theme,
                        currentCaption: document.getElementById('theme-current-label').textContent,
                        previewCaption: document.getElementById('theme-preview-label').textContent,
                        previewMuted: !document.getElementById('theme-preview-label')
                            .classList.contains('has-preview'),
                        saved: localStorage.getItem('millivolt.theme'),
                        previewRows: document.querySelectorAll('#theme-menu .is-preview').length,
                        statusLog: log,
                        focus: document.activeElement?.dataset.themeId,
                    };
                }""")
                require(chosen['theme'] == 'forest' and chosen['open'] and
                        chosen['current'] == 'forest' and chosen['stage'] == 'forest', chosen)
                require(chosen['currentCaption'] == 'Current: Forest / Black' and
                        chosen['previewCaption'] == 'Preview: same' and chosen['previewMuted'], chosen)
                require(chosen['saved'] == '{"theme":"forest","mode":"black"}' and
                        chosen['previewRows'] == 0, chosen)
                require(chosen['focus'] == 'forest', chosen)
                require(len(chosen['statusLog']) == 1 and
                        chosen['statusLog'][0] == 'Current: Forest / Black', chosen)

                # A surface hover composes with the committed palette: the
                # preview pane shows Forest / Clear while the current pane and
                # the root stay on the committed Forest / Black.
                await page.hover('[data-mode-id="clear"]')
                surface_preview = await page.evaluate("""() => {
                    const stage = document.getElementById('theme-preview');
                    const current = document.getElementById('theme-current');
                    const stageStyle = getComputedStyle(stage);
                    const currentStyle = getComputedStyle(current);
                    const clear = document.querySelector('[data-mode-id="clear"]');
                    return {
                        stage: stage.dataset.theme,
                        stageMode: stage.dataset.mode,
                        current: current.dataset.theme,
                        currentMode: current.dataset.mode,
                        root: document.documentElement.dataset.theme,
                        rootMode: document.documentElement.dataset.mode,
                        caption: document.getElementById('theme-preview-label').textContent,
                        currentCaption: document.getElementById('theme-current-label').textContent,
                        stageBg: stageStyle.getPropertyValue('--bg').trim(),
                        currentBg: currentStyle.getPropertyValue('--bg').trim(),
                        stageScheme: stageStyle.colorScheme,
                        currentScheme: currentStyle.colorScheme,
                        saved: localStorage.getItem('millivolt.theme'),
                        open: !document.getElementById('theme-menu').hidden,
                        checked: clear.getAttribute('aria-checked'),
                        previewed: clear.classList.contains('is-preview'),
                        previewRows: document.querySelectorAll('#theme-menu .is-preview').length,
                    };
                }""")
                require(surface_preview['stage'] == 'forest' and
                        surface_preview['stageMode'] == 'clear' and
                        surface_preview['current'] == 'forest' and
                        surface_preview['currentMode'] == 'black', surface_preview)
                require(surface_preview['caption'] == 'Preview: Forest / Clear' and
                        surface_preview['currentCaption'] == 'Current: Forest / Black', surface_preview)
                require(surface_preview['stageBg'] == '#eef6ef' and
                        surface_preview['currentBg'] == '#0f1713' and
                        surface_preview['stageScheme'] == 'light' and
                        surface_preview['currentScheme'] == 'dark', surface_preview)
                require(surface_preview['root'] == 'forest' and
                        surface_preview['rootMode'] == 'black' and surface_preview['open'],
                        surface_preview)
                require(surface_preview['saved'] == '{"theme":"forest","mode":"black"}' and
                        surface_preview['checked'] == 'false' and
                        surface_preview['previewed'] and
                        surface_preview['previewRows'] == 1, surface_preview)

                # Moving the pointer off the surface control restores the
                # committed preview without committing the candidate.
                await page.mouse.move(8, 8)
                surface_left = await page.evaluate("""() => ({
                    stage: document.getElementById('theme-preview').dataset.theme,
                    stageMode: document.getElementById('theme-preview').dataset.mode,
                    current: document.getElementById('theme-current').dataset.theme,
                    currentMode: document.getElementById('theme-current').dataset.mode,
                    root: document.documentElement.dataset.theme,
                    rootMode: document.documentElement.dataset.mode,
                    caption: document.getElementById('theme-preview-label').textContent,
                    currentCaption: document.getElementById('theme-current-label').textContent,
                    open: !document.getElementById('theme-menu').hidden,
                    saved: localStorage.getItem('millivolt.theme'),
                    previewRows: document.querySelectorAll('#theme-menu .is-preview').length,
                })""")
                require(surface_left['stage'] == 'forest' and surface_left['stageMode'] == 'black' and
                        surface_left['current'] == 'forest' and
                        surface_left['currentMode'] == 'black', surface_left)
                require(surface_left['caption'] == 'Preview: same' and
                        surface_left['currentCaption'] == 'Current: Forest / Black', surface_left)
                require(surface_left['root'] == 'forest' and surface_left['rootMode'] == 'black' and
                        surface_left['open'] and surface_left['previewRows'] == 0, surface_left)
                require(surface_left['saved'] == '{"theme":"forest","mode":"black"}', surface_left)
                await page.click('[data-mode-id="clear"]')
                await page.wait_for_timeout(220)
                cleared = await page.evaluate("""() => {
                    const rootStyle = getComputedStyle(document.documentElement);
                    return {
                        theme: document.documentElement.dataset.theme,
                        mode: document.documentElement.dataset.mode,
                        open: !document.getElementById('theme-menu').hidden,
                        headerShadow: getComputedStyle(document.querySelector('header')).boxShadow,
                        menuShadow: getComputedStyle(document.getElementById('theme-menu')).boxShadow,
                        shadowRgb: rootStyle.getPropertyValue('--shadow-rgb').trim(),
                        veilRgb: rootStyle.getPropertyValue('--veil-rgb').trim(),
                        veilStrong: rootStyle.getPropertyValue('--veil-strong').trim(),
                        veilSoft: rootStyle.getPropertyValue('--veil-soft').trim(),
                        stageScheme: getComputedStyle(
                            document.getElementById('theme-preview')).colorScheme,
                        currentScheme: getComputedStyle(
                            document.getElementById('theme-current')).colorScheme,
                    };
                }""")
                require(cleared['theme'] == 'forest' and cleared['mode'] == 'clear' and cleared['open'], cleared)
                require(cleared['shadowRgb'] == '38, 58, 44' and
                        cleared['veilRgb'] == '30, 48, 36', cleared)
                require('38, 58, 44' in cleared['headerShadow'], cleared)
                require('38, 58, 44' in cleared['menuShadow'], cleared)
                require('30, 48, 36' in cleared['veilStrong'] and
                        '30, 48, 36' in cleared['veilSoft'], cleared)
                require('.6' in cleared['veilStrong'] and '.55' in cleared['veilSoft'], cleared)
                require(cleared['stageScheme'] == 'light' and cleared['currentScheme'] == 'light', cleared)
                await page.click('#theme-reset')
                reset = await page.evaluate("""() => ({
                    theme: document.documentElement.dataset.theme,
                    mode: document.documentElement.dataset.mode,
                    open: !document.getElementById('theme-menu').hidden,
                    saved: localStorage.getItem('millivolt.theme'),
                })""")
                require(reset == {'theme': 'instrument', 'mode': 'black', 'open': True,
                                  'saved': '{"theme":"instrument","mode":"black"}'}, reset)
                # The mounted plot can be sparse during the fixture run, so
                # prove the cascade on the real vendored stylesheet with the
                # same DOM shape uPlot creates: root uplot/u-hz, u-over child,
                # u-cursor-x grandchild.
                uplot = await page.evaluate("""() => {
                    const fixture = document.createElement('div');
                    fixture.className = 'uplot u-hz';
                    fixture.style.position = 'fixed';
                    fixture.style.left = '-9999px';
                    const over = document.createElement('div');
                    over.className = 'u-over';
                    const cursor = document.createElement('div');
                    cursor.className = 'u-cursor-x';
                    over.appendChild(cursor);
                    fixture.appendChild(over);
                    document.body.appendChild(fixture);
                    const probe = document.createElement('span');
                    probe.style.color = 'var(--muted)';
                    document.body.appendChild(probe);
                    const style = getComputedStyle(cursor);
                    const opts = upOpts(600, 150);
                    const result = {
                        color: style.borderRightColor,
                        expected: getComputedStyle(probe).color,
                        style: style.borderRightStyle,
                        disabled: opts.cursor.y === false && opts.select.show === false,
                    };
                    probe.remove();
                    fixture.remove();
                    return result;
                }""")
                require(uplot['color'] == uplot['expected'] and uplot['style'] == 'dashed' and
                        uplot['disabled'], uplot)

                # Programmatic (assistive-technology) activation commits a row
                # without moving DOM focus. The committed panes and the polite
                # status must agree, so no stale candidate line survives.
                at_activation = await page.evaluate("""() => {
                    const reset = document.getElementById('theme-reset');
                    reset.focus();
                    const forest = document.querySelector('[data-theme-id="forest"]');
                    forest.dispatchEvent(new MouseEvent('mouseenter', {bubbles: true}));
                    const before = {
                        stage: document.getElementById('theme-preview').dataset.theme,
                        status: document.getElementById('theme-status').textContent,
                    };
                    forest.click();
                    return {
                        before,
                        theme: document.documentElement.dataset.theme,
                        focus: document.activeElement?.id,
                        stage: document.getElementById('theme-preview').dataset.theme,
                        status: document.getElementById('theme-status').textContent,
                        caption: document.getElementById('theme-preview-label').textContent,
                        previewMuted: !document.getElementById('theme-preview-label')
                            .classList.contains('has-preview'),
                    };
                }""")
                require(at_activation['before'] == {'stage': 'forest', 'status': ''},
                        at_activation)
                require(at_activation['theme'] == 'forest' and
                        at_activation['focus'] == 'theme-reset' and
                        at_activation['stage'] == 'forest' and
                        at_activation['status'] == '' and
                        at_activation['caption'] == 'Preview: same' and
                        at_activation['previewMuted'], at_activation)
                await page.click('#theme-reset')

                # Close, Escape and an outside click share one cleanup. Each is
                # exercised with a live keyboard candidate: the committed pair,
                # storage and both panes stay put, the candidate line clears and
                # focus never remains on a hidden row.
                await page.focus('[data-theme-id="rose"]')
                candidate_before_close = await page.evaluate("""() => ({
                    stage: document.getElementById('theme-preview').dataset.theme,
                    status: document.getElementById('theme-status').textContent,
                })""")
                require(candidate_before_close == {'stage': 'rose',
                        'status': 'Preview: Rose / Black'}, candidate_before_close)
                await page.evaluate(
                    "document.getElementById('theme-close').click()")
                closed = await page.evaluate("""() => ({
                    open: !document.getElementById('theme-menu').hidden,
                    focus: document.activeElement?.id,
                    theme: document.documentElement.dataset.theme,
                    mode: document.documentElement.dataset.mode,
                    stage: document.getElementById('theme-preview').dataset.theme,
                    stageMode: document.getElementById('theme-preview').dataset.mode,
                    status: document.getElementById('theme-status').textContent,
                    caption: document.getElementById('theme-preview-label').textContent,
                    previewMuted: !document.getElementById('theme-preview-label')
                        .classList.contains('has-preview'),
                    saved: localStorage.getItem('millivolt.theme'),
                })""")
                require(not closed['open'] and closed['focus'] == 'btn-theme', closed)
                require(closed['theme'] == 'instrument' and closed['mode'] == 'black' and
                        closed['stage'] == 'instrument' and closed['stageMode'] == 'black', closed)
                require(closed['status'] == '' and closed['caption'] == 'Preview: same' and
                        closed['previewMuted'] and
                        closed['saved'] == '{"theme":"instrument","mode":"black"}', closed)
                await page.click('#btn-theme')
                await page.focus('[data-theme-id="ember"]')
                await page.keyboard.press('Escape')
                escaped = await page.evaluate("""() => ({
                    open: !document.getElementById('theme-menu').hidden,
                    focus: document.activeElement?.id,
                    theme: document.documentElement.dataset.theme,
                    mode: document.documentElement.dataset.mode,
                    stage: document.getElementById('theme-preview').dataset.theme,
                    stageMode: document.getElementById('theme-preview').dataset.mode,
                    status: document.getElementById('theme-status').textContent,
                    caption: document.getElementById('theme-preview-label').textContent,
                    previewMuted: !document.getElementById('theme-preview-label')
                        .classList.contains('has-preview'),
                    saved: localStorage.getItem('millivolt.theme'),
                })""")
                require(not escaped['open'] and escaped['focus'] == 'btn-theme', escaped)
                require(escaped['theme'] == 'instrument' and escaped['mode'] == 'black' and
                        escaped['stage'] == 'instrument' and escaped['stageMode'] == 'black', escaped)
                require(escaped['status'] == '' and escaped['caption'] == 'Preview: same' and
                        escaped['previewMuted'] and
                        escaped['saved'] == '{"theme":"instrument","mode":"black"}', escaped)
                await page.click('#btn-theme')
                await page.focus('[data-theme-id="nord"]')
                await page.evaluate(
                    "document.body.dispatchEvent(new MouseEvent('click', {bubbles: true}))")
                outside = await page.evaluate("""() => ({
                    open: !document.getElementById('theme-menu').hidden,
                    focusInside: document.getElementById('theme-menu')
                        .contains(document.activeElement),
                    focus: document.activeElement?.id,
                    theme: document.documentElement.dataset.theme,
                    mode: document.documentElement.dataset.mode,
                    stage: document.getElementById('theme-preview').dataset.theme,
                    stageMode: document.getElementById('theme-preview').dataset.mode,
                    status: document.getElementById('theme-status').textContent,
                    caption: document.getElementById('theme-preview-label').textContent,
                    previewMuted: !document.getElementById('theme-preview-label')
                        .classList.contains('has-preview'),
                    saved: localStorage.getItem('millivolt.theme'),
                })""")
                require(not outside['open'] and not outside['focusInside'] and
                        outside['focus'] == 'btn-theme', outside)
                require(outside['theme'] == 'instrument' and outside['mode'] == 'black' and
                        outside['stage'] == 'instrument' and outside['stageMode'] == 'black', outside)
                require(outside['status'] == '' and outside['caption'] == 'Preview: same' and
                        outside['previewMuted'] and
                        outside['saved'] == '{"theme":"instrument","mode":"black"}', outside)

                # The trigger's arrows preview without committing. A closed
                # trigger opens on the checked palette; an open trigger moves
                # the keyboard cursor so Enter applies the focused candidate.
                await page.focus('#btn-theme')
                await page.keyboard.press('ArrowDown')
                trigger_open = await page.evaluate("""() => ({
                    theme: document.documentElement.dataset.theme,
                    open: !document.getElementById('theme-menu').hidden,
                    focus: document.activeElement?.dataset.themeId,
                })""")
                require(trigger_open['theme'] == 'instrument' and trigger_open['open'] and
                        trigger_open['focus'] == 'instrument', trigger_open)
                await page.focus('#btn-theme')
                await page.keyboard.press('ArrowDown')
                trigger_arrow = await page.evaluate("""() => ({
                    theme: document.documentElement.dataset.theme,
                    open: !document.getElementById('theme-menu').hidden,
                    focus: document.activeElement?.dataset.themeId,
                    stage: document.getElementById('theme-preview').dataset.theme,
                    caption: document.getElementById('theme-preview-label').textContent,
                    status: document.getElementById('theme-status').textContent,
                    saved: localStorage.getItem('millivolt.theme'),
                })""")
                require(trigger_arrow['theme'] == 'instrument' and trigger_arrow['open'] and
                        trigger_arrow['focus'] == 'slate' and trigger_arrow['stage'] == 'slate' and
                        trigger_arrow['caption'] == 'Preview: Slate / Black' and
                        trigger_arrow['status'] == 'Preview: Slate / Black' and
                        trigger_arrow['saved'] == '{"theme":"instrument","mode":"black"}', trigger_arrow)
                await page.keyboard.press('Enter')
                trigger_commit = await page.evaluate("""() => ({
                    theme: document.documentElement.dataset.theme,
                    open: !document.getElementById('theme-menu').hidden,
                    focus: document.activeElement?.dataset.themeId,
                    current: document.getElementById('theme-current').dataset.theme,
                    stage: document.getElementById('theme-preview').dataset.theme,
                    currentCaption: document.getElementById('theme-current-label').textContent,
                    previewCaption: document.getElementById('theme-preview-label').textContent,
                    saved: localStorage.getItem('millivolt.theme'),
                })""")
                require(trigger_commit['theme'] == 'slate' and trigger_commit['open'] and
                        trigger_commit['focus'] == 'slate' and
                        trigger_commit['current'] == 'slate' and trigger_commit['stage'] == 'slate' and
                        trigger_commit['currentCaption'] == 'Current: Slate / Black' and
                        trigger_commit['previewCaption'] == 'Preview: same', trigger_commit)

                # Roving tabindex: the arrow cursor owns the group's single tab
                # stop, so Tab out and back returns to the preview row rather
                # than the committed row.
                await page.keyboard.press('ArrowDown')
                roving = await page.evaluate("""() => ({
                    focus: document.activeElement?.dataset.themeId,
                    slate: document.querySelector('[data-theme-id="slate"]').tabIndex,
                    midnight: document.querySelector('[data-theme-id="midnight"]').tabIndex,
                    stops: [...document.querySelectorAll('[data-theme-id]')]
                        .filter(option => option.tabIndex === 0).length,
                    stage: document.getElementById('theme-preview').dataset.theme,
                    status: document.getElementById('theme-status').textContent,
                })""")
                require(roving == {'focus': 'midnight', 'slate': -1, 'midnight': 0,
                                   'stops': 1, 'stage': 'midnight',
                                   'status': 'Preview: Midnight / Black'}, roving)
                await page.keyboard.press('Tab')
                left_group = await page.evaluate("""() => ({
                    inPalette: !!document.activeElement.closest('#theme-list'),
                    id: document.activeElement?.id,
                    midnight: document.querySelector('[data-theme-id="midnight"]').tabIndex,
                    stage: document.getElementById('theme-preview').dataset.theme,
                    status: document.getElementById('theme-status').textContent,
                })""")
                require(not left_group['inPalette'] and left_group['id'] == 'theme-reset' and
                        left_group['midnight'] == 0 and left_group['stage'] == 'slate' and
                        left_group['status'] == '', left_group)
                await page.keyboard.press('Shift+Tab')
                returned = await page.evaluate("""() => ({
                    focus: document.activeElement?.dataset.themeId,
                    midnight: document.querySelector('[data-theme-id="midnight"]').tabIndex,
                    stops: [...document.querySelectorAll('[data-theme-id]')]
                        .filter(option => option.tabIndex === 0).length,
                    stage: document.getElementById('theme-preview').dataset.theme,
                    status: document.getElementById('theme-status').textContent,
                })""")
                require(returned == {'focus': 'midnight', 'midnight': 0, 'stops': 1,
                                     'stage': 'midnight',
                                     'status': 'Preview: Midnight / Black'}, returned)
                await page.click('#theme-reset')
                await page.click('#theme-close')

                # A short window hides the comparator, preview line and hint;
                # above 800px the tray is the single vertical scroller, and a
                # reopen brings the saved palette into view instead of keeping
                # an arbitrary previous offset.
                short_checks = []
                for viewport in ({'width': 1440, 'height': 650},
                                 {'width': 1024, 'height': 400}):
                    await page.set_viewport_size(viewport)
                    await page.evaluate(
                        'new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))')
                    await page.evaluate("""() => {
                        const menu = document.getElementById('theme-menu');
                        if (!menu.hidden) document.getElementById('theme-close').click();
                    }""")
                    await page.click('#btn-theme')
                    short = await page.evaluate("""async () => {
                        const menu = document.getElementById('theme-menu');
                        const footer = menu.querySelector('.theme-ft');
                        const caption = menu.querySelector('.theme-caption-bar');
                        const mode = document.getElementById('theme-mode');
                        const list = document.getElementById('theme-list');
                        const scrollable = menu.scrollHeight > menu.clientHeight + 1;
                        menu.scrollTop = menu.scrollHeight;
                        await new Promise(r => requestAnimationFrame(r));
                        const rect = footer.getBoundingClientRect();
                        const box = menu.getBoundingClientRect();
                        return {
                            open: !menu.hidden,
                            scrollable,
                            scrollTop: menu.scrollTop,
                            footerVisible: rect.top >= box.top - 1 && rect.bottom <= innerHeight + 1,
                            horizontal: menu.scrollWidth > menu.clientWidth + 1,
                            panes: getComputedStyle(menu.querySelector('.theme-panes')).display,
                            previewCaption: getComputedStyle(
                                document.getElementById('theme-preview-label')).display,
                            captionVisible: getComputedStyle(caption).display !== 'none',
                            currentCaption: document.getElementById('theme-current-label').textContent,
                            surfaceVisible: getComputedStyle(mode).display !== 'none',
                            hintHidden: getComputedStyle(menu.querySelector('.theme-hintbar')).display === 'none',
                            listScrolls: list.scrollHeight > list.clientHeight + 1,
                            listOverflow: getComputedStyle(list).overflowY,
                        };
                    }""")
                    require(short['open'] and short['footerVisible'], short)
                    require(not short['horizontal'] and short['panes'] == 'none' and
                            short['previewCaption'] == 'none' and short['captionVisible'] and
                            short['currentCaption'] == 'Current: Instrument / Black' and
                            short['surfaceVisible'] and short['hintHidden'] and
                            not short['listScrolls'] and short['listOverflow'] == 'visible', short)
                    reopened = None
                    if short['scrollable']:
                        await page.evaluate(
                            "document.getElementById('theme-menu').scrollTop = "
                            "document.getElementById('theme-menu').scrollHeight")
                        await page.click('#theme-close')
                        await page.click('#btn-theme')
                        reopened = await page.evaluate("""() => {
                            const menu = document.getElementById('theme-menu');
                            const checked = menu.querySelector(
                                '.theme-opt[aria-checked="true"]').getBoundingClientRect();
                            const box = menu.getBoundingClientRect();
                            return {
                                scrollTop: menu.scrollTop,
                                checkedVisible: checked.top >= box.top - 1 &&
                                    checked.bottom <= box.bottom + 1,
                                currentCaption: document.getElementById(
                                    'theme-current-label').textContent,
                                panes: getComputedStyle(
                                    document.querySelector('.theme-panes')).display,
                            };
                        }""")
                        require(reopened['checkedVisible'] and
                                reopened['currentCaption'] == 'Current: Instrument / Black' and
                                reopened['panes'] == 'none', reopened)
                    short_checks.append({'viewport': viewport, **short, 'reopened': reopened})
                await page.set_viewport_size({'width': 1440, 'height': 1000})
                await page.click('#theme-close')

                # A narrow fine-pointer window is compact too: the width, not
                # the pointer type, owns the comparator.
                await page.set_viewport_size({'width': 800, 'height': 1000})
                await page.evaluate(
                    'new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))')
                await page.click('#btn-nav')
                await page.click('#btn-theme')
                narrow = await page.evaluate("""() => {
                    const menu = document.getElementById('theme-menu');
                    const list = document.getElementById('theme-list');
                    const box = list.getBoundingClientRect();
                    const rows = [...list.querySelectorAll('.theme-opt')].filter(row => {
                        const rect = row.getBoundingClientRect();
                        return rect.top >= box.top - 1 && rect.bottom <= box.bottom + 1;
                    });
                    return {
                        fine: matchMedia('(pointer: fine)').matches,
                        hover: matchMedia('(hover: hover)').matches,
                        panes: getComputedStyle(menu.querySelector('.theme-panes')).display,
                        previewCaption: getComputedStyle(
                            document.getElementById('theme-preview-label')).display,
                        hint: getComputedStyle(menu.querySelector('.theme-hintbar')).display,
                        caption: getComputedStyle(menu.querySelector('.theme-caption-bar')).display,
                        rows: rows.length,
                        listScrolls: list.scrollHeight > list.clientHeight + 1,
                    };
                }""")
                require(narrow['fine'] and narrow['hover'], narrow)
                require(narrow['panes'] == 'none' and narrow['previewCaption'] == 'none' and
                        narrow['hint'] == 'none' and narrow['caption'] != 'none' and
                        narrow['rows'] == 8 and not narrow['listScrolls'], narrow)
                await page.click('#theme-close')
                await page.click('#btn-nav')
                await page.set_viewport_size({'width': 1440, 'height': 1000})

                # On a phone the tray lives inside the nav flyout's content box:
                # no horizontal flyout scroll and 44px touch targets.
                await page.set_viewport_size({'width': 390, 'height': 844})
                await page.click('#btn-nav')
                await page.click('#btn-theme')
                tray_fits = []
                for width in (390, 360, 320):
                    await page.set_viewport_size({'width': width, 'height': 844})
                    state = await page.evaluate("""async () => {
                        await new Promise(r => requestAnimationFrame(r));
                        const fly = document.getElementById('hdr-actions');
                        const menu = document.getElementById('theme-menu');
                        const captionBar = menu.querySelector('.theme-caption-bar');
                        const style = getComputedStyle(fly);
                        const flyRect = fly.getBoundingClientRect();
                        const menuRect = menu.getBoundingClientRect();
                        const left = flyRect.left + parseFloat(style.borderLeftWidth) + parseFloat(style.paddingLeft);
                        const right = flyRect.right - parseFloat(style.borderRightWidth) - parseFloat(style.paddingRight);
                        const heights = selector => [...menu.querySelectorAll(selector)]
                            .map(node => node.getBoundingClientRect().height);
                        const modeHeights = heights('.theme-mode-opt');
                        const paletteHeights = heights('.theme-opt');
                        const footerHeights = heights('.theme-ft .btn');
                        const themeBtn = document.getElementById('btn-theme');
                        const themeIcon = themeBtn.querySelector('svg.hdr-ico');
                        const themeLabel = themeBtn.querySelector('.hdr-btn-label');
                        const themeRect = themeBtn.getBoundingClientRect();
                        const iconRect = themeIcon.getBoundingClientRect();
                        const labelRect = themeLabel.getBoundingClientRect();
                        const modeRect = document.getElementById('theme-mode').getBoundingClientRect();
                        const barRect = captionBar.getBoundingClientRect();
                        const barStyle = getComputedStyle(captionBar);
                        const barInner = barRect.width - parseFloat(barStyle.paddingLeft) -
                            parseFloat(barStyle.paddingRight) -
                            parseFloat(barStyle.borderLeftWidth) -
                            parseFloat(barStyle.borderRightWidth);
                        const hints = [...menu.querySelectorAll('.theme-hint')];
                        const names = [...menu.querySelectorAll('.theme-name')];
                        const swatches = [...menu.querySelectorAll('.theme-swatch')];
                        return {
                            open: fly.classList.contains('is-open') && !menu.hidden,
                            menuWidth: Math.round(menuRect.width),
                            contentWidth: Math.round(right - left),
                            inside: menuRect.left >= left - 1 && menuRect.right <= right + 1,
                            horizontal: fly.scrollWidth > fly.clientWidth + 1 ||
                                menu.scrollWidth > menu.clientWidth + 1,
                            modeCount: modeHeights.length,
                            paletteCount: paletteHeights.length,
                            footerCount: footerHeights.length,
                            minMode: Math.min(...modeHeights),
                            minPalette: Math.min(...paletteHeights),
                            minFooter: Math.min(...footerHeights),
                            modeInCaption: document.getElementById('theme-mode')
                                .closest('.theme-caption-bar') === captionBar,
                            currentCaption: document.getElementById('theme-current-label').textContent,
                            previewCaption: getComputedStyle(
                                document.getElementById('theme-preview-label')).display,
                            barDirection: getComputedStyle(captionBar).flexDirection,
                            modeWidth: Math.round(modeRect.width),
                            barWidth: Math.round(barInner),
                            hintVisible: hints.some(hint => getComputedStyle(hint).display !== 'none'),
                            nameCount: names.length,
                            swatchCount: swatches.length,
                            namesVisible: names.every(name =>
                                name.textContent.trim() !== '' &&
                                name.getBoundingClientRect().width > 0),
                            themeWidth: themeRect.width,
                            themeHeight: themeRect.height,
                            iconWidth: iconRect.width,
                            labelRight: labelRect.right,
                            themeRight: themeRect.right,
                            pageOverflow: document.documentElement.scrollWidth > innerWidth + 1,
                        };
                    }""")
                    require(state['open'] and state['inside'] and not state['horizontal'], state)
                    require(state['menuWidth'] <= state['contentWidth'], state)
                    require(state['modeCount'] == 2 and state['paletteCount'] == 8 and state['footerCount'] == 2, state)
                    require(state['minMode'] >= 44 and state['minFooter'] >= 44 and state['minPalette'] >= 44, state)
                    require(state['modeInCaption'] and state['currentCaption'].startswith('Current: ') and
                            state['previewCaption'] == 'none', state)
                    require(state['nameCount'] == 8 and state['swatchCount'] == 8 and
                            state['namesVisible'], state)
                    if width <= 360:
                        require(state['barDirection'] == 'column' and
                                abs(state['modeWidth'] - state['barWidth']) <= 2 and
                                not state['hintVisible'], state)
                    require(state['themeWidth'] > 100 and state['themeHeight'] >= 44, state)
                    require(15.5 <= state['iconWidth'] <= 16.5, state)
                    require(state['labelRight'] <= state['themeRight'] + 0.5, state)
                    require(not state['pageOverflow'], state)
                    tray_fits.append({'width': width, **state})
                await page.set_viewport_size({'width': 390, 'height': 844})
                await page.click('#theme-close')
                await page.click('#btn-nav')
                results.append({'theme_tray': {'tray': tray_state, 'preview': preview,
                    'left_tray': left_tray, 'chosen': chosen,
                    'surface_preview': surface_preview, 'surface_left': surface_left,
                    'cleared': cleared, 'reset': reset, 'closed': closed, 'escaped': escaped,
                    'trigger_open': trigger_open, 'trigger_arrow': trigger_arrow,
                    'trigger_commit': trigger_commit, 'uplot': uplot,
                    'at_activation': at_activation, 'roving': roving,
                    'returned': returned,
                    'short': short_checks, 'narrow': narrow, 'tray_fits': tray_fits}})

                # A coarse-pointer device has no hover preview, so the tray
                # must not advertise the hover gesture. On a phone the nav
                # flyout is the only vertical scroller: a touch pan over the
                # tray must move #hdr-actions, with Reset/Close reachable.
                touch_context = await browser.new_context(
                    viewport={'width': 390, 'height': 844}, has_touch=True,
                    is_mobile=True, service_workers='block')
                try:
                    await touch_context.route(
                        '**/*', lambda route: browser_route(route, base, errors))
                    await operator_signin(touch_context, base)
                    touch_page = await touch_context.new_page()
                    touch_page.on('pageerror', lambda error: errors.append(str(error)))
                    await touch_page.goto(base + '/', wait_until='domcontentloaded')
                    await touch_page.wait_for_function('chartAgg && explorerAgg && lastData')
                    await touch_page.click('#btn-nav')
                    await touch_page.click('#btn-theme')
                    touch_state = await touch_page.evaluate("""() => {
                        const size = id => {
                            const rect = document.getElementById(id).getBoundingClientRect();
                            return { width: rect.width, height: rect.height };
                        };
                        const menu = document.getElementById('theme-menu');
                        return {
                            coarse: matchMedia('(pointer: coarse)').matches,
                            hoverNone: matchMedia('(hover: none)').matches,
                            hint: getComputedStyle(
                                document.querySelector('.theme-hintbar')).display,
                            panes: getComputedStyle(menu.querySelector('.theme-panes')).display,
                            previewCaption: getComputedStyle(
                                document.getElementById('theme-preview-label')).display,
                            captionVisible: getComputedStyle(
                                menu.querySelector('.theme-caption-bar')).display !== 'none',
                            surfaceLabel: menu.querySelector('.theme-mode-label')?.textContent,
                            currentCaption: document.getElementById('theme-current-label').textContent,
                            surfaceInCaption: document.getElementById('theme-mode')
                                .closest('.theme-caption-bar') !== null,
                            close: document.getElementById('theme-close')?.textContent,
                            themeTrigger: size('btn-theme'),
                            navTrigger: size('btn-nav'),
                        };
                    }""")
                    require(touch_state['coarse'] and touch_state['hoverNone'] and
                            touch_state['hint'] == 'none' and touch_state['panes'] == 'none',
                            touch_state)
                    require(touch_state['previewCaption'] == 'none' and
                            touch_state['captionVisible'] and touch_state['surfaceInCaption'] and
                            touch_state['surfaceLabel'] == 'Surface' and
                            touch_state['currentCaption'].startswith('Current: ') and
                            touch_state['close'] == 'Close', touch_state)
                    require(all(touch_state[key][side] >= 44 for key in ('themeTrigger', 'navTrigger')
                                for side in ('width', 'height')), touch_state)
                    require(touch_state['themeTrigger']['width'] > 100, touch_state)
                    touch_cdp = await touch_context.new_cdp_session(touch_page)
                    touch_checks = []
                    for height, palette in ((844, 'slate'), (600, 'forest')):
                        if await touch_page.locator('#theme-menu').is_visible():
                            await touch_page.click('#theme-close')
                        nav_open = await touch_page.evaluate(
                            "document.getElementById('hdr-actions').classList.contains('is-open')")
                        if nav_open:
                            await touch_page.click('#btn-nav')
                        await touch_page.set_viewport_size({'width': 390, 'height': height})
                        await touch_page.click('#btn-nav')
                        await touch_page.click('#btn-theme')
                        await touch_page.evaluate(
                            'new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))')
                        point = await touch_page.evaluate("""async () => {
                            const fly = document.getElementById('hdr-actions');
                            const list = document.getElementById('theme-list');
                            fly.scrollTop = Math.min(120,
                                Math.max(0, fly.scrollHeight - fly.clientHeight - 1));
                            await new Promise(r => requestAnimationFrame(r));
                            const flyRect = fly.getBoundingClientRect();
                            const option = [...list.querySelectorAll('.theme-opt')].find(node => {
                                const rect = node.getBoundingClientRect();
                                return rect.top >= flyRect.top + 8 &&
                                    rect.bottom <= flyRect.bottom - 8;
                            });
                            if (!option) return null;
                            const rect = option.getBoundingClientRect();
                            return {
                                before: fly.scrollTop,
                                x: rect.left + rect.width / 2,
                                y: rect.top + rect.height / 2,
                            };
                        }""")
                        require(point is not None, {'height': height})
                        before = point['before']
                        # A real CDP touch pan, not a synthetic wheel: the
                        # phone gesture must move the flyout's own scroller.
                        end_y = max(8, point['y'] - 160)
                        await touch_cdp.send('Input.dispatchTouchEvent', {
                            'type': 'touchStart',
                            'touchPoints': [{'x': point['x'], 'y': point['y'], 'id': 1,
                                             'radiusX': 1, 'radiusY': 1, 'force': 1}],
                        })
                        for step in range(1, 9):
                            y = point['y'] + (end_y - point['y']) * step / 8
                            await touch_cdp.send('Input.dispatchTouchEvent', {
                                'type': 'touchMove',
                                'touchPoints': [{'x': point['x'], 'y': y, 'id': 1,
                                                 'radiusX': 1, 'radiusY': 1, 'force': 1}],
                            })
                            await touch_page.wait_for_timeout(16)
                        await touch_cdp.send('Input.dispatchTouchEvent', {
                            'type': 'touchEnd', 'touchPoints': [],
                        })
                        await touch_page.wait_for_timeout(220)
                        state = await touch_page.evaluate("""() => {
                            const fly = document.getElementById('hdr-actions');
                            const menu = document.getElementById('theme-menu');
                            const list = document.getElementById('theme-list');
                            return {
                                scrollTop: fly.scrollTop,
                                scrollable: fly.scrollHeight > fly.clientHeight + 1,
                                menuOverflow: getComputedStyle(menu).overflowY,
                                listOverflow: getComputedStyle(list).overflowY,
                                listScrollTop: list.scrollTop,
                                menuOpen: !menu.hidden,
                            };
                        }""")
                        require(state['menuOpen'] and state['scrollable'], state)
                        require(state['scrollTop'] > before,
                                {'height': height, 'before': before, **state})
                        require(state['menuOverflow'] in ('visible', 'clip') and
                                state['listOverflow'] == 'visible', state)
                        footer_state = await touch_page.evaluate("""async () => {
                            const fly = document.getElementById('hdr-actions');
                            fly.scrollTop = fly.scrollHeight;
                            await new Promise(r => requestAnimationFrame(r));
                            const footer = document.querySelector('#theme-menu .theme-ft')
                                .getBoundingClientRect();
                            const box = fly.getBoundingClientRect();
                            return {
                                footerVisible: footer.top >= box.top - 1 &&
                                    footer.bottom <= box.bottom + 1 &&
                                    footer.bottom <= innerHeight + 1,
                                resetVisible: document.getElementById('theme-reset')
                                    .getBoundingClientRect().bottom <= innerHeight + 1,
                            };
                        }""")
                        require(footer_state['footerVisible'] and footer_state['resetVisible'],
                                {'height': height, **footer_state})
                        await touch_page.locator(
                            f'[data-theme-id="{palette}"]').scroll_into_view_if_needed()
                        await touch_page.locator(f'[data-theme-id="{palette}"]').click()
                        applied = await touch_page.evaluate(
                            'document.documentElement.dataset.theme')
                        require(applied == palette, {'height': height, 'theme': applied})
                        # A reopened tray (and its flyout) brings the saved
                        # palette into view even after a deep scroll. Use
                        # scripted clicks so Playwright's scroll-into-view does
                        # not move the flyout for us.
                        reopened = await touch_page.evaluate("""async (palette) => {
                            const fly = document.getElementById('hdr-actions');
                            fly.scrollTop = fly.scrollHeight;
                            await new Promise(r => requestAnimationFrame(r));
                            document.getElementById('theme-close').click();
                            document.getElementById('btn-theme').dispatchEvent(
                                new MouseEvent('click', {bubbles: true, detail: 1}));
                            await new Promise(r => requestAnimationFrame(r));
                            const checked = document.querySelector(
                                `[data-theme-id="${palette}"]`).getBoundingClientRect();
                            const flyBox = fly.getBoundingClientRect();
                            return {
                                flyScrollTop: fly.scrollTop,
                                menuScrollTop: document.getElementById('theme-menu').scrollTop,
                                checkedVisible: checked.top >= flyBox.top - 1 &&
                                    checked.bottom <= flyBox.bottom + 1,
                            };
                        }""", palette)
                        require(reopened['checkedVisible'],
                                {'height': height, **reopened})
                        touch_checks.append({'height': height, 'before': before,
                                             'applied': applied, 'reopened': reopened,
                                             **state, **footer_state})
                    await touch_page.set_viewport_size({'width': 1024, 'height': 768})
                    await touch_page.evaluate(
                        'new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))')
                    coarse_desktop = await touch_page.evaluate("""() => {
                        const rect = document.getElementById('btn-theme').getBoundingClientRect();
                        return {
                            coarse: matchMedia('(pointer: coarse)').matches,
                            width: rect.width,
                            height: rect.height,
                            navVisible: getComputedStyle(document.getElementById('btn-nav')).display !== 'none',
                            panes: getComputedStyle(document.querySelector('.theme-panes')).display,
                            caption: getComputedStyle(document.querySelector('.theme-caption-bar')).display,
                            previewCaption: getComputedStyle(
                                document.getElementById('theme-preview-label')).display,
                        };
                    }""")
                    require(coarse_desktop['coarse'] and
                            abs(coarse_desktop['width'] - 44) <= 0.5 and
                            abs(coarse_desktop['height'] - 44) <= 0.5 and
                            not coarse_desktop['navVisible'], coarse_desktop)
                    require(coarse_desktop['panes'] == 'none' and
                            coarse_desktop['previewCaption'] == 'none' and
                            coarse_desktop['caption'] != 'none', coarse_desktop)
                    results.append({'theme_touch': {**touch_state,
                                                   'coarse_desktop': coarse_desktop,
                                                   'viewport_checks': touch_checks}})
                finally:
                    await touch_context.close()
                await page.reload(wait_until='domcontentloaded')
                await page.wait_for_function('chartAgg && explorerAgg && lastData')
                require(await page.locator('#chart-preset').input_value() == 'latency', 'saved chart preset lost')
                require(await page.locator('#chart-pct').input_value() == '99', 'saved chart percentile lost')
                storm_checks = []
                for viewport in ({'width': 1440, 'height': 1000}, {'width': 390, 'height': 844}):
                    await page.set_viewport_size(viewport)
                    state = await page.evaluate('''async () => {
                        const fixture = {enabled:true,banner_enabled:true,storms:Array.from({length:8}, (_, i) => ({
                            provider:'neutral-provider-with-a-long-scope-name.example',
                            model:i ? 'neutral-model-with-a-long-name/' + i : '',scope:i ? 'model' : 'provider',
                            state:i ? 'half_open' : 'open',reason:i ? 'transport' : 'http_503',quota:false,
                            error_percent:75,error_requests:9,failures:15,samples:20,window_ms:60000,queued:4,
                            active_models:3,affected_models:2,affected_model_percent:200/3,
                            retry_at:'2026-09-05T12:00:00Z',recovery_successes:i ? 1 : 0,recovery_required:2,
                        }))};
                        window.browserStormFixture=fixture;
                        applyStormState(fixture);
                        const banner=document.querySelector('#storm-banner');
                        banner.focus();
                        await new Promise(requestAnimationFrame);
                        banner.scrollTop=30;
                        const row=banner.firstElementChild;
                        applyStormState(fixture);
                        const rect=banner.getBoundingClientRect();
                        const state={visible:!banner.hidden,rows:banner.querySelectorAll('.storm-row').length,
                            height:rect.height,bounded:rect.height<=Math.min(180,innerHeight*.25)+1,
                            scrollable:banner.scrollHeight>banner.clientHeight,
                            retained:banner.firstElementChild===row && banner.scrollTop===30,
                            focused:document.activeElement===banner,
                            horizontal:banner.scrollWidth>banner.clientWidth+1 || rect.right>innerWidth+1,
                            recovery:banner.textContent.includes('Checking recovery'),
                            attempt_rate:banner.textContent.includes('75% failed attempts'),
                            affected_requests:banner.textContent.includes('9 requests affected')};
                        return state;
                    }''')
                    require(state['visible'] and state['rows'] == 8 and state['height'] > 0 and state['bounded'], state)
                    require(state['scrollable'] and state['retained'] and state['focused'] and not state['horizontal'], state)
                    require(state['recovery'] and state['attempt_rate'] and state['affected_requests'], state)
                    trigger = page.locator('#storm-banner button').first
                    await trigger.focus()
                    await page.keyboard.press('Enter')
                    await page.wait_for_function('!document.querySelector("#storm-dialog").hidden')
                    await page.wait_for_function('document.activeElement?.dataset.operator === "storm-close"')
                    details = await page.evaluate('''() => {
                        const dialog=document.querySelector('#storm-dialog');
                        const facts=Object.fromEntries([...dialog.querySelectorAll('dt')].map(node=>[node.textContent,node.nextElementSibling.textContent]));
                        const rect=dialog.querySelector('.storm-dialog-panel').getBoundingClientRect();
                        return {facts,modal:dialog.getAttribute('role')==='dialog' && document.querySelector('main').inert,
                            bounded:rect.left>=0 && rect.right<=innerWidth && rect.top>=0 && rect.bottom<=innerHeight,
                            horizontal:dialog.scrollWidth>dialog.clientWidth+1};
                    }''')
                    require(details['modal'] and details['bounded'] and not details['horizontal'], details)
                    require(details['facts']['Requests affected'] == '9' and details['facts']['Failed upstream attempts'] == '15', details)
                    require(details['facts']['Sampled upstream attempts'] == '20' and details['facts']['Queued requests'] == '4', details)
                    require(details['facts']['Affected active models'] == '2 / 3 (66.7%)' and details['facts']['Next retry'], details)
                    await page.keyboard.press('Tab')
                    require(await page.evaluate('document.activeElement?.dataset.operator === "storm-close"'), 'incident modal lost its focus trap')
                    updated = await page.evaluate('''() => {
                        const close=document.activeElement;
                        browserStormFixture.storms[0]={...browserStormFixture.storms[0],queued:7,error_requests:10,state:'half_open',recovery_successes:1};
                        applyStormState(browserStormFixture);
                        const facts=Object.fromEntries([...document.querySelectorAll('#storm-dialog dt')].map(node=>[node.textContent,node.nextElementSibling.textContent]));
                        return document.activeElement===close && facts['Queued requests']==='7' && facts['Requests affected']==='10' && facts['Successful recovery probes']==='1 / 2';
                    }''')
                    require(updated, 'incident modal did not refresh or displaced keyboard focus')
                    await page.keyboard.press('Escape')
                    require(await page.evaluate('document.querySelector("#storm-dialog").hidden && document.activeElement===document.querySelector("#storm-banner button")'), 'incident Escape did not restore trigger focus')
                    await trigger.click()
                    await page.wait_for_function('document.activeElement?.dataset.operator === "storm-close"')
                    await page.evaluate('applyStormState({...browserStormFixture,storms:[]})')
                    require(await page.locator('#storm-dialog').is_visible(), 'incident resolved by unexpectedly closing focused dialog')
                    require('no longer active' in await page.locator('#storm-dialog').inner_text(), 'incident retained stale recovery counts')
                    await page.locator('#storm-dialog [data-operator="storm-close"]').click()
                    require(await page.evaluate('''() => {
                        const el = document.activeElement;
                        return document.querySelector('#storm-banner').hidden && el &&
                          el.getClientRects().length > 0 && (el.id === 'btn-nav' || el.id === 'btn-settings');
                    }'''), 'resolved incident did not restore visible focus')
                    await page.evaluate('delete window.browserStormFixture')
                    state['details'] = details
                    state['keyboard_and_live_update'] = True
                    storm_checks.append({'width': viewport['width'], **state})
                require(not errors, errors)
                print(json.dumps({'canvas_checks': results, 'layout_checks': layout_checks, 'storm_checks': storm_checks, 'saved_view': True, 'browser_errors': errors}))
            except Exception:
                if screenshot and page is not None and not page.is_closed():
                    await page.screenshot(path=screenshot)
                raise
            finally:
                # Only this run's unique synthetic client is removed. The
                # server's purge fence handles pending asynchronous writes.
                try:
                    if cleanup_needed:
                        cleanup = await page.request.post(base + '/admin/purge', max_redirects=0, data={'client': label}, headers=operator_headers())
                        if not cleanup.ok:
                            raise RuntimeError('fixture cleanup failed: ' + await cleanup.text())
                finally:
                    await browser.close()
    finally:
        upstream.shutdown()
        upstream.server_close()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--target', type=target, required=True)
    parser.add_argument('--screenshot', help='optional graph screenshot output path')
    args = parser.parse_args()
    asyncio.run(check(args.target, args.screenshot))

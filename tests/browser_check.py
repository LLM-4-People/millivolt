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
BODY = (
    b'data: {"choices":[{"delta":{"content":"fixture"},"finish_reason":null}]}\n\n'
    b'data: {"choices":[{"delta":{},"finish_reason":"stop"}],'
    b'"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.001}}\n\n'
    b'data: [DONE]\n\n'
)
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
        self.send_header('Content-Length', str(len(BODY)))
        self.end_headers()
        # Fixture delay guarantees a measurable positive TTFT on fast hosts.
        time.sleep(0.02)
        self.wfile.write(BODY)

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
                            const bucket = chartAgg.buckets.find(b => b.tps.every(Number.isFinite) && b.ttft.every(Number.isFinite));
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
                            const five = Array.from({length:5}, (_, k) => ({...bucket,
                                t: bucket.t + k*bm,
                                tps: bucket.tps.map(v => v * (1 + k/10)),
                                ttft: bucket.ttft.map(v => v + k)}));
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
                            return {sparseBlank, sparseH, mountedH, points:_up.data[0].length, speed, latency, overflow,
                                    legend:document.querySelector('#traffic-legend').textContent};
                        }''')
                        require(state['sparseBlank'], state)
                        require(state['sparseH'] == state['mountedH'], state)
                        require(state['points'] == 5 and state['speed'] and state['latency'], state)
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
                    const measured = full.buckets.filter(b => Number.isFinite(b.tps?.[idx]) && Number.isFinite(b.ttft?.[idx]));
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
                # ever mounts (no uPlot, no blank canvas) and the card
                # carries tiles-only so the tiles take the space. The
                # recovered-429 fixture proves the health invariant end to
                # end: the health pair reads 0 errors / 2 rate limited in the
                # chart's two health colors. Tiles toggle like legend
                # buttons: click hides a tile to a label-only stub (grid cell
                # kept), persists in dash.chart, and a reload restores the
                # selection. No percentile selector, no visible pXX label.
                await page.select_option('#chart-preset', 'overview')
                for viewport in ({'width': 1440, 'height': 1000}, {'width': 390, 'height': 844}):
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
                        return {tilesOnly: card.classList.contains('tiles-only'),
                                plot: !!_up, blank: !!document.querySelector('#chart-traffic canvas.chart-blank'),
                                wrapHidden: getComputedStyle(document.getElementById('chart-traffic')).display === 'none',
                                overflow, rlTotal, errTotal,
                                errors0:errV && errV.textContent === '0',
                                rateLimited2:rlV && rlV.textContent === '2',
                                tiles:document.querySelectorAll('#chart-totals .chart-total').length,
                                sparks:document.querySelectorAll('#chart-totals svg.spark').length,
                                pctHidden:document.getElementById('chart-pct').hidden,
                                legend:document.querySelector('#traffic-legend').textContent};
                    }""")
                    require(state['tilesOnly'] and not state['plot'] and not state['blank'] and state['wrapHidden'], state)
                    require(state['rlTotal'] == 2 and state['errTotal'] == 0, state)
                    require(state['errors0'] and state['rateLimited2'], state)
                    require(not state['overflow'], state)
                    require(state['tiles'] == 7, state)
                    require(state['sparks'] == 7, state)
                    require(state['pctHidden'], state)
                    require(not any(p in state['legend'] for p in ('p50', 'p95', 'p99')), state)
                    # Tile toggle contract on the real DOM: click hides the
                    # tile to a label-only stub, the grid cell count is
                    # stable, and the choice persists in dash.chart.
                    toggle = await page.evaluate("""async () => {
                        const before = document.querySelectorAll('#chart-totals .chart-total').length;
                        document.querySelector('[data-tile="tokens"]').click();
                        await new Promise(requestAnimationFrame);
                        const stub = document.querySelector('[data-tile="tokens"]');
                        const after = document.querySelectorAll('#chart-totals .chart-total').length;
                        const saved = JSON.parse(localStorage.getItem('dash.chart') || '{}');
                        return {before, after, off: stub.classList.contains('off'),
                                pressed: stub.getAttribute('aria-pressed') === 'false',
                                persisted: (saved.hidden && saved.hidden.overview || []).join() === 'tokens'};
                    }""")
                    require(toggle['before'] == toggle['after'] == 7 and toggle['off'] and toggle['pressed'] and toggle['persisted'], toggle)
                    await page.evaluate('document.querySelector("[data-tile=\'tokens\']").click()')
                    results.append({'width': viewport['width'], 'preset': 'overview', **state})
                # Restore the saved-view expectations the reload check pins.
                await page.select_option('#chart-preset', 'latency')
                await page.select_option('#chart-pct', '99')
                layout_checks = []
                for viewport in ({'width': 1440, 'height': 1000}, {'width': 1706, 'height': 810}):
                    await page.set_viewport_size(viewport)
                    state = await page.evaluate('''() => {
                        const explorer = document.querySelector('#explorer');
                        const pair = document.querySelector('.grid-pair');
                        const footer = document.querySelector('footer');
                        const er = explorer.getBoundingClientRect();
                        const pr = pair.getBoundingClientRect();
                        const fr = footer.getBoundingClientRect();
                        const locked = getComputedStyle(document.documentElement)
                            .getPropertyValue('--gallery-locked').trim();
                        const explorerMax = parseFloat(getComputedStyle(explorer).maxHeight);
                        return {
                            locked,
                            pageScrollY: document.documentElement.scrollHeight > innerHeight + 1,
                            pageScrollX: document.documentElement.scrollWidth > innerWidth + 1,
                            explorerH: Math.round(er.height),
                            explorerMax,
                            pairH: Math.round(pr.height),
                            footerBottom: Math.round(fr.bottom),
                            inView: er.top >= 0 && pr.top >= 0 && fr.top >= 0
                                && er.bottom <= innerHeight + 1 && pr.bottom <= innerHeight + 1
                                && fr.bottom <= innerHeight + 1,
                        };
                    }''')
                    require(state['locked'] == '1', state)
                    require(not state['pageScrollY'] and not state['pageScrollX'], state)
                    require(state['explorerMax'] <= 240, state)
                    require(state['explorerH'] <= state['explorerMax'] + 1, state)
                    require(state['pairH'] > state['explorerH'], state)
                    require(state['inView'], state)
                    layout_checks.append({'width': viewport['width'], 'height': viewport['height'], **state})
                if screenshot:
                    await page.locator('.traffic-card').screenshot(path=screenshot)
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
                            state:i ? 'half_open' : 'open',reason:i ? 'transport' : 'http_503',
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

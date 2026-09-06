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

from .support import require

DEV_DIR = Path('/tmp/millivolt')  # Reserved dev.sh namespace, never arbitrary data.
BODY = (
    b'data: {"choices":[{"delta":{"content":"fixture"},"finish_reason":null}]}\n\n'
    b'data: {"choices":[{"delta":{},"finish_reason":"stop"}],'
    b'"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.001}}\n\n'
    b'data: [DONE]\n\n'
)


class Upstream(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', '0')))
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
                await page.goto(base + '/#client=' + label, wait_until='domcontentloaded')
                await page.wait_for_function('chartAgg && explorerAgg && lastData')
                await page.select_option('#chart-preset', 'latency')
                await page.wait_for_function('_up && chartAgg.tps_p.some(v => v != null)')
                results = []
                for viewport in ({'width': 1440, 'height': 1000}, {'width': 390, 'height': 844}):
                    await page.set_viewport_size(viewport)
                    for pct in ('50', '95', '99'):
                        await page.select_option('#chart-pct', pct)
                        # Use one actual server bucket to pin the singleton
                        # case independently of a calendar-minute boundary.
                        state = await page.evaluate('''async () => {
                            const bucket = chartAgg.buckets.find(b => b.tps.every(Number.isFinite) && b.ttft.every(Number.isFinite));
                            if (!bucket) throw new Error('fixture has no percentile-bearing bucket');
                            chartAgg = {...chartAgg, buckets:[bucket], from_ms:bucket.t, now_ms:bucket.t+chartAgg.bucket_ms};
                            renderChart();
                            // uPlot commits setData in a microtask; inspect
                            // the accepted frame, not its previous canvas.
                            await new Promise(requestAnimationFrame);
                            const pixels = _up.ctx.getImageData(0,0,_up.ctx.canvas.width,_up.ctx.canvas.height).data;
                            let speed=0, latency=0;
                            for (let i=0;i<pixels.length;i+=4) {
                                if (!pixels[i+3]) continue;
                                if (pixels[i+1]>pixels[i]*1.3 && pixels[i+1]>pixels[i+2]*1.1) speed++;
                                if (pixels[i+2]>pixels[i]*1.3 && pixels[i+2]>pixels[i+1]*1.1) latency++;
                            }
                            const card=document.querySelector('.traffic-card');
                            const overflow=[...card.querySelectorAll('*')].filter(e=>e.clientWidth && e.scrollWidth>e.clientWidth+2).map(e=>e.id||e.className);
                            return {speed,latency,overflow, legend:document.querySelector('#traffic-legend').textContent};
                        }''')
                        require(state['speed'] and state['latency'], state)
                        require(not state['overflow'], state)
                        require(not any(p in state['legend'] for p in ('p50', 'p95', 'p99')), state)
                        results.append({'width': viewport['width'], 'pct': pct, **state})
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
                    require(await page.evaluate('document.activeElement===document.querySelector("#btn-settings") && document.querySelector("#storm-banner").hidden'), 'resolved incident did not restore visible focus')
                    await page.evaluate('delete window.browserStormFixture')
                    state['details'] = details
                    state['keyboard_and_live_update'] = True
                    storm_checks.append({'width': viewport['width'], **state})
                require(not errors, errors)
                print(json.dumps({'canvas_checks': results, 'storm_checks': storm_checks, 'saved_view': True, 'browser_errors': errors}))
            except Exception:
                if screenshot and page is not None and not page.is_closed():
                    await page.screenshot(path=screenshot)
                raise
            finally:
                # Only this run's unique synthetic client is removed. The
                # server's purge fence handles pending asynchronous writes.
                try:
                    if cleanup_needed:
                        cleanup = await page.request.post(base + '/metrics/purge', max_redirects=0, data={'client': label})
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

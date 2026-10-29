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
import uuid
from http.server import ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

from playwright.async_api import async_playwright

from .browser_chart import chart_checks, focus_checks
from .browser_drawer import drawer_checks
from .browser_fixture import BODY, BODY_429, BODY_HEAD, BODY_TAIL, DEV_DIR, Upstream
from .browser_log import log_checks
from .browser_storm import storm_checks as run_storm_checks
from .browser_theme import theme_checks
from .support import DOCS_CAPTURE_VIEWPORT, operator_headers, operator_signin, require


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
                layout_checks = []
                await chart_checks(page, screenshot, results, layout_checks)

                # The published docs capture leaves headroom for the request
                # table's learned content widths and font variation. The table
                # is content-sized, not a fixed budget.
                await page.set_viewport_size(DOCS_CAPTURE_VIEWPORT)
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

                await focus_checks(page, results)
                await log_checks(page, results)
                await drawer_checks(page, results)

                await theme_checks(page, browser, base, errors, results, browser_route)

                await page.reload(wait_until='domcontentloaded')
                await page.wait_for_function('chartAgg && explorerAgg && lastData')
                require(await page.locator('#chart-preset').input_value() == 'latency', 'saved chart preset lost')
                require(await page.locator('#chart-pct').input_value() == '99', 'saved chart percentile lost')
                storm_checks = []
                await run_storm_checks(page, errors, storm_checks)
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

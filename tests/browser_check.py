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
from .browser_fixture import BODY, BODY_429, BODY_HEAD, BODY_TAIL, DEV_DIR, Upstream
from .browser_log import log_checks
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

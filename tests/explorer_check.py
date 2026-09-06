#!/usr/bin/env python3
"""Isolated HTTP + real-browser lineage/429 regression; never manages a proxy.

Run only after scripts/dev.sh has started the desired private dev binary.
Reuses browser_check's canonical target/database/browser guards. Starts one
loopback mock only AFTER the private-config check, creates a unique client,
and purges only that synthetic client in finally. No operator setting changes.
SSE is disabled by the shared route guard: this exercises HTTP/bootstrap/poll.
The separate --docs-history mode publishes only a revalidated derived history
copy using read-only browser requests; it never creates or purges fixtures.
"""

import argparse
import asyncio
import hashlib
import json
import threading
import tempfile
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import quote, urlencode, urlsplit

from playwright.async_api import async_playwright

from scripts.backup_db import verify_docs_copy
from . import browser_check as guards
from .browser_check import require
from .support import ROOT, operator_headers, operator_signin

ORDER = ['provider', 'model', 'client', 'conversation', 'tool', 'time', 'status', 'error', 'key']
KEY = 'local-lineage-fixture-only'
ANSWER = json.dumps({
    'choices': [{'message': {'role': 'assistant', 'content': 'fixture answer',
                            'tool_calls': [{'id': 'fixture-call', 'type': 'function',
                                            'function': {'name': 'fixture_tool', 'arguments': '{}'}}]},
                 'finish_reason': 'tool_calls'}],
    'usage': {'prompt_tokens': 5, 'completion_tokens': 3, 'total_tokens': 8, 'cost': 0.0001},
}, separators=(',', ':')).encode()


def navigation(base, filters, dim='conversation'):
    pairs = [*filters, ('by', dim)]
    return base + '/#/' + '/'.join(quote(str(part), safe='') for pair in pairs for part in pair)


class FixtureUpstream(BaseHTTPRequestHandler):
    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        try:
            payload = json.loads(raw)
            case = payload['model']
            require(case in ('fixture-success', 'fixture-retry429', 'fixture-retry500', 'fixture-final429'), 'unexpected fixture model')
            require(not self.headers.get('X-Proxy-Session') and not self.headers.get('X-Proxy-Parent-Session'), 'proxy-only relationship header leaked')
            with self.server.fixture_lock:
                self.server.calls[case] = self.server.calls.get(case, 0) + 1
                attempt = self.server.calls[case]
            status, body = 200, ANSWER
            if case == 'fixture-retry429' and attempt == 1:
                status = 429
                body = b'{"error":{"type":"rate_limit_error","message":"fixture temporary limit"}}'
            elif case == 'fixture-retry500' and attempt == 1:
                status = 500
                body = b'{"error":{"type":"fixture_error","code":"fixture_transient","message":"fixture recovered failure"}}'
            elif case == 'fixture-final429':
                # A documented nonretryable quota class gives a deterministic
                # final 429 without changing the proxy's retry configuration.
                status = 429
                body = b'{"error":{"type":"insufficient_quota","message":"fixture quota boundary"}}'
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            if status in (429, 500):
                self.send_header('Retry-After', '1')
            self.end_headers()
            self.wfile.write(body)
        except Exception as error:
            with self.server.fixture_lock:
                self.server.errors.append(str(error))
            self.send_error(500)

    def log_message(self, *args):
        pass


async def explorer(request, base, label, dim='conversation', extra=()):
    query = urlencode([('dim', dim), ('f', 'client:' + label), *[('f', key + ':' + value) for key, value in extra]])
    response = await request.get(base + '/metrics/agg/explorer?' + query, max_redirects=0)
    try:
        require(response.status == 200, ('explorer status', response.status))
        return await response.json()
    finally:
        await response.dispose()


async def wait_payload(request, base, label, expected):
    for _ in range(100):
        payload = await explorer(request, base, label)
        if payload.get('scope', {}).get('matches') == expected:
            return payload
        await asyncio.sleep(0.05)
    raise RuntimeError('fixture accounting did not reach ' + str(expected))


async def accepted_view(page, label, dim, matches):
    await page.wait_for_function('''([client,dim,n]) => typeof explorerAgg !== 'undefined' &&
        explorerAgg?.dim === dim && explorerAgg.scope?.matches === n &&
        explorerState().filters.some(f => f.dim === 'client' && f.id === client)''',
        arg=[label, dim, matches], timeout=15000)
    await page.wait_for_function('''() => document.querySelector('#xp-gallery .xp-node') &&
        !document.querySelector('#xp-gallery .empty')''', timeout=15000)


def docs_snapshot(document, client, count):
    """Fail closed before publishing images: only this fixture may be visible.

    A private path can still contain copied operator history. Global totals,
    complete rows and pending state must agree, not merely the selected scope.
    Go encodes empty snapshot slices as null; missing fields are not empty.
    """
    require(isinstance(document, dict), 'missing documentation snapshot')
    for key in ('records', 'in_flight_records'):
        require(key in document and (document[key] is None or isinstance(document[key], list)),
                'invalid documentation snapshot rows: ' + key)
    rows = document['records'] or []
    require(len(rows) == count and all(isinstance(row, dict) and row.get('client') == client for row in rows),
            'documentation snapshot contains non-fixture or incomplete history')
    require(not document['in_flight_records'], 'documentation snapshot has pending requests')
    for section, key, expected in (('kpi', 'requests', count), ('kpi', 'in_flight', 0), ('storage', 'dropped', 0)):
        value = document.get(section)
        require(isinstance(value, dict) and type(value.get(key)) is int and value[key] == expected,
                'documentation snapshot has unexpected ' + section + '.' + key)


async def docs_preflight(request, base, client, count):
    response = await request.get(base + '/metrics/bootstrap', max_redirects=0)
    try:
        require(response.status == 200, ('documentation preflight status', response.status))
        docs_snapshot(await response.json(), client, count)
    finally:
        await response.dispose()


def docs_directory(value):
    output = Path(value).absolute()
    require(output.resolve() == output and (not output.exists() or output.is_dir()),
            'documentation output must be a direct directory, without symlinked ancestors')
    return output


async def capture_image(page, destination, selector=None):
    docs_directory(destination.parent)
    require(not destination.is_symlink(), 'refusing a symlinked documentation image')
    destination.parent.mkdir(parents=True, exist_ok=True)
    # Use actual responsive layout and wait for font/canvas commits. No CSS,
    # chart arrays or record values are changed to manufacture a prettier view.
    await page.evaluate('''async () => {
        await document.fonts.ready;
        await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    }''')
    subject = page.locator(selector) if selector else page
    options = {'path': str(destination), 'type': 'png', 'scale': 'css', 'animations': 'disabled'}
    if not selector:
        options['full_page'] = True
    await subject.screenshot(**options)


def publish_images(staging, output):
    """Validate the complete nested gallery before replacing any public image."""
    output = docs_directory(output)
    snapshots = tuple(sorted(staging.rglob('*.png')))
    require(snapshots, 'documentation capture produced no images')
    for source in snapshots:
        require(source.is_file() and not source.is_symlink(), 'invalid staged image')
        destination = output / source.relative_to(staging)
        docs_directory(destination.parent)
        require(not destination.is_symlink() and (not destination.exists() or destination.is_file()),
                'invalid documentation image destination')
    for source in snapshots:
        destination = output / source.relative_to(staging)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(source.read_bytes())
    return len(snapshots)


async def history_route(route, base, errors):
    # Documentation capture cannot submit inference or operator mutations.
    if route.request.method not in ('GET', 'HEAD'):
        errors.append('documentation capture attempted a non-read request')
        await route.abort()
        return
    path = urlsplit(route.request.url).path
    # GET is not enough: the proxy catch-all can forward GET model discovery
    # upstream. Only dashboard resources and the reads used by this workflow
    # are admitted. The shared guard still enforces origin/redirect/SSE rules.
    if path not in ('/', '/favicon.ico', '/admin/config', '/admin/restart',
                    '/metrics/bootstrap', '/metrics/agg/chart', '/metrics/agg/explorer',
                    '/metrics/agg/log', '/metrics/live/stream') and not (
            path.startswith('/dash/') and path.endswith(('.js', '.css'))
            and all(part not in ('', '.', '..') for part in path[1:].split('/'))
            and '%' not in path and '\\' not in path):
        errors.append('documentation capture attempted a non-dashboard resource')
        await route.abort()
        return
    await guards.browser_route(route, base, errors)


async def capture_history(args):
    """Capture only an explicitly derived, revalidated history copy.

    This is separate from the synthetic regression above: no fixture requests,
    purge, configuration changes, raw history, or failure images are produced.
    """
    base = guards.target(args.target)
    output = docs_directory(args.docs_history)
    errors = []
    async with async_playwright() as playwright:
        browser = await playwright.chromium.launch()
        try:
            context = await browser.new_context(viewport={'width': 1440, 'height': 1000}, service_workers='block')
            await context.route('**/*', lambda route: history_route(route, base, errors))
            await operator_signin(context, base)
            response = await context.request.get(base + '/admin/config', max_redirects=0)
            try:
                require(response.status == 200, 'documentation configuration preflight failed')
                document = await response.json()
            finally:
                await response.dispose()
            guards.private_config(document, base)
            public_config = ROOT / 'proxy.example.yaml'
            require(Path(document['path']).read_bytes() == public_config.read_bytes(),
                    'history capture requires the public example, never a private operator config')
            database = Path(document['effective']['db_path'])
            summary = verify_docs_copy(database)
            require(summary['records'] > 0, 'history capture requires recorded requests')
            page = await context.new_page()
            page.on('pageerror', lambda error: errors.append(str(error)))

            async def ready(dim):
                await page.wait_for_function('''([dim,n]) => explorerAgg?.dim === dim &&
                    explorerAgg.scope?.matches === n && kpiAgg?.requests === n &&
                    chartAgg?.buckets.reduce((sum,b) => sum+b.req,0) === n &&
                    lastData && _up && !_renderQueued && !_renderDirty''',
                    arg=[dim, summary['records']], timeout=30000)
                require(await page.evaluate('''() => kpiInFlight() === 0 &&
                    !lastData.in_flight_records?.length && storageState?.dropped === 0'''),
                    'history capture has pending work or storage loss')
                require(not errors, errors)

            # Stage every image outside the repository. A failed guard leaves
            # the published gallery unchanged, and no failure image is retained.
            with tempfile.TemporaryDirectory(prefix='millivolt-docs-images-') as temporary:
                staging = Path(temporary)
                await page.goto(navigation(base, [], 'provider'), wait_until='domcontentloaded')
                await page.select_option('#chart-window', 'all')
                await ready('provider')
                await capture_image(page, staging / 'overview/dashboard.png')
                # The natural-height desktop layout leaves room for the full
                # dimension rail in focused explorer captures.
                await page.set_viewport_size({'width': 1440, 'height': 900})
                await ready('provider')
                await capture_image(page, staging / 'explorer/providers.png', '#explorer')

                await page.goto(navigation(base, [], 'model'), wait_until='domcontentloaded')
                await ready('model')
                await capture_image(page, staging / 'explorer/models.png', '#explorer')

                # Below the existing 1200px breakpoint, the chart spans the
                # full content width. This is the app's layout, not injected CSS.
                await page.set_viewport_size({'width': 1180, 'height': 1000})
                for preset, filename in (('traffic', 'traffic.png'), ('errors', 'errors.png'),
                                         ('tokens', 'tokens.png'), ('latency', 'speed-latency.png'),
                                         ('cost', 'cost.png')):
                    await page.select_option('#chart-preset', preset)
                    if preset == 'latency':
                        await page.select_option('#chart-pct', '95')
                    await ready('model')
                    await capture_image(page, staging / 'charts' / filename, '.traffic-card')

                await page.set_viewport_size({'width': 2000, 'height': 900})
                await ready('model')
                require(await page.locator('#tbl-requests').evaluate('el => el.scrollWidth <= el.clientWidth + 2'),
                        'request-table capture would clip columns')
                await capture_image(page, staging / 'requests/table.png', '.grid-pair > .card:last-child')
                await page.set_viewport_size({'width': 1180, 'height': 850})
                request_id = await page.evaluate('''() => {
                    const candidates = logVisible().filter(r => !r.live && !r.debug &&
                        r.usage?.output_tokens > 0 && r.ttft_ms > 0);
                    return (candidates.find(r => r.attempts?.length) || candidates[0])?.id;
                }''')
                require(request_id, 'history has no visible finalized request with usage and timing')
                await page.locator('#tbl-requests tr.exp-row:not(.retry-sub) [data-open-req="' + request_id + '"]').click()
                await page.wait_for_function('''id => drawerId === id &&
                    document.querySelector('#drawer').classList.contains('open')''', arg=request_id)
                require(await page.locator('#drawer-debug').count() == 0, 'documentation drawer contains debug data')
                await ready('model')
                await capture_image(page, staging / 'requests/details.png', '#drawer')
                await page.set_viewport_size({'width': 1180, 'height': 600})
                await page.locator('#drawer-body').evaluate('''body => {
                    const tokens = [...body.querySelectorAll('h4')].find(h => h.textContent === 'Tokens');
                    if (!tokens) throw new Error('request detail has no token section');
                    body.scrollTop += tokens.getBoundingClientRect().top - body.getBoundingClientRect().top;
                }''')
                await capture_image(page, staging / 'requests/performance.png', '#drawer')
                await page.keyboard.press('Escape')
                await page.wait_for_function('() => drawerId === null')

                await page.set_viewport_size({'width': 1440, 'height': 900})
                # Opening these menus is read-only with empty filter controls.
                # The mutation and external-origin guards remain unchanged.
                for kind in ('pause', 'debug', 'limits', 'logs', 'clear', 'restart'):
                    prefix = {'logs': 'lf', 'clear': 'cf'}.get(kind)
                    if prefix:
                        require(await page.evaluate('''prefix =>
                            [...document.querySelectorAll('[id^="' + prefix + '-"]')].every(el => !el.value)''',
                            prefix), 'documentation filter menu must start empty')
                    await page.locator('#btn-' + kind).click()
                    await page.locator('#' + kind + '-menu').wait_for(state='visible')
                    if prefix:
                        require(await page.evaluate('''prefix => !clearFilterActive(filterFromUI(prefix))''', prefix),
                                'documentation filter menu selected a scope')
                    if kind == 'restart':
                        await page.wait_for_function('() => typeof restartServerState.available === "boolean"')
                    await ready('model')
                    await capture_image(page, staging / 'menus' / (kind + '.png'), '#' + kind + '-menu')
                    await page.keyboard.press('Escape')
                    await page.locator('#' + kind + '-menu').wait_for(state='hidden')

                await page.set_viewport_size({'width': 1440, 'height': 720})
                await page.locator('#btn-settings').click()
                for category in ('dashboard', 'queue', 'storage', 'providers'):
                    await page.set_viewport_size({'width': 1440, 'height': 800 if category == 'queue' else 720})
                    await page.locator('#settings-rail [data-st-cat="' + category + '"]').click()
                    await page.wait_for_function('''category => settingsDoc && settingsCat === category &&
                        document.querySelector('#btn-settings-apply').disabled''', arg=category)
                    await ready('model')
                    await capture_image(page, staging / 'settings' / (category + '.png'), '#settings-sheet')
                require(verify_docs_copy(database) == summary, 'history changed while capturing')
                require(Path(document['path']).read_bytes() == public_config.read_bytes(),
                        'configuration changed while capturing')
                require(not errors, errors)
                image_count = publish_images(staging, output)
            print(json.dumps({'history': summary, 'images': image_count, 'browser_errors': errors}))
        finally:
            await browser.close()


async def layout(page):
    return await page.evaluate('''() => {
        const targets = ['#explorer','#xp-rail','#xp-dim-trigger','.xp-rail-item',
            '.xp-node','.xp-node-foot','.xp-node-signals','.xp-conversation-parent'];
        const overflow = [...document.querySelectorAll(targets.join(','))]
            .filter(e => e.clientWidth && e.scrollWidth > e.clientWidth + 2)
            .map(e => ({selector:e.id||e.className, width:e.clientWidth, scroll:e.scrollWidth}));
        return {viewport:innerWidth, documentOverflow:document.documentElement.scrollWidth > innerWidth+2, overflow};
    }''')


async def check(args):
    base = guards.target(args.target)
    label = 'lineage-browser-' + uuid.uuid4().hex
    sessions = {name: name + '-' + uuid.uuid4().hex[:8] for name in ('parent', 'child-one', 'child-two', 'orphan', 'missing')}
    snapshot_dir = Path(args.screenshots) if args.screenshots else None
    if snapshot_dir:
        snapshot_dir.mkdir(parents=True, exist_ok=True)
    upstream = None
    mock_thread = None
    cleanup_needed = False
    results = {'fixture_client': label, 'checks': [], 'browser_errors': []}
    async with async_playwright() as playwright:
        browser = await playwright.chromium.launch()
        context = await browser.new_context(viewport={'width': 1440, 'height': 1000}, service_workers='block')
        await context.route('**/*', lambda route: guards.browser_route(route, base, results['browser_errors']))
        await operator_signin(context, base)
        page = await context.new_page()
        page.on('pageerror', lambda error: results['browser_errors'].append(str(error)))
        try:
            response = await context.request.get(base + '/admin/config', max_redirects=0)
            try:
                require(response.status == 200, ('configuration preflight status', response.status))
                document = await response.json()
                guards.private_config(document, base)
                require(document.get('effective', {}).get('max_retries', 0) >= 1, 'dev config must allow at least one retry')
            finally:
                await response.dispose()
            if args.docs_images:
                await docs_preflight(context.request, base, label, 0)
            upstream = ThreadingHTTPServer(('127.0.0.1', 0), FixtureUpstream)
            upstream.fixture_lock = threading.Lock()
            upstream.calls, upstream.errors = {}, []
            mock_thread = threading.Thread(target=upstream.serve_forever, daemon=True)
            mock_thread.start()

            async def send(session, parent='', case='fixture-success', expected=200):
                nonlocal cleanup_needed
                headers = {'X-Proxy-Base-URL': f'http://127.0.0.1:{upstream.server_port}',
                           'X-Proxy-Key': KEY, 'X-Proxy-Client': label, 'X-Proxy-Session': sessions[session]}
                if parent:
                    headers['X-Proxy-Parent-Session'] = sessions[parent]
                cleanup_needed = True
                response = await context.request.post(base + '/v1/chat/completions', headers=headers,
                    max_redirects=0, timeout=30000,
                    data={'model': case, 'stream': False, 'messages': [{'role': 'user', 'content': 'neutral local fixture'}]})
                try:
                    body = await response.body()
                    require(response.status == expected, ('inference status', case, response.status))
                    if expected == 200:
                        require(body == ANSWER, ('response was not transparent', case))
                finally:
                    await response.dispose()

            await send('parent')
            await send('child-one', 'parent')
            await send('child-two', 'parent', 'fixture-retry429')
            payload = await wait_payload(context.request, base, label, 3)
            require(payload['conversation_summary'] == {'main': 1, 'sub': 2, 'unresolved': 0}, payload['conversation_summary'])
            await page.goto(navigation(base, [('client', label)]), wait_until='domcontentloaded')
            await accepted_view(page, label, 'conversation', 3)
            require(await page.locator('[data-xp-dim="conversation"] .rail-n').text_content() == '1 main + 2 sub', 'initial main/sub rail summary')
            results['checks'].append('HTTP + browser: one observed main and two declared children')

            await send('child-one', 'parent', 'fixture-retry500')
            await send('orphan', 'missing')
            await send('child-two', 'parent', 'fixture-final429', 429)
            payload = await wait_payload(context.request, base, label, 6)
            require(payload['conversation_summary'] == {'main': 1, 'sub': 3, 'unresolved': 0}, payload['conversation_summary'])
            require(payload['rail']['conversation'] == 4, 'missing parent inflated rail count')
            groups = {g['name']: g for g in payload['groups']}
            parent_group = groups['s:' + sessions['parent']]
            first_group = groups['s:' + sessions['child-one']]
            second_group = groups['s:' + sessions['child-two']]
            orphan_group = groups['s:' + sessions['orphan']]
            require((parent_group['err_final'], parent_group['rate_limit_requests']) == (0, 0), 'zero footer counts absent')
            require((first_group['err_final'], first_group['rate_limit_requests']) == (1, 0), 'recovered 500 error accounting')
            require((second_group['n'], second_group['err_final'], second_group['rate_limit_requests']) == (2, 0, 2), 'recovered/final 429 conflated with errors')
            require(orphan_group['conversation']['parent_observed'] is False, 'missing parent fabricated an observation')
            results['checks'].append('HTTP: missing parent, recovered 500, recovered/final 429, zero API counts retained')

            for viewport in ({'width': 1440, 'height': 1000}, {'width': 390, 'height': 844}):
                await page.set_viewport_size(viewport)
                for dim in ORDER:
                    await page.goto(navigation(base, [('client', label)], dim), wait_until='domcontentloaded')
                    await accepted_view(page, label, dim, 6)
                    actual_order = await page.locator('#xp-rail [data-xp-dim]').evaluate_all('(nodes)=>nodes.map(n=>n.dataset.xpDim)')
                    require(actual_order == ORDER, ('category order', actual_order))
                    footers = await page.locator('#xp-gallery .xp-node').evaluate_all('''nodes => {
                        const groups = new Map(explorerAgg.groups.map(g => [g.name,g]));
                        return nodes.map(n => {
                            const g = groups.get(n.dataset.xpKey);
                            const expectedError = g.err_final > 0 ? fmt(g.err_final)+' err' : '';
                            const expectedRate = g.rate_limit_requests > 0 ? fmt(g.rate_limit_requests)+' ×429' : '';
                            return {
                                error:n.querySelector('.xp-node-err')?.textContent || '',
                                rate:n.querySelector('.xp-node-rate')?.textContent || '',
                                text:n.querySelector('.xp-node-signals')?.textContent || '',
                                expectedError, expectedRate,
                                expected:[expectedError,expectedRate].filter(Boolean).join(' · '),
                            };
                        });
                    }''')
                    require(footers and all(f['error'] == f['expectedError'] and f['rate'] == f['expectedRate']
                            and f['text'] == f['expected'] for f in footers), ('health badge visibility or separator', dim, footers))
                    if dim == 'provider':
                        require(len(footers) == 1 and footers[0]['text'] == '1 err · 2 ×429', 'combined health signals')
                    measured = await layout(page)
                    require(not measured['documentOverflow'] and not measured['overflow'], (dim, measured))
                    if dim == 'conversation':
                        roles = await page.locator('.xp-conversation-role').all_text_contents()
                        require(roles.count('main') == 1 and roles.count('sub') == 3, ('roles', roles))
                        orphan = page.locator('.xp-conversation-node').filter(has=page.locator('[data-xp-key="s:' + sessions['orphan'] + '"]'))
                        require('not observed' in await orphan.locator('.xp-conversation-parent').text_content(), 'missing-parent label')
                        require(await page.locator('.xp-node .xp-conversation-parent').count() == 0, 'nested parent link inside card button')
                        main_card = page.locator('[data-xp-key="s:' + sessions['parent'] + '"]')
                        require(await main_card.locator('.xp-node-err, .xp-node-rate').count() == 0, 'zero badges must be absent')
                        failed_card = page.locator('[data-xp-key="s:' + sessions['child-one'] + '"]')
                        require(await failed_card.locator('.xp-node-signals').text_content() == '1 err'
                                and await failed_card.locator('.xp-node-rate').count() == 0, 'error-only browser signal')
                        limited_card = page.locator('[data-xp-key="s:' + sessions['child-two'] + '"]')
                        require(await limited_card.locator('.xp-node-signals').text_content() == '2 ×429'
                                and await limited_card.locator('.xp-node-err').count() == 0, '429-only browser signal')
                        if snapshot_dir:
                            await page.locator('#explorer').screenshot(path=str(snapshot_dir / f'lineage-{viewport["width"]}.png'))
                    results['checks'].append({'width': viewport['width'], 'dimension': dim, 'cards': len(footers), 'overflow': measured['overflow']})

                await page.goto(navigation(base, [('client', label)]), wait_until='domcontentloaded')
                await accepted_view(page, label, 'conversation', 6)
                child = page.locator('.xp-conversation-node').filter(has=page.locator('[data-xp-key="s:' + sessions['child-one'] + '"]'))
                await child.locator('a.xp-conversation-parent').click()
                await accepted_view(page, label, 'conversation', 1)
                filters = await page.evaluate('Object.fromEntries(explorerState().filters.map(f=>[f.dim,f.id]))')
                require(filters == {'client': label, 'key': hashlib.sha256(KEY.encode()).hexdigest(), 'conversation': 's:' + sessions['parent']}, ('parent pivot namespace', filters))
                require((await page.locator('#f-scope').text_content()).startswith('1 of '), 'parent exact-leaf footer count')
                results['checks'].append({'width': viewport['width'], 'parent_pivot': 'exact leaf, one request, full namespace'})

                # Presentation-only replay stresses compact counts without
                # generating hundreds of thousands of fake stored requests.
                high = await page.evaluate('''() => {
                    const prior=explorerAgg;
                    try {
                        explorerAgg={...prior,conversation_summary:{main:123456,sub:234567,unresolved:345678}};
                        renderDimRail(explorerState());
                        const els=[...document.querySelectorAll('[data-xp-dim="conversation"],#xp-dim-trigger')];
                        return els.filter(e=>e.clientWidth).map(e=>({text:e.textContent,width:e.clientWidth,scroll:e.scrollWidth}));
                    } finally { explorerAgg=prior;renderDimRail(explorerState()); }
                }''')
                require(high and all(v['scroll'] <= v['width'] + 2 for v in high), ('high/unresolved count layout', high))
                results['checks'].append({'width': viewport['width'], 'presentation_only_high_counts': high})

            require(not upstream.errors, upstream.errors)
            require(upstream.calls == {'fixture-success': 3, 'fixture-retry429': 2, 'fixture-retry500': 2, 'fixture-final429': 1}, ('upstream attempts', upstream.calls))
            require(not results['browser_errors'], results['browser_errors'])
            if args.docs_images:
                # Reload real server state after presentation-only regressions.
                # Never manufacture chart history or publish failure captures.
                await page.set_viewport_size({'width': 1440, 'height': 1000})
                await page.goto(navigation(base, [('client', label)]), wait_until='domcontentloaded')
                await accepted_view(page, label, 'conversation', 6)
                await page.wait_for_function('''() => chartAgg && lastData && kpiAgg?.requests === 6 &&
                    !_renderQueued && !_renderDirty''')
                await page.evaluate('() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))')
                await docs_preflight(context.request, base, label, 6)
                docs_snapshot(await page.evaluate('() => ({...lastData,kpi:kpiAgg,storage:storageState})'), label, 6)
                require(not results['browser_errors'], results['browser_errors'])
                output = docs_directory(args.docs_images)
                output.mkdir(parents=True, exist_ok=True)
                await capture_image(page, output / 'dashboard.png')
                await capture_image(page, output / 'explorer.png', '#explorer')
                results['checks'].append('documentation images: complete synthetic-only server and browser snapshots')
            results['mock_attempts'] = upstream.calls
            print(json.dumps(results))
        except Exception:
            if snapshot_dir and not page.is_closed():
                await page.screenshot(path=str(snapshot_dir / 'failure.png'), full_page=True)
            raise
        finally:
            try:
                if cleanup_needed:
                    response = await context.request.post(base + '/admin/purge', max_redirects=0, data={'client': label}, headers=operator_headers())
                    try:
                        require(response.status == 200, ('scoped fixture cleanup failed', response.status, label))
                    finally:
                        await response.dispose()
            finally:
                await browser.close()
                if upstream:
                    upstream.shutdown()
                    upstream.server_close()
                    mock_thread.join(timeout=5)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--target', required=True)
    parser.add_argument('--screenshots', help='optional scratch screenshot directory')
    publication = parser.add_mutually_exclusive_group()
    publication.add_argument('--docs-images', help='publish two synthetic fixture images; requires fresh empty dev history')
    publication.add_argument('--docs-history', help='publish the dashboard gallery from an explicitly redacted history copy; no fixture requests or mutations')
    args = parser.parse_args()
    if args.docs_history and args.screenshots:
        parser.error('--docs-history cannot retain diagnostic screenshots')
    asyncio.run(capture_history(args) if args.docs_history else check(args))

import argparse
import asyncio
import copy
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch
from playwright.async_api import async_playwright

from .. import browser_check as fixture
from ..explorer_check import docs_directory, docs_snapshot, history_route, publish_images
from ..browser_check import require
from ..support import ROOT, clip_js

try:
    require(False, 'checks survive optimization')
except RuntimeError:
    pass
else:
    raise RuntimeError('fixture assertions were disabled')

for value in ('http://127.0.0.1:8080', 'http://@127.0.0.1:8081', 'http://127.0.0.1:no', 'http://127.0.0.1:99999', 'http://example.invalid:8081'):
    try:
        fixture.target(value)
    except argparse.ArgumentTypeError:
        pass
    else:
        raise RuntimeError('accepted unsafe target: ' + value)

base = 'http://127.0.0.1:18081'
doc = {'path': '/tmp/millivolt/millivolt-dev-18081.yaml', 'overrides': {'db_path': 'none'}, 'effective': {'db_path': ''}, 'restart_required': []}
fixture.private_config(doc, base)
doc['overrides']['db_path'] = doc['effective']['db_path'] = '/tmp/millivolt/millivolt-dev-safe.db'
fixture.private_config(doc, base)
for database in ('/srv/millivolt/proxy.db', '/tmp/millivolt/sub/millivolt-dev.db', '/tmp/millivolt/other.db', ''):
    doc['overrides']['db_path'] = doc['effective']['db_path'] = database
    try:
        fixture.private_config(doc, base)
    except RuntimeError:
        pass
    else:
        raise RuntimeError('accepted unsafe database: ' + database)
doc['overrides']['db_path'] = 'none'
doc['effective']['db_path'] = ''
doc['restart_required'] = ['db_path']
try:
    fixture.private_config(doc, base)
except RuntimeError:
    pass
else:
    raise RuntimeError('accepted ambiguous database restart')

# Publication must reject copied history, partial snapshots and malformed
# counters even when target/database guards accepted the private instance.
empty = {'records': None, 'in_flight_records': None,
         'kpi': {'requests': 0, 'in_flight': 0}, 'storage': {'dropped': 0}}
docs_snapshot(empty, 'fixture', 0)
complete = {**empty, 'records': [{'client': 'fixture'}], 'kpi': {'requests': 1, 'in_flight': 0}}
docs_snapshot(complete, 'fixture', 1)
for section, key, value in (
        ('kpi', 'requests', 2), ('kpi', 'requests', True), ('kpi', 'requests', '1'),
        ('kpi', 'in_flight', 1), ('storage', 'dropped', 1),
        (None, 'records', [{'client': 'private'}]), (None, 'records', []),
        (None, 'records', {}), (None, 'records', [None]),
        (None, 'in_flight_records', [{'client': 'fixture'}]), (None, 'kpi', None)):
    invalid = copy.deepcopy(complete)
    (invalid[section] if section else invalid)[key] = value
    try:
        docs_snapshot(invalid, 'fixture', 1)
    except RuntimeError:
        pass
    else:
        raise RuntimeError('accepted unsafe documentation snapshot: ' + str((section, key, value)))
for key in empty:
    invalid = copy.deepcopy(empty)
    del invalid[key]
    try:
        docs_snapshot(invalid, 'fixture', 0)
    except RuntimeError:
        pass
    else:
        raise RuntimeError('accepted incomplete documentation snapshot: ' + key)

hits = []

with tempfile.TemporaryDirectory(prefix='millivolt-gallery-guard-') as directory:
    root = Path(directory)
    staging, output = root / 'staging', root / 'output'
    for category in ('charts', 'settings'):
        folder = staging / category
        folder.mkdir(parents=True)
        (folder / 'shared.png').write_bytes(category.encode())
    require(publish_images(staging, output) == 2, 'nested gallery image count changed')
    for category in ('charts', 'settings'):
        require((output / category / 'shared.png').read_bytes() == category.encode(),
                'publication flattened or mixed category folders')
    (staging / 'charts/shared.png').write_bytes(b'changed')
    image = output / 'settings/shared.png'
    image.unlink()
    image.symlink_to(staging / 'settings/shared.png')
    try:
        publish_images(staging, output)
    except RuntimeError:
        pass
    else:
        raise RuntimeError('publication accepted a symlinked nested image')
    require((output / 'charts/shared.png').read_bytes() == b'charts',
            'publication replaced an image before validating every destination')
    image.unlink()
    image.parent.rmdir()
    image.parent.symlink_to(staging / 'settings', target_is_directory=True)
    try:
        publish_images(staging, output)
    except RuntimeError:
        pass
    else:
        raise RuntimeError('publication accepted a symlinked category folder')

# Reject ambiguous publication modes before a browser, network request or
# output directory exists, including with interpreter assertions disabled.
with tempfile.TemporaryDirectory(prefix='millivolt-capture-guard-') as directory:
    output = Path(directory) / 'unpublished'
    require(docs_directory(output) == output, 'direct publication directory was rejected')
    link = Path(directory) / 'linked'
    link.symlink_to(output, target_is_directory=True)
    for unsafe in (link, link / 'nested'):
        try:
            docs_directory(unsafe)
        except RuntimeError:
            pass
        else:
            raise RuntimeError('accepted symlinked publication directory')
    for conflicting in ('--docs-images', '--screenshots'):
        result = subprocess.run(
            [sys.executable, '-B', '-O', '-m', 'tests.explorer_check',
             '--target', base, '--docs-history', str(output), conflicting, str(output)],
            cwd=ROOT, capture_output=True, text=True, timeout=10,
        )
        require(result.returncode == 2 and 'error:' in result.stderr and not output.exists(),
                'ambiguous documentation publication did not fail before capture')


async def history_method_checks():
    with patch.object(fixture, 'browser_route', new_callable=AsyncMock) as delegated:
        for method in ('POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS', 'TRACE', 'CONNECT', 'get'):
            route = SimpleNamespace(request=SimpleNamespace(method=method), abort=AsyncMock())
            errors = []
            await history_route(route, base, errors)
            route.abort.assert_awaited_once_with()
            delegated.assert_not_awaited()
            require(len(errors) == 1, 'non-read documentation request did not report rejection')
        for method in ('GET', 'HEAD'):
            route = SimpleNamespace(request=SimpleNamespace(method=method, url=base + '/'), abort=AsyncMock())
            errors = []
            await history_route(route, base, errors)
            delegated.assert_awaited_once_with(route, base, errors)
            route.abort.assert_not_awaited()
            require(not errors, 'read-only documentation request was rejected')
            delegated.reset_mock()
        for path in ('/v1/models', '/v1/chat/completions', '/metrics/export', '/admin/pause',
                     '/dash/../v1/models', '/dash/%2e%2e/private.js', '/dash//private.js',
                     '/dash/../private.js', '/dash/private.db', '/dash/\\private.js'):
            route = SimpleNamespace(request=SimpleNamespace(method='GET', url=base + path), abort=AsyncMock())
            errors = []
            await history_route(route, base, errors)
            route.abort.assert_awaited_once_with()
            delegated.assert_not_awaited()
            require(len(errors) == 1, 'non-dashboard GET was admitted during capture')


class Receiver(BaseHTTPRequestHandler):
    def do_GET(self):
        hits.append(self.path)
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'foreign origin')
    def log_message(self, *args):
        pass
class Origin(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/redirect':
            self.send_response(302)
            self.send_header('Location', f'http://127.0.0.1:{receiver.server_port}/forbidden')
            self.end_headers()
        else:
            self.send_response(200)
            self.send_header('Content-Type', 'text/html')
            self.end_headers()
            self.wfile.write(b'<title>safe</title>')
    def log_message(self, *args):
        pass
receiver = ThreadingHTTPServer(('127.0.0.1', 0), Receiver)
origin = ThreadingHTTPServer(('127.0.0.1', 0), Origin)
for server in (receiver, origin):
    threading.Thread(target=server.serve_forever, daemon=True).start()

async def clip_predicate_checks(browser):
    """The layout probes' one clip predicate must classify by what a reader sees.

    A scroll box that clips a rendered value is the defect the probes exist to
    catch; an element that paints nothing by design - screen-reader-only text,
    a closed disclosure, a zero-opacity surface - reports the same scroll
    numbers while showing nothing, and is what made a correct chart toolbar fail.
    """
    context = await browser.new_context(service_workers='block')
    try:
        page = await context.new_page()
        await page.set_content("""
          <div id="card">
            <span id="value" style="display:inline-block;max-width:14px;overflow:hidden;
                  white-space:nowrap">1234567890</span>
            <span id="hidden" style="position:absolute;width:1px;height:1px;overflow:hidden;
                  clip:rect(0,0,0,0);clip-path:inset(50%);white-space:nowrap">long label</span>
            <div style="display:none"><span id="gone" style="display:inline-block;max-width:14px;
                  overflow:hidden;white-space:nowrap">1234567890</span></div>
            <div style="opacity:0"><span id="invisible" style="display:inline-block;max-width:14px;
                  overflow:hidden;white-space:nowrap">1234567890</span></div>
            <div style="clip-path:inset(50%)"><span id="under-clip"
                  style="display:inline-block;max-width:14px;overflow:hidden;
                  white-space:nowrap">1234567890</span></div>
          </div>""")
        scan = clip_js("""async () => {
          const ids = clipped(document.getElementById('card')).map(e => e.id).sort();
          return {ids, raw: [...document.querySelectorAll('#card *')]
            .filter(e => e.clientWidth && e.scrollWidth > e.clientWidth + 2)
            .map(e => e.id).sort()};
        }""")
        state = await page.evaluate(scan)
        require(state['ids'] == ['value'], ('clip predicate misclassified', state))
        # The old comparison is what failed a correct page: it must still report
        # these, or this pin would pass on a predicate that reports nothing.
        require('hidden' in state['raw'], ('fixture no longer reproduces the false positive', state))
        require('gone' not in state['ids'] and 'invisible' not in state['ids']
                and 'under-clip' not in state['ids'],
                ('paints-nothing ancestors are not exempt', state))
        return state
    finally:
        await context.close()


async def main():
    await history_method_checks()
    async with async_playwright() as p:
        browser = await p.chromium.launch()
        try:
            clip_state = await clip_predicate_checks(browser)
            context = await browser.new_context(service_workers='block')
            errors = []
            base = f'http://127.0.0.1:{origin.server_port}'
            await context.route('**/*', lambda route: fixture.browser_route(route, base, errors))
            page = await context.new_page()
            await page.goto(base)
            require(await page.title() == 'safe', 'same-origin buffered page failed')
            try:
                await page.goto(base + '/redirect')
            except Exception:
                pass
            require(not hits and any('redirected' in error for error in errors), ('redirect guard failed', hits, errors))
            try:
                await page.goto(f'http://127.0.0.1:{receiver.server_port}/direct')
            except Exception:
                pass
            require(not hits and any('foreign origin' in error for error in errors), ('origin guard failed', hits, errors))
            print('PASS: explicit guards, database namespace/restart, publication directories/modes, read-only dashboard capture, same-origin browser load, redirected/direct foreign browser origins blocked, clip predicate classifies hidden text as invisible')
        finally:
            await browser.close()
try:
    asyncio.run(main())
finally:
    for server in (origin, receiver):
        server.shutdown()
        server.server_close()

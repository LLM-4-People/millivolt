import argparse
import asyncio
import copy
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from playwright.async_api import async_playwright

import browser_check as fixture
from explorer_check import docs_snapshot
from browser_check import require

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

async def main():
    async with async_playwright() as p:
        browser = await p.chromium.launch()
        try:
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
            print('PASS: explicit guards, database namespace/restart, same-origin browser load, redirected/direct foreign browser origins blocked')
        finally:
            await browser.close()
try:
    asyncio.run(main())
finally:
    for server in (origin, receiver):
        server.shutdown()
        server.server_close()

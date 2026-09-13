#!/usr/bin/env python3
"""dev-traffic.py - neutral fixture traffic for the isolated dev instance.

Owned by scripts/dev-traffic.sh (the lifecycle owner: start/stop/status). Do
not launch this driver by hand - the wrapper owns the PID file identity.

Loopback upstream only, never a paid provider. Bursts land roughly one
minute apart so the chart's 1-minute buckets produce a readable multi-bucket
timeline. Fixture semantics mirror tests/browser_check.py's upstream (a
distinct deterministic test fixture) with demo variety:

- plain content  -> SSE 200 with varied in/out/cache/reasoning/cost usage
- 'throttle' content -> first attempt answers 429, the retry succeeds
  (an absorbed attempt: rate-limited, never an error)
- 'bad' content  -> final 400 (an error, distinct from rate limiting)

Stop is SIGTERM-clean: the upstream server shuts down and the driver exits
without finishing the remaining bursts.
"""
import json
import os
import signal
import socket
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BASE = None            # http://host:port of the dev instance (set in main)
UPSTREAM_PORT = None    # loopback upstream port (set in main)
STOP = threading.Event()


def sse_body(content: bytes) -> bytes:
    # Deterministic variety per distinct body: token counts, cache share,
    # reasoning and cost all vary so every overview surface has shape.
    i = sum(content) % 97
    cached = (i * 37) % 160
    in_tok = 110 + (i * 13) % 90
    out_tok = 32 + (i * 7) % 60
    usage = {
        'prompt_tokens': in_tok,
        'completion_tokens': out_tok,
        'total_tokens': in_tok + out_tok,
        'prompt_tokens_details': {'cached_tokens': cached},
        'completion_tokens_details': {'reasoning_tokens': (i % 3) * (i % 5 + 1) * 4},
        'cost': round(0.0008 + i * 0.00021, 6),
    }
    return (
        'data: {"choices":[{"delta":{"content":"fixture"},"finish_reason":null}]}\n\n'
        'data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":' + json.dumps(usage) + '}\n\n'
        'data: [DONE]\n\n'
    ).encode()


class Upstream(BaseHTTPRequestHandler):
    _throttle_hits = {}

    def do_POST(self):
        length = int(self.headers.get('Content-Length', '0'))
        body = self.rfile.read(length)
        time.sleep(0.03)  # guarantees a measurable positive TTFT
        if b'bad' in body:
            err = b'{"error":{"message":"bad request","type":"invalid_request_error","code":400}}'
            self._send(400, 'application/json', err)
            return
        if b'throttle' in body:
            hits = Upstream._throttle_hits[body] = Upstream._throttle_hits.get(body, 0) + 1
            if hits % 2 == 1:
                err = b'{"error":{"message":"rate limited","type":"rate_limit_error","code":429}}'
                self._send(429, 'application/json', err)
                return
        self._send(200, 'text/event-stream', sse_body(body))

    def _send(self, status, ctype, payload):
        self.send_response(status)
        self.send_header('Content-Type', ctype)
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


def post(content: str) -> None:
    import urllib.request
    data = json.dumps({'model': 'dev-demo-model', 'stream': True,
                       'messages': [{'role': 'user', 'content': content}]}).encode()
    req = urllib.request.Request(BASE + '/v1/chat/completions', data=data, method='POST', headers={
        'X-Proxy-Base-URL': 'http://127.0.0.1:%d' % UPSTREAM_PORT,
        'X-Proxy-Key': 'local-fixture-only', 'X-Proxy-Client': 'dev-traffic',
        'Content-Type': 'application/json',
    })
    with urllib.request.urlopen(req, timeout=60) as r:
        r.read()


def bursts(count: int, gap: float) -> list:
    """One labeled content per request; a throttle and a bad request ride the
    first two bursts so the health surfaces carry both distinct counts."""
    out = []
    for n in range(count):
        burst = ['fixture %d-%d' % (n, k) for k in range(5)]
        if n == 0:
            burst += ['throttle a', 'bad x']
        elif n == 1:
            burst += ['throttle b']
        out.append(burst)
    return out


def main() -> int:
    global BASE, UPSTREAM_PORT
    here = os.path.dirname(os.path.abspath(__file__))
    root = os.path.dirname(here)
    sys.path.insert(0, root)
    from tests.support import require

    port = int(os.environ.get('DEV_PORT', '8081'))
    count = int(os.environ.get('DEV_TRAFFIC_BURSTS', '5'))
    gap = float(os.environ.get('DEV_TRAFFIC_GAP', '62'))
    require(1024 <= port <= 65535 and port != 8080,
            'DEV_PORT must be 1024..65535 and not production port 8080')
    require(count > 0, 'DEV_TRAFFIC_BURSTS must be positive')
    require(gap > 0, 'DEV_TRAFFIC_GAP must be positive (seconds)')
    BASE = 'http://127.0.0.1:%d' % port

    srv = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    UPSTREAM_PORT = srv.server_address[1]
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    signal.signal(signal.SIGTERM, lambda *_: (STOP.set(), srv.shutdown()))

    print('dev traffic: %d bursts %gs apart against %s (upstream 127.0.0.1:%d)'
          % (count, gap, BASE, UPSTREAM_PORT), flush=True)
    plan = bursts(count, gap)
    for n, burst in enumerate(plan):
        for content in burst:
            if STOP.is_set():
                print('dev traffic: stopped mid-burst', flush=True)
                return 0
            try:
                post(content)
                print('ok   ' + content, flush=True)
            except Exception as exc:  # a 400 fixture raises on the client side
                if 'bad' in content:
                    print('ok   %s (final 400 as designed)' % content, flush=True)
                else:
                    print('FAIL %s %r' % (content, exc), flush=True)
        if n < len(plan) - 1:
            STOP.wait(gap)
    print('dev traffic: done', flush=True)
    return 0


if __name__ == '__main__':
    sys.exit(main())

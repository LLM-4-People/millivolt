"""Shared fixture constants and the local upstream for the browser check."""

import time
from http.server import BaseHTTPRequestHandler
from pathlib import Path

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

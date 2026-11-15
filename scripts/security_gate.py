#!/usr/bin/env python3
"""Fail when the repository has an open code-scanning alert.

A promotion or a release must not land while a finding is unresolved. The gate
reads the repository API on every run instead of a cached export, so a finding
uploaded after the last review blocks the next attempt. A dismissed alert is
not open: a dismissal is a reviewed resolution and the security page keeps its
reason next to the code.

Usage:

    GITHUB_REPOSITORY=LLM-4-People/millivolt GH_TOKEN=... scripts/security_gate.py

Exit status: 0 when every alert is resolved, 1 when any alert is open or the
API cannot be read, 2 when no token is available. The gate fails closed: an
unreadable security state is never treated as clean.
"""

from __future__ import annotations

import json
import os
import re
import sys
from urllib import error, request

CANONICAL_REPOSITORY = "LLM-4-People/millivolt"
PER_PAGE = 100
TIMEOUT_SECONDS = 20
NEXT_LINK = re.compile(r'<([^>]+)>;\s*rel="next"')


class GateError(Exception):
    """The security state could not be read."""


def api_base() -> str:
    return os.environ.get("GITHUB_API_URL", "https://api.github.com").rstrip("/")


def repository() -> str:
    return os.environ.get("GITHUB_REPOSITORY", CANONICAL_REPOSITORY)


def next_link(header: str) -> str:
    match = NEXT_LINK.search(header or "")
    return match.group(1) if match else ""


def fetch_json(url: str, token: str) -> tuple[object, str]:
    req = request.Request(url)
    req.add_header("Authorization", f"Bearer {token}")
    req.add_header("Accept", "application/vnd.github+json")
    req.add_header("X-GitHub-Api-Version", "2022-11-28")
    try:
        with request.urlopen(req, timeout=TIMEOUT_SECONDS) as response:
            body = response.read()
            link = response.headers.get("Link", "")
    except error.HTTPError as exc:
        raise GateError(f"{url} returned HTTP {exc.code}") from exc
    except error.URLError as exc:
        raise GateError(f"cannot reach {url}: {exc.reason}") from exc
    try:
        return json.loads(body), link
    except ValueError as exc:
        raise GateError(f"{url} did not return JSON: {exc}") from exc


def open_alerts(repo: str, token: str, base: str, fetch=fetch_json) -> list[dict]:
    """Every open alert, following the API's pagination."""
    alerts: list[dict] = []
    url = f"{base}/repos/{repo}/code-scanning/alerts?state=open&per_page={PER_PAGE}"
    seen: set[str] = set()
    while url:
        if url in seen:
            raise GateError(f"pagination loop at {url}")
        seen.add(url)
        page, link = fetch(url, token)
        if not isinstance(page, list):
            raise GateError(f"{url} did not return a list of alerts")
        alerts.extend(page)
        url = next_link(link)
    return alerts


def describe(alert: dict) -> str:
    rule = alert.get("rule") or {}
    instance = alert.get("most_recent_instance") or {}
    location = instance.get("location") or {}
    severity = rule.get("security_severity_level") or rule.get("severity") or "unknown"
    path = location.get("path") or "unknown path"
    line = location.get("start_line")
    where = f"{path}:{line}" if line else path
    return f"#{alert.get('number')} {rule.get('id') or 'unknown rule'} {where} [{severity}]"


def main(argv: list[str] | None = None) -> int:
    if argv:
        print("usage: scripts/security_gate.py", file=sys.stderr)
        return 2
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if not token:
        print("security gate: no GH_TOKEN or GITHUB_TOKEN is available", file=sys.stderr)
        return 2
    repo = repository()
    try:
        alerts = open_alerts(repo, token, api_base())
    except GateError as exc:
        print(f"security gate: {exc}", file=sys.stderr)
        print("security gate: the security state is unknown, so the run fails closed", file=sys.stderr)
        return 1
    if not alerts:
        print("security gate: no open code-scanning alerts")
        return 0
    print(f"security gate: {len(alerts)} open code-scanning alert(s):", file=sys.stderr)
    for alert in alerts:
        print(f"  {describe(alert)}", file=sys.stderr)
    print("security gate: fix the finding or record a reviewed dismissal before releasing",
          file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))

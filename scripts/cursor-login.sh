#!/usr/bin/env bash
# cursor-login.sh - first-time interactive Cursor login.
#
# ONE job: run the PKCE deep-link flow (the same login Cursor's own CLI uses;
# no desktop IDE required) once and print the exact headers to configure a
# millivolt client with X-Proxy-Format: cursor.
#
# The script writes no files, but its output contains credentials: terminal
# scrollback, redirection and CI logs may retain them. Never share this output.
# Copy the printed headers into private client config. This is for FIRST-TIME
# login only; renewal is the proxy's job (stateless auto-refresh): while the
# client presents X-Proxy-Refresh-Token, the proxy exchanges it on Cursor's
# own host when the JWT expires and keeps the request flowing (in-place
# refresh; the fresh pair also rides the X-Proxy-Access-Token /
# X-Proxy-Refresh-Token response headers). If the refresh token is ever
# rejected upstream, just run this login again.
#
# Requires: curl (HTTP/2 for auth/poll), openssl, jq. Nothing is written to disk.
#
# Env overrides:
#   CURSOR_API_BASE      poll origin        (default https://api2.cursor.sh)
#   CURSOR_LOGIN_BASE    deep-link origin   (default https://www.cursor.com)
#   CURSOR_LOGIN_TIMEOUT seconds to wait for browser approval (default 300)
#   NO_OPEN              set to any value to skip auto-opening the browser
set -euo pipefail

API_BASE="${CURSOR_API_BASE:-https://api2.cursor.sh}"
LOGIN_BASE="${CURSOR_LOGIN_BASE:-https://www.cursor.com}"
POLL_TIMEOUT_S="${CURSOR_LOGIN_TIMEOUT:-300}"
POLL_INTERVAL_S=2

b64url() { # base64url-encode stdin, no padding
	openssl base64 -A | tr '+/' '-_' | tr -d '='
}

die() { echo "cursor-login: $*" >&2; exit 1; }

for c in curl openssl jq; do
	command -v "$c" >/dev/null 2>&1 || die "required tool not found: $c"
done

# --- PKCE deep-link login ---------------------------------------------------------
verifier="$(openssl rand 32 | b64url)"
challenge="$(printf '%s' "$verifier" | openssl dgst -sha256 -binary | b64url)"
uuid="$(cat /proc/sys/kernel/random/uuid 2>/dev/null || openssl rand -hex 16)"
url="${LOGIN_BASE}/loginDeepControl?challenge=${challenge}&uuid=${uuid}&mode=login&redirectTarget=cli"

echo "Open this URL in a browser and approve the login:" >&2
echo "" >&2
echo "  $url" >&2
echo "" >&2
if [[ -z "${NO_OPEN:-}" ]]; then
	(command -v xdg-open >/dev/null && xdg-open "$url" >/dev/null 2>&1 &) || true
fi
echo "Waiting up to ${POLL_TIMEOUT_S}s for approval…" >&2

# auth/poll returns 200 {accessToken,refreshToken,...} once approved;
# 202/404 while pending. HTTP/2 to match Cursor's expectations.
deadline=$(( $(date +%s) + POLL_TIMEOUT_S ))
while :; do
	now=$(date +%s)
	(( now < deadline )) || die "timed out waiting for login approval"
	resp="$(curl -sS --http2 -w '\n%{http_code}' \
		-G "${API_BASE}/auth/poll" \
		--data-urlencode "uuid=${uuid}" \
		--data-urlencode "verifier=${verifier}" \
		--max-time 20 || true)"
	code="${resp##*$'\n'}"
	body="${resp%$'\n'*}"
	if [[ "$code" == "200" ]]; then
		token="$(printf '%s' "$body" | jq -r '.accessToken // .apiKey // empty')"
		[[ -n "$token" ]] || die "approved but no accessToken in response (body withheld)"
		break
	fi
	sleep "$POLL_INTERVAL_S"
done
refresh="$(printf '%s' "$body" | jq -r '.refreshToken // empty')"

# --- output: the headers to configure a client -------------------------------------
echo "" >&2
if [[ -n "$refresh" ]]; then
	echo "Login approved. These headers contain credentials; keep the output private:" >&2
	echo "" >&2
	# stdout: copy-paste-ready header lines.
	printf 'Authorization: Bearer %s\n' "$token"
	printf 'X-Proxy-Refresh-Token: %s\n' "$refresh"
	printf 'X-Proxy-Base-URL: %s\n' "$API_BASE"
	printf 'X-Proxy-Format: cursor\n'
else
	echo "Login approved, but NO refresh token was returned (API-key-mode link?) -" >&2
	echo "the proxy cannot renew this token; re-run this login when it expires." >&2
	echo "" >&2
	printf 'Authorization: Bearer %s\n' "$token"
	printf 'X-Proxy-Base-URL: %s\n' "$API_BASE"
	printf 'X-Proxy-Format: cursor\n'
fi

#!/usr/bin/env bash
# grok-login.sh - first-time interactive Grok account login.
#
# ONE job: run xAI's OAuth device-code flow (RFC 8628) once and print the
# exact headers to configure a millivolt client. You approve the sign-in in a
# browser; the result is an account access token for https://api.x.ai/v1,
# not creation of an xai-… API key. Account eligibility and billing remain
# provider-controlled; successful login does not guarantee inference access.
#
# The script writes no files, but its output contains credentials: terminal
# scrollback, redirection and CI logs may retain them. Never share this output.
# Copy the printed headers into private client config. This is for FIRST-TIME
# login only; renewal is the proxy's job (stateless auto-refresh): while the
# client presents X-Proxy-Refresh-Token, the proxy exchanges it when the JWT
# expires or is about to. Inference returns HTTP 401 (code "token_expired")
# without an upstream send. Adopt access_token and any returned
# refresh_token from the error body, retain the old refresh token when no
# replacement is returned, then retry. Model discovery refreshes transparently
# without returning tokens. If the refresh token is rejected upstream, run
# this login again. See docs/adapters.md#token-refresh.
#
# Login traffic goes to auth.x.ai directly and is unaffected by any proxy
# base-URL override on inference calls.
#
# Requires: curl, jq. Nothing is written to disk.
#
# Env overrides:
#   XAI_AUTH_BASE        OAuth origin          (default https://auth.x.ai)
#   XAI_OAUTH_CLIENT_ID  device-flow client id (default: the public client
#                        shared with the proxy's auto-refresh - one OAuth
#                        registration; see internal/proxy/tokenrefresh.go)
#   GROK_LOGIN_TIMEOUT   approval-poll budget in seconds (default 600)
#   NO_OPEN              set to any value to skip auto-opening the browser
set -euo pipefail

AUTH_BASE="${XAI_AUTH_BASE:-https://auth.x.ai}"
CLIENT_ID="${XAI_OAUTH_CLIENT_ID:-b1a00492-073a-47ea-816f-4c329264a828}"
# offline_access is what makes the token endpoint return a refresh_token.
SCOPE="openid profile email offline_access grok-cli:access api:access"
POLL_TIMEOUT_S="${GROK_LOGIN_TIMEOUT:-600}"

die() { echo "grok-login: $*" >&2; exit 1; }

for c in curl jq; do
	command -v "$c" >/dev/null 2>&1 || die "required tool not found: $c"
done

# token_post <form-data…> - POST the token endpoint, split "body" and HTTP code.
token_post() {
	curl -sS -w '\n%{http_code}' -X POST "${AUTH_BASE}/oauth2/token" \
		-H 'Content-Type: application/x-www-form-urlencoded' "$@"
}

# --- device-code request --------------------------------------------------------
resp="$(curl -sS -w '\n%{http_code}' -X POST "${AUTH_BASE}/oauth2/device/code" \
	-H 'Content-Type: application/x-www-form-urlencoded' \
	--data-urlencode "client_id=${CLIENT_ID}" \
	--data-urlencode "scope=${SCOPE}" \
	--max-time 20 || true)"
code="${resp##*$'\n'}"
body="${resp%$'\n'*}"
[[ "$code" == "200" ]] || die "device-code request failed (HTTP ${code}; body withheld)"

device_code="$(printf '%s' "$body" | jq -r '.device_code // empty')"
[[ -n "$device_code" ]] || die "no device_code in response (body withheld)"
interval="$(printf '%s' "$body" | jq -r '.interval // 5')"
vurl="$(printf '%s' "$body" | jq -r '.verification_uri // empty')"
vurl_complete="$(printf '%s' "$body" | jq -r '.verification_uri_complete // empty')"
user_code="$(printf '%s' "$body" | jq -r '.user_code // empty')"

echo "Open this URL in a browser and approve the sign-in (code ${user_code}):" >&2
echo "" >&2
echo "  ${vurl_complete:-$vurl}" >&2
echo "" >&2
if [[ -z "${NO_OPEN:-}" ]]; then
	(command -v xdg-open >/dev/null && xdg-open "${vurl_complete:-$vurl}" >/dev/null 2>&1 &) || true
fi
echo "Waiting up to ${POLL_TIMEOUT_S}s for approval…" >&2

# --- approval poll ---------------------------------------------------------------
# Pending → 400 {"error":"authorization_pending"}; approved → 200 tokens.
# slow_down → honor it by widening the interval (RFC 8628 §3.5: +5s).
deadline=$(( $(date +%s) + POLL_TIMEOUT_S ))
while :; do
	now=$(date +%s)
	(( now < deadline )) || die "timed out waiting for login approval"
	resp="$(token_post \
		--data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:device_code' \
		--data-urlencode "device_code=${device_code}" \
		--data-urlencode "client_id=${CLIENT_ID}" \
		--max-time 20 || true)"
	code="${resp##*$'\n'}"
	body="${resp%$'\n'*}"
	if [[ "$code" == "200" ]]; then
		break
	fi
	err="$(printf '%s' "$body" | jq -r '.error // empty' 2>/dev/null || true)"
	case "$err" in
		authorization_pending) ;;
		slow_down) interval=$(( interval + 5 )) ;;
		expired_token) die "device code expired before approval - rerun to get a fresh one" ;;
		access_denied) die "sign-in was denied" ;;
		*) die "token poll failed (HTTP ${code}; body withheld)" ;;
	esac
	sleep "$interval"
done

# --- output: the headers to configure a client ------------------------------------
token="$(printf '%s' "$body" | jq -r '.access_token // empty')"
[[ -n "$token" ]] || die "approved but no access_token in response (body withheld)"
refresh="$(printf '%s' "$body" | jq -r '.refresh_token // empty')"

echo "" >&2
if [[ -n "$refresh" ]]; then
	echo "Login approved. These headers contain credentials; keep the output private:" >&2
	echo "" >&2
	# stdout: copy-paste-ready header lines.
	printf 'Authorization: Bearer %s\n' "$token"
	printf 'X-Proxy-Refresh-Token: %s\n' "$refresh"
	printf 'X-Proxy-Base-URL: https://api.x.ai/v1\n'
else
	echo "Login approved, but NO refresh token was returned (offline_access scope" >&2
	echo "missing) - the proxy cannot renew this token; re-run this login when it expires." >&2
	echo "" >&2
	printf 'Authorization: Bearer %s\n' "$token"
	printf 'X-Proxy-Base-URL: https://api.x.ai/v1\n'
fi

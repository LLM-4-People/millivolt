package main

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/logging"
	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// healthcheckTimeout bounds a probe well inside the Docker HEALTHCHECK
// --timeout so a hanging probe reports unhealthy instead of stalling.
const healthcheckTimeout = 3 * time.Second

// probeOKBody is the exact success document every probe route answers; a 200
// with any other body (a proxy error page, a captive portal) is not the
// instance.
const probeOKBody = `{"ok":true}` + "\n"

// runHealthcheck is the -healthcheck mode for Docker HEALTHCHECK in the
// distroless image: the liveness probe.
func runHealthcheck() int { return runProbe(mcp.RouteHealthz) }

// runReadycheck is the -readycheck mode: the readiness probe. Compose uses it
// as the service readiness probe so a deployment waits for a durable pipeline
// that can actually persist.
func runReadycheck() int { return runProbe(mcp.RouteReadyz) }

// runProbe is the shared probe implementation. It loads the same config file
// and CLI overrides as the server to find the listen address - read-only, no
// database, no listeners - probes path on it and returns an exit code. The
// probe pins the exact response document and refuses redirects so only the
// real handler can green-light the instance; both routes are intentionally
// unauthenticated: a probe carries no operator credential.
func runProbe(route mcp.Route) int {
	cfg, _, err := config.LoadFileStrict(liveConfigPath)
	if err != nil {
		// Fail closed on a named config that cannot be read. Substituting the
		// built-in defaults would probe the default listener and green-light a
		// container whose real document is gone or broken - the defaults name
		// a port this deployment may not use. Only an empty -config reaches
		// the built-in defaults, and LoadFileStrict already answers those
		// without an error.
		logging.Printf("", "probe %s: config %q: %v", route, liveConfigPath, err)
		return 1
	}
	applyCLIOverrides(cfg)
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		logging.Printf("", "probe %s: listen address %q: %v", route, cfg.Listen, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	target := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: string(route)}
	client := &http.Client{
		Timeout:   healthcheckTimeout,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(target.String())
	if err != nil {
		logging.Printf("", "probe %s: %v", route, err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		logging.Printf("", "probe %s: body: %v", route, err)
		return 1
	}
	if resp.StatusCode != http.StatusOK || string(body) != probeOKBody {
		// The readiness failure reason is in the body; surface it instead of
		// leaving the operator with only a status code.
		logging.Printf("", "probe %s: HTTP %d from %s: %s", route, resp.StatusCode, target.String(), strings.TrimRight(string(body), "\n"))
		return 1
	}
	return 0
}

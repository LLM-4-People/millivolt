package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// healthcheckTimeout bounds the probe well inside the Docker HEALTHCHECK
// --timeout so a hanging probe reports unhealthy instead of stalling.
const healthcheckTimeout = 3 * time.Second

// runHealthcheck is the -healthcheck mode for Docker HEALTHCHECK in the
// distroless image (no shell, no curl): it loads the same config file and CLI
// overrides as the server to find the listen address - read-only, no
// database, no listeners - probes GET /healthz on it and returns an exit
// code. The probe pins the exact response document and refuses redirects so
// only the real liveness handler can green-light the instance; /healthz is
// intentionally unauthenticated: this probe carries no operator credential.
func runHealthcheck() int {
	cfg, err := config.LoadFile(liveConfigPath)
	if err != nil {
		log.Printf("healthcheck: config: %v", err)
		return 1
	}
	applyCLIOverrides(cfg)
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		log.Printf("healthcheck: listen address %q: %v", cfg.Listen, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	target := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/healthz"}
	client := &http.Client{
		Timeout:   healthcheckTimeout,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(target.String())
	if err != nil {
		log.Printf("healthcheck: %v", err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		log.Printf("healthcheck: body: %v", err)
		return 1
	}
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}`+"\n" {
		log.Printf("healthcheck: HTTP %d from %s", resp.StatusCode, target.String())
		return 1
	}
	return 0
}

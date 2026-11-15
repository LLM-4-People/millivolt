package web

import (
	"slices"
	"strings"
)

// ReservedNamespace is one URL namespace of the origin: a millivolt-owned route
// family together with the three policies every consumer must apply to it. The
// list below is the single owner; the operator gate, the mux reservations and
// the service-worker live-path list are all generated from it, so a namespace
// cannot be gated but cached, or reserved but forwarded to inference.
type ReservedNamespace struct {
	// Root is the namespace root ("/admin"). The "/" row matches only itself.
	Root string
	// Gated puts the namespace in the operator plane.
	Gated bool
	// Reserved installs the root and its subtree on the mux, so an
	// unregistered subpath answers the reserved 404 instead of falling through
	// to the inference catch-all.
	Reserved bool
	// Live keeps the service worker off the namespace: its responses are
	// authenticated or stream per request and must never be answered from the
	// shell cache.
	Live bool
}

// namespaces is the canonical table. Order is cosmetic; membership is what
// matters. Every origin-root brand/PWA path, every operator namespace and the
// inference base appear exactly once.
var namespaces = []ReservedNamespace{
	// The dashboard entry: gated (it is the login page or the shell) and never
	// cached, because the same URL answers either.
	{Root: "/", Gated: true, Live: true},
	{Root: "/index.html", Gated: true, Live: true},
	// Open probes and assets: ungated, and still never cached.
	{Root: "/healthz", Live: true},
	{Root: "/models", Live: true},
	{Root: "/sw.js", Live: true},
	// Operator plane. Every unregistered spelling is reserved.
	{Root: "/admin", Gated: true, Reserved: true, Live: true},
	{Root: "/session", Gated: true, Reserved: true, Live: true},
	{Root: "/metrics", Gated: true, Reserved: true, Live: true},
	{Root: "/mcp", Gated: true, Reserved: true, Live: true},
	// Gated, but registered individually and safe to cache when content-hashed.
	{Root: "/dash", Gated: true},
	// Transparent inference: ungated, streamed, never cached.
	{Root: "/v1", Live: true},
}

// ReservedNamespaces returns the table for the consumers that build routes.
func ReservedNamespaces() []ReservedNamespace { return slices.Clone(namespaces) }

// normalizeNamespacePath reads a decoded backslash as a separator. No Go mux
// pattern does, but upstream proxies and caches historically have, so the
// namespace test cannot depend on the next hop's reading.
func normalizeNamespacePath(path string) string {
	return strings.ReplaceAll(path, `\`, "/")
}

// InNamespace reports whether path is the namespace root or below it. The "/"
// root matches only itself, so it does not swallow the whole origin.
func InNamespace(path, root string) bool {
	path = normalizeNamespacePath(path)
	if root == "/" {
		return path == "/"
	}
	return path == root || strings.HasPrefix(path, root+"/")
}

// Gated reports whether path belongs to the operator plane.
func Gated(path string) bool {
	path = normalizeNamespacePath(path)
	for _, ns := range namespaces {
		if ns.Gated && InNamespace(path, ns.Root) {
			return true
		}
	}
	return false
}

// LiveWorkerPaths returns the worker's never-cache list: every live exact path
// and, for a namespace root, the root+"/" prefix that keeps the subtree live.
func LiveWorkerPaths() []string {
	out := make([]string, 0, len(namespaces)*2)
	for _, ns := range namespaces {
		if !ns.Live {
			continue
		}
		out = append(out, ns.Root)
		if ns.Root != "/" {
			out = append(out, ns.Root+"/")
		}
	}
	return out
}

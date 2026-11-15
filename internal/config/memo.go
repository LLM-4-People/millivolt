package config

import "sync"

// pointerMemo is the one-entry cache shape for a value exported from one live
// *Config. Two exports need it: the dashboard bootstrap payload (polled per open
// tab every few seconds) and the model-canonicalization snapshot (read per SSE
// event). Both change only when an accepted reload swaps the live *Config, and
// both are re-derived on every read if the caller is handed a fresh copy.
//
// Pointer identity is the exact invalidation: boot and every reload publish a
// NEW *Config and never mutate the old one, so a cached value cannot outlive its
// config. The retained source pointer also keeps that address from being reused
// by a later allocation, so an address match is never a false hit. Never key on
// a time window or on content equality.
type pointerMemo[T any] struct {
	mu     sync.Mutex
	source *Config
	value  T
	valid  bool
}

// get returns the value built for cfg, building it at most once per distinct
// *Config. The result is SHARED with every caller until the live config pointer
// changes and must be treated as immutable: encode it, never mutate it.
// Concurrent first callers serialize on the one build.
func (m *pointerMemo[T]) get(cfg *Config, build func(*Config) T) T {
	if cfg == nil {
		// No silent empty value: a nil live config is a wiring defect.
		panic("config: memo read with a nil config")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.valid && m.source == cfg {
		return m.value
	}
	value := build(cfg)
	m.source, m.value, m.valid = cfg, value, true
	return value
}

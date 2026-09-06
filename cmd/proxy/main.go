package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LLM-4-People/millivolt"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/proxy"
	"github.com/LLM-4-People/millivolt/internal/storage"
	"github.com/LLM-4-People/millivolt/internal/web"
)

// Package-level state shared by the reload endpoint/handler and the reload
// helper. liveCfg is the live config (updated in place on reload so the next
// reload diffs against the latest applied values); liveProxy is the running
// proxy server; liveConfigPath is the -config flag value.
var (
	liveMu             sync.Mutex
	liveCfg            *config.Config
	bootCfg            *config.Config // immutable process-bound settings
	liveProxy          *proxy.Server
	liveBuf            *metrics.Buffer
	liveStore          *storage.Store
	liveConfigPath     string
	liveListenOverride string
	liveDBOverride     string
)

// rejectUnless is the RFC 9110 method gate for mux handlers in this
// file. Mux patterns stay unmethoded so a wrong-method request cannot
// fall through to the LLM proxy; Allow lists the one supported method.
func rejectUnless(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, `{"error":"`+method+` only"}`, http.StatusMethodNotAllowed)
	return false
}

func main() {
	configPath := flag.String("config", "proxy.yaml", "path to config file (empty for built-in defaults)")
	printConfig := flag.Bool("print-config", false, "print documented built-in defaults as YAML and exit without loading configuration")
	printExampleConfig := flag.Bool("print-example-config", false, "print the documented example with enabled provider profiles and exit without loading configuration")
	printVersion := flag.Bool("version", false, "print application and source build metadata as JSON and exit without loading configuration")
	// Override flags let a dev instance run alongside the main one on a
	// different port + scratch DB (see scripts/dev.sh) without editing the
	// real config. Empty = keep the config-file/default value.
	listenOverride := flag.String("listen", "", "override listen address (e.g. 127.0.0.1:8081); keeps instances on distinct ports")
	dbOverride := flag.String("db-path", "", `override storage DB path ("" = config value; "none" = disable durable storage)`)
	// Lets scripts/dev.sh keep tracking the dev instance across UI-triggered
	// restarts: the handoff child inherits this flag and rewrites the file
	// with its own pid on boot.
	pidFile := flag.String("pid-file", "", "write the process pid to this file after boot")
	healthcheck := flag.Bool("healthcheck", false, "probe GET /healthz on the configured listen address and exit nonzero on failure (Docker HEALTHCHECK); loads config read-only")
	flag.Parse()
	// Boot-time globals the healthcheck and reload helpers share. Set before
	// any mode that reads configuration.
	liveConfigPath = *configPath
	liveListenOverride = *listenOverride
	liveDBOverride = *dbOverride
	outputs := 0
	for _, enabled := range []bool{*printVersion, *printConfig, *printExampleConfig} {
		if enabled {
			outputs++
		}
	}
	if outputs > 1 {
		log.Fatal("-version, -print-config and -print-example-config are mutually exclusive")
	}
	if *printVersion {
		info, err := millivolt.CurrentBuild()
		if err != nil {
			log.Fatalf("version: %v", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(info); err != nil {
			log.Fatalf("version: %v", err)
		}
		return
	}
	// Printing configuration is read-only, even if operational flags name an
	// invalid config, database, PID file, or listen address.
	if *printConfig || *printExampleConfig {
		configuration := config.Default
		if *printExampleConfig {
			configuration = config.Example
		}
		if err := config.WriteYAML(os.Stdout, configuration()); err != nil {
			log.Fatalf("print config: %v", err)
		}
		return
	}
	// Docker HEALTHCHECK mode: probe and exit; nothing below runs.
	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	applyCLIOverrides(cfg)

	buf := metrics.NewBuffer(cfg.HistorySize)

	// Durable storage is optional; a missing DB path skips it.
	var store *storage.Store
	var rec metrics.Recorder = buf
	if cfg.DBPath != "" {
		s, err := storage.Open(cfg.DBPath, storage.Options{
			WriteChanCap:  cfg.StorageWriteChanCap,
			BatchCap:      cfg.StorageBatchCap,
			FlushInterval: cfg.StorageFlushInterval,
			QueryTimeout:  cfg.StorageQueryTimeout,
			QueryMaxBytes: cfg.StorageQueryMaxBytes,
			QueryMaxRows:  cfg.StorageQueryMaxRows,
			// WriteTrackCap is an internal derivation (2x the ring), not a
			// config knob: the written-id set must cover every record the
			// ring can hold so the aggregate merge dedupes exactly.
			WriteTrackCap: cfg.HistorySize * 2,
		})
		if err != nil {
			log.Fatalf("storage: %v", err)
		}
		store = s
		liveStore = s

		// provider_aliases merge stored history before the ring backfill reads
		// it, so the dashboard groups one provider as one entity from the first
		// paint. A failed rename fails the boot (a bad value never applies
		// silently); totals are label-independent, so no rebuild is needed.
		if len(cfg.ProviderAliases) > 0 {
			renamed, err := s.RenameProviders(context.Background(), cfg.ProviderAliases)
			if err != nil {
				log.Fatalf("provider_aliases: %v", err)
			}
			if renamed > 0 {
				log.Printf("provider_aliases: merged %d records into canonical labels", renamed)
			}
		}

		// Fan records out to both the ring buffer (live) and durable store.
		rec = s.Recorder(buf)

		// Backfill the ring buffer with recent history so the dashboard
		// survives restarts. Bounded by the store query timeout via
		// withQueryTimeout so a wedged read pool can't hang startup (the
		// signal handler isn't installed yet).
		recs, err := s.LoadRecent(context.Background(), cfg.HistorySize)
		if err != nil {
			// Degraded but not fatal: boot with an empty ring (history is still
			// in SQLite). Never fail silently - log the persistence failure.
			log.Printf("backfill skipped: %v", err)
		} else {
			buf.Backfill(recs)
			log.Printf("backfilled %d records from storage", len(recs))
		}
		// Cap full live snapshots to the newest window - durable storage is
		// on here, so applySnapshotLimit applies the cap (see its doc for the
		// full rule; the derivation is internal, not a tunable).
		applySnapshotLimit(buf, store, cfg.DashLogRows)
		// Prime the written-id set with the backfilled ids: these rows are
		// already durable. A fresh process with an empty written set would
		// merge the whole backfilled ring into the server aggregates as
		// "un-flushed" and double-count history (live bug - 33k vs 23k).
		if len(recs) > 0 {
			ids := make([]string, 0, len(recs))
			for _, r := range recs {
				ids = append(ids, r.ID)
			}
			s.MarkWritten(ids)
		}
	}

	proxySrv := proxy.New(cfg, rec)
	if store != nil {
		proxySrv.AttachPausePersist(store)
	}
	// Publish live state for config reload (SIGHUP + /admin/reload).
	liveCfg = cfg
	bootCfg = cfg.Clone()
	liveProxy = proxySrv
	liveBuf = buf
	liveConfigPath = *configPath

	// The operator gate's credential is process-bound (env only, re-read on
	// restart or handoff, never on reload). Failing the boot on an invalid
	// credential keeps deny-by-default honest - no silent weaker policy.
	gate := newOperatorGate(mustOperatorToken())

	mux := http.NewServeMux()
	// The /admin and /metrics namespaces own no unregistered handler:
	// reserving both the roots and the subtrees means neither the
	// unauthenticated gate nor an authenticated request can ever forward a
	// namespace look-alike to the upstream catch-all. Registered exact
	// patterns (session, pause, config, agg/*, ...) keep mux precedence.
	mux.Handle("/admin", http.NotFoundHandler())
	mux.Handle("/admin/", http.NotFoundHandler())
	mux.Handle("/metrics", http.NotFoundHandler())
	mux.Handle("/metrics/", http.NotFoundHandler())
	// The liveness probe is the one open server route besides inference:
	// Docker HEALTHCHECK and load balancers cannot carry the operator
	// credential. Depth (storage, feeds) stays on the gated dashboard.
	mux.HandleFunc("/healthz", handleHealthz)
	// The session handshake mints the operator cookie; it is exempt from the
	// credential gate by definition and owns its own failure throttling.
	mux.Handle("/admin/session", http.NewCrossOriginProtection().Handler(http.HandlerFunc(gate.handleAdminSession)))
	// The SSE feed is not gzipped (it flushes event-by-event; every other
	// dashboard route is buffered text and compresses).
	mux.Handle("/metrics/live/stream", http.HandlerFunc(buf.HandleStream))
	mux.Handle("/metrics/prometheus", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metrics.HandlePrometheus(w, r, buf)
	}))
	// Dashboard aggregates: every non-table metric surface is computed here
	// against all requests since inception (durable store + un-flushed ring
	// records, exact dedupe via the store's written-id set; ring-only when
	// durable storage is disabled).
	agg := web.NewAggAPI(buf, store, cfg.StorageQueryTimeout)
	if err := agg.PreloadHistory(context.Background()); err != nil {
		log.Printf("dashboard: initial analytical history load failed (reads will retry): %v", err)
	}
	// Bootstrap state providers: the effective config, operator pause holds,
	// and provider limits ride the boot payload so a fresh page paints in one
	// round trip (same shapes as the dedicated GET endpoints - the shared
	// builders are the single owner).
	agg.Dash = func() any {
		liveMu.Lock()
		defer liveMu.Unlock()
		return liveCfg.Clone().Map()
	}
	agg.Pause = func() any { return proxySrv.PauseSnapshot() }
	agg.Throttle = func() any { return proxySrv.ThrottleSnapshot() }
	agg.Debug = func() any { return proxySrv.DebugSnapshot() }
	agg.Storm = func() any { return proxySrv.StormSnapshot() }
	// Model canonicalization rules (config.CanonicalModel semantics) ride the
	// bootstrap so the client mirror groups identically to the server fold;
	// read live per request so a Settings save hot-applies on the next tick.
	agg.ModelCanon = func() config.ModelCanon {
		liveMu.Lock()
		defer liveMu.Unlock()
		return liveCfg.ModelCanon()
	}
	buf.SetModelObserver(agg.ObserveModels)
	proxySrv.ModelObserver = agg.ObserveModelNames
	// The agg endpoints are buffered dashboard JSON (log pages can be
	// hundreds of KB) - gzip like every other buffered dashboard route.
	aggGet := func(h http.HandlerFunc) http.Handler { return web.Gzip(h) }
	mux.Handle("/metrics/agg/chart", aggGet(agg.HandleAggChart))
	mux.Handle("/metrics/agg/explorer", aggGet(agg.HandleAggExplorer))
	mux.Handle("/metrics/agg/log", aggGet(agg.HandleLogPage))
	mux.Handle("/metrics/bootstrap", aggGet(agg.HandleBootstrap))
	registerLogRoutes(mux, buf, store)

	// Operator pause: in-flight requests finish, new ones queue until resume.
	// The dashboard Pause/Resume button is the UI for this.
	mux.Handle("/admin/pause", http.HandlerFunc(proxySrv.HandlePause))
	mux.Handle("/admin/throttle", http.HandlerFunc(proxySrv.HandleThrottle))
	mux.Handle("/admin/debug", http.HandlerFunc(proxySrv.HandleDebug))
	mux.Handle("/admin/debug/capture", web.Gzip(http.HandlerFunc(proxySrv.HandleDebugCapture)))

	// Config hot-reload. POST re-reads the config file and hot-applies the
	// reloadable subset with zero dropped requests (the listener never closes).
	// Startup-bound fields (listen, db_path, storage write pipeline, Open/AggAPI
	// query timeout, HTTP server timeouts, history_size) stay at their boot
	// consumers; reload still swaps the snapshot and reports them as
	// restart_required (GET /admin/config effective shows the new values).
	ov := map[string]string{}
	if liveListenOverride != "" {
		ov["listen"] = liveListenOverride
	}
	if liveDBOverride != "" {
		ov["db_path"] = liveDBOverride
	}
	mux.Handle("/admin/config", &config.Handler{
		Path:    liveConfigPath,
		Startup: bootCfg,
		Effective: func() *config.Config {
			liveMu.Lock()
			defer liveMu.Unlock()
			return liveCfg.Clone()
		},
		Overrides: ov,
		Persist:   reloadConfig,
	})

	mux.HandleFunc("/admin/reload", func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnless(w, r, http.MethodPost) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		skipped, err := reloadConfig()
		if err != nil {
			http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusBadRequest)
			return
		}
		log.Printf("config reloaded from %s (restart-required fields skipped: %s)", *configPath, strings.Join(skipped, ","))
		enc, _ := json.Marshal(map[string]any{"ok": true, "restart_required": skipped})
		w.Write(enc)
	})

	// Root serves the dashboard; web.DashPrefix is the allowlisted CSS/JS
	// tree (a miss is 404 - never forwarded to the LLM proxy). Register
	// both /dash and /dash/ so a slash-less path cannot fall through to
	// the catch-all proxy. Everything else is proxied.
	dashboard := web.Gzip(web.Handler(agg))
	dash := http.HandlerFunc(web.ServeDash) // immutable assets select precompressed bytes themselves
	mux.Handle("/{$}", dashboard)
	mux.Handle("/index.html", dashboard)
	mux.Handle("/favicon.ico", web.Favicon())
	mux.Handle(strings.TrimSuffix(web.DashPrefix, "/"), dash)
	mux.Handle(web.DashPrefix, dash)
	mux.Handle("/", proxySrv)

	dbDesc := cfg.DBPath
	if dbDesc == "" {
		dbDesc = "(in-memory only)"
	}
	log.Printf("millivolt listening on %s  db=%s", cfg.Listen, dbDesc)

	// Claim the port, killing any stale proxy instance holding it - unless
	// this process was started by a restart handoff, which passes the live
	// listening socket instead (claimPort must be bypassed then: the draining
	// parent still holds the port and would be exe-identity-matched and
	// SIGTERMed by our own port-claim logic).
	var ln net.Listener
	if inherited, ok := inheritedListener(); ok {
		ln = inherited
	} else {
		ln, err = claimPort(cfg.Listen)
		if err != nil {
			log.Fatalf("serve: %v", err)
		}
	}
	// srvCtx is the base context for every request. Cancelling it on shutdown
	// aborts in-flight SSE streams (the metrics feed and proxied LLM streams)
	// immediately, so httpSrv.Shutdown isn't gated on them hitting the timeout.
	srvCtx, stopServing := context.WithCancel(context.Background())
	defer stopServing()

	handler := protectOperatorRequests(mux, gate)
	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		// No WriteTimeout: it would cut off long-lived SSE streams.
		BaseContext: func(net.Listener) context.Context { return srvCtx },
	}

	// Serve in a goroutine; main blocks on the shutdown signal so we can shut
	// down gracefully and close storage only after in-flight requests finish.

	// Rebuild & graceful restart (dashboard Restart button → /admin/restart).
	// The restarter owns the handoff choreography; see restart.go for the
	// zero-lost-requests guarantees.
	rst := newRestarter(&restartDeps{
		listener: ln,
		server:   httpSrv,
		cloneServer: func() *http.Server {
			return &http.Server{
				Handler:           handler,
				ReadHeaderTimeout: cfg.ReadHeaderTimeout,
				IdleTimeout:       cfg.IdleTimeout,
				// No WriteTimeout: it would cut off long-lived SSE streams.
				BaseContext: func(net.Listener) context.Context { return srvCtx },
			}
		},
		closeFeeds: buf.CloseFeeds,
		flushStore: func() error {
			if store == nil {
				return nil
			}
			return store.Flush()
		},
		drainTimeout: func() time.Duration {
			liveMu.Lock()
			defer liveMu.Unlock()
			return liveCfg.RestartDrainTimeout
		},
		args: os.Args[1:],
	})
	mux.HandleFunc("/admin/restart", rst.handleRestart)

	// A handoff child refreshes the pid file it inherited (scripts/dev.sh
	// stays authoritative across UI-triggered restarts), then tells the
	// waiting parent it is fully booted - store open, totals scanned, ring
	// backfilled, holds restored - and is about to serve.
	if *pidFile != "" {
		if perr := os.WriteFile(*pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); perr != nil {
			log.Printf("pid file: %v", perr)
		}
	}
	signalReady()

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	// Loop so SIGHUP reloads config without exiting; SIGINT/SIGTERM break out to
	// a graceful shutdown. A serve error exits immediately (store drained first).
	var shutdownSig os.Signal
loop:
	for {
		select {
		case err := <-serveErr:
			if rst.committed() || (errors.Is(err, http.ErrServerClosed) && rst.servingServer() != httpSrv) {
				// A restart handoff closed the listener deliberately
				// (httpSrv.Shutdown inside the choreography). Serve returning
				// is expected - the handoff goroutine owns the shutdown
				// sequence (drain → flush → spawn → exit) and must not be
				// preempted by this loop exiting the process mid-handoff,
				// which would kill the very streams being drained.
				continue
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				// A mid-life serve failure. Drain the store before exiting (the
				// same guarantee the signal path gives) - log.Printf, not
				// Fatalf, so the drain actually runs before we exit nonzero.
				log.Printf("serve: %v", err)
				if store != nil {
					if cerr := store.Close(); cerr != nil {
						log.Printf("storage close: %v", cerr)
					}
				}
				os.Exit(1)
			}
			// Serve returned cleanly without a signal (e.g. listener closed).
			// Drain the store before exiting so no buffered metrics are lost.
			if store != nil {
				if cerr := store.Close(); cerr != nil {
					log.Printf("storage close: %v", cerr)
				}
			}
			return
		case sig := <-sigc:
			if sig == syscall.SIGHUP {
				// Config reload: hot-apply the reloadable subset with zero
				// dropped requests; report any restart-required field changes.
				skipped, err := reloadConfig()
				if err != nil {
					log.Printf("config reload failed (keeping current): %v", err)
				} else if len(skipped) > 0 {
					log.Printf("config reloaded; restart required for: %s", strings.Join(skipped, ", "))
				} else {
					log.Printf("config reloaded from %s", liveConfigPath)
				}
				continue
			}
			if handoffDone := rst.handoffDone(); handoffDone != nil {
				// A restart handoff owns the shutdown sequence (drain → flush
				// → handoff → exit). Racing it with a second shutdown would
				// abort the very streams the handoff is draining; wait it out
				// and keep looping if serving resumed.
				log.Printf("received %v during restart handoff; the handoff owns shutdown", sig)
				<-handoffDone
				continue
			}
			shutdownSig = sig
			break loop
		}
	}
	log.Printf("received %s, shutting down", shutdownSig)

	// Abort live streams first so Shutdown returns promptly instead of waiting
	// the full timeout for SSE connections that never finish on their own.
	stopServing()

	liveMu.Lock()
	shutdownTo := liveCfg.ShutdownTimeout
	liveMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTo)
	defer cancel()
	if err := rst.drain(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		// DeadlineExceeded is a benign outcome for streaming workloads (a
		// connection outlived the grace window); anything else is real.
		log.Printf("shutdown: %v", err)
	}
	// In-flight requests have completed (or been aborted); safe to close the
	// store, which drains its write channel regardless of the HTTP outcome.
	if store != nil {
		if err := store.Close(); err != nil {
			log.Printf("storage close: %v", err)
		}
	}
}

// reloadConfig re-reads the config file and hot-applies the reloadable subset
// to the running proxy with zero dropped requests. It returns the list of
// startup-bound fields that changed (listen, storage write pipeline, Open/AggAPI
// query timeout, HTTP server timeouts, history size): those consumers stay at
// boot values, but the snapshot is still swapped (GET effective shows them)
// and they are reported as restart_required. The current cfg is updated in
// place so a second reload diffs against the latest snapshot.
func applyCLIOverrides(cfg *config.Config) {
	if liveListenOverride != "" {
		cfg.Listen = liveListenOverride
	}
	switch liveDBOverride {
	case "":
		// keep config value
	case "none":
		cfg.DBPath = "" // in-memory only
	default:
		cfg.DBPath = liveDBOverride
	}
}

func reloadConfig() ([]string, error) {
	liveMu.Lock()
	defer liveMu.Unlock()
	fresh, err := config.LoadFile(liveConfigPath)
	if err != nil {
		return nil, err
	}
	applyCLIOverrides(fresh)
	// Compute which startup-bound fields changed (these can't move on a live
	// process) so we can report them as restart-required. Schema().HotReload
	// is the single source of that classification.
	skipped := config.StartupBoundChanges(bootCfg, fresh)
	// A changed provider_aliases map rewrites stored history through the
	// single-conn write pool (same serialization PurgeWhere relies on). Run
	// before the snapshot swap so a failed rename keeps the running config.
	// Records already in the in-memory ring keep their old label until they
	// age out or the process restarts; new requests re-key immediately.
	if liveStore != nil && !maps.Equal(liveCfg.ProviderAliases, fresh.ProviderAliases) {
		if _, err := liveStore.RenameProviders(context.Background(), fresh.ProviderAliases); err != nil {
			return nil, fmt.Errorf("provider_aliases: %w", err)
		}
	}
	// Hot-apply the reloadable subset. The proxy swaps its config snapshot and
	// upstream transport atomically; in-flight requests are unaffected.
	liveProxy.Reload(fresh.Clone())
	// The full-snapshot cap derives from dash_log_rows (hot-reloadable) -
	// re-apply it so the boot-time bound can't drift from the live value.
	// The helper gates on durable storage exactly like boot: an
	// in-memory-only instance must stay unlimited (the ring is all
	// history; the paging fallback is dead without a store).
	applySnapshotLimit(liveBuf, liveStore, fresh.DashLogRows)
	// Keep the shared cfg in sync so the next reload diffs against the latest.
	cloned := fresh.Clone()
	*liveCfg = *cloned
	return skipped, nil
}

// applySnapshotLimit is the one owner of the full-snapshot cap wiring, shared
// by boot and reload (dash_log_rows hot-reloads, so the bound is re-applied
// on every reload). Full live snapshots are capped to the newest
// BootRingCapMul × dashLogRows window only when durable storage backs the
// ring: a cursor-less client then needs just the first log pages, and older
// rows page from the store via /metrics/agg/log. With the store off, the
// ring is all history and that paging fallback is dead - capping would
// strand older rows until process restart (a silent miss of exactly the
// class the cursor protocol forbids) - so the limit stays unlimited.
func applySnapshotLimit(buf *metrics.Buffer, store *storage.Store, dashLogRows int) {
	if buf == nil {
		return
	}
	if store == nil {
		buf.SetSnapshotLimit(0)
		return
	}
	buf.SetSnapshotLimit(web.BootRingCapMul * dashLogRows)
}

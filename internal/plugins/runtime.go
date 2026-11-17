package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Runtime bounds. These are internal guardrails, not operator tunables: a
// plugin is untrusted code and cannot grow beyond them by editing config.
const (
	maxModuleBytes  = 16 << 20 // Go WASI guests are multi-megabyte
	maxOutputBytes  = 256 << 10
	maxMemoryPages  = 128             // 8 MiB linear memory for full-capability Go WASI guests
	pluginCallLimit = 6 * time.Second // covers the bounded 5s http:fetch cap
)

type hostStateKey struct{}

type hostState struct {
	input        []byte
	manifest     *PluginManifest
	output       []byte
	httpResponse []byte
	calls        map[string]int
	denials      int
}

func (s *hostState) allow(capability, call string) uint32 {
	s.calls[call]++
	if !s.manifest.allows(capability) {
		s.denials++
		return hostDenied
	}
	return 0
}

const hostDenied = ^uint32(0)

type instance struct {
	id         string
	manifest   *PluginManifest
	handle     api.Function
	initFn     api.Function
	shutdown   api.Function
	module     api.Module
	archive    string
	failures   atomic.Uint64
	lastOutput atomic.Value
}

// Runtime owns all instantiated plugin modules. It is built once at boot;
// plugin changes require a restart.
type Runtime struct {
	mu             sync.Mutex
	rt             wazero.Runtime
	host           api.Module
	instances      []*instance
	seq            atomic.Uint64
	last           atomic.Value
	trust          *Trust
	locks          *Locks
	lockPath       string
	lastDenials    atomic.Value
	httpAllowHosts map[string]struct{}
}

// SetLockPath lets boot wire the lock file used for verified recovery.
func (r *Runtime) SetLockPath(path string) {
	if r != nil {
		r.lockPath = path
	}
}

// SetHTTPAllowHosts wires the operator allowlist for the generic http:fetch
// capability. It must be called before modules are loaded.
func (r *Runtime) SetHTTPAllowHosts(hosts []string) {
	if r != nil {
		r.httpAllowHosts = normalizeHTTPHosts(hosts)
	}
}

// NewRuntime builds the embedded WASM runtime with a hard memory ceiling and
// context-bound termination. No filesystem or network namespace is exposed.
func NewRuntime(ctx context.Context) (*Runtime, error) {
	config := wazero.NewRuntimeConfigCompiler().
		WithMemoryLimitPages(maxMemoryPages).
		WithCloseOnContextDone(true)
	r := wazero.NewRuntimeWithConfig(ctx, config)
	// Go WASI guests import the preview1 clock/random interfaces. No filesystem
	// or network is configured; the ABI host functions remain the only I/O.
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		_ = r.Close(ctx)
		return nil, err
	}
	out := &Runtime{rt: r, httpAllowHosts: map[string]struct{}{}}
	host, err := registerHostFunctions(ctx, r, out)
	if err != nil {
		_ = r.Close(ctx)
		return nil, err
	}
	out.host = host
	return out, nil
}

// LoadDir is the development wrapper: no trust store and no digest pins.
func (r *Runtime) LoadDir(ctx context.Context, dir string) error {
	return r.LoadDirWithPolicy(ctx, dir, nil, nil)
}

// LoadDirWithPolicy verifies signatures and pins before instantiating any
// module. A bad manifest, signature, pin, digest or export fails the whole
// boot rather than loading a partial plugin set. Legacy .wasm pairs and
// single-file .mvplugin archives share the same policy. A verified previous
// archive is the only permitted recovery from an invalid active archive.
func (r *Runtime) LoadDirWithPolicy(ctx context.Context, dir string, trust *Trust, locks *Locks) error {
	if r == nil {
		return fmt.Errorf("nil plugin runtime")
	}
	r.trust = trust
	r.locks = locks
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("plugin dir %q: %w", dir, err)
	}
	count := 0
	for _, entry := range entries {
		name := entry.Name()
		isModule := strings.HasSuffix(name, ".wasm")
		isArchive := strings.HasSuffix(name, ".mvplugin")
		if !isModule && !isArchive {
			continue
		}
		count++
		if count > MaxPluginFiles {
			return fmt.Errorf("plugin dir: more than %d modules", MaxPluginFiles)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("plugin module %q: %w", name, err)
		}
		limit := maxModuleBytes
		if isArchive {
			limit = maxPackageArchiveBytes
		}
		if !info.Mode().IsRegular() || info.Size() > int64(limit) {
			return fmt.Errorf("plugin module %q: not a regular file under the %d byte cap", name, limit)
		}
		raw, err := readBoundedFile(filepath.Join(dir, name), limit)
		if err != nil {
			return err
		}
		var manifest *PluginManifest
		var code []byte
		var digest, archive string
		if isArchive {
			pkg, err := ReadPackage(raw)
			if err == nil && name != pkg.Manifest.ID+".mvplugin" {
				err = fmt.Errorf("manifest id is %q", pkg.Manifest.ID)
			}
			if err == nil {
				err = trust.Verify(pkg.Manifest)
			}
			if err == nil {
				err = locks.Check(name, pkg.ArchiveSHA256)
			}
			if err != nil {
				recovered, recoveryErr := r.recoverArchive(ctx, dir, name, trust, locks)
				if recoveryErr != nil {
					return fmt.Errorf("plugin archive %q: %w; recovery failed: %v", name, err, recoveryErr)
				}
				log.Printf("plugins: recovered %s from previous archive", name)
				pkg = recovered
			}
			manifest, code, digest, archive = pkg.Manifest, pkg.Module, pkg.ArchiveSHA256, name
		} else {
			manifest, err = LoadPluginManifest(filepath.Join(dir, strings.TrimSuffix(name, ".wasm")+".json"))
			if err != nil {
				return err
			}
			code = raw
			sum := sha256.Sum256(code)
			digest = hex.EncodeToString(sum[:])
			if manifest.Module != name {
				return fmt.Errorf("plugin module %q: manifest names %q", name, manifest.Module)
			}
			if digest != manifest.ModuleSHA256 {
				return fmt.Errorf("plugin module %q: digest mismatch", name)
			}
		}
		if err := trust.Verify(manifest); err != nil {
			return fmt.Errorf("plugin module %q: %w", name, err)
		}
		if err := locks.Check(name, digest); err != nil {
			return fmt.Errorf("plugin module %q: %w", name, err)
		}
		if err := r.load(ctx, manifest, code, archive); err != nil {
			return fmt.Errorf("plugin module %q: %w", name, err)
		}
	}
	return nil
}

func (r *Runtime) recoverArchive(ctx context.Context, dir, name string, trust *Trust, locks *Locks) (*Package, error) {
	if r == nil || locks == nil || r.lockPath == "" {
		return nil, fmt.Errorf("recovery requires a configured lock file")
	}
	id := strings.TrimSuffix(name, ".mvplugin")
	if !pluginIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid archive name %q", name)
	}
	state, ok := locks.Install(id)
	if !ok || state.Previous == nil {
		return nil, fmt.Errorf("no previous archive for %q", id)
	}
	prevRaw, err := readBoundedFile(filepath.Join(dir, state.Previous.Archive), maxPackageArchiveBytes)
	if err != nil {
		return nil, err
	}
	pkg, err := ReadPackage(prevRaw)
	if err != nil {
		return nil, err
	}
	if pkg.Manifest.ID != id || pkg.Manifest.Version != state.Previous.Version ||
		pkg.ArchiveSHA256 != state.Previous.ArchiveSHA256 {
		return nil, fmt.Errorf("previous archive does not match install state")
	}
	if err := trust.Verify(pkg.Manifest); err != nil {
		return nil, err
	}
	currentRaw, currentErr := readBoundedFile(filepath.Join(dir, name), maxPackageArchiveBytes)
	next := locks.Clone()
	if err := next.SetPin(name, pkg.ArchiveSHA256); err != nil {
		return nil, err
	}
	if err := next.SetInstall(id, InstallState{
		Version:         pkg.Manifest.Version,
		Archive:         name,
		Source:          state.Previous.Source,
		ResolvedVersion: pkg.Manifest.Version,
		ArchiveSHA256:   pkg.ArchiveSHA256,
		InstalledAt:     time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return nil, err
	}
	lockTmp, err := next.writeTemp(r.lockPath)
	if err != nil {
		return nil, err
	}
	defer os.Remove(lockTmp)
	activeTmp, err := stageArchiveFile(dir, name, prevRaw)
	if err != nil {
		return nil, err
	}
	defer os.Remove(activeTmp)
	if err := replaceArchiveFile(dir, name, currentRaw, currentErr == nil, activeTmp); err != nil {
		return nil, err
	}
	if err := commitLockTemp(lockTmp, r.lockPath); err != nil {
		if currentErr == nil {
			restoreArchiveFile(dir, name, currentRaw, true)
		}
		return nil, err
	}
	_ = os.Remove(filepath.Join(dir, state.Previous.Archive))
	*locks = *next
	r.locks = locks
	return pkg, nil
}

// Pin records the current digest of every valid module or archive and keeps
// any existing install metadata. It verifies signatures before writing.
func (r *Runtime) Pin(dir, lockPath string) ([]string, error) {
	if r == nil || dir == "" {
		return nil, fmt.Errorf("plugin dir is required")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	// Install and update commit state to the lock file without hot-swapping
	// the boot-loaded runtime. Read the committed document here so an explicit
	// pin cannot overwrite newer install metadata with the boot snapshot.
	pins, err := LoadLocks(lockPath)
	if err != nil {
		return nil, err
	}
	count := 0
	var pinned []string
	for _, entry := range entries {
		name := entry.Name()
		isModule := strings.HasSuffix(name, ".wasm")
		isArchive := strings.HasSuffix(name, ".mvplugin")
		if !isModule && !isArchive {
			continue
		}
		count++
		if count > MaxPluginFiles {
			return nil, fmt.Errorf("plugin dir: more than %d modules", MaxPluginFiles)
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		limit := maxModuleBytes
		if isArchive {
			limit = maxPackageArchiveBytes
		}
		if !info.Mode().IsRegular() || info.Size() > int64(limit) {
			return nil, fmt.Errorf("plugin module %q: not a regular file under the %d byte cap", name, limit)
		}
		raw, err := readBoundedFile(filepath.Join(dir, name), limit)
		if err != nil {
			return nil, err
		}
		if isArchive {
			pkg, err := ReadPackage(raw)
			if err != nil {
				return nil, err
			}
			if name != pkg.Manifest.ID+".mvplugin" {
				return nil, fmt.Errorf("plugin archive %q: manifest id is %q", name, pkg.Manifest.ID)
			}
			if err := r.trust.Verify(pkg.Manifest); err != nil {
				return nil, fmt.Errorf("plugin archive %q: %w", name, err)
			}
			if err := pins.SetPin(name, pkg.ArchiveSHA256); err != nil {
				return nil, err
			}
			pinned = append(pinned, name)
			continue
		}
		manifest, err := LoadPluginManifest(filepath.Join(dir, strings.TrimSuffix(name, ".wasm")+".json"))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		if digest != manifest.ModuleSHA256 {
			return nil, fmt.Errorf("plugin module %q: digest mismatch", name)
		}
		if err := r.trust.Verify(manifest); err != nil {
			return nil, fmt.Errorf("plugin module %q: %w", name, err)
		}
		if err := pins.SetPin(name, digest); err != nil {
			return nil, err
		}
		pinned = append(pinned, name)
	}
	if err := pins.Save(lockPath); err != nil {
		return nil, err
	}
	r.locks = pins
	return pinned, nil
}

func (r *Runtime) load(ctx context.Context, manifest *PluginManifest, code []byte, archive string) error {
	compiled, err := r.rt.CompileModule(ctx, code)
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	mod, err := r.rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().
		WithName(manifest.ID).
		WithStartFunctions())
	if err != nil {
		return fmt.Errorf("instantiate: %w", err)
	}
	exports := map[string]api.Function{}
	for _, name := range []string{"mv_init", "mv_handle", "mv_shutdown"} {
		fn := mod.ExportedFunction(name)
		if fn == nil {
			_ = mod.Close(ctx)
			return fmt.Errorf("missing export %q", name)
		}
		exports[name] = fn
	}
	inst := &instance{
		id:       manifest.ID,
		manifest: manifest,
		module:   mod,
		archive:  archive,
		initFn:   exports["mv_init"],
		handle:   exports["mv_handle"],
		shutdown: exports["mv_shutdown"],
	}
	state := &hostState{manifest: manifest, calls: map[string]int{}}
	callCtx, cancel := context.WithTimeout(ctx, pluginCallLimit)
	defer cancel()
	// Go 1.27 WASI guests export _start for runtime initialization; older
	// reactor builds export _initialize. Call exactly one before mv_init.
	initialize := mod.ExportedFunction("_start")
	initName := "_start"
	if initialize == nil {
		initialize = mod.ExportedFunction("_initialize")
		initName = "_initialize"
	}
	if initialize != nil {
		if _, err := initialize.Call(callCtx); err != nil {
			_ = mod.Close(ctx)
			return fmt.Errorf("%s: %w", initName, err)
		}
	}
	if _, err := inst.initFn.Call(withHostState(callCtx, state)); err != nil {
		_ = mod.Close(ctx)
		return fmt.Errorf("mv_init: %w", err)
	}
	r.mu.Lock()
	r.instances = append(r.instances, inst)
	r.mu.Unlock()
	return nil
}

// Enabled reports whether any plugin instance was loaded.
func (r *Runtime) Enabled() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.instances) > 0
}

// PublishRecord fans out a finalized record to all loaded analyzers. Plugin
// failures are logged and counted; they never reach the inference response.
func (r *Runtime) PublishRecord(parent context.Context, rec *metrics.Record) {
	if r == nil || rec == nil {
		return
	}
	r.mu.Lock()
	instances := append([]*instance(nil), r.instances...)
	r.mu.Unlock()
	if len(instances) == 0 {
		return
	}
	event := pluginEvent{
		Type: "request.recorded",
		Seq:  r.seq.Add(1),
		Record: &pluginRecord{
			ID:                 rec.ID,
			Start:              rec.Start.Format(time.RFC3339Nano),
			Status:             rec.StatusCode,
			Provider:           rec.Provider,
			Model:              rec.Model,
			Client:             rec.Client,
			DurationMs:         rec.End.Sub(rec.Start).Milliseconds(),
			InputTokens:        rec.Usage.InputTokens,
			OutputTokens:       rec.Usage.OutputTokens,
			ErrorType:          rec.ErrorType,
			Cost:               rec.Cost,
			RateLimitRemaining: rec.RateLimitRemaining,
		},
	}
	input, err := json.Marshal(event)
	if err != nil {
		log.Printf("plugins: marshal event: %v", err)
		return
	}
	for _, inst := range instances {
		state := &hostState{input: input, manifest: inst.manifest, calls: map[string]int{}}
		ctx, cancel := context.WithTimeout(parent, pluginCallLimit)
		_, callErr := inst.handle.Call(withHostState(ctx, state))
		cancel()
		if callErr != nil {
			inst.failures.Add(1)
			log.Printf("plugins: %s handle failed: %v", inst.id, callErr)
		}
		if len(state.output) > 0 {
			r.last.Store(append([]byte(nil), state.output...))
			inst.lastOutput.Store(append([]byte(nil), state.output...))
		}
		r.lastDenials.Store(state.denials)
	}
}

// LastOutput is the most recent response document written by a guest. It is
// diagnostic state for tests and status reporting, never input to inference.
func (r *Runtime) LastOutput() []byte {
	if r == nil {
		return nil
	}
	value := r.last.Load()
	if value == nil {
		return nil
	}
	return append([]byte(nil), value.([]byte)...)
}

// LastDenials is the most recent count of denied host calls.
func (r *Runtime) LastDenials() int {
	if r == nil {
		return 0
	}
	value := r.lastDenials.Load()
	if value == nil {
		return 0
	}
	return value.(int)
}

// Failures counts guest call failures since boot.
func (r *Runtime) Failures() uint64 {
	if r == nil {
		return 0
	}
	var total uint64
	for _, inst := range r.instances {
		total += inst.failures.Load()
	}
	return total
}

// Close shuts down every guest and the runtime itself.
func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	var firstErr error
	for _, inst := range r.instances {
		state := &hostState{manifest: inst.manifest, calls: map[string]int{}}
		callCtx, cancel := context.WithTimeout(ctx, pluginCallLimit)
		_, err := inst.shutdown.Call(withHostState(callCtx, state))
		cancel()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := r.rt.Close(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func withHostState(ctx context.Context, state *hostState) context.Context {
	return context.WithValue(ctx, hostStateKey{}, state)
}

func currentState(ctx context.Context) *hostState {
	state, _ := ctx.Value(hostStateKey{}).(*hostState)
	return state
}

func readBoundedFile(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("file exceeds the %d byte cap", limit)
	}
	return b, nil
}

func registerHostFunctions(ctx context.Context, rt wazero.Runtime, owner *Runtime) (api.Module, error) {
	builder := rt.NewHostModuleBuilder("env")
	copyIn := func(ctx context.Context, mod api.Module, ptr, length uint32) uint32 {
		state := currentState(ctx)
		if state == nil || mod.Memory() == nil {
			return hostDenied
		}
		n := length
		if int(n) > len(state.input) {
			n = uint32(len(state.input))
		}
		if n > 0 && !mod.Memory().Write(ptr, state.input[:n]) {
			return hostDenied
		}
		return n
	}
	copyOut := func(ctx context.Context, mod api.Module, ptr, length uint32) uint32 {
		state := currentState(ctx)
		if state == nil || length > maxOutputBytes || mod.Memory() == nil {
			return hostDenied
		}
		b, ok := mod.Memory().Read(ptr, length)
		if !ok {
			return hostDenied
		}
		state.output = append([]byte(nil), b...)
		return 0
	}
	capabilityCall := func(capability string) func(context.Context, api.Module, uint32, uint32) uint32 {
		return func(ctx context.Context, _ api.Module, _, _ uint32) uint32 {
			state := currentState(ctx)
			if state == nil {
				return hostDenied
			}
			return state.allow(capability, capability)
		}
	}
	fetchRequest := func(ctx context.Context, mod api.Module, ptr, length uint32) uint32 {
		state := currentState(ctx)
		if state == nil || mod.Memory() == nil || length == 0 || length > maxFetchRequestBytes {
			return hostDenied
		}
		if code := state.allow("http:fetch", "http:fetch"); code != 0 {
			return code
		}
		raw, ok := mod.Memory().Read(ptr, length)
		if !ok {
			return hostDenied
		}
		result := fetchHTTP(ctx, raw, owner.httpAllowHosts)
		encoded, err := json.Marshal(result)
		if err != nil {
			return hostDenied
		}
		state.httpResponse = encoded
		return 0
	}
	fetchResponse := func(ctx context.Context, mod api.Module, ptr, capacity uint32) uint32 {
		state := currentState(ctx)
		if state == nil || mod.Memory() == nil || len(state.httpResponse) == 0 {
			return hostDenied
		}
		n := uint32(len(state.httpResponse))
		if n > capacity {
			n = capacity
		}
		if !mod.Memory().Write(ptr, state.httpResponse[:n]) {
			return hostDenied
		}
		return n
	}
	builder.NewFunctionBuilder().WithFunc(copyIn).Export("mv_read_input")
	builder.NewFunctionBuilder().WithFunc(copyOut).Export("mv_write_output")
	builder.NewFunctionBuilder().WithFunc(capabilityCall("log")).Export("mv_log")
	builder.NewFunctionBuilder().WithFunc(capabilityCall("emit_metric")).Export("mv_emit_metric")
	builder.NewFunctionBuilder().WithFunc(capabilityCall("read:records")).Export("mv_read_record")
	builder.NewFunctionBuilder().WithFunc(capabilityCall("read:config")).Export("mv_read_config_view")
	builder.NewFunctionBuilder().WithFunc(capabilityCall("declare_tool")).Export("mv_declare_tool")
	builder.NewFunctionBuilder().WithFunc(capabilityCall("declare_ui_panel")).Export("mv_declare_ui_panel")
	builder.NewFunctionBuilder().WithFunc(fetchRequest).Export("mv_http_fetch_request")
	builder.NewFunctionBuilder().WithFunc(fetchResponse).Export("mv_http_fetch_response")
	return builder.Instantiate(ctx)
}

// pluginEvent mirrors the SDK's request.recorded envelope.
type pluginEvent struct {
	Type   string        `json:"type"`
	Seq    uint64        `json:"seq"`
	Record *pluginRecord `json:"record,omitempty"`
}

type pluginRecord struct {
	ID                 string  `json:"id"`
	Start              string  `json:"start"`
	Status             int     `json:"status"`
	Provider           string  `json:"provider"`
	Model              string  `json:"model"`
	Client             string  `json:"client"`
	DurationMs         int64   `json:"duration_ms"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	ErrorType          string  `json:"error_type,omitempty"`
	Cost               float64 `json:"cost,omitempty"`
	RateLimitRemaining int     `json:"rate_limit_remaining,omitempty"`
}

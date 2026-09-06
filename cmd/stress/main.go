// stress measures the real, isolated dev proxy against an in-process local
// upstream. It never starts/stops a proxy or changes its configuration.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Synthetic fixture quantities, not production configuration.
const (
	fixtureInputTokens = 100
	fixtureCost        = .001
)

type options struct {
	target, levels, sources         string
	duration, hold, timeout, sample time.Duration
	chunks, upstreams               int
	maxRSS                          float64
	token                           string
}

func main() {
	var o options
	flag.StringVar(&o.target, "target", "", "required dev URL (loopback only; port 8080 forbidden)")
	flag.StringVar(&o.levels, "concurrency", "1,8,32,128,512", "comma-separated simultaneous client workers")
	flag.StringVar(&o.sources, "source-ips", "", "optional comma-separated loopback source IPs; expands the client tuple budget without OS tuning")
	flag.IntVar(&o.upstreams, "upstreams", 1, "number of local mock destinations; >1 measures aggregate multi-upstream capacity")
	flag.DurationVar(&o.duration, "duration", 5*time.Second, "time starting requests at each level; active requests then drain")
	flag.DurationVar(&o.hold, "stream-duration", time.Second, "upstream stream duration; 0 measures fast responses")
	flag.IntVar(&o.chunks, "chunks", 10, "content frames per upstream response")
	flag.DurationVar(&o.timeout, "timeout", 10*time.Second, "per-request, accounting-drain and final fixture-cleanup timeout")
	flag.DurationVar(&o.sample, "sample", 100*time.Millisecond, "proxy CPU/RSS/FD sampling cadence")
	flag.Float64Var(&o.maxRSS, "max-rss-mib", 2048, "stop the ramp if sampled proxy RSS exceeds this safety budget")
	// The final fixture cleanup purges through the gated operator plane, so
	// the tool needs the same credential the dev instance was started with.
	o.token = os.Getenv("MILLIVOLT_OPERATOR_TOKEN")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, "stress:", err)
		os.Exit(1)
	}
}

func validate(o options) (*url.URL, []int, error) {
	u, err := url.Parse(o.target)
	if err != nil {
		return nil, nil, err
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if err != nil || ip == nil || !ip.IsLoopback() || u.Scheme != "http" || port < 1024 || port > 65535 || port == 8080 || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, nil, errors.New("target must be an explicit http://loopback-IP:dev-port URL, never production :8080")
	}
	if o.token == "" {
		return nil, nil, errors.New("MILLIVOLT_OPERATOR_TOKEN is required: the dev instance's operator credential owns fixture cleanup")
	}
	if o.duration <= 0 || o.hold < 0 || o.chunks < 1 || o.upstreams < 1 || o.timeout <= o.hold || o.sample <= 0 || o.maxRSS <= 0 || math.IsNaN(o.maxRSS) || math.IsInf(o.maxRSS, 0) {
		return nil, nil, errors.New("invalid workload or safety bounds")
	}
	if _, err := sourceIPs(o.sources, ip); err != nil {
		return nil, nil, err
	}
	var levels []int
	for _, part := range strings.Split(o.levels, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 {
			return nil, nil, errors.New("concurrency levels must be positive integers")
		}
		levels = append(levels, n)
	}
	return u, levels, nil
}

func sourceIPs(value string, target net.IP) ([]net.IP, error) {
	if value == "" {
		return []net.IP{nil}, nil // let the kernel select the ordinary source
	}
	var out []net.IP
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		ip := net.ParseIP(strings.TrimSpace(part))
		if ip == nil || !ip.IsLoopback() || (ip.To4() == nil) != (target.To4() == nil) || seen[ip.String()] {
			return nil, errors.New("source IPs must be unique loopback addresses matching the target address family")
		}
		seen[ip.String()] = true
		out = append(out, ip)
	}
	return out, nil
}

func localClient(timeout time.Duration, transport *http.Transport) *http.Client {
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// operatorTransport injects the operator credential into every control-plane
// read: the dashboard plane is gated and the tool must behave like an
// authenticated operator. Inference traffic uses a different, plain client.
type operatorTransport struct {
	base  http.RoundTripper
	token string
}

func (t operatorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// Only these nonsecret settings explain the measured workload. Never emit
// the whole effective config: provider-injected headers can hold credentials.
func performanceSettings(effective map[string]any) map[string]any {
	out := make(map[string]any)
	for _, key := range []string{"max_conns_per_host", "max_idle_conns", "max_idle_conns_per_host", "storage_write_chan_cap", "storage_batch_cap", "storage_flush_interval", "upstream_timeout", "max_concurrent", "max_queue_size", "max_queue_wait", "max_retries", "quality_retries"} {
		if value, ok := effective[key]; ok {
			out[key] = value
		}
	}
	return out
}

type control struct {
	client *http.Client
	base   string
}

// The process-local signal is read outside the timed workload. It distinguishes
// an accounting shortfall from a confirmed durable-write drop without guessing.
type storageState struct {
	FeedID  string `json:"feed_id"`
	Storage struct {
		Enabled bool   `json:"enabled"`
		Dropped uint64 `json:"dropped"`
	} `json:"storage"`
}

func storageDropDelta(before, after storageState) (uint64, error) {
	if !before.Storage.Enabled || !after.Storage.Enabled || before.FeedID == "" || before.FeedID != after.FeedID || after.Storage.Dropped < before.Storage.Dropped {
		return 0, errors.New("storage signal changed epoch or became unavailable during workload")
	}
	return after.Storage.Dropped - before.Storage.Dropped, nil
}

func (c control) get(ctx context.Context, path string, dst any) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s returned HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

type settings struct {
	Path            string            `json:"path"`
	RestartRequired []string          `json:"restart_required"`
	Overrides       map[string]string `json:"overrides"`
	Effective       map[string]any    `json:"effective"`
}

// Match dev.sh's reserved disposable namespace, including the explicit database
// override. A saved setting can differ from the running database during restart.
func validateDevSettings(cfg settings, u *url.URL) error {
	expected := "/tmp/millivolt/millivolt-dev-" + u.Port()
	if cfg.Path != expected+".yaml" || cfg.Overrides["listen"] != u.Host {
		return errors.New("target is not a scripts/dev.sh private-config instance")
	}
	if slices.Contains(cfg.RestartRequired, "db_path") {
		return errors.New("target has an ambiguous pending database restart")
	}
	override := cfg.Overrides["db_path"]
	effective, ok := cfg.Effective["db_path"].(string)
	if !ok || override == "" {
		return errors.New("target requires an explicit dev database override")
	}
	if override == "none" {
		if effective != "" {
			return errors.New("disabled database override does not match effective state")
		}
		return nil
	}
	if effective != override || filepath.Clean(effective) != effective ||
		filepath.Dir(effective) != filepath.Dir(expected) ||
		!strings.HasPrefix(filepath.Base(effective), "millivolt-dev") ||
		!strings.HasSuffix(effective, ".db") {
		return errors.New("target database is outside the private dev namespace")
	}
	return nil
}

// Files are checked without reading their contents. Reject symlink aliases and
// hard links: a disposable-looking path must not name another instance's file.
func privateFile(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("private dev file is missing or has a symlink alias")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("private dev file is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return errors.New("private dev file has ambiguous link ownership")
	}
	return nil
}

func verifyDev(ctx context.Context, c control, u *url.URL) (settings, int, error) {
	var cfg settings
	if err := c.get(ctx, "/admin/config", &cfg); err != nil {
		return cfg, 0, err
	}
	expected := "/tmp/millivolt/millivolt-dev-" + u.Port()
	if err := validateDevSettings(cfg, u); err != nil {
		return cfg, 0, err
	}
	if err := privateFile(cfg.Path); err != nil {
		return cfg, 0, err
	}
	if cfg.Overrides["db_path"] != "none" {
		if err := privateFile(cfg.Overrides["db_path"]); err != nil {
			return cfg, 0, err
		}
	}
	var status struct {
		PID int `json:"pid"`
	}
	if err := c.get(ctx, "/admin/restart", &status); err != nil {
		return cfg, 0, err
	}
	args, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", status.PID))
	if err != nil || !strings.Contains(string(args), "-pid-file\x00"+expected+".pid\x00") {
		return cfg, 0, errors.New("dev PID identity could not be verified")
	}
	return cfg, status.PID, nil
}

// clearClients runs only after generators have drained. Verify ownership at
// each mutation after waiting for known pending records at the deletion fence;
// otherwise a canceled request could finalize after its fixture was removed.
func (c control) clearClients(ctx context.Context, tags []string, sample time.Duration, verify func(context.Context) error, token string) error {
	for _, tag := range tags {
		if tag == "" {
			return errors.New("empty fixture client cannot be cleaned up")
		}
	}
	if len(tags) == 0 {
		return nil
	}
	for {
		var snapshot struct {
			FeedID  string          `json:"feed_id"`
			Pending json.RawMessage `json:"in_flight_records"`
		}
		if err := c.get(ctx, "/metrics/bootstrap", &snapshot); err != nil {
			return err
		}
		if snapshot.FeedID == "" || len(snapshot.Pending) == 0 {
			return errors.New("fixture cleanup requires a complete pending snapshot")
		}
		var pending []struct {
			Client string `json:"client"`
		}
		if err := json.Unmarshal(snapshot.Pending, &pending); err != nil {
			return errors.New("invalid fixture pending snapshot")
		}
		busy := false
		for _, row := range pending {
			busy = busy || slices.Contains(tags, row.Client)
		}
		if !busy {
			break
		}
		select {
		case <-time.After(sample):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, tag := range tags {
		if verify == nil {
			return errors.New("fixture cleanup requires an ownership verifier")
		}
		if err := verify(ctx); err != nil {
			return err
		}
		// The administrative filter's client field is an exact match. Encode a
		// nonempty object, never an empty body (which means all history).
		body, err := json.Marshal(struct {
			Client string `json:"client"`
		}{tag})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/admin/purge", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		var result struct {
			OK bool `json:"ok"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil || !result.OK {
			return fmt.Errorf("fixture cleanup failed for %s (HTTP %d)", tag, resp.StatusCode)
		}
	}
	return nil
}

type processSample struct {
	cpuTicks uint64
	rssMiB   float64
	fds      int
}

func readProcess(pid int) (processSample, error) {
	var s processSample
	base := fmt.Sprintf("/proc/%d/", pid)
	raw, err := os.ReadFile(base + "stat")
	if err != nil {
		return s, err
	}
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return s, errors.New("invalid process stat")
	}
	f := strings.Fields(string(raw[i+1:])) // starts at field 3 (state)
	if len(f) < 22 {
		return s, errors.New("short process stat")
	}
	u, err := strconv.ParseUint(f[11], 10, 64)
	if err != nil {
		return s, err
	}
	sys, err := strconv.ParseUint(f[12], 10, 64)
	if err != nil {
		return s, err
	}
	s.cpuTicks = u + sys
	rss, err := strconv.ParseUint(f[21], 10, 64)
	if err != nil {
		return s, err
	}
	s.rssMiB = float64(rss) * float64(os.Getpagesize()) / (1 << 20)
	fds, err := os.ReadDir(base + "fd")
	if err != nil {
		return s, err
	}
	s.fds = len(fds)
	return s, nil
}

type fixture struct {
	o                       options
	current, peak, accepted atomic.Int64
	frames                  []string
	expected                string
}

func newFixture(o options) *fixture {
	f := &fixture{o: o}
	for range o.chunks {
		f.frames = append(f.frames, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
	}
	f.frames = append(f.frames, fmt.Sprintf("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"cost\":%g}}\n\ndata: [DONE]\n\n", fixtureInputTokens, o.chunks, fixtureCost))
	f.expected = strings.Join(f.frames, "")
	return f
}

func (f *fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return
	}
	n := f.current.Add(1)
	f.accepted.Add(1)
	defer f.current.Add(-1)
	for old := f.peak.Load(); n > old && !f.peak.CompareAndSwap(old, n); old = f.peak.Load() {
	}
	w.Header().Set("Content-Type", "text/event-stream")
	var timer *time.Timer
	if f.o.hold > 0 {
		timer = time.NewTimer(f.o.hold / time.Duration(f.o.chunks))
		defer timer.Stop()
	}
	for i, frame := range f.frames {
		if _, err := io.WriteString(w, frame); err != nil {
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		if timer != nil && i < f.o.chunks {
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
			timer.Reset(f.o.hold / time.Duration(f.o.chunks))
		}
	}
}

type outcome struct {
	elapsed, first time.Duration
	response       bool
	err            error
}
type result struct {
	Concurrency                                  int   `json:"concurrency"`
	UpstreamPeak                                 int64 `json:"upstream_peak"`
	Requests, HTTPResponses, Successes, Failures int
	UpstreamAccepted                             int64
	Seconds, RequestsPerSecond                   float64
	P50Ms, P95Ms, P99Ms, FirstByteP95Ms          float64
	CPUPercent, PeakRSSMiB                       float64
	GeneratorCPUPercent, GeneratorPeakRSSMiB     float64
	PeakFDs                                      int
	GeneratorPeakFDs                             int
	DurableRequests                              int64
	DurableCost                                  float64
	StorageDropped                               *uint64 `json:"storage_dropped,omitempty"`
	AccountingOK                                 bool
	AccountingComparable                         bool
	Errors                                       []string `json:"errors,omitempty"`
}

func percentileMs(v []int64, pct float64) float64 {
	slices.Sort(v)
	return metrics.Percentile(v, pct) / float64(time.Millisecond)
}

func stage(ctx context.Context, c control, clients []*http.Client, pid int, hz float64, o options, upstreams []string, f *fixture, concurrency int, durable bool, tag string) (result, error) {
	res := result{Concurrency: concurrency}
	startSample, err := readProcess(pid)
	if err != nil {
		return res, err
	}
	res.PeakRSSMiB, res.PeakFDs = startSample.rssMiB, startSample.fds
	if startSample.rssMiB > o.maxRSS {
		return res, errors.New("proxy already exceeds RSS safety budget; no load started")
	}
	generatorStart, err := readProcess(os.Getpid())
	if err != nil {
		return res, err
	}
	res.GeneratorPeakRSSMiB, res.GeneratorPeakFDs = generatorStart.rssMiB, generatorStart.fds
	f.peak.Store(0)
	f.accepted.Store(0)
	payload := `{"model":"load-fixture","stream":true,"messages":[{"role":"user","content":"synthetic local capacity test"}]}`
	results := make(chan outcome, concurrency)
	workCtx, stop := context.WithCancel(ctx)
	defer stop()
	started := time.Now()
	deadline := started.Add(o.duration)
	var stopStarting atomic.Bool
	var wg sync.WaitGroup
	for worker := range concurrency {
		wg.Go(func() {
			client := clients[worker%len(clients)]
			upstream := upstreams[worker%len(upstreams)]
			for !stopStarting.Load() && time.Now().Before(deadline) && workCtx.Err() == nil {
				begin := time.Now()
				req, err := http.NewRequestWithContext(workCtx, http.MethodPost, c.base+"/v1/chat/completions", strings.NewReader(payload))
				if err != nil {
					stopStarting.Store(true)
					results <- outcome{err: err}
					return
				}
				req.Header.Set("Authorization", "Bearer synthetic-local-load-key")
				req.Header.Set("X-Proxy-Base-URL", upstream)
				req.Header.Set("X-Proxy-Client", tag)
				req.Header.Set("Content-Type", "application/json")
				r, err := client.Do(req)
				var first time.Duration
				if err == nil {
					one := make([]byte, 1)
					n, readErr := r.Body.Read(one)
					first = time.Since(begin)
					rest, bodyErr := io.ReadAll(r.Body)
					r.Body.Close()
					if r.StatusCode != 200 || (readErr != nil && readErr != io.EOF) || bodyErr != nil || string(one[:n])+string(rest) != f.expected {
						err = fmt.Errorf("status=%d bytes=%d read=%v/%v (expected %d unchanged bytes)", r.StatusCode, n+len(rest), readErr, bodyErr, len(f.expected))
					}
				}
				if err != nil {
					stopStarting.Store(true)
				}
				results <- outcome{elapsed: time.Since(begin), first: first, response: r != nil, err: err}
			}
		})
	}
	go func() { wg.Wait(); close(results) }()
	tick := time.NewTicker(o.sample)
	defer tick.Stop()
	var latency, first []int64
	var sampleErr error
loop:
	for {
		select {
		case r, ok := <-results:
			if !ok {
				break loop
			}
			res.Requests++
			if r.response {
				res.HTTPResponses++
			}
			if r.err != nil {
				res.Failures++
				if len(res.Errors) < 3 {
					res.Errors = append(res.Errors, r.err.Error())
				}
			} else {
				res.Successes++
				latency = append(latency, int64(r.elapsed))
				first = append(first, int64(r.first))
			}
		case <-tick.C:
			s, err := readProcess(pid)
			if err != nil {
				sampleErr = err
				stop()
				continue
			}
			res.PeakRSSMiB = max(res.PeakRSSMiB, s.rssMiB)
			res.PeakFDs = max(res.PeakFDs, s.fds)
			g, err := readProcess(os.Getpid())
			if err != nil {
				sampleErr = err
				stop()
				continue
			}
			res.GeneratorPeakRSSMiB = max(res.GeneratorPeakRSSMiB, g.rssMiB)
			res.GeneratorPeakFDs = max(res.GeneratorPeakFDs, g.fds)
			if s.rssMiB > o.maxRSS {
				sampleErr = errors.New("proxy exceeded RSS safety budget")
				stop()
			}
		}
	}
	res.Seconds = time.Since(started).Seconds()
	endSample, err := readProcess(pid)
	if err != nil {
		return res, err
	}
	res.CPUPercent = float64(endSample.cpuTicks-startSample.cpuTicks) / hz / res.Seconds * 100
	generatorEnd, err := readProcess(os.Getpid())
	if err != nil {
		return res, err
	}
	res.GeneratorCPUPercent = float64(generatorEnd.cpuTicks-generatorStart.cpuTicks) / hz / res.Seconds * 100
	res.GeneratorPeakRSSMiB = max(res.GeneratorPeakRSSMiB, generatorEnd.rssMiB)
	res.GeneratorPeakFDs = max(res.GeneratorPeakFDs, generatorEnd.fds)
	res.PeakRSSMiB = max(res.PeakRSSMiB, endSample.rssMiB)
	res.PeakFDs = max(res.PeakFDs, endSample.fds)
	res.UpstreamPeak = f.peak.Load()
	res.UpstreamAccepted = f.accepted.Load()
	res.RequestsPerSecond = float64(res.Successes) / res.Seconds
	res.P50Ms = percentileMs(latency, 50)
	res.P95Ms = percentileMs(latency, 95)
	res.P99Ms = percentileMs(latency, 99)
	res.FirstByteP95Ms = percentileMs(first, 95)
	if sampleErr != nil {
		return res, sampleErr
	}
	if durable {
		// Separate durable acceptance from HTTP success; the async writer may
		// intentionally drop rather than block when its queue is saturated.
		query := fmt.Sprintf("SELECT count(*) AS n, coalesce(sum(input_tokens),0) AS input, coalesce(sum(output_tokens),0) AS output, coalesce(sum(cost),0) AS cost FROM requests WHERE client='%s'", tag)
		limit := time.Now().Add(o.timeout)
		res.AccountingComparable = res.Failures == 0
		for {
			var rows []struct {
				N, Input, Output int64
				Cost             float64
			}
			if err := c.get(ctx, "/metrics/query?q="+url.QueryEscape(query), &rows); err != nil {
				return res, err
			}
			if len(rows) != 1 {
				return res, errors.New("invalid durable accounting result")
			}
			res.DurableRequests = rows[0].N
			res.DurableCost = rows[0].Cost
			expectedCost := float64(res.Successes) * fixtureCost
			// Summing binary floating-point currency introduces rounding only;
			// the tolerance is numerical, not a permitted missing-record budget.
			costOK := !math.IsNaN(rows[0].Cost) && !math.IsInf(rows[0].Cost, 0) && math.Abs(rows[0].Cost-expectedCost) <= max(1, expectedCost)*1e-9
			res.AccountingOK = res.AccountingComparable && rows[0].N == int64(res.Requests) && rows[0].Input == int64(res.Requests)*fixtureInputTokens && rows[0].Output == int64(res.Requests)*int64(o.chunks) && costOK
			// A dial failure never entered the proxy; failed attempts cannot
			// establish an exact accepted-record expectation. Do not label
			// that mismatch a storage drop.
			if res.AccountingOK || (!res.AccountingComparable && rows[0].N >= int64(res.Successes)) || time.Now().After(limit) {
				break
			}
			select {
			case <-time.After(o.sample):
			case <-ctx.Done():
				return res, ctx.Err()
			}
		}
	}
	return res, ctx.Err()
}

func run(ctx context.Context, o options) (err error) {
	u, levels, err := validate(o)
	if err != nil {
		return err
	}
	c := control{&http.Client{Transport: operatorTransport{base: &http.Transport{}, token: o.token},
		Timeout: o.timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		strings.TrimRight(o.target, "/")}
	defer c.client.CloseIdleConnections()
	cfg, pid, err := verifyDev(ctx, c, u)
	if err != nil {
		return err
	}
	var tags []string
	defer func() {
		if len(tags) == 0 {
			return
		}
		// Cleanup is outside all stage timing/drop measurements, so later ramp
		// levels retain the existing cumulative-history baseline. Cancellation
		// does not abandon fixtures, and the existing timeout bounds all cleanup.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.timeout)
		defer cancel()
		cleanupErr := c.clearClients(cleanupCtx, tags, o.sample, func(ctx context.Context) error {
			current, currentPID, err := verifyDev(ctx, c, u)
			if err != nil {
				return err
			}
			if currentPID != pid || current.Overrides["db_path"] != cfg.Overrides["db_path"] {
				return errors.New("dev identity changed; fixture cleanup refused")
			}
			return nil
		}, o.token)
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup incomplete for clients %v: %w", tags, cleanupErr))
		}
	}()
	ticks, err := exec.CommandContext(ctx, "getconf", "CLK_TCK").Output()
	if err != nil {
		return err
	}
	hz, err := strconv.ParseFloat(strings.TrimSpace(string(ticks)), 64)
	if err != nil || hz <= 0 {
		return errors.New("invalid CLK_TCK")
	}
	f := newFixture(o)
	var upstreams []string
	for range o.upstreams {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		server := &http.Server{Handler: f, ReadHeaderTimeout: o.timeout}
		defer server.Close()
		go server.Serve(ln)
		upstreams = append(upstreams, "http://"+ln.Addr().String())
	}
	sources, _ := sourceIPs(o.sources, net.ParseIP(u.Hostname())) // validated above
	var clients []*http.Client
	for _, source := range sources {
		idle := (slices.Max(levels) + len(sources) - 1) / len(sources)
		transport := &http.Transport{MaxIdleConns: idle, MaxIdleConnsPerHost: idle}
		if source != nil {
			dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: source}}
			transport.DialContext = dialer.DialContext
		}
		defer transport.CloseIdleConnections()
		clients = append(clients, localClient(o.timeout, transport))
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(map[string]any{"kind": "workload", "pid": pid, "target": c.base, "model": "closed-loop", "stream_duration": o.hold.String(), "chunks": o.chunks, "duration_per_level": o.duration.String(), "upstreams": o.upstreams, "source_ips": o.sources, "performance_settings": performanceSettings(cfg.Effective)}); err != nil {
		return err
	}
	durable := cfg.Effective["db_path"] != ""
	for _, n := range levels {
		var before storageState
		if durable {
			if err := c.get(ctx, "/metrics/bootstrap", &before); err != nil {
				return err
			}
		}
		tag := "stress-" + rand.Text()
		tags = append(tags, tag) // before the first attempt, including failures
		r, err := stage(ctx, c, clients, pid, hz, o, upstreams, f, n, durable, tag)
		if err == nil && durable {
			var after storageState
			err = c.get(ctx, "/metrics/bootstrap", &after)
			if err == nil {
				var dropped uint64
				dropped, err = storageDropDelta(before, after)
				if err == nil {
					r.StorageDropped = &dropped
				}
			}
		}
		if e := enc.Encode(r); e != nil {
			return e
		}
		if err != nil {
			return err
		}
		if r.Failures > 0 || (durable && !r.AccountingOK) || (r.StorageDropped != nil && *r.StorageDropped > 0) {
			return errors.New("ramp stopped: response or durable-accounting failure")
		}
	}
	return nil
}

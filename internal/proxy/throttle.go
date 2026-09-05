package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// Persistent provider-wide caps. Set from the dashboard or from
// X-Proxy-Limit-* headers: one client writes, every client of that
// provider is limited until the cap is changed or turned off.

const (
	hdrLimitConcurrency = "X-Proxy-Limit-Concurrency"
	hdrLimitRequests    = "X-Proxy-Limit-Requests"
	hdrLimitTokens      = "X-Proxy-Limit-Tokens"
)

// Safety guardrails on a header/UI-supplied cap. Not user-tunable: they
// bound a hostile or typo'd value so it cannot stall the provider forever
// or overflow the token bucket. Window is 1s..24h (same ceiling as
// maxRetryHint - a daily quota window).
const (
	maxLimitConcurrency = 100_000
	maxLimitRequests    = 1_000_000_000
	maxLimitTokens      = 1_000_000_000_000
	minLimitWindow      = time.Second
	maxLimitWindow      = 24 * time.Hour
)

type throttlePersist interface {
	SaveThrottle(ctx context.Context, raw []byte) error
	LoadThrottle(ctx context.Context) ([]byte, error)
}

type persistedThrottle struct {
	Provider    string    `json:"provider"`
	Concurrency int       `json:"concurrency,omitempty"`
	Requests    int64     `json:"requests,omitempty"`
	ReqWindow   string    `json:"request_window,omitempty"`
	Tokens      int64     `json:"tokens,omitempty"`
	TokWindow   string    `json:"token_window,omitempty"`
	Source      string    `json:"source,omitempty"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

type persistedThrottleDoc struct {
	Throttles []persistedThrottle `json:"throttles,omitempty"`
}

type throttleRuntime struct {
	persist    throttlePersist
	mu         sync.Mutex
	persistGen uint64
}

func (s *Server) attachThrottlePersist(p throttlePersist) {
	s.throttle.persist = p
	ctx, cancel := s.storeQueryCtx()
	defer cancel()
	raw, err := p.LoadThrottle(ctx)
	if err != nil {
		log.Printf("throttle: load: %v", err)
		return
	}
	items := decodeThrottles(raw)
	if len(items) == 0 {
		return
	}
	var ts []scheduler.Throttle
	for _, it := range items {
		t, err := persistedToThrottle(it)
		if err != nil {
			log.Printf("throttle: skip %s: %v", it.Provider, err)
			continue
		}
		s.noteProvider(t.Provider)
		ts = append(ts, t)
	}
	s.scheduler.RestoreThrottles(ts)
	if len(ts) > 0 {
		log.Printf("throttle restored (%d provider caps)", len(ts))
	}
}

func decodeThrottles(raw []byte) []persistedThrottle {
	if len(raw) == 0 {
		return nil
	}
	var doc persistedThrottleDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("throttle: decode: %v", err)
		return nil
	}
	return doc.Throttles
}

func persistedToThrottle(it persistedThrottle) (scheduler.Throttle, error) {
	if strings.TrimSpace(it.Provider) == "" {
		return scheduler.Throttle{}, fmt.Errorf("provider required")
	}
	lim := scheduler.Limit{Concurrency: it.Concurrency, Requests: it.Requests, Tokens: it.Tokens}
	if it.Requests > 0 {
		d, err := parseLimitWindow(it.ReqWindow)
		if err != nil {
			return scheduler.Throttle{}, err
		}
		lim.ReqWindow = d
	}
	if it.Tokens > 0 {
		d, err := parseLimitWindow(it.TokWindow)
		if err != nil {
			return scheduler.Throttle{}, err
		}
		lim.TokWindow = d
	}
	if err := validateLimit(lim); err != nil {
		return scheduler.Throttle{}, err
	}
	return scheduler.Throttle{
		Provider:  it.Provider,
		Limit:     lim,
		Source:    it.Source,
		UpdatedBy: it.UpdatedBy,
		UpdatedAt: it.UpdatedAt,
	}, nil
}

func throttleToPersisted(t scheduler.Throttle) persistedThrottle {
	it := persistedThrottle{
		Provider:    t.Provider,
		Concurrency: t.Limit.Concurrency,
		Requests:    t.Limit.Requests,
		Tokens:      t.Limit.Tokens,
		Source:      t.Source,
		UpdatedBy:   t.UpdatedBy,
		UpdatedAt:   t.UpdatedAt,
	}
	if t.Limit.ReqWindow > 0 {
		it.ReqWindow = config.FormatDuration(t.Limit.ReqWindow)
	}
	if t.Limit.TokWindow > 0 {
		it.TokWindow = config.FormatDuration(t.Limit.TokWindow)
	}
	return it
}

func (s *Server) applyThrottle(t scheduler.Throttle) error {
	return s.updateThrottle(t.Provider, func(scheduler.Throttle) scheduler.Throttle { return t })
}

func (s *Server) updateThrottle(provider string, update func(scheduler.Throttle) scheduler.Throttle) error {
	if !s.scheduler.UpdateThrottle(provider, update) {
		return nil
	}
	s.noteProvider(provider)
	return s.persistThrottles()
}

func (s *Server) persistThrottles() error {
	s.throttle.mu.Lock()
	s.throttle.persistGen++
	gen := s.throttle.persistGen
	s.throttle.mu.Unlock()
	for {
		items := s.scheduler.ListThrottles()
		snap := make([]persistedThrottle, 0, len(items))
		for _, inf := range items {
			snap = append(snap, throttleToPersisted(inf.Throttle))
		}
		err := s.writeThrottleSnap(snap)
		s.throttle.mu.Lock()
		if s.throttle.persistGen == gen {
			s.throttle.mu.Unlock()
			if err != nil {
				return &operatorPersistenceError{err}
			}
			return nil
		}
		gen = s.throttle.persistGen
		s.throttle.mu.Unlock()
	}
}

func (s *Server) writeThrottleSnap(items []persistedThrottle) error {
	if s.throttle.persist == nil {
		return nil
	}
	raw, err := json.Marshal(persistedThrottleDoc{Throttles: items})
	if err != nil {
		log.Printf("throttle: persist: %v", err)
		return err
	}
	ctx, cancel := s.storeQueryCtx()
	defer cancel()
	if err := s.throttle.persist.SaveThrottle(ctx, raw); err != nil {
		log.Printf("throttle: persist: %v", err)
		return err
	}
	return nil
}

// applyThrottleHeaders parses X-Proxy-Limit-* on this request. Absent
// headers leave the current cap alone. Any present header that is
// malformed rejects the request WITHOUT applying the others (atomic).
// A well-formed set merges into the provider cap and persists.
func (s *Server) applyThrottleHeaders(r *http.Request, provider, client string) error {
	if provider == "" {
		return nil
	}
	hasC := headerPresent(r, hdrLimitConcurrency)
	hasR := headerPresent(r, hdrLimitRequests)
	hasT := headerPresent(r, hdrLimitTokens)
	if !hasC && !hasR && !hasT {
		return nil
	}
	// Parse/validate the patch before taking the policy lock. Applying it below
	// merges against the latest cap, so concurrent requests changing different
	// dimensions cannot overwrite each other's updates.
	var next scheduler.Limit
	if hasC {
		n, off, err := parseLimitInt(r.Header.Get(hdrLimitConcurrency), maxLimitConcurrency)
		if err != nil {
			return fmt.Errorf("invalid %s %q", hdrLimitConcurrency, r.Header.Get(hdrLimitConcurrency))
		}
		if off {
			next.Concurrency = 0
		} else {
			next.Concurrency = int(n)
		}
	}
	if hasR {
		n, win, off, err := parseLimitRate(r.Header.Get(hdrLimitRequests), maxLimitRequests)
		if err != nil {
			return fmt.Errorf("invalid %s %q", hdrLimitRequests, r.Header.Get(hdrLimitRequests))
		}
		if off {
			next.Requests = 0
			next.ReqWindow = 0
		} else {
			next.Requests = n
			next.ReqWindow = win
		}
	}
	if hasT {
		n, win, off, err := parseLimitRate(r.Header.Get(hdrLimitTokens), maxLimitTokens)
		if err != nil {
			return fmt.Errorf("invalid %s %q", hdrLimitTokens, r.Header.Get(hdrLimitTokens))
		}
		if off {
			next.Tokens = 0
			next.TokWindow = 0
		} else {
			next.Tokens = n
			next.TokWindow = win
		}
	}
	if err := validateLimit(next); err != nil {
		return err
	}
	// Persistence failures are logged by the writer. The validated cap is
	// already active; a save failure must not reject the inference request.
	_ = s.updateThrottle(provider, func(t scheduler.Throttle) scheduler.Throttle {
		if hasC {
			t.Limit.Concurrency = next.Concurrency
		}
		if hasR {
			t.Limit.Requests, t.Limit.ReqWindow = next.Requests, next.ReqWindow
		}
		if hasT {
			t.Limit.Tokens, t.Limit.TokWindow = next.Tokens, next.TokWindow
		}
		t.Source = scheduler.ThrottleSourceHeader
		t.UpdatedBy = client
		t.UpdatedAt = time.Time{}
		return t
	})
	return nil
}

func validateLimit(l scheduler.Limit) error {
	if l.Concurrency < 0 || l.Concurrency > maxLimitConcurrency {
		return fmt.Errorf("concurrency must be 0..%d", maxLimitConcurrency)
	}
	if l.Requests < 0 || l.Requests > maxLimitRequests {
		return fmt.Errorf("requests must be 0..%d", maxLimitRequests)
	}
	if l.Tokens < 0 || l.Tokens > maxLimitTokens {
		return fmt.Errorf("tokens must be 0..%d", maxLimitTokens)
	}
	if l.Requests > 0 {
		if l.ReqWindow < minLimitWindow || l.ReqWindow > maxLimitWindow {
			return fmt.Errorf("request window must be %s..%s", minLimitWindow, maxLimitWindow)
		}
	}
	if l.Tokens > 0 {
		if l.TokWindow < minLimitWindow || l.TokWindow > maxLimitWindow {
			return fmt.Errorf("token window must be %s..%s", minLimitWindow, maxLimitWindow)
		}
	}
	return nil
}

func parseLimitInt(s string, max int64) (n int64, off bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, fmt.Errorf("empty")
	}
	if isLimitOff(s) {
		return 0, true, nil
	}
	n, err = strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > max {
		return 0, false, fmt.Errorf("out of range")
	}
	return n, false, nil
}

func parseLimitRate(s string, max int64) (n int64, window time.Duration, off bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false, fmt.Errorf("empty")
	}
	if isLimitOff(s) {
		return 0, 0, true, nil
	}
	countStr, winStr, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0, false, fmt.Errorf("want COUNT/WINDOW")
	}
	n, err = strconv.ParseInt(strings.TrimSpace(countStr), 10, 64)
	if err != nil || n <= 0 || n > max {
		return 0, 0, false, fmt.Errorf("count out of range")
	}
	window, err = parseLimitWindow(strings.TrimSpace(winStr))
	if err != nil {
		return 0, 0, false, err
	}
	return n, window, false, nil
}

func isLimitOff(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "off", "none", "unlimited":
		return true
	}
	return false
}

func parseLimitWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("window required")
	}
	// Accept 1d / 2d as 24h units (time.ParseDuration has no 'd').
	if len(s) > 1 && (s[len(s)-1] == 'd' || s[len(s)-1] == 'D') {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid window %q", s)
		}
		d := time.Duration(n * float64(24*time.Hour))
		if d < minLimitWindow || d > maxLimitWindow {
			return 0, fmt.Errorf("window out of range")
		}
		return d, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid window %q", s)
	}
	if d < minLimitWindow || d > maxLimitWindow {
		return 0, fmt.Errorf("window out of range")
	}
	return d, nil
}

// estimateTokens is the Acquire-time reservation: prompt chars/4 plus
// req_max_tokens when the client sent one. Zero means "don't reserve;
// settle actual usage when the response ends".
func estimateTokens(rec *metrics.Record) int64 {
	if rec == nil {
		return 0
	}
	chars := rec.CharsSystem + rec.CharsUser + rec.CharsAssistant + rec.CharsTool
	est := int64(chars / 4)
	if rec.ReqMaxTokens != nil && *rec.ReqMaxTokens > 0 {
		est += int64(*rec.ReqMaxTokens)
	}
	return est
}

// settleTokens is the actual usage charged against the token window.
// Prefer the provider's total; otherwise input+output. Reasoning is a
// subset of output and is not added. Zero refunds the reservation. Impossible
// usage remains explicitly unavailable and retains the admitted reservation.
func settleTokens(rec *metrics.Record) int64 {
	if rec == nil {
		return 0
	}
	if rec.ErrorCode == metrics.CodeInvalidUsage {
		return scheduler.TokensUnavailable
	}
	n, err := metrics.SumCounts(rec.Usage.InputTokens, rec.Usage.OutputTokens)
	if err != nil || rec.Usage.TotalTokens < 0 {
		invalidateUsage(rec, metrics.ErrMetricRange)
		return scheduler.TokensUnavailable
	}
	if rec.Usage.TotalTokens > 0 {
		return rec.Usage.TotalTokens
	}
	return n
}

// invalidateUsage owns the explicit unavailable signal shared by protocol
// adapters and final quota settlement. Existing failure detail is preserved.
func invalidateUsage(rec *metrics.Record, err error) {
	rec.Usage = metrics.Usage{}
	rec.GenTokens, rec.AnswerTokens = 0, 0
	rec.ErrorCode = metrics.CodeInvalidUsage
	if rec.ErrorType == "" {
		rec.ErrorType = metrics.CodeInvalidUsage
	}
	if rec.ErrorMsg == "" {
		rec.ErrorMsg = err.Error()
	}
}

// HandleThrottle is GET/POST /admin/throttle. GET returns every active
// cap plus known providers (history ∪ current caps) so the dashboard can
// set a limit on a seen provider. POST requires {"provider"} (deny by
// default). clear:true drops the cap. Otherwise any supplied dimension
// merges; omitted dimensions keep their current value; 0 / empty window
// turns that dimension off.
func (s *Server) HandleThrottle(w http.ResponseWriter, r *http.Request) {
	if !rejectUnlessGetPost(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		writeOperatorState(w, s.ThrottleSnapshot(), nil)
	case http.MethodPost:
		var body struct {
			Provider    string  `json:"provider"`
			Clear       bool    `json:"clear"`
			Concurrency *int    `json:"concurrency"`
			Requests    *int64  `json:"requests"`
			ReqWindow   *string `json:"request_window"`
			Tokens      *int64  `json:"tokens"`
			TokWindow   *string `json:"token_window"`
		}
		if err := adminjson.Decode(w, r, &body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		provider := strings.TrimSpace(body.Provider)
		if provider == "" {
			http.Error(w, `{"error":"provider required"}`, http.StatusBadRequest)
			return
		}
		if body.Clear {
			persistErr := s.applyThrottle(scheduler.Throttle{Provider: provider})
			log.Printf("throttle cleared (%s)", provider)
			writeOperatorState(w, s.ThrottleSnapshot(), persistErr)
			return
		}
		if body.Concurrency == nil && body.Requests == nil && body.Tokens == nil &&
			body.ReqWindow == nil && body.TokWindow == nil {
			http.Error(w, `{"error":"concurrency, requests, or tokens required"}`, http.StatusBadRequest)
			return
		}
		var next scheduler.Limit
		var updateErr error
		persistErr := s.updateThrottle(provider, func(cur scheduler.Throttle) scheduler.Throttle {
			next = cur.Limit
			if body.Concurrency != nil {
				if *body.Concurrency < 0 {
					updateErr = fmt.Errorf("concurrency must be >= 0")
					return cur
				}
				next.Concurrency = *body.Concurrency
			}
			if body.Requests != nil || body.ReqWindow != nil {
				var n int64
				if body.Requests != nil {
					n = *body.Requests
				} else {
					n = next.Requests
				}
				if n < 0 {
					updateErr = fmt.Errorf("requests must be >= 0")
					return cur
				}
				if n == 0 {
					next.Requests = 0
					next.ReqWindow = 0
				} else {
					win := next.ReqWindow
					if body.ReqWindow != nil {
						d, err := parseLimitWindow(*body.ReqWindow)
						if err != nil {
							updateErr = fmt.Errorf("invalid request_window")
							return cur
						}
						win = d
					}
					if win <= 0 {
						updateErr = fmt.Errorf("request_window required")
						return cur
					}
					next.Requests = n
					next.ReqWindow = win
				}
			}
			if body.Tokens != nil || body.TokWindow != nil {
				var n int64
				if body.Tokens != nil {
					n = *body.Tokens
				} else {
					n = next.Tokens
				}
				if n < 0 {
					updateErr = fmt.Errorf("tokens must be >= 0")
					return cur
				}
				if n == 0 {
					next.Tokens = 0
					next.TokWindow = 0
				} else {
					win := next.TokWindow
					if body.TokWindow != nil {
						d, err := parseLimitWindow(*body.TokWindow)
						if err != nil {
							updateErr = fmt.Errorf("invalid token_window")
							return cur
						}
						win = d
					}
					if win <= 0 {
						updateErr = fmt.Errorf("token_window required")
						return cur
					}
					next.Tokens = n
					next.TokWindow = win
				}
			}
			if err := validateLimit(next); err != nil {
				updateErr = err
				return cur
			}
			return scheduler.Throttle{
				Provider:  provider,
				Limit:     next,
				Source:    scheduler.ThrottleSourceUI,
				UpdatedBy: "dashboard",
			}
		})
		if updateErr != nil {
			http.Error(w, `{"error":"`+updateErr.Error()+`"}`, http.StatusBadRequest)
			return
		}
		log.Printf("throttle set (%s conc=%d req=%d/%s tok=%d/%s)", provider,
			next.Concurrency, next.Requests, config.FormatDuration(next.ReqWindow),
			next.Tokens, config.FormatDuration(next.TokWindow))
		writeOperatorState(w, s.ThrottleSnapshot(), persistErr)
	}
}

// ThrottleSnapshot builds the GET /admin/throttle state document - the
// single owner of that shape, shared by the HTTP handler and the dashboard
// bootstrap payload (so the two can never disagree).
func (s *Server) ThrottleSnapshot() map[string]any {
	infos := s.scheduler.ListThrottles()
	throttles := make([]map[string]any, 0, len(infos))
	for _, inf := range infos {
		throttles = append(throttles, map[string]any{
			"provider":           inf.Provider,
			"concurrency":        inf.Limit.Concurrency,
			"requests":           inf.Limit.Requests,
			"request_window":     durationOrEmpty(inf.Limit.ReqWindow),
			"tokens":             inf.Limit.Tokens,
			"token_window":       durationOrEmpty(inf.Limit.TokWindow),
			"source":             inf.Source,
			"updated_by":         inf.UpdatedBy,
			"updated_at":         inf.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"in_flight":          inf.InFlight,
			"queued":             inf.Queued,
			"requests_remaining": inf.ReqRemaining,
			"tokens_remaining":   inf.TokRemaining,
			"requests_capacity":  inf.ReqCapacity,
			"tokens_capacity":    inf.TokCapacity,
		})
	}
	known := sanitizeNameList(append(s.knownProviders(), throttleProviders(infos)...))
	return map[string]any{
		"ok":              true,
		"throttles":       throttles,
		"known_providers": nullSlice(known),
		"active":          len(throttles) > 0,
	}
}

func throttleProviders(infos []scheduler.ThrottleInfo) []string {
	out := make([]string, 0, len(infos))
	for _, inf := range infos {
		if inf.Provider != "" {
			out = append(out, inf.Provider)
		}
	}
	return out
}

func durationOrEmpty(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return config.FormatDuration(d)
}

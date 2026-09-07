package config

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func settingsPost(h *Handler, revision, values string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(`{"revision":"`+revision+`","values":`+values+`}`)))
	return rr
}

func TestSettingsStrictSaveBoundary(t *testing.T) {
	h := &Handler{Path: filepath.Join(t.TempDir(), "config.yaml")}
	revision := configRevision(Default())
	for _, raw := range []string{
		`null`, `[]`, ` `, `{"values":{}}`,
		`{"revision":"` + revision + `","values":{},"typo":1}`,
		`{"revision":"` + revision + `","values":{},"values":{"max_retries":1}}`,
		`{"revision":"` + revision + `","values":{}} {}`,
		`{"revision":"` + revision + `","values":{"model_rules":[{"mode":"lower","typo":true}]}}`,
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(raw)))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", raw, rr.Code, rr.Body.String())
		}
		if _, err := os.Stat(h.Path); !os.IsNotExist(err) {
			t.Fatalf("rejected input wrote a file: %v", err)
		}
	}
}

func TestSettingsInvalidAllowlistCannotDisableRestriction(t *testing.T) {
	for _, list := range []string{`[""]`, `[" "]`, `["https://safe.example", ""]`, `[null]`} {
		t.Run(list, func(t *testing.T) {
			c := Default()
			c.AllowedBaseURLs = []string{"https://safe.example"}
			h := &Handler{Path: filepath.Join(t.TempDir(), "config.yaml")}
			if err := WriteFile(h.Path, c); err != nil {
				t.Fatal(err)
			}
			rr := settingsPost(h, configRevision(c), `{"allowed_base_urls":`+list+`}`)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("invalid allowlist accepted: %d %s", rr.Code, rr.Body.String())
			}
			got, err := LoadFile(h.Path)
			if err != nil || !reflect.DeepEqual(got.AllowedBaseURLs, c.AllowedBaseURLs) {
				t.Fatalf("restriction changed: %v %v", got, err)
			}
		})
	}
}

func TestSettingsListenSyntaxValidatedBeforeSave(t *testing.T) {
	for _, address := range []string{"not-an-address", "127.0.0.1:-1", ":65536", "http://localhost:9000", "bad host:9000"} {
		c := Default()
		c.Listen = address
		if err := c.Validate(); err == nil {
			t.Errorf("invalid listen accepted: %q", address)
		}
	}
}

func TestSettingsConcurrentRevisionAndReadIsolation(t *testing.T) {
	h := &Handler{Path: filepath.Join(t.TempDir(), "config.yaml")}
	revision := configRevision(Default())
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, value := range []string{`{"max_retries":2}`, `{"max_retries":3}`} {
		go func() { <-start; results <- settingsPost(h, revision, value) }()
	}
	close(start)
	codes := map[int]int{}
	for range 2 {
		codes[(<-results).Code]++
	}
	if codes[200] != 1 || codes[409] != 1 {
		t.Fatalf("one stale writer must fail: %v", codes)
	}
	cur, err := LoadFile(h.Path)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	h.Persist = func() ([]string, error) { close(entered); <-release; return nil, nil }
	go func() { results <- settingsPost(h, configRevision(cur), `{"max_retries":4}`) }()
	<-entered
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
		got <- rr
	}()
	select {
	case rr := <-got:
		t.Error("GET exposed a half-completed save/reload")
		got <- rr
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if rr := <-results; rr.Code != 200 {
		t.Fatalf("save=%d %s", rr.Code, rr.Body.String())
	}
	rr := <-got
	if rr.Code != 200 || rr.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rr.Body.String(), `"max_retries":4`) {
		t.Fatalf("GET after save: %d %s", rr.Code, rr.Body.String())
	}
}

func TestSettingsReloadFailureReportsSavedRevision(t *testing.T) {
	h := &Handler{
		Path:      filepath.Join(t.TempDir(), "config.yaml"),
		Persist:   func() ([]string, error) { return nil, errors.New("storage unavailable") },
		Effective: Default,
	}
	rr := settingsPost(h, configRevision(Default()), `{"max_retries":2}`)
	var result struct {
		Saved     bool           `json:"saved"`
		Revision  string         `json:"revision"`
		Values    map[string]any `json:"values"`
		Effective map[string]any `json:"effective"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 500 || !result.Saved || result.Revision == "" || result.Values["max_retries"] != float64(2) || result.Effective["max_retries"] != float64(Default().MaxRetries) {
		t.Fatalf("save/reload distinction lost: %d %s", rr.Code, rr.Body.String())
	}
	h.Persist = nil
	if next := settingsPost(h, result.Revision, `{"max_retries":3}`); next.Code != 200 {
		t.Fatalf("returned revision cannot resume editing: %d %s", next.Code, next.Body.String())
	}
	cur, _ := LoadFile(h.Path)
	h.Persist = func() ([]string, error) { return nil, os.WriteFile(h.Path, []byte("invalid: ["), 0o600) }
	if next := settingsPost(h, configRevision(cur), `{"max_retries":4}`); next.Code != 500 {
		t.Fatalf("failed readback must not panic or succeed: %d %s", next.Code, next.Body.String())
	}
}

func TestSettingsRestartWarningMatchesGetAndPost(t *testing.T) {
	h := &Handler{Path: filepath.Join(t.TempDir(), "config.yaml"), Startup: Default()}
	revision := configRevision(Default())
	for _, values := range []string{`{"db_path":""}`, `{"quality_retries":0}`} {
		post := settingsPost(h, revision, values)
		var saved map[string]any
		if err := json.Unmarshal(post.Body.Bytes(), &saved); err != nil {
			t.Fatal(err)
		}
		if post.Code != 200 || !reflect.DeepEqual(saved["restart_required"], []any{"db_path"}) {
			t.Fatalf("save lost boot-relative warning: %d %s", post.Code, post.Body.String())
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
		var loaded map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &loaded); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"restart_required", "revision", "values", "effective"} {
			if !reflect.DeepEqual(saved[key], loaded[key]) {
				t.Fatalf("GET/POST drift for %s", key)
			}
		}
		revision = loaded["revision"].(string)
	}
}

func TestWriteYAMLStringIdentityAndPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, text := range []string{"null", "~", "true", "1e3", "trailing ", " leading", "a\\b", "hé😀", "line\nnext"} {
		c := Default()
		c.DBPath = text
		c.ProviderAliases = map[string]string{"null": "~"}
		c.ModelRules = []ModelRule{{Mode: "exact", From: "null", To: "~"}}
		if err := WriteFile(path, c); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadFile(path)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if !reflect.DeepEqual(c.Map(), loaded.Map()) {
			t.Fatalf("YAML changed string %q: db_path=%q aliases=%v rules=%v", text, loaded.DBPath, loaded.ProviderAliases, loaded.ModelRules)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode=%v err=%v", info, err)
		}
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, Default()); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("existing mode changed: %v", info.Mode())
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- WriteFile(path, Default()) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadFile(path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v %v", entries, err)
	}
}

package config

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSchemaCoversConfigFields(t *testing.T) {
	want := map[string]bool{
		// yaml:"-" with a presence probe - still a user-tunable.
		"upstream_timeout": true,
	}
	rt := reflect.TypeOf(Config{})
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		tag := strings.Split(sf.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		want[tag] = true
	}
	got := map[string]bool{}
	for _, f := range Schema() {
		if got[f.Key] {
			t.Errorf("Schema: duplicate key %q", f.Key)
		}
		got[f.Key] = true
		if FieldByKey(f.Key) == nil {
			t.Errorf("FieldByKey(%q) = nil", f.Key)
		}
		if f.Label == "" || f.Help == "" || f.Kind == "" || f.Category == "" {
			t.Errorf("Schema %q: missing label/help/kind/category", f.Key)
		}
		if c := f.fieldRVCheck(); !c {
			t.Errorf("Schema %q: no settable Config field (yaml tag / upstream_timeout probe)", f.Key)
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("Config yaml field %q missing from Schema()", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("Schema key %q has no Config yaml field", k)
		}
	}
}

func (f Field) fieldRVCheck() bool {
	c := Default()
	return c.fieldRV(f.Key).IsValid() && c.fieldRV(f.Key).CanSet()
}

func TestProviderSchemaDocumentsFieldsAndTemplates(t *testing.T) {
	f := FieldByKey("providers")
	if f == nil {
		t.Fatal("providers schema missing")
	}
	line := f.TypeLine(nil)
	rt := reflect.TypeOf(ProviderOverride{})
	for i := 0; i < rt.NumField(); i++ {
		key := strings.Split(rt.Field(i).Tag.Get("yaml"), ",")[0]
		if key != "" && key != "-" && !strings.Contains(line, key) {
			t.Errorf("provider schema omits configurable field %q", key)
		}
	}
	for _, template := range []string{"{{uuid4}}", "{{platform}}"} {
		if !strings.Contains(f.Help, template) {
			t.Errorf("provider schema omits supported template %s", template)
		}
	}
}

func TestGeneratedYAMLHasCanonicalEOF(t *testing.T) {
	var out bytes.Buffer
	if err := WriteYAML(&out, Default()); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(out.Bytes(), []byte("\n")) || bytes.HasSuffix(out.Bytes(), []byte("\n\n")) {
		t.Fatal("generated YAML must end with exactly one newline")
	}
}

func TestSchemaCategoriesExist(t *testing.T) {
	cats := map[string]bool{}
	for _, c := range Categories() {
		if cats[c.ID] {
			t.Errorf("duplicate category %q", c.ID)
		}
		if c.Label == "" {
			t.Errorf("category %q missing label", c.ID)
		}
		cats[c.ID] = true
	}
	for _, f := range Schema() {
		if !cats[f.Category] {
			t.Errorf("field %q category %q not in Categories()", f.Key, f.Category)
		}
	}
}

func TestWriteYAMLRoundTripDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteYAML(&buf, Default()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile(WriteYAML(Default())): %v\n%s", err, buf.String())
	}
	if !reflect.DeepEqual(got.Map(), Default().Map()) {
		t.Errorf("round-trip Map mismatch\n got %#v\nwant %#v", got.Map(), Default().Map())
	}
}

func TestApplyUnknownKeyRejected(t *testing.T) {
	c := Default()
	if err := c.Apply(map[string]any{"not_a_key": 1}); err == nil {
		t.Fatal("expected reject unknown key")
	}
}

func TestApplyQualityRetriesZero(t *testing.T) {
	c := Default()
	if err := c.Apply(map[string]any{"quality_retries": 0}); err != nil {
		t.Fatal(err)
	}
	if c.QualityRetries != 0 {
		t.Fatalf("quality_retries = %d, want 0", c.QualityRetries)
	}
}

func TestApplyCaptureBodyPreviewFalse(t *testing.T) {
	c := Default()
	c.CaptureBodyPreview = true
	if err := c.Apply(map[string]any{"capture_body_preview": false}); err != nil {
		t.Fatal(err)
	}
	if c.CaptureBodyPreview {
		t.Fatal("capture_body_preview still true")
	}
}

func TestApplyDurationAndList(t *testing.T) {
	c := Default()
	err := c.Apply(map[string]any{
		"base_backoff":      "250ms",
		"allowed_base_urls": []any{"https://api.alpha.example", "http://10.0.0.5:8000"},
		"max_request_bytes": float64(2 << 20),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseBackoff != 250*time.Millisecond {
		t.Errorf("base_backoff = %v", c.BaseBackoff)
	}
	if len(c.AllowedBaseURLs) != 2 {
		t.Errorf("allowed_base_urls = %v", c.AllowedBaseURLs)
	}
	if c.MaxRequestBytes != 2<<20 {
		t.Errorf("max_request_bytes = %d", c.MaxRequestBytes)
	}
}

func TestApplyRejectsDurationNumber(t *testing.T) {
	c := Default()
	if err := c.Apply(map[string]any{"base_backoff": float64(1e9)}); err == nil {
		t.Fatal("duration as JSON number must be rejected")
	}
}

func TestStartupBoundChangesFromSchema(t *testing.T) {
	a := Default()
	b := Default()
	b.Listen = ":9999"
	b.MaxRetries = 9
	got := StartupBoundChanges(a, b)
	if len(got) != 1 || got[0] != "listen" {
		t.Fatalf("got %v, want [listen] (max_retries hot-reloads)", got)
	}
	b.HistorySize = 12
	b.StorageBatchCap = 10
	got = StartupBoundChanges(a, b)
	want := map[string]bool{"listen": true, "history_size": true, "storage_batch_cap": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected %q", k)
		}
	}
}

func TestHandlerGetAndPost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("max_retries: 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var live *Config
	load := func() *Config {
		c, err := LoadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	live = load()
	h := &Handler{
		Path:      path,
		Effective: func() *Config { return live.Clone() },
		Overrides: map[string]string{"listen": "127.0.0.1:8081"},
		Persist: func() ([]string, error) {
			c, err := LoadFile(path)
			if err != nil {
				return nil, err
			}
			skipped := StartupBoundChanges(live, c)
			live = c
			return skipped, nil
		},
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	if rr.Code != 200 {
		t.Fatalf("GET status %d body %s", rr.Code, rr.Body.String())
	}
	var doc struct {
		Fields    []Field           `json:"fields"`
		Values    map[string]any    `json:"values"`
		Defaults  map[string]any    `json:"defaults"`
		Effective map[string]any    `json:"effective"`
		Overrides map[string]string `json:"overrides"`
		Writable  bool              `json:"writable"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Writable || len(doc.Fields) != len(Schema()) {
		t.Fatalf("writable=%v fields=%d want %d", doc.Writable, len(doc.Fields), len(Schema()))
	}
	if int(doc.Values["max_retries"].(float64)) != 9 {
		t.Errorf("values.max_retries = %v", doc.Values["max_retries"])
	}
	if doc.Overrides["listen"] != "127.0.0.1:8081" {
		t.Errorf("overrides = %v", doc.Overrides)
	}

	post := `{"revision":"` + configRevision(live) + `","values":{"max_retries":3,"quality_retries":0,"capture_body_preview":true,"listen":":9999"}}`
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(post)))
	if rr.Code != 200 {
		t.Fatalf("POST status %d body %s", rr.Code, rr.Body.String())
	}
	if live.MaxRetries != 3 || live.QualityRetries != 0 || !live.CaptureBodyPreview {
		t.Fatalf("applied live = %+v", live)
	}
	// CLI override must not be written into the file.
	if live.Listen != Default().Listen {
		t.Errorf("listen written from override form value: %q", live.Listen)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(`{"revision":"`+configRevision(live)+`","values":{"not_a_key":1}}`)))
	if rr.Code != 400 {
		t.Fatalf("unknown key status %d, want 400", rr.Code)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(`{"revision":"`+configRevision(live)+`","values":{"history_size":0}}`)))
	if rr.Code != 400 {
		t.Fatalf("out of range status %d, want 400", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/admin/config", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status %d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("DELETE Allow = %q, want GET, POST", got)
	}
}

func TestHandlerPostEmptyRejected(t *testing.T) {
	h := &Handler{Path: filepath.Join(t.TempDir(), "c.yaml")}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(`{}`)))
	if rr.Code != 400 {
		t.Fatalf("status %d, want 400", rr.Code)
	}
}

func TestMapEmptySlicesAreJSONArrays(t *testing.T) {
	b, err := json.Marshal(Default().Map()["allowed_base_urls"])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Fatalf("allowed_base_urls JSON = %s, want [] (not null)", b)
	}
	b, err = json.Marshal(Default().Map()["providers"])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{}" {
		t.Fatalf("providers JSON = %s, want {}", b)
	}
}

func TestMapDurationsAreStrings(t *testing.T) {
	m := Default().Map()
	if m["base_backoff"] != "1s" {
		t.Errorf("base_backoff = %v, want 1s", m["base_backoff"])
	}
	if m["max_backoff"] != "2m" {
		t.Errorf("max_backoff = %v, want 2m", m["max_backoff"])
	}
	if m["storage_flush_interval"] != "500ms" {
		t.Errorf("storage_flush_interval = %v", m["storage_flush_interval"])
	}
}

func TestTypeLineDurationRange(t *testing.T) {
	d := Default()
	st := FieldByKey("shutdown_timeout")
	if st == nil {
		t.Fatal("shutdown_timeout missing from Schema")
	}
	line := st.TypeLine(d.ShutdownTimeout)
	if !strings.Contains(line, "duration") || !strings.Contains(line, "range > 0") {
		t.Errorf("shutdown_timeout TypeLine = %q, want duration and range > 0", line)
	}

	hb := FieldByKey("cursor_heartbeat_interval")
	if hb == nil {
		t.Fatal("cursor_heartbeat_interval missing from Schema")
	}
	hline := hb.TypeLine(d.CursorHeartbeatInterval)
	if !strings.Contains(hline, "1ns") || !strings.Contains(hline, "10m") {
		t.Errorf("cursor_heartbeat_interval TypeLine = %q, want 1ns and 10m", hline)
	}

	idle := FieldByKey("idle_timeout")
	if idle == nil {
		t.Fatal("idle_timeout missing from Schema")
	}
	iline := idle.TypeLine(d.IdleTimeout)
	if !strings.Contains(iline, "0 = no timeout") {
		t.Errorf("idle_timeout TypeLine = %q, want 0 = no timeout", iline)
	}

	mi := FieldByKey("max_idle_conns_per_host")
	if mi == nil {
		t.Fatal("max_idle_conns_per_host missing from Schema")
	}
	mline := mi.TypeLine(d.MaxIdleConnsPerHost)
	if !strings.Contains(mline, ">= 1") {
		t.Errorf("max_idle_conns_per_host TypeLine = %q, want >= 1", mline)
	}
	if strings.Contains(strings.ToLower(mline), "unlimited") {
		t.Errorf("max_idle_conns_per_host TypeLine = %q, must not claim unlimited", mline)
	}

	mb := FieldByKey("max_request_bytes")
	if mb == nil {
		t.Fatal("max_request_bytes missing from Schema")
	}
	bline := mb.TypeLine(d.MaxRequestBytes)
	if !strings.Contains(bline, "1GiB") || !strings.Contains(bline, "bytes") {
		t.Errorf("max_request_bytes TypeLine = %q, want 1GiB and bytes", bline)
	}
	if !strings.Contains(bline, "1024..1GiB") {
		t.Errorf("max_request_bytes TypeLine = %q, want range 1024..1GiB", bline)
	}

	qr := FieldByKey("queue_retry_after")
	if qr == nil {
		t.Fatal("queue_retry_after missing from Schema")
	}
	qline := qr.TypeLine(d.QueueRetryAfter)
	if !strings.Contains(qline, "seconds") {
		t.Errorf("queue_retry_after TypeLine = %q, want seconds", qline)
	}
}

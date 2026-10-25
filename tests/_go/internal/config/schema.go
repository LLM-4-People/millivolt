package config

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
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

// TestEverySchemaFieldHasDefault mechanically ties Schema() to Default():
// every registered setting must resolve to a field populated by Default(), and
// a duration that does not declare zero semantics must have a positive default.
// This keeps the Settings registry and the defaults owner from drifting.
func TestEverySchemaFieldHasDefault(t *testing.T) {
	defaults := Default()
	for _, f := range Schema() {
		value := defaults.fieldValue(f.Key)
		if value == nil {
			t.Errorf("Default() has no value for schema key %q", f.Key)
			continue
		}
		if f.Kind != KindDuration || f.ZeroMeans != "" {
			continue
		}
		d, ok := value.(time.Duration)
		if !ok || d <= 0 {
			t.Errorf("Default().%s = %v, want a positive duration because the schema declares no zero semantics", f.Key, value)
		}
	}
}

// TestDeliveryConsumersDoNotDeclareDefaults enforces the other half of the
// ownership rule: the request/stream boundary files must not reintroduce
// package-level duration defaults. Every deadline comes from Config and is
// passed in at the boundary.
func TestDeliveryConsumersDoNotDeclareDefaults(t *testing.T) {
	root := filepath.Join("..", "..")
	files := []string{
		filepath.Join(root, "cmd", "proxy", "deadline.go"),
		filepath.Join(root, "internal", "proxy", "sse_keepalive.go"),
		filepath.Join(root, "internal", "proxy", "stream_idle.go"),
		filepath.Join(root, "internal", "proxy", "relay_nonstream.go"),
		filepath.Join(root, "internal", "proxy", "relay_translate.go"),
		filepath.Join(root, "internal", "proxy", "relay_serve.go"),
		filepath.Join(root, "internal", "proxy", "relay_upstream.go"),
		filepath.Join(root, "internal", "proxy", "relay_retry.go"),
		filepath.Join(root, "internal", "proxy", "relay_transform.go"),
		filepath.Join(root, "internal", "proxy", "relay_stream.go"),
		filepath.Join(root, "internal", "proxy", "proxy.go"),
	}
	for _, path := range files {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if durationType(value.Type) || containsDurationLiteral(value.Values) {
					names := make([]string, 0, len(value.Names))
					for _, name := range value.Names {
						names = append(names, name.Name)
					}
					t.Errorf("%s declares package-level duration default %s; read it from Config at the boundary instead", path, strings.Join(names, ", "))
				}
			}
		}
	}
}

func durationType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "time" && sel.Sel.Name == "Duration"
}

func containsDurationLiteral(exprs []ast.Expr) bool {
	for _, expr := range exprs {
		found := false
		ast.Inspect(expr, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if ok && id.Name == "time" && (sel.Sel.Name == "Second" || sel.Sel.Name == "Minute" || sel.Sel.Name == "Hour") {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
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

func TestWriteYAMLRoundTripEachSchemaKey(t *testing.T) {
	for _, f := range Schema() {
		t.Run(f.Key, func(t *testing.T) {
			c := Default()
			sample := alternateSchemaValue(t, f, c)
			if err := c.Apply(map[string]any{f.Key: sample}); err != nil {
				t.Fatalf("Apply %s: %v", f.Key, err)
			}
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate %s: %v", f.Key, err)
			}
			if reflect.DeepEqual(c.Map()[f.Key], Default().Map()[f.Key]) {
				t.Fatalf("sample for %s is still the default", f.Key)
			}
			path := filepath.Join(t.TempDir(), "c.yaml")
			if err := WriteFile(path, c); err != nil {
				t.Fatal(err)
			}
			got, err := LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(got.Map()[f.Key], Default().Map()[f.Key]) {
				t.Fatalf("LoadFile dropped non-default %s: %#v", f.Key, got.Map()[f.Key])
			}
			path2 := filepath.Join(t.TempDir(), "round.yaml")
			if err := WriteFile(path2, got); err != nil {
				t.Fatal(err)
			}
			got2, err := LoadFile(path2)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Map(), got2.Map()) {
				t.Fatalf("WriteYAML/LoadFile is not stable for %s\n got %#v\nwant %#v", f.Key, got2.Map()[f.Key], got.Map()[f.Key])
			}
		})
	}
}

func alternateSchemaValue(t *testing.T, f Field, base *Config) any {
	t.Helper()
	switch f.Kind {
	case KindBool:
		return !base.fieldValue(f.Key).(bool)
	case KindString:
		if f.Key == "listen" {
			return "127.0.0.1:9999"
		}
		if f.Key == "quota_pause_mode" {
			// An exact-match enum: alternate with another member.
			return QuotaPauseRetry
		}
		if f.Key == "translation_loss" {
			// An exact-match enum: alternate with another member.
			return TranslationLossReject
		}
		return "alt-" + base.fieldValue(f.Key).(string)
	case KindInt:
		n := base.fieldValue(f.Key).(int)
		for _, cand := range []int{n + 1, n - 1} {
			trial := base.Clone()
			trial.fieldRV(f.Key).SetInt(int64(cand))
			if trial.Validate() == nil {
				return cand
			}
		}
	case KindBytes:
		n := int64(base.fieldValue(f.Key).(ByteSize))
		// The schema band is a candidate too: zero-default fields need a
		// sample inside their floor/ceiling, which default +/- 1 KiB cannot
		// reach.
		cands := []int64{n + 1024, n - 1024}
		if f.Min != nil {
			cands = append(cands, int64(*f.Min))
		}
		if f.Max != nil {
			cands = append(cands, int64(*f.Max))
		}
		for _, cand := range cands {
			trial := base.Clone()
			trial.fieldRV(f.Key).SetInt(cand)
			if trial.Validate() == nil {
				return FormatByteSize(cand)
			}
		}
	case KindDuration:
		d := base.fieldValue(f.Key).(time.Duration)
		for _, cand := range []time.Duration{d + time.Second, d - time.Second, d + time.Millisecond} {
			if cand < 0 {
				continue
			}
			trial := base.Clone()
			trial.fieldRV(f.Key).SetInt(int64(cand))
			if trial.Validate() == nil {
				return FormatDuration(cand)
			}
		}
	case KindStrings:
		if f.Key == "allowed_base_urls" {
			return []any{"https://api.example.invalid"}
		}
		if f.Key == "storm_status_codes" {
			return []any{"500"}
		}
		if f.Key == "retryable_error_classes" {
			return []any{"model_warming_up"}
		}
	case KindProviders:
		return map[string]any{"neutral.example": map[string]any{"cost_keys": []any{"usage.cost"}}}
	case KindAliases:
		return map[string]any{"old.example": "new.example"}
	case KindModelRules:
		return []any{map[string]any{"mode": "lower"}}
	case KindRequestOverrides:
		return []any{map[string]any{"client": "opencode", "headers": map[string]any{"x-sample-hint": "v1"}}}
	case KindSubConversations:
		return []any{map[string]any{"client": "opencode", "params": []any{"promptCacheKey"}, "strip": true}}
	}
	t.Fatalf("no valid non-default sample for %s (%s)", f.Key, f.Kind)
	return nil
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
	err := c.Apply(map[string]any{"not_a_key": 1})
	if err == nil {
		t.Fatal("expected reject unknown key")
	}
	// The rejection wording is a pinned contract (the errUnknownKey owner,
	// shared by Settings Apply and the YAML overlay path): byte-exact, with
	// the key rendered via %q.
	if err.Error() != `unknown config key "not_a_key"` {
		t.Fatalf("Apply error = %q, want the exact unknown-key wording", err.Error())
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
		"max_request_bytes": "2MiB",
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
			c, _, err := LoadFileRepair(path)
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
	d := Default()
	m := d.Map()
	for _, f := range Schema() {
		if f.Kind != KindDuration {
			continue
		}
		got, ok := m[f.Key].(string)
		if !ok {
			t.Errorf("%s Map type = %T, want string", f.Key, m[f.Key])
			continue
		}
		want := FormatDuration(d.fieldValue(f.Key).(time.Duration))
		if got != want {
			t.Errorf("%s = %q, want %q", f.Key, got, want)
		}
	}
}

func TestMapByteSizesAreStrings(t *testing.T) {
	d := Default()
	m := d.Map()
	for _, f := range Schema() {
		if f.Kind != KindBytes {
			continue
		}
		got, ok := m[f.Key].(string)
		if !ok {
			t.Errorf("%s Map type = %T, want string", f.Key, m[f.Key])
			continue
		}
		n, err := parseByteSize(d.fieldValue(f.Key))
		if err != nil {
			t.Fatalf("%s fieldValue: %v", f.Key, err)
		}
		want := FormatByteSize(n)
		if got != want {
			t.Errorf("%s = %q, want %q", f.Key, got, want)
		}
	}
}

func TestSchemaZeroTokenMatchesFormatters(t *testing.T) {
	for _, f := range Schema() {
		switch f.Kind {
		case KindDuration:
			if f.ZeroToken != FormatDuration(0) {
				t.Errorf("%s zero_token = %q, want %q", f.Key, f.ZeroToken, FormatDuration(0))
			}
		case KindBytes:
			if f.ZeroToken != FormatByteSize(0) {
				t.Errorf("%s zero_token = %q, want %q", f.Key, f.ZeroToken, FormatByteSize(0))
			}
		default:
			if f.ZeroToken != "" {
				t.Errorf("%s zero_token = %q, want empty", f.Key, f.ZeroToken)
			}
		}
	}
}

func TestTypeLineDurationRange(t *testing.T) {
	d := Default()
	st := FieldByKey("shutdown_timeout")
	if st == nil {
		t.Fatal("shutdown_timeout missing from Schema")
	}
	line := st.TypeLine(d.ShutdownTimeout)
	if !strings.Contains(line, "duration") || !strings.Contains(line, "range > "+FormatDuration(0)) {
		t.Errorf("shutdown_timeout TypeLine = %q, want duration and range > %s", line, FormatDuration(0))
	}
	if !strings.Contains(line, "default "+FormatDuration(d.ShutdownTimeout)) {
		t.Errorf("shutdown_timeout TypeLine = %q, want default %s", line, FormatDuration(d.ShutdownTimeout))
	}

	hb := FieldByKey("cursor_heartbeat_interval")
	if hb == nil {
		t.Fatal("cursor_heartbeat_interval missing from Schema")
	}
	hline := hb.TypeLine(d.CursorHeartbeatInterval)
	if !strings.Contains(hline, heartbeatRange()) {
		t.Errorf("cursor_heartbeat_interval TypeLine = %q, want %s", hline, heartbeatRange())
	}

	idle := FieldByKey("idle_timeout")
	if idle == nil {
		t.Fatal("idle_timeout missing from Schema")
	}
	iline := idle.TypeLine(d.IdleTimeout)
	if !strings.Contains(iline, FormatDuration(0)+" = no timeout") {
		t.Errorf("idle_timeout TypeLine = %q, want %s = no timeout", iline, FormatDuration(0))
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
	if !strings.Contains(bline, FormatByteSize(MaxRequestBytesMax)) || !strings.Contains(bline, "bytes") {
		t.Errorf("max_request_bytes TypeLine = %q, want %s and bytes", bline, FormatByteSize(MaxRequestBytesMax))
	}
	if !strings.Contains(bline, FormatByteSize(MaxRequestBytesMin)+".."+FormatByteSize(MaxRequestBytesMax)) {
		t.Errorf("max_request_bytes TypeLine = %q, want range %s..%s", bline, FormatByteSize(MaxRequestBytesMin), FormatByteSize(MaxRequestBytesMax))
	}
	if !strings.Contains(bline, "default "+FormatByteSize(int64(d.MaxRequestBytes))) {
		t.Errorf("max_request_bytes TypeLine = %q, want default %s", bline, FormatByteSize(int64(d.MaxRequestBytes)))
	}

	qr := FieldByKey("queue_retry_after")
	if qr == nil {
		t.Fatal("queue_retry_after missing from Schema")
	}
	qline := qr.TypeLine(d.QueueRetryAfter)
	if !strings.Contains(qline, "duration") || !strings.Contains(qline, FormatDuration(QueueRetryAfterMax)) {
		t.Errorf("queue_retry_after TypeLine = %q, want duration %s..%s", qline, FormatDuration(0), FormatDuration(QueueRetryAfterMax))
	}
	if strings.Contains(qline, "seconds") {
		t.Errorf("queue_retry_after TypeLine = %q, must not use integer seconds", qline)
	}
}

// TestSchemaHelpStatesTranslationScope pins the corrected help text and the
// generated example together, so a regeneration cannot drift back to the old
// OpenAI-only/Anthropic-ensure_tools/first-occurrence claims.
func TestSchemaHelpStatesTranslationScope(t *testing.T) {
	cases := []struct {
		key  string
		want string
		// helpNotWant and exampleNotWant are separate because a legitimate
		// mention in another key's example text must not weaken the field
		// assertion. The providers help must never return to the broad
		// "Anthropic client bodies" promise, while the example legitimately
		// contains that phrase in request_overrides.
		helpNotWant    string
		exampleNotWant string
	}{
		{"providers", "on a Chat outbound wire", "Anthropic client bodies", ""},
		{"request_overrides", "on a Chat Completions or Anthropic client body", "on the OpenAI wire", "on the OpenAI wire"},
		{"request_overrides", "Responses client body is not rewritten", "translated-anthropic requests", "translated-anthropic requests"},
		{"sub_conversations", "a duplicate top-level key drops the identity", "resolve by their first occurrence", "resolve by their first occurrence"},
		{"mcp_enabled", "operator-gated like /metrics and Bearer-only: a session cookie alone is refused", "Bearer-only like /metrics", "Bearer-only like /metrics"},
	}
	for _, tc := range cases {
		field := FieldByKey(tc.key)
		if field == nil {
			t.Fatalf("schema key %q missing", tc.key)
		}
		if !strings.Contains(field.Help, tc.want) {
			t.Fatalf("%s help does not state %q: %s", tc.key, tc.want, field.Help)
		}
		if tc.helpNotWant != "" && strings.Contains(field.Help, tc.helpNotWant) {
			t.Fatalf("%s help still states %q: %s", tc.key, tc.helpNotWant, field.Help)
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "proxy.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The YAML writer wraps documentation into comment lines, so join them
	// before asserting a contiguous sentence.
	example := exampleHelpText(string(raw))
	for _, tc := range cases {
		if !strings.Contains(example, tc.want) {
			t.Fatalf("proxy.example.yaml does not carry the corrected %s text %q", tc.key, tc.want)
		}
		if tc.exampleNotWant != "" && strings.Contains(example, tc.exampleNotWant) {
			t.Fatalf("proxy.example.yaml still carries the old %s text %q", tc.key, tc.exampleNotWant)
		}
	}
}

// exampleHelpText joins the generated example's wrapped comment lines into one
// searchable string. Comments are "# ..." possibly indented; every line break
// inside a comment block becomes one space.
func exampleHelpText(example string) string {
	lines := strings.Split(example, "\n")
	var joined []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			joined = append(joined, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
		}
	}
	return strings.Join(joined, " ")
}

// TestSettingsHintsStateTranslationScope pins the dashboard hints to the same
// runtime gates as the schema: request-override body fields read the inbound
// Chat/Anthropic body before translation, and ensured tools require a Chat
// client body on a Chat outbound wire.
func TestSettingsHintsStateTranslationScope(t *testing.T) {
	// Each hint is owned by the editor module that renders it; the read follows
	// the literal, not the pre-split entry-point filename.
	for _, tc := range []struct {
		module  string
		want    string
		notWant string
	}{
		{
			module:  "settings.js",
			want:    "on Chat Completions or Anthropic client bodies, before translation",
			notWant: "on the OpenAI wire",
		},
		{
			module:  "settings_providers.js",
			want:    "relayed Chat Completions client bodies on a Chat outbound wire",
			notWant: "every chat request's tools array",
		},
		{
			module:  "settings.js",
			want:    "Chat or Anthropic client-body token",
			notWant: "OpenAI-wire token ceilings",
		},
	} {
		source := strings.Join(strings.Fields(string(settingsModuleSource(t, tc.module))), " ")
		if !strings.Contains(source, tc.want) {
			t.Errorf("%s omits %q", tc.module, tc.want)
		}
		if strings.Contains(source, tc.notWant) {
			t.Errorf("%s still states %q", tc.module, tc.notWant)
		}
	}
}

// TestProtocolDocsQualifyPassthrough pins the same rewrite exception in the
// protocol document that the adapters document states: passthrough skips
// translation, but configured request overrides, a sub-conversation strip or
// ensured tools can still rewrite the upstream body.
func TestProtocolDocsQualifyPassthrough(t *testing.T) {
	doc := strings.Join(strings.Fields(repositoryText(t, filepath.Join("docs", "protocol.md"))), " ")
	for _, tc := range []struct {
		want    string
		notWant string
	}{
		{
			want:    "Without a configured request rewrite, a passthrough request's body bytes are relayed unchanged",
			notWant: "is byte-transparent passthrough",
		},
		{
			want:    "Absent means passthrough; configured request rewrites can still change the body",
			notWant: "Absent means byte-transparent passthrough",
		},
		{
			want:    "Ordinary requests relay the original body bytes unless a configured request override",
			notWant: "Ordinary requests preserve original body bytes",
		},
	} {
		if !strings.Contains(doc, tc.want) {
			t.Errorf("docs/protocol.md omits %q", tc.want)
		}
		if strings.Contains(doc, tc.notWant) {
			t.Errorf("docs/protocol.md still states %q", tc.notWant)
		}
	}
}

// TestEnsureToolsChoiceHelpMatchesRuntime pins the A3/D3/G2 rule on every
// prose owner: absent, null, or auto becomes none; explicit none stays;
// required, named, or malformed skips injection.
func TestEnsureToolsChoiceHelpMatchesRuntime(t *testing.T) {
	const rule = "absent, null, or auto tool_choice is forced to none"
	const old = "tool_choice none added when the client sent no tools"
	field := FieldByKey("providers")
	if field == nil {
		t.Fatal("providers schema key missing")
	}
	if !strings.Contains(field.Help, rule) || strings.Contains(field.Help, old) {
		t.Fatalf("providers help does not state the choice rule: %s", field.Help)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "proxy.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	example := exampleHelpText(string(raw))
	if !strings.Contains(example, rule) || strings.Contains(example, old) {
		t.Fatalf("generated example does not state the choice rule")
	}
	doc := strings.Join(strings.Fields(repositoryText(t, filepath.Join("docs", "adapters.md"))), " ")
	for _, want := range []string{
		"an absent, null, or `auto`",
		"`tool_choice` is forced to `none`",
		"`required`, named, or malformed choice skips injection entirely",
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("docs/adapters.md omits %q", want)
		}
	}
	if strings.Contains(doc, "adds `tool_choice: none` when the client sent no tools of its own") {
		t.Fatal("docs/adapters.md still states the unconditional none rule")
	}
	runtimeComments := strings.Join(strings.Fields(strings.ReplaceAll(
		repositoryText(t, filepath.Join("internal", "config", "config_defaults.go")), "//", " ")), " ")
	if !strings.Contains(runtimeComments, "a required or named choice skips injection") {
		t.Fatal("the bundled Zen comment omits the required/named injection rule")
	}
	if strings.Contains(runtimeComments, "tool_choice none so stubs can never be invoked") {
		t.Fatal("the bundled Zen comment still states the unconditional none rule")
	}
}

// TestDocsStateRuntimeContracts pins the round-5 documentation corrections to
// their runtime owners: boot repair's duplicate-key exception, the strip scope,
// non-stream vs streamed item status, the quota wire limit, the ensure-tools
// passthrough qualification, canonical backup replace, Anthropic sampling
// compatibility, response.cancelled, and the synthetic sequence reset.
func TestDocsStateRuntimeContracts(t *testing.T) {
	normalize := func(rel string) string {
		return strings.Join(strings.Fields(repositoryText(t, filepath.Join("docs", rel))), " ")
	}
	operations := normalize("operations.md")
	adapters := normalize("adapters.md")
	protocol := normalize("protocol.md")
	example := exampleHelpText(repositoryText(t, "proxy.example.yaml"))
	configSource := strings.Join(strings.Fields(strings.ReplaceAll(repositoryText(t, filepath.Join("internal", "config", "config_types.go")), "//", " ")), " ")

	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"boot repair duplicate key", operations, "except an ambiguous duplicate top-level key, which fails closed and stays in place"},
		{"generated boot repair duplicate key", example, "except an ambiguous duplicate top-level key, which fails closed and stays in place"},
		{"strip includes Responses", operations, "including a Responses client body"},
		{"strip is not inbound-limited", operations, "not limited to Chat Completions or Anthropic inbound"},
		{"strip operations wording", operations, "tracked client-body fields"},
		{"strip protocol scope", protocol, "client request body before relay or translation. It applies to any JSON body, including a Responses client body"},
		{"transport header exception", operations, "`accept-encoding` and `range` are the exception"},
		{"transport header client map", operations, "`X-Proxy-Headers` and an `X-Proxy-Auth-Header` naming one"},
		{"transport header protocol row", protocol, "`accept-encoding` and `range` are proxy-owned and rejected with 400"},
		{"transport header provider comment", configSource, "accept-encoding and range are proxy-owned: the X-Proxy-Headers map rejects them with 400"},
		{"transport header example", example, "accept-encoding and range are proxy-owned: the X-Proxy-Headers map rejects them with 400"},
		{"strip config list comment", configSource, "client request body before relay or translation, including a Responses client body"},
		{"strip config field comment", configSource, "It applies to any JSON body, including a Responses client body"},
		{"streamed item added status", adapters, "`response.output_item.added` carries `status: \"in_progress\"`"},
		{"streamed item done status", adapters, "each `response.output_item.done` carries the status at which that item closed"},
		{"streamed item terminal status", adapters, "only an item still open at an incomplete or failed terminal is rendered `incomplete`"},
		{"non-stream message item status", adapters, "A non-streaming translated Responses message item carries `status: \"incomplete\"` when the response-level status is not `completed`"},
		{"non-stream function call status", adapters, "Function-call items always carry `status: \"completed\"`"},
		{"request tool id pre-scan", adapters, "Tool ids are pre-reserved once per request across every message and item before any message is decoded"},
		{"failure terminal open call", adapters, "On Anthropic and Responses, an already-announced open call is left unclosed on those three terminals"},
		{"chat failure terminal announcement", adapters, "The Chat target already streamed the announcement with empty arguments"},
		{"translated response identity default", adapters, "A translated non-streaming response assumes a schema-valid source identity"},
		{"parallel Anthropic blocks", adapters, "open their indexed content blocks concurrently"},
		{"degenerate Responses sequence reset", adapters, "The degenerate terminal-region substitution emits the same error plus `response.failed` pair at 0 and 1"},
		{"null header member", protocol, "A JSON `null` member is treated as an omitted header"},
		{"retained header name cap", operations, "a header name is capped at 512 bytes"},
		{"retained header name count", operations, "at most 256 names are retained per map"},
		{"quota wire limit", adapters + " " + example, "OpenAI-compatible wire only; Anthropic and Cursor upstreams keep the off behavior"},
		{"ensure-tools passthrough qualification", configSource, "byte-identical only"},
		{"ensure-tools rewrite actors", configSource, "when no configured request override or strip also rewrites the body"},
		{"backup replace canonical", operations, "loads and validates the backup, writes canonical YAML, and preserves CLI-overridden"},
		{"backup replace in example", example, "replace loads and validates the backup, writes canonical YAML, and preserves CLI-overridden"},
		{"anthropic sampling compatibility", adapters, "MessageCreateParamsBase"},
		{"anthropic strict endpoint", adapters, "strict current endpoint may reject them"},
		{"responses cancelled extension", adapters, "response.cancelled"},
		{"responses cancelled compatibility", adapters, "forward-compatible extension"},
		{"synthetic sequence reset", adapters, "restart `sequence_number` at 0 and 1"},
		{"quota recovery consecutive probes", operations, "consecutive successful probes (the default 1 closes on the first 2xx)"},
		{"quota recovery protocol wording", protocol, "consecutive successful probes (the default 1 closes on the first 2xx)"},
		{"quota recovery example wording", example, "consecutive successful probes (the default 1 closes on the first 2xx)"},
		{"memory limit precedence operations", operations, "With the setting at 0, an operator `GOMEMLIMIT` wins"},
		{"memory limit override wording", operations, "A set value overrides `GOMEMLIMIT`, not `GOGC`"},
		{"memory limit precedence example", example, "With the setting at 0, an operator GOMEMLIMIT wins"},
		{"memory limit override example", example, "a set value overrides GOMEMLIMIT, not GOGC"},
		{"strip passthrough scope", operations, "Without `strip`, the client's spelling reaches the upstream wire on a same-protocol passthrough"},
		{"reasoning channels documented", adapters, "A Responses reasoning item exposes two independent text channels"},
		{"reasoning channels not merged", adapters, "The proxy does not deduplicate across channels"},
		{"generation artifacts are output presence", adapters, "Provider-executed generation artifacts (`program`, `program_output`, `image_generation_call`, `code_interpreter_call`) are output presence"},
		{"generation artifacts keep the stop finish", adapters, "its finish stays the source's stop rather than `tool_calls`"},
	} {
		if !strings.Contains(tc.text, tc.want) {
			t.Errorf("%s: missing %q", tc.name, tc.want)
		}
	}

	settings := repositoryText(t, filepath.Join("internal", "web", "static", "js", "settings.js"))
	for name, text := range map[string]string{
		"config comments": configSource,
		"protocol doc":    protocol,
		"operations doc":  operations,
		"settings label":  settings,
	} {
		if strings.Contains(text, "relayed upstream body") {
			t.Errorf("%s still scopes strip to the relayed upstream body", name)
		}
	}
	if strings.Contains(operations, "governs the passthrough body") {
		t.Error("operations still calls the stripped fields the passthrough body")
	}
	if !strings.Contains(settings, "client request body before relay or translation") {
		t.Error("settings strip label omits the pre-translation scope")
	}
	if !strings.Contains(settings, "strip removes every configured field present at the top level of the client request body before relay or translation") {
		t.Error("settings strip hint still scopes the fields to the relayed body")
	}

	providers := FieldByKey("providers")
	if providers == nil || !strings.Contains(providers.Help, "accept-encoding and range are proxy-owned: the X-Proxy-Headers map rejects them with 400") {
		t.Error("providers help omits the transport-mode header exception")
	}

	quota := FieldByKey("quota_pause_mode")
	if quota == nil || !strings.Contains(quota.Help, "OpenAI-compatible wire only") {
		t.Errorf("quota_pause_mode help omits the wire qualifier")
	}
	sub := FieldByKey("sub_conversations")
	if sub == nil || !strings.Contains(sub.Help, "including a Responses client body") {
		t.Errorf("sub_conversations help omits the strip scope")
	}
	var backup *Category
	for _, category := range Categories() {
		if category.ID == "backup" {
			c := category
			backup = &c
			break
		}
	}
	if backup == nil || !strings.Contains(backup.Help, "replace loads and validates the backup") {
		t.Errorf("backup category help still claims replace writes the backup as-is")
	}
}

func TestSchemaHelpPinsValidationRules(t *testing.T) {
	baseURLs := FieldByKey("allowed_base_urls")
	if baseURLs == nil {
		t.Fatal("allowed_base_urls is not registered")
	}
	for _, phrase := range []string{
		"literally lowercase http:// or https://",
		"lowercase hostname",
		"no userinfo, query, fragment or trailing slash",
		"port after a hostname in 1..65535",
	} {
		if !strings.Contains(baseURLs.Help, phrase) {
			t.Errorf("allowed_base_urls help omits %q: %s", phrase, baseURLs.Help)
		}
	}

	rules := FieldByKey("model_rules")
	if rules == nil {
		t.Fatal("model_rules is not registered")
	}
	if !strings.Contains(rules.Help, "Capped at 64 rules.") {
		t.Errorf("model_rules help omits the 64-rule cap: %s", rules.Help)
	}

	overrides := FieldByKey("request_overrides")
	if overrides == nil {
		t.Fatal("request_overrides is not registered")
	}
	for _, phrase := range []string{
		"Capped at 64 rules.",
		"the target's resolved auth header and Authorization are never set or removed",
		"Accept-Encoding and Range are stripped after the override merge",
	} {
		if !strings.Contains(overrides.Help, phrase) {
			t.Errorf("request_overrides help omits %q: %s", phrase, overrides.Help)
		}
	}
	operations := repositoryText(t, filepath.Join("docs", "operations.md"))
	if !strings.Contains(operations, "The list is capped at 64 rules.") {
		t.Error("docs/operations.md omits the model_rules cap")
	}
}

// TestSSEKeepaliveHelpOmitsTTFB pins the pacer's actual surface: it starts on
// OnWait (queue), covers an operator hold and mid-stream gaps, and is not
// started before upstream headers on an admitted-immediately stream. Promising
// TTFB keepalives would describe a comment the pacer deliberately does not
// commit before the body mode is known.
func TestSSEKeepaliveHelpOmitsTTFB(t *testing.T) {
	field := FieldByKey("sse_keepalive_interval")
	if field == nil {
		t.Fatal("sse_keepalive_interval is missing from Schema()")
	}
	if !strings.Contains(field.Help, "queue wait, operator hold, mid-stream gaps") {
		t.Fatalf("help does not name the covered waits: %s", field.Help)
	}
	if strings.Contains(field.Help, "TTFB") {
		t.Fatalf("help still promises a TTFB keepalive: %s", field.Help)
	}
}

// TestModelRulesHelpNamesTheRealFilterSurfaces pins the current UI: the header
// Clear menu is gone and the canonical model grouping reaches the Logs
// download filter and the Settings delete filter.
func TestModelRulesHelpNamesTheRealFilterSurfaces(t *testing.T) {
	field := FieldByKey("model_rules")
	if field == nil {
		t.Fatal("model_rules is missing from Schema()")
	}
	if !strings.Contains(field.Help, "the Logs download and Settings delete-filter model optgroups") {
		t.Fatalf("help does not name the real filter surfaces: %s", field.Help)
	}
	if strings.Contains(field.Help, "Clear/Logs") {
		t.Fatalf("help still names the removed Clear menu: %s", field.Help)
	}
}

// TestSSEKeepaliveCommentsOmitUncoveredPhases keeps the config field, the
// pacer and the relay comment aligned with the user-facing Help. Start is
// called only from OnWait (queue admission or operator hold), so an
// admitted-immediately stream gets no comment before upstream headers and none
// during a retry backoff. A stale claim in any of those owners re-promises a
// phase the ticker does not cover.
func TestSSEKeepaliveCommentsOmitUncoveredPhases(t *testing.T) {
	for _, rel := range []string{
		"internal/config/config_types.go",
		"internal/proxy/sse_keepalive.go",
		"internal/proxy/proxy.go",
	} {
		source := repositoryText(t, rel)
		for _, stale := range []string{"upstream TTFB", "queue / hold / TTFB", "retry backoff, and"} {
			if strings.Contains(source, stale) {
				t.Errorf("%s still promises the stale keepalive scope %q", rel, stale)
			}
		}
	}
}

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
		filepath.Join(root, "internal", "proxy", "relay.go"),
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
			if err := c.Apply(sample); err != nil {
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

// alternateSchemaValue returns the override map that moves f off its default:
// a single setting, changed on its own. It is a map rather than a bare value
// because that is what Apply takes, not because any setting is coupled to
// another.
func alternateSchemaValue(t *testing.T, f Field, base *Config) map[string]any {
	t.Helper()
	one := func(v any) map[string]any { return map[string]any{f.Key: v} }
	switch f.Kind {
	case KindBool:
		return one(!base.fieldValue(f.Key).(bool))
	case KindString:
		if f.Key == "listen" {
			return one("127.0.0.1:9999")
		}
		if f.Key == "quota_pause_mode" {
			// An exact-match enum: alternate with another member.
			return one(QuotaPauseRetry)
		}
		return one("alt-" + base.fieldValue(f.Key).(string))
	case KindInt:
		n := base.fieldValue(f.Key).(int)
		for _, cand := range []int{n + 1, n - 1} {
			trial := base.Clone()
			trial.fieldRV(f.Key).SetInt(int64(cand))
			if trial.Validate() == nil {
				return one(cand)
			}
		}
	// No setting is coupled to another today, so a single-key sample must
	// validate on its own for every kind. The one setting that ever needed a
	// companion is gone; if a coupled setting returns, the round-trip test
	// fails here with a clear "sample is still the default" message rather
	// than this probe quietly searching the bool settings for a fix.
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
				return one(FormatByteSize(cand))
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
				return one(FormatDuration(cand))
			}
		}
	case KindStrings:
		if f.Key == "allowed_base_urls" {
			return one([]any{"https://api.example.invalid"})
		}
		if f.Key == "storm_status_codes" {
			return one([]any{"500"})
		}
		if f.Key == "retryable_error_classes" {
			return one([]any{"model_warming_up"})
		}
	case KindProviders:
		return one(map[string]any{"neutral.example": map[string]any{"cost_keys": []any{"usage.cost"}}})
	case KindAliases:
		return one(map[string]any{"old.example": "new.example"})
	case KindModelRules:
		return one([]any{map[string]any{"mode": "lower"}})
	case KindRequestOverrides:
		return one([]any{map[string]any{"client": "opencode", "headers": map[string]any{"x-sample-hint": "v1"}}})
	case KindSubConversations:
		return one([]any{map[string]any{"client": "opencode", "params": []any{"promptCacheKey"}, "strip": true}})
	}
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

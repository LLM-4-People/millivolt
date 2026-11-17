package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/plugincred"
)

func pluginHash(code []byte) string {
	sum := sha256.Sum256(code)
	return hex.EncodeToString(sum[:])
}

func writePlugin(t *testing.T, dir, id string, code []byte, hash string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, id+".wasm"), code, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{
		"id":%q,"version":"0.1.0","abi":"v1","min_millivolt":"0.3.26",
		"capabilities":["emit_metric"],"module":%q,"module_sha256":%q,"license":"MIT"
	}`, id, id+".wasm", hash)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeLoadsAndPublishesRecord(t *testing.T) {
	dir := t.TempDir()
	code := fixtureModule()
	writePlugin(t, dir, "fixture", code, pluginHash(code))

	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	if err := r.LoadDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if !r.Enabled() {
		t.Fatal("runtime must report a loaded plugin")
	}

	start := time.Now()
	rec := &metrics.Record{
		ID:         "req-1",
		Provider:   "example.test",
		Model:      "model-a",
		Client:     "pi",
		StatusCode: 200,
		Start:      start,
		End:        start.Add(5 * time.Millisecond),
	}
	rec.Usage.InputTokens = 11
	rec.Usage.OutputTokens = 3
	r.PublishRecord(context.Background(), rec)

	if got := string(r.LastOutput()); got != string(fixtureResponse) {
		t.Fatalf("guest output = %q, want %q", got, fixtureResponse)
	}
	if failures := r.Failures(); failures != 0 {
		t.Fatalf("Failures() = %d, want 0", failures)
	}
}

func TestRuntimeRefusesDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "bad", fixtureModule(), strings.Repeat("0", 64))
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	if err := r.LoadDir(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("LoadDir error = %v, want digest mismatch", err)
	}
}

func TestRuntimeEmptyDirIsDisabled(t *testing.T) {
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	if err := r.LoadDir(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if r.Enabled() {
		t.Fatal("empty dir must not enable plugins")
	}
	r.PublishRecord(context.Background(), &metrics.Record{ID: "noop"})
	if r.LastOutput() != nil {
		t.Fatal("disabled runtime produced output")
	}
}

func TestFetchHTTPEnforcesAllowlistAndBodyCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"percent":62}`))
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	raw, err := json.Marshal(httpFetchRequest{
		URL:              server.URL + "/api/experimental/usage/limits",
		Method:           "GET",
		MaxResponseBytes: 32 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	allowed := fetchHTTP(context.Background(), raw, map[string]struct{}{host: {}})
	if allowed.Status != http.StatusOK || allowed.Body != `{"percent":62}` || allowed.Error != "" {
		t.Fatalf("allowed fetch = %+v", allowed)
	}
	denied := fetchHTTP(context.Background(), raw, map[string]struct{}{})
	if denied.Status != 0 || denied.Error == "" {
		t.Fatalf("denied fetch = %+v", denied)
	}
	raw, err = json.Marshal(httpFetchRequest{URL: server.URL, Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	badMethod := fetchHTTP(context.Background(), raw, map[string]struct{}{host: {}})
	if badMethod.Error == "" {
		t.Fatal("non-GET fetch was accepted")
	}
}

func TestFetchHTTPEnvSubstitutionAndCredential(t *testing.T) {
	t.Setenv("OPENCODE_WORKSPACE_ID", "wrk_test")
	t.Setenv("MILLIVOLT_PLUGIN_HTTP_CREDENTIAL_OPENCODE_CONSOLE", "auth=secret-value")
	var observedPath, observedCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedPath = r.URL.Path
		observedCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	raw, err := json.Marshal(httpFetchRequest{
		URL:              server.URL + "/workspace/${env:OPENCODE_WORKSPACE_ID}/go",
		CredentialID:     "opencode_console",
		CredentialHeader: "Cookie",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := fetchHTTPWithEnv(context.Background(), raw, map[string]struct{}{host: {}}, os.LookupEnv, nil)
	if got.Error != "" || got.Status != http.StatusOK {
		t.Fatalf("credentialed fetch = %+v", got)
	}
	if observedPath != "/workspace/wrk_test/go" || observedCookie != "auth=secret-value" {
		t.Fatalf("server saw path=%q cookie=%q", observedPath, "<redacted>")
	}
}

func TestFetchHTTPUsesRequestCredential(t *testing.T) {
	var observedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	raw, err := json.Marshal(httpFetchRequest{
		URL:              server.URL + "/usage",
		CredentialID:     requestCredentialID,
		CredentialHeader: "Authorization",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := plugincred.With(context.Background(), "Authorization", "Bearer sk-request-test")
	got := fetchHTTP(ctx, raw, map[string]struct{}{host: {}})
	if got.Error != "" || got.Status != http.StatusOK {
		t.Fatalf("request-credential fetch = %+v", got)
	}
	if observedAuth != "Bearer sk-request-test" {
		t.Fatalf("server saw unexpected Authorization value")
	}
}

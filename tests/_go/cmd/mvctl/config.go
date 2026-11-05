package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

const configJSON = `{
  "values": {
    "name": "live",
    "max_retries": 3,
    "mcp_timeout": "30s",
    "capture_body_preview": false,
    "providers": {"p": {"headers": {
      "Authorization": "Bearer secret-value",
      "X-Trace": "visible",
      "x-prompt-tokens": "5"
    }}}
  },
  "effective": {
    "name": "live",
    "max_retries": 3,
    "mcp_timeout": "30s",
    "capture_body_preview": false,
    "providers": {"p": {"headers": {"Authorization": "Bearer secret-value"}}}
  },
  "defaults": {"name": "default", "max_retries": 2, "mcp_timeout": "30s", "capture_body_preview": false, "providers": {}},
  "revision": "rev1",
  "restart_required": [],
  "fields": [
    {"key":"name","kind":"string"},
    {"key":"max_retries","kind":"int"},
    {"key":"mcp_timeout","kind":"duration"},
    {"key":"capture_body_preview","kind":"bool"},
    {"key":"providers","kind":"providers"},
    {"key":"provider_aliases","kind":"aliases"}
  ],
  "writable": true,
  "path": "/tmp/proxy.yaml"
}`

func TestConfigGetRedactsCredentialHeadersOnly(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/config": jsonHandler(t, configJSON),
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "config", "get", "--json")
	if code != 0 {
		t.Fatalf("config get = %d", code)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	values := document["values"].(map[string]any)
	providers := values["providers"].(map[string]any)
	headers := providers["p"].(map[string]any)["headers"].(map[string]any)
	if headers["Authorization"] != "[redacted]" {
		t.Errorf("Authorization = %v, want redacted", headers["Authorization"])
	}
	if headers["x-prompt-tokens"] != "5" {
		t.Errorf("x-prompt-tokens = %v, want the token counter preserved", headers["x-prompt-tokens"])
	}
	if strings.Contains(stdout, "secret-value") {
		t.Fatal("config get --json leaked a credential")
	}
	code, stdout, _ = runCLI(t, "--url", fixture.url(), "config", "get", "--reveal", "--json")
	if code != 0 || !strings.Contains(stdout, "secret-value") {
		t.Fatalf("config get --reveal = %d, did not reveal", code)
	}
}

func TestConfigSetUsesSchemaKindsAndToleratesCommitted500(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/config": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, configJSON)
				return
			}
			body, _ := io.ReadAll(r.Body)
			want := `{"revision":"rev1","values":{"name":"123","max_retries":5,"mcp_timeout":"2m","provider_aliases":{"old":"new"}}}`
			assertJSONEqual(t, string(body), want)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"saved":true,"error":"settings saved, but reload failed: denied"}`)
		},
	})
	code, stdout, stderr := runCLI(t, "--url", fixture.url(), "config", "set",
		"name=123", "max_retries=5", "mcp_timeout=2m", `provider_aliases={"old":"new"}`, "--yes")
	if code != 0 {
		t.Fatalf("config set = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "saved") || !strings.Contains(stdout, "reload failed") {
		t.Fatalf("config set did not report the committed warning: %q", stdout)
	}
}

func TestConfigSetRejectsUnknownKeyBeforeWriting(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/config": jsonHandler(t, configJSON),
	})
	code, _, stderr := runCLI(t, "--url", fixture.url(), "config", "set", "bogus=1", "--yes")
	if code != 2 || !strings.Contains(stderr, "unknown config key") {
		t.Fatalf("config set = %d, stderr %q", code, stderr)
	}
	if got := fixture.callCount("/admin/config"); got != 1 {
		t.Fatalf("config set made %d calls, want only the revision GET", got)
	}
}

func TestConfigReloadAndDiff(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/config": jsonHandler(t, configJSON),
		"/admin/reload": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"ok":true,"restart_required":[],"dropped_keys":[]}`)
		},
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "config", "reload", "--yes")
	if code != 0 || !strings.Contains(stdout, "config reloaded") {
		t.Fatalf("reload = %d, %q", code, stdout)
	}
	code, stdout, _ = runCLI(t, "--url", fixture.url(), "config", "diff")
	if code != 0 || !strings.Contains(stdout, "name") {
		t.Fatalf("diff = %d, %q", code, stdout)
	}
}

func TestConfigKeysAndSingleValue(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/config": jsonHandler(t, configJSON),
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "config", "keys", "mcp")
	if code != 0 || !strings.Contains(stdout, "mcp_timeout") || strings.Contains(stdout, "max_retries") {
		t.Fatalf("config keys = %d, %q", code, stdout)
	}
	code, stdout, _ = runCLI(t, "--url", fixture.url(), "config", "get", "max_retries")
	if code != 0 || !strings.Contains(stdout, "max_retries = 3") {
		t.Fatalf("config get key = %d, %q", code, stdout)
	}
	if code, _, _ := runCLI(t, "--url", fixture.url(), "config", "get", "missing"); code != 2 {
		t.Fatalf("config get missing = %d, want 2", code)
	}
}

func TestConfigSetPreviewMakesNoWrite(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/config": jsonHandler(t, configJSON),
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "config", "set", "max_retries=4")
	if code != 0 || !strings.Contains(stdout, "Preview only") {
		t.Fatalf("config preview = %d, %q", code, stdout)
	}
	for _, call := range fixture.callsFor("/admin/config") {
		if call.Method != http.MethodGet {
			t.Fatalf("config preview wrote: %+v", call)
		}
	}
}

func TestConfigSetRejectsMissingRevision(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/config": jsonHandler(t, strings.Replace(configJSON, `"revision": "rev1"`, `"revision": ""`, 1)),
	})
	code, _, stderr := runCLI(t, "--url", fixture.url(), "config", "set", "max_retries=4", "--yes")
	if code != 1 || !strings.Contains(stderr, "no config revision") {
		t.Fatalf("config set = %d, stderr %q", code, stderr)
	}
}

func TestConfigSetRedactsSavedProviderHeaders(t *testing.T) {
	response := `{"saved":true,"values":{"name":"live","providers":{"p":{"headers":{
		"Authorization":"Bearer secret-value","X-Trace":"visible"}}}},
		"effective":{"providers":{"p":{"headers":{"Authorization":"Bearer secret-value"}}}}}`
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/config": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, configJSON)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, response)
		},
	})
	code, stdout, stderr := runCLI(t, "--url", fixture.url(), "config", "set", "max_retries=4", "--yes", "--json")
	if code != 0 {
		t.Fatalf("config set = %d, stderr %q", code, stderr)
	}
	if strings.Contains(stdout, "secret-value") {
		t.Fatalf("config set --json leaked a saved provider header: %q", stdout)
	}
	if !strings.Contains(stdout, "[redacted]") || !strings.Contains(stdout, "visible") {
		t.Fatalf("config set did not apply the shared redactor: %q", stdout)
	}
	code, stdout, _ = runCLI(t, "--url", fixture.url(), "config", "set", "max_retries=4", "--yes", "--reveal", "--json")
	if code != 0 || !strings.Contains(stdout, "secret-value") {
		t.Fatalf("config set --reveal = %d, did not reveal: %q", code, stdout)
	}
}

func TestConfigSetRejectsEmptyBoolean(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/config": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, configJSON)
				return
			}
			t.Error("config set posted an empty boolean")
		},
	})
	code, _, stderr := runCLI(t, "--url", fixture.url(), "config", "set", "capture_body_preview=", "--yes")
	if code != 2 || !strings.Contains(stderr, "empty value is not a bool") {
		t.Fatalf("empty bool = %d, stderr %q", code, stderr)
	}
}

func TestConfigGetRedactsLastReloadError(t *testing.T) {
	withReload := strings.Replace(configJSON, `"writable": true,`,
		`"last_reload": {"ok": false, "error": "rejected Authorization: Bearer secret-value", "dropped_keys": []},
  "writable": true,`, 1)
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/config": jsonHandler(t, withReload),
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "config", "get", "--json")
	if code != 0 || strings.Contains(stdout, "secret-value") {
		t.Fatalf("config get leaked the last reload error: %d %q", code, stdout)
	}
	if !strings.Contains(stdout, "[redacted]") {
		t.Fatalf("config get did not mark the omitted reload text: %q", stdout)
	}
	code, stdout, _ = runCLI(t, "--url", fixture.url(), "config", "get", "--reveal", "--json")
	if code != 0 || !strings.Contains(stdout, "secret-value") {
		t.Fatalf("config get --reveal = %d, did not reveal the reload error: %q", code, stdout)
	}
}

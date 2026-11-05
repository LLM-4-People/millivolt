package main

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupStreamsMagicAndSelectsMembers(t *testing.T) {
	payload := "MVB1payload"
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/backup": func(w http.ResponseWriter, r *http.Request) {
			query := r.URL.Query()
			if got := query.Get("config"); got != "1" {
				t.Errorf("config = %q, want 1", got)
			}
			if got := query.Get("database"); got != "0" {
				t.Errorf("database = %q, want 0", got)
			}
			_, _ = io.WriteString(w, payload)
		},
	})
	dir := t.TempDir()
	out := filepath.Join(dir, "backup.mvb")
	code, stdout, stderr := runCLI(t, "--url", fixture.url(), "backup", "--out", out, "--database=false")
	if code != 0 || stderr != "" {
		t.Fatalf("backup = %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "backup saved") {
		t.Fatalf("backup output = %q", stdout)
	}
	raw, err := os.ReadFile(out)
	if err != nil || string(raw) != payload {
		t.Fatalf("archive = %q, %v", raw, err)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if code, _, _ := runCLI(t, "--url", fixture.url(), "backup", "--out", out, "--database=false"); code != 2 {
		t.Fatalf("overwrite refusal = %d, want 2", code)
	}
	if code, _, _ := runCLI(t, "--url", fixture.url(), "backup", "--out", out, "--database=false", "--overwrite"); code != 0 {
		t.Fatalf("overwrite = %d", code)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".part") {
			t.Errorf("temporary archive left behind: %s", entry.Name())
		}
	}
}

func TestBackupRejectsNonArchiveWithoutPublishing(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/backup": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<html>not an archive</html>")
		},
	})
	dir := t.TempDir()
	out := filepath.Join(dir, "backup.mvb")
	code, _, stderr := runCLI(t, "--url", fixture.url(), "backup", "--out", out)
	if code != 1 || !strings.Contains(stderr, "not a millivolt archive") {
		t.Fatalf("backup = %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("non-archive was published at %s", out)
	}
}

func TestBackupRequiresAMember(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/backup": jsonHandler(t, `{}`),
	})
	out := filepath.Join(t.TempDir(), "backup.mvb")
	code, _, _ := runCLI(t, "--url", fixture.url(), "backup", "--out", out, "--config=false", "--database=false")
	if code != 2 {
		t.Fatalf("both members off = %d, want 2", code)
	}
	if fixture.callCount("/admin/backup") != 0 {
		t.Fatal("invalid selection reached the proxy")
	}
}

func TestRestoreStreamsExplicitSelection(t *testing.T) {
	archive := "MVB1restore-data"
	path := filepath.Join(t.TempDir(), "archive.mvb")
	if err := os.WriteFile(path, []byte(archive), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/restore": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if string(body) != archive {
				t.Errorf("uploaded body = %q, want %q", body, archive)
			}
			query, _ := url.ParseQuery(r.URL.RawQuery)
			if got := query.Get("config"); got != "1" {
				t.Errorf("config = %q", got)
			}
			if got := query.Get("database"); got != "1" {
				t.Errorf("database = %q", got)
			}
			if got := query.Get("config_mode"); got != "replace" {
				t.Errorf("config_mode = %q", got)
			}
			if got := query.Get("database_mode"); got != "merge" {
				t.Errorf("database_mode = %q", got)
			}
			if got := query.Get("inspect"); got == "1" {
				_, _ = io.WriteString(w, `{"ok":true,"members":{"config":true,"database":true}}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"restart_required":["db_path"]}`)
		},
	})
	code, stdout, stderr := runCLI(t, "--url", fixture.url(), "restore", "--file", path, "--database-mode", "merge")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "inspect complete") {
		t.Fatalf("inspect = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	calls := fixture.callsFor("/admin/restore")
	if !strings.Contains(calls[len(calls)-1].Query, "inspect=1") {
		t.Fatalf("inspect query = %q", calls[len(calls)-1].Query)
	}
	code, stdout, _ = runCLI(t, "--url", fixture.url(), "restore", "--file", path, "--database-mode", "merge", "--yes")
	if code != 0 || !strings.Contains(stdout, "restart required") {
		t.Fatalf("apply = %d, %q", code, stdout)
	}
	calls = fixture.callsFor("/admin/restore")
	if strings.Contains(calls[len(calls)-1].Query, "inspect=1") {
		t.Fatalf("apply query still inspects: %q", calls[len(calls)-1].Query)
	}
}

func TestBackupUsesConfiguredDefaults(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/backup": func(w http.ResponseWriter, r *http.Request) {
			query := r.URL.Query()
			if query.Get("config") != "1" || query.Get("database") != "1" {
				t.Errorf("default selection = config %q database %q, want both 1", query.Get("config"), query.Get("database"))
			}
			_, _ = io.WriteString(w, "MVB1payload")
		},
	})
	out := filepath.Join(t.TempDir(), "backup.mvb")
	if code, _, stderr := runCLI(t, "--url", fixture.url(), "backup", "--out", out); code != 0 {
		t.Fatalf("backup = %d, stderr %q", code, stderr)
	}
}

func TestBackupRejectsTruncatedStream(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/backup": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "MVB1short")
		},
	})
	out := filepath.Join(t.TempDir(), "backup.mvb")
	code, _, stderr := runCLI(t, "--url", fixture.url(), "backup", "--out", out)
	if code != 1 || !strings.Contains(stderr, "stream backup") {
		t.Fatalf("truncated backup = %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("truncated archive was published at %s", out)
	}
}

func TestRestoreInspectRedactsProviderHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.mvb")
	if err := os.WriteFile(path, []byte("MVB1data"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspect := `{"ok":true,"config":{"present":true,"bytes":10,"modified":["name"],
		"values":{"name":"archive","providers":{"p":{"headers":{
			"Authorization":"Bearer secret-value","X-Trace":"visible"}}}},
		"vs_live":["name"]},"database":{"present":false}}`
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/restore": jsonHandler(t, inspect),
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "restore", "--file", path, "--json")
	if code != 0 || strings.Contains(stdout, "secret-value") {
		t.Fatalf("restore inspect leaked a provider header: %d %q", code, stdout)
	}
	if !strings.Contains(stdout, "[redacted]") || !strings.Contains(stdout, "visible") {
		t.Fatalf("restore inspect did not apply the shared redactor: %q", stdout)
	}
	code, human, _ := runCLI(t, "--url", fixture.url(), "restore", "--file", path)
	if code != 0 || !strings.Contains(human, "replace") || !strings.Contains(human, "restore selection") {
		t.Fatalf("restore inspect did not name its modes: %d %q", code, human)
	}
	code, stdout, _ = runCLI(t, "--url", fixture.url(), "restore", "--file", path, "--reveal", "--json")
	if code != 0 || !strings.Contains(stdout, "secret-value") {
		t.Fatalf("restore inspect --reveal = %d, did not reveal: %q", code, stdout)
	}
}

func TestRestoreRejectsInvalidInputBeforeUpload(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/restore": jsonHandler(t, `{"ok":true}`),
	})
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.mvb")
	if err := os.WriteFile(bad, []byte("NOPE"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good.mvb")
	if err := os.WriteFile(good, []byte("MVB1data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"restore", "--file", bad, "--yes"}, 2},
		{[]string{"restore", "--file", good, "--config=false", "--database=false", "--yes"}, 2},
		{[]string{"restore", "--file", good, "--config-mode", "bogus", "--yes"}, 2},
		{[]string{"restore", "--file", good, "--config-mode", "", "--yes"}, 2},
		{[]string{"restore", "--file", filepath.Join(dir, "missing.mvb"), "--yes"}, 1},
	} {
		code, _, _ := runCLI(t, append([]string{"--url", fixture.url()}, tc.args...)...)
		if code != tc.want {
			t.Errorf("%v = %d, want %d", tc.args, code, tc.want)
		}
	}
	if got := fixture.callCount("/admin/restore"); got != 0 {
		t.Fatalf("invalid restore reached the proxy %d times", got)
	}
	// The mode grammar is normalized by internal/backup: the HTTP query key and
	// the flag accept the same spellings.
	code, _, _ := runCLI(t, "--url", fixture.url(), "restore", "--file", good, "--database-mode", " MERGE ", "--yes")
	if code != 0 {
		t.Fatalf("normalized mode = %d", code)
	}
	calls := fixture.callsFor("/admin/restore")
	if !strings.Contains(calls[len(calls)-1].Query, "database_mode=merge") {
		t.Fatalf("normalized mode query = %q", calls[len(calls)-1].Query)
	}
}

func TestRestoreInspectWinsOverYes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.mvb")
	if err := os.WriteFile(path, []byte("MVB1data"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/restore": jsonHandler(t, `{"ok":true}`),
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "restore", "--file", path, "--inspect", "--yes")
	if code != 0 || !strings.Contains(stdout, "inspect complete") {
		t.Fatalf("restore inspect = %d, %q", code, stdout)
	}
	calls := fixture.callsFor("/admin/restore")
	if !strings.Contains(calls[len(calls)-1].Query, "inspect=1") {
		t.Fatalf("inspect query = %q", calls[len(calls)-1].Query)
	}
}

func TestRestoreSelectionFlagsAndNonRegularFile(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/restore": jsonHandler(t, `{"ok":true,"config":{"present":false},"database":{"present":false}}`),
	})
	dir := t.TempDir()
	if code, _, stderr := runCLI(t, "--url", fixture.url(), "restore", "--file", dir); code != 2 || !strings.Contains(stderr, "regular file") {
		t.Fatalf("directory restore = %d, stderr %q", code, stderr)
	}
	path := filepath.Join(dir, "archive.mvb")
	if err := os.WriteFile(path, []byte("MVB1data"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, "--url", fixture.url(), "restore", "--file", path, "--config=false")
	if code != 0 {
		t.Fatalf("config-off inspect = %d, stderr %q", code, stderr)
	}
	calls := fixture.callsFor("/admin/restore")
	query, _ := url.ParseQuery(calls[len(calls)-1].Query)
	if query.Get("config") != "0" || query.Get("database") != "1" || query.Get("inspect") != "1" {
		t.Fatalf("selection query = %q, want config=0 database=1 inspect=1", calls[len(calls)-1].Query)
	}
}

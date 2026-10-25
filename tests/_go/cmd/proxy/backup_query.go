package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// TestParseRestoreModePresence pins that an absent key keeps the documented
// replace default while an explicit empty or whitespace value is a malformed
// mode, never a silent replace.
func TestParseRestoreModePresence(t *testing.T) {
	for _, key := range []string{"config_mode", "database_mode"} {
		for _, tc := range []struct {
			name    string
			q       url.Values
			want    string
			wantErr bool
		}{
			{"absent", url.Values{}, "replace", false},
			{"empty", url.Values{key: {""}}, "", true},
			{"whitespace", url.Values{key: {" \t "}}, "", true},
			{"merge", url.Values{key: {"merge"}}, "merge", false},
			{"replace", url.Values{key: {"replace"}}, "replace", false},
			{"uppercase", url.Values{key: {" MERGE "}}, "merge", false},
			{"invalid", url.Values{key: {"bogus"}}, "", true},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				got, err := parseRestoreMode(tc.q, key)
				if tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), key) {
						t.Fatalf("parseRestoreMode(%q) = %q, %v, want a %s error", tc.name, got, err, key)
					}
					return
				}
				if err != nil || got != tc.want {
					t.Fatalf("parseRestoreMode(%q) = %q, %v, want %q", tc.name, got, err, tc.want)
				}
			})
		}
	}
}

// TestBackupRestoreAdoptStrictQuery pins the destructive plane's strict-query
// adoption: both routes parse the raw query string, so a malformed pair can
// never be silently dropped into a broader action (inspect=%zz must not turn
// a would-be 400 into a real restore; config=1&database=%zz must not
// silently skip the database member), every repeated consumed flag is
// denied, never first-wins, and every key outside the consumed set is
// rejected outright - the ignore class belongs to idempotent fetches, while
// on a mutating route an unknown or case-variant key silently selects the
// default, broader action (?INSPECT=1 would run the real restore where
// ?inspect=1 previews). The fixture archive carries both members so each
// dropped or ignored pair would otherwise change what the restore really
// does. Each row also pins the 400's wording through the routes' JSON error
// transport, and the boundary rows pin the gate order: parse error,
// duplicates, unknown keys, then the value grammar - whose tail rows pin
// the member and mode 400s exactly, an invalid mode spelling answering the
// 400 rather than silently selecting the broader replace default.
func TestBackupRestoreAdoptStrictQuery(t *testing.T) {
	liveReloadFixture(t)
	// A non-default spelling so the closing invariant can see a silent
	// restore: the fixture archive carries the default config, so a crafted
	// query that slips past the gates rewrites these bytes.
	start := liveCfg.Clone()
	start.MaxRetries = 9
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	live, _, livePath, dir := openLiveStoreFixture(t)
	live.Record(&metrics.Record{ID: "strict-row", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := live.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var yamlBuf bytes.Buffer
	if err := config.WriteYAML(&yamlBuf, config.Default()); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(dir, backup.Archive{Config: yamlBuf.Bytes(), Database: data})
	if err != nil {
		t.Fatal(err)
	}
	liveCfg = config.Default()
	liveCfg.DBPath = livePath
	liveStore = live
	beforeConfig, err := os.ReadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)

	for _, tc := range []struct {
		name, target, method, want string
	}{
		{"restore inspect=%zz must not become a real restore", "/admin/restore?inspect=%zz", http.MethodPost, "invalid query"},
		{"restore config=1&database=%zz must not skip the database member", "/admin/restore?config=1&database=%zz", http.MethodPost, "invalid query"},
		{"repeated inspect flag is denied, never first-wins", "/admin/restore?inspect=1&inspect=0", http.MethodPost, "duplicate inspect"},
		{"repeated config_mode is denied, never first-wins", "/admin/restore?config_mode=merge&config_mode=replace", http.MethodPost, "duplicate config_mode"},
		{"repeated database_mode is denied, never first-wins", "/admin/restore?database_mode=merge&database_mode=replace", http.MethodPost, "duplicate database_mode"},
		{"backup config=1&database=%zz must not silently become config-only", "/admin/backup?config=1&database=%zz", http.MethodGet, "invalid query"},
		{"repeated backup config flag is denied, never first-wins", "/admin/backup?config=1&config=0", http.MethodGet, "duplicate config"},
		{"restore INSPECT=1 must not run the real restore as the default action", "/admin/restore?INSPECT=1", http.MethodPost, "unknown key INSPECT"},
		{"restore CONFIG_MODE=merge must not silently become the replace default", "/admin/restore?CONFIG_MODE=merge", http.MethodPost, "unknown key CONFIG_MODE"},
		{"backup CONFIG=1 must not silently broaden the archive", "/admin/backup?CONFIG=1", http.MethodGet, "unknown key CONFIG"},
		{"restore bogus=1 is an unknown key, not an ignored one", "/admin/restore?bogus=1", http.MethodPost, "unknown key bogus"},
		{"backup bogus=1 is an unknown key, not an ignored one", "/admin/backup?bogus=1", http.MethodGet, "unknown key bogus"},
		{"a parse error outranks the unknown key", "/admin/restore?bogus=1&bad=%zz", http.MethodPost, "invalid query"},
		{"a restore parse error outranks the duplicate key", "/admin/restore?config=1&config=0&bad=%zz", http.MethodPost, "invalid query"},
		{"a duplicate outranks the unknown key", "/admin/restore?config=1&config=0&bogus=1", http.MethodPost, "duplicate config"},
		{"an unknown key outranks the value grammar", "/admin/restore?inspect=x&bogus=1", http.MethodPost, "unknown key bogus"},
		{"the value grammar answers last", "/admin/restore?inspect=x", http.MethodPost, "inspect: want 1 or 0"},
		{"an invalid restore config member answers the member grammar's 400", "/admin/restore?config=x", http.MethodPost, "config: want 1 or 0"},
		{"an invalid restore database member answers the member grammar's 400", "/admin/restore?database=x", http.MethodPost, "database: want 1 or 0"},
		{"an invalid config_mode answers the 400, never a silent replace", "/admin/restore?config_mode=bogus", http.MethodPost, "config_mode: want merge or replace"},
		{"an invalid database_mode answers the 400, never a silent replace", "/admin/restore?database_mode=bogus", http.MethodPost, "database_mode: want merge or replace"},
		{"an explicit empty config_mode is denied, never the replace default", "/admin/restore?config_mode=", http.MethodPost, "config_mode: empty value; want merge or replace"},
		{"an explicit whitespace database_mode is denied, never the replace default", "/admin/restore?database_mode=%20", http.MethodPost, "database_mode: empty value; want merge or replace"},
		{"a backup parse error outranks the duplicate key", "/admin/backup?config=1&config=0&bad=%zz", http.MethodGet, "invalid query"},
		{"a backup duplicate outranks the unknown key", "/admin/backup?config=1&config=0&bogus=1", http.MethodGet, "duplicate config"},
		{"a backup unknown key outranks the value grammar", "/admin/backup?config=x&bogus=1", http.MethodGet, "unknown key bogus"},
		{"an invalid backup config member answers the member grammar's 400", "/admin/backup?config=x", http.MethodGet, "config: want 1 or 0"},
		{"an invalid backup database member answers the member grammar's 400", "/admin/backup?database=x", http.MethodGet, "database: want 1 or 0"},
	} {
		w := httptest.NewRecorder()
		var body io.Reader
		if tc.method == http.MethodPost {
			body = bytes.NewReader(raw)
		}
		mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.target, body))
		var errBody struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
			t.Errorf("%s: body is not the flat error shape: %v (%q)", tc.name, err, w.Body.String())
			continue
		}
		if w.Code != http.StatusBadRequest || errBody.Error != tc.want {
			t.Errorf("%s: status=%d error=%q, want 400 %q", tc.name, w.Code, errBody.Error, tc.want)
		}
	}
	// No crafted query may reach a destructive side effect: the config file
	// stays byte-identical and no database snapshot is staged.
	afterConfig, err := os.ReadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatal("a crafted query rewrote the live config file")
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(livePath)); !os.IsNotExist(err) {
		t.Fatal("a crafted query staged a database snapshot")
	}
}

// TestRestorePercentDecodedConsumedKeyIsConsumed is the percent-decoding freeze
// row: a percent-decoded spelling of a consumed key (?%63onfig=1) IS the
// consumed key, never an unknown one, so the restore runs exactly as the
// canonical ?config=1 does. The canonical spelling's behavior is pinned
// collectively: the route-level round-trip tests pin the restore 200s and
// the applied config, while the ok-marker pins live in the other-spelling
// rows (the database-stage and merge restores). A strict parse that stopped
// decoding keys would answer the unknown-key 400 instead of restoring.
func TestRestorePercentDecodedConsumedKeyIsConsumed(t *testing.T) {
	liveReloadFixture(t)
	start := config.Default()
	start.MaxRetries = 9
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadConfig(); err != nil {
		t.Fatal(err)
	}
	arch := config.Default()
	arch.MaxRetries = 2
	var yamlBuf bytes.Buffer
	if err := config.WriteYAML(&yamlBuf, arch); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode("", backup.Archive{Config: yamlBuf.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/restore?%63onfig=1", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("decoded spelling status %d body %s, want the 200 the canonical ?config=1 answers (the decoded key is consumed, not unknown)", rr.Code, rr.Body.String())
	}
	var body struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || !body.OK {
		t.Fatalf("restore body %s is not the ok marker the canonical spelling answers", rr.Body.String())
	}
	if liveCfg.MaxRetries != 2 {
		t.Fatalf("the decoded spelling did not run the real restore: max_retries=%d, want the archive's 2", liveCfg.MaxRetries)
	}
}

// TestBackupRestoreConsumedQueryKeysPinned is the consumed-set source pin:
// nothing bounded the two lists' contents, so widening restoreQueryKeys by
// one key would silently re-admit the ignore class wave 24 removed. The pin
// reads cmd/proxy/backup.go's source text (the package dir is the test
// binary's working directory, the same relative-read convention
// dev_config.go uses for scripts/dev.sh), asserts the exact list literals,
// confirms the parsed literals against the live package variables so a
// stale parse can never pass, and requires each route's two gates
// (DuplicateQueryKey and UnknownQueryKey) to be called exactly once with
// the same list variable, so the set cannot drift between the gates.
func TestBackupRestoreConsumedQueryKeysPinned(t *testing.T) {
	raw, err := os.ReadFile("backup.go")
	if err != nil {
		t.Fatalf("read cmd/proxy/backup.go: %v", err)
	}
	src := string(raw)
	region := func(decl string) string {
		t.Helper()
		start := strings.Index(src, decl)
		if start < 0 {
			t.Fatalf("cmd/proxy/backup.go is missing %q", decl)
		}
		rest := src[start:]
		end := strings.Index(rest, "\n}")
		if end < 0 {
			t.Fatalf("%q is not terminated by a line-anchored closing brace", decl)
		}
		return rest[:end]
	}
	listLiteral := func(name string) []string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^\t` + name + `\s*=\s*\[\]string\{([^}]*)\}`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("%s is not a top-level []string literal in cmd/proxy/backup.go", name)
		}
		quotes := regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1)
		if len(quotes) == 0 {
			t.Fatalf("%s declares no keys", name)
		}
		keys := make([]string, 0, len(quotes))
		for _, q := range quotes {
			keys = append(keys, q[1])
		}
		return keys
	}
	wantBackup := []string{"config", "database"}
	wantRestore := []string{"inspect", "config", "database", "config_mode", "database_mode"}
	if got := listLiteral("backupQueryKeys"); !slices.Equal(got, wantBackup) {
		t.Errorf("backupQueryKeys source literal = %v, want exactly %v", got, wantBackup)
	}
	if !slices.Equal(backupQueryKeys, wantBackup) {
		t.Errorf("live backupQueryKeys = %v, want exactly %v (the source pin and the variable disagree)", backupQueryKeys, wantBackup)
	}
	if got := listLiteral("restoreQueryKeys"); !slices.Equal(got, wantRestore) {
		t.Errorf("restoreQueryKeys source literal = %v, want exactly %v", got, wantRestore)
	}
	if !slices.Equal(restoreQueryKeys, wantRestore) {
		t.Errorf("live restoreQueryKeys = %v, want exactly %v (the source pin and the variable disagree)", restoreQueryKeys, wantRestore)
	}
	for _, route := range []struct{ decl, list string }{
		{"func handleBackup(", "backupQueryKeys"},
		{"func handleRestore(", "restoreQueryKeys"},
	} {
		body := region(route.decl)
		calls := map[string]string{}
		for _, gate := range []string{"adminjson.DuplicateQueryKey", "adminjson.UnknownQueryKey"} {
			matches := regexp.MustCompile(regexp.QuoteMeta(gate)+`\(([^)]*)\)`).FindAllStringSubmatch(body, -1)
			if len(matches) != 1 {
				t.Errorf("%s calls %s %d times, want exactly one consumed-set gate call", route.decl, gate, len(matches))
				continue
			}
			args := strings.Split(matches[0][1], ",")
			if len(args) != 2 {
				t.Errorf("%s's %s call takes %d arguments, want the parsed query and one list", route.decl, gate, len(args))
				continue
			}
			calls[gate] = strings.TrimSpace(args[1])
		}
		want := route.list + "..."
		if calls["adminjson.DuplicateQueryKey"] != want || calls["adminjson.UnknownQueryKey"] != want {
			t.Errorf("%s gates read %q and %q, want both to pass %s", route.decl,
				calls["adminjson.DuplicateQueryKey"], calls["adminjson.UnknownQueryKey"], want)
		}
	}
}

func TestBackupRequiresAPart(t *testing.T) {
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/backup?config=0&database=0", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
}

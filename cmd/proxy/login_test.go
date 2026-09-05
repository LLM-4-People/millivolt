package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoginFailuresDoNotPrintResponseBodies(t *testing.T) {
	// Stub every network/credential tool: these are error-reporting fixtures,
	// not real account logins or cryptographic/protocol compatibility tests.
	for _, tc := range []struct{ script, status, want string }{
		{"cursor-login.sh", "200", "no accessToken"},
		{"grok-login.sh", "400", "device-code request failed"},
		{"grok-login.sh", "200", "no device_code"},
	} {
		t.Run(tc.script+tc.status, func(t *testing.T) {
			dir := t.TempDir()
			const secret = "SENSITIVE_FIXTURE_MUST_NOT_LOG"
			stubs := map[string]string{
				"curl":    "#!/bin/sh\nprintf '%s\\n%s\\n' '{\"refresh_token\":\"" + secret + "\"}' \"$FIXTURE_STATUS\"\n",
				"jq":      "#!/bin/sh\ncat >/dev/null\n",
				"openssl": "#!/bin/sh\nprintf fixture\n",
			}
			for name, body := range stubs {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "../../scripts/"+tc.script)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"NO_OPEN=1", "FIXTURE_STATUS="+tc.status,
				"CURSOR_API_BASE=http://fixture.invalid", "CURSOR_LOGIN_BASE=http://fixture.invalid",
				"XAI_AUTH_BASE=http://fixture.invalid", "CURSOR_LOGIN_TIMEOUT=10", "GROK_LOGIN_TIMEOUT=10")
			out, err := cmd.CombinedOutput()
			if err == nil || ctx.Err() != nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("expected immediate fixture failure %q: err=%v context=%v", tc.want, err, ctx.Err())
			}
			if strings.Contains(string(out), secret) {
				t.Fatal("failed login printed a credential-bearing response body")
			}
		})
	}
}

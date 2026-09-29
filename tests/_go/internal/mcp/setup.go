package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestSetupValidation pins the fail-fast rules: a missing credential or a
// malformed origin is a setup error, and a trailing slash is normalized.
func TestSetupValidation(t *testing.T) {
	valid := "0123456789abcdef"

	for _, tc := range []struct {
		name  string
		url   string
		token string
		want  string
	}{
		{"missing url", "", valid, "proxy URL is required"},
		{"missing token", "http://127.0.0.1:8081", "", "operator token is required"},
		{"short token", "http://127.0.0.1:8081", "0123456789abcde", "at least 16"},
		{"long token", "http://127.0.0.1:8081", strings.Repeat("x", operatorTokenMaxLen+1), "at most 512"},
		{"wrong scheme", "ftp://127.0.0.1:8081", valid, "http or https"},
		{"no host", "http://", valid, "must include a host"},
		{"path", "http://127.0.0.1:8081/admin", valid, "no path"},
		{"query", "http://127.0.0.1:8081/?a=1", valid, "query or fragment"},
		{"fragment", "http://127.0.0.1:8081#x", valid, "query or fragment"},
		{"credentials in url", "http://user:pass@127.0.0.1:8081", valid, "must not carry credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewService(tc.url, tc.token, DefaultLimits())
			if err == nil {
				t.Fatalf("setup must fail for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q must mention %q", err, tc.want)
			}
			assertNoToken(t, "setup error", err.Error())
		})
	}

	// The length band is inclusive at both ends, like the proxy's boot check.
	for _, token := range []string{
		strings.Repeat("x", operatorTokenMinLen),
		strings.Repeat("x", operatorTokenMaxLen),
	} {
		if _, err := NewService("http://127.0.0.1:8081", token, DefaultLimits()); err != nil {
			t.Fatalf("a %d character token must be accepted: %v", len(token), err)
		}
	}

	// Trailing slashes normalize to a bare origin, and the origin never carries
	// the credential.
	for _, raw := range []string{"http://127.0.0.1:8081/", "  http://127.0.0.1:8081/  ", "http://127.0.0.1:8081"} {
		service, err := NewService(raw, valid, DefaultLimits())
		if err != nil {
			t.Fatalf("%q must normalize: %v", raw, err)
		}
		if got := service.Origin(); got != "http://127.0.0.1:8081" {
			t.Fatalf("Origin() = %q, want the normalized origin", got)
		}
		if strings.Contains(service.Origin(), valid) {
			t.Fatal("the origin must never carry the credential")
		}
	}
}

// TestLimitsValidation pins that an unusable limits policy is a setup failure
// rather than a per-call surprise.
func TestLimitsValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Limits)
		want   string
	}{
		{"no query rows", func(l *Limits) { l.QueryMaxRows = 0 }, "query max rows"},
		{"no page size", func(l *Limits) { l.PageSize = 0 }, "page size"},
		{"no query timeout", func(l *Limits) { l.QueryTimeout = 0 }, "query timeout"},
		{"no timeout", func(l *Limits) { l.Timeout = 0 }, "request timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := DefaultLimits()
			tc.mutate(&limits)
			if _, err := NewService("http://127.0.0.1:8081", "0123456789abcdef", limits); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("limits error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// TestTokenNeverAppearsInToolOutput pins the confidentiality property on the
// explore path: a reflected failure excerpt and a success payload never contain
// the credential, and the recorded wire traffic carries it only in the
// Authorization header. The other reflection paths are covered where they are
// driven: the query excerpt by TestErrorBodyIsRedactedOfTheCredential, the
// config-patch body by TestSetConfigRedactsTheCredentialOnTheToleratedPath, and
// every registered read tool's output by TestRegisteredToolsReachTheProxy.
func TestTokenNeverAppearsInToolOutput(t *testing.T) {
	proxy := newFakeProxy(t)
	// The fake echoes the credential into its error body, which is the worst
	// case a transport could face: the tool message must still be safe to log.
	proxy.respond(http.MethodGet, explorerPath, cannedResponse{
		Status: http.StatusInternalServerError, ContentType: "application/json",
		Body: `{"error":"upstream repeated ` + testToken + `"}`,
	})
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	proxy.json(http.MethodGet, schemaPath, `[{"type":"table","name":"requests","tbl_name":"requests","sql":"CREATE TABLE requests"}]`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	// A failure whose text happens to contain the credential is still a tool
	// message; the operator's own proxy would never send that, and if it did the
	// credential is already the caller's own input. What must never happen is
	// this server ADDING it.
	_, err := service.explore(ctx, ExploreInput{Dim: "provider"})
	if err == nil {
		t.Fatal("expected the echoed failure")
	}
	// The reflected excerpt is model-visible text, so the credential must be
	// gone from it, not only from the success payload checked below.
	assertRedacted(t, testToken, "explore failure", err.Error())

	// Now check the success path: no serialized tool output contains it.
	proxy.json(http.MethodGet, explorerPath, `{"dim":"provider","total":1,"groups":[{"name":"local"}],"rail":{},"scope":{}}`)
	out, err := service.explore(ctx, ExploreInput{Dim: "provider"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, marshalErr := json.Marshal(out)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	assertNoToken(t, "explore output", string(encoded))

	// And on the wire the credential appears only in the Authorization header.
	for _, request := range proxy.requests() {
		assertNoToken(t, request.Method+" "+request.Path+" url", request.Query.Encode())
		assertNoToken(t, request.Method+" "+request.Path+" body", request.Body)
	}
	origin := proxy.origin()
	if strings.Contains(origin, testToken) {
		t.Fatal("the credential must never appear in the request origin")
	}
}

// TestQueryTruncatesWithAnExplicitMarker pins the volume discipline: a
// runaway result is cut at the configured cap and the withholding is stated.
func TestQueryTruncatesWithAnExplicitMarker(t *testing.T) {
	proxy := newFakeProxy(t)
	var body strings.Builder
	body.WriteString("[")
	for i := range 12 {
		if i > 0 {
			body.WriteString(",")
		}
		body.WriteString(`{"id":"r` + string(rune('a'+i)) + `"}`)
	}
	body.WriteString("]")
	proxy.json(http.MethodGet, schemaPath, body.String())

	limits := DefaultLimits()
	limits.QueryMaxRows = 5
	service := newTestService(t, proxy, limits)

	out, err := service.query(context.Background(), QueryInput{SQL: "SELECT id FROM requests"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RowCount != 5 || len(out.Rows) != 5 {
		t.Fatalf("row cap not applied: %+v", out.Truncation)
	}
	if !out.Truncation.Truncated || out.Truncation.Shown != 5 || out.Truncation.Total != 12 {
		t.Fatalf("truncation = %+v", out.Truncation)
	}
	if !strings.Contains(out.Truncation.Marker, "5 of 12") || !strings.Contains(out.Truncation.Marker, "rows") {
		t.Fatalf("marker = %q, must state what was withheld", out.Truncation.Marker)
	}

	// An explicit max_rows wins over the configured default.
	explicit, err := service.query(context.Background(), QueryInput{SQL: "SELECT id FROM requests", MaxRows: 2})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.RowCount != 2 || explicit.Truncation.Total != 12 {
		t.Fatalf("explicit max_rows ignored: %+v", explicit.Truncation)
	}
	// A page that fits reports no truncation.
	fits, err := service.query(context.Background(), QueryInput{SQL: "SELECT id FROM requests", MaxRows: 50})
	if err != nil {
		t.Fatal(err)
	}
	if fits.Truncation.Truncated || fits.Truncation.Marker != "" || fits.RowCount != 12 {
		t.Fatalf("a fitting result must not claim truncation: %+v", fits.Truncation)
	}
	if _, err := service.query(context.Background(), QueryInput{SQL: "SELECT 1", MaxRows: maxQueryRowsCeiling + 1}); err == nil {
		t.Fatal("an absurd max_rows must be refused")
	}
}

// TestQueryRejectsNonSelectLocally pins the local pre-check, and that the proxy
// remains the authority: a statement this check passes is still sent.
func TestQueryRejectsNonSelectLocally(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, schemaPath, `[]`)
	service := newTestService(t, proxy, Limits{})
	for _, sql := range []string{"", "   ", "DELETE FROM requests", "SELECT 1; DROP TABLE requests"} {
		if _, err := service.query(context.Background(), QueryInput{SQL: sql}); err == nil {
			t.Fatalf("%q must be refused locally", sql)
		}
	}
	if len(proxy.requests()) != 0 {
		t.Fatal("a locally refused statement must not reach the proxy")
	}
	// The proxy still answers 413 for its own row/byte ceiling, which the tool
	// surfaces as the proxy's message.
	proxy.fail(http.MethodGet, schemaPath, http.StatusRequestEntityTooLarge, "query limit exceeded", "")
	if _, err := service.query(context.Background(), QueryInput{SQL: "SELECT 1"}); err == nil ||
		!strings.Contains(err.Error(), "413") {
		t.Fatalf("the proxy's own ceiling must surface as an error, got %v", err)
	}
}

// TestListingToolsTruncateWithAMarker pins the same discipline on every listing
// tool, including the ones whose size is the proxy's choice rather than ours.
func TestListingToolsTruncateWithAMarker(t *testing.T) {
	proxy := newFakeProxy(t)
	// The proxy may answer more groups than its documented cap; the tool states
	// the withholding rather than passing it on as if complete.
	groups := make([]string, 0, explorerMaxGroups+4)
	for i := range explorerMaxGroups + 4 {
		groups = append(groups, `{"name":"g`+string(rune('a'+i))+`","n":1}`)
	}
	proxy.json(http.MethodGet, explorerPath, `{"dim":"provider","total":30,"groups":[`+strings.Join(groups, ",")+`],"rail":{},"scope":{}}`)
	buckets := make([]string, 0, chartMaxBuckets+3)
	for i := range chartMaxBuckets + 3 {
		buckets = append(buckets, `{"t":`+string(rune('0'+i%10))+`,"req":1}`)
	}
	proxy.json(http.MethodGet, chartPath, `{"now_ms":1,"from_ms":0,"bucket_ms":1,"buckets":[`+strings.Join(buckets, ",")+`]}`)
	records := make([]string, 0, 12)
	for i := range 12 {
		records = append(records, `{"id":"r`+string(rune('a'+i))+`"}`)
	}
	proxy.json(http.MethodGet, logPath, `{"records":[`+strings.Join(records, ",")+`],"more":true,"cursor_ms":9,"cursor_id":"rl"}`)
	snapshotRecords := make([]string, 0, 12)
	for i := range 12 {
		snapshotRecords = append(snapshotRecords, `{"id":"n`+string(rune('a'+i))+`"}`)
	}
	proxy.json(http.MethodGet, bootstrapPath, `{"seq":1,"feed_id":"f","counters":{},"storage":{"enabled":true},`+
		`"records":[`+strings.Join(snapshotRecords, ",")+`]}`)
	proxy.respond(http.MethodGet, prometheusPat, cannedResponse{
		Status: http.StatusOK, ContentType: "text/plain",
		Body: strings.Repeat("metric 1\n", prometheusMaxLines+10),
	})

	limits := DefaultLimits()
	limits.PageSize = 5
	service := newTestService(t, proxy, limits)
	ctx := context.Background()

	explorer, err := service.explore(ctx, ExploreInput{Dim: "provider"})
	if err != nil {
		t.Fatal(err)
	}
	if len(explorer.Groups) != explorerMaxGroups || !explorer.Truncation.Truncated {
		t.Fatalf("explorer truncation = %+v (%d groups)", explorer.Truncation, len(explorer.Groups))
	}

	chart, err := service.chart(ctx, ChartInput{Window: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if len(chart.Buckets) != chartMaxBuckets || !chart.Truncation.Truncated {
		t.Fatalf("chart truncation = %+v (%d buckets)", chart.Truncation, len(chart.Buckets))
	}

	page, err := service.records(ctx, RecordsInput{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 10 || !page.Truncation.Truncated || page.Truncation.Total != 12 {
		t.Fatalf("records truncation = %+v (%d rows)", page.Truncation, len(page.Records))
	}

	snapshot, err := service.snapshot(ctx, SnapshotInput{MaxRecords: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != 4 || !snapshot.Truncation.Truncated {
		t.Fatalf("snapshot truncation = %+v", snapshot.Truncation)
	}

	prom, err := service.prometheus(ctx, PrometheusInput{})
	if err != nil {
		t.Fatal(err)
	}
	if prom.Lines != prometheusMaxLines || !prom.Truncation.Truncated {
		t.Fatalf("prometheus truncation = %+v (%d lines)", prom.Truncation, prom.Lines)
	}

	// Every marker names the numbers so the model can size the next request.
	for _, marker := range []string{
		explorer.Truncation.Marker, chart.Truncation.Marker,
		page.Truncation.Marker, snapshot.Truncation.Marker, prom.Truncation.Marker,
	} {
		if marker == "" || !strings.Contains(marker, "truncated:") {
			t.Fatalf("marker %q must state the withholding", marker)
		}
	}
}

// TestQueryReportsTheProxysTotalWhenBothLimitsFire pins the honest number when
// BOTH clamps withhold rows. The byte clamp runs after the row cap, so its input
// is already shortened; reporting that intermediate as the total would tell a
// model a 200-row result out of 500 was complete.
func TestQueryReportsTheProxysTotalWhenBothLimitsFire(t *testing.T) {
	rows := make([]string, 0, 12)
	for i := range 12 {
		rows = append(rows, `{"id":"r`+string(rune('a'+i))+`","blob":"`+strings.Repeat("x", 200)+`"}`)
	}
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, schemaPath, `[`+strings.Join(rows, ",")+`]`)

	limits := DefaultLimits()
	limits.QueryMaxRows = 3    // the row cap fires first
	limits.QueryMaxBytes = 400 // and the byte cap fires second
	service := newTestService(t, proxy, limits)

	out, err := service.query(context.Background(), QueryInput{SQL: "SELECT * FROM requests"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RowCount >= 3 {
		t.Fatalf("the byte cap must cut below the row cap: %d rows", out.RowCount)
	}
	if out.Truncation.Total != 12 {
		t.Fatalf("the reported total must be the proxy's 12 rows, not the row-capped 3: %+v", out.Truncation)
	}
	if !strings.Contains(out.Truncation.Marker, "0 of 12") && !strings.Contains(out.Truncation.Marker, " of 12 rows") {
		t.Fatalf("the marker must name the proxy's total: %q", out.Truncation.Marker)
	}
}

package main

import (
	"context"
	"maps"
	"os/exec"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// stdioFixtureToken arms the spawned binary's setup. It is a fixture value for
// a child process that is never pointed at a reachable proxy.
const stdioFixtureToken = "mcp-binary-stdio-fixture-credential"

// TestStdioBinaryServesTheSharedRegistry RUNS the built millivolt-mcp and
// drives a real stdio session through it: initialize, then tools/list. The
// list is compared with an in-process NewServer session rather than a repeated
// count or name list, so a CLI path whose registered surface differs from
// NewServer's (for example one that drops service.Register, or a NewServer
// that does) fails here even though the in-process live suite would stay
// green. The tool list is all a stdio client can observe, so a parallel
// constructor that registers exactly the same surface is not distinguishable
// by this test and is not what it guards against.
func TestStdioBinaryServesTheSharedRegistry(t *testing.T) {
	binary := buildBinary(t)
	command := exec.Command(binary)
	command.Env = environmentWithoutCredentials([]string{
		"MILLIVOLT_MCP_PROXY_URL=http://127.0.0.1:1",
		"MILLIVOLT_MCP_OPERATOR_TOKEN=" + stdioFixtureToken,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := sdk.NewClient(&sdk.Implementation{Name: "mcp-binary-test", Version: "0.0.0"}, nil).
		Connect(ctx, &sdk.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("connect to the built millivolt-mcp: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range listed.Tools {
		got[tool.Name] = true
	}
	service, err := mcp.NewService("http://127.0.0.1:1", stdioFixtureToken, mcp.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	want := inProcessToolNames(t, ctx, service)
	if len(got) == 0 {
		t.Fatal("the stdio binary served no tools; the CLI construction path did not register the shared surface")
	}
	if !maps.Equal(got, want) {
		t.Fatalf("tools/list from the stdio binary differs from the shared registry: got %v, want %v", got, want)
	}
}

// inProcessToolNames connects an in-memory session to the shared NewServer
// registry so a test can compare against production registration without
// repeating its count or its names.
func inProcessToolNames(t *testing.T, ctx context.Context, service *mcp.Service) map[string]bool {
	t.Helper()
	server := mcp.NewServer(service)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "registry-probe", Version: "0.0.0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	return names
}

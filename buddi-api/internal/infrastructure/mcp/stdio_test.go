package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/mcp"
)

// This binary doubles as a real MCP server over stdio. The client-side test spawns
// it as a subprocess, so the stdio path is exercised as it will actually be used —
// as a separate program — rather than as an in-process simulation of one.
const serveEnvVar = "BUDDI_MCP_TEST_SERVE"

// TestMain serves over stdio when asked, and otherwise runs the tests.
func TestMain(m *testing.M) {
	if os.Getenv(serveEnvVar) != "1" {
		os.Exit(m.Run())
	}

	server, err := mcp.NewServer("calendar", "0.1.0", echoTool("calendar.create_event"), echoTool("calendar.list_events"))
	if err != nil {
		panic(err)
	}

	if err := server.Serve(context.Background(), mcp.NewStdioTransport()); err != nil {
		panic(err)
	}

	os.Exit(0)
}

// TestStdioSubprocessReachesASeparateServerProgram covers the transport the
// calendar connector will use when it runs as its own binary.
func TestStdioSubprocessReachesASeparateServerProgram(t *testing.T) {
	binary := buildTestServer(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	command := exec.CommandContext(ctx, binary)
	command.Env = append(os.Environ(), serveEnvVar+"=1")

	clientTransport := newProcessTransport(t, command)

	client, err := mcp.Connect(ctx, mcp.ClientOptions{Name: "calendar", Transport: clientTransport})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if got := len(client.Tools()); got != 2 {
		t.Errorf("tools = %d, want 2", got)
	}

	result, err := client.CallTool(ctx, "calendar.create_event", json.RawMessage(`{"summary":"Deploy freeze"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if !json.Valid(result) {
		t.Errorf("result %q is not valid JSON", result)
	}

	if err := client.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	if err := command.Wait(); err != nil {
		t.Errorf("server process: %v", err)
	}
}

// buildTestServer compiles this test binary into a temporary directory, so the
// spawned server is the same code the tests run against.
func buildTestServer(t *testing.T) string {
	t.Helper()

	name := "mcp-stdio-server"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	binary := filepath.Join(t.TempDir(), name)

	command := exec.Command("go", "test", "-c", "-o", binary, ".")

	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build test server: %v\n%s", err, output)
	}

	return binary
}

// processTransport is a subprocess's stdio as a transport.
type processTransport struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  *bytes.Buffer
}

func newProcessTransport(t *testing.T, command *exec.Cmd) *processTransport {
	t.Helper()

	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}

	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}

	// Captured rather than inherited: a log line on stderr is invisible to the
	// protocol, so keeping it in a buffer lets a failure explain itself.
	stderr := &bytes.Buffer{}
	command.Stderr = stderr

	if err := command.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	t.Cleanup(func() { _ = stdin.Close() })

	return &processTransport{command: command, stdin: stdin, stdout: stdout, stderr: stderr}
}

func (p *processTransport) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *processTransport) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Close closes the write side only, which is what makes the server's read loop end
// with a clean EOF rather than a killed process.
func (p *processTransport) Close() error { return p.stdin.Close() }

func (p *processTransport) String() string { return p.stderr.String() }

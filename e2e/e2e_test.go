//go:build e2e

// Package e2e_test drives real Nx 23.3 builds against the nx-remote-cache
// binary on Garage and versitygw containers. It needs Docker and Node.js (npx).
package e2e_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/s3test"
)

const (
	trustedToken  = "e2e-trusted-token"
	readOnlyToken = "e2e-read-only-token"
	nxVersion     = "23.3"
	workspaceName = "e2e-workspace"
	project       = "@e2e-workspace/shop"
	// Nx 23 summarises cache use per run; after `nx reset` a hit can only come
	// from the remote cache.
	missMarker   = "Cache:             0/1 hit"
	remoteMarker = "Cache:             1/1 hit"
	readyTimeout = 30 * time.Second
	pollInterval = 100 * time.Millisecond
)

// syncBuffer collects server output written from another goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p) //nolint:wrapcheck // bytes.Buffer never fails
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestNxUsesRemoteCacheAndSurvivesStorageOutage(t *testing.T) {
	t.Parallel()
	binary := buildServer(t)
	for name, start := range s3test.Starters() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			backend := start(t)
			serverURL, serverLog := startServer(t, binary, backend)
			workspace := createWorkspace(t)

			out := nxBuild(t, workspace, serverURL, trustedToken)
			if !strings.Contains(out, missMarker) {
				t.Fatalf("first build should run and upload:\n%s", out)
			}
			if !strings.Contains(serverLog.String(), `method=PUT`) {
				t.Fatalf("no upload reached the server:\n%s", serverLog.String())
			}

			out = nxBuild(t, workspace, serverURL, readOnlyToken)
			if !strings.Contains(out, remoteMarker) {
				t.Fatalf("read-only build should restore from the remote cache:\n%s", out)
			}

			backend.Stop(t)
			out = nxBuild(t, workspace, serverURL, trustedToken)
			if !strings.Contains(out, missMarker) {
				t.Fatalf("build during storage outage should rebuild as a cache miss:\n%s", out)
			}
			if !strings.Contains(serverLog.String(), `msg="Storage unavailable"`) {
				t.Fatalf("outage not logged:\n%s", serverLog.String())
			}
		})
	}
}

func buildServer(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "nx-remote-cache")
	//nolint:gosec // builds this repo's server into a test temp dir
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../cmd/nx-remote-cache")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}
	return binary
}

func startServer(t *testing.T, binary string, backend *s3test.Backend) (string, *syncBuffer) {
	t.Helper()
	port := freePort(t)
	logs := &syncBuffer{}
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), binary)
	cmd.Env = append(os.Environ(),
		"PORT="+strconv.Itoa(port),
		"AWS_REGION="+backend.Region,
		"AWS_ACCESS_KEY_ID="+backend.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY="+backend.SecretAccessKey,
		"S3_BUCKET_NAME="+s3test.Bucket,
		"S3_ENDPOINT_URL="+backend.Endpoint,
		"NX_CACHE_ACCESS_TOKEN="+trustedToken,
		"NX_CACHE_READ_ONLY_TOKEN="+readOnlyToken,
	)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	url := "http://127.0.0.1:" + strconv.Itoa(port)
	waitHealthy(t, url, logs)
	return url, logs
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected address %T", listener.Addr())
	}
	return addr.Port
}

func waitHealthy(t *testing.T, url string, logs *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/health", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("server did not become healthy:\n%s", logs.String())
}

func createWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "npx", "-y", "create-nx-workspace@"+nxVersion, workspaceName,
		"--preset=react-monorepo", "--appName=shop", "--bundler=vite", "--style=css",
		"--e2eTestRunner=none", "--unitTestRunner=none", "--linter=none", "--formatter=none",
		"--aiAgents=none", "--nxCloud=skip", "--skipGit", "--interactive=false")
	cmd.Dir = dir
	cmd.Env = withoutAIAgentEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create workspace: %v\n%s", err, out)
	}
	return filepath.Join(dir, workspaceName)
}

// withoutAIAgentEnv drops variables create-nx-workspace treats as "running under
// an AI agent". In that mode it swaps --preset for a GitHub template fetched at
// HEAD (a different Nx version and project layout), so local runs inside an
// agent would stop matching CI.
func withoutAIAgentEnv(environ []string) []string {
	agentVars := []string{
		"CLAUDECODE", "CLAUDE_CODE", "OPENCODE", "CODEX_THREAD_ID", "SUPERSET_AGENT_ID",
		"COPILOT_CLI", "REPL_ID", "CURSOR_TRACE_ID", "GEMINI_CLI",
	}
	kept := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(agentVars, name) {
			kept = append(kept, kv)
		}
	}
	return kept
}

// nxBuild clears local Nx state first so every build consults the remote cache.
func nxBuild(t *testing.T, workspace, serverURL, token string) string {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.CommandContext(t.Context(), "npx", args...) //nolint:gosec // fixed nx commands in a test workspace
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(),
			"NX_SELF_HOSTED_REMOTE_CACHE_SERVER="+serverURL,
			"NX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN="+token,
			"NX_DAEMON=false", "NX_NO_CLOUD=true", "NX_TUI=false",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("npx %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("nx", "reset")
	return run("nx", "build", project, "--outputStyle=static")
}

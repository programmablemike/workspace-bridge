package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/programmablemike/workspace-bridge/internal/mcpserver"
	"github.com/programmablemike/workspace-bridge/internal/testutil"
	"github.com/programmablemike/workspace-bridge/internal/workspace"
)

const daemonToken = "process-test-token-0123456789abcdef"

func TestDaemonHelper(t *testing.T) {
	if os.Getenv("WORKSPACE_BRIDGE_TEST_HELPER") != "1" {
		return
	}
	os.Args = []string{"workspace-bridge", "serve", "--config", os.Getenv("WORKSPACE_BRIDGE_TEST_CONFIG")}
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	os.Exit(0)
}

type bearerTransport struct{}

func (bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+daemonToken)
	return http.DefaultTransport.RoundTrip(r)
}
func TestDaemonProcessRestart(t *testing.T) {
	root := testutil.Repo(t)
	testutil.Write(t, root, "solution.go", "package solution\n// original\n")
	testutil.Git(t, root, "add", ".")
	testutil.Git(t, root, "commit", "-m", "initial")
	dir := t.TempDir()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	address := ln.Addr().String()
	ln.Close()
	cfg := filepath.Join(dir, "bridge.yaml")
	body := fmt.Sprintf("server:\n  listen: %q\nstorage:\n  path: %q\nobserver:\n  poll_interval: 40ms\nworkspaces:\n  - id: session\n    backend: git\n    root: %q\n", address, filepath.Join(dir, "state"), root)
	if e = os.WriteFile(cfg, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	start := func() (*exec.Cmd, func()) {
		t.Helper()
		log, e := os.CreateTemp(dir, "daemon-log-")
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command(executable, "-test.run=^TestDaemonHelper$")
		cmd.Env = append(os.Environ(), "WORKSPACE_BRIDGE_TEST_HELPER=1", "WORKSPACE_BRIDGE_TEST_CONFIG="+cfg, "WORKSPACE_BRIDGE_TOKEN="+daemonToken)
		cmd.Stdout = log
		cmd.Stderr = log
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cmd.Process.Signal(os.Interrupt)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case e := <-done:
				if e != nil {
					t.Errorf("daemon exit: %v", e)
				}
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				<-done
				t.Error("daemon did not stop")
			}
			log.Close()
			bytes, _ := os.ReadFile(log.Name())
			if strings.Contains(string(bytes), daemonToken) || strings.Contains(string(bytes), "// original") {
				t.Error("logs leaked content")
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			resp, e := http.Get("http://" + address + "/healthz")
			if e == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return cmd, stop
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("daemon not ready")
		return nil, nil
	}
	connect := func() *mcp.ClientSession {
		t.Helper()
		client := mcp.NewClient(&mcp.Implementation{Name: "process-acceptance", Version: "1"}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + address + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{}}}, nil)
		if e != nil {
			t.Fatal(e)
		}
		return session
	}
	query := func(client *mcp.ClientSession, name string, args any) json.RawMessage {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, e := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal(result.StructuredContent)
		var out struct {
			Data  json.RawMessage  `json:"data"`
			Error *workspace.Error `json:"error"`
		}
		if e = json.Unmarshal(b, &out); e != nil {
			t.Fatal(e)
		}
		if out.Error != nil {
			return nil
		}
		return out.Data
	}
	latest := func(client *mcp.ClientSession, previous string) string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var out mcpserver.SnapshotResult
			json.Unmarshal(query(client, "workspace_snapshot", map[string]any{"workspace_id": "session"}), &out)
			if out.Revision != "" && out.Revision != previous {
				return out.Revision
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("no snapshot")
		return ""
	}
	_, stop := start()
	client := connect()
	a := latest(client, "")
	var first workspace.EventPage
	json.Unmarshal(query(client, "workspace_changes", map[string]any{"workspace_id": "session"}), &first)
	testutil.Write(t, root, "solution.go", "package solution\n// modified\n")
	testutil.Write(t, root, "test.go", "package solution\n")
	expectedStatus := testutil.Git(t, root, "status", "--porcelain=v2", "-z")
	b := latest(client, a)
	// Wait until both independent saves are captured.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var d mcpserver.DiffResult
		json.Unmarshal(query(client, "workspace_diff", map[string]any{"workspace_id": "session", "from_revision": a, "to_revision": b}), &d)
		if len(d.Changes) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("missing tracked change or untracked addition")
		}
		b = latest(client, b)
	}
	if expectedStatus != testutil.Git(t, root, "status", "--porcelain=v2", "-z") {
		t.Fatal("observer changed git")
	}
	os.Remove(filepath.Join(root, "solution.go"))
	os.Remove(filepath.Join(root, "test.go"))
	client.Close()
	stop()
	_, stop = start()
	defer stop()
	client = connect()
	defer client.Close()
	for rev, expected := range map[string]string{a: "original", b: "modified"} {
		var read mcpserver.ReadResult
		json.Unmarshal(query(client, "workspace_read", map[string]any{"workspace_id": "session", "revision": rev, "path": "solution.go"}), &read)
		if !strings.Contains(read.Text, expected) {
			t.Fatal("historical bytes lost after process restart")
		}
	}
	var events workspace.EventPage
	json.Unmarshal(query(client, "workspace_changes", map[string]any{"workspace_id": "session", "after_cursor": first.Next}), &events)
	if len(events.Events) == 0 {
		t.Fatal("durable cursor lost")
	}
	if len(query(client, "workspace_list", map[string]any{})) == 0 {
		t.Fatal("list failed")
	}
}

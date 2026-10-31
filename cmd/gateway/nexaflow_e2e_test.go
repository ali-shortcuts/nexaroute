package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// TestNexaflowProductionGatewayWiringE2E boots the real production main()
// path with the nexaflow config section enabled and drives the canonical
// workflow API over HTTP. It is the regression test for the gap disclosed by
// PR #764: before this wiring, cmd/gateway had no AttachNexaflow caller, so
// every production /admin/api/nexaflow path fell through to the static
// server and answered 404. The test uses the production binary's own startup
// (config load, store open, service attach, startup recovery, real listener)
// rather than a test-only AttachNexaflow call.
func TestNexaflowProductionGatewayWiringE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a real listener and a real workflow store")
	}
	t.Setenv("NEXAROUTE_ADMIN_KEY", "")
	t.Setenv("NEXAROUTE_LISTEN", "")
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "false")

	dir := t.TempDir()
	port := findFreePort(t)
	storePath := filepath.Join(dir, "nexaflow-store.json")
	const adminKey = "nexaflow-e2e-admin"

	cfg := config.Default()
	cfg.Listen = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.Probe.Enabled = false
	cfg.Probe.OnStart = false
	cfg.Logging.File = "off"
	cfg.Logging.ConsoleMaxLinesPerMinute = 1000
	cfg.Admin.APIKey = adminKey
	cfg.Nexaflow = config.NexaflowConfig{
		Enabled:   true,
		StorePath: storePath,
		WorkerID:  "nexaflow-e2e",
	}
	configPath := writeTestConfig(t, cfg)

	logFile, err := os.Create(filepath.Join(dir, "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	oldArgs, oldStderr, oldNotify := os.Args, os.Stderr, notifyGatewayContext
	defer func() {
		os.Args, os.Stderr, notifyGatewayContext = oldArgs, oldStderr, oldNotify
		_ = logFile.Close()
	}()
	os.Args = []string{"nexaroute", "-config", configPath, "-no-browser"}
	os.Stderr = logFile
	cancelReady := make(chan context.CancelFunc, 1)
	notifyGatewayContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		cancelReady <- cancel
		return ctx, cancel
	}
	done := make(chan struct{})
	go func() {
		main()
		close(done)
	}()
	cancel := <-cancelReady
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Errorf("gateway did not stop after cancellation")
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitGatewayHealthy(t, base)

	client := &http.Client{Timeout: 10 * time.Second}
	call := func(t *testing.T, method, path string, body any, wantStatus int) map[string]any {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal %s: %v", path, err)
			}
			reader = bytes.NewReader(raw)
		}
		req, err := http.NewRequest(method, base+path, reader)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		req.Header.Set("x-admin-key", adminKey)
		if body != nil {
			req.Header.Set("content-type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, wantStatus, raw)
		}
		out := map[string]any{}
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("decode %s: %v (%s)", path, err, raw)
			}
		}
		return out
	}

	nodes := call(t, http.MethodGet, "/admin/api/nexaflow/nodes", nil, http.StatusOK)
	if !strings.Contains(fmt.Sprint(nodes["nodes"]), "nexaroute.control.json_transform") {
		t.Fatalf("nodes response lacks json_transform: %v", nodes)
	}

	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            "e2e-gateway-wiring",
		Name:          "e2e-gateway-wiring",
		Nodes: []nexaflow.DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
			{ID: "transform", NodeID: "nexaroute.control.json_transform", Version: nexaflow.SupportedVersion,
				Input: map[string]any{
					"value":      map[string]any{"hello": "world"},
					"operations": []any{map[string]any{"op": "set_const", "field": "done", "value": true}},
				}},
		},
		Edges:    []nexaflow.DefinitionEdge{{ID: "e1", From: "start", To: "transform"}},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
	}
	call(t, http.MethodPost, "/admin/api/nexaflow/definitions", def, http.StatusCreated)
	call(t, http.MethodPost, "/admin/api/nexaflow/definitions/"+def.ID+"/publish", nil, http.StatusOK)

	run := call(t, http.MethodPost, "/admin/api/nexaflow/definitions/"+def.ID+"/runs",
		map[string]any{"idempotency_key": "e2e-run-1", "input": map[string]any{}}, http.StatusCreated)
	runID, _ := run["id"].(string)
	if runID == "" {
		t.Fatalf("run response has no id: %v", run)
	}
	if state, _ := run["state"].(string); state != string(nexaflow.RunSucceeded) {
		t.Fatalf("run state = %q, want %q: %v", state, nexaflow.RunSucceeded, run)
	}

	detail := call(t, http.MethodGet, "/admin/api/nexaflow/runs/"+runID, nil, http.StatusOK)
	gotRun, _ := detail["run"].(map[string]any)
	if id, _ := gotRun["id"].(string); id != runID {
		t.Fatalf("run detail id = %q, want %q", id, runID)
	}
	if state, _ := gotRun["state"].(string); state != string(nexaflow.RunSucceeded) {
		t.Fatalf("run detail state = %q: %v", state, detail)
	}

	info, err := os.Stat(storePath)
	if err != nil || info.Size() == 0 {
		t.Fatalf("production workflow store not persisted at %s: info=%v err=%v", storePath, info, err)
	}
}

func waitGatewayHealthy(t *testing.T, base string) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gateway at %s never became healthy", base)
}

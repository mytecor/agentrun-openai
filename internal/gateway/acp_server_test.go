package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dmora/agentrun"
	"github.com/dmora/agentrun/engine/acp"
	acpengine "github.com/dmora/agentrun/engine/acp"
)

func init() {
	if os.Getenv("TEST_FAKE_ACP") == "1" {
		runFakeACP()
		os.Exit(0)
	}
}

func runFakeACP() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	enc := json.NewEncoder(os.Stdout)

	type rpcReq struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      *int64          `json:"id,omitempty"`
		Method  string          `json:"method,omitempty"`
		Params  json.RawMessage `json:"params,omitempty"`
	}

	for scanner.Scan() {
		var req rpcReq
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": 1,
					"agentCapabilities": map[string]any{
						"loadSession": true,
					},
					"agentInfo": map[string]string{
						"name":    "fake-acp",
						"version": "1.0.0",
					},
					"authMethods": []any{},
				},
			})
		case "session/new":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"sessionId": "fake-session-001",
					"models": map[string]any{
						"currentModelId": "fake-model-alpha",
						"availableModels": []map[string]string{
							{"modelId": "fake-model-alpha", "name": "Fake Model Alpha"},
							{"modelId": "fake-model-beta", "name": "Fake Model Beta"},
						},
					},
				},
			})
		case "session/load":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"models": map[string]any{
						"currentModelId": "fake-model-alpha",
						"availableModels": []map[string]string{
							{"modelId": "fake-model-alpha", "name": "Fake Model Alpha"},
							{"modelId": "fake-model-beta", "name": "Fake Model Beta"},
						},
					},
				},
			})
		case "session/set_config_option":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"configOptions": []any{},
				},
			})
		case "session/prompt":
			var params struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(req.Params, &params)
			sid := params.SessionID
			if sid == "" {
				sid = "fake-session-001"
			}
			// Emit streaming update notification
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": sid,
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content": map[string]string{
							"type": "text",
							"text": "response from fake acp",
						},
					},
				},
			})
			// Respond to prompt (turn done)
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"stopReason": "end_turn",
					"usage": map[string]int{
						"inputTokens":  10,
						"outputTokens": 5,
						"totalTokens":  15,
					},
				},
			})
		case "shutdown":
			os.Exit(0)
		}
	}
}

// Test 5: Generic ACP backend appears with base ID in /v1/models
func TestGenericACPBaseModelInModelsList(t *testing.T) {
	fake := &fakeEngine{listErr: errors.New("discovery failure")}
	server := New(Config{
		Engines:      map[string]agentrun.Engine{"pi": fake},
		BackendKinds: map[string]BackendKind{"pi": BackendGenericACP},
		ModelDetails: map[string]ModelDetails{"pi": {Name: "Pi Agent"}},
		DefaultCWD:   "/tmp",
		TurnTimeout:  time.Second,
		SessionTTL:   time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"pi"`) {
		t.Fatalf("models body does not contain base id pi: %s", body)
	}
	if !strings.Contains(body, `"name":"Pi Agent"`) {
		t.Fatalf("models body does not contain Pi Agent: %s", body)
	}
}

// Test 6: Discovered ACP models appear as <id>/<model>
func TestDiscoveredGenericACPModelsNamespace(t *testing.T) {
	fake := &fakeEngine{
		models: []agentrun.ModelInfo{
			{ID: "gpt-5.6-sol", Name: "GPT 5.6 Sol"},
			{ID: "claude-sonnet-4.6", Name: "Claude Sonnet 4.6"},
		},
	}
	server := New(Config{
		Engines:      map[string]agentrun.Engine{"pi": fake},
		BackendKinds: map[string]BackendKind{"pi": BackendGenericACP},
		ModelDetails: map[string]ModelDetails{"pi": {Name: "Pi"}},
		DefaultCWD:   "/tmp",
		TurnTimeout:  time.Second,
		SessionTTL:   time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"pi"`) {
		t.Errorf("missing base id pi: %s", body)
	}
	if !strings.Contains(body, `"id":"pi/gpt-5.6-sol"`) {
		t.Errorf("missing discovered model pi/gpt-5.6-sol: %s", body)
	}
	if !strings.Contains(body, `"id":"pi/claude-sonnet-4.6"`) {
		t.Errorf("missing discovered model pi/claude-sonnet-4.6: %s", body)
	}
}

// Test 7: Generic ACP model ID does not undergo Codex effort grouping
func TestGenericACPNoCodexEffortGrouping(t *testing.T) {
	fake := &fakeEngine{
		models: []agentrun.ModelInfo{
			{ID: "model-a[high]", Name: "Model A (high)"},
			{ID: "model-a[medium]", Name: "Model A (medium)"},
		},
	}
	server := New(Config{
		Engines:      map[string]agentrun.Engine{"custom-acp": fake},
		BackendKinds: map[string]BackendKind{"custom-acp": BackendGenericACP},
		ModelDetails: map[string]ModelDetails{"custom-acp": {Name: "Custom ACP"}},
		DefaultCWD:   "/tmp",
		TurnTimeout:  time.Second,
		SessionTTL:   time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	body := rec.Body.String()
	// Must contain the literal model IDs with bracket suffixes, NOT collapsed
	if !strings.Contains(body, `"id":"custom-acp/model-a[high]"`) {
		t.Errorf("expected literal id custom-acp/model-a[high], got %s", body)
	}
	if !strings.Contains(body, `"id":"custom-acp/model-a[medium]"`) {
		t.Errorf("expected literal id custom-acp/model-a[medium], got %s", body)
	}
	// And must NOT have reasoning_efforts attached
	var resp struct {
		Data []struct {
			ID               string   `json:"id"`
			ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, m := range resp.Data {
		if strings.HasPrefix(m.ID, "custom-acp/") && len(m.ReasoningEfforts) > 0 {
			t.Errorf("model %q unexpectedly has reasoning_efforts: %v", m.ID, m.ReasoningEfforts)
		}
	}
}

// Test 8: Request to <id>/<model> passes model to agentrun.Session.Model
func TestGenericACPRequestWithSubModel(t *testing.T) {
	fake := &fakeEngine{
		models: []agentrun.ModelInfo{
			{ID: "gpt-5.6-sol", Name: "GPT 5.6"},
		},
	}
	server := New(Config{
		Engines:      map[string]agentrun.Engine{"pi": fake},
		BackendKinds: map[string]BackendKind{"pi": BackendGenericACP},
		DefaultCWD:   "/tmp",
		TurnTimeout:  time.Second,
		SessionTTL:   time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	resp := doChat(t, server, `{"model":"pi/gpt-5.6-sol","messages":[{"role":"user","content":"hello"}]}`, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sessions) != 1 {
		t.Fatalf("len(fake.sessions) = %d, want 1", len(fake.sessions))
	}
	if fake.sessions[0].Model != "gpt-5.6-sol" {
		t.Errorf("session.Model = %q, want gpt-5.6-sol", fake.sessions[0].Model)
	}
}

// Test 9: Request to <id> leaves backend model empty/default
func TestGenericACPRequestWithBaseModel(t *testing.T) {
	fake := &fakeEngine{
		models: []agentrun.ModelInfo{
			{ID: "gpt-5.6-sol", Name: "GPT 5.6"},
		},
	}
	server := New(Config{
		Engines:      map[string]agentrun.Engine{"pi": fake},
		BackendKinds: map[string]BackendKind{"pi": BackendGenericACP},
		DefaultCWD:   "/tmp",
		TurnTimeout:  time.Second,
		SessionTTL:   time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	resp := doChat(t, server, `{"model":"pi","messages":[{"role":"user","content":"hello"}]}`, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sessions) != 1 {
		t.Fatalf("len(fake.sessions) = %d, want 1", len(fake.sessions))
	}
	if fake.sessions[0].Model != "" {
		t.Errorf("session.Model = %q, want empty (default)", fake.sessions[0].Model)
	}
}

// Test 10: Existing Codex effort behaviour does not regress
func TestExistingCodexEffortBehaviorNotRegressed(t *testing.T) {
	fake := &fakeEngine{
		models: []agentrun.ModelInfo{
			{ID: "gpt-test[medium]", Name: "GPT Test (medium)"},
			{ID: "gpt-test[high]", Name: "GPT Test (high)"},
		},
	}
	server := New(Config{
		Engines:      map[string]agentrun.Engine{"codex": fake},
		BackendKinds: map[string]BackendKind{"codex": BackendCodexACP},
		ModelDetails: map[string]ModelDetails{"codex": {Name: "Codex", ContextWindow: 200000, MaxTokens: 32000}},
		DefaultCWD:   "/tmp",
		TurnTimeout:  time.Second,
		SessionTTL:   time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	// Verify /v1/models collapses effort models
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"codex/gpt-test"`) || !strings.Contains(body, `"reasoning_efforts"`) {
		t.Fatalf("codex models catalog unexpected: %s", body)
	}

	// Verify chat completion with reasoning_effort maps to acp reasoning_effort option
	resp := doChat(t, server, `{"model":"codex/gpt-test","reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sessions) != 1 {
		t.Fatalf("sessions count = %d, want 1", len(fake.sessions))
	}
	if got := fake.sessions[0].Options[acpengine.SessionConfigOption("reasoning_effort")]; got != "high" {
		t.Errorf("codex reasoning_effort option = %q, want high", got)
	}
	if fake.sessions[0].Model != "gpt-test" {
		t.Errorf("codex model = %q, want gpt-test", fake.sessions[0].Model)
	}
}

// Test 11: Session affinity and idle eviction work for generic ACP with the same lifecycle code
func TestGenericACPSessionAffinityAndIdleEviction(t *testing.T) {
	fake := &fakeEngine{}
	server := New(Config{
		Engines:      map[string]agentrun.Engine{"pi": fake},
		BackendKinds: map[string]BackendKind{"pi": BackendGenericACP},
		DefaultCWD:   "/tmp",
		TurnTimeout:  time.Second,
		SessionTTL:   time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	// First turn with affinity
	headers := map[string]string{"X-Session-Affinity": "pi-user"}
	resp1 := doChat(t, server, `{"model":"pi","messages":[{"role":"user","content":"turn 1"}]}`, headers)
	if resp1.Code != http.StatusOK {
		t.Fatalf("first turn status = %d, body = %s", resp1.Code, resp1.Body.String())
	}

	fake.mu.Lock()
	if len(fake.procs) != 1 {
		fake.mu.Unlock()
		t.Fatalf("procs = %d, want 1", len(fake.procs))
	}
	proc := fake.procs[0]
	fake.mu.Unlock()

	// Evict idle
	state := server.registry.lock("pi-user:pi")
	state.lastAccess = time.Now().Add(-2 * time.Hour)
	state.mu.Unlock()
	server.registry.evictIdle()

	proc.mu.Lock()
	stopped := proc.stopped
	proc.mu.Unlock()
	if !stopped {
		t.Fatal("process was not stopped upon eviction")
	}

	// Continuation turn
	payload, _ := json.Marshal(map[string]any{
		"model": "pi",
		"messages": []map[string]string{
			{"role": "user", "content": "turn 1"},
			{"role": "assistant", "content": "answer: turn 1"},
			{"role": "user", "content": "turn 2"},
		},
	})
	resp2 := doChat(t, server, string(payload), headers)
	if resp2.Code != http.StatusOK {
		t.Fatalf("second turn status = %d, body = %s", resp2.Code, resp2.Body.String())
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sessions) != 2 {
		t.Fatalf("sessions count = %d, want 2", len(fake.sessions))
	}
	if got := fake.sessions[1].Options[agentrun.OptionResumeID]; got != "resume-1" {
		t.Errorf("resumed with id %q, want resume-1", got)
	}
}

// Test 12: After eviction ResumeID is preserved and new process gets it upon continuation
func TestGenericACPEvictionResumesWithResumeID(t *testing.T) {
	storePath := fmt.Sprintf("%s/sessions.json", t.TempDir())
	fake := &fakeEngine{}
	server := New(Config{
		Engines:      map[string]agentrun.Engine{"pi": fake},
		BackendKinds: map[string]BackendKind{"pi": BackendGenericACP},
		DefaultCWD:   "/tmp",
		TurnTimeout:  time.Second,
		SessionTTL:   time.Hour,
		SessionStore: storePath,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	headers := map[string]string{"X-Session-Affinity": "resume-test"}
	resp1 := doChat(t, server, `{"model":"pi","messages":[{"role":"user","content":"hello"}]}`, headers)
	if resp1.Code != http.StatusOK {
		t.Fatalf("turn 1 failed: %s", resp1.Body.String())
	}

	// Trigger eviction
	state := server.registry.lock("resume-test:pi")
	state.lastAccess = time.Now().Add(-2 * time.Hour)
	savedResumeID := state.resumeID
	state.mu.Unlock()
	if savedResumeID == "" {
		t.Fatal("expected non-empty resumeID before eviction")
	}

	server.registry.evictIdle()

	// Verify state still has resumeID after process stopped
	state = server.registry.lock("resume-test:pi")
	if state.resumeID != savedResumeID {
		t.Fatalf("resumeID after eviction = %q, want %q", state.resumeID, savedResumeID)
	}
	if state.process != nil {
		t.Fatal("process was not cleared on eviction")
	}
	state.mu.Unlock()

	// Continuation request receives saved resumeID
	payload, _ := json.Marshal(map[string]any{
		"model": "pi",
		"messages": []map[string]string{
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "answer: hello"},
			{"role": "user", "content": "next"},
		},
	})
	resp2 := doChat(t, server, string(payload), headers)
	if resp2.Code != http.StatusOK {
		t.Fatalf("turn 2 failed: %s", resp2.Body.String())
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sessions) != 2 {
		t.Fatalf("sessions count = %d, want 2", len(fake.sessions))
	}
	if got := fake.sessions[1].Options[agentrun.OptionResumeID]; got != savedResumeID {
		t.Errorf("resumed with id %q, want %q", got, savedResumeID)
	}
}

// Test 13: Failed discovery does not erase last-known model catalog
func TestGenericACPDiscoveryRetainsCatalogOnFailure(t *testing.T) {
	fake := &fakeEngine{
		models: []agentrun.ModelInfo{
			{ID: "gpt-5.6-sol", Name: "GPT 5.6"},
		},
	}
	server := New(Config{
		Engines:      map[string]agentrun.Engine{"pi": fake},
		BackendKinds: map[string]BackendKind{"pi": BackendGenericACP},
		DefaultCWD:   "/tmp",
		TurnTimeout:  time.Second,
		SessionTTL:   time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	// First call succeeds
	rec1 := httptest.NewRecorder()
	server.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(rec1.Body.String(), `"id":"pi/gpt-5.6-sol"`) {
		t.Fatalf("first discovery missing model: %s", rec1.Body.String())
	}

	// Second call fails discovery
	fake.mu.Lock()
	fake.listErr = errors.New("network timeout")
	fake.mu.Unlock()

	rec2 := httptest.NewRecorder()
	server.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(rec2.Body.String(), `"id":"pi/gpt-5.6-sol"`) {
		t.Fatalf("cached model lost after failed discovery: %s", rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), `"id":"pi"`) {
		t.Fatalf("base model lost after failed discovery: %s", rec2.Body.String())
	}
}

// Test 15: Integration test with real stdio ACP binary (self-subprocess)
func TestGenericACPIntegrationStdio(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot find test executable: %v", err)
	}

	t.Setenv("TEST_FAKE_ACP", "1")

	acpEngine := acp.NewEngine(
		acp.WithBinary(exe),
		acp.WithStderrWriter(io.Discard),
	)

	server := New(Config{
		Engines:      map[string]agentrun.Engine{"stdio-acp": acpEngine},
		BackendKinds: map[string]BackendKind{"stdio-acp": BackendGenericACP},
		DefaultCWD:   t.TempDir(),
		TurnTimeout:  10 * time.Second,
		SessionTTL:   time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	// 1. Model discovery over real ACP stdio
	modelsRec := httptest.NewRecorder()
	server.ServeHTTP(modelsRec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if modelsRec.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", modelsRec.Code, modelsRec.Body.String())
	}
	body := modelsRec.Body.String()
	if !strings.Contains(body, `"id":"stdio-acp/fake-model-alpha"`) {
		t.Errorf("missing discovered alpha model: %s", body)
	}
	if !strings.Contains(body, `"id":"stdio-acp/fake-model-beta"`) {
		t.Errorf("missing discovered beta model: %s", body)
	}

	// 2. Chat completion over real ACP stdio
	chatResp := doChat(t, server, `{"model":"stdio-acp/fake-model-alpha","messages":[{"role":"user","content":"hello"}]}`, nil)
	if chatResp.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", chatResp.Code, chatResp.Body.String())
	}
	chatBody := chatResp.Body.String()
	if !strings.Contains(chatBody, "response from fake acp") {
		t.Errorf("chat response does not contain fake acp text: %s", chatBody)
	}
}

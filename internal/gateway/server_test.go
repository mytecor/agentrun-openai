package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmora/agentrun"
	acpengine "github.com/dmora/agentrun/engine/acp"
)

type fakeEngine struct {
	mu              sync.Mutex
	sessions        []agentrun.Session
	procs           []*fakeProcess
	failResumeStart bool
	failResumeTurn  bool
	models          []agentrun.ModelInfo
	listErr         error
	effectiveModel  string
	thinking        bool
}

func (e *fakeEngine) Validate() error { return nil }

func (e *fakeEngine) ListModels(_ context.Context, session agentrun.Session) ([]agentrun.ModelInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.listErr != nil {
		return nil, e.listErr
	}
	return agentrun.CloneModelCatalog(e.models), nil
}

func (e *fakeEngine) Start(_ context.Context, session agentrun.Session, _ ...agentrun.Option) (agentrun.Process, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	resumeID := session.Options[agentrun.OptionResumeID]
	e.sessions = append(e.sessions, session)
	if resumeID != "" && e.failResumeStart {
		e.failResumeStart = false
		return nil, fmt.Errorf("%w: expired", agentrun.ErrSessionNotFound)
	}
	resumed := resumeID != ""
	if resumeID == "" {
		resumeID = fmt.Sprintf("resume-%d", len(e.procs)+1)
	}
	p := &fakeProcess{
		output: make(chan agentrun.Message, 16), resumeID: resumeID, replayOnStart: resumed,
		missingOnStart: resumed && e.failResumeTurn, model: session.Model, thinking: e.thinking,
	}
	if e.effectiveModel != "" {
		p.model = e.effectiveModel
	}
	e.procs = append(e.procs, p)
	return p, nil
}

type fakeProcess struct {
	mu             sync.Mutex
	output         chan agentrun.Message
	prompts        []string
	stopped        bool
	resumeID       string
	replayOnStart  bool
	missingOnStart bool
	model          string
	thinking       bool
}

func (p *fakeProcess) Output() <-chan agentrun.Message { return p.output }

func (p *fakeProcess) Send(_ context.Context, prompt string) error {
	p.mu.Lock()
	p.prompts = append(p.prompts, prompt)
	p.mu.Unlock()
	if p.missingOnStart {
		p.missingOnStart = false
		p.output <- agentrun.Message{Type: agentrun.MessageError, Content: "No conversation found with that session ID"}
		return nil
	}
	if p.replayOnStart {
		p.output <- agentrun.Message{Type: agentrun.MessageText, Content: "replayed old answer"}
		p.replayOnStart = false
	}
	init := &agentrun.InitMeta{}
	if p.model == "" {
		init = nil
	} else {
		init.Model = p.model
	}
	p.output <- agentrun.Message{Type: agentrun.MessageInit, ResumeID: p.resumeID, Init: init}
	if p.thinking {
		p.output <- agentrun.Message{Type: agentrun.MessageThinkingDelta, Content: "let me "}
		p.output <- agentrun.Message{Type: agentrun.MessageThinkingDelta, Content: "consider"}
		p.output <- agentrun.Message{Type: agentrun.MessageThinking, Content: "let me consider"}
	}
	p.output <- agentrun.Message{Type: agentrun.MessageTextDelta, Content: "answer: "}
	p.output <- agentrun.Message{Type: agentrun.MessageTextDelta, Content: prompt}
	p.output <- agentrun.Message{Type: agentrun.MessageText, Content: "answer: " + prompt}
	p.output <- agentrun.Message{Type: agentrun.MessageResult, Usage: &agentrun.Usage{InputTokens: 3, OutputTokens: 4}}
	return nil
}

func (p *fakeProcess) Stop(context.Context) error {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	return nil
}
func (p *fakeProcess) Wait() error { return nil }
func (p *fakeProcess) Err() error  { return nil }

func newTestServer(engine *fakeEngine) *Server {
	return newTestServerWithStore(engine, "")
}

func newTestServerWithStore(engine *fakeEngine, storePath string) *Server {
	return New(Config{
		Engines:      map[string]agentrun.Engine{"test-agent": engine},
		DefaultCWD:   "/tmp",
		TurnTimeout:  time.Second,
		SessionTTL:   time.Hour,
		SessionStore: storePath,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func continuationPayload(t *testing.T, user, answer, next string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"model": "test-agent",
		"messages": []map[string]string{
			{"role": "user", "content": user},
			{"role": "assistant", "content": answer},
			{"role": "user", "content": next},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func doChat(t *testing.T, server *Server, payload string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	return recorder
}

func TestIdleEvictionResumesNativeSession(t *testing.T) {
	engine := &fakeEngine{}
	server := newTestServer(engine)
	defer server.Close()

	first := doChat(t, server, `{"model":"test-agent","messages":[{"role":"user","content":"first"}]}`, map[string]string{"X-Session-Affinity": "idle"})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	state := server.registry.lock("idle:test-agent")
	state.lastAccess = time.Now().Add(-2 * time.Hour)
	state.mu.Unlock()
	server.registry.evictIdle()

	second := doChat(t, server, continuationPayload(t, "first", "answer: first", "second"), map[string]string{"X-Session-Affinity": "idle"})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(engine.sessions))
	}
	if got := engine.sessions[1].Options[agentrun.OptionResumeID]; got != "resume-1" {
		t.Fatalf("resume id = %q, want resume-1", got)
	}
	if got := engine.procs[1].prompts; len(got) != 1 || got[0] != "second" {
		t.Fatalf("resumed prompts = %#v, want [second]", got)
	}
	if strings.Contains(second.Body.String(), "replayed old answer") {
		t.Fatalf("response contains replayed output: %s", second.Body.String())
	}
}

func TestSessionStoreResumesAfterServerRestartWithoutTranscriptText(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	firstEngine := &fakeEngine{}
	firstServer := newTestServerWithStore(firstEngine, storePath)
	first := doChat(t, firstServer, `{"model":"test-agent","messages":[{"role":"user","content":"private prompt"}]}`, map[string]string{"X-Session-Affinity": "restart"})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	firstServer.Close()

	stored, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "private prompt") || strings.Contains(string(stored), "answer: private prompt") {
		t.Fatalf("session store contains transcript text: %s", stored)
	}

	secondEngine := &fakeEngine{}
	secondServer := newTestServerWithStore(secondEngine, storePath)
	defer secondServer.Close()
	second := doChat(t, secondServer, continuationPayload(t, "private prompt", "answer: private prompt", "continue"), map[string]string{"X-Session-Affinity": "restart"})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	secondEngine.mu.Lock()
	defer secondEngine.mu.Unlock()
	if len(secondEngine.sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(secondEngine.sessions))
	}
	if got := secondEngine.sessions[0].Options[agentrun.OptionResumeID]; got != "resume-1" {
		t.Fatalf("resume id = %q, want resume-1", got)
	}
}

func TestMissingNativeSessionFallsBackToFreshConversation(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	firstServer := newTestServerWithStore(&fakeEngine{}, storePath)
	first := doChat(t, firstServer, `{"model":"test-agent","messages":[{"role":"user","content":"first"}]}`, map[string]string{"X-Session-Affinity": "expired"})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	firstServer.Close()

	engine := &fakeEngine{failResumeStart: true}
	server := newTestServerWithStore(engine, storePath)
	defer server.Close()
	second := doChat(t, server, continuationPayload(t, "first", "answer: first", "second"), map[string]string{"X-Session-Affinity": "expired"})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.sessions) != 2 {
		t.Fatalf("start attempts = %d, want 2", len(engine.sessions))
	}
	if got := engine.sessions[0].Options[agentrun.OptionResumeID]; got != "resume-1" {
		t.Fatalf("first attempt resume id = %q, want resume-1", got)
	}
	if got := engine.sessions[1].Options[agentrun.OptionResumeID]; got != "" {
		t.Fatalf("fallback resume id = %q, want empty", got)
	}
	if prompt := engine.sessions[1].Prompt; !strings.Contains(prompt, "first") || !strings.Contains(prompt, "second") {
		t.Fatalf("fallback prompt does not contain the full conversation: %q", prompt)
	}
}

func TestMissingNativeSessionReportedByTurnFallsBackToFreshConversation(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	firstServer := newTestServerWithStore(&fakeEngine{}, storePath)
	first := doChat(t, firstServer, `{"model":"test-agent","messages":[{"role":"user","content":"first"}]}`, map[string]string{"X-Session-Affinity": "expired-turn"})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	firstServer.Close()

	engine := &fakeEngine{failResumeTurn: true}
	server := newTestServerWithStore(engine, storePath)
	defer server.Close()
	second := doChat(t, server, continuationPayload(t, "first", "answer: first", "second"), map[string]string{"X-Session-Affinity": "expired-turn"})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.sessions) != 2 {
		t.Fatalf("start attempts = %d, want 2", len(engine.sessions))
	}
	if got := engine.sessions[1].Options[agentrun.OptionResumeID]; got != "" {
		t.Fatalf("fallback resume id = %q, want empty", got)
	}
	if prompt := engine.sessions[1].Prompt; !strings.Contains(prompt, "first") || !strings.Contains(prompt, "second") {
		t.Fatalf("fallback prompt does not contain the full conversation: %q", prompt)
	}
}

func TestLinearConversationReusesProcessAndSendsOnlyNewTurn(t *testing.T) {
	engine := &fakeEngine{}
	server := newTestServer(engine)
	defer server.Close()

	first := doChat(t, server, `{"model":"test-agent","messages":[{"role":"user","content":"inspect auth"}],"temperature":0}`, map[string]string{"X-Session-Affinity": "pi-1"})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	var firstResponse struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatal(err)
	}
	answer := firstResponse.Choices[0].Message.Content
	payload, _ := json.Marshal(map[string]any{
		"model": "test-agent",
		"messages": []map[string]string{
			{"role": "user", "content": "inspect auth"},
			{"role": "assistant", "content": answer},
			{"role": "user", "content": "now fix it"},
		},
	})
	second := doChat(t, server, string(payload), map[string]string{"X-Session-Affinity": "pi-1"})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.procs) != 1 {
		t.Fatalf("process count = %d, want 1", len(engine.procs))
	}
	got := engine.procs[0].prompts
	want := []string{"inspect auth", "now fix it"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("prompts = %#v, want %#v", got, want)
	}
}

func TestDivergedHistoryReplacesProcess(t *testing.T) {
	engine := &fakeEngine{}
	server := newTestServer(engine)
	defer server.Close()

	first := doChat(t, server, `{"model":"test-agent","messages":[{"role":"user","content":"A"}]}`, map[string]string{"X-Session-Affinity": "branch"})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	second := doChat(t, server, `{"model":"test-agent","messages":[{"role":"user","content":"different branch"}]}`, map[string]string{"X-Session-Affinity": "branch"})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.procs) != 2 {
		t.Fatalf("process count = %d, want 2", len(engine.procs))
	}
	if !engine.procs[0].stopped {
		t.Fatal("old process was not stopped")
	}
}

func TestStreamingChatCompletion(t *testing.T) {
	engine := &fakeEngine{}
	server := newTestServer(engine)
	defer server.Close()

	response := doChat(t, server, `{"model":"test-agent","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`, map[string]string{"session_id": "stream-1"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}
	body := response.Body.String()
	for _, expected := range []string{`"role":"assistant"`, `"content":"answer: "`, `"content":"hello"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, expected) {
			t.Errorf("stream does not contain %q:\n%s", expected, body)
		}
	}
}

func TestValidationAndAuthentication(t *testing.T) {
	engine := &fakeEngine{}
	server := newTestServer(engine)
	server.config.APIKey = "secret"
	defer server.Close()

	unauthorized := doChat(t, server, `{}`, nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	bad := doChat(t, server, `{"model":"test-agent","messages":[{"role":"assistant","content":"not a user turn"}]}`, map[string]string{"Authorization": "Bearer secret"})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d, body = %s", bad.Code, bad.Body.String())
	}
}

func TestModelsAreOpenAIShaped(t *testing.T) {
	server := newTestServer(&fakeEngine{})
	server.config.ModelDetails = map[string]ModelDetails{
		"test-agent": {Name: "Test Agent", ContextWindow: 1234, MaxTokens: 567},
	}
	defer server.Close()
	for _, path := range []string{"/v1/models", "/models"} {
		req := httptest.NewRequest(http.MethodGet, path, bytes.NewReader(nil))
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		body := recorder.Body.String()
		if recorder.Code != http.StatusOK || !strings.Contains(body, `"object":"list"`) ||
			!strings.Contains(body, `"name":"Test Agent"`) || !strings.Contains(body, `"context_window":1234`) ||
			!strings.Contains(body, `"max_tokens":567`) {
			t.Fatalf("path = %s, status = %d, body = %s", path, recorder.Code, body)
		}
	}
}

func TestDiscoveredModelRoutesBackendModelAndKeepsEngineAlias(t *testing.T) {
	engine := &fakeEngine{models: []agentrun.ModelInfo{
		{ID: "gpt-test[low]", Name: "GPT Test (low)"},
		{ID: "gpt-test[medium]", Name: "GPT Test (medium)"},
		{ID: "gpt-test[high]", Name: "GPT Test (high)"},
	}}
	server := New(Config{
		Engines: map[string]agentrun.Engine{"codex": engine},
		ModelDetails: map[string]ModelDetails{
			"codex": {Name: "Codex", ContextWindow: 200000, MaxTokens: 32000},
		},
		DefaultCWD: "/tmp", TurnTimeout: time.Second, SessionTTL: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsResponse := httptest.NewRecorder()
	server.ServeHTTP(modelsResponse, modelsRequest)
	if body := modelsResponse.Body.String(); !strings.Contains(body, `"id":"codex/gpt-test"`) ||
		!strings.Contains(body, `"id":"codex"`) || !strings.Contains(body, `"reasoning_efforts"`) ||
		strings.Contains(body, `"id":"codex/gpt-test[high]"`) {
		t.Fatalf("models body = %s", body)
	}

	response := doChat(t, server, `{"model":"codex/gpt-test","messages":[{"role":"user","content":"hello"}]}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.sessions) != 1 || engine.sessions[0].Model != "gpt-test" ||
		engine.sessions[0].Options[acpengine.SessionConfigOption("reasoning_effort")] != "medium" {
		t.Fatalf("sessions = %#v, want backend model gpt-test with medium effort", engine.sessions)
	}
}

func TestReasoningEffortSelectsCodexVariantAndSeparatesAffinity(t *testing.T) {
	engine := &fakeEngine{models: []agentrun.ModelInfo{
		{ID: "gpt-test[medium]", Name: "GPT Test (medium)"},
		{ID: "gpt-test[high]", Name: "GPT Test (high)"},
	}}
	server := New(Config{
		Engines: map[string]agentrun.Engine{"codex": engine}, DefaultCWD: "/tmp",
		TurnTimeout: time.Second, SessionTTL: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	headers := map[string]string{"X-Session-Affinity": "same"}
	high := doChat(t, server, `{"model":"codex/gpt-test","reasoning_effort":"high","messages":[{"role":"user","content":"high"}]}`, headers)
	medium := doChat(t, server, `{"model":"codex/gpt-test","reasoning_effort":"medium","messages":[{"role":"user","content":"medium"}]}`, headers)
	if high.Code != http.StatusOK || medium.Code != http.StatusOK {
		t.Fatalf("high = %d %s; medium = %d %s", high.Code, high.Body.String(), medium.Code, medium.Body.String())
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.sessions) != 2 || engine.sessions[0].Model != "gpt-test" || engine.sessions[1].Model != "gpt-test" ||
		engine.sessions[0].Options[acpengine.SessionConfigOption("reasoning_effort")] != "high" ||
		engine.sessions[1].Options[acpengine.SessionConfigOption("reasoning_effort")] != "medium" {
		t.Fatalf("sessions = %#v", engine.sessions)
	}
}

func TestUnsupportedReasoningEffortRejected(t *testing.T) {
	engine := &fakeEngine{models: []agentrun.ModelInfo{{ID: "gpt-test[medium]", Name: "GPT Test (medium)"}}}
	server := New(Config{
		Engines: map[string]agentrun.Engine{"codex": engine}, DefaultCWD: "/tmp",
		TurnTimeout: time.Second, SessionTTL: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()
	server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	response := doChat(t, server, `{"model":"codex/gpt-test","reasoning_effort":"ultra","messages":[{"role":"user","content":"hello"}]}`, nil)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"reasoning_effort_not_supported"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestDiscoveryIncludesClaudeAndRetainsLastCatalogOnFailure(t *testing.T) {
	engine := &fakeEngine{models: []agentrun.ModelInfo{
		{ID: "default", Name: "Default", Aliases: []string{"claude-sonnet-5"}},
		{ID: "sonnet", Name: "Sonnet", Aliases: []string{"claude-sonnet-5"}},
	}}
	server := New(Config{
		Engines:      map[string]agentrun.Engine{"claude-code": engine},
		ModelDetails: map[string]ModelDetails{"claude-code": {Name: "Claude Code", ContextWindow: 200000, MaxTokens: 32000}},
		DefaultCWD:   "/tmp", TurnTimeout: time.Second, SessionTTL: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	first := httptest.NewRecorder()
	server.ServeHTTP(first, request)
	if !strings.Contains(first.Body.String(), `"id":"claude-code/sonnet"`) ||
		strings.Contains(first.Body.String(), `"id":"claude-code/default"`) ||
		!strings.Contains(first.Body.String(), `"thinking_level_map"`) {
		t.Fatalf("first models body = %s", first.Body.String())
	}
	chat := doChat(t, server, `{"model":"claude-code/sonnet","reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`, nil)
	if chat.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", chat.Code, chat.Body.String())
	}
	engine.mu.Lock()
	if got := engine.sessions[0].Options[agentrun.OptionEffort]; got != "high" {
		engine.mu.Unlock()
		t.Fatalf("Claude effort = %q, want high", got)
	}
	engine.mu.Unlock()

	engine.mu.Lock()
	engine.listErr = errors.New("temporary discovery failure")
	engine.mu.Unlock()
	second := httptest.NewRecorder()
	server.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(second.Body.String(), `"id":"claude-code/sonnet"`) {
		t.Fatalf("cached models body = %s", second.Body.String())
	}
}

func TestDiscoveredRouteRejectsUnexpectedEffectiveModel(t *testing.T) {
	engine := &fakeEngine{
		models:         []agentrun.ModelInfo{{ID: "gpt-test[medium]"}},
		effectiveModel: "gpt-other",
	}
	server := New(Config{
		Engines: map[string]agentrun.Engine{"codex": engine}, DefaultCWD: "/tmp",
		TurnTimeout: time.Second, SessionTTL: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()
	server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	response := doChat(t, server, `{"model":"codex/gpt-test","messages":[{"role":"user","content":"hello"}]}`, nil)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "backend selected model") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestAllowedRootsPermitChildrenAndRejectOutside(t *testing.T) {
	allowed := t.TempDir()
	child := filepath.Join(allowed, "project")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(&fakeEngine{})
	server.config.AllowedRoots = []string{allowed}
	defer server.Close()

	inside := doChat(t, server, `{"model":"test-agent","messages":[{"role":"user","content":"inside"}]}`, map[string]string{"X-Agent-CWD": child})
	if inside.Code != http.StatusOK {
		t.Fatalf("inside status = %d, body = %s", inside.Code, inside.Body.String())
	}
	outside := doChat(t, server, `{"model":"test-agent","messages":[{"role":"user","content":"outside"}]}`, map[string]string{"X-Agent-CWD": t.TempDir()})
	if outside.Code != http.StatusForbidden || !strings.Contains(outside.Body.String(), `"code":"cwd_not_allowed"`) {
		t.Fatalf("outside status = %d, body = %s", outside.Code, outside.Body.String())
	}
}

func TestAllowedRootsRejectSymlinkEscape(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	server := newTestServer(&fakeEngine{})
	server.config.AllowedRoots = []string{allowed}
	defer server.Close()

	response := doChat(t, server, `{"model":"test-agent","messages":[{"role":"user","content":"escape"}]}`, map[string]string{"X-Agent-CWD": link})
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

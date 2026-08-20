package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmora/agentrun"
)

type fakeEngine struct {
	mu       sync.Mutex
	sessions []agentrun.Session
	procs    []*fakeProcess
}

func (e *fakeEngine) Validate() error { return nil }

func (e *fakeEngine) Start(_ context.Context, session agentrun.Session, _ ...agentrun.Option) (agentrun.Process, error) {
	p := &fakeProcess{output: make(chan agentrun.Message, 16)}
	e.mu.Lock()
	e.sessions = append(e.sessions, session)
	e.procs = append(e.procs, p)
	e.mu.Unlock()
	return p, nil
}

type fakeProcess struct {
	mu      sync.Mutex
	output  chan agentrun.Message
	prompts []string
	stopped bool
}

func (p *fakeProcess) Output() <-chan agentrun.Message { return p.output }

func (p *fakeProcess) Send(_ context.Context, prompt string) error {
	p.mu.Lock()
	p.prompts = append(p.prompts, prompt)
	p.mu.Unlock()
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
	return New(Config{
		Engines:     map[string]agentrun.Engine{"test-agent": engine},
		DefaultCWD:  "/tmp",
		TurnTimeout: time.Second,
		SessionTTL:  time.Hour,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
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
	defer server.Close()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", bytes.NewReader(nil))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"object":"list"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

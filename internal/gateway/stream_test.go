package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamDeltas returns the delta object of every chat.completion.chunk in an
// SSE body, in order.
func streamDeltas(t *testing.T, body string) []map[string]any {
	t.Helper()
	var deltas []map[string]any
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			deltas = append(deltas, choice.Delta)
		}
	}
	return deltas
}

func TestStreamEmitsThinkingAsReasoningContent(t *testing.T) {
	server := newTestServer(&fakeEngine{thinking: true})
	defer server.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"test-agent","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var reasoning, content strings.Builder
	for _, delta := range streamDeltas(t, recorder.Body.String()) {
		if value, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(value)
		}
		if value, ok := delta["content"].(string); ok {
			content.WriteString(value)
		}
	}
	if reasoning.String() != "let me consider" {
		t.Errorf("reasoning_content = %q, want %q", reasoning.String(), "let me consider")
	}
	if !strings.Contains(content.String(), "answer: ") {
		t.Errorf("content = %q, want it to carry the answer", content.String())
	}
	if strings.Contains(content.String(), "consider") {
		t.Errorf("thinking leaked into content: %q", content.String())
	}
}

func TestNonStreamKeepsThinkingOutOfContent(t *testing.T) {
	server := newTestServer(&fakeEngine{thinking: true})
	defer server.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`))
	server.ServeHTTP(recorder, request)

	var response struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	message := response.Choices[0].Message
	if message.ReasoningContent != "let me consider" {
		t.Errorf("reasoning_content = %q", message.ReasoningContent)
	}
	if strings.Contains(message.Content, "consider") {
		t.Errorf("thinking leaked into content: %q", message.Content)
	}
}

// A coding agent runs its tools inside the backend, so a turn can stay silent
// for minutes. Without a heartbeat, clients with stall detectors abort it.
func TestHeartbeatEmitsEmptyDeltasWhileSilent(t *testing.T) {
	recorder := httptest.NewRecorder()
	collector := newCollector(recorder, true, "chatcmpl-test", 0, "test-agent", false, nil)
	if err := collector.startStream(); err != nil {
		t.Fatalf("startStream: %v", err)
	}
	stop := collector.startHeartbeat(20 * time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	stop()
	collector.finishStream()

	empty := 0
	for _, delta := range streamDeltas(t, recorder.Body.String()) {
		if len(delta) == 0 {
			empty++
		}
	}
	// One empty delta closes the stream; heartbeats are the rest.
	if empty < 3 {
		t.Errorf("empty deltas = %d, want at least 3 (heartbeats plus the final chunk)", empty)
	}
}

func TestHeartbeatStaysSilentWhileTextFlows(t *testing.T) {
	recorder := httptest.NewRecorder()
	collector := newCollector(recorder, true, "chatcmpl-test", 0, "test-agent", false, nil)
	if err := collector.startStream(); err != nil {
		t.Fatalf("startStream: %v", err)
	}
	stop := collector.startHeartbeat(50 * time.Millisecond)
	for range 8 {
		collector.appendText("chunk ")
		time.Sleep(20 * time.Millisecond)
	}
	stop()

	for _, delta := range streamDeltas(t, recorder.Body.String()) {
		if len(delta) == 0 {
			t.Fatal("heartbeat fired while the stream was still producing text")
		}
	}
}

func TestHeartbeatWritesNothingAfterStreamEnds(t *testing.T) {
	recorder := httptest.NewRecorder()
	collector := newCollector(recorder, true, "chatcmpl-test", 0, "test-agent", false, nil)
	if err := collector.startStream(); err != nil {
		t.Fatalf("startStream: %v", err)
	}
	stop := collector.startHeartbeat(10 * time.Millisecond)
	collector.finishStream()
	time.Sleep(60 * time.Millisecond)
	stop()

	body := recorder.Body.String()
	if got := strings.Count(body, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] count = %d, want 1", got)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream continued past [DONE]: %q", body[max(0, len(body)-120):])
	}
}

func TestHeartbeatDisabled(t *testing.T) {
	recorder := httptest.NewRecorder()
	collector := newCollector(recorder, true, "chatcmpl-test", 0, "test-agent", false, nil)
	if err := collector.startStream(); err != nil {
		t.Fatalf("startStream: %v", err)
	}
	stop := collector.startHeartbeat(-1)
	time.Sleep(40 * time.Millisecond)
	stop()

	for _, delta := range streamDeltas(t, recorder.Body.String()) {
		if len(delta) == 0 {
			t.Fatal("heartbeat fired although it was disabled")
		}
	}
}

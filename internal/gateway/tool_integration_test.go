package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmora/agentrun"
	"github.com/dmora/agentrun/engine/acp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This subprocess speaks ACP over real stdio and launches the descriptor it
// receives as an MCP subprocess. It knows no gateway internals or IPC details.
func runToolACP() {
	logEvent := func(v any) {
		f, err := os.OpenFile(os.Getenv("TEST_TOOL_ACP"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err == nil {
			_ = json.NewEncoder(f).Encode(v)
			f.Close()
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	enc := json.NewEncoder(os.Stdout)
	var session *mcp.ClientSession
	defer func() {
		if session != nil {
			session.Close()
		}
	}()
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			continue
		}
		reply := func(result any) { _ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) }
		fail := func(err error) {
			logEvent(map[string]any{"error": err.Error()})
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32603, "message": err.Error()}})
		}
		text := func(s string) {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "tool-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": s}}}})
		}
		switch req.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "agentInfo": map[string]string{"name": "fake", "version": "1"}, "authMethods": []any{}})
		case "session/new":
			var p struct {
				MCPServers []struct {
					Name, Command string
					Args          []string
				} `json:"mcpServers"`
			}
			_ = json.Unmarshal(req.Params, &p)
			logEvent(map[string]any{"start": true, "servers": len(p.MCPServers)})
			if len(p.MCPServers) > 0 {
				d := p.MCPServers[0]
				cmd := exec.Command(d.Command, d.Args...)
				var err error
				session, err = mcp.NewClient(&mcp.Implementation{Name: "fake-acp", Version: "1"}, nil).Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
				if err != nil {
					fail(err)
					continue
				}
				tools, err := session.ListTools(context.Background(), nil)
				if err != nil {
					fail(err)
					continue
				}
				logEvent(map[string]any{"tools": tools.Tools})
			}
			reply(map[string]any{"sessionId": "tool-session"})
		case "session/prompt":
			logEvent(map[string]any{"prompt": true})
			var p struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(req.Params, &p)
			prompt := ""
			for _, v := range p.Prompt {
				prompt += v.Text
			}
			if strings.Contains(prompt, "stall") {
				select {}
			}
			if strings.Contains(prompt, "parallel") {
				errors := make(chan error, 2)
				for _, name := range []string{"foo", "bar"} {
					go func(name string) {
						result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"x": 1}})
						if err == nil && result.IsError {
							err = fmt.Errorf("tool error")
						}
						errors <- err
					}(name)
				}
				one, two := <-errors, <-errors
				if one != nil {
					fail(one)
				} else if two != nil {
					fail(two)
				} else {
					text("final")
					reply(map[string]string{"stopReason": "end_turn"})
				}
				continue
			}
			if session == nil || strings.Contains(prompt, "no-call") {
				text("final")
				reply(map[string]string{"stopReason": "end_turn"})
				continue
			}
			names := []string{"foo"}
			if strings.Contains(prompt, "sequential") {
				names = append(names, "bar")
			}
			failed := false
			for _, name := range names {
				text("before " + name)
				result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"x": 1}})
				if err != nil {
					fail(err)
					failed = true
					break
				}
				if result.IsError {
					fail(fmt.Errorf("MCP error: %v", result.Content))
					failed = true
					break
				}
				content := ""
				for _, c := range result.Content {
					if c, ok := c.(*mcp.TextContent); ok {
						content += c.Text
					}
				}
				logEvent(map[string]any{"result": content, "name": name})
			}
			if !failed {
				text("final")
				reply(map[string]string{"stopReason": "end_turn"})
			}
		}
	}
}

const testTools = `[{"type":"function","function":{"name":"foo","description":"Foo tool","parameters":{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}}},{"type":"function","function":{"name":"bar","description":"Bar tool","parameters":{"type":"object","properties":{"x":{"type":"integer"}}}}}]`

func toolServer(t *testing.T, timeout time.Duration) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("TEST_TOOL_ACP", path)
	exe, _ := os.Executable()
	s := New(Config{Engines: map[string]agentrun.Engine{"fake": acp.NewEngine(acp.WithBinary(exe), acp.WithStderrWriter(io.Discard))}, DefaultCWD: t.TempDir(), TurnTimeout: timeout, SessionTTL: time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(s.Close)
	return s, path
}
func toolRequest(t *testing.T, s *Server, messages []chatMessage, tools string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"model": "fake", "messages": messages, "tools": json.RawMessage(tools), "stream": stream})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw))).WithContext(ctx)
	req.Header.Set("X-Session-ID", "tools-session")
	// affinity accepts X-Session-Affinity; response uses X-Session-ID.
	req.Header.Set("X-Session-Affinity", "tools-session")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}
func userMessage(s string) chatMessage {
	raw, _ := json.Marshal(s)
	return chatMessage{Role: "user", Content: raw}
}
func responseMessage(t *testing.T, r *httptest.ResponseRecorder, stream bool) (chatMessage, string) {
	t.Helper()
	if r.Code != 200 {
		t.Fatalf("status %d: %s", r.Code, r.Body.String())
	}
	if !stream {
		var v struct {
			Choices []struct {
				Message chatMessage `json:"message"`
				Reason  string      `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v.Choices[0].Message, v.Choices[0].Reason
	}
	message := chatMessage{Role: "assistant"}
	text := ""
	reason := ""
	for _, line := range strings.Split(r.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Error   any `json:"error"`
			Choices []struct {
				Delta struct {
					Content string     `json:"content"`
					Calls   []toolCall `json:"tool_calls"`
				} `json:"delta"`
				Reason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		if chunk.Error != nil {
			t.Fatalf("stream error: %s", line)
		}
		for _, choice := range chunk.Choices {
			text += choice.Delta.Content
			message.ToolCalls = append(message.ToolCalls, choice.Delta.Calls...)
			if choice.Reason != "" {
				reason = choice.Reason
			}
		}
	}
	if !strings.Contains(r.Body.String(), "data: [DONE]") {
		t.Fatal("missing DONE")
	}
	message.Content, _ = json.Marshal(text)
	return message, reason
}
func addResult(messages []chatMessage, assistant chatMessage) []chatMessage {
	return append(messages, assistant, chatMessage{Role: "tool", ToolCallID: assistant.ToolCalls[0].ID, Content: json.RawMessage(`"ok"`)})
}
func TestToolsACPIntegration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, sequential := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/sequential=%v", stream, sequential), func(t *testing.T) {
				s, path := toolServer(t, 10*time.Second)
				prompt := "single"
				if sequential {
					prompt = "sequential"
				}
				messages := []chatMessage{userMessage(prompt)}
				names := []string{"foo"}
				if sequential {
					names = append(names, "bar")
				}
				for _, name := range names {
					assistant, reason := responseMessage(t, toolRequest(t, s, messages, testTools, stream), stream)
					if reason != "tool_calls" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Name != name || assistant.ToolCalls[0].Function.Arguments != `{"x":1}` || string(assistant.Content) != `"before `+name+`"` {
						t.Fatalf("response: %+v, reason %s", assistant, reason)
					}
					messages = addResult(messages, assistant)
				}
				assistant, reason := responseMessage(t, toolRequest(t, s, messages, testTools, stream), stream)
				if reason != "stop" || string(assistant.Content) != `"final"` {
					t.Fatalf("final: %+v %s", assistant, reason)
				}
				data, _ := os.ReadFile(path)
				log := string(data)
				if strings.Count(log, `"prompt":true`) != 1 || strings.Count(log, `"start":true`) != 1 || strings.Count(log, `"result":"ok"`) != len(names) {
					t.Fatalf("not one ACP turn: %s", log)
				}
				if !strings.Contains(log, `"description":"Foo tool"`) || !strings.Contains(log, `"name":"bar"`) || !strings.Contains(log, `"required":["x"]`) {
					t.Fatalf("discovery lost schemas: %s", log)
				}
			})
		}
	}
}
func TestToolsInvalidResultsAndIdentity(t *testing.T) {
	s, path := toolServer(t, 10*time.Second)
	messages := []chatMessage{userMessage("single")}
	assistant, _ := responseMessage(t, toolRequest(t, s, messages, testTools, false), false)
	valid := addResult(messages, assistant)
	bad := append([]chatMessage(nil), valid...)
	bad[len(bad)-1].ToolCallID = "unknown"
	if r := toolRequest(t, s, bad, testTools, false); r.Code != 400 {
		t.Fatalf("unknown: %d %s", r.Code, r.Body.String())
	}
	dup := append(append([]chatMessage(nil), valid...), valid[len(valid)-1])
	if r := toolRequest(t, s, dup, testTools, false); r.Code != 400 {
		t.Fatalf("duplicate: %d %s", r.Code, r.Body.String())
	}
	// Object key order does not reset the session.
	reordered := strings.Replace(testTools, `"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]`, `"required":["x"],"properties":{"x":{"type":"integer"}},"type":"object"`, 1)
	_, reason := responseMessage(t, toolRequest(t, s, valid, reordered, false), false)
	if reason != "stop" {
		t.Fatal(reason)
	}
	if r := toolRequest(t, s, valid, testTools, false); r.Code != 400 {
		t.Fatalf("replayed result: %d", r.Code)
	}
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), `"start":true`) != 1 {
		t.Fatal(string(data))
	}
}
func TestToolsCleanup(t *testing.T) {
	for _, mode := range []string{"evict", "reset", "shutdown", "timeout", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			timeout := 10 * time.Second
			if mode == "timeout" {
				timeout = 700 * time.Millisecond
			}
			s, _ := toolServer(t, timeout)
			messages := []chatMessage{userMessage("single")}
			assistant, _ := responseMessage(t, toolRequest(t, s, messages, testTools, false), false)
			state := s.registry.lock("tools-session:fake")
			turn := state.active
			facade := state.facade
			state.mu.Unlock()
			switch mode {
			case "evict":
				state.mu.Lock()
				state.lastAccess = time.Now().Add(-2 * time.Hour)
				state.mu.Unlock()
				s.registry.evictIdle()
			case "reset":
				responseMessage(t, toolRequest(t, s, []chatMessage{userMessage("no-call")}, strings.ReplaceAll(testTools, "Foo tool", "new description"), false), false)
			case "shutdown":
				s.Close()
			case "disconnect":
				facade.server.Close()
			}
			select {
			case <-turn.ctx.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("pending turn was not canceled")
			}
			select {
			case <-turn.done:
			case <-time.After(3 * time.Second):
				t.Fatal("RunTurn leaked")
			}
			if mode != "shutdown" {
				r := toolRequest(t, s, addResult(messages, assistant), testTools, false)
				if r.Code < 400 || r.Code >= 500 {
					t.Fatalf("stale result: %d %s", r.Code, r.Body.String())
				}
			}
		})
	}
}
func TestNoToolsHasNoMCPDescriptor(t *testing.T) {
	s, path := toolServer(t, 10*time.Second)
	responseMessage(t, toolRequest(t, s, []chatMessage{userMessage("hello")}, `[]`, false), false)
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"servers":0`) {
		t.Fatal(string(data))
	}
}

func TestToolsParallelACP(t *testing.T) {
	s, path := toolServer(t, 10*time.Second)
	messages := []chatMessage{userMessage("parallel")}
	seen := map[string]bool{}
	for range 2 {
		assistant, reason := responseMessage(t, toolRequest(t, s, messages, testTools, false), false)
		if reason != "tool_calls" || len(assistant.ToolCalls) != 1 {
			t.Fatalf("%+v %s", assistant, reason)
		}
		name := assistant.ToolCalls[0].Function.Name
		if seen[name] {
			t.Fatal("duplicate tool")
		}
		seen[name] = true
		messages = addResult(messages, assistant)
	}
	_, reason := responseMessage(t, toolRequest(t, s, messages, testTools, false), false)
	if reason != "stop" {
		t.Fatal(reason)
	}
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), `"prompt":true`) != 1 {
		t.Fatal(string(data))
	}
}

func TestToolsReuseAfterFinal(t *testing.T) {
	s, path := toolServer(t, 10*time.Second)
	messages := []chatMessage{userMessage("single")}
	assistant, _ := responseMessage(t, toolRequest(t, s, messages, testTools, false), false)
	messages = addResult(messages, assistant)
	final, _ := responseMessage(t, toolRequest(t, s, messages, testTools, false), false)
	messages = append(messages, final, userMessage("no-call"))
	responseMessage(t, toolRequest(t, s, messages, testTools, false), false)
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), `"prompt":true`) != 2 || strings.Count(string(data), `"start":true`) != 1 {
		t.Fatal(string(data))
	}
}

func TestToolsChoiceEnforcement(t *testing.T) {
	for _, tc := range []struct {
		choice, prompt string
		status         int
	}{
		{`"required"`, "no-call", 502}, {`"none"`, "single", 502},
		{`{"type":"function","function":{"name":"bar"}}`, "single", 502},
		{`{"type":"function","function":{"name":"foo"}}`, "single", 200},
		{`"required"`, "single", 200}, {`"none"`, "no-call", 200},
	} {
		t.Run(tc.choice+tc.prompt, func(t *testing.T) {
			s, _ := toolServer(t, 10*time.Second)
			raw, _ := json.Marshal(map[string]any{"model": "fake", "tools": json.RawMessage(testTools), "tool_choice": json.RawMessage(tc.choice), "messages": []chatMessage{userMessage(tc.prompt)}})
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw)))
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestToolsTimeoutDuringHTTP(t *testing.T) {
	s, _ := toolServer(t, 300*time.Millisecond)
	rec := toolRequest(t, s, []chatMessage{userMessage("stall")}, testTools, false)
	if rec.Code != 504 || !strings.Contains(rec.Body.String(), "deadline exceeded") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestToolsChangedToolsetRejectsOldResult(t *testing.T) {
	s, _ := toolServer(t, 10*time.Second)
	messages := []chatMessage{userMessage("single")}
	assistant, _ := responseMessage(t, toolRequest(t, s, messages, testTools, false), false)
	state := s.registry.lock("tools-session:fake")
	turn := state.active
	state.mu.Unlock()
	changed := strings.ReplaceAll(testTools, "Foo tool", "Changed tool")
	r := toolRequest(t, s, addResult(messages, assistant), changed, false)
	if r.Code != 400 {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	select {
	case <-turn.done:
	case <-time.After(time.Second):
		t.Fatal("old turn not stopped")
	}
	responseMessage(t, toolRequest(t, s, []chatMessage{userMessage("no-call")}, changed, false), false)
}

func TestToolsCloseDuringAttachedHTTP(t *testing.T) {
	s, path := toolServer(t, 10*time.Second)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- toolRequest(t, s, []chatMessage{userMessage("stall")}, testTools, false) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), `"prompt":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ACP did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case r := <-done:
		if r.Code != 502 {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP handler did not exit")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close deadlocked")
	}
}

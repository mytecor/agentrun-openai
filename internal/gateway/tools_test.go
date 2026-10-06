package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestToolValidationAndCanonicalHistory(t *testing.T) {
	var tools []functionTool
	_ = json.Unmarshal([]byte(testTools), &tools)
	for _, choice := range []string{`"auto"`, `"none"`, `"required"`, `{"type":"function","function":{"name":"foo"}}`} {
		if _, _, _, err := normalizeTools(tools, json.RawMessage(choice)); err != nil {
			t.Fatal(err)
		}
	}
	for _, choice := range []string{`"bad"`, `"named"`, `{}`, `{"type":"function","function":{"name":"missing"}}`, `42`} {
		if _, _, _, err := normalizeTools(tools, json.RawMessage(choice)); err == nil {
			t.Fatalf("accepted %s", choice)
		}
	}
	if _, _, _, err := normalizeTools(nil, json.RawMessage(`"required"`)); err == nil {
		t.Fatal("required without tools")
	}
	for _, bad := range []string{`[{"type":"custom"}]`, `[{"type":"function","function":{"name":"foo","parameters":{"type":"array"}}}]`, `[{"type":"function","function":{"name":"foo","parameters":null}}]`} {
		var v []functionTool
		_ = json.Unmarshal([]byte(bad), &v)
		if _, _, _, err := normalizeTools(v, nil); err == nil {
			t.Fatal(bad)
		}
	}
	_, hash, _, _ := normalizeTools(tools, nil)
	tools[0], tools[1] = tools[1], tools[0]
	_, reordered, _, _ := normalizeTools(tools, nil)
	if hash != reordered {
		t.Fatal("tool order changed identity")
	}
	raw, err := canonicalJSON([]byte(`{"z":9007199254740993,"a":1}`))
	if err != nil || string(raw) != `{"a":1,"z":9007199254740993}` {
		t.Fatal(string(raw), err)
	}
	first := []chatMessage{{Role: "assistant", Content: json.RawMessage(`null`), ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: calledFunction{Name: "foo", Arguments: `{"z":2,"a":1}`}}}}}
	a, err := normalizeMessages(first)
	if err != nil {
		t.Fatal(err)
	}
	first[0].Content = json.RawMessage(`""`)
	first[0].ToolCalls[0].Function.Arguments = `{ "a": 1, "z": 2 }`
	b, err := normalizeMessages(first)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPrefix(a, b) || transcriptHash(a) != transcriptHash(b) {
		t.Fatal("tool history not canonical")
	}
	first[0].ToolCalls[0].ID = "call_2"
	c, _ := normalizeMessages(first)
	if hasPrefix(a, c) {
		t.Fatal("IDs not compared")
	}
}

func TestFacadeIsolationAndUnknownTool(t *testing.T) {
	var tools []functionTool
	_ = json.Unmarshal([]byte(testTools), &tools)
	f, err := newMCPFacade(context.Background(), tools)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	other, err := newMCPFacade(context.Background(), tools)
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	call := func(path, token, body string) int {
		req, _ := http.NewRequest("POST", "http://"+f.descriptor.Args[1]+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if status := call("/tools", other.token, ""); status != 401 {
		t.Fatal(status)
	}
	if status := call("/call", f.token, `{"name":"missing","arguments":{}}`); status != 400 {
		t.Fatal(status)
	}
	if status := call("/tools", f.token, ""); status != 200 {
		t.Fatal(status)
	}
}

func TestConcurrentPendingCallsAndPolicy(t *testing.T) {
	turn := newActiveTurn(context.Background(), toolPolicy{Mode: "auto"})
	defer turn.cancel(context.Canceled)
	result := make(chan error, 2)
	for _, name := range []string{"foo", "bar"} {
		go func(name string) {
			value, err := turn.call(context.Background(), name, []byte(`{"x":1}`))
			if err == nil && value != "ok" {
				t.Errorf("result %q", value)
			}
			result <- err
		}(name)
	}
	events := []turnEvent{<-turn.events, <-turn.events}
	turn.deliver(events[0].call.ID)
	if err := turn.resolve([]transcriptMessage{{Role: "tool", ToolCallID: events[0].call.ID, Content: "ok"}}, toolPolicy{Mode: "auto"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first call stuck")
	}
	turn.deliver(events[1].call.ID)
	if err := turn.resolve([]transcriptMessage{{Role: "tool", ToolCallID: events[1].call.ID, Content: "ok"}}, toolPolicy{Mode: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := turn.call(context.Background(), "foo", []byte(`{}`)); err == nil {
		t.Fatal("none allowed a call")
	}
}

func TestToolHistoryIsNotANewPrompt(t *testing.T) {
	messages := []transcriptMessage{{Role: "user", Content: "old"}, {Role: "assistant", ToolCalls: []toolCall{{ID: "call_1"}}}}
	if _, err := turnPrompt(messages); err == nil {
		t.Fatal("trailing tool call resent previous user prompt")
	}
	messages = append(messages, transcriptMessage{Role: "tool", ToolCallID: "call_1", Content: "old result"}, transcriptMessage{Role: "user", Content: "new"})
	prompt, err := turnPrompt(messages)
	if err != nil || strings.Contains(prompt, "old result") || strings.Contains(prompt, "call_1") {
		t.Fatalf("tool history replayed: %s, %v", prompt, err)
	}
}

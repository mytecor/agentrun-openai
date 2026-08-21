package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/dmora/agentrun"
)

type collector struct {
	w              http.ResponseWriter
	stream         bool
	id             string
	created        int64
	model          string
	text           strings.Builder
	usage          completionUsage
	sawDelta       bool
	streamStarted  bool
	resumeID       string
	waitingForInit bool
}

func newCollector(w http.ResponseWriter, stream bool, id string, created int64, model string, resumed bool) *collector {
	return &collector{w: w, stream: stream, id: id, created: created, model: model, waitingForInit: resumed}
}

func (c *collector) startStream() error {
	if _, ok := c.w.(http.Flusher); !ok {
		return fmt.Errorf("response writer does not support flushing")
	}
	c.w.Header().Set("Content-Type", "text/event-stream")
	c.w.Header().Set("Cache-Control", "no-cache")
	c.w.Header().Set("Connection", "keep-alive")
	c.streamStarted = true
	c.writeChunk(map[string]string{"role": "assistant"}, nil, nil)
	return nil
}

func (c *collector) handle(message agentrun.Message) error {
	switch message.Type {
	case agentrun.MessageInit:
		if message.ResumeID != "" {
			c.resumeID = message.ResumeID
		}
		c.waitingForInit = false
	case agentrun.MessageTextDelta:
		if c.waitingForInit {
			return nil
		}
		c.sawDelta = true
		c.appendText(message.Content)
	case agentrun.MessageText:
		if c.waitingForInit {
			return nil
		}
		if !c.sawDelta {
			c.appendText(message.Content)
		}
		c.sawDelta = false
	case agentrun.MessageResult:
		if message.Usage != nil {
			c.usage.PromptTokens = message.Usage.InputTokens
			c.usage.CompletionTokens = message.Usage.OutputTokens
			c.usage.TotalTokens = message.Usage.InputTokens + message.Usage.OutputTokens
		}
	case agentrun.MessageError:
		return fmt.Errorf("agent: %s", message.Content)
	}
	return nil
}

func (c *collector) appendText(text string) {
	c.text.WriteString(text)
	if c.stream && text != "" {
		c.writeChunk(map[string]string{"content": text}, nil, nil)
	}
}

func (c *collector) writeCompletion() {
	c.w.Header().Set("Content-Type", "application/json")
	response := map[string]any{
		"id": c.id, "object": "chat.completion", "created": c.created, "model": c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": c.text.String()},
			"finish_reason": "stop",
		}},
		"usage": c.usage,
	}
	_ = json.NewEncoder(c.w).Encode(response)
}

func (c *collector) finishStream() {
	reason := "stop"
	c.writeChunk(map[string]string{}, &reason, &c.usage)
	fmt.Fprint(c.w, "data: [DONE]\n\n")
	c.flush()
}

func (c *collector) streamError(err error) {
	if !c.streamStarted {
		return
	}
	payload := apiErrorEnvelope{Error: apiError{Message: err.Error(), Type: "server_error", Code: "agent_turn_failed"}}
	data, _ := json.Marshal(payload)
	fmt.Fprintf(c.w, "data: %s\n\n", data)
	fmt.Fprint(c.w, "data: [DONE]\n\n")
	c.flush()
}

func (c *collector) writeChunk(delta map[string]string, finishReason *string, usage *completionUsage) {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}
	payload := map[string]any{
		"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []any{choice},
	}
	if usage != nil {
		payload["usage"] = usage
	}
	data, _ := json.Marshal(payload)
	fmt.Fprintf(c.w, "data: %s\n\n", data)
	c.flush()
}

func (c *collector) flush() {
	if flusher, ok := c.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

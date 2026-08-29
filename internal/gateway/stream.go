package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dmora/agentrun"
)

// heartbeatMarker is what a keep-alive delta carries. An empty delta keeps the
// TCP connection open but is invisible to OpenAI-compatible clients: they parse
// a chunk only when it holds a non-empty string, so a stall detector watching
// for progress still sees a dead stream. A zero-width space is non-empty on the
// wire and renders as nothing.
const heartbeatMarker = "\u200b"

type collector struct {
	w              http.ResponseWriter
	stream         bool
	id             string
	created        int64
	model          string
	text           strings.Builder
	reasoning      strings.Builder
	usage          completionUsage
	sawDelta       bool
	sawThinking    bool
	streamStarted  bool
	resumeID       string
	waitingForInit bool
	effectiveIDs   []string

	// mu guards every write to w. The heartbeat writes from its own goroutine
	// while the turn is still streaming, and http.ResponseWriter is not safe
	// for concurrent use.
	mu        sync.Mutex
	closed    bool
	lastWrite time.Time
}

func newCollector(w http.ResponseWriter, stream bool, id string, created int64, model string, resumed bool, effectiveIDs []string) *collector {
	return &collector{w: w, stream: stream, id: id, created: created, model: model, waitingForInit: resumed, effectiveIDs: effectiveIDs}
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
		if message.Init != nil && message.Init.Model != "" && len(c.effectiveIDs) > 0 && !contains(c.effectiveIDs, message.Init.Model) {
			return fmt.Errorf("backend selected model %q; expected one of %q", message.Init.Model, c.effectiveIDs)
		}
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
	case agentrun.MessageThinkingDelta:
		if c.waitingForInit {
			return nil
		}
		c.sawThinking = true
		c.appendReasoning(message.Content)
	case agentrun.MessageThinking:
		if c.waitingForInit {
			return nil
		}
		if !c.sawThinking {
			c.appendReasoning(message.Content)
		}
		c.sawThinking = false
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

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (c *collector) appendText(text string) {
	c.text.WriteString(text)
	if c.stream && text != "" {
		c.writeChunk(map[string]string{"content": text}, nil, nil)
	}
}

// appendReasoning streams thinking output as reasoning_content. It is kept out
// of c.text so the conversation transcript stores only the assistant's answer.
func (c *collector) appendReasoning(text string) {
	if text == "" {
		return
	}
	c.reasoning.WriteString(text)
	if c.stream {
		c.writeChunk(map[string]string{"reasoning_content": text}, nil, nil)
	}
}

// startHeartbeat emits a keep-alive delta whenever the stream has been silent for
// interval. A coding agent runs its own tools between text blocks, so a turn
// can legitimately produce no output for minutes; without a heartbeat, client
// stall detectors abort the request mid-turn. The returned function stops the
// heartbeat and waits for its goroutine to exit.
func (c *collector) startHeartbeat(interval time.Duration) func() {
	if !c.stream || interval <= 0 {
		return func() {}
	}
	// Tick faster than the interval: a tick that lands just after real output
	// is skipped, so ticking at exactly interval would allow silences of up to
	// twice it.
	tick := interval / 2
	if tick < time.Millisecond {
		tick = time.Millisecond
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				c.beat(interval)
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

func (c *collector) beat(interval time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || time.Since(c.lastWrite) < interval {
		return
	}
	c.writeChunkLocked(map[string]string{"reasoning_content": heartbeatMarker}, nil, nil)
}

func (c *collector) resetForFreshSession() {
	c.text.Reset()
	c.reasoning.Reset()
	c.usage = completionUsage{}
	c.sawDelta = false
	c.sawThinking = false
	c.resumeID = ""
	c.waitingForInit = false
}

func (c *collector) writeCompletion() {
	c.w.Header().Set("Content-Type", "application/json")
	message := map[string]string{"role": "assistant", "content": c.text.String()}
	if c.reasoning.Len() > 0 {
		message["reasoning_content"] = c.reasoning.String()
	}
	response := map[string]any{
		"id": c.id, "object": "chat.completion", "created": c.created, "model": c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
		"usage": c.usage,
	}
	_ = json.NewEncoder(c.w).Encode(response)
}

func (c *collector) finishStream() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	reason := "stop"
	c.writeChunkLocked(map[string]string{}, &reason, &c.usage)
	fmt.Fprint(c.w, "data: [DONE]\n\n")
	c.flushLocked()
	c.closed = true
}

func (c *collector) streamError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.streamStarted || c.closed {
		return
	}
	payload := apiErrorEnvelope{Error: apiError{Message: err.Error(), Type: "server_error", Code: "agent_turn_failed"}}
	data, _ := json.Marshal(payload)
	fmt.Fprintf(c.w, "data: %s\n\n", data)
	fmt.Fprint(c.w, "data: [DONE]\n\n")
	c.flushLocked()
	c.closed = true
}

func (c *collector) writeChunk(delta map[string]string, finishReason *string, usage *completionUsage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeChunkLocked(delta, finishReason, usage)
}

func (c *collector) writeChunkLocked(delta map[string]string, finishReason *string, usage *completionUsage) {
	if c.closed {
		return
	}
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
	c.flushLocked()
	c.lastWrite = time.Now()
}

func (c *collector) flushLocked() {
	if flusher, ok := c.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

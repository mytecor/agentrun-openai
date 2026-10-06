package gateway

import (
	"context"
	"fmt"
	"sync"

	"github.com/dmora/agentrun"
)

type turnEvent struct {
	message *agentrun.Message
	call    *toolCall
	done    bool
	err     error
}
type pendingCall struct {
	call      toolCall
	result    chan string
	delivered bool
}

// activeTurn owns everything that survives an HTTP response boundary. No
// goroutine belonging to it retains a ResponseWriter or a request context.
type activeTurn struct {
	ctx                   context.Context
	cancel                context.CancelCauseFunc
	events                chan turnEvent
	done                  chan struct{}
	mu                    sync.Mutex
	pending               map[string]*pendingCall
	policy                toolPolicy
	sawDelta, sawThinking bool // collector deduplication across response segments
}

func newActiveTurn(parent context.Context, policy toolPolicy) *activeTurn {
	ctx, cancel := context.WithCancelCause(parent)
	return &activeTurn{ctx: ctx, cancel: cancel, events: make(chan turnEvent, 64), done: make(chan struct{}), pending: map[string]*pendingCall{}, policy: policy}
}
func (t *activeTurn) emit(e turnEvent) error {
	select {
	case <-t.ctx.Done():
		return context.Cause(t.ctx)
	case t.events <- e:
		return nil
	}
}
func (t *activeTurn) run(proc agentrun.Process, prompt string, first bool) {
	defer close(t.done)
	run := agentrun.RunTurn
	if first {
		run = agentrun.RunFirstTurn
	}
	err := run(t.ctx, proc, prompt, func(m agentrun.Message) error { return t.emit(turnEvent{message: &m}) })
	_ = t.emit(turnEvent{done: true, err: err})
}
func (t *activeTurn) call(ctx context.Context, name string, args []byte) (string, error) {
	raw, err := canonicalJSON(args)
	if err != nil || len(raw) == 0 || raw[0] != '{' {
		return "", fmt.Errorf("tool arguments must be a JSON object")
	}
	t.mu.Lock()
	if t.policy.Mode == "none" || (t.policy.Mode == "named" && t.policy.Name != name) {
		t.mu.Unlock()
		return "", fmt.Errorf("tool %q is disallowed by tool_choice", name)
	}
	p := &pendingCall{call: toolCall{ID: randomID("call_"), Type: "function", Function: calledFunction{Name: name, Arguments: string(raw)}}, result: make(chan string, 1)}
	t.pending[p.call.ID] = p
	t.mu.Unlock()
	if err := t.emit(turnEvent{call: &p.call}); err != nil {
		return "", err
	}
	select {
	case result := <-p.result:
		return result, nil
	case <-t.ctx.Done():
		return "", context.Cause(t.ctx)
	case <-ctx.Done():
		t.cancel(fmt.Errorf("MCP call disconnected: %w", ctx.Err()))
		return "", ctx.Err()
	}
}
func (t *activeTurn) deliver(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := t.pending[id]; p != nil {
		p.delivered = true
	}
}

// Validate the entire batch before releasing any calls. Undelivered concurrent
// calls are left queued for a later response segment.
func (t *activeTurn) resolve(messages []transcriptMessage, policy toolPolicy) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := context.Cause(t.ctx); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range messages {
		p := t.pending[m.ToolCallID]
		if m.Role != "tool" || p == nil || !p.delivered || seen[m.ToolCallID] {
			return fmt.Errorf("unknown, duplicate or undelivered tool_call_id %q", m.ToolCallID)
		}
		seen[m.ToolCallID] = true
	}
	if len(seen) == 0 {
		return fmt.Errorf("active turn requires tool results")
	}
	for id, p := range t.pending {
		if p.delivered && !seen[id] {
			return fmt.Errorf("missing tool result for %q", id)
		}
	}
	t.policy = policy
	for _, m := range messages {
		p := t.pending[m.ToolCallID]
		delete(t.pending, m.ToolCallID)
		p.result <- m.Content
	}
	return nil
}

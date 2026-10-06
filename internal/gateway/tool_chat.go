package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dmora/agentrun"
)

// handleToolChat is called with the session lock held. Only this handler reads
// turn events and writes a response; the running turn belongs to sessionState.
func (s *Server) handleToolChat(w http.ResponseWriter, r *http.Request, request chatRequest, messages []transcriptMessage, route modelRoute, state *sessionState, key, cwd, hash string, tools []functionTool, policy toolPolicy) {
	isResult := messages[len(messages)-1].Role == "tool"
	same := state.cwd == cwd && state.toolHash == hash && state.matchesHistoryPrefix(messages)
	if isResult {
		if state.active != nil && (state.cwd != cwd || state.toolHash != hash) {
			s.clearSession(state, key)
		}
		if state.active == nil || !same {
			message := "tool result does not belong to an active session; preserve session affinity, history and tools"
			if state.turnErr != nil {
				message = state.turnErr.Error()
			}
			writeError(w, 400, message, "invalid_request_error", "invalid_tool_result")
			return
		}
		if err := state.active.resolve(messages[state.persistedHistoryCount():], policy); err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error", "invalid_tool_result")
			return
		}
	} else {
		if same && state.active != nil {
			writeError(w, 409, "the active turn is waiting for tool results", "invalid_request_error", "turn_pending")
			return
		}
		delta := messages
		if same && state.process != nil && (state.facade == nil || state.facade.ctx.Err() == nil) {
			delta = messages[state.persistedHistoryCount():]
		} else {
			s.clearSession(state, key)
		}
		// Historical tool messages may be checked for continuity, but may never
		// serve as new ACP prompts or resolve calls from an abandoned session.
		for _, m := range delta {
			if m.Role == "tool" && same {
				writeError(w, 400, "unexpected tool result", "invalid_request_error", "invalid_tool_result")
				return
			}
		}
		prompt, err := turnPrompt(delta)
		if err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error", "invalid_messages")
			return
		}
		if policy.Mode == "required" {
			prompt += "\n\nCall at least one of the provided MCP function tools before answering."
		}
		if policy.Mode == "named" {
			prompt += fmt.Sprintf("\n\nCall the provided MCP function tool %q before answering.", policy.Name)
		}
		if policy.Mode == "none" {
			prompt += "\n\nDo not call the provided MCP function tools for this response."
		}
		first := state.process == nil
		state.turnErr = nil
		if first && len(tools) > 0 {
			state.facade, err = newMCPFacade(s.ctx, tools)
			if err != nil {
				writeError(w, 502, err.Error(), "server_error", "bridge_start_failed")
				return
			}
		}
		timeoutCtx, timeoutCancel := context.WithTimeout(s.ctx, s.config.TurnTimeout)
		turn := newActiveTurn(timeoutCtx, policy)
		// Disconnects cancel work only while this HTTP exchange is attached.
		// Unregister before returning tool_calls so the next exchange can resume.
		detachRequest := context.AfterFunc(r.Context(), func() { turn.cancel(r.Context().Err()) })
		defer detachRequest()
		state.active = turn
		state.toolHash = hash
		state.cwd = cwd
		if state.facade != nil {
			state.facade.bind(turn)
		}
		if first {
			options := map[string]string{agentrun.OptionHITL: string(agentrun.HITLOff)}
			if system := systemPrompt(messages); system != "" {
				options[agentrun.OptionSystemPrompt] = system
			}
			session := agentrun.Session{CWD: cwd, Model: route.backendModel, Prompt: prompt, Options: options}
			if state.facade != nil {
				session.MCPServers = []agentrun.MCPServer{state.facade.descriptor}
			}
			state.process, err = route.engine.Start(turn.ctx, session)
			if err != nil {
				close(turn.done)
				timeoutCancel()
				s.clearSession(state, key)
				writeError(w, 502, err.Error(), "server_error", "agent_start_failed")
				return
			}
		}
		go turn.run(state.process, prompt, first)
		// This watcher also cleans up a timed-out turn between HTTP exchanges.
		// Comparing pointers prevents a late watcher from clearing a new turn.
		go func() {
			<-turn.ctx.Done()
			timeoutCancel()
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.active == turn {
				err := context.Cause(turn.ctx)
				s.clearSession(state, key)
				state.turnErr = err
			}
		}()
	}
	turn := state.active
	c := newCollector(w, request.Stream, randomID("chatcmpl-"), time.Now().Unix(), request.Model, false, route.effectiveIDs)
	c.sawDelta = turn.sawDelta
	c.sawThinking = turn.sawThinking
	if request.Stream {
		_ = c.startStream()
		defer c.startHeartbeat(s.config.StreamHeartbeat)()
	}
	var err error
	finished := false
	for !finished && err == nil {
		select {
		case <-r.Context().Done():
			err = r.Context().Err()
		case <-turn.ctx.Done():
			err = context.Cause(turn.ctx)
		case event := <-turn.events:
			if event.message != nil {
				err = c.handle(*event.message)
			}
			if event.call != nil {
				if policy.Mode == "none" || (policy.Mode == "named" && policy.Name != event.call.Function.Name) {
					err = fmt.Errorf("pending call to %q conflicts with tool_choice", event.call.Function.Name)
					break
				}
				turn.deliver(event.call.ID)
				c.toolCalls = append(c.toolCalls, *event.call)
				finished = true
			}
			if event.done {
				err = event.err
				finished = true
				if err == nil && (policy.Mode == "required" || policy.Mode == "named") {
					err = fmt.Errorf("ACP turn completed without the tool call required by tool_choice")
				}
				if err == nil {
					state.active = nil
					if state.facade != nil {
						state.facade.bind(nil)
					}
					turn.cancel(context.Canceled)
				}
			}
		}
	}
	if err == nil && state.active != nil {
		err = context.Cause(turn.ctx)
	}
	if err != nil {
		s.clearSession(state, key)
		state.turnErr = err
		if request.Stream {
			c.streamError(err)
		} else {
			status := 502
			if errors.Is(err, context.DeadlineExceeded) {
				status = 504
			}
			writeError(w, status, err.Error(), "server_error", "agent_turn_failed")
		}
		return
	}
	turn.sawDelta = c.sawDelta
	turn.sawThinking = c.sawThinking
	state.history = append(append([]transcriptMessage(nil), messages...), transcriptMessage{Role: "assistant", Content: c.text.String(), ToolCalls: c.toolCalls})
	state.historyCount = len(state.history)
	state.historyHash = transcriptHash(state.history)
	state.lastAccess = time.Now()
	if c.resumeID != "" {
		state.resumeID = c.resumeID
	}
	// Pending turns are deliberately never persisted: MCP calls cannot survive
	// gateway restart, and native resume must not resurrect stale IPC endpoints.
	if err := s.registry.store.delete(key); err != nil {
		s.config.Logger.Warn("delete tool session metadata", "error", err)
	}
	if request.Stream {
		c.finishStream()
	} else {
		c.writeCompletion()
	}
}

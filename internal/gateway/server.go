package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dmora/agentrun"
)

type Config struct {
	Engines     map[string]agentrun.Engine
	DefaultCWD  string
	APIKey      string
	TurnTimeout time.Duration
	SessionTTL  time.Duration
	Logger      *slog.Logger
}

type Server struct {
	config   Config
	models   []string
	registry *registry
	stop     chan struct{}
	close    sync.Once
}

func New(config Config) *Server {
	if config.TurnTimeout <= 0 {
		config.TurnTimeout = 30 * time.Minute
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	models := make([]string, 0, len(config.Engines))
	for model := range config.Engines {
		models = append(models, model)
	}
	sort.Strings(models)
	s := &Server{config: config, models: models, registry: newRegistry(config.SessionTTL), stop: make(chan struct{})}
	go s.janitor()
	return s
}

func (s *Server) Close() {
	s.close.Do(func() {
		close(s.stop)
		s.registry.close()
	})
}

func (s *Server) janitor() {
	interval := s.config.SessionTTL / 2
	if interval <= 0 || interval > time.Minute {
		interval = time.Minute
	} else if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.registry.evictIdle()
		case <-s.stop:
			return
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		s.handleModels(w)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		s.handleChat(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if s.config.APIKey == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+s.config.APIKey
}

func (s *Server) handleModels(w http.ResponseWriter) {
	data := make([]map[string]any, 0, len(s.models))
	for _, model := range s.models {
		data = append(data, map[string]any{"id": model, "object": "model", "created": 0, "owned_by": "agentrun"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	decoder := json.NewDecoder(r.Body)
	var request chatRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request_error", "invalid_json")
		return
	}
	if err := decoder.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request body must contain exactly one JSON object", "invalid_request_error", "invalid_json")
		return
	}
	engine := s.config.Engines[request.Model]
	if engine == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("model %q was not found", request.Model), "invalid_request_error", "model_not_found")
		return
	}
	messages, err := normalizeMessages(request.Messages)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_messages")
		return
	}
	if request.Stream {
		if _, ok := w.(http.Flusher); !ok {
			writeError(w, http.StatusInternalServerError, "response writer does not support flushing", "server_error", "streaming_unsupported")
			return
		}
	}
	sessionID := affinityID(r, request.SessionID)
	if sessionID == "" {
		sessionID = randomID("session-")
	} else if !validSessionID(sessionID) {
		writeError(w, http.StatusBadRequest, "session affinity must be at most 512 characters and contain no control characters", "invalid_request_error", "invalid_session_id")
		return
	}
	w.Header().Set("X-Session-ID", sessionID)
	cwd := strings.TrimSpace(r.Header.Get("X-Agent-CWD"))
	if cwd == "" {
		cwd = s.config.DefaultCWD
	}
	if !filepath.IsAbs(cwd) {
		writeError(w, http.StatusBadRequest, "X-Agent-CWD must be an absolute path", "invalid_request_error", "invalid_cwd")
		return
	}

	state := s.registry.lock(sessionID + ":" + request.Model)
	defer state.mu.Unlock()
	state.lastAccess = time.Now()

	reset := state.process == nil || state.cwd != cwd || !hasPrefix(messages, state.history)
	var delta []transcriptMessage
	if reset {
		delta = messages
		stopProcess(state.process)
		state.process = nil
		state.history = nil
	} else {
		delta = messages[len(state.history):]
	}
	prompt, err := turnPrompt(delta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_messages")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.config.TurnTimeout)
	defer cancel()
	first := state.process == nil
	if first {
		// This HTTP API cannot relay interactive permission prompts. HITL off
		// lets the coding-agent runtime execute its own tools inside the chosen
		// working directory instead of silently denying every operation.
		options := map[string]string{agentrun.OptionHITL: string(agentrun.HITLOff)}
		if system := systemPrompt(messages); system != "" {
			options[agentrun.OptionSystemPrompt] = system
		}
		process, startErr := engine.Start(ctx, agentrun.Session{CWD: cwd, Prompt: prompt, Options: options})
		if startErr != nil {
			s.logError("start agent", startErr, request.Model, sessionID)
			writeError(w, http.StatusBadGateway, startErr.Error(), "server_error", "agent_start_failed")
			return
		}
		state.process = process
		state.cwd = cwd
	}

	completionID := randomID("chatcmpl-")
	created := time.Now().Unix()
	collector := newCollector(w, request.Stream, completionID, created, request.Model)
	if request.Stream {
		if err := collector.startStream(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error", "streaming_unsupported")
			return
		}
	}
	run := agentrun.RunTurn
	if first {
		run = agentrun.RunFirstTurn
	}
	err = run(ctx, state.process, prompt, collector.handle)
	if err != nil {
		s.logError("agent turn", err, request.Model, sessionID)
		stopProcess(state.process)
		state.process = nil
		state.history = nil
		if request.Stream {
			collector.streamError(err)
			return
		}
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeError(w, status, err.Error(), "server_error", "agent_turn_failed")
		return
	}

	assistant := transcriptMessage{Role: "assistant", Content: collector.text.String()}
	state.history = append(append([]transcriptMessage(nil), messages...), assistant)
	state.lastAccess = time.Now()
	if request.Stream {
		collector.finishStream()
		return
	}
	collector.writeCompletion()
}

func affinityID(r *http.Request, bodyValue string) string {
	for _, value := range []string{r.Header.Get("X-Session-Affinity"), r.Header.Get("Session-ID"), r.Header.Get("session_id"), r.Header.Get("X-Client-Request-ID"), bodyValue} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func validSessionID(value string) bool {
	return len(value) <= 512 && strings.IndexFunc(value, unicode.IsControl) < 0
}

func randomID(prefix string) string {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(data[:])
}

func (s *Server) logError(operation string, err error, model, sessionID string) {
	s.config.Logger.Error(operation, "error", err, "model", model, "session_id", sessionID)
}

func writeError(w http.ResponseWriter, status int, message, errorType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiError{Message: message, Type: errorType, Code: code}})
}

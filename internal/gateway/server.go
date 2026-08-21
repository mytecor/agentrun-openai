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
	Engines        map[string]agentrun.Engine
	ModelDetails   map[string]ModelDetails
	DiscoverModels func(context.Context) ([]DiscoveredModel, error)
	DefaultCWD     string
	AllowedRoots   []string
	APIKey         string
	TurnTimeout    time.Duration
	SessionTTL     time.Duration
	SessionStore   string
	Logger         *slog.Logger
}

type ModelDetails struct {
	Name          string
	ContextWindow int
	MaxTokens     int
}

type DiscoveredModel struct {
	ID           string
	Engine       string
	BackendModel string
	Details      ModelDetails
}

type modelRoute struct {
	engine       agentrun.Engine
	engineID     string
	backendModel string
	details      ModelDetails
}

type Server struct {
	config   Config
	models   []string
	registry *registry
	routesMu sync.RWMutex
	routes   map[string]modelRoute
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
	registry, err := newRegistry(config.SessionTTL, config.SessionStore)
	if err != nil {
		config.Logger.Warn("load session store", "error", err, "path", config.SessionStore)
	}
	s := &Server{config: config, models: models, registry: registry, routes: make(map[string]modelRoute), stop: make(chan struct{})}
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
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models"):
		s.handleModels(w, r)
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

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	discovered := s.discoverModels(r.Context())
	data := make([]map[string]any, 0, len(s.models)+len(discovered))
	for _, model := range s.models {
		data = append(data, modelObject(model, s.config.ModelDetails[model]))
	}
	for _, model := range discovered {
		data = append(data, modelObject(model.ID, model.Details))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func modelObject(id string, details ModelDetails) map[string]any {
	name := details.Name
	if name == "" {
		name = id
	}
	return map[string]any{
		"id": id, "object": "model", "created": 0, "owned_by": "agentrun",
		"name": name, "context_window": details.ContextWindow, "max_tokens": details.MaxTokens,
	}
}

func (s *Server) discoverModels(ctx context.Context) []DiscoveredModel {
	if s.config.DiscoverModels == nil {
		return nil
	}
	models, err := s.config.DiscoverModels(ctx)
	if err != nil {
		s.config.Logger.Warn("discover backend models", "error", err)
		s.routesMu.RLock()
		defer s.routesMu.RUnlock()
		cached := make([]DiscoveredModel, 0, len(s.routes))
		for id, route := range s.routes {
			cached = append(cached, DiscoveredModel{ID: id, Engine: route.engineID, BackendModel: route.backendModel, Details: route.details})
		}
		sort.Slice(cached, func(i, j int) bool { return cached[i].ID < cached[j].ID })
		return cached
	}
	routes := make(map[string]modelRoute, len(models))
	valid := models[:0]
	for _, model := range models {
		engine := s.config.Engines[model.Engine]
		if engine == nil || model.ID == "" || model.BackendModel == "" {
			continue
		}
		routes[model.ID] = modelRoute{engine: engine, engineID: model.Engine, backendModel: model.BackendModel, details: model.Details}
		valid = append(valid, model)
	}
	s.routesMu.Lock()
	s.routes = routes
	s.routesMu.Unlock()
	sort.Slice(valid, func(i, j int) bool { return valid[i].ID < valid[j].ID })
	return valid
}

func (s *Server) resolveModel(id string) (modelRoute, bool) {
	if engine := s.config.Engines[id]; engine != nil {
		return modelRoute{engine: engine, engineID: id, details: s.config.ModelDetails[id]}, true
	}
	s.routesMu.RLock()
	route, ok := s.routes[id]
	s.routesMu.RUnlock()
	if ok {
		return route, true
	}
	engineID, backendModel, found := strings.Cut(id, "/")
	if !found || backendModel == "" {
		return modelRoute{}, false
	}
	engine := s.config.Engines[engineID]
	if engine == nil {
		return modelRoute{}, false
	}
	return modelRoute{engine: engine, engineID: engineID, backendModel: backendModel}, true
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
	route, ok := s.resolveModel(request.Model)
	if !ok {
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
	if len(s.config.AllowedRoots) > 0 {
		resolved, allowed, resolveErr := resolveAllowedCWD(cwd, s.config.AllowedRoots)
		if resolveErr != nil {
			writeError(w, http.StatusBadRequest, "invalid X-Agent-CWD: "+resolveErr.Error(), "invalid_request_error", "invalid_cwd")
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "X-Agent-CWD is outside the configured allowed roots", "invalid_request_error", "cwd_not_allowed")
			return
		}
		cwd = resolved
	}

	state := s.registry.lock(sessionID + ":" + request.Model)
	defer state.mu.Unlock()
	state.lastAccess = time.Now()

	continuation := state.cwd == cwd && state.matchesHistoryPrefix(messages)
	resume := state.process == nil && state.resumeID != "" && continuation
	reset := !continuation || (state.process == nil && !resume)
	var delta []transcriptMessage
	if reset {
		delta = messages
		stopProcess(state.process)
		s.clearSession(state, sessionID, request.Model)
	} else {
		delta = messages[state.persistedHistoryCount():]
	}
	prompt, err := turnPrompt(delta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_messages")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.config.TurnTimeout)
	defer cancel()
	first := state.process == nil
	start := func(useResume bool, turnPrompt string) error {
		// This HTTP API cannot relay interactive permission prompts. HITL off
		// lets the coding-agent runtime execute its own tools inside the chosen
		// working directory instead of silently denying every operation.
		options := map[string]string{agentrun.OptionHITL: string(agentrun.HITLOff)}
		if useResume {
			options[agentrun.OptionResumeID] = state.resumeID
		}
		if system := systemPrompt(messages); system != "" {
			options[agentrun.OptionSystemPrompt] = system
		}
		process, startErr := route.engine.Start(ctx, agentrun.Session{CWD: cwd, Model: route.backendModel, Prompt: turnPrompt, Options: options})
		if startErr != nil {
			return startErr
		}
		state.process = process
		state.cwd = cwd
		return nil
	}
	if first {
		startErr := start(resume, prompt)
		if startErr != nil && resume && isMissingNativeSession(startErr) {
			s.logError("native session unavailable; starting fresh", startErr, request.Model, sessionID)
			s.clearSession(state, sessionID, request.Model)
			resume = false
			delta = messages
			prompt, err = turnPrompt(delta)
			if err == nil {
				startErr = start(false, prompt)
			}
		}
		if startErr != nil {
			s.logError("start agent", startErr, request.Model, sessionID)
			writeError(w, http.StatusBadGateway, startErr.Error(), "server_error", "agent_start_failed")
			return
		}
	}

	completionID := randomID("chatcmpl-")
	created := time.Now().Unix()
	collector := newCollector(w, request.Stream, completionID, created, request.Model, resume)
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
	if err != nil && resume && isMissingNativeSession(err) && collector.text.Len() == 0 {
		s.logError("native session unavailable during first turn; starting fresh", err, request.Model, sessionID)
		stopProcess(state.process)
		s.clearSession(state, sessionID, request.Model)
		resume = false
		prompt, err = turnPrompt(messages)
		if err == nil {
			err = start(false, prompt)
		}
		if err == nil {
			collector.resetForFreshSession()
			err = agentrun.RunFirstTurn(ctx, state.process, prompt, collector.handle)
		}
	}
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
	state.historyCount = len(state.history)
	state.historyHash = transcriptHash(state.history)
	if collector.resumeID != "" {
		state.resumeID = collector.resumeID
	}
	state.lastAccess = time.Now()
	if state.resumeID != "" {
		err := s.registry.store.put(sessionID+":"+request.Model, persistedSession{
			ResumeID: state.resumeID, CWD: state.cwd, HistoryCount: state.historyCount, HistoryHash: state.historyHash,
		})
		if err != nil {
			s.logError("save session", err, request.Model, sessionID)
		}
	}
	if request.Stream {
		collector.finishStream()
		return
	}
	collector.writeCompletion()
}

func (s *Server) clearSession(state *sessionState, sessionID, model string) {
	state.process = nil
	state.history = nil
	state.historyCount = 0
	state.historyHash = ""
	state.resumeID = ""
	if err := s.registry.store.delete(sessionID + ":" + model); err != nil {
		s.logError("delete saved session", err, model, sessionID)
	}
}

func resolveAllowedCWD(cwd string, roots []string) (string, bool, error) {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", false, err
	}
	for _, root := range roots {
		resolvedRoot, rootErr := filepath.EvalSymlinks(root)
		if rootErr != nil {
			return "", false, fmt.Errorf("resolve allowed root %s: %w", root, rootErr)
		}
		rel, relErr := filepath.Rel(resolvedRoot, resolved)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return resolved, true, nil
		}
	}
	return resolved, false, nil
}

func isMissingNativeSession(err error) bool {
	if errors.Is(err, agentrun.ErrSessionNotFound) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"no conversation found", "session not found", "could not find session", "unknown session"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
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

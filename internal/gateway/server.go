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

// defaultStreamHeartbeat keeps streams alive through long tool-only stretches.
// Coding agents run their tools inside the backend, so a turn can produce no
// visible output for minutes; clients with stall detectors abort well before
// the turn timeout.
const defaultStreamHeartbeat = 20 * time.Second

type Config struct {
	Engines      map[string]agentrun.Engine
	ModelDetails map[string]ModelDetails
	DefaultCWD   string
	AllowedRoots []string
	APIKey       string
	TurnTimeout  time.Duration
	SessionTTL   time.Duration
	SessionStore string
	// StreamHeartbeat is how long a stream may stay silent before a keep-alive
	// delta is emitted. Zero selects the default; negative disables it.
	StreamHeartbeat time.Duration
	Logger          *slog.Logger
}

type ModelDetails struct {
	Name          string
	ContextWindow int
	MaxTokens     int
}

type effortVariant struct {
	effort  string
	modelID string
}

type modelRoute struct {
	engine         agentrun.Engine
	engineID       string
	backendModel   string
	effectiveIDs   []string
	effortVariants []effortVariant
	selectedEffort string
	details        ModelDetails
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
	if config.StreamHeartbeat == 0 {
		config.StreamHeartbeat = defaultStreamHeartbeat
	}
	if config.StreamHeartbeat < 0 {
		config.StreamHeartbeat = 0
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}

	if config.ModelDetails == nil {
		config.ModelDetails = make(map[string]ModelDetails)
	}
	if config.Engines == nil {
		config.Engines = make(map[string]agentrun.Engine)
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
		data = append(data, modelObject(model, s.config.ModelDetails[model], nil))
	}
	for _, model := range discovered {
		data = append(data, modelObject(model.id, model.details, model.efforts))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func modelObject(id string, details ModelDetails, efforts []string) map[string]any {
	name := details.Name
	if name == "" {
		name = id
	}
	result := map[string]any{
		"id": id, "object": "model", "created": 0, "owned_by": "agentrun",
		"name": name, "context_window": details.ContextWindow, "max_tokens": details.MaxTokens,
	}
	if len(efforts) > 0 {
		result["reasoning_efforts"] = efforts
		result["thinking_level_map"] = thinkingLevelMap(efforts)
	}
	return result
}

func thinkingLevelMap(efforts []string) map[string]any {
	result := make(map[string]any, len(efforts))
	for _, effort := range efforts {
		result[effort] = effort
	}
	return result
}

type discoveredModel struct {
	id      string
	details ModelDetails
	efforts []string
}

func (s *Server) discoverModels(ctx context.Context) []discoveredModel {
	for _, engineID := range s.models {
		engine := s.config.Engines[engineID]
		lister, ok := engine.(agentrun.ModelLister)
		if !ok {
			continue
		}
		discoveryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		models, err := lister.ListModels(discoveryCtx, agentrun.Session{CWD: s.config.DefaultCWD})
		cancel()
		if err != nil {
			if !errors.Is(err, agentrun.ErrModelDiscoveryUnsupported) {
				s.config.Logger.Warn("discover backend models", "engine", engineID, "error", err)
			}
			continue
		}

		base := s.config.ModelDetails[engineID]
		fresh := groupedBracketRoutes(engineID, engine, base, models)

		s.routesMu.Lock()
		for id, route := range s.routes {
			if route.engineID == engineID {
				delete(s.routes, id)
			}
		}
		for id, route := range fresh {
			s.routes[id] = route
		}
		s.routesMu.Unlock()
	}

	s.routesMu.RLock()
	discovered := make([]discoveredModel, 0, len(s.routes))
	for id, route := range s.routes {
		efforts := route.efforts()
		discovered = append(discovered, discoveredModel{id: id, details: route.details, efforts: efforts})
	}
	s.routesMu.RUnlock()
	sort.Slice(discovered, func(i, j int) bool { return discovered[i].id < discovered[j].id })
	return discovered
}

func (route modelRoute) efforts() []string {
	efforts := make([]string, 0, len(route.effortVariants))
	for _, variant := range route.effortVariants {
		efforts = append(efforts, variant.effort)
	}
	return efforts
}

func groupedBracketRoutes(engineID string, engine agentrun.Engine, base ModelDetails, models []agentrun.ModelInfo) map[string]modelRoute {
	routes := make(map[string]modelRoute)
	for _, model := range models {
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		baseID, effort, bracket := splitEffortModelID(modelID)
		if !bracket {
			baseID = modelID
		}
		id := engineID + "/" + baseID
		route := routes[id]
		if len(route.effortVariants) == 0 {
			name := strings.TrimSpace(model.Name)
			if bracket {
				name = strings.TrimSuffix(name, " ("+effort+")")
			}
			if name == "" {
				name = baseID
			}
			details := base
			details.Name = name
			if base.Name != "" {
				details.Name = base.Name + " · " + name
			}
			route = modelRoute{engine: engine, engineID: engineID, backendModel: baseID, details: details}
			if !bracket {
				route.effectiveIDs = append([]string{modelID}, model.Aliases...)
			}
		}
		if bracket {
			found := false
			for i, variant := range route.effortVariants {
				if variant.effort == effort {
					route.effortVariants[i].modelID = modelID
					found = true
					break
				}
			}
			if !found {
				route.effortVariants = append(route.effortVariants, effortVariant{effort: effort, modelID: modelID})
			}
		}
		routes[id] = route
	}
	return routes
}

func splitEffortModelID(id string) (string, string, bool) {
	if !strings.HasSuffix(id, "]") {
		return "", "", false
	}
	open := strings.LastIndexByte(id, '[')
	if open <= 0 || open == len(id)-2 {
		return "", "", false
	}
	base, effort := id[:open], id[open+1:len(id)-1]
	if strings.ContainsAny(base, "[]") || strings.ContainsAny(effort, "[]") || strings.TrimSpace(effort) == "" {
		return "", "", false
	}
	return base, effort, true
}

func (s *Server) resolveModel(id string) (modelRoute, bool) {
	if engine := s.config.Engines[id]; engine != nil {
		return modelRoute{
			engine:   engine,
			engineID: id,
			details:  s.config.ModelDetails[id],
		}, true
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
	return modelRoute{
		engine:       engine,
		engineID:     engineID,
		backendModel: backendModel,
	}, true
}

func selectReasoningEffort(route modelRoute, requested string) (modelRoute, error) {
	if len(route.effortVariants) > 0 {
		if requested == "" {
			requested = route.effortVariants[0].effort
		}
		for _, variant := range route.effortVariants {
			if variant.effort == requested {
				route.effectiveIDs = []string{route.backendModel, variant.modelID}
				route.selectedEffort = requested
				route.backendModel = variant.modelID
				return route, nil
			}
		}
		return modelRoute{}, fmt.Errorf("reasoning_effort %q is not available for this model", requested)
	}
	if requested != "" {
		return modelRoute{}, fmt.Errorf("reasoning_effort %q cannot be selected: this model has no advertised effort variants", requested)
	}
	return route, nil
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
	if route.backendModel != "" && len(route.effectiveIDs) == 0 && len(route.effortVariants) == 0 {
		s.discoverModels(r.Context())
		route, _ = s.resolveModel(request.Model)
	}
	route, err := selectReasoningEffort(route, request.ReasoningEffort)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "reasoning_effort_not_supported")
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

	stateKey := sessionID + ":" + request.Model
	if route.selectedEffort != "" {
		stateKey += ":" + route.selectedEffort
	}
	state := s.registry.lock(stateKey)
	defer state.mu.Unlock()
	state.lastAccess = time.Now()

	continuation := state.cwd == cwd && state.matchesHistoryPrefix(messages)
	resume := state.process == nil && state.resumeID != "" && continuation
	reset := !continuation || (state.process == nil && !resume)
	var delta []transcriptMessage
	if reset {
		delta = messages
		stopProcess(state.process)
		s.clearSession(state, stateKey)
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
			s.clearSession(state, stateKey)
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
	collector := newCollector(w, request.Stream, completionID, created, request.Model, resume, route.effectiveIDs)
	if request.Stream {
		if err := collector.startStream(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error", "streaming_unsupported")
			return
		}
		defer collector.startHeartbeat(s.config.StreamHeartbeat)()
	}
	run := agentrun.RunTurn
	if first {
		run = agentrun.RunFirstTurn
	}
	err = run(ctx, state.process, prompt, collector.handle)
	if err != nil && resume && isMissingNativeSession(err) && collector.text.Len() == 0 {
		s.logError("native session unavailable during first turn; starting fresh", err, request.Model, sessionID)
		stopProcess(state.process)
		s.clearSession(state, stateKey)
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
		err := s.registry.store.put(stateKey, persistedSession{
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

func (s *Server) clearSession(state *sessionState, stateKey string) {
	state.process = nil
	state.history = nil
	state.historyCount = 0
	state.historyHash = ""
	state.resumeID = ""
	if err := s.registry.store.delete(stateKey); err != nil {
		s.config.Logger.Warn("delete saved session", "error", err, "session_key", stateKey)
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

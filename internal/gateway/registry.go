package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/dmora/agentrun"
)

type sessionState struct {
	mu         sync.Mutex
	process    agentrun.Process
	history    []transcriptMessage
	cwd        string
	lastAccess time.Time
}

type registry struct {
	mu       sync.Mutex
	sessions map[string]*sessionState
	ttl      time.Duration
}

func newRegistry(ttl time.Duration) *registry {
	return &registry{sessions: make(map[string]*sessionState), ttl: ttl}
}

// lock returns the state with its mutex held. Taking the state lock while the
// registry lock is held prevents the janitor from evicting a state between
// lookup and use.
func (r *registry) lock(key string) *sessionState {
	r.mu.Lock()
	state := r.sessions[key]
	if state == nil {
		state = &sessionState{lastAccess: time.Now()}
		r.sessions[key] = state
	}
	state.mu.Lock()
	r.mu.Unlock()
	return state
}

func (r *registry) evictIdle() {
	if r.ttl <= 0 {
		return
	}
	cutoff := time.Now().Add(-r.ttl)
	var stale []*sessionState
	r.mu.Lock()
	for key, state := range r.sessions {
		if !state.mu.TryLock() {
			continue
		}
		if state.lastAccess.Before(cutoff) {
			delete(r.sessions, key)
			stale = append(stale, state)
		} else {
			state.mu.Unlock()
		}
	}
	r.mu.Unlock()
	for _, state := range stale {
		stopProcess(state.process)
		state.mu.Unlock()
	}
}

func (r *registry) close() {
	r.mu.Lock()
	states := make([]*sessionState, 0, len(r.sessions))
	for _, state := range r.sessions {
		states = append(states, state)
	}
	r.sessions = make(map[string]*sessionState)
	r.mu.Unlock()
	for _, state := range states {
		state.mu.Lock()
		stopProcess(state.process)
		state.mu.Unlock()
	}
}

func stopProcess(process agentrun.Process) {
	if process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = process.Stop(ctx)
}

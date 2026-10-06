package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/dmora/agentrun"
	"github.com/mytecor/agentrun-openai/internal/mcpbridge"
)

type mcpFacade struct {
	ctx        context.Context
	cancel     context.CancelFunc
	server     *http.Server
	descriptor agentrun.MCPServer
	token      string
	tools      []mcpbridge.Tool
	mu         sync.Mutex
	active     *activeTurn
}

func newMCPFacade(parent context.Context, tools []functionTool) (*mcpFacade, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	f := &mcpFacade{ctx: ctx, cancel: cancel, token: hex.EncodeToString(secret)}
	for _, tool := range tools {
		f.tools = append(f.tools, mcpbridge.Tool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: tool.Function.Parameters})
	}
	f.descriptor = agentrun.MCPServer{Name: "openai-functions", Command: exe, Args: []string{"__mcp-bridge", listener.Addr().String(), f.token}}
	f.server = &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = f.server.Serve(listener) }()
	return f, nil
}
func (f *mcpFacade) bind(t *activeTurn) { f.mu.Lock(); defer f.mu.Unlock(); f.active = t }
func (f *mcpFacade) abort(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active != nil {
		f.active.cancel(err)
	}
}
func (f *mcpFacade) close() {
	f.cancel()
	f.abort(fmt.Errorf("MCP session closed"))
	_ = f.server.Close()
}
func (f *mcpFacade) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+f.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/tools":
		_ = json.NewEncoder(w).Encode(f.tools)
	case "/watch":
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-f.ctx.Done():
		case <-r.Context().Done():
			f.abort(fmt.Errorf("MCP bridge disconnected"))
			f.cancel()
		}
	case "/call":
		var call mcpbridge.Call
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&call); err != nil {
			http.Error(w, "invalid tool call", 400)
			return
		}
		known := false
		for _, t := range f.tools {
			if t.Name == call.Name {
				known = true
				break
			}
		}
		if !known {
			http.Error(w, "unknown tool name", 400)
			return
		}
		f.mu.Lock()
		turn := f.active
		f.mu.Unlock()
		if turn == nil {
			http.Error(w, "no active turn", 409)
			return
		}
		if len(call.Arguments) == 0 {
			call.Arguments = json.RawMessage(`{}`)
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		result, err := turn.call(r.Context(), call.Name, call.Arguments)
		if err != nil {
			_ = json.NewEncoder(w).Encode(mcpbridge.Result{Error: err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(mcpbridge.Result{Content: result})
	default:
		http.NotFound(w, r)
	}
}

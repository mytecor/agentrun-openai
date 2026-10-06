// Package mcpbridge implements the private stdio MCP subprocess. The gateway
// owns tool execution state; this process only translates MCP to local IPC.
package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}
type Call struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type Result struct {
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

// Run serves stdio until the ACP client or the owning gateway disconnects.
// address is a numeric loopback host:port, never an arbitrary remote URL.
func Run(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("__mcp-bridge requires address and session token")
	}
	host, _, err := net.SplitHostPort(args[0])
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("bridge address must be numeric loopback")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 10 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(ctx context.Context, path string, input any) (*http.Response, error) {
		var body bytes.Buffer
		if input != nil {
			if err := json.NewEncoder(&body).Encode(input); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+args[0]+path, &body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+args[1])
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, fmt.Errorf("bridge IPC: %s", data)
		}
		return resp, nil
	}
	watch, err := request(ctx, "/watch", nil)
	if err != nil {
		return err
	}
	defer watch.Body.Close()
	watchDone := make(chan struct{})
	go func() { defer close(watchDone); _, _ = io.Copy(io.Discard, watch.Body); cancel() }()
	defer func() { cancel(); watch.Body.Close(); <-watchDone }()
	resp, err := request(ctx, "/tools", nil)
	if err != nil {
		return err
	}
	var definitions []Tool
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&definitions)
	resp.Body.Close()
	if err != nil {
		return err
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "agentrun-openai", Version: "1"}, nil)
	for _, t := range definitions {
		server.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Headers are sent immediately by the gateway; the body may wait for an
			// OpenAI tool result for the whole remaining ACP turn timeout.
			resp, err := request(ctx, "/call", Call{Name: req.Params.Name, Arguments: req.Params.Arguments})
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			var result Result
			if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); err != nil {
				return nil, err
			}
			if result.Error != "" {
				return nil, fmt.Errorf("%s", result.Error)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result.Content}}}, nil
		})
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}

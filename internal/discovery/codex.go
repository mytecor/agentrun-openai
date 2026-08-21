package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/mytecor/agentrun-openai/internal/gateway"
)

const (
	defaultContextWindow = 200_000
	defaultMaxTokens     = 32_000
)

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type modelListResult struct {
	Data []struct {
		ID          string `json:"id"`
		Model       string `json:"model"`
		DisplayName string `json:"displayName"`
		Hidden      bool   `json:"hidden"`
	} `json:"data"`
	NextCursor *string `json:"nextCursor"`
}

type lockedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.data = append(b.data, p...)
	if len(b.data) > 4096 {
		b.data = append([]byte(nil), b.data[len(b.data)-4096:]...)
	}
	b.mu.Unlock()
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// CodexModels asks the authenticated Codex app-server for the same model
// catalog used by its native picker. The subprocess is short-lived and is
// terminated as soon as all model/list pages have been read.
func CodexModels(ctx context.Context, binary string) ([]gateway.DiscoveredModel, error) {
	return codexModelsCommand(exec.CommandContext(ctx, binary, "app-server", "--stdio"))
}

func codexModelsCommand(cmd *exec.Cmd) ([]gateway.DiscoveredModel, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex discovery stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex discovery stdout: %w", err)
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex discovery: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(stdout)
	if err := encoder.Encode(map[string]any{
		"id": 1, "method": "initialize",
		"params": map[string]any{
			"clientInfo":   map[string]string{"name": "agentrun-openai", "version": "1"},
			"capabilities": map[string]bool{"experimentalApi": true},
		},
	}); err != nil {
		return nil, fmt.Errorf("initialize codex discovery: %w", err)
	}
	if _, err := readResponse(decoder, 1); err != nil {
		return nil, withStderr(err, stderr.String())
	}

	var models []gateway.DiscoveredModel
	var cursor *string
	requestID := 2
	for {
		params := map[string]any{"includeHidden": false}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		if err := encoder.Encode(map[string]any{"id": requestID, "method": "model/list", "params": params}); err != nil {
			return nil, fmt.Errorf("request codex models: %w", err)
		}
		raw, err := readResponse(decoder, requestID)
		if err != nil {
			return nil, withStderr(err, stderr.String())
		}
		var page modelListResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode codex model list: %w", err)
		}
		for _, model := range page.Data {
			backendModel := strings.TrimSpace(model.Model)
			if backendModel == "" {
				backendModel = strings.TrimSpace(model.ID)
			}
			if backendModel == "" || model.Hidden {
				continue
			}
			name := strings.TrimSpace(model.DisplayName)
			if name == "" {
				name = backendModel
			}
			models = append(models, gateway.DiscoveredModel{
				ID: "codex/" + backendModel, Engine: "codex", BackendModel: backendModel,
				Details: gateway.ModelDetails{Name: "Codex · " + name, ContextWindow: defaultContextWindow, MaxTokens: defaultMaxTokens},
			})
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		requestID++
	}
	if len(models) == 0 {
		return nil, withStderr(fmt.Errorf("codex returned an empty model catalog"), stderr.String())
	}
	return models, nil
}

func readResponse(decoder *json.Decoder, requestID int) (json.RawMessage, error) {
	for {
		var response rpcResponse
		if err := decoder.Decode(&response); err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("codex app-server closed before response %d", requestID)
			}
			return nil, fmt.Errorf("read codex response %d: %w", requestID, err)
		}
		if response.ID != requestID {
			continue
		}
		if response.Error != nil {
			return nil, fmt.Errorf("codex RPC error %d: %s", response.Error.Code, response.Error.Message)
		}
		return response.Result, nil
	}
}

func withStderr(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	if len(stderr) > 500 {
		stderr = stderr[len(stderr)-500:]
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

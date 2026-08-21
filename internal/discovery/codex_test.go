package discovery

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestCodexModels(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=TestCodexModelsHelper")
	cmd.Env = append(os.Environ(), "AGENTRUN_CODEX_DISCOVERY_HELPER=1")
	models, err := codexModelsCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if got := models[0]; got.ID != "codex/gpt-test" || got.BackendModel != "gpt-test" || got.Details.Name != "Codex · GPT Test" {
		t.Fatalf("first model = %#v", got)
	}
	if got := models[1]; got.ID != "codex/model-id-fallback" || got.BackendModel != "model-id-fallback" {
		t.Fatalf("second model = %#v", got)
	}
}

func TestCodexModelsHelper(t *testing.T) {
	if os.Getenv("AGENTRUN_CODEX_DISCOVERY_HELPER") != "1" {
		return
	}
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	var request map[string]any
	if err := decoder.Decode(&request); err != nil {
		os.Exit(2)
	}
	_ = encoder.Encode(map[string]any{"id": 1, "result": map[string]any{"userAgent": "test"}})
	if err := decoder.Decode(&request); err != nil {
		os.Exit(3)
	}
	_ = encoder.Encode(map[string]any{
		"id": 2,
		"result": map[string]any{
			"data": []map[string]any{
				{"id": "gpt-test", "model": "gpt-test", "displayName": "GPT Test", "hidden": false},
				{"id": "model-id-fallback", "model": "", "displayName": "", "hidden": false},
				{"id": "hidden", "model": "hidden", "displayName": "Hidden", "hidden": true},
			},
			"nextCursor": nil,
		},
	})
	os.Exit(0)
}

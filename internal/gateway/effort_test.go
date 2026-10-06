package gateway

import (
	"reflect"
	"testing"

	"github.com/dmora/agentrun"
)

func TestDynamicEffortCatalog(t *testing.T) {
	routes := groupedBracketRoutes("agent", nil, ModelDetails{}, []agentrun.ModelInfo{
		{ID: "model[Budget]"}, {ID: "model[Deep-v2]"}, {ID: "model[Budget]"}, {ID: "model[minimal]"},
	})
	route := routes["agent/model"]
	want := []string{"Budget", "Deep-v2", "minimal"}
	if !reflect.DeepEqual(route.efforts(), want) {
		t.Fatalf("order = %v", route.efforts())
	}
	metadata := modelObject("agent/model", ModelDetails{}, route.efforts())
	if !reflect.DeepEqual(metadata["thinking_level_map"], map[string]any{"Budget": "Budget", "Deep-v2": "Deep-v2", "minimal": "minimal"}) {
		t.Fatalf("metadata = %v", metadata)
	}
	if _, ok := metadata["default_reasoning_effort"]; ok {
		t.Fatal("invented default")
	}
	for _, effort := range append([]string{""}, want...) {
		selected, err := selectReasoningEffort(route, effort)
		if err != nil {
			t.Fatal(err)
		}
		expected := effort
		if expected == "" {
			expected = want[0]
		}
		if selected.backendModel != "model["+expected+"]" {
			t.Fatalf("selected = %+v", selected)
		}
	}
	if _, err := selectReasoningEffort(route, "budget"); err == nil {
		t.Fatal("case mismatch accepted")
	}
	if _, err := selectReasoningEffort(route, "unadvertised"); err == nil {
		t.Fatal("unadvertised effort accepted")
	}
}

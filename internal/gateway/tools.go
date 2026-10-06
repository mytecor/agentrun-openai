package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

type functionTool struct {
	Type     string             `json:"type"`
	Function functionDefinition `json:"function"`
}
type functionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}
type toolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function calledFunction `json:"function"`
}
type calledFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type toolPolicy struct{ Mode, Name string }

var functionName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// UseNumber preserves large integers in schemas and arguments. Encoding maps
// sorts object keys, so whitespace and key order do not change identity.
func canonicalJSON(raw []byte) ([]byte, error) {
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON")
	}
	return json.Marshal(value)
}
func normalizeTools(tools []functionTool, choice json.RawMessage) ([]functionTool, string, toolPolicy, error) {
	normalized := append([]functionTool(nil), tools...)
	names := map[string]bool{}
	for i := range normalized {
		t := &normalized[i]
		if t.Type != "function" || !functionName.MatchString(t.Function.Name) || names[t.Function.Name] {
			return nil, "", toolPolicy{}, fmt.Errorf("tools must contain unique function names (letters, digits, underscores or hyphens, 1–64 characters)")
		}
		names[t.Function.Name] = true
		if len(t.Function.Parameters) == 0 {
			t.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		raw, err := canonicalJSON(t.Function.Parameters)
		if err != nil || len(raw) == 0 || raw[0] != '{' {
			return nil, "", toolPolicy{}, fmt.Errorf("tool %q parameters must be a JSON Schema object", t.Function.Name)
		}
		var schema struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &schema) != nil || schema.Type != "object" {
			return nil, "", toolPolicy{}, fmt.Errorf("tool %q parameters must have type object", t.Function.Name)
		}
		t.Function.Parameters = raw
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Function.Name < normalized[j].Function.Name })
	hash := ""
	if len(normalized) > 0 {
		raw, _ := json.Marshal(normalized)
		sum := sha256.Sum256(raw)
		hash = hex.EncodeToString(sum[:])
	}
	policy := toolPolicy{Mode: "auto"}
	if len(choice) > 0 && string(choice) != "null" {
		if err := json.Unmarshal(choice, &policy.Mode); err == nil && policy.Mode == "named" {
			return nil, "", policy, fmt.Errorf("named tool_choice must use the function object form")
		} else if err != nil {
			var named struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(choice, &named) != nil || named.Type != "function" || !names[named.Function.Name] {
				return nil, "", policy, fmt.Errorf("tool_choice must name an available function")
			}
			policy = toolPolicy{Mode: "named", Name: named.Function.Name}
		}
	}
	if policy.Mode != "auto" && policy.Mode != "none" && policy.Mode != "required" && policy.Mode != "named" {
		return nil, "", policy, fmt.Errorf("unsupported tool_choice %q", policy.Mode)
	}
	if len(normalized) == 0 && (policy.Mode == "required" || policy.Mode == "named") {
		return nil, "", policy, fmt.Errorf("tool_choice requires tools")
	}
	return normalized, hash, policy, nil
}

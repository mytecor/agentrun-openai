package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

func normalizeMessages(messages []chatMessage) ([]transcriptMessage, error) {
	if len(messages) == 0 {
		return nil, errors.New("messages must contain at least one message")
	}
	result := make([]transcriptMessage, 0, len(messages))
	for i, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("messages[%d].role %q is not supported", i, message.Role)
		}
		if len(message.ToolCalls) > 0 && role != "assistant" {
			return nil, fmt.Errorf("tool_calls requires role assistant")
		}
		if (role == "tool") != (message.ToolCallID != "") {
			return nil, fmt.Errorf("role tool requires tool_call_id; other roles must omit it")
		}
		calls := append([]toolCall(nil), message.ToolCalls...)
		seen := map[string]bool{}
		for j := range calls {
			call := &calls[j]
			raw, e := canonicalJSON([]byte(call.Function.Arguments))
			if call.ID == "" || seen[call.ID] || call.Type != "function" || !functionName.MatchString(call.Function.Name) || e != nil || len(raw) == 0 || raw[0] != '{' {
				return nil, fmt.Errorf("invalid assistant tool call")
			}
			seen[call.ID] = true
			call.Function.Arguments = string(raw)
		}
		raw := message.Content
		if role == "assistant" && len(calls) > 0 && (len(raw) == 0 || string(raw) == "null") {
			raw = json.RawMessage(`""`)
		}
		content, err := textContent(raw)
		if err != nil {
			return nil, fmt.Errorf("messages[%d].content: %w", i, err)
		}
		result = append(result, transcriptMessage{Role: role, Content: content, ToolCalls: calls, ToolCallID: message.ToolCallID})
	}
	return result, nil
}

func textContent(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", errors.New("must be text")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("must be a string or an array of text parts")
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Type != "text" && part.Type != "input_text" {
			return "", fmt.Errorf("content part type %q is not supported", part.Type)
		}
		b.WriteString(part.Text)
	}
	return b.String(), nil
}

func hasPrefix(messages, prefix []transcriptMessage) bool {
	if len(messages) < len(prefix) {
		return false
	}
	for i := range prefix {
		if !reflect.DeepEqual(messages[i], prefix[i]) {
			return false
		}
	}
	return true
}

func systemPrompt(messages []transcriptMessage) string {
	var parts []string
	for _, message := range messages {
		if message.Role == "system" || message.Role == "developer" {
			parts = append(parts, message.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

func turnPrompt(messages []transcriptMessage) (string, error) {
	// Check the actual boundary before filtering native tool history. Otherwise
	// a trailing assistant tool call could accidentally resend an earlier user.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "system" || messages[i].Role == "developer" {
			continue
		}
		if messages[i].Role != "user" {
			return "", errors.New("the last non-system message must have role user")
		}
		break
	}
	filtered := make([]transcriptMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role != "system" && message.Role != "developer" && message.Role != "tool" && len(message.ToolCalls) == 0 {
			filtered = append(filtered, message)
		}
	}
	if len(filtered) == 0 {
		return "", errors.New("messages must contain a user turn")
	}
	lastUser := -1
	for i := len(filtered) - 1; i >= 0; i-- {
		if filtered[i].Role == "user" {
			lastUser = i
			break
		}
	}
	if lastUser < 0 || lastUser != len(filtered)-1 {
		return "", errors.New("the last non-system message must have role user")
	}
	if len(filtered) == 1 {
		if strings.TrimSpace(filtered[0].Content) == "" {
			return "", errors.New("the user turn must not be empty")
		}
		return filtered[0].Content, nil
	}
	var b strings.Builder
	b.WriteString("Conversation context:\n")
	for _, message := range filtered {
		b.WriteString("\n[")
		b.WriteString(message.Role)
		b.WriteString("]\n")
		b.WriteString(message.Content)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

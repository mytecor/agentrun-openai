package gateway

import "encoding/json"

type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	Stream          bool          `json:"stream"`
	SessionID       string        `json:"session_id,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type transcriptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type apiErrorEnvelope struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

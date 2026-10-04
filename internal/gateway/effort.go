package gateway

import (
	"errors"
	"fmt"
	"strings"
)

// EffortFormat defines how model names and reasoning efforts are handled for a backend.
type EffortFormat string

const (
	// EffortFormatNone treats model names as literal 1:1 IDs without bracket effort parsing.
	EffortFormatNone EffortFormat = "none"

	// EffortFormatBracket parses bracketed effort variants (e.g. "model[effort]")
	// and aggregates them into base models with selectable reasoning effort.
	EffortFormatBracket EffortFormat = "bracket"
)

// ParseEffortFormat parses an effort format name.
// Supports "none", "bracket" (and aliases "brackets", "codex").
func ParseEffortFormat(name string) (EffortFormat, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "none":
		return EffortFormatNone, nil
	case "bracket", "brackets", "codex":
		return EffortFormatBracket, nil
	default:
		return "", fmt.Errorf("unknown effort format %q: expected 'bracket' (or 'codex') or 'none'", name)
	}
}

// EffortFormatConfig holds parsed effort format mapping per backend and an optional global default.
type EffortFormatConfig struct {
	PerBackend map[string]EffortFormat
	Default    EffortFormat
}

// ParseEffortFormatSpecs parses a list of effort format specifications.
// Supported syntax:
//   - "id=format" (e.g. "codex=bracket", "codex=codex", "pi=none")
//   - "id" (if matches "codex", maps backend "codex" to bracket format; if matches a known format, sets Default)
//   - "bracket", "none", "codex" without '=': sets global default, and for "codex" also maps "codex" backend.
func ParseEffortFormatSpecs(specs []string) (EffortFormatConfig, error) {
	config := EffortFormatConfig{
		PerBackend: make(map[string]EffortFormat),
	}
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		if id, formatStr, ok := strings.Cut(spec, "="); ok {
			id = strings.TrimSpace(id)
			if id == "" {
				return EffortFormatConfig{}, errors.New("backend ID in effort format spec must not be empty")
			}
			format, err := ParseEffortFormat(formatStr)
			if err != nil {
				return EffortFormatConfig{}, fmt.Errorf("effort format for %q: %w", id, err)
			}
			config.PerBackend[id] = format
			continue
		}

		// Spec without '=': could be a format name or backend ID shorthand
		format, err := ParseEffortFormat(spec)
		if err == nil {
			if strings.EqualFold(spec, "codex") {
				config.PerBackend["codex"] = EffortFormatBracket
				if config.Default == "" {
					config.Default = EffortFormatBracket
				}
			} else {
				config.Default = format
			}
			continue
		}

		// Non-format string without '=': treat as backend ID that should use bracket format
		config.PerBackend[spec] = EffortFormatBracket
	}
	return config, nil
}

// SplitEffortFormatList splits an environment variable value by newline, semicolon, or comma.
func SplitEffortFormatList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var result []string
	for _, entry := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '\n' || r == ';' || r == ','
	}) {
		if entry = strings.TrimSpace(entry); entry != "" {
			result = append(result, entry)
		}
	}
	return result
}

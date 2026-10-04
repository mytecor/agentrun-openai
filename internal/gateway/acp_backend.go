package gateway

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/dmora/agentrun/engine/acp"
)

// ACPBackendConfig configures a generic ACP backend.
type ACPBackendConfig struct {
	ID     string
	Binary string
	Args   []string
}

// NewEngine constructs a new acp.Engine for this configuration.
func (c ACPBackendConfig) NewEngine(stderr io.Writer) *acp.Engine {
	if stderr == nil {
		stderr = os.Stderr
	}
	return acp.NewEngine(
		acp.WithBinary(c.Binary),
		acp.WithArgs(c.Args...),
		acp.WithStderrWriter(stderr),
	)
}

func validateACPID(id string) error {
	if id == "" {
		return errors.New("acp backend ID must not be empty")
	}
	if strings.Contains(id, "/") {
		return fmt.Errorf("acp backend ID %q must not contain '/'", id)
	}
	if strings.ContainsAny(id, " \t\r\n:") {
		return fmt.Errorf("acp backend ID %q contains invalid characters", id)
	}
	return nil
}

// Supported formats:
//   - Standard: id=command [args...]
//     e.g. "pi=pi-acp", "opencode=opencode acp", "gemini=gemini --experimental-acp"
//   - Structured: id=...,binary=...,args=...
func ParseACPBackend(spec string) (ACPBackendConfig, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ACPBackendConfig{}, errors.New("acp backend specification must not be empty")
	}

	// Check for structured key-value syntax: id=...,binary=...,args=...
	if strings.HasPrefix(spec, "id=") && (strings.Contains(spec, ",binary=") || strings.Contains(spec, ",bin=")) {
		return parseStructuredACPBackend(spec)
	}

	id, cmd, ok := strings.Cut(spec, "=")
	if !ok {
		return ACPBackendConfig{}, fmt.Errorf("invalid acp backend specification %q: expected id=command [args...]", spec)
	}
	id = strings.TrimSpace(id)
	if err := validateACPID(id); err != nil {
		return ACPBackendConfig{}, err
	}

	cmd = strings.TrimSpace(cmd)
	// If the entire command is enclosed in quotes (e.g. opencode="opencode acp" passed literally), unwrap them.
	if len(cmd) >= 2 && ((cmd[0] == '"' && cmd[len(cmd)-1] == '"') || (cmd[0] == '\'' && cmd[len(cmd)-1] == '\'')) {
		cmd = strings.TrimSpace(cmd[1 : len(cmd)-1])
	}

	tokens, err := splitCommandLine(cmd)
	if err != nil {
		return ACPBackendConfig{}, fmt.Errorf("acp backend %q: %w", id, err)
	}
	if len(tokens) == 0 {
		return ACPBackendConfig{}, fmt.Errorf("acp backend %q: binary must not be empty", id)
	}

	// If there were no spaces or quotes in the command string, but commas were used (e.g. opencode,acp),
	// support comma separation for arguments.
	if len(tokens) == 1 && strings.Contains(tokens[0], ",") && !strings.ContainsAny(cmd, " \t\"'") {
		commaParts := strings.Split(tokens[0], ",")
		var cleaned []string
		for _, part := range commaParts {
			if part = strings.TrimSpace(part); part != "" {
				cleaned = append(cleaned, part)
			}
		}
		if len(cleaned) > 0 {
			tokens = cleaned
		}
	}

	binary := strings.TrimSpace(tokens[0])
	if binary == "" {
		return ACPBackendConfig{}, fmt.Errorf("acp backend %q: binary must not be empty", id)
	}

	var args []string
	if len(tokens) > 1 {
		args = tokens[1:]
	}

	return ACPBackendConfig{
		ID:     id,
		Binary: binary,
		Args:   args,
	}, nil
}

func parseStructuredACPBackend(spec string) (ACPBackendConfig, error) {
	parts := strings.Split(spec, ",")
	var id, binary string
	var args []string

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return ACPBackendConfig{}, fmt.Errorf("invalid acp key-value pair %q in %q", part, spec)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "id":
			id = v
		case "binary", "bin":
			binary = v
		case "args":
			parsedArgs, err := splitCommandLine(v)
			if err != nil {
				return ACPBackendConfig{}, fmt.Errorf("acp backend args in %q: %w", spec, err)
			}
			args = parsedArgs
		default:
			return ACPBackendConfig{}, fmt.Errorf("unknown acp key %q in %q", k, spec)
		}
	}

	if err := validateACPID(id); err != nil {
		return ACPBackendConfig{}, err
	}
	if binary == "" {
		return ACPBackendConfig{}, fmt.Errorf("acp backend %q: binary must not be empty", id)
	}
	return ACPBackendConfig{
		ID:     id,
		Binary: binary,
		Args:   args,
	}, nil
}

// ParseACPBackend parses a single ACP backend specification.

// ParseACPBackends parses a list of ACP backend specifications and validates for duplicates.
func ParseACPBackends(specs []string) ([]ACPBackendConfig, error) {
	var backends []ACPBackendConfig
	seen := make(map[string]bool)

	for _, spec := range specs {
		b, err := ParseACPBackend(spec)
		if err != nil {
			return nil, err
		}
		if seen[b.ID] {
			return nil, fmt.Errorf("duplicate acp backend ID %q", b.ID)
		}
		seen[b.ID] = true
		backends = append(backends, b)
	}
	return backends, nil
}

func splitCommandLine(cmd string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	var inQuote rune
	var escaped bool
	var hasToken bool

	for _, r := range cmd {
		if escaped {
			current.WriteRune(r)
			escaped = false
			hasToken = true
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if inQuote != 0 {
			if r == inQuote {
				inQuote = 0
			} else {
				current.WriteRune(r)
			}
			hasToken = true
			continue
		}
		if r == '"' || r == '\'' {
			inQuote = r
			hasToken = true
			continue
		}
		if unicode.IsSpace(r) {
			if hasToken {
				tokens = append(tokens, current.String())
				current.Reset()
				hasToken = false
			}
			continue
		}
		current.WriteRune(r)
		hasToken = true
	}

	if escaped {
		return nil, errors.New("incomplete escape sequence in command line")
	}
	if inQuote != 0 {
		return nil, fmt.Errorf("unterminated quote %c in command line", inQuote)
	}
	if hasToken {
		tokens = append(tokens, current.String())
	}
	return tokens, nil
}

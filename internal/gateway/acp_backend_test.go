package gateway

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseACPBackendSingle(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    ACPBackendConfig
		wantErr bool
	}{
		{
			name: "simple without args",
			spec: "pi=pi-acp",
			want: ACPBackendConfig{ID: "pi", Binary: "pi-acp", Args: nil},
		},
		{
			name: "command with single arg",
			spec: "opencode=opencode acp",
			want: ACPBackendConfig{ID: "opencode", Binary: "opencode", Args: []string{"acp"}},
		},
		{
			name: "command with flag arg",
			spec: "gemini=gemini --experimental-acp",
			want: ACPBackendConfig{ID: "gemini", Binary: "gemini", Args: []string{"--experimental-acp"}},
		},
		{
			name: "quoted args with spaces",
			spec: `custom="my-bin --opt 'val with space'"`,
			want: ACPBackendConfig{ID: "custom", Binary: "my-bin", Args: []string{"--opt", "val with space"}},
		},
		{
			name: "structured key-value format",
			spec: "id=custom,binary=my-bin,args=--opt 'val with space'",
			want: ACPBackendConfig{ID: "custom", Binary: "my-bin", Args: []string{"--opt", "val with space"}},
		},
		{
			name: "comma separated command format",
			spec: "opencode=opencode,acp",
			want: ACPBackendConfig{ID: "opencode", Binary: "opencode", Args: []string{"acp"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseACPBackend(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseACPBackend(%q) error = %v, wantErr %v", tt.spec, err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseACPBackend(%q) = %#v, want %#v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestParseACPBackendsMultiple(t *testing.T) {
	specs := []string{
		"pi=pi-acp",
		"opencode=opencode acp",
		"gemini=gemini --experimental-acp",
	}
	backends, err := ParseACPBackends(specs)
	if err != nil {
		t.Fatalf("ParseACPBackends unexpected error: %v", err)
	}
	if len(backends) != 3 {
		t.Fatalf("len(backends) = %d, want 3", len(backends))
	}

	expected := []ACPBackendConfig{
		{ID: "pi", Binary: "pi-acp", Args: nil},
		{ID: "opencode", Binary: "opencode", Args: []string{"acp"}},
		{ID: "gemini", Binary: "gemini", Args: []string{"--experimental-acp"}},
	}
	for i, exp := range expected {
		if !reflect.DeepEqual(backends[i], exp) {
			t.Errorf("backends[%d] = %#v, want %#v", i, backends[i], exp)
		}
	}
}

func TestParseACPBackendDuplicateID(t *testing.T) {
	specs := []string{
		"pi=pi-acp",
		"pi=other-pi-acp",
	}
	_, err := ParseACPBackends(specs)
	if err == nil {
		t.Fatal("expected error on duplicate ID, got nil")
	}
	if !strings.Contains(err.Error(), `duplicate acp backend ID "pi"`) {
		t.Fatalf("error = %q, want duplicate ID message", err.Error())
	}
}

func TestParseACPBackendAllowsStandardNames(t *testing.T) {
	valid := []string{
		"codex=custom-codex",
		"claude-code=custom-claude",
		"agy=custom-agy",
		"pi=pi-acp",
	}
	for _, spec := range valid {
		b, err := ParseACPBackend(spec)
		if err != nil {
			t.Errorf("unexpected error for %q: %v", spec, err)
		}
		if b.ID == "" || b.Binary == "" {
			t.Errorf("backend not parsed correctly: %#v", b)
		}
	}
}

func TestParseACPBackendValidation(t *testing.T) {
	invalidSpecs := []struct {
		spec    string
		wantErr string
	}{
		{"", "must not be empty"},
		{"=binary", "must not be empty"},
		{"pi", "expected id=command"},
		{"foo/bar=binary", "must not contain '/'"},
		{"foo:bar=binary", "contains invalid characters"},
		{"foo bar=binary", "contains invalid characters"},
		{"pi=", "binary must not be empty"},
		{`pi=bin "unclosed`, "unterminated quote"},
	}

	for _, tt := range invalidSpecs {
		_, err := ParseACPBackend(tt.spec)
		if err == nil {
			t.Errorf("expected error for invalid spec %q, got nil", tt.spec)
		} else if !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("ParseACPBackend(%q) error = %q, want substr %q", tt.spec, err.Error(), tt.wantErr)
		}
	}
}

func TestACPBinaryArgsDirectExecNoShell(t *testing.T) {
	// Shell metacharacters must be preserved as literal argv tokens without shell evaluation.
	spec := `custom=my-bin arg;rm -rf / $VAR && true 'foo|bar'`
	cfg, err := ParseACPBackend(spec)
	if err != nil {
		t.Fatalf("ParseACPBackend unexpected error: %v", err)
	}
	if cfg.Binary != "my-bin" {
		t.Fatalf("cfg.Binary = %q, want my-bin", cfg.Binary)
	}
	wantArgs := []string{"arg;rm", "-rf", "/", "$VAR", "&&", "true", "foo|bar"}
	if !reflect.DeepEqual(cfg.Args, wantArgs) {
		t.Fatalf("cfg.Args = %#v, want %#v", cfg.Args, wantArgs)
	}
}

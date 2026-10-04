package gateway

import (
	"reflect"
	"testing"
)

func TestParseEffortFormat(t *testing.T) {
	tests := []struct {
		input   string
		want    EffortFormat
		wantErr bool
	}{
		{"", EffortFormatNone, false},
		{"none", EffortFormatNone, false},
		{"bracket", EffortFormatBracket, false},
		{"BRACKET", EffortFormatBracket, false},
		{"brackets", EffortFormatBracket, false},
		{"codex", EffortFormatBracket, false},
		{"unknown", "", true},
	}

	for _, tt := range tests {
		got, err := ParseEffortFormat(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseEffortFormat(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("ParseEffortFormat(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseEffortFormatSpecs(t *testing.T) {
	tests := []struct {
		name    string
		specs   []string
		want    EffortFormatConfig
		wantErr bool
	}{
		{
			name:  "empty",
			specs: nil,
			want: EffortFormatConfig{
				PerBackend: map[string]EffortFormat{},
			},
		},
		{
			name:  "explicit backend formats",
			specs: []string{"codex=bracket", "pi=none"},
			want: EffortFormatConfig{
				PerBackend: map[string]EffortFormat{
					"codex": EffortFormatBracket,
					"pi":    EffortFormatNone,
				},
			},
		},
		{
			name:  "codex shorthand flag",
			specs: []string{"codex"},
			want: EffortFormatConfig{
				PerBackend: map[string]EffortFormat{
					"codex": EffortFormatBracket,
				},
				Default: EffortFormatBracket,
			},
		},
		{
			name:  "global default format",
			specs: []string{"bracket"},
			want: EffortFormatConfig{
				PerBackend: map[string]EffortFormat{},
				Default:    EffortFormatBracket,
			},
		},
		{
			name:  "arbitrary backend shorthand defaults to bracket",
			specs: []string{"my-agent"},
			want: EffortFormatConfig{
				PerBackend: map[string]EffortFormat{
					"my-agent": EffortFormatBracket,
				},
			},
		},
		{
			name:    "invalid format name in spec",
			specs:   []string{"codex=invalid"},
			wantErr: true,
		},
		{
			name:    "empty id in spec",
			specs:   []string{"=bracket"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseEffortFormatSpecs(tt.specs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseEffortFormatSpecs(%v) error = %v, wantErr %v", tt.specs, err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseEffortFormatSpecs(%v) = %#v, want %#v", tt.specs, got, tt.want)
			}
		})
	}
}

func TestSplitEffortFormatList(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"codex=bracket", []string{"codex=bracket"}},
		{"codex=bracket; pi=none", []string{"codex=bracket", "pi=none"}},
		{"codex=bracket,pi=none", []string{"codex=bracket", "pi=none"}},
		{"codex=bracket\npi=none\n", []string{"codex=bracket", "pi=none"}},
	}

	for _, tt := range tests {
		got := SplitEffortFormatList(tt.input)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitEffortFormatList(%q) = %#v, want %#v", tt.input, got, tt.want)
		}
	}
}

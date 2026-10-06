package main

import (
	"reflect"
	"testing"
)

func TestSplitACPList(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "empty",
			input: "",
			want:  nil,
		},
		{
			name:  "single",
			input: "pi=pi-acp",
			want:  []string{"pi=pi-acp"},
		},
		{
			name:  "semicolon separated",
			input: "pi=pi-acp; opencode=opencode acp; gemini=gemini --experimental-acp",
			want:  []string{"pi=pi-acp", "opencode=opencode acp", "gemini=gemini --experimental-acp"},
		},
		{
			name:  "newline separated",
			input: "pi=pi-acp\nopencode=opencode acp\n",
			want:  []string{"pi=pi-acp", "opencode=opencode acp"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitACPList(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitACPList(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestACPFlagList(t *testing.T) {
	var list acpFlagList
	if err := list.Set("pi=pi-acp"); err != nil {
		t.Fatal(err)
	}
	if err := list.Set("opencode=opencode acp"); err != nil {
		t.Fatal(err)
	}
	want := []string{"pi=pi-acp", "opencode=opencode acp"}
	if !reflect.DeepEqual([]string(list), want) {
		t.Fatalf("list = %#v, want %#v", list, want)
	}

	if err := list.Set("  "); err == nil {
		t.Fatal("expected error on empty flag value")
	}
}

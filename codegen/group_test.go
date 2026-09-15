package codegen

import "testing"

func TestSanitizeGroup(t *testing.T) {
	tests := []struct {
		in, pkg, want string
	}{
		{"user", "graph", "user"},
		{"User", "graph", "user"},
		{"schema", "graph", "types"},
		{"model", "graph", "modelgrp"},
		{"graph", "graph", "graphgrp"},
		{"internal", "graph", "internalgrp"},
		{"", "graph", "types"},
	}
	for _, tt := range tests {
		if got := sanitizeGroup(tt.in, tt.pkg); got != tt.want {
			t.Errorf("sanitizeGroup(%q, %q) = %q, want %q", tt.in, tt.pkg, got, tt.want)
		}
	}
	if got := sanitizeGroup(fileStem("user.graphql"), "graph"); got != "user" {
		t.Errorf("file stem user.graphql: got %q", got)
	}
}

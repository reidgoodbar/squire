package main

import (
	"strings"
	"testing"
)

func TestUsageTextDocumentsKernelContract(t *testing.T) {
	text := usageText()
	for _, want := range []string{
		"Squire Kernel v1",
		"Agent chooses. Squire serves.",
		"Native fallback always exists.",
		"Runtime decisions are replay or native.",
		"squire kernel maintain --background",
		"squire kernel run -- git rev-parse HEAD",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("usage text missing %q:\n%s", want, text)
		}
	}
}

func TestHelpTextForArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "global long help", args: []string{"--help"}, want: "usage:"},
		{name: "global help topic", args: []string{"help"}, want: "usage:"},
		{name: "kernel run topic", args: []string{"kernel", "run", "--help"}, want: "The \"--\" delimiter is"},
		{name: "kernel maintain topic", args: []string{"help", "kernel", "maintain"}, want: "resident maintainer"},
		{name: "boost topic", args: []string{"boost", "-h"}, want: "no broad Codex speedup claim"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, ok := helpTextForArgs(tt.args)
			if !ok {
				t.Fatalf("helpTextForArgs(%v) did not detect help", tt.args)
			}
			if !strings.Contains(text, tt.want) {
				t.Fatalf("help text missing %q:\n%s", tt.want, text)
			}
		})
	}
}

func TestHelpTextDoesNotInterceptCommandHelpAfterDelimiter(t *testing.T) {
	if text, ok := helpTextForArgs([]string{"kernel", "run", "--", "git", "--help"}); ok {
		t.Fatalf("help intercepted command argv after --:\n%s", text)
	}
}

func TestCommandAfterDelimiter(t *testing.T) {
	argv, err := commandAfterDelimiter("squire kernel run", []string{"--", "git", "status", "--short"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv, " ") != "git status --short" {
		t.Fatalf("argv = %q", strings.Join(argv, " "))
	}

	for _, args := range [][]string{
		nil,
		{"git", "status"},
		{"--"},
	} {
		if _, err := commandAfterDelimiter("squire kernel run", args); err == nil {
			t.Fatalf("commandAfterDelimiter(%v) returned nil error", args)
		}
	}
}

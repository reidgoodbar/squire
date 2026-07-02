package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveSquireBinaryUsesOverride(t *testing.T) {
	t.Setenv("SQUIRE_BIN", "/tmp/custom-squire")
	got, err := resolveSquireBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/custom-squire" {
		t.Fatalf("resolveSquireBinary = %q", got)
	}
}

func TestIsExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "squire")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && isExecutable(path) {
		t.Fatal("0644 file should not be executable")
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if !isExecutable(path) {
		t.Fatal("0755 file should be executable")
	}
	if isExecutable(dir) {
		t.Fatal("directory should not be executable")
	}
}

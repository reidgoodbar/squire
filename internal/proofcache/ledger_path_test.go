package proofcache

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultStoreRootUsesCanonicalGitStateDirectory(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	gitDir := runNative(ctx, repo, []string{"git", "rev-parse", "--absolute-git-dir"})
	if gitDir.ExitCode != 0 {
		t.Fatalf("resolve git dir: %s", gitDir.Stderr)
	}
	want := filepath.Join(strings.TrimSpace(string(gitDir.Stdout)), "squire", "state")
	if got := DefaultStoreRoot(repo); got != want {
		t.Fatalf("DefaultStoreRoot = %q, want %q", got, want)
	}
}

func TestDefaultStoreRootUsesCanonicalNonGitStateDirectory(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, ".squire", "state")
	if got := DefaultStoreRoot(dir); got != want {
		t.Fatalf("DefaultStoreRoot = %q, want %q", got, want)
	}
}

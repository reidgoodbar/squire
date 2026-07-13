package proofcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func makeBenchRepo(ctx context.Context) (string, func(), error) {
	dir, err := os.MkdirTemp("", "squire-bench-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	steps := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "squire@example.invalid"},
		{"git", "config", "user.name", "Squire"},
	}
	for _, step := range steps {
		if res := runNative(ctx, dir, step); res.ExitCode != 0 {
			cleanup()
			return "", func() {}, fmt.Errorf("%s failed: %s", displayCommand(step), string(res.Stderr))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("first\n"), 0o600); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if res := runNative(ctx, dir, []string{"git", "add", "file.txt"}); res.ExitCode != 0 {
		cleanup()
		return "", func() {}, fmt.Errorf("git add failed: %s", string(res.Stderr))
	}
	if res := runNative(ctx, dir, []string{"git", "commit", "-m", "initial"}); res.ExitCode != 0 {
		cleanup()
		return "", func() {}, fmt.Errorf("git commit failed: %s", string(res.Stderr))
	}
	return dir, cleanup, nil
}

func benchCommit(ctx context.Context, dir, msg string) error {
	path := filepath.Join(dir, "file.txt")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s\n", msg); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if res := runNative(ctx, dir, []string{"git", "add", "file.txt"}); res.ExitCode != 0 {
		return fmt.Errorf("git add failed: %s", string(res.Stderr))
	}
	if res := runNative(ctx, dir, []string{"git", "commit", "-m", msg}); res.ExitCode != 0 {
		return fmt.Errorf("git commit failed: %s", string(res.Stderr))
	}
	return nil
}

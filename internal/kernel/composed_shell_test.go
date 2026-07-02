package kernel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestComposedShellFixedRgSourceReplayMatchesNative(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg command unavailable")
	}
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("src", "search.txt")
	if err := os.WriteFile(filepath.Join(repo, rel), []byte("alpha\nbeta\ngamma\nbetamax\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", rel}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare file")
	}

	script := "rg -F beta src/search.txt | head -n 1"
	res, ok := k.ReplayComposedShell(ctx, "test", repo, script)
	if !ok || res.Mode != ModeReplay {
		t.Fatalf("composed shell did not replay: ok=%v mode=%s diagnostics=%v", ok, res.Mode, res.Diagnostics)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"sh", "-c", script}))
}

func TestComposedShellRejectsNonFixedRgSource(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("src", "search.txt")
	if err := os.WriteFile(filepath.Join(repo, rel), []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", rel}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare file")
	}

	if _, ok := k.ReplayComposedShell(ctx, "test", repo, "rg beta src/search.txt | head -n 1"); ok {
		t.Fatalf("non-fixed rg source replayed")
	}
}

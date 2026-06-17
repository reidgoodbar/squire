package kernel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Tests that once an eligible workspace file has been prewarmed for an
// exact sed window, other bounded sed windows and a full `cat` can be
// replayed without prewarming those exact ranges.
func TestPrewarmedFileReplaysOtherSedRangesAndCat(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)

	fpath := filepath.Join("src", "file.ts")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(filepath.Join(repo, fpath), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)

	count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "2,3p", fpath})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare any windows")
	}

	argv := []string{"sed", "-n", "1,2p", fpath}
	res := New(storeRoot).Run(ctx, "test", repo, argv)
	if res.Mode != ModeReplay {
		t.Fatalf("sed 1,2p mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))

	argvCat := []string{"cat", fpath}
	resCat := New(storeRoot).Run(ctx, "test", repo, argvCat)
	if resCat.Mode != ModeReplay {
		t.Fatalf("cat mode = %s, want replay; diagnostics=%v", resCat.Mode, resCat.Diagnostics)
	}
	assertSameResult(t, resCat.Stdout, resCat.Stderr, resCat.ExitCode, runNative(ctx, repo, argvCat))
}

func TestPrewarmSingleWindowThenDifferentWindowAndCat(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)

	fpath := filepath.Join("lib", "thing.ts")
	if err := os.MkdirAll(filepath.Join(repo, "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "a\nb\nc\nd\ne\nf\n"
	if err := os.WriteFile(filepath.Join(repo, fpath), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))

	count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "4,5p", fpath})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare any windows")
	}

	argv := []string{"sed", "-n", "2,4p", fpath}
	res := k.Run(ctx, "test", repo, argv)
	if res.Mode != ModeReplay {
		t.Fatalf("sed 2,4p mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))

	argvCat := []string{"cat", fpath}
	resCat := k.Run(ctx, "test", repo, argvCat)
	if resCat.Mode != ModeReplay {
		t.Fatalf("cat mode = %s, want replay; diagnostics=%v", resCat.Mode, resCat.Diagnostics)
	}
	assertSameResult(t, resCat.Stdout, resCat.Stderr, resCat.ExitCode, runNative(ctx, repo, argvCat))
}

func TestWarmFileSedReplayMatchesNativeWithoutFinalNewline(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	fpath := filepath.Join("src", "no-newline.ts")
	if err := os.WriteFile(filepath.Join(repo, fpath), []byte("one\ntwo\nthree"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,2p", fpath}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare any windows")
	}

	argv := []string{"sed", "-n", "3,3p", fpath}
	res := k.Run(ctx, "test", repo, argv)
	if res.Mode != ModeReplay {
		t.Fatalf("sed 3,3p mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
}

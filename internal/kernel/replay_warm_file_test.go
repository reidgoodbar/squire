package kernel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
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

func TestWarmFileFixedGrepReplayMatchesNative(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("src", "grep.txt")
	if err := os.WriteFile(filepath.Join(repo, rel), []byte("alpha\nbeta\ngamma\nbetamax"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", rel}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare file")
	}

	for _, argv := range [][]string{
		{"grep", "-F", "beta", rel},
		{"grep", "-q", "-F", "beta", rel},
		{"grep", "-F", "-q", "beta", rel},
		{"grep", "-q", "-F", "missing", rel},
	} {
		t.Run(displayCommand(argv), func(t *testing.T) {
			res := k.Run(ctx, "test", repo, argv)
			if res.Mode != ModeReplay {
				t.Fatalf("mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
			}
			assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
		})
	}
}

func TestWarmFileFixedRgReplayMatchesNative(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg command unavailable")
	}
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("src", "search.txt")
	if err := os.WriteFile(filepath.Join(repo, rel), []byte("alpha\nbeta\ngamma\nbetamax"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", rel}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare file")
	}

	for _, argv := range [][]string{
		{"rg", "-F", "beta", rel},
		{"rg", "-n", "-F", "beta", rel},
		{"rg", "--line-number", "--fixed-strings", "beta", rel},
		{"rg", "-q", "-F", "beta", rel},
		{"rg", "-q", "-F", "missing", rel},
	} {
		t.Run(displayCommand(argv), func(t *testing.T) {
			res := k.Run(ctx, "test", repo, argv)
			if res.Mode != ModeReplay {
				t.Fatalf("mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
			}
			assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
		})
	}
}

func TestWarmFileFixedRgDoesNotReplayRegexOrRecursiveSearch(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("src", "search.txt")
	if err := os.WriteFile(filepath.Join(repo, rel), []byte("alpha\nbeta\ngamma\nbetamax"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", rel}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare file")
	}

	for _, argv := range [][]string{
		{"rg", "beta", rel},
		{"rg", "-F", "beta", "src"},
		{"rg", "-F", "beta", rel, "README.md"},
		{"rg", "-F", "-beta", rel},
	} {
		t.Run(displayCommand(argv), func(t *testing.T) {
			res := k.Run(ctx, "test", repo, argv)
			if res.Mode == ModeReplay {
				t.Fatalf("unsafe rg form replayed: argv=%v proof=%+v", argv, res.Proof)
			}
		})
	}
}

func TestFileTypeReplayMatchesNative(t *testing.T) {
	if _, err := exec.LookPath("file"); err != nil {
		t.Skip("file command unavailable")
	}
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("src", "component.ts")
	if err := os.WriteFile(filepath.Join(repo, rel), []byte("export const value = 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	argv := []string{"file", rel}
	res := k.Run(ctx, "test", repo, argv)
	if res.Mode != ModeReplay {
		t.Fatalf("mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
}

func TestPrintenvReplayInvalidatesOnValueChange(t *testing.T) {
	if _, err := exec.LookPath("printenv"); err != nil {
		t.Skip("printenv command unavailable")
	}
	ctx := context.Background()
	repo := testRepo(t, ctx)
	t.Setenv("SQUIRE_TEST_PRINTENV", "one")

	k := New(DefaultStoreRoot(repo))
	if err := k.Store.Init(); err != nil {
		t.Fatal(err)
	}
	ledger, err := k.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	ws := k.Oracle.ShadowSnapshot(ctx, repo)
	var phases PhaseTimings
	argv := []string{"printenv", "SQUIRE_TEST_PRINTENV"}
	if !k.prewarmProofGatedCandidate(ctx, repo, "test", argv, ws, ledger, &phases, "test printenv warm") {
		t.Fatalf("printenv candidate did not prewarm")
	}
	if err := k.Store.Save(ledger); err != nil {
		t.Fatal(err)
	}
	k.hydratePreparedReplayCache(ledger, k.Store.Signal(), &phases)

	res := k.Run(ctx, "test", repo, argv)
	if res.Mode != ModeReplay {
		t.Fatalf("mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))

	t.Setenv("SQUIRE_TEST_PRINTENV", "two")
	after := k.Run(ctx, "test", repo, argv)
	if after.Mode == ModeReplay {
		t.Fatalf("printenv replayed after env value changed")
	}
	assertSameResult(t, after.Stdout, after.Stderr, after.ExitCode, runNative(ctx, repo, argv))
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

func TestWarmFileHeadTailReplayMatchesNative(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	fpath := filepath.Join("src", "window.ts")
	content := []byte("one\ntwo\nthree\nfour\nfive\nsix\n")
	if err := os.WriteFile(filepath.Join(repo, fpath), content, 0o600); err != nil {
		t.Fatal(err)
	}
	noNewline := filepath.Join("src", "partial.ts")
	if err := os.WriteFile(filepath.Join(repo, noNewline), []byte("alpha\nbeta\ngamma"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", fpath}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare any windows")
	}
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", noNewline}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare partial file")
	}

	for _, argv := range [][]string{
		{"head", fpath},
		{"head", "-n", "3", fpath},
		{"head", "-3", fpath},
		{"tail", fpath},
		{"tail", "-n", "3", fpath},
		{"tail", "-3", fpath},
		{"head", "-n", "2", noNewline},
		{"tail", "-n", "2", noNewline},
	} {
		t.Run(displayCommand(argv), func(t *testing.T) {
			res := k.Run(ctx, "test", repo, argv)
			if res.Mode != ModeReplay {
				t.Fatalf("mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
			}
			assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
		})
	}
}

func TestWarmFileHeadTailDoNotReplayAfterFileChange(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("src", "changing.ts")
	path := filepath.Join(repo, rel)
	if err := os.WriteFile(path, []byte("old1\nold2\nold3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", rel}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare file")
	}
	before := k.Run(ctx, "test", repo, []string{"tail", "-n", "2", rel})
	if before.Mode != ModeReplay {
		t.Fatalf("before mode = %s, want replay", before.Mode)
	}
	if err := os.WriteFile(path, []byte("new1\nnew2\nnew3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := k.Run(ctx, "test", repo, []string{"tail", "-n", "2", rel})
	if after.Mode == ModeReplay {
		t.Fatalf("tail replayed after file content changed")
	}
	assertSameResult(t, after.Stdout, after.Stderr, after.ExitCode, runNative(ctx, repo, []string{"tail", "-n", "2", rel}))
}

func TestWarmFileDoesNotReplaySymlinkOutsideWorkspace(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "outside.ts")
	if err := os.WriteFile(outsideFile, []byte("export const secret = 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(repo, "src", "outside.ts")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	k := New(DefaultStoreRoot(repo))
	k.mu.Lock()
	k.asyncForegroundObserve = true
	k.mu.Unlock()
	defer k.WaitForegroundObservations()
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", "src/outside.ts"}); err != nil {
		t.Fatal(err)
	} else if count != 0 {
		t.Fatalf("out-of-workspace symlink prewarmed %d replay entries", count)
	}

	for _, argv := range [][]string{
		{"cat", "src/outside.ts"},
		{"sed", "-n", "1,1p", "src/outside.ts"},
	} {
		t.Run(displayCommand(argv), func(t *testing.T) {
			res := k.Run(ctx, "test", repo, argv)
			if res.Mode == ModeReplay {
				t.Fatalf("out-of-workspace symlink replayed: proof=%+v", res.Proof)
			}
			assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
		})
	}

	argv := []string{"sed", "-n", "1,1p", "src/outside.ts"}
	first := k.Run(ctx, "test", repo, argv)
	if first.Mode == ModeReplay {
		t.Fatalf("out-of-workspace symlink replayed on first run: proof=%+v", first.Proof)
	}
	k.WaitForegroundObservations()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		again := k.Run(ctx, "test", repo, argv)
		if again.Mode == ModeReplay {
			t.Fatalf("out-of-workspace symlink replayed after foreground observation: proof=%+v", again.Proof)
		}
		assertSameResult(t, again.Stdout, again.Stderr, again.ExitCode, runNative(ctx, repo, argv))
		time.Sleep(25 * time.Millisecond)
	}
}

func TestWarmFileDoesNotReplayAfterSameStatSignalContentChange(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("src", "same-stat.ts")
	path := filepath.Join(repo, rel)
	original := []byte("export const value = 111;\n")
	changed := []byte("export const value = 222;\n")
	if len(original) != len(changed) {
		t.Fatal("test fixture contents must have equal size")
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Unix(1_700_000_000, 123_456_789)
	if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", rel})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare any windows")
	}
	warmed := k.Run(ctx, "test", repo, []string{"cat", rel})
	if warmed.Mode != ModeReplay {
		t.Fatalf("warmup cat mode = %s, want replay; diagnostics=%v", warmed.Mode, warmed.Diagnostics)
	}
	assertSameResult(t, warmed.Stdout, warmed.Stderr, warmed.ExitCode, runNative(ctx, repo, []string{"cat", rel}))

	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		t.Skipf("filesystem did not preserve same legacy stat signal: before=%q after=%q", legacyStatSignal(before), legacyStatSignal(after))
	}

	res := k.Run(ctx, "test", repo, []string{"cat", rel})
	native := runNative(ctx, repo, []string{"cat", rel})
	if string(native.Stdout) != string(changed) {
		t.Fatalf("native did not see changed bytes: %q", native.Stdout)
	}
	if res.Mode == ModeReplay {
		t.Fatalf("same-stat content change replayed stale output: got %q want native %q", res.Stdout, native.Stdout)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, native)
}

func legacyStatSignal(info os.FileInfo) string {
	return info.Mode().String() + "|" + info.ModTime().String() + "|" + strconv.FormatInt(info.Size(), 10)
}

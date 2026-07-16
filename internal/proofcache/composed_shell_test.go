package proofcache

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposedShellNLAndSedReplayMatchesNative(t *testing.T) {
	if _, err := exec.LookPath("nl"); err != nil {
		t.Skip("nl command unavailable")
	}
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	for i := 1; i <= 3200; i++ {
		fmt.Fprintf(&content, "line %04d with enough content to exceed the direct replay output cap\n", i)
	}
	rel := filepath.Join("src", "large.py")
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", rel}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare file")
	}
	if direct := k.Run(ctx, "test", repo, []string{"nl", "-ba", rel}); direct.Mode == ModeReplay {
		t.Fatalf("oversized direct nl unexpectedly replayed")
	}

	script := "nl -ba src/large.py | sed -n '300,480p'"
	res, ok := k.ReplayComposedShell(ctx, "test", repo, script)
	if !ok || res.Mode != ModeReplay {
		t.Fatalf("composed shell did not replay: ok=%v mode=%s diagnostics=%v", ok, res.Mode, res.Diagnostics)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"sh", "-c", script}))
}

func TestComposedShellMultiRangeSelectionMatchesNative(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("src", "selection.py")
	if err := os.WriteFile(filepath.Join(repo, rel), []byte("one\ntwo\nthree\nfour\nfive\nsix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1p", rel}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatal("adaptive prewarm did not prepare file")
	}

	for _, script := range []string{
		"sed -n '1,3p;2,4p' src/selection.py | tail -n 4",
		"nl -ba src/selection.py | sed -n '1,3p;2,4p;2p'",
	} {
		res, ok := k.ReplayComposedShell(ctx, "test", repo, script)
		if !ok || res.Mode != ModeReplay {
			t.Fatalf("composed shell did not replay %q: ok=%v mode=%s diagnostics=%v", script, ok, res.Mode, res.Diagnostics)
		}
		assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"sh", "-c", script}))
	}
}

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

func TestComposedShellShortHeadAndPwdMatchNative(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("src", "window.txt")
	if err := os.WriteFile(filepath.Join(repo, rel), []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if count, err := k.PrewarmAdjacent(ctx, repo, "test", []string{"sed", "-n", "1,1p", rel}); err != nil {
		t.Fatal(err)
	} else if count == 0 {
		t.Fatal("adaptive prewarm did not prepare file")
	}

	for _, script := range []string{
		"cat src/window.txt | head -2",
		"pwd; cat src/window.txt | head -3",
	} {
		res, ok := k.ReplayComposedShell(ctx, "test", repo, script)
		if !ok || res.Mode != ModeReplay {
			t.Fatalf("composed shell did not replay %q: ok=%v mode=%s diagnostics=%v", script, ok, res.Mode, res.Diagnostics)
		}
		assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"sh", "-c", script}))
	}
}

func TestComposedShellParsesEscapedDoubleQuoteLiterally(t *testing.T) {
	plan, ok := parseComposedShell(`rg -n "arg\(\"[A-Z]" test --glob '*.cc' | head -80`)
	if !ok {
		t.Fatal("valid escaped quote command did not parse")
	}
	root := plan.nodes[plan.root]
	if root.kind != shellNodePipe {
		t.Fatalf("root kind = %v, want pipe", root.kind)
	}
	rg := plan.nodes[root.left]
	if got, want := rg.argv[2], `arg\("[A-Z]`; got != want {
		t.Fatalf("regex = %q, want %q", got, want)
	}
}

func TestComposedShellGitLsFilesWCAndSortReplayMatchesNative(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src", "flask"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		filepath.Join("src", "flask", "z.py"): "z = 1\n",
		filepath.Join("src", "flask", "a.py"): "a = 1\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if res := runNative(ctx, repo, []string{"git", "add", name}); res.ExitCode != 0 {
			t.Fatalf("git add failed: %s", res.Stderr)
		}
	}
	if res := runNative(ctx, repo, []string{"git", "commit", "-m", "add source tree"}); res.ExitCode != 0 {
		t.Fatalf("git commit failed: %s", res.Stderr)
	}

	k := New(DefaultStoreRoot(repo))
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{
		"git ls-files src/flask | wc -l",
		"printf 'z\\na\\n' | sort",
		"git ls-files src/flask | sort",
	} {
		t.Run(script, func(t *testing.T) {
			res, ok := k.ReplayComposedShell(ctx, "test", repo, script)
			if !ok || res.Mode != ModeReplay {
				t.Fatalf("composed shell did not replay: ok=%v mode=%s diagnostics=%v", ok, res.Mode, res.Diagnostics)
			}
			assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"sh", "-c", script}))
		})
	}
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

func TestComposedShellRejectsUnsupportedNLForms(t *testing.T) {
	for _, script := range []string{
		"nl src/app.py | sed -n '1,4p'",
		"nl -bt src/app.py | sed -n '1,4p'",
		"nl -ba src/app.py | sed '1,4p'",
	} {
		if _, ok := parseComposedShell(script); !ok {
			continue
		}
		plan, _ := parseComposedShell(script)
		if productionRuntimePlanAllowed(".", plan, plan.root, false) {
			t.Fatalf("unsupported nl composition allowed: %s", script)
		}
	}
}

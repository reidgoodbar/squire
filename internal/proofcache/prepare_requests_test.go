package proofcache

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func encodePrepareRequestForTest(t *testing.T, req prepareRequest) []byte {
	t.Helper()
	var out bytes.Buffer
	out.Write(prepareRequestMagic[:])
	if err := binary.Write(&out, binary.LittleEndian, uint32(len(req.CWD))); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&out, binary.LittleEndian, uint32(len(req.Argv))); err != nil {
		t.Fatal(err)
	}
	out.WriteString(req.CWD)
	for _, arg := range req.Argv {
		if err := binary.Write(&out, binary.LittleEndian, uint32(len(arg))); err != nil {
			t.Fatal(err)
		}
		out.WriteString(arg)
	}
	return out.Bytes()
}

func writePrepareRequestForTest(t *testing.T, storeRoot, cwd string, argv []string) string {
	t.Helper()
	dir := filepath.Join(storeRoot, prepareRequestDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := preparedReplayLookupKey(cwd, argv) + ".req"
	path := filepath.Join(dir, name)
	data := encodePrepareRequestForTest(t, prepareRequest{CWD: cwd, Argv: argv})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDecodePrepareRequestRejectsMalformedFrames(t *testing.T) {
	cwd := filepath.Clean(t.TempDir())
	valid := encodePrepareRequestForTest(t, prepareRequest{CWD: cwd, Argv: []string{"rg", "-F", "needle", "."}})
	if got, err := decodePrepareRequest(valid); err != nil || got.CWD != cwd || normalizeArgv(got.Argv) != normalizeArgv([]string{"rg", "-F", "needle", "."}) {
		t.Fatalf("decodePrepareRequest(valid) = %+v, %v", got, err)
	}
	for name, frame := range map[string][]byte{
		"empty":          nil,
		"bad magic":      append([]byte("BADMAGIC"), valid[8:]...),
		"truncated":      valid[:len(valid)-1],
		"trailing bytes": append(append([]byte(nil), valid...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodePrepareRequest(frame); err == nil {
				t.Fatal("malformed request decoded successfully")
			}
		})
	}
}

func TestMaintainerConsumesFixedRgDemandAndInvalidatesOnMutation(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg is not installed")
	}
	ctx := context.Background()
	repo := testRepo(t, ctx)
	path := filepath.Join(repo, "search.txt")
	if err := os.WriteFile(path, []byte("first needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	argv := []string{"rg", "-F", "needle", "."}
	requestPath := writePrepareRequestForTest(t, storeRoot, repo, argv)
	cycle, err := k.consumePrepareRequests(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Observed != 1 || cycle.Prepared != 1 || cycle.Rejected != 0 {
		t.Fatalf("unexpected request cycle: %+v", cycle)
	}
	if _, err := os.Stat(requestPath); !os.IsNotExist(err) {
		t.Fatalf("prepared request was not removed: %v", err)
	}
	want := runNative(ctx, repo, argv)
	got, ok := FastHotClientReplay(ctx, "test", repo, storeRoot, argv)
	if !ok {
		t.Fatal("demanded rg command did not enter hot snapshot")
	}
	assertSameResult(t, got.Stdout, got.Stderr, got.ExitCode, want)

	if err := os.WriteFile(path, []byte("first needle\nsecond needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := FastHotClientReplay(ctx, "test", repo, storeRoot, argv); ok {
		t.Fatal("stale rg output replayed after source mutation")
	}
	writePrepareRequestForTest(t, storeRoot, repo, argv)
	cycle, err = k.consumePrepareRequests(ctx, 8)
	if err != nil || cycle.Prepared != 1 {
		t.Fatalf("reprepare after mutation = %+v, %v", cycle, err)
	}
	want = runNative(ctx, repo, argv)
	got, ok = FastHotClientReplay(ctx, "test", repo, storeRoot, argv)
	if !ok {
		t.Fatal("mutated rg command did not re-enter hot snapshot")
	}
	assertSameResult(t, got.Stdout, got.Stderr, got.ExitCode, want)
}

func TestMaintainerCachesFixedRgNoMatchExit(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg is not installed")
	}
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	argv := []string{"rg", "-F", "definitely-absent-squire-pattern", "."}
	writePrepareRequestForTest(t, storeRoot, repo, argv)
	cycle, err := k.consumePrepareRequests(ctx, 8)
	if err != nil || cycle.Prepared != 1 || cycle.Rejected != 0 {
		t.Fatalf("no-match request cycle = %+v, %v", cycle, err)
	}
	got, ok := FastHotClientReplay(ctx, "test", repo, storeRoot, argv)
	if !ok || got.ExitCode != 1 || len(got.Stdout) != 0 || len(got.Stderr) != 0 {
		t.Fatalf("no-match hot replay = %+v, hit=%v", got, ok)
	}
}

func TestMaintainerConsumesBoundedRegexRgDemandWithExactInvalidation(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg is not installed")
	}
	t.Setenv("NO_COLOR", "0")
	ctx := context.Background()
	repo := testRepo(t, ctx)
	path := filepath.Join(repo, "search.txt")
	if err := os.WriteFile(path, []byte("alpha marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	argv := []string{"rg", "-n", "(alpha|omega) marker", ".", "--glob", "!ignored/**"}
	writePrepareRequestForTest(t, storeRoot, repo, argv)
	cycle, err := k.consumePrepareRequests(ctx, 8)
	if err != nil || cycle.Prepared != 1 || cycle.Rejected != 0 {
		t.Fatalf("bounded rg request cycle = %+v, %v", cycle, err)
	}
	want := runNative(ctx, repo, argv)
	got, ok := FastHotClientReplay(ctx, "test", repo, storeRoot, argv)
	if !ok {
		t.Fatal("bounded rg command did not enter hot snapshot")
	}
	assertSameResult(t, got.Stdout, got.Stderr, got.ExitCode, want)

	t.Setenv("NO_COLOR", "1")
	if _, ok := FastHotClientReplay(ctx, "test", repo, storeRoot, argv); ok {
		t.Fatal("bounded rg replay crossed an output-relevant environment change")
	}
	t.Setenv("NO_COLOR", "0")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("omega marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, ok := FastHotClientReplay(ctx, "test", repo, storeRoot, argv); ok {
		t.Fatal("stale bounded rg output replayed after same-size, restored-mtime mutation")
	}
	writePrepareRequestForTest(t, storeRoot, repo, argv)
	cycle, err = k.consumePrepareRequests(ctx, 8)
	if err != nil || cycle.Prepared != 1 {
		t.Fatalf("bounded rg reprepare = %+v, %v", cycle, err)
	}
	want = runNative(ctx, repo, argv)
	got, ok = FastHotClientReplay(ctx, "test", repo, storeRoot, argv)
	if !ok {
		t.Fatal("mutated bounded rg command did not re-enter hot snapshot")
	}
	assertSameResult(t, got.Stdout, got.Stderr, got.ExitCode, want)

	missing := []string{"rg", "-n", "definitely-(absent)", "."}
	writePrepareRequestForTest(t, storeRoot, repo, missing)
	cycle, err = k.consumePrepareRequests(ctx, 8)
	if err != nil || cycle.Prepared != 1 {
		t.Fatalf("bounded rg no-match prepare = %+v, %v", cycle, err)
	}
	got, ok = FastHotClientReplay(ctx, "test", repo, storeRoot, missing)
	if !ok || got.ExitCode != 1 || len(got.Stdout) != 0 || len(got.Stderr) != 0 {
		t.Fatalf("bounded rg no-match replay = %+v, hit=%v", got, ok)
	}
}

func TestMaintainerRejectsUnsafePrepareRequestWithoutExecution(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(repo, "must-not-exist")
	argv := []string{"sh", "-c", "touch must-not-exist"}
	writePrepareRequestForTest(t, storeRoot, repo, argv)
	cycle, err := k.consumePrepareRequests(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Observed != 1 || cycle.Prepared != 0 || cycle.Rejected != 1 {
		t.Fatalf("unsafe request cycle = %+v", cycle)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unsafe request executed: %v", err)
	}
}

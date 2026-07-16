package proofcache

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeGitHistoryCorpusIncludesTypedCommitAndPathRecords(t *testing.T) {
	commits := []gitHistoryCorpusCommit{
		{
			hash:        strings.Repeat("a", 40),
			oneline:     []byte("aaaaaaa first subject"),
			parentCount: 1,
			paths:       []string{"src/alpha.go", "README.md"},
		},
		{
			hash:        strings.Repeat("b", 40),
			oneline:     []byte("bbbbbbb initial"),
			parentCount: 0,
			paths:       []string{"README.md"},
		},
	}
	frame, ok := encodeGitHistoryCorpus(commits, true)
	if !ok {
		t.Fatal("encodeGitHistoryCorpus failed")
	}
	if got := binary.LittleEndian.Uint32(frame[8:12]); got != gitHistoryCorpusVersion {
		t.Fatalf("version = %d, want %d", got, gitHistoryCorpusVersion)
	}
	if got := binary.LittleEndian.Uint32(frame[12:16]); got != 2 {
		t.Fatalf("commit count = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(frame[28:32]); got != 1 {
		t.Fatalf("complete = %d, want 1", got)
	}
	pathRecordsOffset := int(binary.LittleEndian.Uint32(frame[20:24]))
	payloadOffset := int(binary.LittleEndian.Uint32(frame[24:28]))
	if pathRecordsOffset != gitHistoryCorpusHeaderBytes+2*gitHistoryCorpusRecordBytes || payloadOffset <= pathRecordsOffset {
		t.Fatalf("invalid offsets: paths=%d payload=%d", pathRecordsOffset, payloadOffset)
	}
	first := frame[gitHistoryCorpusHeaderBytes : gitHistoryCorpusHeaderBytes+gitHistoryCorpusRecordBytes]
	if got := binary.LittleEndian.Uint32(first[20:24]); got != 2 {
		t.Fatalf("first path count = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(first[24:28]); got != 1 {
		t.Fatalf("first parent count = %d, want 1", got)
	}
	lineOffset := int(binary.LittleEndian.Uint32(first[8:12]))
	lineLength := int(binary.LittleEndian.Uint32(first[12:16]))
	if got := string(frame[lineOffset : lineOffset+lineLength]); got != string(commits[0].oneline) {
		t.Fatalf("oneline = %q, want %q", got, commits[0].oneline)
	}
	pathRecord := frame[pathRecordsOffset : pathRecordsOffset+gitHistoryCorpusPathBytes]
	pathOffset := int(binary.LittleEndian.Uint32(pathRecord[0:4]))
	pathLength := int(binary.LittleEndian.Uint32(pathRecord[4:8]))
	if got := string(frame[pathOffset : pathOffset+pathLength]); got != "src/alpha.go" {
		t.Fatalf("first path = %q", got)
	}
}

func TestBuildGitHistoryCorpusCapturesNativeOrderAndChangedPaths(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	commitFile(t, ctx, repo, "src/alpha.go", "package src\n", "add alpha")
	frame, ok := buildGitHistoryCorpus(ctx, repo)
	if !ok {
		t.Fatal("buildGitHistoryCorpus failed")
	}
	if binary.LittleEndian.Uint32(frame[28:32]) != 1 {
		t.Fatal("small repository history was not marked complete")
	}
	count := int(binary.LittleEndian.Uint32(frame[12:16]))
	if count != 2 {
		t.Fatalf("commit count = %d, want 2", count)
	}
	first := frame[gitHistoryCorpusHeaderBytes : gitHistoryCorpusHeaderBytes+gitHistoryCorpusRecordBytes]
	lineOffset := int(binary.LittleEndian.Uint32(first[8:12]))
	lineLength := int(binary.LittleEndian.Uint32(first[12:16]))
	if got := string(frame[lineOffset : lineOffset+lineLength]); !strings.HasSuffix(got, " add alpha") {
		t.Fatalf("newest oneline = %q", got)
	}
	pathStart := int(binary.LittleEndian.Uint32(first[16:20]))
	pathCount := int(binary.LittleEndian.Uint32(first[20:24]))
	if pathCount != 1 {
		t.Fatalf("newest path count = %d, want 1", pathCount)
	}
	pathRecordsOffset := int(binary.LittleEndian.Uint32(frame[20:24]))
	pathRecord := frame[pathRecordsOffset+pathStart*gitHistoryCorpusPathBytes : pathRecordsOffset+(pathStart+1)*gitHistoryCorpusPathBytes]
	pathOffset := int(binary.LittleEndian.Uint32(pathRecord[0:4]))
	pathLength := int(binary.LittleEndian.Uint32(pathRecord[4:8]))
	if got := string(frame[pathOffset : pathOffset+pathLength]); got != "src/alpha.go" {
		t.Fatalf("newest changed path = %q", got)
	}
}

func TestGitLogViewFingerprintIncludesLooseRefs(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	_, gitDir, ok := discoverGitDir(repo)
	if !ok {
		t.Fatal("discoverGitDir failed")
	}
	before := gitLogViewFingerprint(gitDir)
	head := runNative(ctx, repo, []string{"git", "rev-parse", "HEAD"})
	if head.ExitCode != 0 {
		t.Fatalf("git rev-parse failed: %s", head.Stderr)
	}
	ref := filepath.Join(gitDir, "refs", "tags", "history-proof")
	if err := os.MkdirAll(filepath.Dir(ref), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ref, head.Stdout, 0o600); err != nil {
		t.Fatal(err)
	}
	after := gitLogViewFingerprint(gitDir)
	if before == after {
		t.Fatal("loose ref did not change git log view fingerprint")
	}
}

func TestGitObjectNamespaceFingerprintChangesForUnreachableObject(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	_, gitDir, ok := discoverGitDir(repo)
	if !ok {
		t.Fatal("discoverGitDir failed")
	}
	before, ok := gitObjectNamespaceFingerprint(gitDir)
	if !ok {
		t.Fatal("initial object namespace fingerprint failed")
	}
	payload := filepath.Join(repo, "unreachable-object-payload")
	if err := os.WriteFile(payload, []byte("not reachable from any ref\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := runNative(ctx, repo, []string{"git", "hash-object", "-w", payload})
	if result.ExitCode != 0 {
		t.Fatalf("git hash-object failed: %s", result.Stderr)
	}
	after, ok := gitObjectNamespaceFingerprint(gitDir)
	if !ok {
		t.Fatal("updated object namespace fingerprint failed")
	}
	if before == after {
		t.Fatal("unreachable loose object did not change object namespace fingerprint")
	}
}

func TestGitObjectNamespaceFingerprintRejectsPathspecEnvironment(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	_, gitDir, ok := discoverGitDir(repo)
	if !ok {
		t.Fatal("discoverGitDir failed")
	}
	if _, ok := gitObjectNamespaceFingerprint(gitDir); !ok {
		t.Fatal("standard repository layout was unexpectedly rejected")
	}
	for _, key := range []string{
		"GIT_LITERAL_PATHSPECS",
		"GIT_GLOB_PATHSPECS",
		"GIT_NOGLOB_PATHSPECS",
		"GIT_ICASE_PATHSPECS",
		"GIT_CEILING_DIRECTORIES",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "1")
			if _, ok := gitObjectNamespaceFingerprint(gitDir); ok {
				t.Fatalf("non-empty %s must disable bounded history replay", key)
			}
		})
	}
}

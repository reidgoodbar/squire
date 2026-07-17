package green

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
)

type inputMatcher struct {
	include []string
	exclude []string
}

type workspaceObservation struct {
	ID        string
	State     string
	Untracked int
}

const (
	maxMatchedInputFiles = 100000
	maxMatchedInputBytes = int64(20 << 30)
)

type digestCacheEntry struct {
	signal string
	digest string
	size   int64
}

type regularIdentity struct {
	digest      string
	size        int64
	mode        string
	changeToken string
}

var regularFileDigestCache = struct {
	sync.Mutex
	entries map[string]digestCacheEntry
}{entries: make(map[string]digestCacheEntry)}

func computeCheckProof(ctx context.Context, repoRoot string, config Config, check Check) (CheckProof, error) {
	workspace := observeWorkspace(ctx, repoRoot)
	return computeCheckProofAtWorkspace(ctx, repoRoot, config, check, workspace.ID)
}

func computeCheckProofAtWorkspace(ctx context.Context, repoRoot string, config Config, check Check, workspaceID string) (CheckProof, error) {
	files, inputDigest, matchedBytes, err := collectInputProof(ctx, repoRoot, check)
	if err != nil {
		return CheckProof{}, err
	}
	environment := validationEnvironment(os.Environ(), check.CWD, check.Env)
	environmentDigest := digestStrings(environment)
	executablePath, err := resolveExecutable(check.Command[0], check.CWD, environment)
	if err != nil {
		return CheckProof{}, err
	}
	executableIdentity, err := readRegularIdentity(ctx, executablePath)
	if err != nil {
		return CheckProof{}, fmt.Errorf("hash executable %s: %w", executablePath, err)
	}
	executableDigest := digestStrings([]string{"squire-green-executable-v1", executablePath, executableIdentity.mode, executableIdentity.changeToken, executableIdentity.digest})
	commandDigest := digestStrings(check.Command)
	digest := digestStrings([]string{
		"squire-green-proof-v1",
		config.Digest,
		check.Name,
		commandDigest,
		filepath.Clean(check.CWD),
		inputDigest,
		environmentDigest,
		executablePath,
		executableDigest,
	})
	return CheckProof{
		Digest:              digest,
		InputDigest:         inputDigest,
		EnvironmentDigest:   environmentDigest,
		ExecutablePath:      executablePath,
		ExecutableDigest:    executableDigest,
		MatchedFiles:        len(files),
		MatchedBytes:        matchedBytes,
		ObservedWorkspaceID: workspaceID,
		Environment:         environment,
		CWD:                 check.CWD,
	}, nil
}

func collectInputProof(ctx context.Context, repoRoot string, check Check) ([]string, string, int64, error) {
	matcher := inputMatcher{include: check.Inputs, exclude: check.Exclude}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, "", 0, err
	}
	var entries []string
	var matchedBytes int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.IsDir() && rel != "." && matcher.excludesSubtree(rel) {
			return filepath.SkipDir
		}
		if entry.IsDir() && rel != "." && !matcher.couldContain(rel) {
			return filepath.SkipDir
		}
		if rel == "." || entry.IsDir() || !matcher.matches(rel) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			line, size, err := symlinkProof(ctx, root, path, rel, info)
			if err != nil {
				return err
			}
			entries = append(entries, line)
			matchedBytes += size
			if len(entries) > maxMatchedInputFiles || matchedBytes > maxMatchedInputBytes {
				return errors.New("declared input proof exceeds Green bounds")
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("declared input is not a regular file: %s", rel)
		}
		identity, err := readRegularIdentity(ctx, path)
		if err != nil {
			return fmt.Errorf("hash input %s: %w", rel, err)
		}
		entries = append(entries, strings.Join([]string{
			"file", rel, identity.mode, strconv.FormatInt(identity.size, 10), identity.changeToken, identity.digest,
		}, "\x00"))
		matchedBytes += identity.size
		if len(entries) > maxMatchedInputFiles || matchedBytes > maxMatchedInputBytes {
			return errors.New("declared input proof exceeds Green bounds")
		}
		return nil
	})
	if err != nil {
		return nil, "", 0, err
	}
	sort.Strings(entries)
	proofParts := []string{"squire-green-input-v1"}
	for _, pattern := range check.Inputs {
		proofParts = append(proofParts, "include\x00"+pattern)
	}
	for _, pattern := range check.Exclude {
		proofParts = append(proofParts, "exclude\x00"+pattern)
	}
	proofParts = append(proofParts, entries...)
	return entries, digestStrings(proofParts), matchedBytes, nil
}

func symlinkProof(ctx context.Context, root, path, rel string, info os.FileInfo) (string, int64, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", 0, err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", 0, fmt.Errorf("resolve declared symlink input %s: %w", rel, err)
	}
	if !pathWithinRoot(realPath, root) {
		return "", 0, fmt.Errorf("declared symlink input resolves outside repository: %s", rel)
	}
	realInfo, err := os.Stat(realPath)
	if err != nil || !realInfo.Mode().IsRegular() {
		return "", 0, fmt.Errorf("declared symlink input does not resolve to a regular file: %s", rel)
	}
	identity, err := readRegularIdentity(ctx, realPath)
	if err != nil {
		return "", 0, err
	}
	realRel, err := filepath.Rel(root, realPath)
	if err != nil {
		return "", 0, err
	}
	return strings.Join([]string{
		"symlink",
		rel,
		info.Mode().String(),
		target,
		filepath.ToSlash(realRel),
		identity.mode,
		strconv.FormatInt(identity.size, 10),
		fileChangeToken(info),
		identity.changeToken,
		identity.digest,
	}, "\x00"), identity.size, nil
}

func readRegularIdentity(ctx context.Context, path string) (regularIdentity, error) {
	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() {
		return regularIdentity{}, errors.New("not a regular file")
	}
	digest, size, err := digestRegularFile(ctx, path)
	if err != nil {
		return regularIdentity{}, err
	}
	after, err := os.Stat(path)
	if err != nil || !sameFileSnapshot(before, after) || size != after.Size() {
		return regularIdentity{}, errors.New("file changed while proving identity")
	}
	return regularIdentity{
		digest:      digest,
		size:        size,
		mode:        after.Mode().String(),
		changeToken: fileChangeToken(after),
	}, nil
}

func digestRegularFile(ctx context.Context, path string) (string, int64, error) {
	before, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	if !before.Mode().IsRegular() {
		return "", 0, errors.New("not a regular file")
	}
	cacheSignal := regularFileCacheSignal(before)
	if cacheSignal != "" {
		regularFileDigestCache.Lock()
		cached, ok := regularFileDigestCache.entries[filepath.Clean(path)]
		regularFileDigestCache.Unlock()
		if ok && cached.signal == cacheSignal {
			return cached.digest, cached.size, nil
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", 0, errors.New("file identity changed before hashing")
	}
	hash := sha256.New()
	buffer := make([]byte, 128*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			if _, err := hash.Write(buffer[:read]); err != nil {
				return "", 0, err
			}
			size += int64(read)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", 0, readErr
		}
	}
	after, err := file.Stat()
	if err != nil || !sameFileSnapshot(opened, after) || size != after.Size() {
		return "", 0, errors.New("file changed while hashing")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if cacheSignal != "" {
		regularFileDigestCache.Lock()
		if len(regularFileDigestCache.entries) >= 8192 {
			regularFileDigestCache.entries = make(map[string]digestCacheEntry)
		}
		regularFileDigestCache.entries[filepath.Clean(path)] = digestCacheEntry{signal: cacheSignal, digest: digest, size: size}
		regularFileDigestCache.Unlock()
	}
	return digest, size, nil
}

func (matcher inputMatcher) matches(rel string) bool {
	matched := false
	for _, pattern := range matcher.include {
		if ok, _ := doublestar.Match(pattern, rel); ok {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, pattern := range matcher.exclude {
		if ok, _ := doublestar.Match(pattern, rel); ok {
			return false
		}
	}
	return true
}

func validationEnvironment(base []string, cwd string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides)+1)
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || key == "_" || key == "OLDPWD" || key == "SHLVL" || strings.HasPrefix(key, "SQUIRE_") {
			continue
		}
		values[key] = value
	}
	values["PWD"] = filepath.Clean(cwd)
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func resolveExecutable(name, cwd string, environment []string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) || (runtime.GOOS == "windows" && strings.Contains(name, "/")) {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		return validateExecutablePath(path)
	}
	pathValue := environmentValue(environment, "PATH")
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = cwd
		}
		candidate := filepath.Join(dir, name)
		paths := []string{candidate}
		if runtime.GOOS == "windows" && filepath.Ext(candidate) == "" {
			for _, extension := range strings.Split(environmentValue(environment, "PATHEXT"), ";") {
				if extension != "" {
					paths = append(paths, candidate+strings.ToLower(extension), candidate+strings.ToUpper(extension))
				}
			}
		}
		for _, path := range paths {
			if resolved, err := validateExecutablePath(path); err == nil {
				return resolved, nil
			}
		}
	}
	return "", fmt.Errorf("validation executable %q not found on PATH", name)
}

func validateExecutablePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("not a regular executable")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("not executable")
	}
	return filepath.Clean(real), nil
}

func environmentValue(environment []string, name string) string {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func observeWorkspace(ctx context.Context, repoRoot string) workspaceObservation {
	head := nativeOutput(ctx, repoRoot, []string{"git", "rev-parse", "HEAD"})
	status := nativeOutput(ctx, repoRoot, []string{"git", "status", "--porcelain=v1", "-z", "--untracked-files=all"})
	state := "unknown"
	untracked := 0
	if status.ok {
		if len(status.output) == 0 {
			state = "clean"
		} else {
			state = "modified"
			for _, entry := range strings.Split(string(status.output), "\x00") {
				if strings.HasPrefix(entry, "?? ") {
					untracked++
				}
			}
		}
	}
	return workspaceObservation{
		ID:        digestStrings([]string{"squire-green-workspace-v1", string(head.output), string(status.output), strconv.FormatBool(head.ok), strconv.FormatBool(status.ok)}),
		State:     state,
		Untracked: untracked,
	}
}

type commandOutput struct {
	output []byte
	ok     bool
}

func nativeOutput(ctx context.Context, cwd string, argv []string) commandOutput {
	if len(argv) == 0 {
		return commandOutput{}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = append(validationEnvironment(os.Environ(), cwd, nil), "GIT_OPTIONAL_LOCKS=0")
	output, err := cmd.Output()
	return commandOutput{output: output, ok: err == nil}
}

func digestStrings(values []string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(hash, strconv.Itoa(len(value)))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func sameFileSnapshot(before, after os.FileInfo) bool {
	return before != nil && after != nil &&
		os.SameFile(before, after) &&
		before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime()) &&
		before.Mode() == after.Mode() &&
		fileChangeToken(before) == fileChangeToken(after)
}

func regularFileCacheSignal(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	token := fileChangeToken(info)
	if token == "" || token == "unsupported" {
		return ""
	}
	return strings.Join([]string{
		strconv.FormatInt(info.Size(), 10),
		strconv.FormatInt(info.ModTime().UnixNano(), 10),
		info.Mode().String(),
		token,
	}, "|")
}

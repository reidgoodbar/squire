package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"squire.run/internal/proofcache"
)

var sessionShimNames = []string{
	"git",
	"cat",
	"sed",
	"head",
	"tail",
	"file",
	"grep",
	"printenv",
	"ls",
	"whoami",
	"uname",
	"id",
	"hostname",
	"which",
	"command",
	"rg",
	"go",
	"node",
	"npm",
	"pnpm",
	"yarn",
	"python",
	"python3",
	"pip",
	"pip3",
	"cargo",
	"rustc",
	"make",
}

type sessionOptions struct {
	Command              []string
	ExtraEnv             []string
	PreloadLib           string
	Quiet                bool
	MetadataOnly         bool
	NoWarm               bool
	NoMaintainer         bool
	EnableWarmFileReplay bool
	Preload              bool
}

func runSession(ctx context.Context, cwd, storeRoot string, args []string) error {
	opts, err := parseSessionOptions(args)
	if err != nil {
		return err
	}
	code, err := runScopedSession(ctx, cwd, storeRoot, opts)
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}

func parseSessionOptions(args []string) (sessionOptions, error) {
	var opts sessionOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 >= len(args) {
				return opts, fmt.Errorf("squire session requires a command after --")
			}
			opts.Command = append([]string(nil), args[i+1:]...)
			if opts.MetadataOnly && opts.NoWarm {
				return opts, fmt.Errorf("squire session cannot combine --metadata-only and --no-warm")
			}
			return opts, nil
		}
		switch arg {
		case "--quiet":
			opts.Quiet = true
		case "--metadata-only":
			opts.MetadataOnly = true
		case "--no-warm":
			opts.NoWarm = true
		case "--no-maintainer":
			opts.NoMaintainer = true
		case "--enable-warm-file-replay":
			opts.EnableWarmFileReplay = true
		case "--preload":
			opts.Preload = true
		case "--preload-lib":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("squire session --preload-lib requires a path")
			}
			i++
			opts.PreloadLib = args[i]
		default:
			return opts, fmt.Errorf("unknown session option %q", arg)
		}
	}
	return opts, fmt.Errorf("squire session requires -- before the command")
}

func runScopedSession(ctx context.Context, cwd, storeRoot string, opts sessionOptions) (int, error) {
	env := append(os.Environ(), "SQUIRE_STORE_ROOT="+storeRoot)
	if sessionCommandIsCodex(opts.Command) {
		env = append(env, squireCodexBridgeEnv()...)
	}
	env = append(env, opts.ExtraEnv...)
	transport := "native"
	var err error
	launcherForSafety := opts.Command[0]
	if !strings.ContainsRune(launcherForSafety, os.PathSeparator) {
		if resolved, ok := lookupExecutableInPath(launcherForSafety, envValue(env, "PATH"), cwd); ok {
			launcherForSafety = resolved
		}
	}
	tryPreload := opts.Preload || !isPreloadUnsafeLauncher(launcherForSafety)
	if tryPreload {
		preloadLib, preloadErr := resolveSessionPreloadLib(opts.PreloadLib)
		if preloadErr == nil {
			preloadHelper, helperErr := resolveSessionPreloadHelper(preloadLib)
			if helperErr != nil && opts.Preload {
				return 0, helperErr
			}
			env, err = buildPreloadSessionEnvironment(cwd, preloadLib, preloadHelper, env, opts.EnableWarmFileReplay)
			if err != nil {
				return 0, err
			}
			transport = "preload"
		} else if opts.Preload || opts.PreloadLib != "" {
			return 0, preloadErr
		}
	}

	if _, err := proofcache.Setup(ctx, cwd, storeRoot); err != nil {
		return 0, err
	}
	if !opts.NoMaintainer {
		if _, err := proofcache.StartBackgroundMaintainer(ctx, cwd, storeRoot, proofcache.DefaultBackgroundMaintainerOptions()); err != nil {
			return 0, err
		}
	}
	if !opts.NoWarm {
		if opts.MetadataOnly {
			if _, err := proofcache.WarmMetadata(ctx, cwd, storeRoot); err != nil {
				return 0, err
			}
		} else if _, err := proofcache.Warm(ctx, cwd, storeRoot); err != nil {
			return 0, err
		}
	}

	if !opts.Quiet {
		if transport == "preload" {
			fmt.Fprintf(os.Stderr, "squire session: scoped preload active, native fallback available\n")
		} else {
			fmt.Fprintf(os.Stderr, "squire session: native execution active; preload not applied\n")
		}
	}

	commandPath := opts.Command[0]
	if !strings.ContainsRune(commandPath, os.PathSeparator) {
		if resolved, ok := lookupExecutableInPath(commandPath, envValue(env, "PATH"), cwd); ok {
			commandPath = resolved
		}
	}
	cmd := exec.CommandContext(ctx, commandPath, opts.Command[1:]...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	eventPipe := startHotEventPipe(storeRoot)
	if eventPipe != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, eventPipe.writer)
		cmd.Env = append(cmd.Env, fmt.Sprintf("SQUIRE_HOT_EVENT_FD=%d", 3+len(cmd.ExtraFiles)-1))
	}
	hotSnapshotFile := openHotSnapshotFile(storeRoot)
	if hotSnapshotFile != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, hotSnapshotFile)
		cmd.Env = append(cmd.Env, fmt.Sprintf("SQUIRE_HOT_SNAPSHOT_FD=%d", 3+len(cmd.ExtraFiles)-1))
	}
	err = cmd.Start()
	if eventPipe != nil {
		_ = eventPipe.writer.Close()
	}
	if hotSnapshotFile != nil {
		_ = hotSnapshotFile.Close()
	}
	if err == nil {
		err = cmd.Wait()
	}
	finishHotEventPipe(eventPipe)
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

func sessionCommandIsCodex(command []string) bool {
	if len(command) == 0 {
		return false
	}
	base := filepath.Base(command[0])
	return base == "codex" || base == "codex.exe"
}

func openHotSnapshotFile(storeRoot string) *os.File {
	if storeRoot == "" {
		return nil
	}
	source, err := os.Open(filepath.Join(storeRoot, "hot_snapshot.bin"))
	if err != nil {
		return nil
	}
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<20 {
		_ = source.Close()
		return nil
	}
	if !sessionLocalHotSnapshotEnabled() {
		return source
	}
	local, err := localHotSnapshotCopy(source, info.Size())
	if err != nil {
		if _, seekErr := source.Seek(0, io.SeekStart); seekErr == nil {
			return source
		}
		_ = source.Close()
		return nil
	}
	_ = source.Close()
	return local
}

func sessionLocalHotSnapshotEnabled() bool {
	value := os.Getenv("SQUIRE_SESSION_LOCAL_HOT_SNAPSHOT")
	return value != "" && value != "0"
}

func localHotSnapshotCopy(source *os.File, size int64) (*os.File, error) {
	tmp, err := os.CreateTemp("", "squire-hot-snapshot.")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	written, err := io.Copy(tmp, io.LimitReader(source, size+1))
	if err != nil || written != size {
		cleanup()
		if err != nil {
			return nil, err
		}
		return nil, io.ErrUnexpectedEOF
	}
	if err := tmp.Chmod(0o400); err != nil {
		cleanup()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	readonly, err := os.Open(tmpPath)
	_ = os.Remove(tmpPath)
	if err != nil {
		return nil, err
	}
	return readonly, nil
}

type hotEventPipe struct {
	reader *os.File
	writer *os.File
	done   chan struct{}
}

func startHotEventPipe(storeRoot string) *hotEventPipe {
	if storeRoot == "" {
		return nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil
	}
	setSessionPipeNonblock(writer)
	pipe := &hotEventPipe{
		reader: reader,
		writer: writer,
		done:   make(chan struct{}),
	}
	go func() {
		defer close(pipe.done)
		defer reader.Close()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 1024), 16*1024)
		for scanner.Scan() {
			_ = proofcache.AppendHotClientEventLine(storeRoot, scanner.Bytes())
		}
	}()
	return pipe
}

func finishHotEventPipe(pipe *hotEventPipe) {
	if pipe == nil {
		return
	}
	select {
	case <-pipe.done:
		return
	case <-time.After(500 * time.Millisecond):
		_ = pipe.reader.Close()
	}
	select {
	case <-pipe.done:
	case <-time.After(100 * time.Millisecond):
	}
}

func resolveSessionPreloadLib(explicit string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if env := os.Getenv("SQUIRE_PRELOAD_LIB"); env != "" {
		candidates = append(candidates, env)
	}
	name := preloadLibraryName()
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	if path, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, path)
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if abs, err := filepath.Abs(candidate); err == nil {
			candidate = abs
		}
		if isRegularFile(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("squire session --preload could not find %s; install Squire or pass --preload-lib <path>", name)
}

func resolveSessionPreloadHelper(preloadLib string) (string, error) {
	candidates := []string{}
	if env := os.Getenv("SQUIRE_PRELOAD_HELPER"); env != "" {
		candidates = append(candidates, env)
	}
	if preloadLib != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(preloadLib), "squire-preload-helper"))
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "squire-preload-helper"))
	}
	if path, err := exec.LookPath("squire-preload-helper"); err == nil {
		candidates = append(candidates, path)
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if abs, err := filepath.Abs(candidate); err == nil {
			candidate = abs
		}
		if isExecutableFile(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("squire session could not find executable squire-preload-helper; install Squire to support posix_spawn file-actions replay")
}

func preloadLibraryName() string {
	if runtimeGOOS() == "darwin" {
		return "squire-preload.dylib"
	}
	return "squire-preload.so"
}

func isPreloadUnsafeLauncher(command string) bool {
	name := filepath.Base(command)
	if strings.HasPrefix(name, "python") || strings.HasPrefix(name, "pip") {
		return true
	}
	if runtimeGOOS() == "darwin" {
		if strings.HasPrefix(command, "/bin/") || strings.HasPrefix(command, "/usr/bin/") {
			return true
		}
		switch name {
		case "sh", "bash", "zsh", "env":
			return true
		}
	}
	return false
}

func buildPreloadSessionEnvironment(cwd, preloadLib, preloadHelper string, base []string, includeWarmFileReplay bool) ([]string, error) {
	basePath := envSliceValue(base, "PATH")
	if basePath == "" {
		basePath = os.Getenv("PATH")
	}
	overrides := map[string]string{
		"SQUIRE_SHIM_REAL_PATH": basePath,
		"SQUIRE_PRELOAD_ENABLE": "1",
		"SQUIRE_PRELOAD_LIB":    preloadLib,
	}
	if preloadHelper != "" {
		overrides["SQUIRE_PRELOAD_HELPER"] = preloadHelper
	}
	if runtimeGOOS() == "darwin" {
		overrides["DYLD_INSERT_LIBRARIES"] = prependPathList(preloadLib, envSliceValue(base, "DYLD_INSERT_LIBRARIES"))
	} else {
		overrides["LD_PRELOAD"] = prependPathList(preloadLib, envSliceValue(base, "LD_PRELOAD"))
	}
	for _, name := range sessionShimNames {
		if name == "command" {
			continue
		}
		realPath, ok := lookupExecutableInPath(name, basePath, cwd)
		if !ok {
			continue
		}
		key := realToolEnvKey(name)
		overrides[key] = realPath
		if signal, ok := executablePreloadSignal(realPath); ok {
			overrides[key+"_PATH_HASH"] = hashStringLocal(realPath)
			overrides[key+"_FILE_HASH"] = signal.fileHash
			overrides[key+"_STAT_SIGNAL"] = signal.statSignal
		}
	}
	if includeWarmFileReplay {
		overrides["SQUIRE_SHIM_ENABLE_WARM_FILE_REPLAY"] = "1"
	}
	return mergeEnv(base, overrides), nil
}

func prependPathList(value, existing string) string {
	if existing == "" {
		return value
	}
	return value + string(os.PathListSeparator) + existing
}

func lookupExecutableInPath(name, pathValue, cwd string) (string, bool) {
	if name == "" || strings.ContainsRune(name, os.PathSeparator) {
		return "", false
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		candidate := filepath.Join(dir, name)
		if isExecutableFile(candidate) {
			if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
				return resolved, true
			}
			return candidate, true
		}
	}
	return "", false
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

type preloadExecutableSignal struct {
	fileHash   string
	statSignal string
}

func executablePreloadSignal(path string) (preloadExecutableSignal, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return preloadExecutableSignal{}, false
	}
	contentHash, ok := hashFileLocal(path)
	if !ok {
		return preloadExecutableSignal{}, false
	}
	statSignal, ok := preloadFileStatSignal(info)
	if !ok {
		return preloadExecutableSignal{}, false
	}
	signal := filepath.Base(path) + "|" + contentHash + "|" + statSignal
	return preloadExecutableSignal{
		fileHash:   hashStringLocal(signal),
		statSignal: statSignal,
	}, true
}

func hashFileLocal(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func hashStringLocal(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func runtimeGOOS() string {
	if override := os.Getenv("GOOS_OVERRIDE_FOR_TEST"); override != "" {
		return strings.ToLower(override)
	}
	return runtime.GOOS
}

func runtimeGOARCH() string {
	if override := os.Getenv("GOARCH_OVERRIDE_FOR_TEST"); override != "" {
		return strings.ToLower(override)
	}
	return runtime.GOARCH
}

func realToolEnvKey(name string) string {
	var b strings.Builder
	b.WriteString("SQUIRE_REAL_")
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(toASCIIUpper(r))
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func toASCIIUpper(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - ('a' - 'A')
	}
	return r
}

func mergeEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	seen := map[string]bool{}
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if value, exists := overrides[key]; exists {
			out = append(out, key+"="+value)
			seen[key] = true
			continue
		}
		out = append(out, item)
		seen[key] = true
	}
	for key, value := range overrides {
		if !seen[key] {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func envSliceValue(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func envValue(env []string, key string) string {
	return envSliceValue(env, key)
}

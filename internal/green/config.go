package green

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/bmatcuk/doublestar/v4"
)

const (
	defaultQuiescence   = 750 * time.Millisecond
	defaultPollInterval = 30 * time.Second
	defaultTimeout      = 10 * time.Minute
	maxChecks           = 64
	maxPatternsPerCheck = 256
	maxCommandArgs      = 256
	maxConfigBytes      = 1 << 20
)

var ErrNotConfigured = errors.New("Squire Green is not configured")

type rawConfig struct {
	Version      int        `toml:"version"`
	Quiescence   string     `toml:"quiescence"`
	PollInterval string     `toml:"poll_interval"`
	Concurrency  int        `toml:"concurrency"`
	Checks       []rawCheck `toml:"check"`
}

type rawCheck struct {
	Name     string            `toml:"name"`
	Command  []string          `toml:"command"`
	Inputs   []string          `toml:"inputs"`
	Exclude  []string          `toml:"exclude"`
	CWD      string            `toml:"cwd"`
	Required *bool             `toml:"required"`
	Timeout  string            `toml:"timeout"`
	Env      map[string]string `toml:"env"`
}

func LoadConfig(repoRoot string) (Config, error) {
	canonical, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return Config{}, err
	}
	repoRoot = canonical
	path := filepath.Join(repoRoot, filepath.FromSlash(ConfigRelativePath))
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{Path: path}, ErrNotConfigured
		}
		return Config{}, err
	}
	if !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("Green config must be a regular file: %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	if len(b) > maxConfigBytes {
		return Config{}, fmt.Errorf("Green config exceeds %d bytes", maxConfigBytes)
	}
	var raw rawConfig
	metadata, err := toml.Decode(string(b), &raw)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		return Config{}, fmt.Errorf("unknown Green config fields: %s", strings.Join(keys, ", "))
	}
	return normalizeConfig(repoRoot, path, b, raw)
}

func normalizeConfig(repoRoot, path string, source []byte, raw rawConfig) (Config, error) {
	version := raw.Version
	if version == 0 {
		version = configVersion
	}
	if version != configVersion {
		return Config{}, fmt.Errorf("unsupported Green config version %d", version)
	}
	if len(raw.Checks) == 0 {
		return Config{}, errors.New("Green config must declare at least one [[check]]")
	}
	if len(raw.Checks) > maxChecks {
		return Config{}, fmt.Errorf("Green config has %d checks; maximum is %d", len(raw.Checks), maxChecks)
	}
	quiescence, err := parseBoundedDuration("quiescence", raw.Quiescence, defaultQuiescence, 100*time.Millisecond, time.Minute)
	if err != nil {
		return Config{}, err
	}
	poll, err := parseBoundedDuration("poll_interval", raw.PollInterval, defaultPollInterval, 100*time.Millisecond, time.Minute)
	if err != nil {
		return Config{}, err
	}
	concurrency := raw.Concurrency
	if concurrency == 0 {
		concurrency = defaultConcurrency()
	}
	if concurrency < 1 || concurrency > 8 {
		return Config{}, fmt.Errorf("concurrency must be between 1 and 8")
	}

	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return Config{}, err
	}
	root = filepath.Clean(root)
	config := Config{
		Path:         path,
		Digest:       digestBytes(source),
		Version:      version,
		Quiescence:   quiescence,
		PollInterval: poll,
		Concurrency:  concurrency,
		Checks:       make([]Check, 0, len(raw.Checks)),
	}
	names := make(map[string]struct{}, len(raw.Checks))
	requiredChecks := 0
	for i, candidate := range raw.Checks {
		check, err := normalizeCheck(root, candidate)
		if err != nil {
			return Config{}, fmt.Errorf("check %d: %w", i+1, err)
		}
		if _, exists := names[check.Name]; exists {
			return Config{}, fmt.Errorf("check %d: duplicate name %q", i+1, check.Name)
		}
		names[check.Name] = struct{}{}
		if check.Required {
			requiredChecks++
		}
		config.Checks = append(config.Checks, check)
	}
	if requiredChecks == 0 {
		return Config{}, errors.New("Green config must declare at least one required check")
	}
	return config, nil
}

func normalizeCheck(repoRoot string, raw rawCheck) (Check, error) {
	name := strings.TrimSpace(raw.Name)
	if !validCheckName(name) {
		return Check{}, errors.New("name must be 1-80 printable characters")
	}
	if len(raw.Command) == 0 || len(raw.Command) > maxCommandArgs {
		return Check{}, fmt.Errorf("%q command must contain 1-%d arguments", name, maxCommandArgs)
	}
	command := append([]string(nil), raw.Command...)
	for _, arg := range command {
		if strings.ContainsRune(arg, '\x00') {
			return Check{}, fmt.Errorf("%q command contains a NUL byte", name)
		}
	}
	if len(raw.Inputs) == 0 || len(raw.Inputs) > maxPatternsPerCheck {
		return Check{}, fmt.Errorf("%q inputs must contain 1-%d patterns", name, maxPatternsPerCheck)
	}
	inputs, err := normalizePatterns(raw.Inputs)
	if err != nil {
		return Check{}, fmt.Errorf("%q inputs: %w", name, err)
	}
	exclude, err := normalizePatterns(raw.Exclude)
	if err != nil {
		return Check{}, fmt.Errorf("%q exclude: %w", name, err)
	}
	cwd := raw.CWD
	if cwd == "" {
		cwd = "."
	}
	if filepath.IsAbs(cwd) {
		return Check{}, fmt.Errorf("%q cwd must be relative to the repository", name)
	}
	absCWD := filepath.Clean(filepath.Join(repoRoot, filepath.FromSlash(cwd)))
	if !pathWithinRoot(absCWD, repoRoot) {
		return Check{}, fmt.Errorf("%q cwd escapes the repository", name)
	}
	info, err := os.Stat(absCWD)
	if err != nil || !info.IsDir() {
		return Check{}, fmt.Errorf("%q cwd is not a readable directory: %s", name, cwd)
	}
	realCWD, err := filepath.EvalSymlinks(absCWD)
	if err != nil || !pathWithinRoot(realCWD, repoRoot) {
		return Check{}, fmt.Errorf("%q cwd resolves outside the repository", name)
	}
	timeout, err := parseBoundedDuration("timeout", raw.Timeout, defaultTimeout, 100*time.Millisecond, 24*time.Hour)
	if err != nil {
		return Check{}, fmt.Errorf("%q: %w", name, err)
	}
	required := true
	if raw.Required != nil {
		required = *raw.Required
	}
	env := make(map[string]string, len(raw.Env))
	for key, value := range raw.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return Check{}, fmt.Errorf("%q has invalid env entry %q", name, key)
		}
		env[key] = value
	}
	return Check{
		Name:     name,
		Command:  command,
		Inputs:   inputs,
		Exclude:  exclude,
		CWD:      filepath.Clean(realCWD),
		Required: required,
		Timeout:  timeout,
		Env:      env,
	}, nil
}

func validCheckName(name string) bool {
	if name == "" || len(name) > 80 {
		return false
	}
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func normalizePatterns(patterns []string) ([]string, error) {
	result := make([]string, 0, len(patterns))
	seen := make(map[string]struct{}, len(patterns))
	for _, original := range patterns {
		pattern := filepath.ToSlash(strings.TrimSpace(original))
		if pattern == "" || strings.ContainsRune(pattern, '\x00') || filepath.IsAbs(original) {
			return nil, fmt.Errorf("invalid pattern %q", original)
		}
		pattern = strings.TrimPrefix(pattern, "./")
		if pattern == ".." || strings.HasPrefix(pattern, "../") {
			return nil, fmt.Errorf("pattern escapes the repository: %q", original)
		}
		if !doublestar.ValidatePattern(pattern) {
			return nil, fmt.Errorf("invalid glob pattern %q", original)
		}
		if _, exists := seen[pattern]; exists {
			continue
		}
		seen[pattern] = struct{}{}
		result = append(result, pattern)
	}
	sort.Strings(result)
	return result, nil
}

func parseBoundedDuration(name, value string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	if duration < minimum || duration > maximum {
		return 0, fmt.Errorf("%s must be between %s and %s", name, minimum, maximum)
	}
	return duration, nil
}

func pathWithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func canonicalRepoRoot(repoRoot string) (string, error) {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("Green repository root is not a directory: %s", repoRoot)
	}
	return filepath.Clean(real), nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func defaultConcurrency() int {
	if runtime.NumCPU() < 2 {
		return 1
	}
	return 2
}

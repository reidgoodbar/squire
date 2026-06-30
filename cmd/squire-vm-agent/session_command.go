package main

import (
	"os"
	"path/filepath"
	"strings"
)

type guestRequest struct {
	Version      int               `json:"version"`
	CWD          string            `json:"cwd"`
	StoreRoot    string            `json:"store_root"`
	Argv         []string          `json:"argv"`
	Env          map[string]string `json:"env,omitempty"`
	Interactive  bool              `json:"interactive,omitempty"`
	TerminalRows int               `json:"terminal_rows,omitempty"`
	TerminalCols int               `json:"terminal_cols,omitempty"`
}

func guestSquireSessionCommand(squire string, argv []string) []string {
	command := []string{squire, "session", "--quiet"}
	switch guestSessionTransport(argv) {
	case "preload":
		command = append(command, "--preload")
	}
	command = append(command, "--")
	return append(command, argv...)
}

func guestCommandEnv(req guestRequest) []string {
	overrides := map[string]string{
		"SQUIRE_KERNEL_STORE_ROOT":          req.StoreRoot,
		"SQUIRE_STORE_ROOT":                 req.StoreRoot,
		"SQUIRE_SESSION_LOCAL_HOT_SNAPSHOT": "1",
	}
	for key, value := range req.Env {
		if guestEnvAllowed(key) {
			overrides[key] = value
		}
	}
	return mergeGuestEnv(os.Environ(), overrides)
}

func guestEnvAllowed(key string) bool {
	switch key {
	case "SQUIRE_VM_GUEST_SESSION_TRANSPORT",
		"SQUIRE_VM_GUEST_PRELOAD_LIB",
		"SQUIRE_PRELOAD_ENABLE",
		"SQUIRE_PRELOAD_LIB",
		"SQUIRE_PRELOAD_HELPER",
		"SQUIRE_PRELOAD_TRACE",
		"SQUIRE_SHIM_DEBUG",
		"SQUIRE_SHIM_REQUIRE_HIT",
		"SQUIRE_SHIM_DISABLE_EVENT_LOG",
		"SQUIRE_SHIM_ENABLE_WARM_FILE_REPLAY",
		"SQUIRE_SESSION_LOCAL_HOT_SNAPSHOT":
		return true
	default:
		return false
	}
}

func mergeGuestEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]bool, len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if value, exists := overrides[key]; exists {
				out = append(out, key+"="+value)
				seen[key] = true
				continue
			}
		}
		out = append(out, entry)
	}
	for key, value := range overrides {
		if !seen[key] {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func guestSessionTransport(argv []string) string {
	if len(argv) == 0 {
		return "auto"
	}
	switch strings.ToLower(os.Getenv("SQUIRE_VM_GUEST_SESSION_TRANSPORT")) {
	case "preload":
		return "preload"
	case "auto", "", "path-shims", "path_shims", "shims":
	default:
		return "auto"
	}
	if !guestIsCodexLauncher(argv[0]) {
		return "auto"
	}
	if guestPreloadAvailable() {
		return "preload"
	}
	return "auto"
}

func guestIsCodexLauncher(firstArg string) bool {
	name := filepath.Base(firstArg)
	return name == "codex" || strings.HasPrefix(name, "codex-")
}

func guestPreloadAvailable() bool {
	for _, candidate := range []string{
		os.Getenv("SQUIRE_VM_GUEST_PRELOAD_LIB"),
		os.Getenv("SQUIRE_PRELOAD_LIB"),
		"/usr/local/bin/squire-preload.so",
		"/squire-preload.so",
	} {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

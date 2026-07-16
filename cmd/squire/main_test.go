package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"squire.run/internal/proofcache"
)

func TestUsageTextDocumentsProductContract(t *testing.T) {
	text := usageText()
	for _, want := range []string{
		"Squire",
		"squire codex",
		"squire status",
		"squire doctor",
		"squire explain",
		"squire prepare",
		"squire version",
		"Agents use ordinary commands.",
		"original native execution path",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("usage text missing %q:\n%s", want, text)
		}
	}
}

func TestHelpTextForArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "global long help", args: []string{"--help"}, want: "usage:"},
		{name: "global help topic", args: []string{"help"}, want: "usage:"},
		{name: "codex topic", args: []string{"help", "codex"}, want: "canonical product path"},
		{name: "status topic", args: []string{"help", "status"}, want: "hit/miss counters"},
		{name: "doctor topic", args: []string{"help", "doctor"}, want: "required Codex helper"},
		{name: "explain topic", args: []string{"help", "explain"}, want: "never executed natively"},
		{name: "advanced topic", args: []string{"help", "advanced"}, want: "compatibility and diagnostics"},
		{name: "session topic", args: []string{"help", "session"}, want: "scoped Squire session"},
		{name: "vm topic", args: []string{"help", "vm"}, want: "isolated Linux execution mode"},
		{name: "vm session topic", args: []string{"vm", "session", "--help"}, want: "guest lifecycle runner"},
		{name: "version topic", args: []string{"help", "version"}, want: "build identity"},
		{name: "runtime run topic", args: []string{"runtime", "run", "--help"}, want: "The \"--\" delimiter is"},
		{name: "runtime maintain topic", args: []string{"help", "runtime", "maintain"}, want: "resident maintainer"},
		{name: "runtime adapter topic", args: []string{"help", "runtime", "adapter"}, want: "model still"},
		{name: "legacy runtime alias", args: []string{"help", "kernel", "status"}, want: "runtime readiness"},
		{name: "boost topic", args: []string{"boost", "-h"}, want: "no broad Codex speedup claim"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, ok := helpTextForArgs(tt.args)
			if !ok {
				t.Fatalf("helpTextForArgs(%v) did not detect help", tt.args)
			}
			if !strings.Contains(text, tt.want) {
				t.Fatalf("help text missing %q:\n%s", tt.want, text)
			}
		})
	}
}

func TestCodexVMOptionsForStatus(t *testing.T) {
	if got := codexCommand([]string{"exec", "Explain this repo"}); !reflect.DeepEqual(got, []string{"codex", "exec", "Explain this repo"}) {
		t.Fatalf("codexCommand = %#v", got)
	}

	unavailable := vmStatusReport{Available: false, Backend: vmBackendVirtualization}
	if _, ok := codexVMOptionsForStatus(unavailable, []string{"exec", "task"}); ok {
		t.Fatal("unavailable VM should not be selected")
	}

	available := vmStatusReport{Available: true, Backend: vmBackendExternalRunner, Runner: "/tmp/runner"}
	opts, ok := codexVMOptionsForStatus(available, []string{"exec", "task"})
	if !ok {
		t.Fatal("available VM should be selected")
	}
	if opts.Backend != vmBackendExternalRunner || opts.Runner != "/tmp/runner" {
		t.Fatalf("unexpected VM opts: %+v", opts)
	}
	if !opts.Quiet {
		t.Fatal("squire codex should launch VM sessions quietly")
	}
	if !reflect.DeepEqual(opts.Command, []string{"codex", "exec", "task"}) {
		t.Fatalf("VM command = %#v", opts.Command)
	}
}

func TestParseSessionOptions(t *testing.T) {
	opts, err := parseSessionOptions([]string{"--quiet", "--metadata-only", "--no-maintainer", "--enable-warm-file-replay", "--preload", "--preload-lib", "/tmp/preload.dylib", "--", "sh", "-lc", "git status --short"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Quiet || !opts.MetadataOnly || opts.NoWarm || !opts.NoMaintainer || !opts.EnableWarmFileReplay || !opts.Preload || opts.PreloadLib != "/tmp/preload.dylib" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
	if !reflect.DeepEqual(opts.Command, []string{"sh", "-lc", "git status --short"}) {
		t.Fatalf("command = %#v", opts.Command)
	}

	if _, err := parseSessionOptions([]string{"--quiet"}); err == nil {
		t.Fatal("missing delimiter should fail")
	}
	if _, err := parseSessionOptions([]string{"--metadata-only", "--no-warm", "--", "sh"}); err == nil {
		t.Fatal("conflicting warm options should fail")
	}
	if _, err := parseSessionOptions([]string{"--path-shims", "--", "sh"}); err == nil {
		t.Fatal("removed path-shim transport should fail")
	}
}

func TestSessionCommandIsCodex(t *testing.T) {
	for _, command := range [][]string{
		{"codex"},
		{"/usr/local/bin/codex", "exec", "task"},
		{"codex.exe"},
	} {
		if !sessionCommandIsCodex(command) {
			t.Fatalf("sessionCommandIsCodex(%v) = false, want true", command)
		}
	}
	for _, command := range [][]string{
		nil,
		{"sh", "-c", "codex"},
		{"squire"},
	} {
		if sessionCommandIsCodex(command) {
			t.Fatalf("sessionCommandIsCodex(%v) = true, want false", command)
		}
	}
}

func TestParseVMSessionOptions(t *testing.T) {
	opts, err := parseVMSessionOptions([]string{"--quiet", "--backend", "external-runner", "--runner", "/tmp/runner", "--", "codex", "exec", "task"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Quiet || opts.Backend != vmBackendExternalRunner || opts.Runner != "/tmp/runner" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
	if !reflect.DeepEqual(opts.Command, []string{"codex", "exec", "task"}) {
		t.Fatalf("command = %#v", opts.Command)
	}
	if _, err := parseVMSessionOptions([]string{"--backend", "bad", "--", "sh"}); err == nil {
		t.Fatal("bad backend should fail")
	}
	if _, err := parseVMSessionOptions([]string{"--quiet"}); err == nil {
		t.Fatal("missing delimiter should fail")
	}
}

func TestVMStatusLinuxLocal(t *testing.T) {
	t.Setenv("GOOS_OVERRIDE_FOR_TEST", "linux")
	t.Setenv("GOARCH_OVERRIDE_FOR_TEST", "arm64")
	report := detectVMStatus("/repo", "/store", vmBackendAuto, "")
	if !report.Available {
		t.Fatalf("linux-local should be available: %+v", report)
	}
	if report.Backend != vmBackendLinuxLocal {
		t.Fatalf("backend = %q, want linux-local", report.Backend)
	}
	if report.HostArch != "arm64" {
		t.Fatalf("host arch = %q", report.HostArch)
	}
	if report.ChangesAgentCommands {
		t.Fatal("VM mode should not change agent commands")
	}
	if report.UsesHostCommandShims {
		t.Fatal("VM mode should not use host command shims")
	}
	if !report.PreservesHostMacSemantics {
		t.Fatal("linux-local should preserve host semantics on Linux")
	}
}

func TestVMStatusDarwinRequiresHelperAndGuestConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GOOS_OVERRIDE_FOR_TEST", "darwin")
	t.Setenv("PATH", tmp)
	t.Setenv("SQUIRE_VM_HELPER", "")
	t.Setenv("SQUIRE_VM_DARWIN_HELPER", "")
	t.Setenv("SQUIRE_VM_RUNNER", "")
	report := detectVMStatus("/repo", "/store", vmBackendAuto, "")
	if report.Available {
		t.Fatalf("darwin VM should require a helper and guest config: %+v", report)
	}
	if report.Backend != vmBackendVirtualization {
		t.Fatalf("backend = %q, want virtualization-framework", report.Backend)
	}
	if report.PreservesHostMacSemantics {
		t.Fatal("Linux VM mode should not claim to preserve macOS host semantics")
	}
	text := vmStatusOut(report, outputShort)
	if !strings.Contains(text, "squire-vm-darwin helper is not installed") {
		t.Fatalf("short status missing helper diagnostic:\n%s", text)
	}
	if !strings.Contains(text, "uses_host_command_shims: false") {
		t.Fatalf("short status should make host shim boundary explicit:\n%s", text)
	}
}

func TestVMStatusDarwinHelperRequiresGuestConfig(t *testing.T) {
	tmp := t.TempDir()
	helper := filepath.Join(tmp, "squire-vm-darwin")
	writeExecutableScript(t, helper, `#!/bin/sh
printf '%s\n' '{"available":false,"framework_supported":true,"guest_configured":false,"diagnostics":["missing readable SQUIRE_VM_KERNEL or SQUIRE_VM_BUNDLE/kernel","missing readable SQUIRE_VM_INITRD or SQUIRE_VM_DISK"]}'
`)
	t.Setenv("GOOS_OVERRIDE_FOR_TEST", "darwin")
	t.Setenv("PATH", tmp)
	t.Setenv("SQUIRE_VM_HELPER", helper)
	t.Setenv("SQUIRE_VM_KERNEL", "")
	t.Setenv("SQUIRE_VM_INITRD", "")
	t.Setenv("SQUIRE_VM_DISK", "")

	report := detectVMStatus("/repo", "/store", vmBackendAuto, "")
	if report.Available {
		t.Fatalf("darwin VM should require guest assets: %+v", report)
	}
	if report.VMHelper != helper {
		t.Fatalf("vm helper = %q, want %q", report.VMHelper, helper)
	}
	if report.GuestConfigured {
		t.Fatal("guest should not be configured without kernel/initrd/disk")
	}
	text := vmStatusOut(report, outputShort)
	if !strings.Contains(text, "guest_configured: false") || !strings.Contains(text, "missing readable SQUIRE_VM_KERNEL") {
		t.Fatalf("short status missing guest config diagnostics:\n%s", text)
	}
}

func TestVMStatusDarwinHelperConfigured(t *testing.T) {
	tmp := t.TempDir()
	helper := filepath.Join(tmp, "squire-vm-darwin")
	kernelFile := filepath.Join(tmp, "kernel")
	initrdFile := filepath.Join(tmp, "initrd")
	writeExecutableScript(t, helper, `#!/bin/sh
printf '%s\n' '{"available":true,"framework_supported":true,"guest_configured":true,"diagnostics":["guest kernel configuration is present"]}'
`)
	if err := os.WriteFile(kernelFile, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrdFile, []byte("initrd"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOS_OVERRIDE_FOR_TEST", "darwin")
	t.Setenv("PATH", tmp)
	t.Setenv("SQUIRE_VM_HELPER", helper)
	t.Setenv("SQUIRE_VM_KERNEL", kernelFile)
	t.Setenv("SQUIRE_VM_INITRD", initrdFile)
	t.Setenv("SQUIRE_VM_AGENT_PORT", "2048")

	report := detectVMStatus("/repo", "/store", vmBackendAuto, "")
	if !report.Available {
		t.Fatalf("darwin VM should be available with helper and guest assets: %+v", report)
	}
	if !report.GuestConfigured {
		t.Fatal("guest should be configured")
	}
	if report.GuestAgentPort != 2048 {
		t.Fatalf("guest agent port = %d, want 2048", report.GuestAgentPort)
	}
	json := vmStatusOut(report, outputJSON)
	if !strings.Contains(json, `"backend": "virtualization-framework"`) || !strings.Contains(json, `"guest_configured": true`) {
		t.Fatalf("json status missing virtualization fields:\n%s", json)
	}
}

func TestVMStatusExternalRunner(t *testing.T) {
	tmp := t.TempDir()
	runner := filepath.Join(tmp, "runner")
	writeExecutable(t, runner)
	t.Setenv("GOOS_OVERRIDE_FOR_TEST", "darwin")
	report := detectVMStatus("/repo", "/store", vmBackendAuto, runner)
	if !report.Available {
		t.Fatalf("external runner should be available: %+v", report)
	}
	if report.Backend != vmBackendExternalRunner {
		t.Fatalf("backend = %q, want external-runner", report.Backend)
	}
	if report.Runner != runner {
		t.Fatalf("runner = %q, want %q", report.Runner, runner)
	}
	json := vmStatusOut(report, outputJSON)
	if !strings.Contains(json, `"backend": "external-runner"`) || !strings.Contains(json, `"available": true`) {
		t.Fatalf("json status missing external runner fields:\n%s", json)
	}
}

func TestVMExternalRunnerArgs(t *testing.T) {
	args := vmExternalRunnerArgs("/repo", "/store", []string{"codex", "exec", "task"})
	want := []string{"session", "--cwd", "/repo", "--store-root", "/store", "--", "codex", "exec", "task"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("runner args = %#v, want %#v", args, want)
	}
}

func TestBuildPreloadSessionEnvironment(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "python3"} {
		writeExecutable(t, filepath.Join(bin, name))
	}
	preload := filepath.Join(tmp, "squire-preload.dylib")
	if err := os.WriteFile(preload, []byte("lib"), 0o644); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(tmp, "squire-preload-helper")
	writeExecutable(t, helper)
	t.Setenv("GOOS_OVERRIDE_FOR_TEST", "darwin")
	env, err := buildPreloadSessionEnvironment(tmp, preload, helper, []string{"PATH=" + bin, "DYLD_INSERT_LIBRARIES=/existing/lib.dylib"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := envSliceValue(env, "SQUIRE_PRELOAD_ENABLE"); got != "1" {
		t.Fatalf("SQUIRE_PRELOAD_ENABLE = %q", got)
	}
	if got := envSliceValue(env, "SQUIRE_PRELOAD_LIB"); got != preload {
		t.Fatalf("SQUIRE_PRELOAD_LIB = %q", got)
	}
	if got := envSliceValue(env, "SQUIRE_PRELOAD_HELPER"); got != helper {
		t.Fatalf("SQUIRE_PRELOAD_HELPER = %q", got)
	}
	if got := envSliceValue(env, "DYLD_INSERT_LIBRARIES"); got != preload+string(os.PathListSeparator)+"/existing/lib.dylib" {
		t.Fatalf("DYLD_INSERT_LIBRARIES = %q", got)
	}
	if got := envSliceValue(env, "PATH"); got != bin {
		t.Fatalf("PATH should not be shimmed in preload mode: %q", got)
	}
	wantGit, err := filepath.EvalSymlinks(filepath.Join(bin, "git"))
	if err != nil {
		t.Fatal(err)
	}
	if got := envSliceValue(env, "SQUIRE_REAL_GIT"); got != wantGit {
		t.Fatalf("SQUIRE_REAL_GIT = %q, want %q", got, wantGit)
	}
	for _, key := range []string{
		"SQUIRE_REAL_GIT_PATH_HASH",
		"SQUIRE_REAL_GIT_FILE_HASH",
		"SQUIRE_REAL_GIT_STAT_SIGNAL",
	} {
		if got := envSliceValue(env, key); got == "" {
			t.Fatalf("%s should be precomputed for preload hot-path validation", key)
		}
	}
	if got := envSliceValue(env, "SQUIRE_SHIM_REAL_PATH"); got != bin {
		t.Fatalf("SQUIRE_SHIM_REAL_PATH = %q", got)
	}
}

func TestIsPreloadUnsafeLauncher(t *testing.T) {
	for _, command := range []string{"python", "/opt/homebrew/bin/python3", "/opt/homebrew/bin/python3.14", "pip", "pip3.14"} {
		if !isPreloadUnsafeLauncher(command) {
			t.Fatalf("%s should be treated as preload unsafe", command)
		}
	}
	t.Setenv("GOOS_OVERRIDE_FOR_TEST", "darwin")
	for _, command := range []string{"sh", "/bin/zsh", "/usr/bin/env"} {
		if !isPreloadUnsafeLauncher(command) {
			t.Fatalf("%s should be treated as preload unsafe on darwin", command)
		}
	}
	t.Setenv("GOOS_OVERRIDE_FOR_TEST", "linux")
	for _, command := range []string{"sh", "/bin/zsh", "/usr/bin/env", "codex", "node"} {
		if isPreloadUnsafeLauncher(command) {
			t.Fatalf("%s should not be treated as preload unsafe", command)
		}
	}
}

func TestVersionOutput(t *testing.T) {
	oldVersion, oldCommit, oldDate := buildVersion, buildCommit, buildDate
	t.Cleanup(func() {
		buildVersion, buildCommit, buildDate = oldVersion, oldCommit, oldDate
	})
	buildVersion = "1.2.3"
	buildCommit = "abc123"
	buildDate = "2026-06-18"

	text := versionOut(outputShort)
	for _, want := range []string{
		"Squire 1.2.3",
		"contract: v1",
		"commit: abc123",
		"date: 2026-06-18",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("version short output missing %q:\n%s", want, text)
		}
	}

	json := versionOut(outputJSON)
	for _, want := range []string{
		`"product": "Squire"`,
		`"contract": "v1"`,
		`"version": "1.2.3"`,
		`"commit": "abc123"`,
		`"date": "2026-06-18"`,
	} {
		if !strings.Contains(json, want) {
			t.Fatalf("version json output missing %q:\n%s", want, json)
		}
	}
}

func TestHelpTextDoesNotInterceptCommandHelpAfterDelimiter(t *testing.T) {
	if text, ok := helpTextForArgs([]string{"runtime", "run", "--", "git", "--help"}); ok {
		t.Fatalf("help intercepted command argv after --:\n%s", text)
	}
}

func TestParseAdapterOptionsDefaultsToProductLifecycle(t *testing.T) {
	opts, err := parseAdapterOptions([]string{"--stdio"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Stdio {
		t.Fatal("--stdio was not parsed")
	}
	if !opts.EnsureMaintainer {
		t.Fatal("adapter should ensure the resident maintainer by default")
	}
}

func TestParseAdapterOptionsNoMaintainerDiagnosticEscape(t *testing.T) {
	opts, err := parseAdapterOptions([]string{"--stdio", "--no-maintainer"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Stdio {
		t.Fatal("--stdio was not parsed")
	}
	if opts.EnsureMaintainer {
		t.Fatal("--no-maintainer should disable automatic maintainer lifecycle")
	}
}

func TestParseAdapterOptionsAcceptsLegacyEnsureMaintainer(t *testing.T) {
	opts, err := parseAdapterOptions([]string{"--stdio", "--ensure-maintainer"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.EnsureMaintainer {
		t.Fatal("--ensure-maintainer should remain accepted as a compatibility no-op")
	}
}

func TestRuntimeUsageError(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing subcommand", args: nil, want: "missing runtime subcommand"},
		{name: "unknown subcommand", args: []string{"stats"}, want: `unknown runtime subcommand "stats"`},
		{name: "bad status option", args: []string{"status", "--json"}, want: `unknown runtime status option "--json"`},
		{name: "missing run delimiter", args: []string{"run"}, want: "requires -- before the command"},
		{name: "bad adapter usage", args: []string{"adapter"}, want: "invalid runtime adapter usage"},
		{name: "bad warm option", args: []string{"warm", "--short"}, want: `does not accept option "--short"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runtimeUsageError(tt.args)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("runtimeUsageError(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestRuntimeAdapterServesMultipleRequestsOverOneProcess(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	requests := []adapterRequest{
		{ID: "head", CWD: repo, Argv: []string{"git", "rev-parse", "HEAD"}},
		{ID: "branch", CWD: repo, Argv: []string{"git", "rev-parse", "--abbrev-ref", "HEAD"}},
	}
	var in bytes.Buffer
	for _, req := range requests {
		if err := json.NewEncoder(&in).Encode(req); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := server.serve(ctx, &in, &out); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("adapter returned %d response lines, want 2:\n%s", len(lines), out.String())
	}
	got := make(map[string]adapterResponse)
	for _, line := range lines {
		var resp adapterResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("response is not JSON: %s\n%v", line, err)
		}
		if !resp.OK {
			t.Fatalf("adapter response failed: %+v", resp)
		}
		got[resp.ID] = resp
	}
	assertAdapterStdout(t, got["head"], runGit(t, repo, "rev-parse", "HEAD"))
	assertAdapterStdout(t, got["branch"], runGit(t, repo, "rev-parse", "--abbrev-ref", "HEAD"))
	if len(server.engines) != 1 {
		t.Fatalf("adapter constructed %d engine instances, want one reused instance", len(server.engines))
	}
}

func TestAdapterEnvOverlayRestoresProcessEnvironment(t *testing.T) {
	const key = "SQUIRE_ADAPTER_TEST_ENV"
	t.Setenv(key, "before")
	resp := withAdapterEnv(map[string]string{key: "during"}, false, func() adapterResponse {
		if got := os.Getenv(key); got != "during" {
			return adapterResponse{OK: false, Error: "env during callback = " + got}
		}
		return adapterResponse{OK: true}
	})
	if !resp.OK {
		t.Fatal(resp.Error)
	}
	if got := os.Getenv(key); got != "before" {
		t.Fatalf("env after callback = %q, want before", got)
	}
}

func TestAdapterFastResponseWriterMatchesJSONSemantics(t *testing.T) {
	resp := adapterResponse{
		ID:                "id-with-quote-\"",
		OK:                true,
		ReplayHit:         true,
		MissReason:        "not used",
		StdoutB64:         base64.StdEncoding.EncodeToString([]byte("hello\n")),
		StderrB64:         base64.StdEncoding.EncodeToString([]byte("warn\n")),
		ExitCode:          7,
		Mode:              proofcache.ModeReplay,
		Family:            proofcache.FamilyRepoState,
		Proof:             "mmap-hot-snapshot",
		NativeWallMS:      11,
		Diagnostics:       []string{"line\nbreak", "quote\""},
		MaintainerRunning: true,
		MaintainerAlready: true,
	}
	var fast bytes.Buffer
	writeAdapterResponseFast(&fast, resp)
	var got adapterResponse
	if err := json.Unmarshal(bytes.TrimSpace(fast.Bytes()), &got); err != nil {
		t.Fatalf("fast response is invalid JSON: %s\n%v", fast.String(), err)
	}
	if !reflect.DeepEqual(got, resp) {
		t.Fatalf("decoded fast response mismatch\ngot:  %+v\nwant: %+v\njson: %s", got, resp, fast.String())
	}

	var slow bytes.Buffer
	if err := writeAdapterResponseSlow(&slow, resp); err != nil {
		t.Fatal(err)
	}
	var slowMap, fastMap map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(slow.Bytes()), &slowMap); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytes.TrimSpace(fast.Bytes()), &fastMap); err != nil {
		t.Fatal(err)
	}
	if !jsonMapsEqual(slowMap, fastMap) {
		t.Fatalf("fast JSON differs from standard encoder\nfast=%s\nslow=%s", fast.String(), slow.String())
	}
}

func TestAdapterResponseWriterKeepsDebugPhases(t *testing.T) {
	resp := adapterResponse{
		ID:       "debug",
		OK:       true,
		ExitCode: 0,
		Mode:     proofcache.ModeReplay,
		Family:   proofcache.FamilyLocalRepoMetadata,
		Phases:   &proofcache.PhaseTimings{ClassifyMS: 1.25},
	}
	var out bytes.Buffer
	if err := writeAdapterResponse(&out, resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"phases"`) {
		t.Fatalf("debug response missing phases: %s", out.String())
	}
	var got adapterResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	if got.Phases == nil || got.Phases.ClassifyMS != resp.Phases.ClassifyMS {
		t.Fatalf("decoded phases = %+v, want %+v", got.Phases, resp.Phases)
	}
}

func TestAdapterPlanCacheCopiesArgv(t *testing.T) {
	server := &adapterServer{plans: make(map[string]adapterCommandPlan)}
	cwd := t.TempDir()
	argv := []string{"git", "status", "--short"}
	shortPlan := server.planFor(cwd, argv)
	if shortPlan.key == "" {
		t.Fatal("short plan has empty key")
	}
	argv[2] = "--porcelain"
	porcelainPlan := server.planFor(cwd, argv)
	if porcelainPlan.key == shortPlan.key {
		t.Fatalf("mutated argv reused stale plan key %q", porcelainPlan.key)
	}
	argv[2] = "--short"
	again := server.planFor(cwd, argv)
	if again.key != shortPlan.key {
		t.Fatalf("short plan key = %q, want cached %q", again.key, shortPlan.key)
	}
	if len(server.plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(server.plans))
	}
}

func TestRuntimeAdapterNativeDirectSkipsMaintainerAndEngine(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		ensureMaintainer: true,
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	req := adapterRequest{
		ID:   "never",
		CWD:  repo,
		Argv: []string{"python3", "-m", "unittest", "-h"},
	}
	resp := server.handleRequest(ctx, req)
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if resp.Mode != proofcache.ModeNever {
		t.Fatalf("mode = %s, want never", resp.Mode)
	}
	if resp.Family != proofcache.FamilyValidation {
		t.Fatalf("family = %s, want validation", resp.Family)
	}
	if len(server.engines) != 0 {
		t.Fatalf("native-direct path constructed %d engine instances", len(server.engines))
	}
	if len(server.maintainers) != 0 {
		t.Fatalf("native-direct path touched maintainer state: %+v", server.maintainers)
	}
	assertAdapterStdout(t, resp, runCommand(t, repo, "python3", "-m", "unittest", "-h"))
}

func TestStoreRootForPrefersCanonicalEnvironment(t *testing.T) {
	canonical := filepath.Join(t.TempDir(), "canonical")
	legacy := filepath.Join(t.TempDir(), "legacy")
	t.Setenv("SQUIRE_STORE_ROOT", canonical)
	t.Setenv("SQUIRE_KERNEL_STORE_ROOT", legacy)
	if got := storeRootFor(t.TempDir()); got != canonical {
		t.Fatalf("storeRootFor canonical = %q, want %q", got, canonical)
	}

	t.Setenv("SQUIRE_STORE_ROOT", "")
	if got := storeRootFor(t.TempDir()); got != legacy {
		t.Fatalf("storeRootFor legacy fallback = %q, want %q", got, legacy)
	}
}

func TestRuntimeAdapterReplayOnlyNeverRunsNativeForDisallowedCommand(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		ensureMaintainer: true,
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	req := adapterRequest{
		ID:         "never",
		CWD:        repo,
		Argv:       []string{"python3", "-m", "unittest", "-h"},
		ReplayOnly: true,
	}
	resp := server.handleRequest(ctx, req)
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if !resp.ReplayMiss || resp.ReplayHit {
		t.Fatalf("replay flags = hit:%t miss:%t, want miss only", resp.ReplayHit, resp.ReplayMiss)
	}
	if resp.MissReason == "" {
		t.Fatalf("missing miss reason: %+v", resp)
	}
	if resp.StdoutB64 != "" || resp.StderrB64 != "" || resp.Mode != "" {
		t.Fatalf("replay-only miss returned command output or mode: %+v", resp)
	}
	if len(server.engines) != 0 {
		t.Fatalf("replay-only disallowed path constructed %d engine instances", len(server.engines))
	}
	if len(server.maintainers) != 0 {
		t.Fatalf("replay-only disallowed path touched maintainer state: %+v", server.maintainers)
	}
}

func TestRuntimeAdapterCachesPlanAndHotMiss(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	req := adapterRequest{
		ID:   "status",
		CWD:  repo,
		Argv: []string{"git", "status", "--short"},
	}
	first := server.handleRequest(ctx, req)
	if !first.OK {
		t.Fatalf("first response failed: %+v", first)
	}
	if first.Mode != proofcache.ModeNative {
		t.Fatalf("first mode = %s, want native cold miss", first.Mode)
	}
	assertAdapterStdout(t, first, runGit(t, repo, "status", "--short"))
	if len(server.plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(server.plans))
	}
	if len(server.hotMisses) != 1 {
		t.Fatalf("hot misses = %d, want 1", len(server.hotMisses))
	}
	second := server.handleRequest(ctx, req)
	if !second.OK {
		t.Fatalf("second response failed: %+v", second)
	}
	if second.Mode != proofcache.ModeNative {
		t.Fatalf("second mode = %s, want native hot miss memo", second.Mode)
	}
	assertAdapterStdout(t, second, runGit(t, repo, "status", "--short"))
	if len(server.plans) != 1 {
		t.Fatalf("plans after second request = %d, want 1", len(server.plans))
	}
	if len(server.hotMisses) != 1 {
		t.Fatalf("hot misses after second request = %d, want 1", len(server.hotMisses))
	}
}

func TestRuntimeAdapterReplayOnlyColdMissDoesNotRunNative(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	ensure := false
	req := adapterRequest{
		ID:               "status",
		CWD:              repo,
		Argv:             []string{"git", "status", "--short"},
		ReplayOnly:       true,
		EnsureMaintainer: &ensure,
	}
	resp := server.handleRequest(ctx, req)
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if !resp.ReplayMiss || resp.ReplayHit {
		t.Fatalf("replay flags = hit:%t miss:%t, want miss only", resp.ReplayHit, resp.ReplayMiss)
	}
	if resp.StdoutB64 != "" || resp.StderrB64 != "" || resp.Mode != "" {
		t.Fatalf("cold replay-only miss returned command output or mode: %+v", resp)
	}
	if len(server.hotMisses) != 1 {
		t.Fatalf("hot misses = %d, want 1", len(server.hotMisses))
	}
}

func TestRuntimeAdapterReplayOnlyComposedPipeline(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	state := server.stateFor(repo)
	warmRuntimeReplay(t, ctx, state.engine, repo, []string{"git", "rev-parse", "HEAD"})

	ensure := false
	req := adapterRequest{
		ID:               "script",
		CWD:              repo,
		Script:           "git rev-parse HEAD | head -n 1",
		ReplayOnly:       true,
		EnsureMaintainer: &ensure,
	}
	resp := server.handleRequest(ctx, req)
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if !resp.ReplayHit || resp.ReplayMiss {
		t.Fatalf("replay flags = hit:%t miss:%t, want hit only (%+v)", resp.ReplayHit, resp.ReplayMiss, resp)
	}
	assertAdapterStdout(t, resp, runCommand(t, repo, "sh", "-c", req.Script))
	if resp.Proof != "composed-shell-adapter" {
		t.Fatalf("proof = %q, want composed-shell-adapter", resp.Proof)
	}
}

func TestRuntimeAdapterReplayOnlyComposedSequenceAndRedirection(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	state := server.stateFor(repo)
	warmRuntimeReplay(t, ctx, state.engine, repo, []string{"git", "rev-parse", "HEAD"})
	warmRuntimeReplay(t, ctx, state.engine, repo, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"})

	ensure := false
	req := adapterRequest{
		ID:               "script",
		CWD:              repo,
		Script:           "git rev-parse HEAD >/dev/null; git rev-parse --abbrev-ref HEAD",
		ReplayOnly:       true,
		EnsureMaintainer: &ensure,
	}
	resp := server.handleRequest(ctx, req)
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if !resp.ReplayHit || resp.ReplayMiss {
		t.Fatalf("replay flags = hit:%t miss:%t, want hit only (%+v)", resp.ReplayHit, resp.ReplayMiss, resp)
	}
	assertAdapterStdout(t, resp, runCommand(t, repo, "sh", "-c", req.Script))
}

func TestRuntimeAdapterReplayOnlyComposedTailGrepAndShortCircuit(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	state := server.stateFor(repo)
	warmRuntimeReplay(t, ctx, state.engine, repo, []string{"git", "rev-parse", "HEAD"})

	head := strings.TrimSpace(string(runGit(t, repo, "rev-parse", "HEAD")))
	ensure := false
	script := "git rev-parse HEAD | tail -n 1 | grep -F " + head[:12]
	resp := server.handleRequest(ctx, adapterRequest{
		ID:               "tail-grep",
		CWD:              repo,
		Script:           script,
		ReplayOnly:       true,
		EnsureMaintainer: &ensure,
	})
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if !resp.ReplayHit || resp.ReplayMiss {
		t.Fatalf("replay flags = hit:%t miss:%t, want hit only (%+v)", resp.ReplayHit, resp.ReplayMiss, resp)
	}
	assertAdapterStdout(t, resp, runCommand(t, repo, "sh", "-c", script))

	shortCircuit := "git rev-parse HEAD | grep -q -F definitely_missing_squire_pattern && git rev-parse --git-dir"
	resp = server.handleRequest(ctx, adapterRequest{
		ID:               "short-circuit",
		CWD:              repo,
		Script:           shortCircuit,
		ReplayOnly:       true,
		EnsureMaintainer: &ensure,
	})
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if !resp.ReplayHit || resp.ReplayMiss {
		t.Fatalf("replay flags = hit:%t miss:%t, want hit only (%+v)", resp.ReplayHit, resp.ReplayMiss, resp)
	}
	stdout, stderr, code := runCommandRaw(repo, "sh", "-c", shortCircuit)
	assertAdapterOutput(t, resp, stdout, stderr, code)
}

func TestRuntimeAdapterReplayOnlyComposedUnsupportedMisses(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	ensure := false
	resp := server.handleRequest(ctx, adapterRequest{
		ID:               "script",
		CWD:              repo,
		Script:           "python3 -m unittest -h | head -n 1",
		ReplayOnly:       true,
		EnsureMaintainer: &ensure,
	})
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if !resp.ReplayMiss || resp.ReplayHit {
		t.Fatalf("replay flags = hit:%t miss:%t, want miss only", resp.ReplayHit, resp.ReplayMiss)
	}
	if resp.StdoutB64 != "" || resp.StderrB64 != "" || resp.Mode != "" {
		t.Fatalf("unsupported composed miss returned output or mode: %+v", resp)
	}
}

func TestRuntimeAdapterReplayOnlyComposedForLoopAndPrintf(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	state := server.stateFor(repo)
	warmRuntimeReplay(t, ctx, state.engine, repo, []string{"git", "rev-parse", "HEAD"})

	ensure := false
	script := "for i in 1 2; do git rev-parse HEAD; done; printf 'SQUIRE_CODEX_AB_OK\\n'"
	resp := server.handleRequest(ctx, adapterRequest{
		ID:               "for-loop",
		CWD:              repo,
		Script:           script,
		ReplayOnly:       true,
		EnsureMaintainer: &ensure,
	})
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if !resp.ReplayHit || resp.ReplayMiss {
		t.Fatalf("replay flags = hit:%t miss:%t, want hit only (%+v)", resp.ReplayHit, resp.ReplayMiss, resp)
	}
	stdout, stderr, code := runCommandRaw(repo, "sh", "-c", script)
	assertAdapterOutput(t, resp, stdout, stderr, code)
}

func TestRuntimeAdapterReplayOnlyComposedNewlineBlock(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	state := server.stateFor(repo)
	warmRuntimeReplay(t, ctx, state.engine, repo, []string{"git", "rev-parse", "HEAD"})

	ensure := false
	script := "git rev-parse HEAD\ngit rev-parse HEAD\nprintf 'SQUIRE_CODEX_AB_OK\\n'"
	resp := server.handleRequest(ctx, adapterRequest{
		ID:               "newline-block",
		CWD:              repo,
		Script:           script,
		ReplayOnly:       true,
		EnsureMaintainer: &ensure,
	})
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if !resp.ReplayHit || resp.ReplayMiss {
		t.Fatalf("replay flags = hit:%t miss:%t, want hit only (%+v)", resp.ReplayHit, resp.ReplayMiss, resp)
	}
	stdout, stderr, code := runCommandRaw(repo, "sh", "-c", script)
	assertAdapterOutput(t, resp, stdout, stderr, code)
}

func TestRuntimeAdapterReplayOnlyComposedUnsafeForLoopMisses(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		engines:          make(map[string]*proofcache.Engine),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	ensure := false
	resp := server.handleRequest(ctx, adapterRequest{
		ID:               "unsafe-loop",
		CWD:              repo,
		Script:           "for i in 1 2; do git add -h >/dev/null; done",
		ReplayOnly:       true,
		EnsureMaintainer: &ensure,
	})
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if !resp.ReplayMiss || resp.ReplayHit {
		t.Fatalf("replay flags = hit:%t miss:%t, want miss only", resp.ReplayHit, resp.ReplayMiss)
	}
}

func TestBoostUsageError(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing subcommand", args: nil, want: "missing boost subcommand"},
		{name: "unknown subcommand", args: []string{"stats"}, want: `unknown boost subcommand "stats"`},
		{name: "bad status option", args: []string{"status", "--bogus"}, want: `does not accept option "--bogus"`},
		{name: "missing bench target", args: []string{"bench"}, want: "missing boost bench target"},
		{name: "bad bench target", args: []string{"bench", "all"}, want: `unknown boost bench target "all"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boostUsageError(tt.args)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("boostUsageError(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestCommandAfterDelimiter(t *testing.T) {
	argv, err := commandAfterDelimiter("squire runtime run", []string{"--", "git", "status", "--short"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv, " ") != "git status --short" {
		t.Fatalf("argv = %q", strings.Join(argv, " "))
	}

	for _, args := range [][]string{
		nil,
		{"git", "status"},
		{"--"},
	} {
		if _, err := commandAfterDelimiter("squire runtime run", args); err == nil {
			t.Fatalf("commandAfterDelimiter(%v) returned nil error", args)
		}
	}
}

func TestSplitOutputFormatFlag(t *testing.T) {
	args, format, err := splitOutputFormatFlag([]string{"--background-status", "--short"})
	if err != nil {
		t.Fatal(err)
	}
	if format != outputShort {
		t.Fatalf("format = %v, want outputShort", format)
	}
	if strings.Join(args, " ") != "--background-status" {
		t.Fatalf("args = %q", strings.Join(args, " "))
	}

	args, format, err = splitOutputFormatFlag([]string{"--once", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if format != outputJSON {
		t.Fatalf("format = %v, want outputJSON", format)
	}
	if strings.Join(args, " ") != "--once" {
		t.Fatalf("args = %q", strings.Join(args, " "))
	}

	if _, _, err := splitOutputFormatFlag([]string{"--json", "--short"}); err == nil {
		t.Fatalf("splitOutputFormatFlag accepted conflicting output flags")
	}
}

func TestOutputFormatFromTrailingArgs(t *testing.T) {
	format, err := outputFormatFromTrailingArgs([]string{"--short"})
	if err != nil {
		t.Fatal(err)
	}
	if format != outputShort {
		t.Fatalf("format = %v, want outputShort", format)
	}
	if _, err := outputFormatFromTrailingArgs([]string{"--bogus"}); err == nil {
		t.Fatalf("outputFormatFromTrailingArgs accepted unknown option")
	}

	format, err = outputFormatFromTrailingArgsDefault(nil, outputShort)
	if err != nil {
		t.Fatal(err)
	}
	if format != outputShort {
		t.Fatalf("default format = %v, want outputShort", format)
	}
}

func TestBoostStatusOutputFormats(t *testing.T) {
	report := proofcache.BoostStatusReport{
		Claim:                        "scoped",
		EnabledFastPaths:             []string{"git rev-parse HEAD"},
		ProofGatedReplayCandidates:   []string{"cat <bounded workspace source/config file>"},
		Replays:                      3,
		NativeFallbacks:              2,
		HotClientReplays:             1,
		HotClientGoReplays:           0,
		HotClientPreparedReplays:     0,
		HotClientSyntheticReplays:    1,
		HotClientCurrentFileReplays:  1,
		HotClientNativeFallbacks:     0,
		HotClientNativeAvoidedMS:     9,
		HotClientReplayWallUS:        1200,
		HotClientReplayWallMeasured:  1,
		HotClientReplayWallAvgUS:     1200,
		HotClientNetSavedMeasuredMS:  8,
		HotClientEventLogPath:        "/tmp/squire/hot_client_events.log",
		HotClientEventLogExists:      true,
		HotClientEventLogBytes:       42,
		DiagnosticMismatches:         1,
		DiagnosticMismatchCategories: map[string]int{"ordering": 1},
		DiagnosticSampleSkips:        4,
		Invalidations:                "derived from epoch mismatch",
		ROIHistoryMS:                 []int64{5, 6},
		NativeFallbackAvailable:      true,
		RuntimeDecisions:             "replay_or_native",
	}
	text := boostStatusOut(report, outputShort)
	for _, want := range []string{
		"Squire acceleration status",
		"git rev-parse HEAD",
		"replays: 3",
		"native_fallbacks: 2",
		"hot_client_replays: 1",
		"hot_client_synthetic_replays: 1",
		"hot_client_current_file_replays: 1",
		"hot_client_replay_wall_avg_us: 1200",
		"hot_client_net_saved_measured_ms: 8",
		"hot_client_event_log_path: /tmp/squire/hot_client_events.log",
		"hot_client_event_log_exists: true",
		"hot_client_event_log_bytes: 42",
		"native_fallback_available: true",
		"runtime_decisions: replay_or_native",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("boost short output missing %q:\n%s", want, text)
		}
	}
	json := boostStatusOut(report, outputJSON)
	for _, want := range []string{
		`"claim": "scoped"`,
		`"replays": 3`,
		`"hot_client_event_log_exists": true`,
		`"native_fallback_available": true`,
		`"runtime_decisions": "replay_or_native"`,
	} {
		if !strings.Contains(json, want) {
			t.Fatalf("boost json output missing %q:\n%s", want, json)
		}
	}
}

func TestHotEventPipeRecordsValidReplayEvents(t *testing.T) {
	storeRoot := t.TempDir()
	pipe := startHotEventPipe(storeRoot)
	if pipe == nil {
		t.Fatal("expected hot event pipe")
	}
	_, _ = pipe.writer.WriteString("123 replay c-mmap-hot-snapshot 7 11\n")
	_, _ = pipe.writer.WriteString("124 replay c-mmap-hot-synthetic 9 13\n")
	_, _ = pipe.writer.WriteString("125 replay c-current-file 0 17\n")
	_, _ = pipe.writer.WriteString("not an event\n")
	_ = pipe.writer.Close()
	finishHotEventPipe(pipe)
	stats := proofcache.LoadHotClientStats(storeRoot)
	if stats.Replays != 3 {
		t.Fatalf("replays = %d, want 3", stats.Replays)
	}
	if stats.PreparedChildReplays != 1 {
		t.Fatalf("prepared replays = %d, want 1", stats.PreparedChildReplays)
	}
	if stats.SyntheticReplays != 1 {
		t.Fatalf("synthetic replays = %d, want 1", stats.SyntheticReplays)
	}
	if stats.CurrentFileReplays != 1 {
		t.Fatalf("current-file replays = %d, want 1", stats.CurrentFileReplays)
	}
	if stats.NativeWallAvoidedMS != 16 {
		t.Fatalf("native wall avoided = %d, want 16", stats.NativeWallAvoidedMS)
	}
	if stats.ReplayWallUS != 41 {
		t.Fatalf("replay wall = %d, want 41", stats.ReplayWallUS)
	}
}

func TestOpenHotSnapshotFile(t *testing.T) {
	storeRoot := t.TempDir()
	if openHotSnapshotFile(storeRoot) != nil {
		t.Fatal("missing snapshot should not open")
	}
	want := []byte("snapshot")
	if err := os.WriteFile(filepath.Join(storeRoot, "hot_snapshot.bin"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	file := openHotSnapshotFile(storeRoot)
	if file == nil {
		t.Fatal("expected snapshot file")
	}
	defer file.Close()
	got := make([]byte, len(want))
	_, err := file.ReadAt(got, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("snapshot bytes = %q, want %q", got, want)
	}
}

func TestRepoMetadataBenchShortOutput(t *testing.T) {
	report := proofcache.BenchReport{
		Exactness:                    true,
		Mismatches:                   0,
		MutationBoundaryInvalidation: true,
		WorkloadOnlyWallDeltaMS:      12,
		NetROIMS:                     10,
		NoBroadCodexSpeedupClaim:     true,
		Commands:                     []string{"git rev-parse HEAD"},
	}
	text := repoMetadataBenchOut(report, outputShort)
	for _, want := range []string{
		"Squire repo-metadata benchmark",
		"exactness: true",
		"mismatches: 0",
		"mutation_boundary_invalidation: true",
		"workload_only_wall_delta_ms: 12",
		"no_broad_codex_speedup_claim: true",
		"  - git rev-parse HEAD",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("repo metadata short output missing %q:\n%s", want, text)
		}
	}
}

func TestWarmReportShortOutput(t *testing.T) {
	report := proofcache.WarmReport{
		RepoRoot:                "/repo",
		OracleAvailable:         true,
		FastPathPrepared:        5,
		ProofGatedPrewarmed:     7,
		WarmFilesPrepared:       11,
		FileTreeIndexesPrepared: 1,
		ProjectMetadataPrepared: 2,
		CommandPathPrepared:     1,
		EcosystemPrepared:       3,
		DependencyPrepared:      4,
		SourceSymbolPrepared:    6,
		Prepared:                []proofcache.WarmPreparedReport{{Kind: "fast_path_output"}},
		PrivacyMode:             "standard",
		ReplaySetUnchanged:      true,
		AgentVisibleSuggestions: false,
		Notes:                   []string{"no prompt changes"},
	}
	text := warmReportOut(report, outputShort)
	for _, want := range []string{
		"Squire warm",
		"repo_oracle: available",
		"repo_root: /repo",
		"fast_path_prepared: 5",
		"proof_gated_prewarmed: 7",
		"warm_files_prepared: 11",
		"prepared_entries: 1",
		"privacy_mode: standard",
		"replay_set_unchanged: true",
		"agent_visible_suggestions: false",
		"note: no prompt changes",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("warm short output missing %q:\n%s", want, text)
		}
	}
}

func TestDeepLocalBenchShortOutput(t *testing.T) {
	report := proofcache.DeepBenchReport{
		EnabledFastPathExactness:      true,
		EnabledFastPathMismatches:     0,
		NativeOnlyCandidateExactness:  true,
		NativeOnlyCandidateMismatches: 0,
		NoBroadCodexSpeedupClaim:      true,
		SafetyGates:                   proofcache.GateReport{Status: "pass", Passed: true, Required: true},
		PerformanceGates:              proofcache.GateReport{Status: "needs_optimization", Violations: []string{"native fallback overhead p95 over budget"}},
		NeverReplayDiagnostics:        proofcache.NeverReplayDiagnostics{ValidationReplays: 0},
		Performance: proofcache.PerformanceBudgetReport{
			MetadataFastPathP95US:       95,
			ProofGatedReplayP95US:       120,
			NativeFallbackOverheadP95US: 4000,
			NativeOnlyBookkeepingP95US:  9000,
		},
	}
	text := deepLocalBenchOut(report, outputShort)
	for _, want := range []string{
		"Squire deep-local benchmark",
		"safety_gates: pass",
		"performance_gates: needs_optimization",
		"enabled_fast_path_exactness: true",
		"validation_replays: 0",
		"metadata_fast_path_p95_us: 95",
		"native_only_bookkeeping_p95_us: 9000",
		"performance_violation: native fallback overhead p95 over budget",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("deep-local short output missing %q:\n%s", want, text)
		}
	}
}

func TestBackgroundStatusShortOutput(t *testing.T) {
	status := proofcache.BackgroundMaintainerStatus{
		Mode:                    "background_process",
		RepoRoot:                "/repo",
		StoreRoot:               "/store",
		HotCacheSocket:          "/store/hot.sock",
		PID:                     123,
		Running:                 true,
		AlreadyRunning:          true,
		Duration:                "30m0s",
		PollInterval:            "2s",
		LogPath:                 "/store/maintainer.log",
		StatusPath:              "/store/maintainer_status.json",
		AgentVisibleSuggestions: false,
		NativeFallbackAvailable: true,
		Diagnostics:             []string{"ready"},
	}
	text := formatBackgroundStatusShort(status)
	for _, want := range []string{
		"Squire maintainer",
		"status: already_running",
		"running: true",
		"pid: 123",
		"repo_root: /repo",
		"native_fallback: true",
		"agent_visible_suggestions: false",
		"diagnostic: ready",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("short background status missing %q:\n%s", want, text)
		}
	}
}

func TestBackgroundStatusShortPrefersStopState(t *testing.T) {
	status := proofcache.BackgroundMaintainerStatus{
		StoreRoot:               "/store",
		StatusPath:              "/store/maintainer_status.json",
		PID:                     123,
		Started:                 true,
		StopRequested:           true,
		Running:                 false,
		AgentVisibleSuggestions: false,
		NativeFallbackAvailable: true,
	}
	text := formatBackgroundStatusShort(status)
	if !strings.Contains(text, "status: stopped") {
		t.Fatalf("stopped status hidden by stale started flag:\n%s", text)
	}

	status.Running = true
	text = formatBackgroundStatusShort(status)
	if !strings.Contains(text, "status: stop_failed") {
		t.Fatalf("stop failure status hidden by stale started flag:\n%s", text)
	}
}

func TestMaintainerReportShortOutput(t *testing.T) {
	report := proofcache.MaintainerReport{
		Mode:                    "resident_bounded",
		RepoRoot:                "/repo",
		OracleAvailable:         true,
		PollCycles:              2,
		WarmCycles:              1,
		InvalidationsObserved:   1,
		FastPathPrepared:        5,
		ProofGatedPrewarmed:     7,
		PreparedEntriesObserved: 12,
		LastMaintainedAt:        time.Now(),
		AgentVisibleSuggestions: false,
		NativeFallbackAvailable: true,
	}
	text := formatMaintainerReportShort(report)
	for _, want := range []string{
		"Squire maintainer",
		"mode: resident_bounded",
		"repo_oracle: available",
		"poll_cycles: 2",
		"warm_cycles: 1",
		"fast_path_prepared: 5",
		"proof_gated_prewarmed: 7",
		"native_fallback: true",
		"agent_visible_suggestions: false",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("short maintainer report missing %q:\n%s", want, text)
		}
	}
}

func initAdapterGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "squire@example.invalid"},
		{"config", "user.name", "Squire"},
	} {
		stdout, stderr, code := runGitRaw(repo, args...)
		if code != 0 {
			t.Fatalf("git %s failed with code %d\nstdout=%s\nstderr=%s", strings.Join(args, " "), code, stdout, stderr)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Adapter Test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-m", "initial"},
	} {
		stdout, stderr, code := runGitRaw(repo, args...)
		if code != 0 {
			t.Fatalf("git %s failed with code %d\nstdout=%s\nstderr=%s", strings.Join(args, " "), code, stdout, stderr)
		}
	}
	return repo
}

func runGit(t *testing.T, repo string, args ...string) []byte {
	t.Helper()
	stdout, stderr, code := runGitRaw(repo, args...)
	if code != 0 {
		t.Fatalf("git %s failed with code %d\nstdout=%s\nstderr=%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

func runCommand(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	stdout, stderr, code := runCommandRaw(dir, args...)
	if code != 0 {
		t.Fatalf("%s failed with code %d\nstdout=%s\nstderr=%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

func runCommandRaw(dir string, args ...string) ([]byte, []byte, int) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = 1
			stderr.WriteString(err.Error())
		}
	}
	return stdout.Bytes(), stderr.Bytes(), code
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	writeExecutableScript(t, path, "#!/bin/sh\nexit 0\n")
}

func writeExecutableScript(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runGitRaw(repo string, args ...string) ([]byte, []byte, int) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = 1
			stderr.WriteString(err.Error())
		}
	}
	return stdout.Bytes(), stderr.Bytes(), code
}

func assertAdapterStdout(t *testing.T, resp adapterResponse, want []byte) {
	t.Helper()
	assertAdapterOutput(t, resp, want, nil, 0)
}

func assertAdapterOutput(t *testing.T, resp adapterResponse, wantStdout, wantStderr []byte, wantExitCode int) {
	t.Helper()
	got, err := base64.StdEncoding.DecodeString(resp.StdoutB64)
	if err != nil {
		t.Fatalf("stdout is not base64: %v", err)
	}
	if !bytes.Equal(got, wantStdout) {
		t.Fatalf("adapter stdout mismatch for %s\ngot:  %q\nwant: %q", resp.ID, got, wantStdout)
	}
	stderr, err := base64.StdEncoding.DecodeString(resp.StderrB64)
	if err != nil {
		t.Fatalf("stderr is not base64: %v", err)
	}
	if !bytes.Equal(stderr, wantStderr) {
		t.Fatalf("adapter stderr mismatch for %s\ngot:  %q\nwant: %q", resp.ID, stderr, wantStderr)
	}
	if resp.ExitCode != wantExitCode {
		t.Fatalf("adapter exit code for %s = %d, want %d", resp.ID, resp.ExitCode, wantExitCode)
	}
}

func warmRuntimeReplay(t *testing.T, ctx context.Context, k *proofcache.Engine, repo string, argv []string) {
	t.Helper()
	if k == nil {
		t.Fatal("missing engine")
	}
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatalf("warm %v failed: %v", argv, err)
	}
	if !k.PreloadHotSnapshot() {
		t.Fatalf("warm %v did not preload hot snapshot", argv)
	}
	replay, ok := k.FastReplay(ctx, "test-adapter", repo, argv)
	if !ok || replay.Mode != proofcache.ModeReplay {
		t.Fatalf("warm %v replay = (%+v, %t), want replay", argv, replay, ok)
	}
}

func jsonMapsEqual(left, right map[string]any) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

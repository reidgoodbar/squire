package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	vmBackendAuto           = "auto"
	vmBackendLinuxLocal     = "linux-local"
	vmBackendExternalRunner = "external-runner"
	vmBackendVirtualization = "virtualization-framework"
)

type vmStatusReport struct {
	ProductMode               string   `json:"product_mode"`
	Backend                   string   `json:"backend"`
	Available                 bool     `json:"available"`
	HostOS                    string   `json:"host_os"`
	HostArch                  string   `json:"host_arch"`
	Runner                    string   `json:"runner,omitempty"`
	VMHelper                  string   `json:"vm_helper,omitempty"`
	GuestConfigured           bool     `json:"guest_configured"`
	GuestAgentPort            uint32   `json:"guest_agent_port,omitempty"`
	WorkspaceShareTag         string   `json:"workspace_share_tag,omitempty"`
	StoreShareTag             string   `json:"store_share_tag,omitempty"`
	GuestOS                   string   `json:"guest_os"`
	GuestExecution            string   `json:"guest_execution"`
	NativeFallbackAvailable   bool     `json:"native_fallback_available"`
	AgentVisibleSuggestions   bool     `json:"agent_visible_suggestions"`
	ChangesAgentCommands      bool     `json:"changes_agent_commands"`
	UsesHostCommandShims      bool     `json:"uses_host_command_shims"`
	PreservesHostMacSemantics bool     `json:"preserves_host_mac_semantics"`
	Diagnostics               []string `json:"diagnostics,omitempty"`
}

type vmStatusOptions struct {
	Backend string
	Runner  string
	Format  outputFormat
}

type vmSessionOptions struct {
	Command []string
	Backend string
	Runner  string
	Quiet   bool
}

type darwinVMGuestConfig struct {
	Configured   bool
	Kernel       string
	Initrd       string
	Disk         string
	AgentPort    uint32
	WorkspaceTag string
	StoreTag     string
	Diagnostics  []string
}

type darwinVMHelperStatus struct {
	Available          bool     `json:"available"`
	FrameworkSupported bool     `json:"framework_supported"`
	GuestConfigured    bool     `json:"guest_configured"`
	Diagnostics        []string `json:"diagnostics"`
}

func runVM(ctx context.Context, cwd, storeRoot string, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf(vmUsageError(args))
	}
	switch args[0] {
	case "status":
		opts, err := parseVMStatusOptions(args[1:])
		if err != nil {
			return "", err
		}
		return vmStatusOut(detectVMStatus(cwd, storeRoot, opts.Backend, opts.Runner), opts.Format), nil
	case "session":
		opts, err := parseVMSessionOptions(args[1:])
		if err != nil {
			return "", err
		}
		code, err := runVMSession(ctx, cwd, storeRoot, opts)
		if err != nil {
			return "", err
		}
		os.Exit(code)
		return "", nil
	default:
		return "", fmt.Errorf(vmUsageError(args))
	}
}

func vmUsageError(args []string) string {
	if len(args) == 0 {
		return `missing vm subcommand (try "squire help vm")`
	}
	switch args[0] {
	case "status":
		return `invalid vm status usage (try "squire help vm")`
	case "session":
		return `invalid vm session usage (try "squire help vm")`
	default:
		return fmt.Sprintf(`unknown vm subcommand %q (try "squire help vm")`, args[0])
	}
}

func parseVMStatusOptions(args []string) (vmStatusOptions, error) {
	var opts vmStatusOptions
	opts.Backend = vmBackendAuto
	opts.Format = outputShort
	seenFormat := false
	for _, arg := range args {
		if arg == "--short" || arg == "--json" {
			seenFormat = true
			break
		}
	}
	remaining, format, err := splitOutputFormatFlag(args)
	if err != nil {
		return opts, err
	}
	if seenFormat {
		opts.Format = format
	}
	for i := 0; i < len(remaining); i++ {
		switch remaining[i] {
		case "--backend":
			if i+1 >= len(remaining) {
				return opts, fmt.Errorf("squire vm status --backend requires a value")
			}
			i++
			if err := validateVMBackend(remaining[i]); err != nil {
				return opts, err
			}
			opts.Backend = remaining[i]
		case "--runner":
			if i+1 >= len(remaining) {
				return opts, fmt.Errorf("squire vm status --runner requires a path")
			}
			i++
			opts.Runner = remaining[i]
		default:
			return opts, fmt.Errorf("unknown vm status option %q", remaining[i])
		}
	}
	return opts, nil
}

func parseVMSessionOptions(args []string) (vmSessionOptions, error) {
	var opts vmSessionOptions
	opts.Backend = vmBackendAuto
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 >= len(args) {
				return opts, fmt.Errorf("squire vm session requires a command after --")
			}
			opts.Command = append([]string(nil), args[i+1:]...)
			return opts, nil
		}
		switch arg {
		case "--quiet":
			opts.Quiet = true
		case "--backend":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("squire vm session --backend requires a value")
			}
			i++
			if err := validateVMBackend(args[i]); err != nil {
				return opts, err
			}
			opts.Backend = args[i]
		case "--runner":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("squire vm session --runner requires a path")
			}
			i++
			opts.Runner = args[i]
		default:
			return opts, fmt.Errorf("unknown vm session option %q", arg)
		}
	}
	return opts, fmt.Errorf("squire vm session requires -- before the command")
}

func validateVMBackend(backend string) error {
	switch backend {
	case vmBackendAuto, vmBackendLinuxLocal, vmBackendExternalRunner, vmBackendVirtualization:
		return nil
	default:
		return fmt.Errorf("unknown vm backend %q (valid: auto, linux-local, external-runner, virtualization-framework)", backend)
	}
}

func detectVMStatus(cwd, storeRoot, requestedBackend, requestedRunner string) vmStatusReport {
	_ = cwd
	_ = storeRoot
	if requestedBackend == "" {
		requestedBackend = vmBackendAuto
	}
	hostOS := runtimeGOOS()
	report := vmStatusReport{
		ProductMode:               "linux_guest_session",
		Backend:                   requestedBackend,
		HostOS:                    hostOS,
		HostArch:                  runtimeGOARCH(),
		GuestOS:                   "linux",
		GuestExecution:            "ordinary agent-chosen commands served by Squire inside the guest",
		NativeFallbackAvailable:   true,
		AgentVisibleSuggestions:   false,
		ChangesAgentCommands:      false,
		UsesHostCommandShims:      false,
		PreservesHostMacSemantics: hostOS != "darwin",
	}
	runner := resolveVMRunner(requestedRunner)
	if requestedBackend == vmBackendExternalRunner || (requestedBackend == vmBackendAuto && runner != "") {
		report.Backend = vmBackendExternalRunner
		report.Runner = runner
		if runner == "" {
			report.Diagnostics = append(report.Diagnostics, "external-runner backend requires --runner or SQUIRE_VM_RUNNER")
			return report
		}
		if !isExecutableFile(runner) {
			report.Diagnostics = append(report.Diagnostics, "configured runner is not executable: "+runner)
			return report
		}
		report.Available = true
		report.Diagnostics = append(report.Diagnostics, "external guest runner configured")
		return report
	}
	if requestedBackend == vmBackendLinuxLocal || (requestedBackend == vmBackendAuto && hostOS == "linux") {
		report.Backend = vmBackendLinuxLocal
		if hostOS != "linux" {
			report.Diagnostics = append(report.Diagnostics, "linux-local backend requires a Linux host")
			return report
		}
		report.Available = true
		report.Diagnostics = append(report.Diagnostics, "host is already Linux; using the scoped session path directly")
		return report
	}
	if requestedBackend == vmBackendVirtualization || (requestedBackend == vmBackendAuto && hostOS == "darwin") {
		report.Backend = vmBackendVirtualization
		if hostOS != "darwin" {
			report.Diagnostics = append(report.Diagnostics, "Virtualization.framework backend requires macOS")
			return report
		}
		helper := resolveDarwinVMHelper()
		report.VMHelper = helper
		report.GuestAgentPort = 1024
		report.WorkspaceShareTag = "squire-workspace"
		report.StoreShareTag = "squire-store"
		if helper == "" {
			report.Diagnostics = append(report.Diagnostics, "squire-vm-darwin helper is not installed; set SQUIRE_VM_HELPER or install from a release archive")
			report.Diagnostics = append(report.Diagnostics, "set SQUIRE_VM_RUNNER or pass --runner to use an external Linux guest runner")
			return report
		}
		if !isExecutableFile(helper) {
			report.Diagnostics = append(report.Diagnostics, "configured squire-vm-darwin helper is not executable: "+helper)
			return report
		}
		guest := detectDarwinVMGuestConfig()
		report.GuestConfigured = guest.Configured
		report.GuestAgentPort = guest.AgentPort
		report.WorkspaceShareTag = guest.WorkspaceTag
		report.StoreShareTag = guest.StoreTag
		helperStatus, helperStatusErr := queryDarwinVMHelperStatus(helper)
		if helperStatusErr != nil {
			report.Diagnostics = append(report.Diagnostics, "could not query squire-vm-darwin status: "+helperStatusErr.Error())
			return report
		}
		report.GuestConfigured = helperStatus.GuestConfigured
		report.Diagnostics = append(report.Diagnostics, helperStatus.Diagnostics...)
		if !helperStatus.FrameworkSupported {
			return report
		}
		if !helperStatus.GuestConfigured {
			report.Diagnostics = append(report.Diagnostics, "Virtualization.framework helper is installed, but Linux guest assets are not configured")
			return report
		}
		if !helperStatus.Available {
			report.Diagnostics = append(report.Diagnostics, "Virtualization.framework helper is not available")
			return report
		}
		report.Available = true
		report.Diagnostics = append(report.Diagnostics, "Virtualization.framework helper and Linux guest assets are configured")
		return report
	}
	report.Diagnostics = append(report.Diagnostics, "no Linux guest backend is available for this host")
	return report
}

func resolveVMRunner(explicit string) string {
	if explicit != "" {
		if abs, err := filepathAbs(explicit); err == nil {
			return abs
		}
		return explicit
	}
	if env := os.Getenv("SQUIRE_VM_RUNNER"); env != "" {
		if abs, err := filepathAbs(env); err == nil {
			return abs
		}
		return env
	}
	return ""
}

func resolveDarwinVMHelper() string {
	for _, key := range []string{"SQUIRE_VM_HELPER", "SQUIRE_VM_DARWIN_HELPER"} {
		if env := os.Getenv(key); env != "" {
			if abs, err := filepathAbs(env); err == nil {
				return abs
			}
			return env
		}
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "squire-vm-darwin")
		if isExecutableFile(candidate) {
			return candidate
		}
	}
	if path, err := exec.LookPath("squire-vm-darwin"); err == nil {
		return path
	}
	return ""
}

func detectDarwinVMGuestConfig() darwinVMGuestConfig {
	guest := darwinVMGuestConfig{
		AgentPort:    1024,
		WorkspaceTag: "squire-workspace",
		StoreTag:     "squire-store",
	}
	if port := os.Getenv("SQUIRE_VM_AGENT_PORT"); port != "" {
		value, err := strconv.ParseUint(port, 10, 32)
		if err != nil || value == 0 {
			guest.Diagnostics = append(guest.Diagnostics, "SQUIRE_VM_AGENT_PORT must be a positive uint32")
		} else {
			guest.AgentPort = uint32(value)
		}
	}
	if tag := os.Getenv("SQUIRE_VM_WORKSPACE_TAG"); tag != "" {
		guest.WorkspaceTag = tag
	}
	if tag := os.Getenv("SQUIRE_VM_STORE_TAG"); tag != "" {
		guest.StoreTag = tag
	}

	bundle := os.Getenv("SQUIRE_VM_BUNDLE")
	guest.Kernel = envPathWithBundle("SQUIRE_VM_KERNEL", bundle, "kernel")
	guest.Initrd = envPathWithBundle("SQUIRE_VM_INITRD", bundle, "initrd")
	guest.Disk = envPathWithBundle("SQUIRE_VM_DISK", bundle, "disk.img")
	if guest.Kernel == "" || !isReadableFile(guest.Kernel) {
		guest.Diagnostics = append(guest.Diagnostics, "missing readable SQUIRE_VM_KERNEL or SQUIRE_VM_BUNDLE/kernel")
	}
	hasInitrd := guest.Initrd != "" && isReadableFile(guest.Initrd)
	hasDisk := guest.Disk != "" && isReadableFile(guest.Disk)
	if !hasInitrd && !hasDisk {
		guest.Diagnostics = append(guest.Diagnostics, "missing readable SQUIRE_VM_INITRD or SQUIRE_VM_DISK")
	}
	guest.Configured = len(guest.Diagnostics) == 0
	return guest
}

func queryDarwinVMHelperStatus(helper string) (darwinVMHelperStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, "status", "--json")
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return darwinVMHelperStatus{}, fmt.Errorf("timed out")
	}
	if err != nil {
		return darwinVMHelperStatus{}, err
	}
	var status darwinVMHelperStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return darwinVMHelperStatus{}, err
	}
	return status, nil
}

func envPathWithBundle(envKey, bundle, bundleName string) string {
	if value := os.Getenv(envKey); value != "" {
		if abs, err := filepathAbs(value); err == nil {
			return abs
		}
		return value
	}
	if bundle == "" {
		return ""
	}
	return filepath.Join(bundle, bundleName)
}

func isReadableFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func filepathAbs(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func runVMSession(ctx context.Context, cwd, storeRoot string, opts vmSessionOptions) (int, error) {
	status := detectVMStatus(cwd, storeRoot, opts.Backend, opts.Runner)
	if !status.Available {
		return 0, fmt.Errorf("squire vm session unavailable for backend %s: %s", status.Backend, strings.Join(status.Diagnostics, "; "))
	}
	switch status.Backend {
	case vmBackendLinuxLocal:
		if !opts.Quiet {
			fmt.Fprintln(os.Stderr, "squire vm session: linux-local execution active; native fallback available inside Linux")
		}
		return runScopedSession(ctx, cwd, storeRoot, sessionOptions{Command: opts.Command, Quiet: opts.Quiet})
	case vmBackendExternalRunner:
		return runVMExternalRunner(ctx, cwd, storeRoot, status.Runner, opts)
	case vmBackendVirtualization:
		return runVMDarwinHelper(ctx, cwd, storeRoot, status.VMHelper, opts)
	default:
		return 0, fmt.Errorf("squire vm backend %s is not implemented", status.Backend)
	}
}

func runVMExternalRunner(ctx context.Context, cwd, storeRoot, runner string, opts vmSessionOptions) (int, error) {
	if runner == "" {
		return 0, fmt.Errorf("squire vm external-runner requires --runner or SQUIRE_VM_RUNNER")
	}
	args := vmExternalRunnerArgs(cwd, storeRoot, opts.Command)
	cmd := exec.CommandContext(ctx, runner, args...)
	cmd.Dir = cwd
	cmd.Env = mergeEnv(os.Environ(), map[string]string{
		"SQUIRE_VM_HOST_OS":    runtimeGOOS(),
		"SQUIRE_VM_HOST_ARCH":  runtimeGOARCH(),
		"SQUIRE_VM_CWD":        cwd,
		"SQUIRE_VM_STORE_ROOT": storeRoot,
	})
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if !opts.Quiet {
		fmt.Fprintf(os.Stderr, "squire vm session: external Linux guest runner active (%s)\n", runner)
	}
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

func runVMDarwinHelper(ctx context.Context, cwd, storeRoot, helper string, opts vmSessionOptions) (int, error) {
	if helper == "" {
		return 0, fmt.Errorf("squire vm virtualization-framework requires squire-vm-darwin")
	}
	args := vmExternalRunnerArgs(cwd, storeRoot, opts.Command)
	cmd := exec.CommandContext(ctx, helper, args...)
	cmd.Dir = cwd
	cmd.Env = mergeEnv(os.Environ(), map[string]string{
		"SQUIRE_VM_HOST_OS":    runtimeGOOS(),
		"SQUIRE_VM_HOST_ARCH":  runtimeGOARCH(),
		"SQUIRE_VM_CWD":        cwd,
		"SQUIRE_VM_STORE_ROOT": storeRoot,
	})
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if !opts.Quiet {
		fmt.Fprintf(os.Stderr, "squire vm session: macOS Virtualization.framework Linux guest active (%s)\n", helper)
	}
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

func vmExternalRunnerArgs(cwd, storeRoot string, command []string) []string {
	args := []string{"session", "--cwd", cwd, "--store-root", storeRoot, "--"}
	return append(args, command...)
}

func vmStatusOut(report vmStatusReport, format outputFormat) string {
	if format == outputJSON {
		return jsonOut(report)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire VM session")
	fmt.Fprintf(&b, "backend: %s\n", report.Backend)
	fmt.Fprintf(&b, "available: %t\n", report.Available)
	fmt.Fprintf(&b, "host: %s/%s\n", report.HostOS, report.HostArch)
	fmt.Fprintf(&b, "guest: %s\n", report.GuestOS)
	if report.Runner != "" {
		fmt.Fprintf(&b, "runner: %s\n", report.Runner)
	}
	if report.VMHelper != "" {
		fmt.Fprintf(&b, "vm_helper: %s\n", report.VMHelper)
	}
	fmt.Fprintf(&b, "guest_configured: %t\n", report.GuestConfigured)
	if report.GuestAgentPort != 0 {
		fmt.Fprintf(&b, "guest_agent_port: %d\n", report.GuestAgentPort)
	}
	if report.WorkspaceShareTag != "" {
		fmt.Fprintf(&b, "workspace_share_tag: %s\n", report.WorkspaceShareTag)
	}
	if report.StoreShareTag != "" {
		fmt.Fprintf(&b, "store_share_tag: %s\n", report.StoreShareTag)
	}
	fmt.Fprintf(&b, "native_fallback: %t\n", report.NativeFallbackAvailable)
	fmt.Fprintf(&b, "agent_visible_suggestions: %t\n", report.AgentVisibleSuggestions)
	fmt.Fprintf(&b, "changes_agent_commands: %t\n", report.ChangesAgentCommands)
	fmt.Fprintf(&b, "uses_host_command_shims: %t\n", report.UsesHostCommandShims)
	fmt.Fprintf(&b, "preserves_host_mac_semantics: %t\n", report.PreservesHostMacSemantics)
	for _, diagnostic := range report.Diagnostics {
		fmt.Fprintf(&b, "diagnostic: %s\n", diagnostic)
	}
	return b.String()
}

package green

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGreenHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_GREEN_HELPER") != "1" {
		return
	}
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(97)
	}
	switch os.Args[separator+1] {
	case "pass":
		fmt.Fprint(os.Stdout, "TOP-SECRET-GREEN-OUTPUT")
	case "fail":
		fmt.Fprint(os.Stderr, "intentional validation failure")
		os.Exit(7)
	case "sleep":
		if signal := os.Getenv("GREEN_HELPER_SIGNAL"); signal != "" {
			if err := os.WriteFile(signal, []byte("started\n"), 0o600); err != nil {
				os.Exit(96)
			}
		}
		duration, err := time.ParseDuration(os.Getenv("GREEN_HELPER_SLEEP"))
		if err != nil {
			os.Exit(95)
		}
		time.Sleep(duration)
	case "read-input":
		if _, err := os.ReadFile(os.Getenv("GREEN_HELPER_INPUT")); err != nil {
			os.Exit(92)
		}
	case "write-output":
		path := os.Getenv("GREEN_HELPER_OUTPUT")
		if path == "" || os.WriteFile(path, []byte("build output\n"), 0o600) != nil {
			os.Exit(93)
		}
	default:
		os.Exit(94)
	}
}

func TestLoadConfigStrictDefaults(t *testing.T) {
	repo := newGreenWorkspace(t)
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"**/*.txt"}, ""))
	config, err := LoadConfig(repo)
	if err != nil {
		t.Fatal(err)
	}
	if config.Version != 1 || config.Concurrency < 1 || config.Quiescence != defaultQuiescence || config.PollInterval != defaultPollInterval {
		t.Fatalf("unexpected normalized config: %+v", config)
	}
	if len(config.Checks) != 1 || !config.Checks[0].Required || config.Checks[0].Timeout != defaultTimeout {
		t.Fatalf("unexpected normalized check: %+v", config.Checks)
	}

	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"**/*.txt"}, "mystery = true\n"))
	if _, err := LoadConfig(repo); err == nil || !strings.Contains(err.Error(), "unknown Green config fields") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestLoadConfigRequiresAtLeastOneRequiredCheck(t *testing.T) {
	repo := newGreenWorkspace(t)
	config := configFor(helperCommand("pass"), []string{"input.txt"}, "")
	config = strings.Replace(config, "[[check]]", "[[check]]\nrequired = false", 1)
	writeGreenConfig(t, repo, config)
	if _, err := LoadConfig(repo); err == nil || !strings.Contains(err.Error(), "required check") {
		t.Fatalf("required-check error = %v", err)
	}
}

func TestRunPendingPublishesCurrentPassWithoutPersistingOutput(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"input.txt"}, ""))
	report, err := runTrustedPending(context.Background(), repo, store)
	if err != nil {
		t.Fatal(err)
	}
	if report.Started != 1 || report.Completed != 1 || report.Discarded != 0 {
		t.Fatalf("scheduler report = %+v", report)
	}
	status := Inspect(context.Background(), repo, store)
	if !status.Green || len(status.Checks) != 1 || status.Checks[0].State != "passed" || !status.Checks[0].Current {
		t.Fatalf("Green status = %+v", status)
	}
	b, err := os.ReadFile(filepath.Join(store, filepath.FromSlash(stateRelativePath)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "TOP-SECRET-GREEN-OUTPUT") {
		t.Fatal("native validation output was persisted")
	}
}

func TestValidationMayReadDeclaredInputWithoutSelfInvalidating(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	t.Setenv("GREEN_HELPER_INPUT", filepath.Join(repo, "input.txt"))
	writeGreenConfig(t, repo, configFor(helperCommand("read-input"), []string{"input.txt"}, ""))
	report, err := runTrustedPending(context.Background(), repo, store)
	if err != nil {
		t.Fatal(err)
	}
	if report.Discarded != 0 {
		t.Fatalf("declared read discarded validation: %+v", report)
	}
	status := Inspect(context.Background(), repo, store)
	if !status.Green || status.Checks[0].State != "passed" {
		t.Fatalf("declared read status = %+v", status)
	}
}

func TestRunPendingRequiresExactConfigTrust(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"input.txt"}, ""))
	if _, err := RunPending(context.Background(), repo, store); !errors.Is(err, ErrConfigUntrusted) {
		t.Fatalf("untrusted run error = %v", err)
	}
	status := Inspect(context.Background(), repo, store)
	if status.Trusted || status.State != "untrusted" || status.Green {
		t.Fatalf("untrusted status = %+v", status)
	}
}

func TestCurrentFailureIsNotGreenAndDoesNotRerun(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	writeGreenConfig(t, repo, configFor(helperCommand("fail"), []string{"input.txt"}, ""))
	first, err := runTrustedPending(context.Background(), repo, store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runTrustedPending(context.Background(), repo, store)
	if err != nil {
		t.Fatal(err)
	}
	if first.Started != 1 || second.Started != 0 {
		t.Fatalf("failure rerun reports: first=%+v second=%+v", first, second)
	}
	status := Inspect(context.Background(), repo, store)
	if status.Green || status.Checks[0].State != "failed" || !status.Checks[0].Current || status.Checks[0].ExitCode != 7 {
		t.Fatalf("failure status = %+v", status)
	}
}

func TestSameSizeMtimeRestoredRewriteMakesResultStale(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"input.txt"}, ""))
	if _, err := runTrustedPending(context.Background(), repo, store); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, "input.txt")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("bravo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	status := Inspect(context.Background(), repo, store)
	if status.Green || status.Checks[0].State != "stale" || !strings.Contains(status.Checks[0].Reason, "inputs changed") {
		t.Fatalf("same-size rewrite status = %+v", status)
	}
}

func TestUnrelatedMutationKeepsDeclaredCheckCurrent(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"input.txt"}, ""))
	if _, err := runTrustedPending(context.Background(), repo, store); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := Inspect(context.Background(), repo, store)
	if !status.Green || !status.Checks[0].Current {
		t.Fatalf("unrelated mutation invalidated check: %+v", status)
	}
}

func TestMutationDuringValidationDiscardsResult(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(base, "store")
	signal := filepath.Join(base, "started")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	t.Setenv("GREEN_HELPER_SIGNAL", signal)
	t.Setenv("GREEN_HELPER_SLEEP", "1.2s")
	writeGreenConfig(t, repo, configFor(helperCommand("sleep"), []string{"input.txt"}, "timeout = \"5s\"\n"))
	done := make(chan error, 1)
	go func() {
		_, err := runTrustedPending(context.Background(), repo, store)
		done <- err
	}()
	waitForFile(t, signal, 3*time.Second)
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("bravo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	state, err := newStateStore(store).load(repo)
	if err != nil {
		t.Fatal(err)
	}
	record := state.Checks["tests"]
	if record.State != "discarded" || !strings.Contains(record.Error, "changed") {
		t.Fatalf("in-flight mutation record = %+v", record)
	}
	if Inspect(context.Background(), repo, store).Green {
		t.Fatal("discarded in-flight result reported Green")
	}
}

func TestTrustRevokedDuringValidationDiscardsResult(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(base, "store")
	signal := filepath.Join(base, "started")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	t.Setenv("GREEN_HELPER_SIGNAL", signal)
	t.Setenv("GREEN_HELPER_SLEEP", "1.2s")
	writeGreenConfig(t, repo, configFor(helperCommand("sleep"), []string{"input.txt"}, "timeout = \"5s\"\n"))
	if _, err := TrustConfig(repo, store); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := RunPending(context.Background(), repo, store)
		done <- err
	}()
	waitForFile(t, signal, 3*time.Second)
	if err := RevokeConfigTrust(store); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	state, err := newStateStore(store).load(repo)
	if err != nil {
		t.Fatal(err)
	}
	record := state.Checks["tests"]
	if record.State != "discarded" || !strings.Contains(record.Error, "trust changed") {
		t.Fatalf("revoked-trust record = %+v", record)
	}
	status := Inspect(context.Background(), repo, store)
	if status.Trusted || status.Green || status.State != "untrusted" {
		t.Fatalf("revoked-trust status = %+v", status)
	}
}

func TestUndeclaredBuildOutputDoesNotDiscardResult(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	t.Setenv("GREEN_HELPER_OUTPUT", filepath.Join(repo, "build.out"))
	writeGreenConfig(t, repo, configFor(helperCommand("write-output"), []string{"input.txt"}, ""))
	report, err := runTrustedPending(context.Background(), repo, store)
	if err != nil {
		t.Fatal(err)
	}
	if report.Discarded != 0 || !Inspect(context.Background(), repo, store).Green {
		t.Fatalf("undeclared output result: report=%+v status=%+v", report, Inspect(context.Background(), repo, store))
	}
}

func TestStatusUsesRecordedEnvironmentAndRestartRerunsOnEnvironmentChange(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	t.Setenv("GREEN_ENV_VARIANT", "one")
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"input.txt"}, ""))
	first, err := runTrustedPending(context.Background(), repo, store)
	if err != nil {
		t.Fatal(err)
	}
	if first.Started != 1 {
		t.Fatalf("first run = %+v", first)
	}
	t.Setenv("GREEN_ENV_VARIANT", "two")
	if !Inspect(context.Background(), repo, store).Green {
		t.Fatal("incidental status-process environment invalidated recorded provenance")
	}
	second, err := runTrustedPending(context.Background(), repo, store)
	if err != nil {
		t.Fatal(err)
	}
	if second.Started != 1 {
		t.Fatalf("changed validation environment did not rerun: %+v", second)
	}
	if !Inspect(context.Background(), repo, store).Green {
		t.Fatal("rerun under changed environment did not publish Green")
	}
}

func TestTimeoutIsCurrentButNotGreen(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	t.Setenv("GREEN_HELPER_SLEEP", "2s")
	writeGreenConfig(t, repo, configFor(helperCommand("sleep"), []string{"input.txt"}, "timeout = \"200ms\"\n"))
	if _, err := runTrustedPending(context.Background(), repo, store); err != nil {
		t.Fatal(err)
	}
	status := Inspect(context.Background(), repo, store)
	if status.Green || status.Checks[0].State != "timed_out" || !status.Checks[0].Current {
		t.Fatalf("timeout status = %+v", status)
	}
}

func TestSchedulerRunsAfterQuiescence(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	extra := "quiescence = \"100ms\"\npoll_interval = \"100ms\"\n"
	writeGreenConfig(t, repo, strings.Replace(configFor(helperCommand("pass"), []string{"input.txt"}, ""), "version = 1\n", "version = 1\n"+extra, 1))
	if _, err := TrustConfig(repo, store); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan SchedulerReport, 1)
	go func() {
		report, _ := RunScheduler(ctx, repo, store, SchedulerOptions{})
		done <- report
	}()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if Inspect(context.Background(), repo, store).Green {
			cancel()
			report := <-done
			if report.Started < 1 || report.Completed < 1 {
				t.Fatalf("scheduler report = %+v", report)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("scheduler did not publish Green state after quiescence")
}

func TestSchedulerWakesOnDeclaredInputChangeWithoutSteadyPolling(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"input.txt"}, ""))
	if _, err := TrustConfig(repo, store); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan SchedulerReport, 1)
	go func() {
		report, _ := RunScheduler(ctx, repo, store, SchedulerOptions{Quiescence: 100 * time.Millisecond})
		done <- report
	}()
	waitForGreen(t, repo, store, 3*time.Second)
	state, err := newStateStore(store).load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if state.Checks["tests"].Attempt != 1 {
		t.Fatalf("initial attempt = %+v", state.Checks["tests"])
	}
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForAttempt(t, repo, store, "tests", 2, 3*time.Second)
	waitForGreen(t, repo, store, 3*time.Second)
	time.Sleep(300 * time.Millisecond)
	cancel()
	report := <-done
	if report.Cycles != 2 || report.Started != 2 || report.Completed != 2 {
		t.Fatalf("event-driven scheduler performed unexpected steady work: %+v", report)
	}
}

func TestSchedulerWaitsForTrustThenRunsWithoutRestart(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"input.txt"}, ""))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan SchedulerReport, 1)
	go func() {
		report, _ := RunScheduler(ctx, repo, store, SchedulerOptions{Quiescence: 100 * time.Millisecond})
		done <- report
	}()
	time.Sleep(350 * time.Millisecond)
	state, err := newStateStore(store).load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := state.Checks["tests"]; exists {
		t.Fatalf("untrusted scheduler executed check: %+v", state.Checks["tests"])
	}
	if _, err := TrustConfig(repo, store); err != nil {
		t.Fatal(err)
	}
	waitForGreen(t, repo, store, 3*time.Second)
	cancel()
	report := <-done
	if report.Started != 1 || report.Completed != 1 {
		t.Fatalf("post-trust scheduler report = %+v", report)
	}
}

func TestConfigChangeRevokesTrust(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"input.txt"}, ""))
	if _, err := runTrustedPending(context.Background(), repo, store); err != nil {
		t.Fatal(err)
	}
	writeGreenConfig(t, repo, configFor(append(helperCommand("pass"), "extra"), []string{"input.txt"}, ""))
	status := Inspect(context.Background(), repo, store)
	if status.Green || status.Trusted || status.State != "untrusted" || status.Checks[0].State != "untrusted" {
		t.Fatalf("config-change status = %+v", status)
	}
}

func TestSelectiveInvalidationRerunsOnlyAffectedCheck(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	if err := os.WriteFile(filepath.Join(repo, "second.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "version = 1\nconcurrency = 2\n\n" +
		"[[check]]\nname = \"first\"\ncommand = " + tomlStringArray(helperCommand("pass")) + "\ninputs = [\"input.txt\"]\n\n" +
		"[[check]]\nname = \"second\"\ncommand = " + tomlStringArray(helperCommand("pass")) + "\ninputs = [\"second.txt\"]\n"
	writeGreenConfig(t, repo, config)
	first, err := runTrustedPending(context.Background(), repo, store)
	if err != nil {
		t.Fatal(err)
	}
	if first.Started != 2 {
		t.Fatalf("initial selective run = %+v", first)
	}
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := runTrustedPending(context.Background(), repo, store)
	if err != nil {
		t.Fatal(err)
	}
	if second.Started != 1 || !Inspect(context.Background(), repo, store).Green {
		t.Fatalf("selective rerun = %+v status=%+v", second, Inspect(context.Background(), repo, store))
	}
	state, err := newStateStore(store).load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if state.Checks["first"].Attempt != 2 || state.Checks["second"].Attempt != 1 {
		t.Fatalf("selective attempts = %+v", state.Checks)
	}
}

func TestNewGlobMatchMakesResultStale(t *testing.T) {
	repo := newGreenWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"src/**/*.go"}, ""))
	if _, err := runTrustedPending(context.Background(), repo, store); err != nil {
		t.Fatal(err)
	}
	if !Inspect(context.Background(), repo, store).Green {
		t.Fatal("empty but declared glob did not produce a current result")
	}
	if err := os.MkdirAll(filepath.Join(repo, "src", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "nested", "new.go"), []byte("package nested\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := Inspect(context.Background(), repo, store)
	if status.Green || status.Checks[0].State != "stale" || status.Checks[0].MatchedFiles != 1 {
		t.Fatalf("new glob match status = %+v", status)
	}
}

func TestDeclaredSymlinkOutsideRepositoryCannotBeProven(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available to unprivileged Windows tests")
	}
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "link.txt")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_GREEN_HELPER", "1")
	writeGreenConfig(t, repo, configFor(helperCommand("pass"), []string{"link.txt"}, ""))
	_, err := runTrustedPending(context.Background(), repo, filepath.Join(base, "store"))
	if err == nil || !strings.Contains(err.Error(), "outside repository") {
		t.Fatalf("outside symlink error = %v", err)
	}
}

func newGreenWorkspace(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo
}

func helperCommand(mode string) []string {
	return []string{os.Args[0], "-test.run=TestGreenHelperProcess", "--", mode}
}

func configFor(command, inputs []string, checkExtra string) string {
	return "version = 1\n\n[[check]]\n" +
		"name = \"tests\"\n" +
		"command = " + tomlStringArray(command) + "\n" +
		"inputs = " + tomlStringArray(inputs) + "\n" +
		checkExtra
}

func tomlStringArray(values []string) string {
	encoded := make([]string, 0, len(values))
	for _, value := range values {
		b, _ := json.Marshal(value)
		encoded = append(encoded, string(b))
	}
	return "[" + strings.Join(encoded, ", ") + "]"
}

func writeGreenConfig(t *testing.T, repo, content string) {
	t.Helper()
	dir := filepath.Join(repo, ".squire")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checks.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runTrustedPending(ctx context.Context, repo, store string) (SchedulerReport, error) {
	if _, err := TrustConfig(repo, store); err != nil {
		return SchedulerReport{RepoRoot: repo}, err
	}
	return RunPending(ctx, repo, store)
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s after %s", path, timeout)
}

func waitForGreen(t *testing.T, repo, store string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if Inspect(context.Background(), repo, store).Green {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Green after %s", timeout)
}

func waitForAttempt(t *testing.T, repo, store, check string, attempt int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := newStateStore(store).load(repo)
		if err == nil && state.Checks[check].Attempt >= attempt {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s attempt %d after %s", check, attempt, timeout)
}

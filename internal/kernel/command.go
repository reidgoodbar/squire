package kernel

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func normalizeArgv(argv []string) string {
	return strings.Join(argv, "\x00")
}

func displayCommand(argv []string) string {
	return strings.Join(argv, " ")
}

func Classify(argv []string) OperatorFamily {
	if len(argv) == 0 {
		return FamilyShellUnknown
	}
	name := filepath.Base(argv[0])
	if name == "git" {
		if isGitMetadata(argv) {
			return FamilyLocalRepoMetadata
		}
		if isGitRepoState(argv) {
			return FamilyRepoState
		}
		if isGitMutation(argv) {
			return FamilyEditOrMutation
		}
		return FamilyShellUnknown
	}
	if name == "rg" && len(argv) == 2 && argv[1] == "--files" {
		return FamilySearchList
	}
	if isValidationBuildTest(argv) {
		return FamilyValidation
	}
	if isPackageSetup(argv) {
		return FamilyPackageSetup
	}
	return FamilyShellUnknown
}

func isGitMetadata(argv []string) bool {
	if len(argv) != 3 || filepath.Base(argv[0]) != "git" || argv[1] != "rev-parse" {
		return false
	}
	switch argv[2] {
	case "HEAD", "--git-dir", "--abbrev-ref":
		return argv[2] != "--abbrev-ref"
	}
	return false
}

func isGitAbbrevRefHead(argv []string) bool {
	return len(argv) == 4 && filepath.Base(argv[0]) == "git" && argv[1] == "rev-parse" && argv[2] == "--abbrev-ref" && argv[3] == "HEAD"
}

func IsFastPathAllowed(argv []string) bool {
	return isGitMetadata(argv) || isGitAbbrevRefHead(argv)
}

func isGitRepoState(argv []string) bool {
	if len(argv) < 2 || filepath.Base(argv[0]) != "git" {
		return false
	}
	if len(argv) == 2 && argv[1] == "ls-files" {
		return true
	}
	if len(argv) == 3 && argv[1] == "status" && (argv[2] == "--short" || argv[2] == "--porcelain") {
		return true
	}
	return false
}

func IsShadowCandidate(argv []string) bool {
	return isGitRepoState(argv) || (len(argv) == 2 && filepath.Base(argv[0]) == "rg" && argv[1] == "--files")
}

func isGitMutation(argv []string) bool {
	if len(argv) < 2 || filepath.Base(argv[0]) != "git" {
		return false
	}
	switch argv[1] {
	case "add", "am", "apply", "bisect", "branch", "checkout", "cherry-pick", "clean", "commit", "fetch", "merge", "mv", "pull", "push", "rebase", "reset", "restore", "revert", "rm", "stash", "submodule", "switch", "tag", "worktree":
		return true
	default:
		return false
	}
}

func isValidationBuildTest(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	name := filepath.Base(argv[0])
	if len(argv) >= 2 {
		pair := name + " " + argv[1]
		switch pair {
		case "go test", "go build", "cargo test", "cargo build", "npm test", "npm run", "pnpm test", "pnpm run", "yarn test", "mvn test", "gradle test", "make test":
			if pair == "npm run" || pair == "pnpm run" {
				return len(argv) >= 3 && strings.Contains(argv[2], "test")
			}
			return true
		}
	}
	if name == "pytest" || name == "tox" || name == "ninja" {
		return true
	}
	if len(argv) >= 3 && (name == "python" || name == "python3") && argv[1] == "-m" && argv[2] == "pytest" {
		return true
	}
	return false
}

func isPackageSetup(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	name := filepath.Base(argv[0])
	if len(argv) >= 2 {
		pair := name + " " + argv[1]
		switch pair {
		case "npm install", "npm ci", "pnpm install", "yarn install", "pip install", "pip3 install", "go get", "go install", "cargo fetch", "cargo install", "bundle install":
			return true
		}
	}
	return false
}

func runNative(ctx context.Context, cwd string, argv []string) NativeResult {
	start := time.Now()
	if len(argv) == 0 {
		return NativeResult{ExitCode: 127, Stderr: []byte("empty command\n"), Wall: time.Since(start), Err: errors.New("empty command")}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	stdout, err := cmd.Output()
	var stderr []byte
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = exitErr.Stderr
			exitCode = exitErr.ExitCode()
		} else {
			stderr = []byte(fmt.Sprintf("%v\n", err))
			exitCode = 127
		}
	}
	return NativeResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode, Wall: time.Since(start), Err: err}
}

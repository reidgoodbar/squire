package proofcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type CommandInvocation struct {
	OriginalArgv []string
	OriginalCWD  string
	PolicyArgv   []string
	PolicyCWD    string
}

func NormalizeInvocation(cwd string, argv []string) CommandInvocation {
	policyCWD := absPath(cwd)
	if policyCWD == "" {
		policyCWD = cwd
	}
	inv := CommandInvocation{
		OriginalArgv: append([]string(nil), argv...),
		OriginalCWD:  cwd,
		PolicyArgv:   append([]string(nil), argv...),
		PolicyCWD:    policyCWD,
	}
	normalizedArgv, normalizedCWD, ok := normalizeGitInvocation(cwd, argv)
	if !ok {
		return inv
	}
	inv.PolicyArgv = normalizedArgv
	inv.PolicyCWD = normalizedCWD
	return inv
}

func normalizeGitInvocation(cwd string, argv []string) ([]string, string, bool) {
	if len(argv) == 0 || filepath.Base(argv[0]) != "git" {
		return append([]string(nil), argv...), absPath(cwd), true
	}
	effectiveCWD := absPath(cwd)
	if effectiveCWD == "" {
		effectiveCWD = cwd
	}
	normalized := []string{"git"}
	i := 1
	changed := false
	for i < len(argv) {
		arg := argv[i]
		switch {
		case arg == "-C":
			if i+1 >= len(argv) {
				return append([]string(nil), argv...), effectiveCWD, false
			}
			effectiveCWD = resolveGitCWD(effectiveCWD, argv[i+1])
			i += 2
			changed = true
		case strings.HasPrefix(arg, "-C") && len(arg) > len("-C"):
			effectiveCWD = resolveGitCWD(effectiveCWD, strings.TrimPrefix(arg, "-C"))
			i++
			changed = true
		case arg == "-c":
			if i+1 >= len(argv) || !safeGitConfigOverride(argv[i+1]) {
				return append([]string(nil), argv...), effectiveCWD, false
			}
			i += 2
			changed = true
		case strings.HasPrefix(arg, "-c") && len(arg) > len("-c"):
			if !safeGitConfigOverride(strings.TrimPrefix(arg, "-c")) {
				return append([]string(nil), argv...), effectiveCWD, false
			}
			i++
			changed = true
		case strings.HasPrefix(arg, "-"):
			return append([]string(nil), argv...), effectiveCWD, false
		default:
			if changed {
				normalized = append(normalized, argv[i:]...)
				return normalized, effectiveCWD, true
			}
			return append([]string(nil), argv...), effectiveCWD, true
		}
	}
	if changed {
		return normalized, effectiveCWD, true
	}
	return append([]string(nil), argv...), effectiveCWD, true
}

func resolveGitCWD(current, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(current, target))
}

func safeGitConfigOverride(value string) bool {
	key, _, ok := strings.Cut(value, "=")
	if !ok {
		return false
	}
	switch strings.ToLower(key) {
	case "core.hookspath", "core.fsmonitor":
		return true
	default:
		return false
	}
}

func normalizeArgv(argv []string) string {
	return strings.Join(argv, "\x00")
}

func displayCommand(argv []string) string {
	return strings.Join(argv, " ")
}

func Classify(argv []string) OperatorFamily {
	argv = normalizeArgvForPolicy(argv)
	if len(argv) == 0 {
		return FamilyShellUnknown
	}
	name := filepath.Base(argv[0])
	if isToolVersionProbe(argv) || isCommandPathLookup(argv) || isStaticEnvironmentProbe(argv) {
		return FamilyEnvironment
	}
	if isPrintenvProbe(argv) {
		return FamilyEnvironment
	}
	if name == "git" {
		if isGitMetadata(argv) {
			return FamilyLocalRepoMetadata
		}
		if isGitHeadSubjectLog(argv) {
			return FamilyLocalRepoMetadata
		}
		if isGitRemoteMetadata(argv) {
			return FamilyLocalRepoMetadata
		}
		if isGitRepoState(argv) {
			return FamilyRepoState
		}
		if isGitReadOnlyDiff(argv) {
			return FamilyRepoState
		}
		if isGitMutation(argv) {
			return FamilyEditOrMutation
		}
		return FamilyShellUnknown
	}
	if (name == "rg" && len(argv) == 2 && argv[1] == "--files") || isFixedRgFileSearch(argv) {
		return FamilySearchList
	}
	if isDirectoryListing(argv) {
		return FamilySearchList
	}
	if isReplayableFileInspection(argv) {
		return FamilyFileInspection
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
	if isGitBranchShowCurrent(argv) {
		return true
	}
	if isGitAbbrevRefHead(argv) {
		return true
	}
	if len(argv) != 3 || filepath.Base(argv[0]) != "git" || argv[1] != "rev-parse" {
		return false
	}
	switch argv[2] {
	case "HEAD", "--git-dir", "--show-toplevel", "--is-inside-work-tree":
		return true
	}
	return false
}

func isGitAbbrevRefHead(argv []string) bool {
	return len(argv) == 4 && filepath.Base(argv[0]) == "git" && argv[1] == "rev-parse" && argv[2] == "--abbrev-ref" && argv[3] == "HEAD"
}

func isGitBranchShowCurrent(argv []string) bool {
	return len(argv) == 3 && filepath.Base(argv[0]) == "git" && argv[1] == "branch" && argv[2] == "--show-current"
}

func IsFastPathAllowed(argv []string) bool {
	argv = normalizeArgvForPolicy(argv)
	return isGitMetadata(argv) || isGitAbbrevRefHead(argv)
}

func IsProofGatedReplayCandidate(argv []string) bool {
	argv = normalizeArgvForPolicy(argv)
	return isGitStatusState(argv) ||
		isGitLsFiles(argv) ||
		isGitHeadSubjectLog(argv) ||
		isGitReadOnlyDiff(argv) ||
		isReplayableFileInspection(argv) ||
		isToolVersionProbe(argv) ||
		isCommandPathLookup(argv) ||
		isStaticEnvironmentProbe(argv) ||
		isPrintenvProbe(argv) ||
		isDirectoryListing(argv) ||
		isFixedRgFileSearch(argv)
}

func IsReplayAllowed(argv []string) bool {
	return IsFastPathAllowed(argv) || IsProofGatedReplayCandidate(argv)
}

func isGitRepoState(argv []string) bool {
	if len(argv) < 2 || filepath.Base(argv[0]) != "git" {
		return false
	}
	if isGitLsFiles(argv) {
		return true
	}
	if isGitStatusState(argv) {
		return true
	}
	if isGitReadOnlyDiff(argv) {
		return true
	}
	return false
}

func isGitLsFiles(argv []string) bool {
	if len(argv) == 2 && filepath.Base(argv[0]) == "git" && argv[1] == "ls-files" {
		return true
	}
	return len(argv) == 3 &&
		filepath.Base(argv[0]) == "git" &&
		argv[1] == "ls-files" &&
		safeRelativeInspectionPath(filepath.Clean(argv[2]))
}

func isGitStatusState(argv []string) bool {
	return len(argv) == 3 && filepath.Base(argv[0]) == "git" && argv[1] == "status" && (argv[2] == "--short" || argv[2] == "--porcelain")
}

func isGitHeadSubjectLog(argv []string) bool {
	return len(argv) == 4 &&
		filepath.Base(argv[0]) == "git" &&
		argv[1] == "log" &&
		argv[2] == "-1" &&
		argv[3] == "--format=%H%n%s"
}

func isGitReadOnlyDiff(argv []string) bool {
	if len(argv) < 2 || filepath.Base(argv[0]) != "git" || argv[1] != "diff" {
		return false
	}
	if len(argv) == 2 {
		return true
	}
	if len(argv) == 3 && argv[2] == "--stat" {
		return true
	}
	if len(argv) >= 4 && argv[2] == "--" {
		for _, path := range argv[3:] {
			if !safeRelativeInspectionPath(filepath.Clean(path)) {
				return false
			}
		}
		return true
	}
	return false
}

func isRepoSummaryReplayCandidate(argv []string) bool {
	argv = normalizeArgvForPolicy(argv)
	return isGitRepoState(argv) || isGitHeadSubjectLog(argv)
}

func isReplayableFileInspection(argv []string) bool {
	return isWarmFileBackedInspection(argv) || isReplayableFileType(argv)
}

func isWarmFileBackedInspection(argv []string) bool {
	return isReplayableCatFileRead(argv) || isBoundedSedPrint(argv) || isBoundedHeadPrint(argv) || isBoundedTailPrint(argv) || isFixedGrepFileSearch(argv) || isFixedRgFileSearch(argv)
}

func isManifestFileRead(argv []string) bool {
	return isReplayableCatFileRead(argv) && isWellKnownManifestName(filepath.Base(filepath.Clean(argv[1])))
}

func isReplayableCatFileRead(argv []string) bool {
	if len(argv) != 2 || filepath.Base(argv[0]) != "cat" {
		return false
	}
	path := filepath.Clean(argv[1])
	if !safeRelativeInspectionPath(path) {
		return false
	}
	return isReplayableInspectionName(filepath.Base(path))
}

func isReplayableFileType(argv []string) bool {
	if len(argv) != 2 || filepath.Base(argv[0]) != "file" {
		return false
	}
	path := filepath.Clean(argv[1])
	if !safeRelativeInspectionPath(path) {
		return false
	}
	return isReplayableInspectionName(filepath.Base(path))
}

func isFixedGrepFileSearch(argv []string) bool {
	_, _, _, ok := parseFixedGrepArgs(argv)
	return ok
}

func isFixedRgFileSearch(argv []string) bool {
	_, _, _, _, ok := parseFixedRgArgs(argv)
	return ok
}

func parseFixedGrepArgs(argv []string) (pattern, path string, quiet bool, ok bool) {
	if len(argv) != 4 && len(argv) != 5 {
		return "", "", false, false
	}
	if filepath.Base(argv[0]) != "grep" {
		return "", "", false, false
	}
	switch {
	case len(argv) == 4 && argv[1] == "-F":
		pattern = argv[2]
		path = argv[3]
	case len(argv) == 5 && argv[1] == "-F" && argv[2] == "-q":
		quiet = true
		pattern = argv[3]
		path = argv[4]
	case len(argv) == 5 && argv[1] == "-q" && argv[2] == "-F":
		quiet = true
		pattern = argv[3]
		path = argv[4]
	default:
		return "", "", false, false
	}
	if pattern == "" || strings.HasPrefix(pattern, "-") || strings.ContainsAny(pattern, "\x00\n\r") {
		return "", "", false, false
	}
	clean := filepath.Clean(path)
	if !safeRelativeInspectionPath(clean) || !isReplayableInspectionName(filepath.Base(clean)) {
		return "", "", false, false
	}
	return pattern, clean, quiet, true
}

func parseFixedRgArgs(argv []string) (pattern, path string, quiet, lineNumber bool, ok bool) {
	if len(argv) < 4 || len(argv) > 6 || filepath.Base(argv[0]) != "rg" {
		return "", "", false, false, false
	}
	var fixed bool
	var seenPattern bool
	for _, arg := range argv[1:] {
		switch arg {
		case "-F", "--fixed-strings":
			if fixed {
				return "", "", false, false, false
			}
			fixed = true
		case "-q", "--quiet":
			if quiet {
				return "", "", false, false, false
			}
			quiet = true
		case "-n", "--line-number":
			if lineNumber {
				return "", "", false, false, false
			}
			lineNumber = true
		default:
			if strings.HasPrefix(arg, "-") {
				return "", "", false, false, false
			}
			if !seenPattern {
				pattern = arg
				seenPattern = true
				continue
			}
			if path != "" {
				return "", "", false, false, false
			}
			path = arg
		}
	}
	if !fixed || pattern == "" || path == "" || strings.ContainsAny(pattern, "\x00\n\r") {
		return "", "", false, false, false
	}
	clean := filepath.Clean(path)
	if !safeRelativeInspectionPath(clean) || !isReplayableInspectionName(filepath.Base(clean)) {
		return "", "", false, false, false
	}
	return pattern, clean, quiet, lineNumber, true
}

func isBoundedSedPrint(argv []string) bool {
	if len(argv) != 4 || filepath.Base(argv[0]) != "sed" || argv[1] != "-n" {
		return false
	}
	if !isSimpleSedPrintRange(argv[2]) {
		return false
	}
	path := filepath.Clean(argv[3])
	if !safeRelativeInspectionPath(path) {
		return false
	}
	return isReplayableInspectionName(filepath.Base(path))
}

func isBoundedHeadPrint(argv []string) bool {
	if filepath.Base(firstArg(argv)) != "head" {
		return false
	}
	path, _, ok := parseHeadTailArgs(argv, false)
	if !ok {
		return false
	}
	return isReplayableInspectionName(filepath.Base(filepath.Clean(path)))
}

func isBoundedTailPrint(argv []string) bool {
	if filepath.Base(firstArg(argv)) != "tail" {
		return false
	}
	path, _, ok := parseHeadTailArgs(argv, true)
	if !ok {
		return false
	}
	return isReplayableInspectionName(filepath.Base(filepath.Clean(path)))
}

func firstArg(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return argv[0]
}

func parseHeadTailArgs(argv []string, tail bool) (string, int, bool) {
	if len(argv) < 2 || len(argv) > 4 {
		return "", 0, false
	}
	count := 10
	pathIndex := 1
	if len(argv) >= 3 {
		arg := argv[1]
		switch {
		case arg == "-n":
			if len(argv) != 4 {
				return "", 0, false
			}
			n, ok := parseHeadTailCount(argv[2], tail)
			if !ok {
				return "", 0, false
			}
			count = n
			pathIndex = 3
		case strings.HasPrefix(arg, "-n") && len(arg) > 2:
			if len(argv) != 3 {
				return "", 0, false
			}
			n, ok := parseHeadTailCount(arg[2:], tail)
			if !ok {
				return "", 0, false
			}
			count = n
			pathIndex = 2
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			if len(argv) != 3 {
				return "", 0, false
			}
			n, ok := parseHeadTailCount(arg[1:], tail)
			if !ok {
				return "", 0, false
			}
			count = n
			pathIndex = 2
		default:
			return "", 0, false
		}
	}
	if count < 1 || count > 1000 || pathIndex >= len(argv) {
		return "", 0, false
	}
	path := filepath.Clean(argv[pathIndex])
	if !safeRelativeInspectionPath(path) {
		return "", 0, false
	}
	return path, count, true
}

func parseHeadTailCount(s string, tail bool) (int, bool) {
	if s == "" {
		return 0, false
	}
	if tail && strings.HasPrefix(s, "+") {
		return 0, false
	}
	return parsePositiveSmallLine(s)
}

func isDirectoryListing(argv []string) bool {
	_, _, ok := parseDirectoryListing(argv)
	return ok
}

func parseDirectoryListing(argv []string) (string, string, bool) {
	if len(argv) < 1 || len(argv) > 3 || filepath.Base(argv[0]) != "ls" {
		return "", "", false
	}
	flag := ""
	path := "."
	switch len(argv) {
	case 1:
	case 2:
		if strings.HasPrefix(argv[1], "-") {
			if !isSupportedLSFlag(argv[1]) {
				return "", "", false
			}
			flag = argv[1]
		} else {
			path = argv[1]
		}
	case 3:
		if !isSupportedLSFlag(argv[1]) || strings.HasPrefix(argv[2], "-") {
			return "", "", false
		}
		flag = argv[1]
		path = argv[2]
	}
	clean := filepath.Clean(path)
	if clean == "" || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", "", false
	}
	return clean, flag, true
}

func isSupportedLSFlag(flag string) bool {
	switch flag {
	case "-p":
		return true
	default:
		return false
	}
}

func safeRelativeInspectionPath(path string) bool {
	return path != "." &&
		path != "" &&
		!strings.HasPrefix(path, "-") &&
		!filepath.IsAbs(path) &&
		!strings.HasPrefix(path, ".."+string(filepath.Separator)) &&
		path != ".." &&
		!pathContainsHiddenOrVCSPart(path)
}

func pathContainsHiddenOrVCSPart(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".git" || part == ".squire" || strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func isSimpleSedPrintRange(expr string) bool {
	if !strings.HasSuffix(expr, "p") {
		return false
	}
	body := strings.TrimSuffix(expr, "p")
	if body == "" || strings.ContainsAny(body, "$/\\{}[];!qadciw=") {
		return false
	}
	parts := strings.Split(body, ",")
	if len(parts) > 2 {
		return false
	}
	start, ok := parsePositiveSmallLine(parts[0])
	if !ok {
		return false
	}
	end := start
	if len(parts) == 2 {
		var endOK bool
		end, endOK = parsePositiveSmallLine(parts[1])
		if !endOK {
			return false
		}
	}
	return start <= end && end-start <= 500
}

func parsePositiveSmallLine(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 10000 {
			return 0, false
		}
	}
	return n, n > 0
}

func isWellKnownManifestName(name string) bool {
	switch name {
	case "go.mod", "go.sum", "go.work", "go.work.sum",
		"package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "tsconfig.json",
		"Cargo.toml", "Cargo.lock", "rust-toolchain", "rust-toolchain.toml",
		"pyproject.toml", "poetry.lock", "requirements.txt", "requirements-dev.txt", "setup.cfg", "tox.ini",
		"Makefile", "makefile", "Dockerfile", "docker-compose.yml", "compose.yml":
		return true
	default:
		return false
	}
}

func isReplayableInspectionName(name string) bool {
	if isSensitiveInspectionName(name) {
		return false
	}
	if isWellKnownManifestName(name) {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs",
		".py", ".rs", ".java", ".kt", ".kts", ".rb", ".php",
		".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".swift",
		".sh", ".bash", ".zsh", ".fish", ".sql",
		".css", ".scss", ".sass", ".html", ".htm",
		".json", ".jsonc", ".toml", ".yaml", ".yml", ".xml",
		".md", ".markdown", ".txt":
		return true
	default:
		return false
	}
}

func isSensitiveInspectionName(name string) bool {
	lower := strings.ToLower(name)
	if lower == ".env" ||
		strings.HasSuffix(lower, ".env") ||
		strings.HasSuffix(lower, ".pem") ||
		strings.HasSuffix(lower, ".p12") ||
		strings.HasSuffix(lower, ".pfx") ||
		strings.HasSuffix(lower, ".key") ||
		lower == "id_rsa" ||
		lower == "id_ed25519" ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "credential") ||
		strings.Contains(lower, "token") ||
		strings.Contains(lower, "api_key") ||
		strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "private_key") ||
		strings.Contains(lower, "privatekey") {
		return true
	}
	return false
}

func isToolVersionProbe(argv []string) bool {
	if len(argv) != 2 {
		return false
	}
	name := filepath.Base(argv[0])
	arg := argv[1]
	switch name {
	case "git":
		return arg == "--version" || arg == "version"
	case "go":
		return arg == "version"
	case "node", "npm", "pnpm", "yarn":
		return arg == "--version" || arg == "-v"
	case "python", "python3", "pip", "pip3", "cargo", "rustc", "rg":
		return arg == "--version"
	default:
		return false
	}
}

func isCommandPathLookup(argv []string) bool {
	if len(argv) == 2 && filepath.Base(argv[0]) == "which" {
		target := argv[1]
		if target == "" || strings.ContainsAny(target, `/\`) || strings.HasPrefix(target, "-") {
			return false
		}
		return isCommonToolName(target)
	}
	if len(argv) != 3 || filepath.Base(argv[0]) != "command" || argv[1] != "-v" {
		return false
	}
	target := argv[2]
	if target == "" || strings.ContainsAny(target, `/\`) || strings.HasPrefix(target, "-") {
		return false
	}
	return isCommonToolName(target)
}

func isStaticEnvironmentProbe(argv []string) bool {
	if len(argv) == 0 || len(argv) > 2 {
		return false
	}
	name := filepath.Base(argv[0])
	switch name {
	case "whoami", "hostname", "id":
		return len(argv) == 1
	case "uname":
		if len(argv) == 1 {
			return true
		}
		switch argv[1] {
		case "-a", "-m", "-n", "-r", "-s", "-v":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func isPrintenvProbe(argv []string) bool {
	if len(argv) != 2 || filepath.Base(argv[0]) != "printenv" {
		return false
	}
	return safePrintenvName(argv[1])
}

func safePrintenvName(name string) bool {
	if name == "" || len(name) > 128 || sensitiveEnvironmentName(name) {
		return false
	}
	for i, r := range name {
		if i == 0 && (r >= '0' && r <= '9') {
			return false
		}
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}

func sensitiveEnvironmentName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{
		"TOKEN",
		"SECRET",
		"PASSWORD",
		"PASSWD",
		"AUTH",
		"CREDENTIAL",
		"COOKIE",
		"BEARER",
		"PRIVATE",
		"API_KEY",
		"APIKEY",
		"ACCESS_KEY",
		"REFRESH_TOKEN",
		"SESSION_TOKEN",
	} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func commandPathLookupTarget(argv []string) string {
	if len(argv) == 2 && filepath.Base(argv[0]) == "which" {
		return argv[1]
	}
	if len(argv) == 3 && filepath.Base(argv[0]) == "command" && argv[1] == "-v" {
		return argv[2]
	}
	return ""
}

func isCommonToolName(name string) bool {
	switch name {
	case "git", "rg", "go", "node", "npm", "pnpm", "yarn", "python", "python3", "pip", "pip3", "cargo", "rustc", "make":
		return true
	default:
		return false
	}
}

func normalizeArgvForPolicy(argv []string) []string {
	normalized, _, ok := normalizeGitInvocation("", argv)
	if !ok {
		return argv
	}
	return normalized
}

func isGitRemoteMetadata(argv []string) bool {
	if len(argv) == 3 && filepath.Base(argv[0]) == "git" && argv[1] == "remote" && argv[2] == "-v" {
		return true
	}
	return len(argv) == 4 && filepath.Base(argv[0]) == "git" && argv[1] == "remote" && argv[2] == "get-url" && argv[3] == "origin"
}

func isGitMutation(argv []string) bool {
	if len(argv) < 2 || filepath.Base(argv[0]) != "git" {
		return false
	}
	switch argv[1] {
	case "add", "am", "apply", "bisect", "branch", "checkout", "cherry-pick", "clean", "commit", "fetch", "merge", "mv", "pull", "push", "rebase", "reset", "restore", "revert", "rm", "stash", "submodule", "switch", "tag", "worktree":
		if isGitBranchShowCurrent(argv) {
			return false
		}
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
	if name == "node" {
		for _, arg := range argv[1:] {
			if arg == "--test" {
				return true
			}
		}
	}
	if len(argv) >= 3 && (name == "python" || name == "python3") && argv[1] == "-m" && (argv[2] == "pytest" || argv[2] == "unittest") {
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
	if len(argv) == 3 && filepath.Base(argv[0]) == "command" && argv[1] == "-v" && isCommonToolName(argv[2]) {
		path, ok := resolveExecutablePath(cwd, argv[2])
		if !ok {
			return NativeResult{ExitCode: 1, Wall: time.Since(start)}
		}
		return NativeResult{Stdout: []byte(path + "\n"), ExitCode: 0, Wall: time.Since(start)}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	configureNativeCommandCleanup(cmd)
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

func RunNativeDirect(ctx context.Context, cwd string, argv []string) RunResult {
	var phases PhaseTimings
	classifyStart := time.Now()
	inv := NormalizeInvocation(cwd, argv)
	family := Classify(inv.PolicyArgv)
	phases.ClassifyMS = elapsedMS(classifyStart)

	return runNativeDirectInvocation(ctx, inv, family, phases)
}

func RunNativeDirectInvocation(ctx context.Context, inv CommandInvocation, family OperatorFamily) RunResult {
	return runNativeDirectInvocation(ctx, inv, family, PhaseTimings{})
}

func runNativeDirectInvocation(ctx context.Context, inv CommandInvocation, family OperatorFamily, phases PhaseTimings) RunResult {
	nativeStart := time.Now()
	native := runNative(ctx, inv.OriginalCWD, inv.OriginalArgv)
	phases.NativeExecWaitMS = elapsedMS(nativeStart)
	mode := ModeNative
	if family == FamilyValidation || family == FamilyEditOrMutation || family == FamilyPackageSetup {
		mode = ModeNever
	}
	return RunResult{
		Stdout:     native.Stdout,
		Stderr:     native.Stderr,
		ExitCode:   native.ExitCode,
		Mode:       mode,
		Family:     family,
		NativeWall: native.Wall,
		Phases:     phases,
	}
}

func resolveExecutablePath(cwd, name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, filepath.Separator) {
		return "", false
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return filepath.Clean(candidate), true
	}
	return "", false
}

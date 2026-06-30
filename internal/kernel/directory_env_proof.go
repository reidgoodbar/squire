package kernel

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const maxReplayableDirectoryEntries = 2000

func staticEnvironmentProof(cwd string, argv []string) (map[string]string, string, bool) {
	argv = normalizeArgvForPolicy(argv)
	if !isStaticEnvironmentProbe(argv) {
		return nil, "", false
	}
	name := filepath.Base(argv[0])
	signal, ok := executableSignal(cwd, name)
	if !ok {
		return nil, "", false
	}
	hostname, _ := os.Hostname()
	pathEnv := hashString(os.Getenv("PATH"))
	envHash := deterministicStaticEnvHash()
	fp := map[string]string{
		"static_probe":       hashString(name),
		"static_probe_argv":  hashString(normalizeArgv(argv)),
		"tool_path":          signal.PathHash,
		"tool_executable":    signal.FileHash,
		"path_env":           pathEnv,
		"static_env":         envHash,
		"hostname":           hashString(hostname),
		"process_identity":   processIdentitySignal(),
		"static_probe_scope": hashString("session-local-environment"),
	}
	epoch := "static-env:" + hashString(name+"|"+normalizeArgv(argv)+"|"+signal.PathHash+"|"+signal.FileHash+"|"+pathEnv+"|"+envHash+"|"+hostname+"|"+fp["process_identity"])
	return fp, epoch, true
}

func printenvProof(cwd string, argv []string) (map[string]string, string, bool) {
	argv = normalizeArgvForPolicy(argv)
	if !isPrintenvProbe(argv) {
		return nil, "", false
	}
	name := argv[1]
	signal, ok := executableSignal(cwd, "printenv")
	if !ok {
		return nil, "", false
	}
	value, exists := os.LookupEnv(name)
	pathEnv := hashString(os.Getenv("PATH"))
	fp := map[string]string{
		"printenv_name":        hashString(name),
		"printenv_value":       hashString(value),
		"printenv_exists":      hashString(strconv.FormatBool(exists)),
		"tool_path":            signal.PathHash,
		"tool_executable":      signal.FileHash,
		"path_env":             pathEnv,
		"printenv_probe_scope": hashString("session-local-environment"),
	}
	epoch := "printenv:" + hashString(name+"|"+strconv.FormatBool(exists)+"|"+value+"|"+signal.PathHash+"|"+signal.FileHash+"|"+pathEnv)
	return fp, epoch, true
}

func directoryListingProof(cwd string, argv []string, ws WorldState) (map[string]string, string, bool) {
	argv = normalizeArgvForPolicy(argv)
	target, flag, ok := parseDirectoryListing(argv)
	if !ok {
		return nil, "", false
	}
	root := ws.RepoRoot
	if root == "" {
		if discovered, _, ok := discoverGitDir(cwd); ok {
			root = discovered
		}
	}
	if root == "" {
		root = absPath(cwd)
	}
	absTarget := filepath.Clean(filepath.Join(cwd, target))
	realTarget, err := filepath.EvalSymlinks(absTarget)
	if err != nil {
		return nil, "", false
	}
	realTarget = filepath.Clean(realTarget)
	if !pathWithinRoot(realTarget, root) {
		return nil, "", false
	}
	info, err := os.Stat(realTarget)
	if err != nil || !info.IsDir() {
		return nil, "", false
	}
	toolSignal, ok := executableSignal(cwd, "ls")
	if !ok {
		return nil, "", false
	}
	dirEpoch, ok := directoryEntryEpoch(realTarget)
	if !ok {
		return nil, "", false
	}
	envHash := directoryListingEnvHash()
	rel := "."
	if r, err := filepath.Rel(root, realTarget); err == nil {
		rel = filepath.ToSlash(r)
	}
	fp := map[string]string{
		"directory_command": hashString(normalizeArgv(argv)),
		"directory_path":    hashString(rel),
		"directory_flag":    hashString(flag),
		"directory_epoch":   dirEpoch,
		"directory_env":     envHash,
		"tool_name":         hashString("ls"),
		"tool_path":         toolSignal.PathHash,
		"tool_executable":   toolSignal.FileHash,
		"passwd":            fileHashOrMissing("/etc/passwd"),
		"group":             fileHashOrMissing("/etc/group"),
		"localtime":         fileHashOrMissing("/etc/localtime"),
	}
	epoch := "directory-listing:" + hashString(realTarget+"|"+normalizeArgv(argv)+"|"+dirEpoch+"|"+envHash+"|"+toolSignal.FileHash+"|"+fp["passwd"]+"|"+fp["group"]+"|"+fp["localtime"])
	return fp, epoch, true
}

func directoryEntryEpoch(dir string) (string, bool) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > maxReplayableDirectoryEntries {
		return "", false
	}
	parts := []string{"self\x00" + fileHashStatSignal(info)}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		st, err := os.Lstat(path)
		if err != nil {
			return "", false
		}
		linkTarget := ""
		if st.Mode()&os.ModeSymlink != 0 {
			linkTarget, _ = os.Readlink(path)
		}
		parts = append(parts, strings.Join([]string{
			entry.Name(),
			strconv.FormatBool(entry.IsDir()),
			strconv.FormatInt(st.Size(), 10),
			st.Mode().String(),
			fileHashStatSignal(st),
			linkTarget,
		}, "\x00"))
	}
	sort.Strings(parts)
	return hashString(strings.Join(parts, "\n")), true
}

func directoryListingEnvHash() string {
	keys := []string{
		"LC_ALL",
		"LC_COLLATE",
		"LC_CTYPE",
		"LANG",
		"TZ",
		"COLUMNS",
		"CLICOLOR",
		"CLICOLOR_FORCE",
		"LSCOLORS",
		"LS_COLORS",
		"BLOCKSIZE",
	}
	return hashSelectedEnvironment(keys)
}

func deterministicStaticEnvHash() string {
	keys := []string{
		"USER",
		"LOGNAME",
		"HOME",
		"SHELL",
		"HOSTNAME",
		"LANG",
		"LC_ALL",
		"TZ",
	}
	return hashSelectedEnvironment(keys)
}

func fileCommandEnvHash() string {
	keys := []string{
		"LC_ALL",
		"LC_CTYPE",
		"LANG",
		"MAGIC",
	}
	return hashSelectedEnvironment(keys)
}

func hashSelectedEnvironment(keys []string) string {
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+hashString(os.Getenv(key)))
	}
	return hashString(strings.Join(parts, "\n"))
}

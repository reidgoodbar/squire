package proofcache

import "testing"

func TestReplayCandidates(t *testing.T) {
	cases := []struct {
		name             string
		argv             []string
		expectProofGated bool
		expectFast       bool
		expectHot        bool
		expectClass      OperatorFamily
	}{
		{"cat manifest", []string{"cat", "package.json"}, true, false, true, FamilyFileInspection},
		{"cat source", []string{"cat", "src/app.js"}, true, false, true, FamilyFileInspection},
		{"cat outside", []string{"cat", "./../../etc/passwd"}, false, false, false, FamilyShellUnknown},
		{"cat sensitive", []string{"cat", "secret_token.txt"}, false, false, false, FamilyShellUnknown},
		{"sed -n inspect", []string{"sed", "-n", "1,10p", "README.md"}, true, false, true, FamilyFileInspection},
		{"sed unbounded", []string{"sed", "-n", "$p", "README.md"}, false, false, false, FamilyShellUnknown},
		{"head inspect", []string{"head", "-n", "20", "README.md"}, true, false, true, FamilyFileInspection},
		{"head compact inspect", []string{"head", "-20", "README.md"}, true, false, true, FamilyFileInspection},
		{"tail inspect", []string{"tail", "-n", "50", "README.md"}, true, false, true, FamilyFileInspection},
		{"tail follow unsupported", []string{"tail", "-f", "README.md"}, false, false, false, FamilyShellUnknown},
		{"tail plus unsupported", []string{"tail", "-n", "+5", "README.md"}, false, false, false, FamilyShellUnknown},
		{"file inspect", []string{"file", "README.md"}, true, false, true, FamilyFileInspection},
		{"file outside", []string{"file", "../README.md"}, false, false, false, FamilyShellUnknown},
		{"grep fixed", []string{"grep", "-F", "Squire", "README.md"}, true, false, true, FamilyFileInspection},
		{"grep fixed quiet", []string{"grep", "-q", "-F", "Squire", "README.md"}, true, false, true, FamilyFileInspection},
		{"grep regex unsupported", []string{"grep", "Squire.*Engine", "README.md"}, false, false, false, FamilyShellUnknown},
		{"ls root", []string{"ls"}, true, false, true, FamilySearchList},
		{"ls p", []string{"ls", "-p"}, true, false, true, FamilySearchList},
		{"ls la unsupported", []string{"ls", "-la"}, false, false, false, FamilyShellUnknown},
		{"ls unsupported", []string{"ls", "-R"}, false, false, false, FamilyShellUnknown},
		{"rg files", []string{"rg", "--files"}, false, false, false, FamilySearchList},
		{"rg fixed file", []string{"rg", "-F", "rank_routes", "src/app.js"}, true, false, true, FamilySearchList},
		{"rg fixed line numbers", []string{"rg", "-n", "-F", "rank_routes", "src/app.js"}, true, false, true, FamilySearchList},
		{"rg fixed quiet", []string{"rg", "-q", "-F", "rank_routes", "src/app.js"}, true, false, true, FamilySearchList},
		{"rg literal content", []string{"rg", "rank_routes", "src", "tests"}, false, false, false, FamilyShellUnknown},
		{"rg regex content", []string{"rg", "rank_routes|filter_routes", "src", "tests"}, false, false, false, FamilyShellUnknown},
		{"git status short", []string{"git", "status", "--short"}, true, false, true, FamilyRepoState},
		{"git status porcelain", []string{"git", "status", "--porcelain"}, true, false, true, FamilyRepoState},
		{"git ls-files", []string{"git", "ls-files"}, true, false, true, FamilyRepoState},
		{"git ls-files path", []string{"git", "ls-files", "src/app"}, true, false, true, FamilyRepoState},
		{"git ls-files hidden path", []string{"git", "ls-files", ".git"}, false, false, false, FamilyShellUnknown},
		{"git diff", []string{"git", "diff"}, true, false, true, FamilyRepoState},
		{"git diff stat", []string{"git", "diff", "--stat"}, true, false, true, FamilyRepoState},
		{"git diff path", []string{"git", "diff", "--", "src/app.js"}, true, false, true, FamilyRepoState},
		{"git log head subject", []string{"git", "log", "-1", "--format=%H%n%s"}, true, false, true, FamilyLocalRepoMetadata},
		{"which python", []string{"which", "python3"}, true, false, true, FamilyEnvironment},
		{"command v node", []string{"command", "-v", "node"}, true, false, true, FamilyEnvironment},
		{"python --version", []string{"python3", "--version"}, true, false, true, FamilyEnvironment},
		{"pip --version", []string{"pip", "--version"}, true, false, true, FamilyEnvironment},
		{"whoami", []string{"whoami"}, true, false, true, FamilyEnvironment},
		{"uname m", []string{"uname", "-m"}, true, false, true, FamilyEnvironment},
		{"hostname", []string{"hostname"}, true, false, true, FamilyEnvironment},
		{"id", []string{"id"}, true, false, true, FamilyEnvironment},
		{"printenv safe", []string{"printenv", "PATH"}, true, false, true, FamilyEnvironment},
		{"printenv sensitive", []string{"printenv", "OPENAI_API_KEY"}, false, false, false, FamilyShellUnknown},
		{"validation", []string{"npm", "test"}, false, false, false, FamilyValidation},
		{"python unittest validation", []string{"python3", "-m", "unittest", "discover", "-s", "tests"}, false, false, false, FamilyValidation},
		{"node test validation", []string{"node", "--test", "tests/planner.test.js"}, false, false, false, FamilyValidation},
		{"node ts test validation", []string{"node", "--experimental-strip-types", "--test", "tests/planner.test.ts"}, false, false, false, FamilyValidation},
		{"mutation", []string{"git", "commit", "-m", "x"}, false, false, false, FamilyEditOrMutation},
		{"package install", []string{"npm", "install"}, false, false, false, FamilyPackageSetup},
		{"git rev-parse HEAD", []string{"git", "rev-parse", "HEAD"}, false, true, true, FamilyLocalRepoMetadata},
		{"git branch show current", []string{"git", "branch", "--show-current"}, false, true, true, FamilyLocalRepoMetadata},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if IsProofGatedReplayCandidate(c.argv) != c.expectProofGated {
				t.Fatalf("%s: IsProofGatedReplayCandidate got %v want %v", c.name, IsProofGatedReplayCandidate(c.argv), c.expectProofGated)
			}
			if IsFastPathAllowed(c.argv) != c.expectFast {
				t.Fatalf("%s: IsFastPathAllowed got %v want %v", c.name, IsFastPathAllowed(c.argv), c.expectFast)
			}
			if isHotPreparedReplayCandidate(c.argv) != c.expectHot {
				t.Fatalf("%s: isHotPreparedReplayCandidate got %v want %v", c.name, isHotPreparedReplayCandidate(c.argv), c.expectHot)
			}
			fam := Classify(c.argv)
			if fam != c.expectClass {
				t.Fatalf("%s: Classify got %v want %v", c.name, fam, c.expectClass)
			}
		})
	}
}

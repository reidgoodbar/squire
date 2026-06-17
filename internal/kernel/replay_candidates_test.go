package kernel

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
		{"rg files", []string{"rg", "--files"}, false, false, false, FamilySearchList},
		{"rg literal content", []string{"rg", "rank_routes", "src", "tests"}, false, false, false, FamilySearchList},
		{"rg regex content", []string{"rg", "rank_routes|filter_routes", "src", "tests"}, false, false, false, FamilyShellUnknown},
		{"git status short", []string{"git", "status", "--short"}, true, false, true, FamilyRepoState},
		{"git status porcelain", []string{"git", "status", "--porcelain"}, true, false, true, FamilyRepoState},
		{"git ls-files", []string{"git", "ls-files"}, true, false, true, FamilyRepoState},
		{"git diff", []string{"git", "diff"}, true, false, true, FamilyRepoState},
		{"git diff stat", []string{"git", "diff", "--stat"}, true, false, true, FamilyRepoState},
		{"git diff path", []string{"git", "diff", "--", "src/app.js"}, true, false, true, FamilyRepoState},
		{"which python", []string{"which", "python3"}, true, false, true, FamilyEnvironment},
		{"command v node", []string{"command", "-v", "node"}, true, false, true, FamilyEnvironment},
		{"python --version", []string{"python3", "--version"}, true, false, true, FamilyEnvironment},
		{"pip --version", []string{"pip", "--version"}, true, false, true, FamilyEnvironment},
		{"validation", []string{"npm", "test"}, false, false, false, FamilyValidation},
		{"python unittest validation", []string{"python3", "-m", "unittest", "discover", "-s", "tests"}, false, false, false, FamilyValidation},
		{"node test validation", []string{"node", "--test", "tests/planner.test.js"}, false, false, false, FamilyValidation},
		{"node ts test validation", []string{"node", "--experimental-strip-types", "--test", "tests/planner.test.ts"}, false, false, false, FamilyValidation},
		{"mutation", []string{"git", "commit", "-m", "x"}, false, false, false, FamilyEditOrMutation},
		{"package install", []string{"npm", "install"}, false, false, false, FamilyPackageSetup},
		{"git rev-parse HEAD", []string{"git", "rev-parse", "HEAD"}, false, true, true, FamilyLocalRepoMetadata},
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

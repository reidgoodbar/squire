package proofcache

import "testing"

func TestProductionRuntimeInvocationPolicy(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want bool
	}{
		{name: "metadata", argv: []string{"git", "rev-parse", "HEAD"}, want: true},
		{name: "version probe is native", argv: []string{"git", "--version"}, want: false},
		{name: "path probe is native", argv: []string{"command", "-v", "python3"}, want: false},
		{name: "rg files is nondeterministic", argv: []string{"rg", "--files"}, want: false},
		{name: "file magic database is native", argv: []string{"file", "README.md"}, want: false},
		{name: "whoami identity database is native", argv: []string{"whoami"}, want: false},
		{name: "id identity database is native", argv: []string{"id"}, want: false},
		{name: "hostname has current proof", argv: []string{"hostname"}, want: true},
		{name: "uname has current proof", argv: []string{"uname", "-m"}, want: true},
		{name: "composed metadata", argv: []string{"sh", "-c", "git rev-parse HEAD"}, want: true},
		{name: "composed filter", argv: []string{"sh", "-c", "git ls-files | grep -F src"}, want: true},
		{name: "composed short head", argv: []string{"sh", "-c", "git ls-files | head -20"}, want: true},
		{name: "composed working directory", argv: []string{"sh", "-c", "pwd && git rev-parse HEAD"}, want: true},
		{name: "bounded regex rg", argv: []string{"sh", "-c", `rg -n "parse_arg_id\(|is_name_start\(" include test --glob '*.{h,cc}' | head -n 80`}, want: true},
		{name: "bounded regex rg negative glob", argv: []string{"sh", "-c", `rg -n --hidden -S "named|identifier" . --glob '!build/**'`}, want: true},
		{name: "bounded fixed rg glob", argv: []string{"sh", "-c", `rg -n -F "named" src --glob '*.go'`}, want: true},
		{name: "escaped quote regex", argv: []string{"sh", "-c", `rg -n "arg\(\"[A-Z]" test --glob '*.cc' | head -80`}, want: true},
		{name: "newline sequence", argv: []string{"sh", "-c", "git status --short\ngit rev-parse HEAD"}, want: true},
		{name: "command substitution rejected", argv: []string{"sh", "-c", `rg -n "$(touch marker)" .`}, want: false},
		{name: "variable expansion rejected", argv: []string{"sh", "-c", `rg -n "$PATTERN" .`}, want: false},
		{name: "dangerous rg pre rejected", argv: []string{"sh", "-c", `rg --pre 'touch marker' needle .`}, want: false},
		{name: "numbered source window", argv: []string{"sh", "-c", "nl -ba README.md | sed -n '1,20p'"}, want: true},
		{name: "multi-range source window", argv: []string{"sh", "-c", "sed -n '1,20p;40,50p' README.md | tail -n 5"}, want: true},
		{name: "numbered multi-range window", argv: []string{"sh", "-c", "nl -ba README.md | sed -n '1,20p;40,50p'"}, want: true},
		{name: "numbered rst window", argv: []string{"sh", "-c", "nl -ba docs/api.rst | sed -n '1,20p'"}, want: true},
		{name: "unsupported numbered style", argv: []string{"sh", "-c", "nl -bt README.md | sed -n '1,20p'"}, want: false},
		{name: "composed build", argv: []string{"sh", "-c", "go test ./..."}, want: false},
		{name: "composed rg files", argv: []string{"sh", "-c", "rg --files | head -n 5"}, want: false},
		{name: "unsupported filter", argv: []string{"sh", "-c", "git ls-files | python3 -c pass"}, want: false},
		{name: "legacy loop", argv: []string{"sh", "-c", "for i in 1 2; do git rev-parse HEAD; done"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsProductionRuntimeInvocationAllowed(t.TempDir(), test.argv); got != test.want {
				t.Fatalf("IsProductionRuntimeInvocationAllowed(%q) = %v, want %v", test.argv, got, test.want)
			}
		})
	}
}

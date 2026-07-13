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

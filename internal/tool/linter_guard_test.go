package tool

import "testing"

func TestIsLinterInstall(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"cd /home/user && go vet ./... 2>&1 | head -100", false},
		{"go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest", true},
		{"go install honnef.co/go/tools/cmd/staticcheck@latest", true},
		{"npm install -g eslint", true},
		{"pip install pylint", true},
		{"cargo install cargo-fuzz", false}, // not a known linter name
		{"brew install go", false},          // install, but not a linter
		{"go install example.com/foo/cmd/bar@latest", false},
	}
	for _, c := range cases {
		if got := isLinterInstall(c.cmd); got != c.want {
			t.Errorf("isLinterInstall(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestLinterName(t *testing.T) {
	if got := linterName("go install golangci-lint@latest"); got != "golangci-lint" {
		t.Errorf("linterName = %q, want golangci-lint", got)
	}
	if got := linterName("npm i -g some-random-tool"); got != "a linter" {
		t.Errorf("linterName = %q, want generic phrase", got)
	}
}

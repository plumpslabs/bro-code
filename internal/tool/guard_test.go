package tool

import (
	"strings"
	"testing"
)

func TestGuardSensitivePath(t *testing.T) {
	cases := []struct {
		path string
		ok   bool // true = allowed
	}{
		{".env", false},
		{".env.local", false},
		{".env.production", false},
		{"config/.env", false},
		{"backend/.env.test", false},
		{"keys/id_rsa", false},
		{"cert.pem", false},
		{"client.key", false},
		{"service-account.json", false},
		{"secrets.yaml", false},
		{"main.go", true},
		{"internal/app/handler.go", true},
		{"README.md", true},
		{"package.json", true},
		{"src/index.ts", true},
	}
	for _, c := range cases {
		err := GuardSensitivePath(c.path)
		if c.ok && err != nil {
			t.Errorf("GuardSensitivePath(%q) = %v, want nil", c.path, err)
		}
		if !c.ok && err == nil {
			t.Errorf("GuardSensitivePath(%q) = nil, want block", c.path)
		}
	}
}

func TestGuardHeavyPath(t *testing.T) {
	cases := []struct {
		path string
		ok   bool
	}{
		{"node_modules/pkg/index.js", false},
		{"frontend/node_modules/react/index.js", false},
		{"vendor/autoload.php", false},
		{"target/debug/build/x", false},
		{"__pycache__/x.pyc", false},
		{"dist/bundle.js", false},
		{"build/out/x", false},
		{".git/objects/x", false},
		{"venv/lib/x.py", false},
		{"internal/app/handler.go", true},
		{"src/components/Button.tsx", true},
	}
	for _, c := range cases {
		err := GuardHeavyPath(c.path)
		if c.ok && err != nil {
			t.Errorf("GuardHeavyPath(%q) = %v, want nil", c.path, err)
		}
		if !c.ok && err == nil {
			t.Errorf("GuardHeavyPath(%q) = nil, want block", c.path)
		}
	}
}

func TestGuardFileCombined(t *testing.T) {
	if err := GuardFile("node_modules/x/.env"); err == nil {
		t.Error("GuardFile must block .env inside node_modules")
	}
	if err := GuardFile("src/main.go"); err != nil {
		t.Errorf("GuardFile(src/main.go) = %v, want nil", err)
	}
}

func TestGuardSensitiveCommand(t *testing.T) {
	cases := []struct {
		cmd string
		ok  bool
	}{
		{"cat .env", false},
		{"cat .env.production", false},
		{"head -5 .env.local", false},
		{"less id_rsa", false},
		{"sudo cat .env", false},
		{"cat credentials.json", false},
		{"ls -la", true},
		{"grep -rn .env.example .", true},
		{"cat package.json", true},
		{"git status", true},
		{"cat src/config.ts", true},
	}
	for _, c := range cases {
		msg := GuardSensitiveCommand(c.cmd)
		if c.ok && msg != "" {
			t.Errorf("GuardSensitiveCommand(%q) = %q, want allowed", c.cmd, msg)
		}
		if !c.ok && msg == "" {
			t.Errorf("GuardSensitiveCommand(%q) = allowed, want blocked", c.cmd)
		}
	}
	if !strings.Contains(GuardSensitiveCommand("cat .env"), "⛔") {
		t.Error("block message must be a clear hard-block (⛔)")
	}
}

func TestIsHeavyDir(t *testing.T) {
	for _, d := range []string{"node_modules", "vendor", "target", "__pycache__", "dist", ".git", "venv", ".venv"} {
		if !IsHeavyDir(d) {
			t.Errorf("IsHeavyDir(%q) = false, want true", d)
		}
	}
	if IsHeavyDir("src") || IsHeavyDir("internal") {
		t.Error("src/internal must not be heavy dirs")
	}
}

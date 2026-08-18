package repo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverWorkspaceSingleRepo(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0755); err != nil {
		t.Fatal(err)
	}

	ws := DiscoverWorkspace(dir)
	if len(ws.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(ws.Repos))
	}
	if ws.Repos[0].Path != dir {
		t.Errorf("repo path = %s, want %s", ws.Repos[0].Path, dir)
	}
}

func TestDiscoverWorkspaceMultiRepo(t *testing.T) {
	dir := t.TempDir()

	// Sibling microservices
	authDir := filepath.Join(dir, "auth-service")
	payDir := filepath.Join(dir, "payment-service")
	webDir := filepath.Join(dir, "web-portal")

	for _, p := range []string{authDir, payDir, webDir} {
		if err := os.MkdirAll(filepath.Join(p, ".git"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	ws := DiscoverWorkspace(dir)
	if len(ws.Repos) != 3 {
		t.Fatalf("expected 3 repos, got %d", len(ws.Repos))
	}

	match := ws.FindRepoForPath(filepath.Join(authDir, "src", "jwt.go"))
	if match == nil || match.Name != "auth-service" {
		t.Errorf("expected auth-service match, got %v", match)
	}

	matchPay := ws.FindRepoForPath(filepath.Join(payDir, "api", "handler.go"))
	if matchPay == nil || matchPay.Name != "payment-service" {
		t.Errorf("expected payment-service match, got %v", matchPay)
	}
}

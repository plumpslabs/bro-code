package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/search"
)

func TestBlastRadiusTool(t *testing.T) {
	dir := t.TempDir()

	_ = os.WriteFile(filepath.Join(dir, "auth.go"), []byte(`package auth

func AuthenticateUser(token string) bool {
	return token != ""
}
`), 0o644)

	_ = os.WriteFile(filepath.Join(dir, "server.go"), []byte(`package main

func main() {
	AuthenticateUser("secret")
}
`), 0o644)

	idx := search.BuildGlobalIndex(dir)
	tool := &BlastRadiusTool{Index: idx}

	out, err := tool.Execute(context.Background(), `{"target":"AuthenticateUser"}`)
	if err != nil {
		t.Fatalf("BlastRadiusTool failed: %v", err)
	}
	if !strings.Contains(out, "AuthenticateUser") || !strings.Contains(out, "server.go") {
		t.Errorf("expected tool output to list AuthenticateUser and server.go, got: %s", out)
	}
}

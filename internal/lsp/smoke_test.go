//go:build smoke

package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClangdSmoke verifies the LSP client against a REAL clangd server.
// Run explicitly: go test -tags smoke -run TestClangdSmoke ./internal/lsp/ -v
func TestClangdSmoke(t *testing.T) {
	if !binaryExists("clangd") {
		t.Skip("clangd not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.c")
	content := `int add(int a, int b) { return a + b; }
int main() { return add(1, 2); }
int broken() { return undefined_symbol + ; }
`
	if err := os.WriteFile(src, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	time.Sleep(1500 * time.Millisecond) // let clangd parse before querying
	def, err := m.Definition(ctx, src, 2, 21)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	t.Logf("Definition:\n%s", def)
	if !strings.Contains(def, "main.c:1") {
		t.Errorf("definition should point at line 1 (add), got:\n%s", def)
	}

	refs, err := m.References(ctx, src, 2, 21)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	t.Logf("References:\n%s", refs)
	if !strings.Contains(refs, "main.c:2") {
		t.Errorf("references should include main.c:2, got:\n%s", refs)
	}

	hover, err := m.Hover(ctx, src, 2, 21)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	t.Logf("Hover:\n%s", hover)

	diags, err := m.Diagnostics(ctx, src)
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	t.Logf("Diagnostics:\n%s", diags)
}

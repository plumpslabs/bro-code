package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree creates a small project tree for tests.
func writeTree(t *testing.T, root string, files []string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildMapDetectsEntryPointsAndSkipsNoise(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{
		"go.mod", "main.go",
		"internal/app/app.go", "internal/app/handler.go",
		"internal/store/db.go", "internal/store/query.go", "internal/store/migrate.go",
		"node_modules/pkg/index.js", // must be skipped
		"dist/bundle.js",            // must be skipped
		"README.md",
	})

	m := BuildMap(root, nil)
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	joined := strings.Join(m.EntryPoints, ",")
	for _, want := range []string{"go.mod", "main.go", "README.md"} {
		if !strings.Contains(joined, want) {
			t.Errorf("entry points missing %s: %s", want, joined)
		}
	}
	tree := strings.Join(m.Tree, "\n")
	if strings.Contains(tree, "node_modules") {
		t.Error("node_modules must be skipped from the tree")
	}
	if strings.Contains(tree, "dist/") {
		t.Error("dist must be skipped from the tree")
	}
	if !strings.Contains(tree, "internal/") {
		t.Error("expected internal/ in tree")
	}
	// Depth-2: internal/app/handler.go renders as app/ under internal/.
	if !strings.Contains(tree, "app/") {
		t.Error("expected app/ (subdir) in tree")
	}
}

func TestBuildMapCacheHit(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"go.mod", "main.go", "internal/app/app.go"})

	m1 := BuildMap(root, nil)
	m2 := BuildMap(root, nil)
	if m1.Hash == "" {
		t.Fatal("expected hash to be set")
	}
	if m1.Hash != m2.Hash {
		t.Errorf("cache must produce same hash, got %s vs %s", m1.Hash, m2.Hash)
	}
	// The cached map file must exist on disk.
	if _, err := os.Stat(mapPath(root)); err != nil {
		t.Errorf("repo-map.json not persisted: %v", err)
	}

	// Changing the file set invalidates the hash.
	writeTree(t, root, []string{"cmd/cli/main.go"})
	m3 := BuildMap(root, nil)
	if m3.Hash == m1.Hash {
		t.Error("hash must change when the file set changes")
	}
}

func TestUsageRecordTopAndPersist(t *testing.T) {
	root := t.TempDir()
	u := NewUsage(root)

	u.Record([]string{"internal/app/handler.go", "internal/app/handler.go", "go.mod"})
	top := u.Top(5)
	if len(top) != 2 || top[0] != "internal/app/handler.go" || top[1] != "go.mod" {
		t.Errorf("expected handler.go then go.mod, got %v", top)
	}

	// Persist then reload from disk.
	u.Save()
	u2 := NewUsage(root)
	top2 := u2.Top(5)
	if len(top2) != 2 || top2[0] != "internal/app/handler.go" {
		t.Errorf("usage not persisted, got %v", top2)
	}

	// Own meta dir never recorded.
	u.Record([]string{".brocode/memory.md"})
	if len(u.Top(10)) != 2 {
		t.Errorf(".brocode files must not be recorded, got %v", u.Top(10))
	}
}

func TestBuildMapHotFilesFromUsage(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"go.mod", "main.go", "internal/app/app.go", "internal/app/handler.go"})

	u := NewUsage(root)
	u.Record([]string{"internal/app/handler.go", "internal/app/handler.go", "internal/app/handler.go", "main.go"})

	m := BuildMap(root, u)
	if len(m.HotFiles) == 0 || m.HotFiles[0] != "internal/app/handler.go" {
		t.Errorf("expected handler.go as hot file, got %v", m.HotFiles)
	}
	if s := m.String(); !strings.Contains(s, "Most-used files") || !strings.Contains(s, "handler.go") {
		t.Errorf("map string must include hot files, got:\n%s", s)
	}
}

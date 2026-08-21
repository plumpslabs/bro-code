package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodeOutlineTool(t *testing.T) {
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "big_service.go")
	content := `package sample

// Service handles main ops.
type Service struct {
	ID string
}

func (s *Service) DoWork() error {
	return nil
}
`
	if err := os.WriteFile(goFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := &CodeOutlineTool{}
	out, err := tool.Execute(context.Background(), `{"path":"`+goFile+`"}`)
	if err != nil {
		t.Fatalf("CodeOutlineTool failed: %v", err)
	}

	if !strings.Contains(out, "Service") || !strings.Contains(out, "DoWork") {
		t.Errorf("expected outline to contain Service and DoWork, got:\n%s", out)
	}
}

func TestRefactorClusterTool(t *testing.T) {
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "big_service.go")
	content := `package sample

func HandleA() {
	HelperA()
}

func HelperA() {}

func HandleB() {
	HelperB()
}

func HelperB() {}
`
	if err := os.WriteFile(goFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := &RefactorClusterTool{}
	out, err := tool.Execute(context.Background(), `{"path":"`+goFile+`"}`)
	if err != nil {
		t.Fatalf("RefactorClusterTool failed: %v", err)
	}

	if !strings.Contains(out, "Cluster 1") || !strings.Contains(out, "Cluster 2") {
		t.Errorf("expected 2 clusters in output, got:\n%s", out)
	}
}

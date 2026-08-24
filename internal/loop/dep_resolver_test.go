package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/search"
)

func TestAutoResolveDependenciesGo(t *testing.T) {
	tmpDir := t.TempDir()
	goMod := filepath.Join(tmpDir, "go.mod")
	_ = os.WriteFile(goMod, []byte("module example.com/test\n\ngo 1.22\n"), 0644)

	ctx := context.Background()
	diag := "cannot find package \"github.com/google/uuid\" in any of:"
	msg, ok := AutoResolveDependencies(ctx, tmpDir, diag)
	// It should attempt resolution (even if offline or no network, it runs the go mod tidy command without crashing)
	t.Logf("Go AutoResolve output: ok=%v, msg=%s", ok, msg)
}

func TestAutoResolveDependenciesNode(t *testing.T) {
	tmpDir := t.TempDir()
	pkgJSON := filepath.Join(tmpDir, "package.json")
	_ = os.WriteFile(pkgJSON, []byte("{\"name\": \"test\"}\n"), 0644)

	ctx := context.Background()
	diag := "Cannot find module 'lodash' or its corresponding type declarations."
	msg, ok := AutoResolveDependencies(ctx, tmpDir, diag)
	t.Logf("Node AutoResolve output: ok=%v, msg=%s", ok, msg)
}

func TestAutoResolveDependenciesPython(t *testing.T) {
	tmpDir := t.TempDir()
	reqTxt := filepath.Join(tmpDir, "requirements.txt")
	_ = os.WriteFile(reqTxt, []byte("requests>=2.0.0\n"), 0644)

	ctx := context.Background()
	diag := "ModuleNotFoundError: No module named 'requests'"
	msg, ok := AutoResolveDependencies(ctx, tmpDir, diag)
	t.Logf("Python AutoResolve output: ok=%v, msg=%s", ok, msg)
}

func TestCheckBlastRadiusImpact(t *testing.T) {
	tmpDir := t.TempDir()
	fileA := filepath.Join(tmpDir, "service.ts")
	fileB := filepath.Join(tmpDir, "controller.ts")

	codeA := "export function calculateTax(amount: number): number {\n\treturn amount * 0.1;\n}\n"
	codeB := "import { calculateTax } from './service';\n\nexport function handle() {\n\tcalculateTax(100);\n}\n"

	_ = os.WriteFile(fileA, []byte(codeA), 0644)
	_ = os.WriteFile(fileB, []byte(codeB), 0644)

	index := search.BuildGlobalIndex(tmpDir)
	ctx := context.Background()

	// Mock diagnostic that reports error on controller.ts
	mockDiag := func(path string) string {
		if strings.HasSuffix(path, "controller.ts") {
			return "Expected 2 arguments, but got 1."
		}
		return "No diagnostics"
	}

	broken := CheckBlastRadiusImpact(ctx, tmpDir, fileA, mockDiag, index)
	if len(broken) == 0 {
		t.Fatal("expected blast radius warning for controller.ts caller")
	}
	if !strings.Contains(broken[0], "controller.ts") || !strings.Contains(broken[0], "service.ts") {
		t.Fatalf("unexpected broken caller message: %s", broken[0])
	}
}

package ui

import (
	"strings"
	"testing"
)

func TestRenderSideBySideDiff(t *testing.T) {
	oldCode := `func Calculate(a, b int) int {
	return a + b
}
`
	newCode := `func Calculate(a, b int) int {
	if a < 0 {
		return 0
	}
	return a + b
}
`
	diff := RenderSideBySideDiff("calculator.go", oldCode, newCode, 100)

	if !strings.Contains(diff, "DIFF: calculator.go") {
		t.Fatalf("expected file header in diff, got: %s", diff)
	}
	if !strings.Contains(diff, "ORIGINAL (BEFORE)") || !strings.Contains(diff, "MODIFIED (AFTER)") {
		t.Fatalf("expected side-by-side columns, got: %s", diff)
	}
	if !strings.Contains(diff, "Calculate") {
		t.Fatalf("expected function code in diff, got: %s", diff)
	}
}

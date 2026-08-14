package context

import (
	"strings"
	"testing"
)

func TestShrinkwrapAST(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("package sample\n\nimport \"fmt\"\n\ntype Service struct{}\n\n")
	for i := 0; i < 200; i++ {
		sb.WriteString("func DoWork" + string(rune('A'+i%26)) + "() {\n")
		sb.WriteString("    fmt.Println(\"Doing work\")\n")
		sb.WriteString("    x := 10 + 20\n")
		sb.WriteString("    y := x * 2\n")
		sb.WriteString("    z := y * 3\n")
		sb.WriteString("    _ = z\n")
		sb.WriteString("}\n\n")
	}

	content := sb.String()
	compressed := ShrinkwrapAST(content, "sample.go")

	if !strings.Contains(compressed, "SHRINKWRAP AST COMPRESSION APPLIED") {
		t.Errorf("expected shrinkwrap header notice, got %s", compressed[:100])
	}
	if !strings.Contains(compressed, "logic omitted for AST token shrinkwrap") {
		t.Errorf("expected omitted logic marker in compressed output")
	}
	if len(compressed) >= len(content) {
		t.Errorf("compressed size (%d) should be significantly smaller than original (%d)", len(compressed), len(content))
	}
}

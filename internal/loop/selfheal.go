package loop

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SelfHealLadder executes deterministic local formatters/fixers on edited files.
// It repairs syntax formatting (e.g. gofmt, prettier, eslint --fix) locally
// before wasting LLM turns on minor syntax or formatting errors.
func SelfHealLadder(ctx context.Context, filePath string) (string, bool) {
	if filePath == "" {
		return "", false
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	switch ext {
	case ".go":
		cmd := exec.CommandContext(ctx, "gofmt", "-w", filePath)
		if err := cmd.Run(); err == nil {
			return "✅ Applied gofmt self-healing format fix", true
		}
	case ".js", ".ts", ".jsx", ".tsx", ".json", ".css", ".html", ".md", ".yaml", ".yml":
		cmd := exec.CommandContext(ctx, "npx", "prettier", "--write", filePath)
		if err := cmd.Run(); err == nil {
			return "✅ Applied prettier self-healing format fix", true
		}
	case ".py":
		cmd := exec.CommandContext(ctx, "black", "-q", filePath)
		if err := cmd.Run(); err == nil {
			return "✅ Applied black self-healing format fix", true
		}
	case ".rs":
		cmd := exec.CommandContext(ctx, "rustfmt", filePath)
		if err := cmd.Run(); err == nil {
			return "✅ Applied rustfmt self-healing format fix", true
		}
	case ".c", ".cpp", ".h", ".hpp":
		cmd := exec.CommandContext(ctx, "clang-format", "-i", filePath)
		if err := cmd.Run(); err == nil {
			return "✅ Applied clang-format self-healing format fix", true
		}
	case ".php":
		cmd := exec.CommandContext(ctx, "php-cs-fixer", "fix", filePath)
		if err := cmd.Run(); err == nil {
			return "✅ Applied php-cs-fixer self-healing format fix", true
		}
	case ".rb":
		cmd := exec.CommandContext(ctx, "rubocop", "-a", filePath)
		if err := cmd.Run(); err == nil {
			return "✅ Applied rubocop self-healing format fix", true
		}
	}

	return "", false
}

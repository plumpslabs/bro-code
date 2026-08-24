package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// AutoResolveDependencies inspects compiler / LSP diagnostic errors and automatically
// runs the project's native package manager to fetch missing modules/dependencies.
// It is language- and package-manager agnostic (Go, Node/pnpm/yarn/bun, Python/uv/poetry/pip, Rust/cargo, PHP/composer).
func AutoResolveDependencies(ctx context.Context, repoRoot string, diagText string) (string, bool) {
	if repoRoot == "" {
		repoRoot = "."
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// 1. Go: "no required module provides package <pkg>" or "cannot find package"
	if strings.Contains(diagText, "no required module provides package") ||
		strings.Contains(diagText, "cannot find module providing package") ||
		(strings.Contains(diagText, "cannot find package") && fileExistsIn(repoRoot, "go.mod")) {
		cmd := tool.SafeCommandContext(ctx, "go", "mod", "tidy")
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err == nil {
			return "✅ Auto-resolved Go dependencies via 'go mod tidy'", true
		} else if len(out) > 0 {
			// If tidy fails, extract the package name and try go get
			re := regexp.MustCompile(`package\s+([a-zA-Z0-9\.\-_/]+)`)
			if m := re.FindStringSubmatch(diagText); len(m) > 1 {
				pkg := m[1]
				getCmd := tool.SafeCommandContext(ctx, "go", "get", pkg)
				getCmd.Dir = repoRoot
				if err := getCmd.Run(); err == nil {
					_ = tool.SafeCommandContext(ctx, "go", "mod", "tidy").Run()
					return "✅ Auto-installed Go package '" + pkg + "' and ran 'go mod tidy'", true
				}
			}
		}
	}

	// 2. Node / TypeScript / JavaScript: "Cannot find module '<pkg>'", "Module not found"
	if strings.Contains(diagText, "Cannot find module") ||
		strings.Contains(diagText, "Module not found") ||
		strings.Contains(diagText, "Could not find a declaration file for module") {
		switch {
		case fileExistsIn(repoRoot, "pnpm-lock.yaml"):
			cmd := tool.SafeCommandContext(ctx, "pnpm", "install")
			cmd.Dir = repoRoot
			if err := cmd.Run(); err == nil {
				return "✅ Auto-installed Node dependencies via 'pnpm install'", true
			}
		case fileExistsIn(repoRoot, "yarn.lock"):
			cmd := tool.SafeCommandContext(ctx, "yarn", "install")
			cmd.Dir = repoRoot
			if err := cmd.Run(); err == nil {
				return "✅ Auto-installed Node dependencies via 'yarn install'", true
			}
		case fileExistsIn(repoRoot, "bun.lockb") || fileExistsIn(repoRoot, "bun.lock"):
			cmd := tool.SafeCommandContext(ctx, "bun", "install")
			cmd.Dir = repoRoot
			if err := cmd.Run(); err == nil {
				return "✅ Auto-installed Node dependencies via 'bun install'", true
			}
		case fileExistsIn(repoRoot, "package.json"):
			cmd := tool.SafeCommandContext(ctx, "npm", "install")
			cmd.Dir = repoRoot
			if err := cmd.Run(); err == nil {
				return "✅ Auto-installed Node dependencies via 'npm install'", true
			}
		}
	}

	// 3. Python: "No module named '<pkg>'", "ModuleNotFoundError"
	if strings.Contains(diagText, "No module named") || strings.Contains(diagText, "ModuleNotFoundError") {
		switch {
		case fileExistsIn(repoRoot, "uv.lock"):
			cmd := tool.SafeCommandContext(ctx, "uv", "sync")
			cmd.Dir = repoRoot
			if err := cmd.Run(); err == nil {
				return "✅ Auto-synced Python dependencies via 'uv sync'", true
			}
		case fileExistsIn(repoRoot, "poetry.lock"):
			cmd := tool.SafeCommandContext(ctx, "poetry", "install")
			cmd.Dir = repoRoot
			if err := cmd.Run(); err == nil {
				return "✅ Auto-installed Python dependencies via 'poetry install'", true
			}
		case fileExistsIn(repoRoot, "requirements.txt"):
			cmd := tool.SafeCommandContext(ctx, "pip", "install", "-r", "requirements.txt")
			cmd.Dir = repoRoot
			if err := cmd.Run(); err == nil {
				return "✅ Auto-installed Python dependencies via 'pip install -r requirements.txt'", true
			}
		}
	}

	// 4. Rust: "can't find crate for '<pkg>'", "unresolved import"
	if strings.Contains(diagText, "can't find crate for") || (strings.Contains(diagText, "unresolved import") && fileExistsIn(repoRoot, "Cargo.toml")) {
		cmd := tool.SafeCommandContext(ctx, "cargo", "check", "--quiet")
		cmd.Dir = repoRoot
		if err := cmd.Run(); err == nil {
			return "✅ Auto-checked Rust crate dependencies via 'cargo check'", true
		}
	}

	// 5. PHP: composer.json present
	if strings.Contains(diagText, "Class") && strings.Contains(diagText, "not found") && fileExistsIn(repoRoot, "composer.json") {
		cmd := tool.SafeCommandContext(ctx, "composer", "dump-autoload")
		cmd.Dir = repoRoot
		if err := cmd.Run(); err == nil {
			return "✅ Auto-dumped PHP autoload via 'composer dump-autoload'", true
		}
	}

	return "", false
}

// CheckBlastRadiusImpact inspects whether modifying an exported symbol in modifiedFile broke
// any downstream caller files across the repository.
func CheckBlastRadiusImpact(ctx context.Context, repoRoot string, modifiedFile string, diagFn func(string) string, index *search.GlobalIndex) []string {
	if modifiedFile == "" || diagFn == nil || index == nil {
		return nil
	}

	importers := index.Importers(modifiedFile)
	if len(importers) == 0 {
		return nil
	}

	var brokenCallers []string
	checkedFiles := make(map[string]bool)
	checkedFiles[modifiedFile] = true

	for _, impFile := range importers {
		if checkedFiles[impFile] {
			continue
		}
		checkedFiles[impFile] = true
		if diag := diagFn(impFile); diag != "" && !strings.HasPrefix(diag, "No diagnostics") {
			brokenCallers = append(brokenCallers, fmt.Sprintf("%s (importer of %s): %s", filepath.Base(impFile), filepath.Base(modifiedFile), strings.TrimSpace(diag)))
		}
	}
	return brokenCallers
}

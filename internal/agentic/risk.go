package agentic

import (
	"path/filepath"
	"strings"
)

// RiskLevel categorizes the danger of modifying a file.
type RiskLevel int

const (
	L0_Trivial  RiskLevel = iota // Docs, formatting
	L1_Normal                    // Standard business logic
	L2_High                      // Core models, config, routing
	L3_Critical                  // Auth, cryptography, raw SQL, infrastructure
)

// EvaluateFileRisk determines the risk level of modifying a given file.
func EvaluateFileRisk(filePath string) RiskLevel {
	base := strings.ToLower(filepath.Base(filePath))
	ext := filepath.Ext(base)

	if ext == ".md" || ext == ".txt" || ext == ".rst" || base == ".gitignore" || base == ".prettierrc" || base == ".eslintrc" {
		return L0_Trivial
	}

	criticalMatches := []string{
		"auth", "crypto", "security", "login", "password", "token", "secret",
		"sql", "db", "database", "migration", "dockerfile", "docker-compose",
		"k8s", "kubernetes", "credentials", "private_key",
	}
	for _, match := range criticalMatches {
		if strings.Contains(base, match) {
			return L3_Critical
		}
	}

	highMatches := []string{
		"config", "router", "routes", "settings",
		"main.", "index.", "app.", "server.", "agent.", "entry.",
		"package.json", "cargo.toml", "pyproject.toml", "go.mod", "makefile", "pom.xml", "build.gradle", "cmakelists.txt",
	}
	for _, match := range highMatches {
		if strings.Contains(base, match) {
			return L2_High
		}
	}

	return L1_Normal
}

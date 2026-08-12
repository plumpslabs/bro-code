package agentic

import (
	"path/filepath"
	"strings"
)

// RiskLevel categorizes the danger of modifying a file.
type RiskLevel int

const (
	L0_Trivial RiskLevel = iota // Docs, formatting
	L1_Normal                   // Standard business logic
	L2_High                     // Core models, config, routing
	L3_Critical                 // Auth, cryptography, raw SQL, infrastructure
)

// EvaluateFileRisk determines the risk level of modifying a given file.
func EvaluateFileRisk(filePath string) RiskLevel {
	base := strings.ToLower(filepath.Base(filePath))
	ext := filepath.Ext(base)

	if ext == ".md" || ext == ".txt" || base == ".gitignore" {
		return L0_Trivial
	}

	criticalMatches := []string{"auth", "crypto", "security", "login", "password", "token", "sql", "db", "dockerfile"}
	for _, match := range criticalMatches {
		if strings.Contains(base, match) {
			return L3_Critical
		}
	}

	highMatches := []string{"config", "router", "main.go", "app.go", "agent.go"}
	for _, match := range highMatches {
		if strings.Contains(base, match) {
			return L2_High
		}
	}

	return L1_Normal
}

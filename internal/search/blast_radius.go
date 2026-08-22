package search

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// BlastRadiusReport summarizes the ripple impact of changing a symbol or file.
type BlastRadiusReport struct {
	Target       string   `json:"target"`
	IsFile       bool     `json:"is_file"`
	Risk         string   `json:"risk"` // LOW, MEDIUM, HIGH, CRITICAL
	CallersCount int      `json:"callers_count"`
	Callers      []string `json:"callers,omitempty"`
	Importers    []string `json:"importers,omitempty"`
	Definitions  []string `json:"definitions,omitempty"`
}

// Format renders a clean, developer-friendly blast radius card.
func (r *BlastRadiusReport) Format() string {
	if r == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Blast Radius Analysis for %q [Risk: %s, Callers/Refs: %d]:\n", r.Target, r.Risk, r.CallersCount))
	if len(r.Definitions) > 0 {
		sb.WriteString("  Definitions:\n")
		for _, d := range r.Definitions {
			sb.WriteString(fmt.Sprintf("    • %s\n", d))
		}
	}
	if len(r.Callers) > 0 {
		sb.WriteString(fmt.Sprintf("  Referencing Files (%d):\n", len(r.Callers)))
		for _, c := range r.Callers {
			sb.WriteString(fmt.Sprintf("    ↳ %s\n", c))
		}
	}
	if len(r.Importers) > 0 {
		sb.WriteString(fmt.Sprintf("  Direct Importers (%d):\n", len(r.Importers)))
		for _, im := range r.Importers {
			sb.WriteString(fmt.Sprintf("    ↳ %s\n", im))
		}
	}
	switch r.Risk {
	case "CRITICAL":
		sb.WriteString("\n⚠️ CRITICAL BLAST RADIUS: Widely-used core symbol. Maintain backward compatibility or update all callers in a single atomic pass.")
	case "HIGH":
		sb.WriteString("\n⚠️ HIGH BLAST RADIUS: Multiple external callers. Ensure all referenced files are updated.")
	case "MEDIUM":
		sb.WriteString("\n💡 Moderate impact: Update the local callers listed above.")
	case "LOW":
		sb.WriteString("\n✅ Low impact: Isolated local symbol with minimal callers.")
	}
	return sb.String()
}

// BlastRadius computes the blast radius of modifying a given symbol or file path.
func (g *GlobalIndex) BlastRadius(target string) *BlastRadiusReport {
	if g == nil {
		return &BlastRadiusReport{Target: target, Risk: "LOW"}
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return &BlastRadiusReport{Risk: "LOW"}
	}

	report := &BlastRadiusReport{
		Target: target,
		IsFile: strings.Contains(target, "/") || strings.Contains(target, "\\") || filepath.Ext(target) != "",
	}

	if report.IsFile {
		// Target is a file path
		cleanPath := filepath.ToSlash(target)
		importers := g.Importers(cleanPath)
		report.Importers = importers
		report.CallersCount = len(importers)
	} else {
		// Target is a symbol name (function, class, struct, method)
		defs := g.Lookup(target)
		for _, d := range defs {
			report.Definitions = append(report.Definitions, fmt.Sprintf("%s:%d [%s]", filepath.ToSlash(d.File), d.Line, d.Kind))
		}
		refs := g.Referencers(target)
		report.Callers = refs

		var importers []string
		for _, d := range defs {
			for _, im := range g.Importers(d.File) {
				importers = append(importers, im)
			}
		}
		report.Importers = uniqueSorted(importers)

		// Combined unique caller/referencer count
		uniqueFiles := map[string]bool{}
		for _, r := range refs {
			uniqueFiles[r] = true
		}
		for _, im := range report.Importers {
			uniqueFiles[im] = true
		}
		report.CallersCount = len(uniqueFiles)
	}

	// Calculate risk level
	if report.CallersCount >= 10 {
		report.Risk = "CRITICAL"
	} else if report.CallersCount >= 5 {
		report.Risk = "HIGH"
	} else if report.CallersCount >= 2 {
		report.Risk = "MEDIUM"
	} else {
		report.Risk = "LOW"
	}

	sort.Strings(report.Callers)
	sort.Strings(report.Importers)
	sort.Strings(report.Definitions)

	return report
}

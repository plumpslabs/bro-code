package search

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// docFileNames are the instruction/context files BroCode reads at the start of
// a project. BroCode follows the public AGENTS.md standard (the tool-agnostic
// convention shared by all agents): AGENTS.md at the root, plus its own
// project-level .brocode/AGENTS.md. Provider-specific instruction files
// (CLAUDE.md, GEMINI.md, .cursorrules, ...) are deliberately NOT read — they
// belong to other tools and would leak their branding into the agent.
var docFileNames = []string{
	"AGENTS.md",
	".brocode/AGENTS.md",
	"README.md",
}

// ProjectContext is a compact structural overview of the project plus any
// instruction docs, injected into the system prompt so the agent starts with
// orientation instead of blind grep/glob exploration.
type ProjectContext struct {
	Root       string
	Tree       string // 2-level directory tree (names only)
	Docs       string // concatenated contents of AGENTS/CLAUDE/README (capped)
	EntryFiles []string
}

// BuildProjectContext scans the working directory and produces the context.
// Shallow and fast: tree listing uses ReadDir one level at a time (no deep
// walk), docs are read with a size cap.
func BuildProjectContext(root string) *ProjectContext {
	pc := &ProjectContext{Root: root}

	var tree strings.Builder
	tree.WriteString("PROJECT STRUCTURE:\n")

	// Level 1: top-level entries.
	top, _ := os.ReadDir(root)
	var topDirs, topFiles []string
	for _, e := range top {
		name := e.Name()
		if strings.HasPrefix(name, ".") && name != ".github" {
			continue
		}
		switch name {
		case "node_modules", "vendor", "dist", "build", "target", ".next", "Pods", ".gradle":
			continue
		}
		if e.IsDir() {
			topDirs = append(topDirs, name)
		} else {
			topFiles = append(topFiles, name)
		}
	}
	sort.Strings(topDirs)
	sort.Strings(topFiles)

	for _, d := range topDirs {
		tree.WriteString("  " + d + "/\n")
		// Level 2 (only for dirs with a few children, cap output).
		sub, _ := os.ReadDir(filepath.Join(root, d))
		var subs []string
		for _, se := range sub {
			if strings.HasPrefix(se.Name(), ".") {
				continue
			}
			if se.IsDir() {
				subs = append(subs, se.Name()+"/")
			}
		}
		sort.Strings(subs)
		if len(subs) > 12 {
			subs = subs[:12]
		}
		for _, s := range subs {
			tree.WriteString("    " + s + "\n")
		}
	}
	for _, f := range topFiles {
		tree.WriteString("  " + f + "\n")
	}
	pc.Tree = strings.TrimSpace(tree.String())

	// Entry files: common starting points.
	for _, f := range []string{"package.json", "go.mod", "Cargo.toml", "pyproject.toml", "docker-compose.yml", "Makefile", "tsconfig.json"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			pc.EntryFiles = append(pc.EntryFiles, f)
		}
	}

	// Instruction docs: global BroCode instructions load first as the base
	// layer, project docs load after so the repo wins on conflict (same
	// layering as git config: project overrides global).
	var docs []string
	if home, err := os.UserHomeDir(); err == nil {
		if data, rerr := os.ReadFile(filepath.Join(home, ".config", "brocode", "AGENTS.md")); rerr == nil {
			content := string(data)
			if len(content) > 12000 {
				content = content[:12000] + "\n… (truncated)"
			}
			docs = append(docs, "=== ~/.config/brocode/AGENTS.md (global) ===\n"+content)
		}
	}
	for _, name := range docFileNames {
		p := filepath.Join(root, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		content := string(data)
		if len(content) > 12000 {
			content = content[:12000] + "\n… (truncated)"
		}
		docs = append(docs, "=== "+name+" ===\n"+content)
	}
	pc.Docs = strings.Join(docs, "\n\n")

	return pc
}

// String renders the project context for injection into the system prompt.
// Kept compact: tree + entry files, docs only if present.
func (pc *ProjectContext) String() string {
	var sb strings.Builder
	sb.WriteString(pc.Tree + "\n")
	if len(pc.EntryFiles) > 0 {
		sb.WriteString("KEY FILES: " + strings.Join(pc.EntryFiles, ", ") + "\n")
	}
	if pc.Docs != "" {
		sb.WriteString("\n" + pc.Docs)
	}
	return strings.TrimSpace(sb.String())
}

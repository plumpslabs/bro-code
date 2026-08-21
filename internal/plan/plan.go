package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// PlanStep represents a single task unit within a structured plan.
type PlanStep struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Status      string   `json:"status"` // "pending" | "in_progress" | "done" | "blocked" | "skipped"
	Files       []string `json:"files,omitempty"`
}

// Plan represents an explicit execution plan for complex multi-file tasks.
type Plan struct {
	Goal      string     `json:"goal"`
	Status    string     `json:"status"` // "ACTIVE" | "IN_PROGRESS" | "COMPLETED"
	CreatedAt time.Time  `json:"created_at"`
	Steps     []PlanStep `json:"steps"`
	Files     []string   `json:"files,omitempty"`
}

func (p *Plan) ToJSON() string {
	b, _ := json.MarshalIndent(p, "", "  ")
	return string(b)
}

func FromJSON(data string) (*Plan, error) {
	var p Plan
	err := json.Unmarshal([]byte(data), &p)
	return &p, err
}

// CurrentPlanPath returns the file path for the live active plan.
func CurrentPlanPath(workspaceDir string) string {
	return filepath.Join(workspaceDir, ".brocode", "current_plan.md")
}

// ArchiveDirPath returns the directory path for archived completed plans.
func ArchiveDirPath(workspaceDir string) string {
	return filepath.Join(workspaceDir, ".brocode", "plans", "archive")
}

// LoadCurrentPlan reads and parses .brocode/current_plan.md if it exists.
func LoadCurrentPlan(workspaceDir string) (*Plan, error) {
	path := CurrentPlanPath(workspaceDir)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseMarkdownPlan(string(data)), nil
}

// SaveCurrentPlan writes the active plan into .brocode/current_plan.md.
// If an existing plan with a different goal was active, it safely archives
// the previous plan into .brocode/plans/archive/ first so past tasks are never lost.
func SaveCurrentPlan(workspaceDir string, p *Plan) error {
	if p == nil {
		return nil
	}
	if existing, err := LoadCurrentPlan(workspaceDir); err == nil && existing != nil && len(existing.Steps) > 0 {
		if sanitizeSlug(existing.Goal) != sanitizeSlug(p.Goal) {
			_, _ = ArchiveCurrentPlan(workspaceDir)
		}
	}
	path := CurrentPlanPath(workspaceDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(RenderMarkdownPlan(p)), 0o644)
}

// maxArchivePlans is the maximum number of completed plans kept in the archive directory.
// Older plans beyond this limit are automatically pruned to prevent repo bloat.
const maxArchivePlans = 5

// ArchiveCurrentPlan moves a completed current_plan.md to .brocode/plans/archive/
// and cleans up current_plan.md so no stale tasks linger.
func ArchiveCurrentPlan(workspaceDir string) (string, error) {
	p, err := LoadCurrentPlan(workspaceDir)
	if err != nil {
		return "", err
	}
	p.Status = "COMPLETED"
	for i := range p.Steps {
		p.Steps[i].Status = "done"
	}
	archiveDir := ArchiveDirPath(workspaceDir)
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return "", err
	}
	slug := sanitizeSlug(p.Goal)
	if slug == "" {
		slug = "task_plan"
	}
	timestamp := time.Now().Format("2006-01-02_150405")
	archiveName := fmt.Sprintf("%s_%s.md", timestamp, slug)
	archivePath := filepath.Join(archiveDir, archiveName)

	if err := os.WriteFile(archivePath, []byte(RenderMarkdownPlan(p)), 0o644); err != nil {
		return "", err
	}
	_ = os.Remove(CurrentPlanPath(workspaceDir))
	_ = pruneOldArchives(archiveDir, maxArchivePlans)
	return archivePath, nil
}

func pruneOldArchives(archiveDir string, maxCount int) error {
	entries, err := os.ReadDir(archiveDir)
	if err != nil || len(entries) <= maxCount {
		return err
	}
	var mdFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			mdFiles = append(mdFiles, e.Name())
		}
	}
	if len(mdFiles) <= maxCount {
		return nil
	}
	// Sort ascending (oldest first because timestamp prefix is YYYY-MM-DD_HHMMSS)
	sort.Strings(mdFiles)
	toDelete := len(mdFiles) - maxCount
	for i := 0; i < toDelete; i++ {
		_ = os.Remove(filepath.Join(archiveDir, mdFiles[i]))
	}
	return nil
}

// IsAllStepsDone checks if all steps in the plan have been marked done.
func (p *Plan) IsAllStepsDone() bool {
	if p == nil || len(p.Steps) == 0 {
		return false
	}
	for _, s := range p.Steps {
		if s.Status != "done" {
			return false
		}
	}
	return true
}

// MarkStepsDoneByEditedFiles matches edited file paths against step descriptions,
// marking matching steps as done and saving the updated plan.
func MarkStepsDoneByEditedFiles(workspaceDir string, editedFiles []string) (*Plan, error) {
	p, err := LoadCurrentPlan(workspaceDir)
	if err != nil || p == nil || len(p.Steps) == 0 {
		return nil, err
	}
	changed := false
	for i := range p.Steps {
		if p.Steps[i].Status == "done" {
			continue
		}
		for _, ef := range editedFiles {
			base := filepath.Base(ef)
			desc := p.Steps[i].Description
			if strings.Contains(desc, ef) || strings.Contains(desc, base) {
				p.Steps[i].Status = "done"
				changed = true
				break
			}
		}
	}
	if changed {
		_ = SaveCurrentPlan(workspaceDir, p)
	}
	return p, nil
}

// AutoArchiveIfDone archives the plan if all steps have been marked done.
func AutoArchiveIfDone(workspaceDir string) (bool, string, error) {
	p, err := LoadCurrentPlan(workspaceDir)
	if err != nil || p == nil || len(p.Steps) == 0 {
		return false, "", err
	}
	if p.IsAllStepsDone() {
		archPath, err := ArchiveCurrentPlan(workspaceDir)
		return true, archPath, err
	}
	return false, "", nil
}

// ParseMarkdownPlan extracts goal, status, checklist tasks, and impacted files
// using universal Markdown structural grammar (GFM task lists, ordered lists, numbered headings).
func ParseMarkdownPlan(md string) *Plan {
	p := &Plan{
		Status:    "ACTIVE",
		CreatedAt: time.Now(),
	}

	lines := strings.Split(md, "\n")
	stepIdx := 1

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// First markdown heading is taken as the Goal / Title
		if p.Goal == "" && strings.HasPrefix(trimmed, "#") {
			goal := strings.TrimLeft(trimmed, "# \t")
			goal = strings.TrimPrefix(goal, "🎯 ")
			goal = strings.TrimPrefix(goal, "Plan: ")
			p.Goal = strings.TrimSpace(goal)
			continue
		}

		if strings.HasPrefix(trimmed, "**Status:**") {
			status := strings.TrimPrefix(trimmed, "**Status:**")
			p.Status = strings.TrimSpace(status)
			continue
		}

		// GFM checklist task detection: "- [ ]", "* [ ]", "+ [ ]", "- [x]", etc.
		if isChecklistDone(trimmed) {
			desc := cleanTaskDesc(trimmed[5:])
			p.Steps = append(p.Steps, PlanStep{
				ID:          fmt.Sprintf("step_%d", stepIdx),
				Description: desc,
				Status:      "done",
			})
			stepIdx++
			continue
		} else if isChecklistPending(trimmed) {
			desc := cleanTaskDesc(trimmed[5:])
			status := "pending"
			if strings.Contains(trimmed, "✅") || strings.Contains(trimmed, "✔️") {
				status = "done"
			}
			p.Steps = append(p.Steps, PlanStep{
				ID:          fmt.Sprintf("step_%d", stepIdx),
				Description: desc,
				Status:      status,
			})
			stepIdx++
			continue
		}

		// Structural numbered step detection: "### 1. Title", "1. Title", "2) Title"
		if desc, ok := extractStructuralStep(trimmed); ok {
			status := "pending"
			if strings.Contains(trimmed, "✅") || strings.Contains(trimmed, "✔️") {
				status = "done"
			}
			p.Steps = append(p.Steps, PlanStep{
				ID:          fmt.Sprintf("step_%d", stepIdx),
				Description: cleanTaskDesc(desc),
				Status:      status,
			})
			stepIdx++
			continue
		}

		// Structural file path detection in bullet points or backticks
		if (strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ")) && !strings.HasPrefix(trimmed, "- [") {
			clean := strings.TrimLeft(trimmed, "-* \t`")
			clean = strings.TrimRight(clean, "` \t")
			if isLikelyFilePath(clean) {
				p.Files = append(p.Files, clean)
			}
		}
	}

	if p.Goal == "" && len(p.Steps) > 0 {
		p.Goal = p.Steps[0].Description
	}
	return p
}

func isChecklistDone(s string) bool {
	return strings.HasPrefix(s, "- [x]") || strings.HasPrefix(s, "* [x]") || strings.HasPrefix(s, "+ [x]") ||
		strings.HasPrefix(s, "- [X]") || strings.HasPrefix(s, "* [X]") || strings.HasPrefix(s, "+ [X]")
}

func isChecklistPending(s string) bool {
	return strings.HasPrefix(s, "- [ ]") || strings.HasPrefix(s, "* [ ]") || strings.HasPrefix(s, "+ [ ]")
}

// extractStructuralStep parses numbered headings ("### 1. Title") and numbered list items ("1. Title", "1) Title")
func extractStructuralStep(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	// 1. Heading followed by number: "### 1. Title" or "#### 1. Title"
	if strings.HasPrefix(trimmed, "#") {
		h := strings.TrimLeft(trimmed, "# \t")
		if len(h) >= 2 && h[0] >= '1' && h[0] <= '9' {
			for i := 1; i < len(h); i++ {
				if h[i] == '.' || h[i] == ')' || h[i] == ':' || h[i] == ' ' {
					return strings.TrimSpace(h[i+1:]), true
				}
				if h[i] < '0' || h[i] > '9' {
					break
				}
			}
		}
		return "", false
	}
	// 2. Numbered list items: "1. Title", "2) Title", "1: Title"
	clean := strings.TrimLeft(trimmed, "*-•+ \t")
	if len(clean) >= 2 && clean[0] >= '1' && clean[0] <= '9' {
		for i := 1; i < len(clean); i++ {
			if clean[i] == '.' || clean[i] == ')' || clean[i] == ':' {
				if i+1 < len(clean) && (clean[i+1] == ' ' || clean[i+1] == '\t') {
					return strings.TrimSpace(clean[i+2:]), true
				}
			}
			if clean[i] < '0' || clean[i] > '9' {
				break
			}
		}
	}
	return "", false
}

func isLikelyFilePath(s string) bool {
	if strings.Contains(s, " ") || len(s) < 3 {
		return false
	}
	if strings.Contains(s, "/") || strings.Contains(s, "\\") {
		return true
	}
	ext := filepath.Ext(s)
	return ext != "" && len(ext) <= 6
}

// RenderMarkdownPlan formats a Plan into standard GitHub-flavored Markdown.
func RenderMarkdownPlan(p *Plan) string {
	var sb strings.Builder
	goal := p.Goal
	if goal == "" {
		goal = "Active Task Plan"
	}
	sb.WriteString("# 🎯 Plan: " + goal + "\n\n")
	status := p.Status
	if status == "" {
		status = "ACTIVE"
	}
	sb.WriteString("**Status:** " + status + "\n")
	if !p.CreatedAt.IsZero() {
		sb.WriteString("**Created:** " + p.CreatedAt.Format("2006-01-02 15:04:05") + "\n")
	}
	sb.WriteString("\n## 📋 Tasks\n")
	for _, step := range p.Steps {
		marker := "[ ]"
		if step.Status == "done" {
			marker = "[x]"
		}
		sb.WriteString("- " + marker + " " + cleanTaskDesc(step.Description) + "\n")
	}
	if len(p.Files) > 0 {
		sb.WriteString("\n## 📁 Impacted Files\n")
		for _, f := range p.Files {
			sb.WriteString("- " + f + "\n")
		}
	}
	return sb.String()
}

func cleanTaskDesc(desc string) string {
	s := strings.TrimSpace(desc)
	s = strings.TrimSuffix(s, "✅")
	s = strings.TrimSuffix(s, "✔️")
	s = strings.TrimSuffix(s, "(done)")
	s = strings.TrimSuffix(s, "(completed)")
	return strings.TrimSpace(s)
}

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func sanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonAlphanumeric.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

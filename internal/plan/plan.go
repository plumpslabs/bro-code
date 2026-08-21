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

// ParseMarkdownPlan extracts goal, status, checklist tasks, and impacted files.
func ParseMarkdownPlan(md string) *Plan {
	p := &Plan{
		Status:    "ACTIVE",
		CreatedAt: time.Now(),
	}

	lines := strings.Split(md, "\n")
	stepIdx := 1
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			goal := strings.TrimPrefix(trimmed, "# ")
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
		// Checklist task detection
		if strings.HasPrefix(trimmed, "- [x]") || strings.HasPrefix(trimmed, "* [x]") || strings.HasPrefix(trimmed, "- [X]") || strings.HasPrefix(trimmed, "* [X]") {
			desc := cleanTaskDesc(trimmed[5:])
			p.Steps = append(p.Steps, PlanStep{
				ID:          fmt.Sprintf("step_%d", stepIdx),
				Description: desc,
				Status:      "done",
			})
			stepIdx++
		} else if strings.HasPrefix(trimmed, "- [ ]") || strings.HasPrefix(trimmed, "* [ ]") {
			desc := cleanTaskDesc(trimmed[5:])
			status := "pending"
			if strings.Contains(trimmed, "✅") || strings.Contains(trimmed, "✔️") || strings.Contains(trimmed, "[x]") {
				status = "done"
			}
			p.Steps = append(p.Steps, PlanStep{
				ID:          fmt.Sprintf("step_%d", stepIdx),
				Description: desc,
				Status:      status,
			})
			stepIdx++
		} else if isStepHeader(trimmed) {
			desc := cleanTaskDesc(cleanStepDesc(trimmed))
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
		}
	}
	if p.Goal == "" && len(p.Steps) > 0 {
		p.Goal = p.Steps[0].Description
	}
	return p
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

func isStepHeader(line string) bool {
	l := strings.ToLower(strings.TrimSpace(line))
	clean := strings.TrimLeft(l, "#*-• \t")
	if strings.HasPrefix(clean, "step ") || strings.HasPrefix(clean, "langkah ") ||
		strings.HasPrefix(clean, "phase ") || strings.HasPrefix(clean, "tahap ") {
		return true
	}
	return false
}

func cleanStepDesc(line string) string {
	s := strings.TrimLeft(line, "#*-• \t")
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

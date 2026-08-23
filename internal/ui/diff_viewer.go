package ui

import (
	"fmt"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
	"github.com/plumpslabs/bro-code/internal/tool"
)

var (
	diffFileHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Padding(0, 1)
	diffHunkHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	diffLeftHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")).Padding(0, 1)
	diffRightHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("46")).Padding(0, 1)
	diffDeletedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	diffAddedStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("46"))
	diffContextStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
)

// RenderSideBySideDiff renders a two-column (Before vs After) visual diff for a file change.
func RenderSideBySideDiff(path, oldContent, newContent string, termWidth int) string {
	if termWidth <= 40 {
		termWidth = 80
	}

	colWidth := (termWidth - 7) / 2
	if colWidth < 20 {
		colWidth = 35
	}

	edits := myers.ComputeEdits(span.URIFromPath(path), oldContent, newContent)
	if len(edits) == 0 {
		return fmt.Sprintf("No changes detected in %s", path)
	}

	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	var sb strings.Builder
	sb.WriteString(diffFileHeaderStyle.Render(fmt.Sprintf("📄 DIFF: %s", path)))
	sb.WriteString("\n")

	// Header row
	leftTitle := diffLeftHeaderStyle.Render(fitString("ORIGINAL (BEFORE)", colWidth))
	rightTitle := diffRightHeaderStyle.Render(fitString("MODIFIED (AFTER)", colWidth))
	sb.WriteString(fmt.Sprintf(" %s │ %s\n", leftTitle, rightTitle))
	sb.WriteString(strings.Repeat("─", colWidth+2) + "┼" + strings.Repeat("─", colWidth+2) + "\n")

	// Render aligned lines
	maxL := max(len(oldLines), len(newLines))
	if maxL > 300 {
		maxL = 300 // Cap visual diff view
	}

	oldIdx := 0
	newIdx := 0

	for oldIdx < len(oldLines) || newIdx < len(newLines) {
		if oldIdx >= 250 || newIdx >= 250 {
			sb.WriteString(diffHunkHeaderStyle.Render("… (diff truncated for display, full changes applied)") + "\n")
			break
		}

		oldText := ""
		newText := ""
		oldFmt := ""
		newFmt := ""

		if oldIdx < len(oldLines) && newIdx < len(newLines) {
			if oldLines[oldIdx] == newLines[newIdx] {
				// Identical context line
				oldText = fmt.Sprintf("%4d  %s", oldIdx+1, oldLines[oldIdx])
				newText = fmt.Sprintf("%4d  %s", newIdx+1, newLines[newIdx])
				oldFmt = diffContextStyle.Render(fitString(oldText, colWidth))
				newFmt = diffContextStyle.Render(fitString(newText, colWidth))
				oldIdx++
				newIdx++
			} else {
				// Changed lines
				oldText = fmt.Sprintf("%4d -%s", oldIdx+1, oldLines[oldIdx])
				newText = fmt.Sprintf("%4d +%s", newIdx+1, newLines[newIdx])
				oldFmt = diffDeletedStyle.Render(fitString(oldText, colWidth))
				newFmt = diffAddedStyle.Render(fitString(newText, colWidth))
				oldIdx++
				newIdx++
			}
		} else if oldIdx < len(oldLines) {
			// Deleted line only
			oldText = fmt.Sprintf("%4d -%s", oldIdx+1, oldLines[oldIdx])
			oldFmt = diffDeletedStyle.Render(fitString(oldText, colWidth))
			newFmt = strings.Repeat(" ", colWidth)
			oldIdx++
		} else if newIdx < len(newLines) {
			// Added line only
			oldFmt = strings.Repeat(" ", colWidth)
			newText = fmt.Sprintf("%4d +%s", newIdx+1, newLines[newIdx])
			newFmt = diffAddedStyle.Render(fitString(newText, colWidth))
			newIdx++
		}

		sb.WriteString(fmt.Sprintf(" %s │ %s\n", oldFmt, newFmt))
	}

	return sb.String()
}

func fitString(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(s) > width {
		if width > 1 {
			return s[:width-1] + "…"
		}
		return s[:1]
	}
	return s + strings.Repeat(" ", width-len(s))
}

// GenerateSessionDiffSummary collects and renders side-by-side diffs for all recent changes.
func GenerateSessionDiffSummary(targetPath string, termWidth int) string {
	changes := tool.PeekChanges()
	if len(changes) == 0 {
		return "⚠️ No file changes recorded in the active session.\nUse `/diff <path>` to view diff against disk or git status."
	}

	var sb strings.Builder
	rendered := 0

	for _, ch := range changes {
		if targetPath != "" && !strings.Contains(ch.Path, targetPath) {
			continue
		}
		if ch.Action == "deleted" {
			sb.WriteString(diffDeletedStyle.Render(fmt.Sprintf("🗑️ DELETED: %s\n", ch.Path)))
			rendered++
			continue
		}
		if ch.Old == "" && ch.New != "" {
			sb.WriteString(diffAddedStyle.Render(fmt.Sprintf("✨ CREATED: %s (%d lines)\n", ch.Path, len(strings.Split(ch.New, "\n")))))
			rendered++
			continue
		}
		sb.WriteString(RenderSideBySideDiff(ch.Path, ch.Old, ch.New, termWidth))
		sb.WriteString("\n\n")
		rendered++
	}

	if rendered == 0 && targetPath != "" {
		// Try reading disk file diff vs snapshot
		data, err := os.ReadFile(targetPath)
		if err == nil {
			return RenderSideBySideDiff(targetPath, "", string(data), termWidth)
		}
		return fmt.Sprintf("No changes recorded for %q", targetPath)
	}

	return strings.TrimSpace(sb.String())
}

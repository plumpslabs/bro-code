package repo

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// RepoInfo describes a single repository inside a workspace.
type RepoInfo struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsGit bool   `json:"is_git"`
}

// Workspace describes a multi-repo or single-repo project workspace.
type Workspace struct {
	RootPath string     `json:"root_path"`
	Repos    []RepoInfo `json:"repos"`
}

// DiscoverWorkspace scans rootPath for git repositories.
// If rootPath itself is a git repo, it's included as the primary repo.
// Additionally, immediate subdirectories that contain .git are discovered.
// If .brocode/workspace.json exists, its configured repos are loaded.
func DiscoverWorkspace(rootPath string) *Workspace {
	ws := &Workspace{
		RootPath: rootPath,
		Repos:    []RepoInfo{},
	}

	// 1. Check explicit .brocode/workspace.json if present
	cfgPath := filepath.Join(rootPath, ".brocode", "workspace.json")
	if data, err := os.ReadFile(cfgPath); err == nil {
		var custom struct {
			Repos []string `json:"repos"`
		}
		if json.Unmarshal(data, &custom) == nil && len(custom.Repos) > 0 {
			for _, r := range custom.Repos {
				abs := r
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(rootPath, r)
				}
				if isGitRepo(abs) {
					name := filepath.Base(abs)
					ws.Repos = append(ws.Repos, RepoInfo{Name: name, Path: abs, IsGit: true})
				}
			}
			if len(ws.Repos) > 0 {
				return ws
			}
		}
	}

	// 2. Check if rootPath itself is a git repo
	if isGitRepo(rootPath) {
		name := filepath.Base(rootPath)
		ws.Repos = append(ws.Repos, RepoInfo{
			Name:  name,
			Path:  rootPath,
			IsGit: true,
		})
	}

	// 3. Scan immediate child directories for sibling git repos
	entries, err := os.ReadDir(rootPath)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") || skipDirs[name] {
				continue
			}
			childPath := filepath.Join(rootPath, name)
			if isGitRepo(childPath) {
				// Don't add if already added (e.g. if root == childPath)
				alreadyExists := false
				for _, r := range ws.Repos {
					if r.Path == childPath {
						alreadyExists = true
						break
					}
				}
				if !alreadyExists {
					ws.Repos = append(ws.Repos, RepoInfo{
						Name:  name,
						Path:  childPath,
						IsGit: true,
					})
				}
			}
		}
	}

	// If no git repo found, treat rootPath as a non-git project repository
	if len(ws.Repos) == 0 {
		ws.Repos = append(ws.Repos, RepoInfo{
			Name:  filepath.Base(rootPath),
			Path:  rootPath,
			IsGit: false,
		})
	}

	sort.Slice(ws.Repos, func(i, j int) bool {
		return ws.Repos[i].Name < ws.Repos[j].Name
	})

	return ws
}

// FindRepoForPath returns the matching repository for a given file path.
func (w *Workspace) FindRepoForPath(filePath string) *RepoInfo {
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(w.RootPath, filePath)
	}
	clean := filepath.Clean(filePath)

	var bestMatch *RepoInfo
	bestLen := -1

	for i := range w.Repos {
		repoPath := filepath.Clean(w.Repos[i].Path)
		if clean == repoPath || strings.HasPrefix(clean, repoPath+string(filepath.Separator)) {
			if len(repoPath) > bestLen {
				bestLen = len(repoPath)
				bestMatch = &w.Repos[i]
			}
		}
	}
	return bestMatch
}

// isGitRepo checks whether path is a valid Git working directory.
func isGitRepo(path string) bool {
	gitDir := filepath.Join(path, ".git")
	if info, err := os.Stat(gitDir); err == nil && (info.IsDir() || !info.IsDir()) {
		return true
	}
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = path
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// Package repo implements BroCode's deterministic project intelligence layer:
// a repo map (entry points, structure, hot files) built without the LLM, and
// cross-session usage tracking ("the more BroCode is used, the smarter it
// gets"). Both persist under .brocode/ and are injected into the system
// prompt so every session starts warm without spending tokens on re-discovery.
//
//   - RepoMap: tree (depth-2) + entry points, built deterministically from the
//     filesystem, cached to .brocode/repo-map.json keyed by a content hash of
//     the file list — rebuilt only when the project actually changes.
//   - Usage: counts how often each file is read/edited per session, aggregated
//     across sessions in .brocode/usage.json, so warm starts can prioritize the
//     files the team actually works on (not just the ones the LLM guessed).
package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// fileCache avoids re-walking the filesystem on repeated ListProjectFiles calls
// within the same session (BuildMap + SetScopeFiles in app.go both call it).
// Invalidated when the workspace root's mtime advances — a cheap stat that
// catches new/removed top-level files. Nested changes are handled separately by
// the dirty-file snapshot (file-snapshot.json) used by the LSP scanner.
var (
	fileCacheMu    sync.RWMutex
	fileCacheDir   string
	fileCacheMTime time.Time
	fileCacheList  []string
)

// checkWorkspaceChanged returns true when the workspace dir's mtime has
// advanced since the last cache hit (or no cache exists yet).
func checkWorkspaceChanged(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return true // can't stat — force re-walk (safer than stale)
	}
	mtime := fi.ModTime()
	fileCacheMu.RLock()
	cached := fileCacheDir == dir && !mtime.After(fileCacheMTime)
	fileCacheMu.RUnlock()
	return !cached
}

// cachedListProjectFiles returns the project file list, walking the filesystem
// only when the workspace has changed since the last cache hit.
func cachedListProjectFiles(dir string) []string {
	if !checkWorkspaceChanged(dir) {
		fileCacheMu.RLock()
		result := fileCacheList
		fileCacheMu.RUnlock()
		if result != nil {
			return result
		}
	}

	files := listProjectFiles(dir)

	fileCacheMu.Lock()
	fileCacheDir = dir
	fileCacheMTime = time.Now()
	fileCacheList = files
	fileCacheMu.Unlock()

	return files
}

// Map is the deterministic project map: a depth-2 tree plus detected entry
// points and the most-used files (from cross-session usage).
type Map struct {
	// Tree is a compact depth-2 directory listing (relative paths).
	Tree []string `json:"tree"`
	// EntryPoints are the files/commands a developer reaches for first
	// (main.go, package.json bin, cmd/, index.ts, ...).
	EntryPoints []string `json:"entry_points"`
	// Stacks are the repo's primary languages, detected from manifest and
	// entry-point files ("go", "node", "ts", "rust", ...), each with the
	// files that evidence it. The prompt renders them as a one-line STACK
	// hint ("STACK: go (go.mod, main.go)") and biases the skill catalog
	// toward the repo's stack.
	Stacks []Stack `json:"stacks,omitempty"`
	// HotFiles are the top-N files by usage frequency ("" when no usage yet).
	HotFiles []string `json:"hot_files"`
	// Hash is the content hash of the file list this map was built from.
	Hash string `json:"hash"`
}

// skipDirs are directories never walked (always noise for a code map).
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".next": true, ".turbo": true,
	"dist": true, "build": true, ".cache": true, "vendor": true,
	".venv": true, "venv": true, "__pycache__": true, ".brocode": true,
	"coverage": true, "target": true, ".gradle": true, "Pods": true,
}

// isSensitiveRepoName mirrors the tool/search guards: secrets and keys never
// appear in the repo map (entry points, tree, hot files).
func isSensitiveRepoName(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, ".env") {
		return true
	}
	switch lower {
	case ".npmrc", ".pypirc", ".netrc", ".pgpass", ".htpasswd",
		"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa", "credentials.json",
		"service-account.json", "secrets.yaml", "secrets.yml",
		".git-credentials", ".dockercfg":
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".ppk", ".gpg", ".kdbx", ".ovpn"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// skipExts are file extensions never listed in the tree (generated/binary).
var skipExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".ico": true, ".woff": true, ".woff2": true, ".ttf": true, ".lock": true,
	".map": true, ".min.js": true, ".min.css": true, ".pyc": true, ".so": true,
	".dylib": true, ".dll": true, ".exe": true, ".zip": true, ".tar": true,
	".gz": true, ".pdf": true, ".class": true, ".jar": true, ".sum": true,
}

// mapPath returns the cache file location (.brocode/repo-map.json).
func mapPath(workspaceDir string) string {
	return filepath.Join(workspaceDir, ".brocode", "repo-map.json")
}

// usagePath returns the usage file location (.brocode/usage.json).
func usagePath(workspaceDir string) string {
	return filepath.Join(workspaceDir, ".brocode", "usage.json")
}

// BuildMap walks the workspace and produces a deterministic Map, honoring the
// cache: when the file-list hash matches the cached map's hash it is returned
// as-is (no re-scan). Usage is merged in so hot files reflect the latest
// cross-session counts. Returns nil when the workspace is unusable.
func BuildMap(workspaceDir string, usage *Usage) *Map {
	if workspaceDir == "" {
		return nil
	}
	files := cachedListProjectFiles(workspaceDir)
	if len(files) == 0 {
		return nil
	}

	m := &Map{
		Tree:        buildTree(files, workspaceDir),
		EntryPoints: detectEntryPoints(files),
		Stacks:      DetectStackInfo(files),
	}
	if usage != nil {
		m.HotFiles = usage.Top(10)
	}

	// Cache hit: same file list → same map (tree + entry points are pure
	// functions of the file list; hot files come from usage which is always
	// fresh). Rebuild is cheap but skipping it keeps startup snappy.
	hash := hashFiles(files)
	if cached, err := loadMap(mapPath(workspaceDir)); err == nil &&
		cached != nil && cached.Hash == hash {
		cached.HotFiles = m.HotFiles // merge fresh usage
		if len(cached.Stacks) == 0 { // maps cached before stack detection
			cached.Stacks = DetectStackInfo(files)
		}
		return cached
	}
	m.Hash = hash
	_ = saveMap(mapPath(workspaceDir), m)
	return m
}

// String renders the map as a compact system-prompt block ("" when nil).
func (m *Map) String() string {
	if m == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("REPO MAP (deterministic, auto-built):\n")
	if len(m.EntryPoints) > 0 {
		sb.WriteString("Entry points: ")
		sb.WriteString(strings.Join(m.EntryPoints, ", "))
		sb.WriteString("\n")
	}
	if len(m.HotFiles) > 0 {
		sb.WriteString("Most-used files (across sessions): ")
		sb.WriteString(strings.Join(m.HotFiles, ", "))
		sb.WriteString("\n")
	}
	if len(m.Tree) > 0 {
		sb.WriteString("Structure:\n")
		sb.WriteString(strings.Join(m.Tree, "\n"))
	}
	return sb.String()
}

// ── Filesystem walk ─────────────────────────────────────────────────────────

// listProjectFiles returns relative paths of all project files, skipping
// ignored/generated dirs and extensions, sorted for a stable hash.
func listProjectFiles(root string) []string {
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, ".") {
			return nil // hidden files/configs
		}
		if skipExts[strings.ToLower(filepath.Ext(rel))] {
			return nil
		}
		// Never surface sensitive files (.env, keys, credentials) in the map.
		if isSensitiveRepoName(d.Name()) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	sort.Strings(files)
	return files
}

// buildTree renders a compact depth-2 tree (root dirs + their direct children,
// capped so a huge monorepo stays small in the prompt).
func buildTree(files []string, root string) []string {
	rootName := filepath.Base(filepath.Clean(root))
	children := map[string][]string{}
	var dirs []string
	for _, f := range files {
		parts := strings.Split(f, "/")
		if len(parts) == 1 {
			continue // root-level files listed separately
		}
		top := parts[0]
		if _, ok := children[top]; !ok {
			dirs = append(dirs, top)
		}
		if len(parts) == 2 {
			children[top] = append(children[top], parts[1])
		} else {
			// depth ≥ 2: show the subdir name only
			children[top] = append(children[top], parts[1]+"/")
		}
	}
	sort.Strings(dirs)

	var rootFiles []string
	for _, f := range files {
		if !strings.Contains(f, "/") {
			rootFiles = append(rootFiles, f)
		}
	}
	sort.Strings(rootFiles)

	var out []string
	if len(rootFiles) > 0 {
		out = append(out, rootName+"/")
		for _, f := range rootFiles {
			out = append(out, "  "+f)
		}
	}
	for _, d := range dirs {
		out = append(out, d+"/")
		items := children[d]
		sort.Strings(items)
		if len(items) > 8 {
			items = items[:8]
			items = append(items, "…")
		}
		for _, c := range items {
			out = append(out, "  "+c)
		}
	}
	return out
}

// Stack is one detected language plus the files that evidence it.
type Stack struct {
	Name  string   `json:"name"`
	Files []string `json:"files,omitempty"`
}

// DetectStackInfo returns the repo's primary languages, derived
// deterministically from manifest files (go.mod → "go", package.json →
// "node", Cargo.toml → "rust", ...) refined by entry-point code files
// (package.json alone cannot tell TS from JS; src/main.ts → "ts"). Each stack
// carries its evidence files so the prompt can render "STACK: go (go.mod,
// main.go)". Used to bias the skill catalog toward the repo's stack and to
// emit a one-line STACK hint, so stack-specific skills follow the repo instead
// of relying on the model to guess. Empty when no stack is detectable.
func DetectStackInfo(files []string) []Stack {
	byManifest := map[string]string{
		"go.mod": "go", "go.work": "go",
		"package.json":      "node",
		"Cargo.toml":        "rust",
		"pyproject.toml":    "python", "setup.py": "python", "requirements.txt": "python",
		"pom.xml": "java", "build.gradle": "java", "build.gradle.kts": "java",
		"Gemfile":      "ruby",
		"composer.json": "php",
	}
	var stacks []Stack
	find := func(name string) *Stack {
		for i := range stacks {
			if stacks[i].Name == name {
				return &stacks[i]
			}
		}
		return nil
	}
	add := func(name, file string) {
		s := find(name)
		if s == nil {
			stacks = append(stacks, Stack{Name: name})
			s = &stacks[len(stacks)-1]
		}
		for _, e := range s.Files {
			if e == file {
				return
			}
		}
		s.Files = append(s.Files, file)
	}
	// Manifests first (strongest signal), then entry-point code files.
	for _, f := range files {
		if s, ok := byManifest[filepath.Base(f)]; ok {
			add(s, f)
		}
	}
	for _, f := range files {
		switch filepath.Base(f) {
		case "main.go", "cli.go":
			add("go", f)
		case "main.ts", "index.ts", "app.tsx", "main.tsx", "server.ts":
			add("ts", f)
		case "main.js", "index.js", "app.jsx", "server.js":
			add("js", f)
		case "main.py":
			add("python", f)
		case "main.rs":
			add("rust", f)
		case "main.java":
			add("java", f)
		}
	}
	return stacks
}

// DetectStack returns just the stack names (no evidence files).
func DetectStack(files []string) []string {
	info := DetectStackInfo(files)
	names := make([]string, len(info))
	for i, s := range info {
		names[i] = s.Name
	}
	return names
}

// detectEntryPoints finds the files a developer reaches for first, by
// well-known names and directories. Returns relative paths.
func detectEntryPoints(files []string) []string {
	var out []string
	preferred := []string{
		"go.mod", "package.json", "Cargo.toml", "pyproject.toml",
		"pom.xml", "build.gradle", "Makefile", "docker-compose.yml",
		"README.md", "AGENTS.md",
	}
	seen := map[string]bool{}
	for _, p := range preferred {
		for _, f := range files {
			if f == p || strings.HasSuffix(f, "/"+p) {
				if !seen[p] {
					seen[p] = true
					out = append(out, f)
				}
			}
		}
	}
	// Entry-point code files.
	for _, f := range files {
		base := filepath.Base(f)
		if base == "main.go" || base == "main.ts" || base == "index.ts" ||
			base == "index.js" || base == "app.tsx" || base == "main.py" ||
			base == "main.rs" || base == "cli.go" || base == "cmd" {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// ── Cache ───────────────────────────────────────────────────────────────────

// hashFiles produces a stable content hash of the file list.
func hashFiles(files []string) string {
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func loadMap(path string) (*Map, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Map
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func saveMap(path string, m *Map) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// ── Usage tracking ──────────────────────────────────────────────────────────

// Usage counts how often each file was read or edited across sessions, so
// warm starts can prioritize what the team actually works on. Persisted to
// .brocode/usage.json; counts decay slowly so long-gone hotspots fade.
type Usage struct {
	path   string
	counts map[string]int
	dirty  bool
}

// NewUsage loads (or creates) the usage store for the workspace.
func NewUsage(workspaceDir string) *Usage {
	u := &Usage{path: usagePath(workspaceDir), counts: map[string]int{}}
	if u.path == "" {
		return u
	}
	if data, err := os.ReadFile(u.path); err == nil {
		var raw map[string]int
		if json.Unmarshal(data, &raw) == nil {
			u.counts = raw
		}
	}
	return u
}

// Record bumps the count for each touched file. Paths are kept relative for
// portability; counts are capped so no single file dominates forever.
func (u *Usage) Record(paths []string) {
	if u == nil || len(paths) == 0 {
		return
	}
	for _, p := range paths {
		p = filepath.ToSlash(strings.TrimPrefix(p, "./"))
		if p == "" || strings.HasPrefix(p, ".brocode/") {
			continue
		}
		if u.counts[p] < 1000 {
			u.counts[p]++
		}
	}
	u.dirty = true
}

// Top returns the n most-used files ("" when no usage recorded).
func (u *Usage) Top(n int) []string {
	if u == nil || len(u.counts) == 0 {
		return nil
	}
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range u.counts {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	out := make([]string, 0, n)
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, all[i].k)
	}
	return out
}

// Save persists the counts (only when changed).
func (u *Usage) Save() {
	if u == nil || !u.dirty || u.path == "" {
		return
	}
	data, err := json.MarshalIndent(u.counts, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(u.path), 0o755) == nil {
		_ = os.WriteFile(u.path, data, 0o644)
	}
	u.dirty = false
}

// snapshotPath returns the mtime snapshot file location for dirty-file tracking.
func snapshotPath(workspaceDir string) string {
	return filepath.Join(workspaceDir, ".brocode", "file-snapshot.json")
}

// DirtyFiles returns the set of source files that have changed (by mtime)
// since the last snapshot was taken, or all files if no snapshot exists yet.
// This enables incremental LSP scans: only re-open files whose mtime or size
// differs from the cached snapshot, skipping unchanged files entirely.
// Call UpdateSnapshot after scanning to refresh the baseline.
func DirtyFiles(workspaceDir string, files []string) []string {
	snap := loadSnapshot(snapshotPath(workspaceDir))
	if len(snap) == 0 {
		// No snapshot yet — all files are "dirty" (first run).
		UpdateSnapshot(workspaceDir, files)
		return files
	}

	var dirty []string
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		current := fileSig{fi.ModTime().Unix(), fi.Size()}
		if prev, ok := snap[f]; !ok || prev != current {
			dirty = append(dirty, f)
		}
	}
	return dirty
}

// fileSig is a compact mtime+size signature used for change detection.
type fileSig struct {
	ModTime int64
	Size    int64
}

// UpdateSnapshot writes the current mtime signatures for the given files,
// refreshing the dirty-file baseline. Called after a successful scan.
func UpdateSnapshot(workspaceDir string, files []string) {
	snap := make(map[string]fileSig, len(files))
	for _, f := range files {
		fi, err := os.Stat(f)
		if err == nil {
			snap[f] = fileSig{fi.ModTime().Unix(), fi.Size()}
		}
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(snapshotPath(workspaceDir)), 0o755)
	_ = os.WriteFile(snapshotPath(workspaceDir), data, 0o644)
}

// ListProjectFiles returns the full list of project source files (relative,
// sorted, with heavy dirs / sensitive files / binary extensions excluded).
// Public wrapper around listProjectFiles for use by the smart-scope layer.
func ListProjectFiles(workspaceDir string) []string {
	return cachedListProjectFiles(workspaceDir)
}

// loadSnapshot reads the file signature snapshot from disk.
func loadSnapshot(path string) map[string]fileSig {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var raw map[string]struct {
		ModTime int64 `json:"m"`
		Size    int64 `json:"s"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	out := make(map[string]fileSig, len(raw))
	for k, v := range raw {
		out[k] = fileSig{v.ModTime, v.Size}
	}
	return out
}

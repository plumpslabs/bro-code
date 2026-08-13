package tui

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Session persistence — Principle 5: bounded by design. Each quit writes a NEW
// uniquely-named session file (session_<base36-ms>.jsonl) so /history can list
// every past conversation of the project, while a per-project retention cap
// (maxSessionFiles) keeps the directory bounded. The resume flag (-c) always
// picks the newest session file — "continue where I left off".
const (
	sessionDir      = ".brocode"
	sessionFile     = "latest.jsonl" // global resume fallback, overwritten each quit
	maxSessionFiles = 20             // per-project retention cap (oldest pruned)
)

// EnsureGlobalSetup performs zero-setup native initialization of ~/.brocode/ globally.
func EnsureGlobalSetup() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	baseDir := filepath.Join(home, sessionDir)
	projectsDir := filepath.Join(baseDir, "projects")

	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		return err
	}

	cfgPath := filepath.Join(baseDir, "config.json")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		cfgData := map[string]interface{}{
			"version":           "2.5.30",
			"default_mode":      "builder",
			"matcha_integrated": true,
			"auto_diff":         true,
			"session_isolation": "per_project",
			"providers":         []string{"opencode", "antigravity"},
		}
		if data, err := json.MarshalIndent(cfgData, "", "  "); err == nil {
			_ = os.WriteFile(cfgPath, data, 0o644)
		}
	}
	return nil
}

type GlobalConfig struct {
	Version          string   `json:"version"`
	DefaultMode      string   `json:"default_mode"`
	MatchaIntegrated bool     `json:"matcha_integrated"`
	AutoDiff         bool     `json:"auto_diff"`
	SessionIsolation string   `json:"session_isolation"`
	Providers        []string `json:"providers"`
	LastProvider     string   `json:"last_provider,omitempty"`
	LastModel        string   `json:"last_model,omitempty"`
}

func LoadConfig() GlobalConfig {
	var cfg GlobalConfig
	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}
	cfgPath := filepath.Join(home, sessionDir, "config.json")
	if data, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	return cfg
}

func SaveLastModel(provider, model string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	cfgPath := filepath.Join(home, sessionDir, "config.json")
	cfg := LoadConfig()
	cfg.LastProvider = provider
	cfg.LastModel = model
	if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		_ = os.WriteFile(cfgPath, data, 0o644)
	}
}

// sessionLine is the on-disk JSON shape of one chat message. The role is a
// string so the file stays human-readable (transparency, Principle 3 spirit).
// Collapsible display state (summary/content/collapsed) is deliberately NOT
// persisted — it is per-session UI state, not conversation data.
type sessionLine struct {
	Role  string   `json:"role"`
	Text  string   `json:"text"`
	Trace []string `json:"trace,omitempty"`
}

// GetProjectName returns the clean base name of the current working directory.
func GetProjectName() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" || cwd == "." || cwd == "/" {
		return "default"
	}
	base := filepath.Base(cwd)
	if base == "." || base == "/" || base == "" {
		return "default"
	}
	reg := regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	clean := reg.ReplaceAllString(base, "_")
	if clean == "" {
		return "default"
	}
	return clean
}

// GetProjectSessionID returns a deterministic 8-char hex session ID for the current working directory.
func GetProjectSessionID() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "default"
	}
	hash := sha256.Sum256([]byte(cwd))
	return fmt.Sprintf("%x", hash)[:8]
}

// sessionRoot resolves the per-project session DIRECTORY for the current cwd:
// ~/.brocode/projects/<project-name>. Individual session files live inside it.
// Overridable in tests to isolate from the real home directory.
var sessionRoot = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, sessionDir, "projects", GetProjectName()), nil
}

// sessionFileEntry describes one saved session file on disk.
type sessionFileEntry struct {
	name string // basename, e.g. session_k9x2fa.jsonl
	path string
	mod  int64 // UnixNano — tie-free ordering for rapid consecutive saves
}

// sessionFilesIn returns the session_*.jsonl files in dir, newest first.
func sessionFilesIn(dir string) []sessionFileEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []sessionFileEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "session_") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, iErr := e.Info()
		if iErr != nil {
			continue
		}
		files = append(files, sessionFileEntry{
			name: name,
			path: filepath.Join(dir, name),
			mod:  info.ModTime().UnixNano(),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].mod != files[j].mod {
			return files[i].mod > files[j].mod
		}
		// Tie-break on the name: a later save has a larger base36-ms (and a
		// larger numeric suffix within the same millisecond), so newest-first
		// survives even when mtimes tie exactly — a just-written file can
		// never be sorted as "oldest" and pruned.
		return files[i].name > files[j].name
	})
	return files
}

// freshSessionPath returns a path in dir that does not exist yet, using a
// base36-millis timestamp. Collisions (two saves in the same millisecond) get
// a numeric suffix instead of overwriting an existing session.
func freshSessionPath(dir string) string {
	base := strconv.FormatInt(time.Now().UnixMilli(), 36)
	p := filepath.Join(dir, "session_"+base+".jsonl")
	for i := 1; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		// Extreme collision pressure (101 saves in the same millisecond):
		// switch to a nanosecond base rather than return a path that still
		// exists — os.Create in saveSessionTo would truncate an existing
		// session.
		if i > 100 {
			return filepath.Join(dir, "session_"+strconv.FormatInt(time.Now().UnixNano(), 36)+".jsonl")
		}
		p = filepath.Join(dir, fmt.Sprintf("session_%s_%d.jsonl", base, i))
	}
}

// pruneSessionFiles deletes the oldest session files beyond keep so the
// project directory never grows unbounded. The just-written file is the
// newest by mtime and is therefore never a prune target.
func pruneSessionFiles(dir string, keep int) {
	if keep < 1 {
		keep = 1 // never wipe every session (incl. the just-written file)
	}
	files := sessionFilesIn(dir) // newest first
	for i := keep; i < len(files); i++ {
		_ = os.Remove(files[i].path)
	}
}

// SaveSession writes the chat history as JSONL to a NEW per-project session
// file (~/.brocode/projects/<proj>/session_<base36-ms>.jsonl), then prunes
// the project down to maxSessionFiles. It returns the absolute path written.
// When saving to the real home location, a copy also goes to
// ~/.brocode/sessions/latest.jsonl as a global resume fallback. The fallback
// is only written for the real path, so tests that override sessionRoot can
// never touch the host's home directory (a leaked write here previously
// clobbered the real latest.jsonl with test data).
func SaveSession(messages []chatMsg) (string, error) {
	dir, err := sessionRoot()
	if err != nil {
		return "", err
	}
	path := freshSessionPath(dir)
	if err := saveSessionTo(messages, path); err != nil {
		return "", err
	}
	pruneSessionFiles(dir, maxSessionFiles)
	if home, hErr := os.UserHomeDir(); hErr == nil {
		realBase := filepath.Join(home, sessionDir)
		// Require a path separator boundary so a sibling dir like
		// ~/.brocode_backup can never be mistaken for the real base.
		if strings.HasPrefix(dir, realBase+string(filepath.Separator)) {
			latestPath := filepath.Join(realBase, "sessions", sessionFile)
			_ = saveSessionTo(messages, latestPath)
		}
	}
	return path, nil
}

// LoadSession resumes the MOST RECENT session saved for the current project —
// -c means "continue where I left off", and each quit appends a new session
// file, so the newest file is the last conversation.
func LoadSession() ([]chatMsg, error) {
	dir, err := sessionRoot()
	if err != nil {
		return nil, err
	}
	files := sessionFilesIn(dir)
	if len(files) == 0 {
		return nil, nil
	}
	return loadSessionFrom(files[0].path)
}

func saveSessionTo(messages []chatMsg, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, cm := range messages {
		// roleTool rows are transient agentic feedback (⚙ Tool Executed) —
		// never persisted, so a resumed session doesn't replay internal tool
		// output as if the user had typed it.
		if cm.role == roleTool {
			continue
		}
		// Blank rows (an agent turn interrupted before any content, a stray
		// whitespace message) must never be persisted — a resumed session
		// would replay them as empty/divider-only messages.
		if strings.TrimSpace(cm.text) == "" && strings.TrimSpace(cm.content) == "" {
			continue
		}
		b, err := json.Marshal(sessionLine{Role: roleName(cm.role), Text: cm.text, Trace: cm.trace})
		if err != nil {
			return err
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return w.Flush()
}

func loadSessionFrom(path string) ([]chatMsg, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var msgs []chatMsg
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for sc.Scan() {
		var line sessionLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue // skip a corrupt line — never fail the whole session
		}
		msgs = append(msgs, chatMsg{role: roleFromName(line.Role), text: line.Text, trace: line.Trace})
	}
	return msgs, sc.Err()
}

func roleName(r role) string {
	switch r {
	case roleUser:
		return "user"
	case roleAgent:
		return "agent"
	case roleTool:
		return "tool"
	default:
		return "system"
	}
}

func roleFromName(s string) role {
	switch s {
	case "user":
		return roleUser
	case "agent":
		return roleAgent
	case "tool":
		return roleTool
	default:
		return roleSystem
	}
}

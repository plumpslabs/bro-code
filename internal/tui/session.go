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
)

// Session persistence — Principle 5: a single latest.jsonl, overwritten every
// run. Bounded by design: one file, and the chat itself is already capped at
// maxHistory. A full session history (N sessions, TTL retention) comes later
// with the storage layer; for now the resume flag (-c) only needs the last
// session.
const (
	sessionDir  = ".brocode"
	sessionFile = "latest.jsonl"
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

// sessionPath resolves the session file location for the current project:
// ~/.brocode/projects/<project-name>/session_<hash>.jsonl
var sessionPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	projName := GetProjectName()
	id := GetProjectSessionID()
	return filepath.Join(home, sessionDir, "projects", projName, fmt.Sprintf("session_%s.jsonl", id)), nil
}

// SessionFilePath returns the absolute path where the session is saved.
func SessionFilePath() string {
	p, err := sessionPath()
	if err != nil {
		return "~/.brocode/projects/default/latest.jsonl"
	}
	return p
}

// SaveSession writes the chat history as JSONL to ~/.brocode/sessions/session_<id>.jsonl and latest.jsonl.
// Call on quit only when the conversation actually started.
func SaveSession(messages []chatMsg) error {
	path, err := sessionPath()
	if err != nil {
		return err
	}
	if err := saveSessionTo(messages, path); err != nil {
		return err
	}
	// Also save to latest.jsonl for global fallback
	if home, hErr := os.UserHomeDir(); hErr == nil {
		latestPath := filepath.Join(home, sessionDir, "sessions", sessionFile)
		_ = saveSessionTo(messages, latestPath)
	}
	return nil
}

// LoadSession reads the project-specific JSONL session file (~/.brocode/sessions/session_<id>.jsonl).
func LoadSession() ([]chatMsg, error) {
	path, err := sessionPath()
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil, nil
		}
		return nil, statErr
	}
	return loadSessionFrom(path)
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
	default:
		return roleSystem
	}
}

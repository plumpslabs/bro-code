package tui

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// sessionLine is the on-disk JSON shape of one chat message. The role is a
// string so the file stays human-readable (transparency, Principle 3 spirit).
// Collapsible display state (summary/content/collapsed) is deliberately NOT
// persisted — it is per-session UI state, not conversation data.
type sessionLine struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// sessionPath resolves the session file location. Declared as a var so tests
// can point it at a temp dir without touching the real home directory.
var sessionPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, sessionDir, "sessions", sessionFile), nil
}

// SaveSession writes the chat history as JSONL to ~/.brocode/sessions.
// Call on quit only when the conversation actually started.
func SaveSession(messages []chatMsg) error {
	path, err := sessionPath()
	if err != nil {
		return err
	}
	return saveSessionTo(messages, path)
}

// LoadSession reads the JSONL session file. Returns nil messages when no
// session file exists yet.
func LoadSession() ([]chatMsg, error) {
	path, err := sessionPath()
	if err != nil {
		return nil, err
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
		b, err := json.Marshal(sessionLine{Role: roleName(cm.role), Text: cm.text})
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
		msgs = append(msgs, chatMsg{role: roleFromName(line.Role), text: line.Text})
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

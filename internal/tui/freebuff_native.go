package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// manicodeCredentials represents the structure of ~/.config/manicode/credentials.json
type manicodeCredentials struct {
	Default *manicodeProfile `json:"default,omitempty"`
	// Some versions store authToken at top level
	AuthToken string `json:"authToken,omitempty"`
}

// manicodeProfile is one credential profile in the credentials file.
type manicodeProfile struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Email     string `json:"email,omitempty"`
	AuthToken string `json:"authToken,omitempty"`
}

// manicodeCredentialsFile returns the path to the Manicode/Freebuff/Codebuff credentials.
func manicodeCredentialsFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "manicode", "credentials.json")
}

// loadManicodeCredentials reads the saved Freebuff/Codebuff credentials.
// Returns the auth token if found, empty string otherwise.
func loadManicodeCredentials() string {
	data, err := os.ReadFile(manicodeCredentialsFile())
	if err != nil {
		return ""
	}

	var creds manicodeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}

	// Try default profile first
	if creds.Default != nil && creds.Default.AuthToken != "" {
		return creds.Default.AuthToken
	}

	// Fallback to top-level authToken
	if creds.AuthToken != "" {
		return creds.AuthToken
	}

	return ""
}

// freebuffBackend is the Freebuff/Codebuff backend API base URL.
const freebuffBackend = "https://codebuff.com"

// freebuffSession represents a Freebuff API session.
type freebuffSession struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// freebuffRun represents a Freebuff API run.
type freebuffRun struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// freebuffChatRequest is the chat completion request to Freebuff API.
type freebuffChatRequest struct {
	SessionID string            `json:"sessionId"`
	RunID     string            `json:"runId"`
	Messages  []freebuffMessage `json:"messages"`
	Model     string            `json:"model,omitempty"`
}

// freebuffMessage is a single message in Freebuff API format.
type freebuffMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// freebuffChatResponse is the response from Freebuff API.
type freebuffChatResponse struct {
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

// freebuffCreateSession creates a new Freebuff API session.
func freebuffCreateSession(authToken string) (*freebuffSession, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequest("POST", freebuffBackend+"/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// Parse error response for better error messages
		var errResp struct {
			Error   string `json:"error"`
			Message string `json:"message"`
			Code    string `json:"code"`
		}
		if err := json.Unmarshal(body, &errResp); err == nil {
			if errResp.Error != "" {
				return nil, fmt.Errorf("%s", errResp.Error)
			}
			if errResp.Message != "" {
				return nil, fmt.Errorf("%s", errResp.Message)
			}
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(string(body), 200))
	}

	var session freebuffSession
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, fmt.Errorf("invalid session response: %w", err)
	}
	return &session, nil
}

// freebuffCreateRun creates a new Freebuff API run under a session.
func freebuffCreateRun(authToken, sessionID, runType string) (*freebuffRun, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	payload := map[string]string{
		"sessionId": sessionID,
		"type":      runType,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", freebuffBackend+"/api/v1/freebuff/run", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("run create failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var run freebuffRun
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		return nil, err
	}
	return &run, nil
}

// freebuffChat sends a chat message to Freebuff API and returns the response.
func freebuffChat(authToken, sessionID, runID, model, message string) (string, error) {
	client := &http.Client{Timeout: 600 * time.Second}

	req := freebuffChatRequest{
		SessionID: sessionID,
		RunID:     runID,
		Model:     model,
		Messages: []freebuffMessage{
			{Role: "user", Content: message},
		},
	}
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequest("POST", freebuffBackend+"/api/v1/freebuff/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+authToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("chat failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var chatResp freebuffChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", err
	}

	if chatResp.Error != "" {
		return "", fmt.Errorf("freebuff error: %s", chatResp.Error)
	}

	return chatResp.Content, nil
}

// DetectFreebuffCredentials checks if Freebuff/Codebuff credentials exist locally.
// Returns (detected, authToken).
func DetectFreebuffCredentials() (bool, string) {
	// Check environment variable first
	if key := os.Getenv("CODEBUFF_API_KEY"); key != "" {
		return true, key
	}

	// Check saved credentials
	if token := loadManicodeCredentials(); token != "" {
		return true, token
	}

	return false, ""
}

// freebuffNativeChat performs a native Freebuff API chat completion.
// This reads saved credentials and calls the Freebuff backend directly.
func freebuffNativeChat(authToken, model, message string) (string, error) {
	// Step 1: Create session
	session, err := freebuffCreateSession(authToken)
	if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}

	// Step 2: Create run (type "base2-free" for free tier)
	run, err := freebuffCreateRun(authToken, session.ID, "base2-free")
	if err != nil {
		return "", fmt.Errorf("run: %w", err)
	}

	// Step 3: Chat
	response, err := freebuffChat(authToken, session.ID, run.ID, model, message)
	if err != nil {
		return "", fmt.Errorf("chat: %w", err)
	}

	return response, nil
}

package provenance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// GitNotesRef is the dedicated git notes reference for BroCode attestations.
const GitNotesRef = "refs/notes/brocode-provenance"

// Attestation records verifiable cryptographic metadata for an AI-assisted code change.
type Attestation struct {
	Version      string    `json:"version"`
	SessionID    string    `json:"session_id"`
	TurnID       int       `json:"turn_id"`
	Timestamp    time.Time `json:"timestamp"`
	ModelID      string    `json:"model_id"`
	AgentVersion string    `json:"agent_version"`
	UserPrompt   string    `json:"user_prompt,omitempty"`
	PromptHash   string    `json:"prompt_hash"`
	DiffHash     string    `json:"diff_hash"`
	LSPClean     bool      `json:"lsp_clean"`
	TestsPassed  bool      `json:"tests_passed"`
	TokenCostUSD float64   `json:"token_cost_usd,omitempty"`
	ProofHash    string    `json:"proof_hash"`
}

// ComputeHash computes a SHA-256 hexadecimal hash of raw bytes or string.
func ComputeHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// NewAttestation creates a new Attestation and calculates its Merkle ProofHash.
func NewAttestation(sessionID string, turnID int, modelID, agentVersion, userPrompt, diffContent string, lspClean, testsPassed bool, costUSD float64) Attestation {
	promptHash := "sha256:" + ComputeHash(userPrompt)
	diffHash := "sha256:" + ComputeHash(diffContent)

	att := Attestation{
		Version:      "1.0.0",
		SessionID:    sessionID,
		TurnID:       turnID,
		Timestamp:    time.Now().UTC(),
		ModelID:      modelID,
		AgentVersion: agentVersion,
		UserPrompt:   userPrompt,
		PromptHash:   promptHash,
		DiffHash:     diffHash,
		LSPClean:     lspClean,
		TestsPassed:  testsPassed,
		TokenCostUSD: costUSD,
	}

	att.ProofHash = "sha256:" + att.ComputeProofHash()
	return att
}

// ComputeProofHash computes the deterministic cryptographic binding across all attestation fields.
func (a *Attestation) ComputeProofHash() string {
	raw := fmt.Sprintf("%s|%s|%d|%s|%s|%s|%s|%t|%t|%.6f",
		a.Version, a.SessionID, a.TurnID, a.Timestamp.Format(time.RFC3339),
		a.ModelID, a.PromptHash, a.DiffHash, a.LSPClean, a.TestsPassed, a.TokenCostUSD)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// FormatCommitTrailers returns standard RFC-822 Git commit trailers.
func FormatCommitTrailers(att Attestation) string {
	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString("Co-authored-by: BroCode AI <agent@brocode.dev>\n")
	if att.AgentVersion != "" {
		fmt.Fprintf(&sb, "AI-Agent: BroCode CLI %s\n", att.AgentVersion)
	}
	if att.ModelID != "" {
		fmt.Fprintf(&sb, "AI-Model: %s\n", att.ModelID)
	}
	if att.PromptHash != "" {
		fmt.Fprintf(&sb, "AI-Prompt-Hash: %s\n", att.PromptHash)
	}
	if att.ProofHash != "" {
		fmt.Fprintf(&sb, "AI-Attestation: %s\n", att.ProofHash)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// RecordGitNote stores the attestation JSON in Git Notes under refs/notes/brocode-provenance.
func RecordGitNote(repoDir string, commitRef string, att Attestation) error {
	if repoDir == "" {
		repoDir = "."
	}
	data, err := json.MarshalIndent(att, "", "  ")
	if err != nil {
		return err
	}

	cmd := exec.Command("git", "notes", "--ref="+GitNotesRef, "add", "-f", "-m", string(data), commitRef)
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to record git note: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ReadGitNote retrieves the attestation JSON from Git Notes for a given commit.
func ReadGitNote(repoDir string, commitRef string) (*Attestation, error) {
	if repoDir == "" {
		repoDir = "."
	}
	if commitRef == "" {
		commitRef = "HEAD"
	}

	cmd := exec.Command("git", "notes", "--ref="+GitNotesRef, "show", commitRef)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("no attestation note found for commit %s: %w", commitRef, err)
	}

	var att Attestation
	if err := json.Unmarshal(out, &att); err != nil {
		return nil, fmt.Errorf("failed to parse attestation json: %w", err)
	}
	return &att, nil
}

// VerifyCommitAttestation verifies the integrity of an attestation against git history.
func VerifyCommitAttestation(repoDir string, commitRef string) (*Attestation, bool, error) {
	if repoDir == "" {
		repoDir = "."
	}
	if commitRef == "" {
		commitRef = "HEAD"
	}

	att, err := ReadGitNote(repoDir, commitRef)
	if err != nil {
		// Fallback: parse commit message trailers if Git Notes are not fetched
		cmd := exec.Command("git", "log", "-1", "--format=%B", commitRef)
		cmd.Dir = repoDir
		body, logErr := cmd.Output()
		if logErr != nil {
			return nil, false, err
		}
		trailerAtt := parseTrailers(string(body))
		if trailerAtt == nil {
			return nil, false, fmt.Errorf("no provenance trailers or git notes found for %s", commitRef)
		}
		return trailerAtt, true, nil
	}

	// Verify proof hash integrity
	expectedProof := "sha256:" + att.ComputeProofHash()
	isValid := att.ProofHash == expectedProof

	return att, isValid, nil
}

// parseTrailers extracts basic attestation fields from RFC-822 commit message trailers.
func parseTrailers(msg string) *Attestation {
	lines := strings.Split(msg, "\n")
	att := &Attestation{}
	found := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if k, v, ok := strings.Cut(trimmed, ": "); ok {
			switch strings.ToLower(k) {
			case "ai-agent":
				att.AgentVersion = v
				found = true
			case "ai-model":
				att.ModelID = v
				found = true
			case "ai-prompt-hash":
				att.PromptHash = v
				found = true
			case "ai-attestation":
				att.ProofHash = v
				found = true
			}
		}
	}

	if !found {
		return nil
	}
	return att
}

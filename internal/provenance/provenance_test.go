package provenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttestationProofHash(t *testing.T) {
	att := NewAttestation("sess_123", 1, "poolside/laguna-s-2.1", "v0.1.40", "Implement JWT auth", "diff content here", true, true, 0.0125)

	if !strings.HasPrefix(att.PromptHash, "sha256:") {
		t.Fatalf("expected sha256 prompt hash, got: %s", att.PromptHash)
	}
	if !strings.HasPrefix(att.DiffHash, "sha256:") {
		t.Fatalf("expected sha256 diff hash, got: %s", att.DiffHash)
	}
	if !strings.HasPrefix(att.ProofHash, "sha256:") {
		t.Fatalf("expected sha256 proof hash, got: %s", att.ProofHash)
	}

	trailers := FormatCommitTrailers(att)
	if !strings.Contains(trailers, "Co-authored-by: BroCode AI <agent@brocode.dev>") {
		t.Errorf("missing Co-authored-by trailer: %s", trailers)
	}
	if !strings.Contains(trailers, "AI-Model: poolside/laguna-s-2.1") {
		t.Errorf("missing AI-Model trailer: %s", trailers)
	}
	if !strings.Contains(trailers, "AI-Attestation: "+att.ProofHash) {
		t.Errorf("missing AI-Attestation trailer: %s", trailers)
	}
}

func TestGitNotesProvenanceCycle(t *testing.T) {
	// Create temporary git repo
	tmpDir, err := os.MkdirTemp("", "brocode-provenance-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v (%s)", strings.Join(args, " "), err, string(out))
		}
	}

	runGit("init")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.com")

	// Commit initial file
	testFile := filepath.Join(tmpDir, "main.go")
	_ = os.WriteFile(testFile, []byte("package main\n\nfunc main() {}\n"), 0644)
	runGit("add", "main.go")
	runGit("commit", "-m", "initial commit")

	// Create and record attestation
	att := NewAttestation("sess_test_456", 2, "claude-3-7-sonnet", "v0.1.40", "Refactor main", "diff...", true, true, 0.005)
	if err := RecordGitNote(tmpDir, "HEAD", att); err != nil {
		t.Fatalf("RecordGitNote failed: %v", err)
	}

	// Read attestation back
	readAtt, err := ReadGitNote(tmpDir, "HEAD")
	if err != nil {
		t.Fatalf("ReadGitNote failed: %v", err)
	}
	if readAtt.ModelID != "claude-3-7-sonnet" {
		t.Errorf("expected model claude-3-7-sonnet, got: %s", readAtt.ModelID)
	}
	if readAtt.SessionID != "sess_test_456" {
		t.Errorf("expected session sess_test_456, got: %s", readAtt.SessionID)
	}

	// Verify attestation
	verifiedAtt, valid, err := VerifyCommitAttestation(tmpDir, "HEAD")
	if err != nil || !valid {
		t.Fatalf("expected valid attestation, got valid=%v, err=%v", valid, err)
	}
	if verifiedAtt.ProofHash != att.ProofHash {
		t.Errorf("proof hash mismatch: %s vs %s", verifiedAtt.ProofHash, att.ProofHash)
	}

	// Tamper test: corrupt a field
	readAtt.ModelID = "tampered-model"
	corruptedProof := "sha256:" + readAtt.ComputeProofHash()
	if corruptedProof == att.ProofHash {
		t.Error("tampered model must produce different proof hash")
	}
}

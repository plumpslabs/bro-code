package search

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutlineFile(t *testing.T) {
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "monolith.go")
	content := `package sample

// User holds customer data.
type User struct {
	ID   string
	Name string
}

// Authenticate checks user credentials.
func Authenticate(email, pass string) (*User, error) {
	return parseToken(email)
}

func parseToken(token string) (*User, error) {
	return nil, nil
}
`
	if err := os.WriteFile(goFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	syms, err := OutlineFile(goFile)
	if err != nil {
		t.Fatalf("OutlineFile failed: %v", err)
	}

	if len(syms) != 3 {
		t.Fatalf("expected 3 symbols, got %d", len(syms))
	}

	if syms[0].Name != "User" || syms[0].Kind != "struct" {
		t.Errorf("expected User struct, got %+v", syms[0])
	}
	if syms[1].Name != "Authenticate" || syms[1].Kind != "func" {
		t.Errorf("expected Authenticate func, got %+v", syms[1])
	}
	if len(syms[1].Calls) == 0 || syms[1].Calls[0] != "parseToken" {
		t.Errorf("expected Authenticate to call parseToken, got %v", syms[1].Calls)
	}
}

func TestClusterFileSymbols(t *testing.T) {
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "monolith.go")
	content := `package sample

func AuthLogin() {
	AuthValidate()
}

func AuthValidate() {}

func PaymentCharge() {
	PaymentRefund()
}

func PaymentRefund() {}
`
	if err := os.WriteFile(goFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	clusters, err := ClusterFileSymbols(goFile)
	if err != nil {
		t.Fatalf("ClusterFileSymbols failed: %v", err)
	}

	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters (auth and payment), got %d", len(clusters))
	}
}

func TestAnalyzeImpact(t *testing.T) {
	tmpDir := t.TempDir()
	file1 := filepath.Join(tmpDir, "auth.go")
	content1 := `package sample

func ValidateToken() bool {
	return true
}
`
	file2 := filepath.Join(tmpDir, "server.go")
	content2 := `package sample

func HandleReq() {
	ValidateToken()
}
`
	_ = os.WriteFile(file1, []byte(content1), 0644)
	_ = os.WriteFile(file2, []byte(content2), 0644)

	report, err := AnalyzeImpact(tmpDir, "ValidateToken", "auth.go")
	if err != nil {
		t.Fatalf("AnalyzeImpact failed: %v", err)
	}

	if report.FanIn < 1 {
		t.Errorf("expected at least 1 caller for ValidateToken, got %d", report.FanIn)
	}
}

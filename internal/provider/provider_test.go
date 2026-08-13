package provider

import (
	"testing"
)

func TestAutoDetect(t *testing.T) {
	cfg := AppConfig{}
	detected := AutoDetect(cfg)

	if len(detected) == 0 {
		t.Fatalf("expected at least 1 auto-detected provider (opencode gateway), got 0")
	}

	foundOpenCode := false
	for _, d := range detected {
		if d.Info.ID == "opencode" {
			foundOpenCode = true
			break
		}
	}

	if !foundOpenCode {
		t.Errorf("expected OpenCode gateway to be auto-detected by default")
	}
}

func TestLoadConfig(t *testing.T) {
	cfg := LoadConfig()
	if cfg.Providers == nil {
		t.Errorf("expected Providers map to be initialized")
	}
}

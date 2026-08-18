package ui

import (
	"testing"
)

func TestDetectAutocompleteSlashCommands(t *testing.T) {
	state := DetectAutocomplete("/mem", nil)
	if !state.Active {
		t.Fatal("expected autocomplete to be active for /mem")
	}
	if state.Kind != AutoKindSlash {
		t.Errorf("expected Kind = AutoKindSlash, got %v", state.Kind)
	}
	if len(state.Items) == 0 || state.Items[0].Value != "/memory" {
		t.Errorf("expected first match to be /memory, got %+v", state.Items)
	}

	// Space should deactivate slash autocomplete
	stateWithSpace := DetectAutocomplete("/memory arg", nil)
	if stateWithSpace.Active {
		t.Error("expected autocomplete to be inactive when space is present")
	}
}

func TestDetectAutocompleteFileMentions(t *testing.T) {
	files := []string{"cmd/brocode/main.go", "internal/ui/app.go", "internal/ui/autocomplete.go"}

	state := DetectAutocomplete("please inspect @app", files)
	if !state.Active {
		t.Fatal("expected autocomplete to be active for @app")
	}
	if state.Kind != AutoKindFile {
		t.Errorf("expected Kind = AutoKindFile, got %v", state.Kind)
	}
	if len(state.Items) == 0 || state.Items[0].Value != "internal/ui/app.go" {
		t.Errorf("expected match for app.go, got %+v", state.Items)
	}
}

func TestApplyAutocomplete(t *testing.T) {
	slashState := DetectAutocomplete("/un", nil)
	appliedSlash := ApplyAutocomplete("/un", slashState)
	if appliedSlash != "/undo " {
		t.Errorf("expected /undo , got %q", appliedSlash)
	}

	files := []string{"internal/ui/app.go"}
	fileState := DetectAutocomplete("look at @ap", files)
	appliedFile := ApplyAutocomplete("look at @ap", fileState)
	if appliedFile != "look at @internal/ui/app.go " {
		t.Errorf("expected 'look at @internal/ui/app.go ', got %q", appliedFile)
	}
}

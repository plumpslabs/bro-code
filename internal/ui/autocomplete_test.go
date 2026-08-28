package ui

import (
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/agent"
	"github.com/plumpslabs/bro-code/internal/skill"
)

func TestDetectAutocompleteSlashCommands(t *testing.T) {
	state := DetectAutocomplete("/mem", nil, nil, nil, AutocompleteState{})
	if !state.Active {
		t.Fatal("expected autocomplete to be active for /mem")
	}
	if state.Kind != AutoKindSlash {
		t.Errorf("expected Kind = AutoKindSlash, got %v", state.Kind)
	}
	if len(state.Items) == 0 || state.Items[0].Value != "/memory" {
		t.Errorf("expected first match to be /memory, got %+v", state.Items)
	}

	// Test custom skills auto-detect in slash commands
	customSkills := []skill.Skill{
		{Name: "db-migrate", Description: "Safe database migration procedure", Builtin: false},
	}
	skillState := DetectAutocomplete("/db", nil, nil, customSkills, AutocompleteState{})
	if !skillState.Active || len(skillState.Items) == 0 || skillState.Items[0].Value != "/db-migrate" {
		t.Fatalf("expected /db-migrate skill match, got %+v", skillState.Items)
	}

	// Test newly added breakthrough slash commands: /ask, /spec, /tournament
	askState := DetectAutocomplete("/as", nil, nil, nil, AutocompleteState{})
	if !askState.Active || len(askState.Items) == 0 || askState.Items[0].Value != "/ask" {
		t.Fatalf("expected /ask autocomplete match, got %+v", askState.Items)
	}

	specState := DetectAutocomplete("/sp", nil, nil, nil, AutocompleteState{})
	if !specState.Active || len(specState.Items) == 0 || specState.Items[0].Value != "/spec" {
		t.Fatalf("expected /spec autocomplete match, got %+v", specState.Items)
	}

	tournState := DetectAutocomplete("/tour", nil, nil, nil, AutocompleteState{})
	if !tournState.Active || len(tournState.Items) == 0 || tournState.Items[0].Value != "/tournament" {
		t.Fatalf("expected /tournament autocomplete match, got %+v", tournState.Items)
	}

	// Space should deactivate slash autocomplete
	stateWithSpace := DetectAutocomplete("/memory arg", nil, nil, nil, state)
	if stateWithSpace.Active {
		t.Error("expected autocomplete to be inactive when space is present")
	}
}

func TestDetectAutocompletePreservesSelection(t *testing.T) {
	// 1. Initial detection
	state1 := DetectAutocomplete("/", nil, nil, nil, AutocompleteState{})
	if !state1.Active || len(state1.Items) < 3 {
		t.Fatalf("expected at least 3 slash items, got %d", len(state1.Items))
	}
	if state1.Selected != 0 {
		t.Errorf("expected initial selected = 0, got %d", state1.Selected)
	}

	// 2. User moves down to index 2
	state1.Selected = 2

	// 3. Re-detect on same text (e.g. cursor blink or re-render)
	state2 := DetectAutocomplete("/", nil, nil, nil, state1)
	if state2.Selected != 2 {
		t.Errorf("expected preserved selected = 2, got %d", state2.Selected)
	}

	// 4. Query changes (e.g. user types 'u') -> resets to 0
	state3 := DetectAutocomplete("/u", nil, nil, nil, state2)
	if state3.Selected != 0 {
		t.Errorf("expected reset selected = 0 on new query, got %d", state3.Selected)
	}
}

func TestDetectAutocompleteFileAndAgentMentions(t *testing.T) {
	files := []string{"cmd/brocode/main.go", "internal/ui/app.go", "internal/ui/autocomplete.go", "locales/id.json", ".goreleaser.yaml"}
	agents := []agent.CustomAgent{
		{Name: "analyzer", Description: "AST code analyzer and tech debt auditor", Mode: "PLANNER"},
	}

	// 1. Agent mention test
	agState := DetectAutocomplete("please run @analy", files, agents, nil, AutocompleteState{})
	if !agState.Active || len(agState.Items) == 0 || agState.Items[0].Value != "analyzer" {
		t.Fatalf("expected agent match for @analy, got %+v", agState.Items)
	}

	// 2. File mention test
	state := DetectAutocomplete("please inspect @app", files, agents, nil, AutocompleteState{})
	if !state.Active {
		t.Fatal("expected autocomplete to be active for @app")
	}
	if state.Kind != AutoKindFile {
		t.Errorf("expected Kind = AutoKindFile, got %v", state.Kind)
	}
	if len(state.Items) == 0 || state.Items[0].Value != "internal/ui/app.go" {
		t.Errorf("expected match for app.go, got %+v", state.Items)
	}

	jsonState := DetectAutocomplete("check @id", files, agents, nil, AutocompleteState{})
	if !jsonState.Active || len(jsonState.Items) == 0 || jsonState.Items[0].Value != "locales/id.json" {
		t.Errorf("expected match for locales/id.json, got %+v", jsonState.Items)
	}
}

func TestApplyAutocomplete(t *testing.T) {
	slashState := DetectAutocomplete("/un", nil, nil, nil, AutocompleteState{})
	appliedSlash := ApplyAutocomplete("/un", slashState)
	if appliedSlash != "/undo " {
		t.Errorf("expected /undo , got %q", appliedSlash)
	}

	files := []string{"internal/ui/app.go"}
	fileState := DetectAutocomplete("look at @ap", files, nil, nil, AutocompleteState{})
	appliedFile := ApplyAutocomplete("look at @ap", fileState)
	if appliedFile != "look at @internal/ui/app.go " {
		t.Errorf("expected 'look at @internal/ui/app.go ', got %q", appliedFile)
	}
}

func TestRenderAutocompleteSlidingWindow(t *testing.T) {
	state := DetectAutocomplete("/", nil, nil, nil, AutocompleteState{})
	state.Selected = 8 // Select an item deep in the list

	out := RenderAutocomplete(state, 80)
	if out == "" {
		t.Fatal("expected non-empty rendered autocomplete box")
	}
	// Verify active item is rendered with indicator
	if !strings.Contains(out, "▸") {
		t.Errorf("expected highlight indicator '▸' in output:\n%s", out)
	}
}

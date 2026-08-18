package tool

import (
	"strings"
	"testing"
)

func TestValidateJSONNoDuplicateKeys(t *testing.T) {
	// Valid JSON without duplicates
	validJSON := `{
		"roleModal": {
			"recommendations": {
				"assignment": "Penugasan"
			}
		},
		"other": 123
	}`
	if err := ValidateJSONNoDuplicateKeys(validJSON); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}

	// Invalid JSON with duplicate root key
	dupRoot := `{
		"roleModal": {
			"title": "Role"
		},
		"other": "test",
		"roleModal": {
			"recommendations": {
				"assignment": "Penugasan"
			}
		}
	}`
	err := ValidateJSONNoDuplicateKeys(dupRoot)
	if err == nil {
		t.Fatal("expected duplicate key error, got nil")
	}
	if want := "duplicate key \"roleModal\""; !strings.Contains(err.Error(), want) {
		t.Fatalf("expected %q in error, got %v", want, err)
	}

	// Invalid JSON with duplicate nested key
	dupNested := `{
		"roleModal": {
			"title": "Role",
			"title": "Duplicate Role"
		}
	}`
	err = ValidateJSONNoDuplicateKeys(dupNested)
	if err == nil {
		t.Fatal("expected duplicate nested key error, got nil")
	}
	if want := "duplicate key \"title\""; !strings.Contains(err.Error(), want) {
		t.Fatalf("expected %q in error, got %v", want, err)
	}
}

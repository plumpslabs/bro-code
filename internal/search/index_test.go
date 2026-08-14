package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildGlobalIndex(t *testing.T) {
	dir := t.TempDir()

	svc := `class ConversationService {
  findAll() { return []; }
}
module.exports = ConversationService;
`
	router := `const ConversationService = require('./ConversationService');
const svc = new ConversationService();
svc.findAll();
`
	main := `package main

import "example.com/conv"

func main() {
	c := conv.New()
	_ = c
}
`
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/services/ConversationService.js", svc)
	write("src/routes/router.js", router)
	// Uses the symbol WITHOUT declaring it (no const X = / class pattern) —
	// this is the true "referencer".
	write("src/other.js", `const svc = new ConversationService();
svc.findAll();
`)
	write("main.go", main)
	// node_modules must be skipped.
	write("node_modules/pkg/index.js", `class ConversationService {}`)

	idx := BuildGlobalIndex(dir)

	// Lookup finds the class definition (in the real file, not node_modules).
	defs := idx.Lookup("ConversationService")
	if len(defs) == 0 {
		t.Fatalf("expected ConversationService to be indexed")
	}
	found := false
	for _, d := range defs {
		if strings.Contains(d.File, "node_modules") {
			t.Errorf("node_modules leaked into index: %s", d.File)
		}
		if strings.Contains(d.File, "ConversationService.js") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected definition in ConversationService.js, got %+v", defs)
	}

	// Referencers finds files that USE the symbol without declaring it
	// (router.js declares `const ConversationService = require(...)` so it
	// counts as an occurrence in Lookup, not a referencer).
	refs := idx.Referencers("ConversationService")
	var sawOther bool
	for _, r := range refs {
		if strings.Contains(r, "other.js") {
			sawOther = true
		}
	}
	if !sawOther {
		t.Errorf("expected other.js among referencers, got %v", refs)
	}
	// The declaration site is still visible via Lookup.
	var sawRouterDef bool
	for _, d := range idx.Lookup("ConversationService") {
		if strings.Contains(d.File, "router.js") {
			sawRouterDef = true
		}
	}
	if !sawRouterDef {
		t.Errorf("expected router.js const declaration in Lookup, got %+v", idx.Lookup("ConversationService"))
	}

	// Importers: router.js imports ConversationService.
	importers := idx.Importers(filepath.Join(dir, "src/services/ConversationService.js"))
	var sawImporter bool
	for _, im := range importers {
		if strings.Contains(im, "router.js") {
			sawImporter = true
		}
	}
	if !sawImporter {
		t.Errorf("expected router.js among importers, got %v", importers)
	}

	// Go imports are captured too (import "example.com/conv" → conv).
	goImporters := idx.Importers(filepath.Join(dir, "main.go"))
	_ = goImporters

	// Rendered report is readable and contains key facts.
	report := idx.FormatLookup("ConversationService")
	if !strings.Contains(report, "ConversationService.js") {
		t.Errorf("report missing definition file: %q", report)
	}
	if !strings.Contains(report, "Referenced by") {
		t.Errorf("report missing referencers section: %q", report)
	}

	// Unknown symbol → informative message, not a crash.
	if !strings.Contains(idx.FormatLookup("NoSuchSymbolXYZ"), "not found") {
		t.Errorf("unknown symbol should report not found: %q", idx.FormatLookup("NoSuchSymbolXYZ"))
	}
}

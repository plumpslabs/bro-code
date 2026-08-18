package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractGoSymbols(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "main.go")
	content := `package main

type User struct {
	Name string
}

type Greeter interface {
	Greet() string
}

func NewUser(name string) *User { return &User{Name: name} }

func (u *User) Greet() string { return "hi " + u.Name }

func main() {}
`
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	syms, err := ExtractSymbols(f)
	if err != nil {
		t.Fatalf("ExtractSymbols: %v", err)
	}

	names := map[string]string{}
	for _, s := range syms {
		names[s.Name] = s.Kind
	}
	for want, kind := range map[string]string{
		"User":    "struct",
		"Greeter": "interface",
		"NewUser": "func",
		"Greet":   "method",
		"main":    "func",
	} {
		if names[want] != kind {
			t.Errorf("symbol %s = %q, want %q (all: %v)", want, names[want], kind, names)
		}
	}
}

func TestExtractJSSymbols(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "service.js")
	content := `const express = require('express');

class ConversationService {
  constructor() { this.x = 1; }

  async findAll(filter) { return []; }

  static create() { return new ConversationService(); }
}

function helper(a, b) { return a + b; }

module.exports = ConversationService;
`
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	syms, err := ExtractSymbols(f)
	if err != nil {
		t.Fatalf("ExtractSymbols: %v", err)
	}

	names := map[string]string{}
	for _, s := range syms {
		names[s.Name] = s.Kind
	}
	if names["ConversationService"] != "class" {
		t.Errorf("expected ConversationService class, got %v", names)
	}
	if names["findAll"] == "" {
		t.Errorf("expected findAll method, got %v", names)
	}
	if names["helper"] != "func" {
		t.Errorf("expected helper func, got %v", names)
	}
	if names["ConversationService"] != "class" {
		t.Errorf("expected module.exports target, got %v", names)
	}
}

func TestExtractWebFrameworkSymbols(t *testing.T) {
	dir := t.TempDir()

	vueFile := filepath.Join(dir, "Button.vue")
	vueContent := `<script setup>
function handleClick() {}
const calculateTotal = () => 100;
</script>
<template><button @click="handleClick">Click</button></template>`
	if err := os.WriteFile(vueFile, []byte(vueContent), 0644); err != nil {
		t.Fatal(err)
	}

	prismaFile := filepath.Join(dir, "schema.prisma")
	prismaContent := `model User {
  id    Int    @id @default(autoincrement())
  email String @unique
}

enum Role {
  USER
  ADMIN
}`
	if err := os.WriteFile(prismaFile, []byte(prismaContent), 0644); err != nil {
		t.Fatal(err)
	}

	vueSyms, err := ExtractSymbols(vueFile)
	if err != nil {
		t.Fatalf("ExtractSymbols vue: %v", err)
	}
	vueMap := map[string]string{}
	for _, s := range vueSyms {
		vueMap[s.Name] = s.Kind
	}
	if vueMap["handleClick"] != "func" {
		t.Errorf("expected handleClick func in Vue, got %v", vueMap)
	}
	if vueMap["calculateTotal"] != "func" {
		t.Errorf("expected calculateTotal func in Vue, got %v", vueMap)
	}

	prismaSyms, err := ExtractSymbols(prismaFile)
	if err != nil {
		t.Fatalf("ExtractSymbols prisma: %v", err)
	}
	prismaMap := map[string]string{}
	for _, s := range prismaSyms {
		prismaMap[s.Name] = s.Kind
	}
	if prismaMap["User"] != "model" {
		t.Errorf("expected User model in Prisma, got %v", prismaMap)
	}
	if prismaMap["Role"] != "enum" {
		t.Errorf("expected Role enum in Prisma, got %v", prismaMap)
	}
}

func TestBM25RanksRelevantFileFirst(t *testing.T) {
	docs := []Document{
		{ID: "src/payment/gateway.js", Title: "gateway.js", Body: "handles payment gateway integration with midtrans and stripe billing"},
		{ID: "src/auth/login.js", Title: "login.js", Body: "user login and jwt token authentication"},
		{ID: "src/chat/message.js", Title: "message.js", Body: "chat message delivery websocket realtime"},
	}
	idx := NewBM25(docs)
	results := idx.Search("payment gateway integration", 3)
	if len(results) == 0 {
		t.Fatal("no results")
	}
	if !strings.Contains(results[0].Doc.ID, "payment") {
		t.Errorf("top result = %s, want payment/gateway.js", results[0].Doc.ID)
	}
}

func TestIndexDirSkipsNodeModules(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "src", "a.js"), []byte("const a = 1;"), 0644)
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "big.js"), []byte("const b = 2;"), 0644)

	docs, err := IndexDir(dir)
	if err != nil {
		t.Fatalf("IndexDir: %v", err)
	}
	found := false
	for _, d := range docs {
		if strings.Contains(d.ID, "node_modules") {
			found = true
		}
	}
	if found {
		t.Error("IndexDir indexed node_modules files")
	}
	if len(docs) == 0 {
		t.Error("IndexDir found nothing in src")
	}
}

func TestBuildProjectContext(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "crm_sales_backend", "src", "services"), 0755)
	os.MkdirAll(filepath.Join(dir, "crm-widget"), 0755)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Rules\n- never use yarn\n"), 0644)
	// Provider-specific instruction files must NOT be injected.
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Claude-only rules"), 0644)
	os.WriteFile(filepath.Join(dir, "GEMINI.md"), []byte("Gemini-only rules"), 0644)
	os.WriteFile(filepath.Join(dir, ".cursorrules"), []byte("Cursor rules"), 0644)
	os.WriteFile(filepath.Join(dir, "brocode.md"), []byte("Legacy brocode.md"), 0644)
	os.MkdirAll(filepath.Join(dir, ".brocode"), 0755)
	os.WriteFile(filepath.Join(dir, ".brocode", "CLAUDE.md"), []byte("BroCode CLAUDE.md"), 0644)

	pc := BuildProjectContext(dir)
	if !strings.Contains(pc.Tree, "crm_sales_backend") {
		t.Errorf("tree missing crm_sales_backend:\n%s", pc.Tree)
	}
	if !strings.Contains(pc.Tree, "src/") {
		t.Errorf("tree missing level-2 src/:\n%s", pc.Tree)
	}
	if !strings.Contains(pc.Docs, "AGENTS.md") || !strings.Contains(pc.Docs, "never use yarn") {
		t.Errorf("docs missing AGENTS.md content: %q", pc.Docs)
	}
	for _, banned := range []string{"CLAUDE.md", "GEMINI.md", ".cursorrules", "Claude-only", "Gemini-only", "Cursor rules", "Legacy brocode.md", "BroCode CLAUDE.md"} {
		if strings.Contains(pc.Docs, banned) {
			t.Errorf("docs must NOT contain %q (provider-specific config leaked):\n%s", banned, pc.Docs)
		}
	}
	if !strings.Contains(pc.String(), "KEY FILES: package.json") {
		t.Errorf("String missing KEY FILES: %q", pc.String())
	}
}

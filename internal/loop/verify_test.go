package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture creates the given files under a temp dir and chdirs into it.
func writeFixture(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

func TestPlanVerificationGo(t *testing.T) {
	writeFixture(t, map[string]string{"go.mod": "module test\n"})
	cmds := planVerification()
	if len(cmds) != 3 {
		t.Fatalf("expected 3 go checks, got %v", cmds)
	}
	if cmds[0].name != "go" || strings.Join(cmds[0].args, " ") != "build ./..." {
		t.Errorf("unexpected first check: %+v", cmds[0])
	}
	if cmds[1].args[0] != "vet" || cmds[2].args[0] != "test" {
		t.Errorf("expected vet then test, got %+v", cmds)
	}
}

func TestPlanVerificationJS(t *testing.T) {
	// No lockfile, no tsconfig, no scripts → nothing runs (no false failures).
	writeFixture(t, map[string]string{"package.json": "{\"name\":\"x\"}"})
	if cmds := planVerification(); len(cmds) != 0 {
		t.Errorf("bare package.json should not plan checks, got %v", cmds)
	}

	// typecheck + lint scripts → npm run typecheck, npm run lint.
	writeFixture(t, map[string]string{
		"package.json": `{"scripts":{"typecheck":"tsc --noEmit","lint":"eslint ."}}`,
	})
	cmds := planVerification()
	if len(cmds) != 2 {
		t.Fatalf("expected typecheck+lint, got %v", cmds)
	}
	if cmds[0].name != "npm" || strings.Join(cmds[0].args, " ") != "run typecheck" {
		t.Errorf("unexpected typecheck cmd: %+v", cmds[0])
	}
	if cmds[1].name != "npm" || strings.Join(cmds[1].args, " ") != "run lint" {
		t.Errorf("unexpected lint cmd: %+v", cmds[1])
	}

	// bun project with tsconfig and local tsc → bunx tsc --noEmit --skipLibCheck.
	writeFixture(t, map[string]string{
		"bun.lock":              "",
		"tsconfig.json":         "{}",
		"node_modules/.bin/tsc": "#!/usr/bin/env node",
		"package.json":          "{\"name\":\"x\"}",
	})
	cmds = planVerification()
	if len(cmds) != 1 {
		t.Fatalf("expected single tsc fallback for bun, got %v", cmds)
	}
	if cmds[0].name != "bunx" || strings.Join(cmds[0].args, " ") != "tsc --noEmit --skipLibCheck" {
		t.Errorf("unexpected bun tsc cmd: %+v", cmds[0])
	}

	// npm with tsconfig but no local tsc → NO tsc fallback (avoids npx install).
	writeFixture(t, map[string]string{
		"tsconfig.json": "{}",
		"package.json":  "{\"name\":\"x\"}",
	})
	if cmds := planVerification(); len(cmds) != 0 {
		t.Errorf("missing local tsc must not plan a check, got %v", cmds)
	}
}

func TestPlanVerificationPythonRustJava(t *testing.T) {
	writeFixture(t, map[string]string{"pyproject.toml": "[project]\n"})
	cmds := planVerification()
	if len(cmds) != 1 || cmds[0].name != "python3" || strings.Join(cmds[0].args, " ") != "-m compileall -q ." {
		t.Errorf("unexpected python check: %+v", cmds)
	}

	writeFixture(t, map[string]string{"Cargo.toml": "[package]\n"})
	cmds = planVerification()
	if len(cmds) != 1 || cmds[0].name != "cargo" || strings.Join(cmds[0].args, " ") != "check --quiet" {
		t.Errorf("unexpected rust check: %+v", cmds)
	}

	// Maven WITHOUT the wrapper must not auto-run (would try to download deps).
	writeFixture(t, map[string]string{"pom.xml": "<project/>"})
	if cmds := planVerification(); len(cmds) != 0 {
		t.Errorf("bare pom.xml must not plan checks, got %v", cmds)
	}
	// With mvnw wrapper → compile.
	writeFixture(t, map[string]string{"pom.xml": "<project/>", "mvnw": "#!/bin/sh"})
	cmds = planVerification()
	if len(cmds) != 1 || cmds[0].name != "./mvnw" {
		t.Errorf("expected ./mvnw compile, got %+v", cmds)
	}
}

func TestPlanVerificationRuby(t *testing.T) {
	// Gemfile without spec/test → nothing (no false failures).
	writeFixture(t, map[string]string{"Gemfile": "source 'https://rubygems.org'\n"})
	if cmds := planVerification(); len(cmds) != 0 {
		t.Errorf("Gemfile without tests must plan nothing, got %v", cmds)
	}
	// Gemfile + spec/ → bundle exec rspec.
	writeFixture(t, map[string]string{"Gemfile": "", "spec/foo_spec.rb": "RSpec.describe 'x' do\nend\n"})
	cmds := planVerification()
	if len(cmds) != 1 || cmds[0].name != "bundle" || strings.Join(cmds[0].args, " ") != "exec rspec" {
		t.Errorf("unexpected ruby rspec check: %+v", cmds)
	}
	// Gemfile + test/ only → bundle exec rake test (minitest project).
	writeFixture(t, map[string]string{"Gemfile": "", "test/foo_test.rb": "require 'minitest/autorun'\n"})
	cmds = planVerification()
	if len(cmds) != 1 || cmds[0].name != "bundle" || strings.Join(cmds[0].args, " ") != "exec rake test" {
		t.Errorf("unexpected ruby minitest check: %+v", cmds)
	}
}

func TestPlanVerificationPHP(t *testing.T) {
	// Bare composer.json (deps never installed) → nothing, not a false failure.
	writeFixture(t, map[string]string{"composer.json": "{}"})
	if cmds := planVerification(); len(cmds) != 0 {
		t.Errorf("uninstalled composer.json must plan nothing, got %v", cmds)
	}
	// composer.lock + vendor → composer validate (+ phpunit when configured+installed).
	writeFixture(t, map[string]string{
		"composer.json":      "{}",
		"composer.lock":      "{}",
		"phpunit.xml":        "<phpunit/>",
		"vendor/bin/phpunit": "#!/usr/bin/env php",
	})
	cmds := planVerification()
	if len(cmds) != 2 {
		t.Fatalf("expected composer validate + phpunit, got %v", cmds)
	}
	if cmds[0].name != "composer" || strings.Join(cmds[0].args, " ") != "validate --no-check-publish" {
		t.Errorf("unexpected composer cmd: %+v", cmds[0])
	}
	if cmds[1].name != "vendor/bin/phpunit" {
		t.Errorf("expected vendor/bin/phpunit, got %+v", cmds[1])
	}
	// phpunit.xml configured but vendor NOT installed → no phpunit step.
	writeFixture(t, map[string]string{"composer.json": "{}", "composer.lock": "{}", "phpunit.xml": "<phpunit/>"})
	cmds = planVerification()
	if len(cmds) != 1 || cmds[0].name != "composer" {
		t.Errorf("uninstalled phpunit must not be planned, got %v", cmds)
	}
}

func TestPlanVerificationDotNet(t *testing.T) {
	// .csproj → dotnet build.
	writeFixture(t, map[string]string{"App.csproj": "<Project/>"})
	cmds := planVerification()
	if len(cmds) != 1 || cmds[0].name != "dotnet" || strings.Join(cmds[0].args, " ") != "build --nologo" {
		t.Errorf("unexpected dotnet check: %+v", cmds)
	}
	// Test project (*Tests.csproj) → build + test --no-build.
	writeFixture(t, map[string]string{"App.Tests.csproj": "<Project/>"})
	cmds = planVerification()
	if len(cmds) != 2 || cmds[1].args[0] != "test" {
		t.Errorf("expected dotnet build + test, got %v", cmds)
	}
	// .sln also detected.
	writeFixture(t, map[string]string{"App.sln": ""})
	cmds = planVerification()
	if len(cmds) != 1 || cmds[0].name != "dotnet" {
		t.Errorf("expected dotnet build for sln, got %v", cmds)
	}
}

func TestPlanVerificationNoConfig(t *testing.T) {
	writeFixture(t, map[string]string{"README.md": "hi"})
	if cmds := planVerification(); len(cmds) != 0 {
		t.Errorf("no build config must plan nothing, got %v", cmds)
	}
}

func TestDetectJSManager(t *testing.T) {
	writeFixture(t, map[string]string{"package.json": "{}"})
	if got := detectJSManagerIn("."); got != "npm" {
		t.Errorf("default manager = %q, want npm", got)
	}
	writeFixture(t, map[string]string{"bun.lockb": ""})
	if got := detectJSManagerIn("."); got != "bun" {
		t.Errorf("bun.lockb manager = %q, want bun", got)
	}
	writeFixture(t, map[string]string{"pnpm-lock.yaml": ""})
	if got := detectJSManagerIn("."); got != "pnpm" {
		t.Errorf("pnpm manager = %q, want pnpm", got)
	}
	writeFixture(t, map[string]string{"yarn.lock": ""})
	if got := detectJSManagerIn("."); got != "yarn" {
		t.Errorf("yarn manager = %q, want yarn", got)
	}
}

func TestScriptExists(t *testing.T) {
	writeFixture(t, map[string]string{
		"package.json": `{"scripts":{"typecheck":"tsc --noEmit","empty":""}}`,
	})
	if !scriptExistsIn(".", "typecheck") {
		t.Error("expected typecheck script to exist")
	}
	if scriptExistsIn(".", "empty") {
		t.Error("empty script must not count as existing")
	}
	if scriptExistsIn(".", "nope") {
		t.Error("missing script must not exist")
	}
}

func TestDescribeVerification(t *testing.T) {
	writeFixture(t, map[string]string{"go.mod": "module t\n"})
	desc := describeVerification()
	if !strings.Contains(desc, "go build ./...") || !strings.Contains(desc, "go vet ./...") {
		t.Errorf("unexpected description: %q", desc)
	}
}

func TestPlanVerificationMonorepoSubdir(t *testing.T) {
	// Monorepo: no root config, but a package one level down (apps/api) has
	// go.mod → verification must plan checks that run IN that subdir.
	writeFixture(t, map[string]string{
		"apps/api/go.mod": "module api\n",
		"README.md":       "monorepo",
	})
	cmds := planVerification()
	if len(cmds) == 0 {
		t.Fatal("monorepo subdir config must be detected")
	}
	if cmds[0].dir != "apps/api" {
		t.Errorf("expected checks to run in apps/api, got dir=%q", cmds[0].dir)
	}
	if cmds[0].name != "go" {
		t.Errorf("expected go checks, got %q", cmds[0].name)
	}
}

func TestPlanVerificationMonorepoJS(t *testing.T) {
	// JS monorepo: root package.json with workspaces → root checks (typecheck
	// script) are preferred, no subdir scan needed.
	writeFixture(t, map[string]string{
		"package.json": `{"workspaces":["packages/*"],"scripts":{"typecheck":"tsc --noEmit"}}`,
	})
	cmds := planVerification()
	if len(cmds) == 0 {
		t.Fatal("root package.json must be detected")
	}
	if cmds[0].dir != "." {
		t.Errorf("root JS checks should run in cwd, got dir=%q", cmds[0].dir)
	}
}

func TestPlanVerificationSkipsHeavyDirs(t *testing.T) {
	// node_modules with a package.json must not be mistaken for a project.
	writeFixture(t, map[string]string{"node_modules/foo/package.json": "{}"})
	if cmds := planVerification(); len(cmds) != 0 {
		t.Errorf("node_modules must be skipped, got %+v", cmds)
	}
}

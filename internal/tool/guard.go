package tool

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Native file guards — a hard, deterministic layer that blocks the agent from
// touching things it must never read or edit, regardless of what the model
// asks for:
//
//   - Sensitive files (.env, .env.*, private keys, credentials) — never read,
//     never written, never returned. Secrets must not enter the LLM context.
//   - Heavy/vendored directories (node_modules, vendor, target, __pycache__,
//     dist, build, .git, ...) — reading them burns tokens and drowns the
//     agent in noise; they are skipped everywhere.
//
// These are enforced natively in every file tool (read, write, edit, list,
// grep, glob, symbols, bash) so the protection holds even when a free model
// or an aggressive agent loop asks for them directly.

// heavyDirNames are directories the agent must never read into context:
// dependencies, build output, VCS metadata, language-specific caches.
var heavyDirNames = map[string]bool{
	"node_modules": true, "bower_components": true, "vendor": true,
	"dist": true, "build": true, "out": true, "bin": true, "obj": true,
	".git": true, ".next": true, ".nuxt": true, "coverage": true,
	".cache": true, ".turbo": true, "target": true, "__pycache__": true,
	".venv": true, "venv": true, "Pods": true, ".gradle": true,
	".pytest_cache": true, ".mypy_cache": true, ".tox": true,
	".terraform": true, ".serverless": true, ".parcel-cache": true,
	".yarn": true, ".pnpm-store": true, ".svn": true, ".hg": true,
}

// sensitiveFileNames are files whose contents must never enter the LLM
// context: environment/secret files and private keys.
var sensitiveFileNames = map[string]bool{
	".env": true, ".env.local": true, ".env.production": true,
	".env.development": true, ".env.test": true, ".env.staging": true,
	".env.example": true, ".env.sample": true, ".env.backup": true,
	".npmrc": true, ".pypirc": true, ".netrc": true, ".pgpass": true,
	".htpasswd": true, "id_rsa": true, "id_ed25519": true, "id_ecdsa": true,
	"id_dsa": true, ".dockercfg": true, "credentials.json": true,
	"service-account.json": true, "secrets.yaml": true, "secrets.yml": true,
	".git-credentials": true, ".dockerconfigjson": true,
}

// sensitiveExts are file extensions that hold credentials or keys.
var sensitiveExts = []string{
	".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".ppk",
	".gpg", ".asc", ".kdbx", ".ovpn", ".mobileprovision",
}

// IsHeavyDir reports whether a directory name is a dependency/build/VCS dir
// the agent must not read. Exported so glob walking and the repo map reuse
// the same rule.
func IsHeavyDir(name string) bool { return heavyDirNames[name] }

// GuardSensitivePath blocks reading a file whose name or extension marks it
// as sensitive (secrets, keys, env). Returns a hard error the model sees, so
// it learns the path is off-limits instead of silently getting nothing.
func GuardSensitivePath(path string) error {
	base := filepath.Base(path)
	if sensitiveFileNames[base] {
		return fmt.Errorf("⛔ blocked: %q is a sensitive file (secrets/credentials) and BroCode never reads it. If you need to know which env vars exist, read .env.example docs or ask the user.", base)
	}
	lower := strings.ToLower(base)
	for _, ext := range sensitiveExts {
		if strings.HasSuffix(lower, ext) {
			return fmt.Errorf("⛔ blocked: %q is a private key/credential file and BroCode never reads it.", base)
		}
	}
	// Catch .env.production-style variants and any .env* prefix.
	if strings.HasPrefix(lower, ".env") {
		return fmt.Errorf("⛔ blocked: %q is an environment file (secrets) and BroCode never reads it.", base)
	}
	return nil
}

// GuardHeavyPath blocks reading/writing inside a heavy dir (node_modules,
// vendor, target, ...). listPath may be true when the call is a listing that
// legitimately shows the directory name but must not descend into it.
func GuardHeavyPath(path string) error {
	clean := filepath.ToSlash(filepath.Clean(path))
	for part := range heavyDirNames {
		if hasPathPart(clean, part) {
			return fmt.Errorf("⛔ blocked: %q is inside %q (dependencies/build output) and BroCode does not read or edit it — the project's real code lives elsewhere.", path, part)
		}
	}
	return nil
}

// hasPathPart reports whether any slash-separated segment of path equals part.
func hasPathPart(path, part string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == part {
			return true
		}
	}
	return false
}

// GuardFile protects a file path against both sensitive files and heavy dirs.
func GuardFile(path string) error {
	if err := GuardHeavyPath(path); err != nil {
		return err
	}
	return GuardSensitivePath(path)
}

// GuardSensitiveCommand blocks bash commands that clearly read or dump a
// sensitive file (cat/less/head/tail .env, etc.). Not a security boundary —
// the permission gate is — but a hard native habit: the agent never even
// tries. Returns "" when the command is fine.
func GuardSensitiveCommand(cmd string) string {
	lower := strings.ToLower(strings.TrimSpace(cmd))
	for _, name := range []string{
		".env", ".npmrc", ".pypirc", ".netrc", ".pgpass",
		"id_rsa", "id_ed25519", "credentials.json", "secrets.yaml",
		"service-account.json", ".git-credentials",
	} {
		// Match a token boundary so "grep .env.example docs" is allowed but
		// "cat .env" / "cat .env.production" is blocked.
		if strings.Contains(lower, name) {
			// Only block when the token is used as a file argument of a
			// read/dump command, not inside a longer safe reference.
			fields := strings.Fields(lower)
			for _, f := range fields {
				if strings.HasPrefix(f, name) || strings.HasPrefix(name, f) {
					if isDumpVerb(fields) {
						return fmt.Sprintf("⛔ blocked: reading %q (sensitive file) via bash is not allowed.", name)
					}
				}
			}
		}
	}
	return ""
}

// isDumpVerb reports whether the command (token list) dumps file contents —
// the verb may sit behind wrappers like sudo / env / V=1.
func isDumpVerb(fields []string) bool {
	if len(fields) == 0 {
		return false
	}
	for _, w := range fields {
		switch w {
		case "sudo", "env", "-u", "root", "bash", "sh", "zsh", "nohup":
			continue // wrapper tokens — keep scanning
		}
		for _, v := range []string{"cat", "less", "more", "head", "tail", "vi", "vim", "nano", "open", "type"} {
			if w == v || strings.HasPrefix(w, v+" ") {
				return true
			}
		}
		// Not a wrapper and not a dump verb: stop (first real word decides).
		return false
	}
	return false
}

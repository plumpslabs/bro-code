package tool

import (
	"path/filepath"
	"strings"
)

// GateDecision is the outcome of gating a command before execution.
type GateDecision int

const (
	// GateAllow runs the command silently.
	GateAllow GateDecision = iota
	// GateAsk pauses for the user (permission modal).
	GateAsk
	// GateDeny blocks the command outright — never executed regardless of the
	// user's choice (the genuinely catastrophic cases, e.g. rm -rf /).
	GateDeny
)

// gatedKeys are first-word command rules that always require confirmation.
// Every risky/destructive command gets a gate, not just out-of-repo ones.
var gatedKeys = map[string]bool{
	// destructive filesystem
	"rm": true, "rmdir": true, "unlink": true, "shred": true,
	"mv": true, "cp": true, "truncate": true, "dd": true, "install": true,
	// privilege escalation / system control
	"sudo": true, "su": true,
	"shutdown": true, "reboot": true, "halt": true, "poweroff": true,
	"mkfs": true, "fdisk": true, "parted": true, "mount": true, "umount": true,
	// process killing
	"kill": true, "pkill": true, "killall": true, "killall5": true, "xkill": true,
	// permission changes
	"chmod": true, "chown": true, "chgrp": true,
	// remote code execution from a pipe
	"curl": true, "wget": true,
}

// dangerousRm reports whether the command contains an `rm` with a recursive
// flag targeting a root/home path (rm -rf /, sudo rm -rf ~, env rm -rf /*, …)
// — the data-loss cases that must NEVER run, regardless of user choice or the
// session allow-list.
func dangerousRm(cmd string) bool {
	fields := strings.Fields(cmd)
	for i := 0; i < len(fields); i++ {
		if fields[i] != "rm" {
			continue
		}
		var rec bool
		var target string
		for j := i + 1; j < len(fields); j++ {
			f := fields[j]
			if strings.HasPrefix(f, "-") {
				if strings.Contains(f, "r") || strings.Contains(f, "f") {
					rec = true
				}
				continue
			}
			target = f
			break
		}
		if rec && (target == "/" || strings.HasPrefix(target, "/*") || target == "~" || strings.HasPrefix(target, "~/")) {
			return true
		}
	}
	return false
}

// hasForceFlag reports whether a git push flag set contains a force flag
// (--force, --force-with-lease, -f, or combined short flags like -uf).
func hasForceFlag(rest string) bool {
	for _, tok := range strings.Fields(rest) {
		if tok == "-f" || strings.HasPrefix(tok, "--force") {
			return true
		}
		if strings.HasPrefix(tok, "-") && !strings.HasPrefix(tok, "--") && strings.Contains(tok, "f") {
			return true // -uf, -fv, …
		}
	}
	return false
}

// AllowKey returns the session rule key for a command, or "" when the command
// is not gated at all (so "always allow" only ever applies to gated rules).
// The key is coarse on purpose (first word + git subcommand): "rm" covers
// every rm invocation, "git push" covers plain pushes, "git push --force"
// is a distinct key so force-pushing stays gated even after plain pushes are
// allowed.
func AllowKey(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	// Strip leading env assignments (FOO=bar cmd …) so the first real word is
	// inspected.
	for strings.Contains(cmd, "=") {
		head, _, _ := strings.Cut(cmd, " ")
		if strings.Contains(head, "=") && !strings.HasPrefix(head, "-") {
			cmd = strings.TrimSpace(strings.TrimPrefix(cmd, head))
			continue
		}
		break
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	first := strings.ToLower(fields[0])
	switch first {
	case "git":
		if len(fields) < 2 {
			return ""
		}
		sub := strings.ToLower(fields[1])
		rest := strings.Join(fields[2:], " ")
		switch {
		case sub == "push" && hasForceFlag(rest):
			return "git push --force"
		case sub == "commit":
			return "git commit"
		case sub == "reset" || sub == "clean" || sub == "stash":
			return "git " + sub
		case (sub == "checkout" || sub == "restore") && strings.Contains(rest, "--"):
			return "git " + sub
		}
		return "" // plain git operations are safe
	case "curl", "wget":
		// Only dangerous when piped straight into a shell.
		if strings.Contains(cmd, "| sh") || strings.Contains(cmd, "| bash") ||
			strings.Contains(cmd, "| zsh") || strings.Contains(cmd, "| fish") {
			return first + " | shell"
		}
		return ""
	case "cd", "pushd":
		return first
	case "npm", "yarn", "pnpm", "bun":
		if len(fields) >= 3 {
			sub := strings.ToLower(fields[1])
			if sub == "install" || sub == "i" || sub == "add" {
				if pkg := firstNonFlag(fields[2:]); pkg != "" {
					return first + " " + sub + " " + pkg
				}
			}
		}
		return ""
	case "pip", "pip3":
		if len(fields) >= 3 && strings.ToLower(fields[1]) == "install" {
			if pkg := firstNonFlag(fields[2:]); pkg != "" {
				return first + " install " + pkg
			}
		}
		return ""
	case "go":
		if len(fields) >= 3 && strings.ToLower(fields[1]) == "get" {
			if pkg := firstNonFlag(fields[2:]); pkg != "" {
				return "go get " + pkg
			}
		}
		return ""
	case "cargo":
		if len(fields) >= 3 && strings.ToLower(fields[1]) == "add" {
			if pkg := firstNonFlag(fields[2:]); pkg != "" {
				return "cargo add " + pkg
			}
		}
		return ""
	default:
		if gatedKeys[first] {
			return first
		}
		return ""
	}
}

// firstNonFlag returns the first argument that does not start with '-' or '--'.
func firstNonFlag(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// dangerousDatabaseDrop reports whether a command drops or wipes a database/table.
func dangerousDatabaseDrop(cmd string) bool {
	lower := strings.ToLower(cmd)
	if strings.Contains(lower, "drop database") ||
		strings.Contains(lower, "drop table") ||
		strings.Contains(lower, "truncate table") ||
		strings.Contains(lower, "prisma migrate reset") ||
		strings.Contains(lower, "db:drop") ||
		strings.Contains(lower, "db:reset") ||
		strings.Contains(lower, "schema:drop") {
		return true
	}
	return false
}

// GateCommand decides whether cmd may run, needs confirmation, or is blocked.
// repoRoot anchors the out-of-repo escape check for cd/pushd; allow is the
// session allow-list (keys from AllowKey) — matching keys skip the gate.
func GateCommand(cmd, repoRoot string, allow map[string]bool) GateDecision {
	// rm -rf /-class commands are hard-blocked no matter what (not even an
	// explicit "always allow" for that session overrides it).
	if dangerousRm(strings.ToLower(cmd)) {
		return GateDeny
	}

	// Destructive database drop/reset commands must always ask confirmation
	if dangerousDatabaseDrop(cmd) {
		return GateAsk
	}

	key := AllowKey(cmd)
	if key == "" {
		return GateAllow
	}
	if allow != nil {
		if allow[key] {
			return GateAllow
		}
		// Pattern rules: an allow key containing "*" (e.g. "npm *", "git *")
		// matches the whole command via filepath.Match — enterprise-style
		// command scoping (Claude's Bash(npm *) style).
		for rule := range allow {
			if strings.Contains(rule, "*") {
				if ok, _ := filepath.Match(rule, strings.ToLower(strings.TrimSpace(cmd))); ok {
					return GateAllow
				}
			}
		}
	}

	// cd/pushd: escaping the repo root always asks (the agent may wander out
	// of the project the user opened).
	if key == "cd" || key == "pushd" {
		target := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(cmd), key))
		for _, sep := range []string{"&&", "||", ";", "|"} {
			if idx := strings.Index(target, sep); idx >= 0 {
				target = strings.TrimSpace(target[:idx])
			}
		}
		target = strings.Trim(target, " \"'`;")
		if target == "" || target == "~" || target == "$home" || strings.HasPrefix(target, "~/") {
			return GateAsk // home or missing target — the classic wander
		}
		if strings.HasPrefix(target, "-") || target == "." || target == "./" || target == "/workspace" {
			return GateAllow // cd - (previous dir), cd ., or /workspace container root alias
		}
		abs := target
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(repoRoot, abs)
		}
		abs = filepath.Clean(abs)
		root := filepath.Clean(repoRoot)
		if root == "" {
			root = "/"
		}
		absLower := strings.ToLower(abs)
		rootLower := strings.ToLower(root)
		sep := string(filepath.Separator)
		if absLower == rootLower || strings.HasPrefix(absLower, strings.TrimSuffix(rootLower, sep)+sep) {
			return GateAllow // still inside the repo
		}
		return GateAsk
	}

	return GateAsk
}

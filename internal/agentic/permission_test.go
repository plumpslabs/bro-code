package agentic

import "testing"

func TestGateCommandGlobRules(t *testing.T) {
	// allow["git push *"] scopes the allowance to git pushes (audit P3 —
	// enterprise-style Bash(rule) command scoping).
	allow := map[string]bool{"git push *": true}

	// A gated force-push matches the glob rule → allowed.
	if d := GateCommand("git push --force origin main", "/repo", allow); d != GateAllow {
		t.Fatalf("glob 'git push *' must allow force push, got %v", d)
	}
	// A gated non-matching command stays gated.
	if d := GateCommand("rm -rf build", "/repo", allow); d != GateAsk {
		t.Fatalf("rm must stay gated under a git-push-only rule, got %v", d)
	}
	// The hard deny (rm -rf /) wins over any rule.
	if d := GateCommand("rm -rf /", "/repo", allow); d != GateDeny {
		t.Fatalf("hard deny must beat allow rules, got %v", d)
	}

	// An exact allow key still works alongside glob rules.
	allow2 := map[string]bool{"sudo": true, "git *": true}
	if d := GateCommand("sudo apt update", "/repo", allow2); d != GateAllow {
		t.Fatalf("exact key must allow sudo, got %v", d)
	}
	if d := GateCommand("git push --force origin main", "/repo", allow2); d != GateAllow {
		t.Fatalf("glob 'git *' must allow gated git ops, got %v", d)
	}
	if d := GateCommand("rm -rf build", "/repo", allow2); d != GateAsk {
		t.Fatalf("rm must stay gated, got %v", d)
	}
}

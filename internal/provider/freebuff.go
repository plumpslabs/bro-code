package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// FreeBuff is BroCode's native integration for the FreeBuff free gateway
// (https://freebuff.com). Verified 2026-08: FreeBuff's backend is NOT a plain
// OpenAI-compatible endpoint — the public host (freebuff.llm.pm/v1) bounces
// bearer-token requests back to an auth page, and the real backend
// (codebuff.com /api/v1/agent-runs + /api/v1/chat/completions) requires a run
// session lifecycle, a free-tier waiting-room queue, and client fingerprint
// headers. The maintained community bridge Freebuff2API (Go, MIT) implements
// that protocol and exposes a clean OpenAI-compatible proxy on localhost:8080
// with a model registry sourced from the official CodebuffAI free-agents.ts.
//
// BroCode therefore integrates FreeBuff through that local proxy: the provider
// auto-detects when the proxy is alive AND the FreeBuff CLI is logged in (the
// account token in its credentials file is the "is this user actually a
// FreeBuff user" signal). The token itself is never sent anywhere by BroCode.

// FreeBuffDefaultBaseURL is the OpenAI-compatible endpoint of the local
// Freebuff2API proxy (its documented default listen address).
const FreeBuffDefaultBaseURL = "http://localhost:8080/v1"

// FreeBuffModels are the free models declared by the official CodebuffAI
// source tree (freebuff-model-ids.ts + model-config.ts, 2026-08):
//
//	mimo/mimo-v2.5 and mimo/mimo-v2.5-pro (the "mimo/" prefix is part of the
//	wire ID — a bare "mimo-v2.5" is rejected as model_not_found),
//	minimax/minimax-m3,
//	google/gemini-2.5-flash-lite (the file-picker free agent).
//
// This is only the picker baseline when the proxy is unreachable — when the
// local proxy responds, its live /v1/models is authoritative and replaces
// this list so models the proxy does not actually serve are never offered.
var FreeBuffModels = []string{
	"minimax/minimax-m3",
	"mimo/mimo-v2.5",
	"mimo/mimo-v2.5-pro",
	"google/gemini-2.5-flash-lite",
}

// freeBuffCredentialsFile is the FreeBuff CLI's local credential store
// (the same file the official CLI writes — BroCode never touches it).
const freeBuffCredentialsFile = ".config/manicode/credentials.json"

// freeBuffCredentialsPathOverride lets tests point the loader at a temp file
// instead of the real ~/.config/manicode/credentials.json. Empty = compute
// from the user's home directory.
var freeBuffCredentialsPathOverride = ""

// FreeBuffTokenPath returns the absolute path of the FreeBuff CLI credential
// file, or "" when the home directory cannot be resolved.
func FreeBuffTokenPath() string {
	if freeBuffCredentialsPathOverride != "" {
		return freeBuffCredentialsPathOverride
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, freeBuffCredentialsFile)
}

// LoadFreeBuffToken reads the account auth token from the FreeBuff CLI
// credentials file. The file maps profile names to objects containing an
// authToken field (e.g. {"default": {"id": ..., "authToken": "..."}}); the
// first profile that carries a non-empty token wins. Returns "" when the
// file is missing, malformed, or has no token — callers treat that as
// "FreeBuff not logged in" and simply omit the provider.
func LoadFreeBuffToken() string {
	path := FreeBuffTokenPath()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var profiles map[string]map[string]any
	if err := json.Unmarshal(data, &profiles); err != nil {
		return ""
	}
	// Deterministic order: iterate profile names sorted so the result does
	// not depend on Go map iteration order.
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if tok, _ := profiles[name]["authToken"].(string); tok != "" {
			return tok
		}
	}
	return ""
}

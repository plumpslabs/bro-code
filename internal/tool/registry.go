package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/store"
)

// Shared HTTP clients: reusing one Transport per purpose avoids allocating a
// fresh connection pool + TLS state on every fetch_url/web_search call.
var (
	httpClientFetch  = &http.Client{Timeout: 15 * time.Second}
	httpClientSearch = &http.Client{Timeout: 20 * time.Second}
)

// progressKey is the context key for an optional TurnOutputHandler that
// long-running tools (subagent, swarm) can use to stream progress back to the
// engine loop / TUI. nil means no progress forwarding (default).
type progressKey struct{}

// WithProgress attaches a progress callback to the context so blocking tools
// like subagent/scout can forward interim updates without changing the Tool
// interface signature.
func WithProgress(ctx context.Context, cb func(state string, info string)) context.Context {
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, cb)
}

// ProgressFromContext extracts a progress callback, or nil if none set.
func ProgressFromContext(ctx context.Context) func(state string, info string) {
	cb, _ := ctx.Value(progressKey{}).(func(state string, info string))
	return cb
}

// turnFilesKey carries a shared, mutable slice of file paths touched during
// the current turn. Used to build co-read/edit edges (the "graph" in Smart
// Context Graph): files acted on together in one turn become neighbors.
type turnFilesKey struct{}

// WithTurnFiles attaches an initially-empty turn-file collector to ctx. The
// engine wraps each tool call with this so file tools can record co-occurrence.
func WithTurnFiles(ctx context.Context) context.Context {
	files := &[]string{}
	return context.WithValue(ctx, turnFilesKey{}, files)
}

// turnFilesFromContext returns the turn's shared file slice, or nil.
func turnFilesFromContext(ctx context.Context) *[]string {
	f, _ := ctx.Value(turnFilesKey{}).(*[]string)
	return f
}

// recordTurnFile appends a path to the current turn's co-occurrence slice.
func recordTurnFile(ctx context.Context, path string) {
	if f := turnFilesFromContext(ctx); f != nil && path != "" {
		*f = append(*f, path)
	}
}

// Tool represents an executable native tool.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any
	Execute(ctx context.Context, argsJSON string) (string, error)
}

// FileActionRequest describes a critical file mutation (create/delete) that
// needs the user's confirmation before it runs.
type FileActionRequest struct {
	Kind string // "create_file" | "delete_file"
	Path string
}

// FileActionDecision is the user's answer to a file-action confirmation.
type FileActionDecision struct {
	Allow  bool // false = discard (deny) the action
	Always bool // remember this path for the rest of the session
}

// Registry holds all registered tools plus the permission gate state.
type Registry struct {
	tools    map[string]Tool
	repoRoot string          // anchors the cd/pushd escape check
	allow    map[string]bool // session allow-list (keys from AllowKey)
	// allowExact remembers commands the user already approved this session. An
	// "Allow once" on command X therefore does NOT re-prompt when the model runs
	// the SAME command again — previously a re-run popped a fresh gated prompt
	// every time, which is exactly the "Allow once / Allow once" loop users hit.
	allowExact map[string]bool
	askFunc  func(context.Context, []AskQuestion) ([]AskResult, error)
	sandbox  *Sandbox // granular per-tool policy (.brocode/sandbox.json)

	// readOnly / readOnlyBash enforce a mode at the EXECUTOR level, not just
	// in the engine loop: mutating tools (and bash, for PLANNER) are hard
	// blocked here, so ANY execution path — the main loop, subagents, direct
	// Execute calls — cannot mutate in a read-only mode. A subagent spawned
	// from MINER/PLANNER inherits the policy via SubRegistry.
	readOnly     bool
	readOnlyBash bool

	// fileActionFunc asks the user before critical file mutations (create /
	// delete) via the input-bar confirm; fileAllow remembers "always allow"
	// paths for the rest of the session (keys "create_file:<path>").
	fileActionFunc func(context.Context, FileActionRequest) (FileActionDecision, error)
	fileAllow      map[string]bool

	// knowledgeStore is the Smart Context Graph backend. When set, read_file
	// updates it asynchronously and edit_file/write_file/delete_file invalidate
	// entries synchronously — so the engine gets smart warm-start hints without
	// re-scanning unchanged files.
	knowledgeStore *store.Store
}

// SetExecutionPolicy hard-enforces a read-only mode at the executor level.
// readOnly=true blocks write_file/edit_file/delete_file everywhere;
// readOnlyBash additionally blocks bash (PLANNER mode). Safe to call at any
// time; mutating tools return a clear error instead of executing.
func (r *Registry) SetExecutionPolicy(readOnly, readOnlyBash bool) {
	r.readOnly = readOnly
	r.readOnlyBash = readOnlyBash
}

// NewRegistry initializes default built-in tools.
func NewRegistry() *Registry {
	r := &Registry{tools: make(map[string]Tool)}
	r.Register(&ReadFileTool{})
	r.Register(&WriteFileTool{})
	r.Register(&EditFileTool{})
	r.Register(&EditSymbolTool{})
	r.Register(&DeleteFileTool{})
	r.Register(&ListDirTool{})
	r.Register(&GrepTool{})
	r.Register(&GlobTool{})
	r.Register(&BashTool{})
	r.Register(&AskUserTool{})
	r.Register(&FetchURLTool{})
	r.Register(&GitTool{})
	r.Register(&UndoTool{})
	r.Register(&WebSearchTool{})
	r.Register(&ReviewChangesTool{})
	r.Register(&SearchCodeTool{})
	r.Register(&MemoryTool{})
	r.Register(&ContextRecallTool{})
	r.Register(&RunTestsTool{})
	r.Register(&CodeOutlineTool{})
	r.Register(&CodeImpactTool{})
	r.Register(&RefactorClusterTool{})
	return r
}

// SetSearchEmbedder wires an OpenAI-compatible embeddings endpoint onto the
// registered search_code tool (BM25 stays the fallback when it is nil).
func (r *Registry) SetSearchEmbedder(e *search.Embedder) {
	for _, t := range r.tools {
		if sct, ok := t.(*SearchCodeTool); ok {
			sct.SetEmbedder(e)
			return
		}
	}
}

// SetRepoRoot anchors the out-of-repo escape check for cd/pushd gates and
// tells the bash tool where the project root is (so a container sandbox can
// mount it at /workspace).
func (r *Registry) SetRepoRoot(path string) {
	r.repoRoot = path
	if bt, ok := r.tools["bash"].(*BashTool); ok {
		bt.WorkDir = path
	}
}

// SetUserAskHandler wires the interactive ask modal so gated commands can
// request approval from the user. Without it (headless), gated commands run.
func (r *Registry) SetUserAskHandler(fn func(context.Context, []AskQuestion) ([]AskResult, error)) {
	r.askFunc = fn
	if rc, ok := r.tools["review_changes"].(*ReviewChangesTool); ok {
		rc.Ask = fn
	}
}

// SetFileActionHandler wires the input-bar confirmation for critical file
// mutations (create/delete file). Without it (headless), gated file actions
// run — mirroring the bash gate's unattended behavior.
func (r *Registry) SetFileActionHandler(fn func(context.Context, FileActionRequest) (FileActionDecision, error)) {
	r.fileActionFunc = fn
}

// SetMemoryStore wires the cross-session project memory into the memory tool.
// Nil leaves the tool reporting that memory is unavailable.
func (r *Registry) SetMemoryStore(st *memory.Store) {
	if mt, ok := r.tools["memory"].(*MemoryTool); ok {
		mt.Store = st
	}
}

// SetKnowledgeStore wires the Smart Context Graph backend into the read/edit
// Tools so they can update and invalidate knowledge entries. The same store
// backs the self-aware notes layer (context_recall), so it is wired here too.
// Nil disables both.
func (r *Registry) SetKnowledgeStore(st *store.Store) {
	r.knowledgeStore = st
	if crt, ok := r.tools["context_recall"].(*ContextRecallTool); ok {
		crt.Store = st
	}
	if rft, ok := r.tools["read_file"].(*ReadFileTool); ok {
		rft.knowledgeStore = st
	}
}

// SetSandbox applies a granular per-tool permission policy (from
// .brocode/sandbox.json). Nil or disabled sandboxes leave the default
// gate-only behavior untouched. When the sandbox enables the container
// sandbox, the bash tool is switched to run inside Docker.
func (r *Registry) SetSandbox(s *Sandbox) {
	r.sandbox = s
	if s != nil && s.Container != nil && s.Container.Enabled {
		if bt, ok := r.tools["bash"].(*BashTool); ok {
			bt.Container = s.Container
		}
	}
}

// Sandbox returns the active sandbox policy, or nil when none is configured.
func (r *Registry) Sandbox() *Sandbox {
	return r.sandbox
}

// GateAction decides whether a tool call may proceed. Only bash commands are
// gated (the proven legacy design): risky/destructive commands either run
// silently, ask the user for approval (Allow once / Always allow / Deny), or
// are hard-blocked. write_file/edit_file are not gated — the PLANNER mode
// guard already handles read-only enforcement.
func (r *Registry) GateAction(ctx context.Context, tc provider.ToolCall) (approved bool, reason string, err error) {
	// Read-only mode policy (executor-level enforcement): mutating tools and
	// bash are hard-blocked before any sandbox/gate logic. This backstops the
	// engine-loop guards so even a path that bypasses them cannot mutate.
	// lsp_fix/lsp_rename write to disk through the LSP client, so they are
	// blocked here too — read-only mode must be airtight.
	if r.readOnly && (tc.Name == "write_file" || tc.Name == "edit_file" || tc.Name == "edit_symbol" || tc.Name == "delete_file" || tc.Name == "lsp_fix" || tc.Name == "lsp_rename") {
		return false, fmt.Sprintf("⚠️ [READ-ONLY MODE]: Tool '%s' is disabled in PLANNER mode. You do NOT need to write any plan file to disk yourself. Simply output your plan directly as markdown text in your chat response and BroCode will automatically save it to .brocode/current_plan.md. Do NOT attempt to write to .agents/ or any other folders.", tc.Name), nil
	}
	if r.readOnlyBash && tc.Name == "bash" {
		return false, "⚠️ [READ-ONLY MODE]: Tool 'bash' is disabled in PLANNER mode (read-only architecture mode). Switch to BUILDER (Shift+Tab) to execute commands.", nil
	}

	// Sandbox policy first: a blocked tool is hard-denied, never prompted.
	if r.sandbox != nil {
		if reason := r.sandbox.CheckTool(tc.Name, tc.Arguments); reason != "" {
			return false, reason, nil
		}
	}

	var cmd string
	switch tc.Name {
	case "write_file":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return false, "invalid write_file arguments: " + err.Error(), nil
		}
		args.Path = resolvePath(args.Path)
		if err := GuardFile(args.Path); err != nil {
			return false, err.Error(), nil
		}
		return true, "", nil
	case "delete_file":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return false, "invalid delete_file arguments: " + err.Error(), nil
		}
		args.Path = resolvePath(args.Path)
		return r.gateFileAction(ctx, "delete_file", args.Path)
	case "bash":
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return false, "invalid bash arguments: " + err.Error(), nil
		}
		cmd = args.Command
	case "git":
		// Only the commit action is gated (it mutates the repo). Read-only
		// actions (status/diff/log/branch) always pass.
		var args struct {
			Action  string `json:"action"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return false, "invalid git arguments: " + err.Error(), nil
		}
		if args.Action != "commit" {
			return true, "", nil
		}
		cmd = "git commit"
	default:
		return true, "", nil
	}

	switch GateCommand(cmd, r.repoRoot, r.allow) {
	case GateAllow:
		return true, "", nil
	case GateDeny:
		return false, fmt.Sprintf("command %q is prohibited (destructive system operation)", cmd), nil
	}

	// Exact-command session memory: if the user already approved this precise
	// command (Allow once or Always), don't re-prompt for the identical re-run.
	if r.allowExact != nil && r.allowExact[cmd] {
		return true, "", nil
	}

	// GateAsk
	if r.askFunc == nil {
		return true, "", nil // headless/unattended: proceed
	}

	return r.askViaModal(ctx, cmd)
}

// gateFileAction asks the user for approval of a critical file mutation
// (create/delete) through the input-bar confirm handler, with an "always allow
// this path" option that persists for the session. Without a handler
// (headless) the action proceeds, mirroring the bash gate.
func (r *Registry) gateFileAction(ctx context.Context, kind, path string) (bool, string, error) {
	key := kind + ":" + path
	if r.fileAllow != nil && r.fileAllow[key] {
		return true, "", nil
	}
	if r.fileActionFunc == nil {
		return true, "", nil // headless/unattended: proceed
	}
	dec, err := r.fileActionFunc(ctx, FileActionRequest{Kind: kind, Path: path})
	if err != nil {
		return false, "file action approval failed: " + err.Error(), nil
	}
	if !dec.Allow {
		return false, fmt.Sprintf("user discarded %s %s", kind, path), nil
	}
	if dec.Always {
		if r.fileAllow == nil {
			r.fileAllow = map[string]bool{}
		}
		r.fileAllow[key] = true
	}
	return true, "", nil
}

// askViaModal presents a gated command to the user via the interactive modal
// (Allow once / Always allow / Deny). Extracted so the file-action gate can
// stay separate from the command gate.
func (r *Registry) askViaModal(ctx context.Context, cmd string) (bool, string, error) {
	question := "⚠️ BroCode wants to run a gated command:\n\n```\n" + cmd + "\n```"
	// Soft guard: installing external linters mid-task is redundant with
	// BroCode's built-in LSP (lsp_scan) and network-heavy. Warn + redirect
	// instead of blocking — the user can still allow if they really want to.
	if isLinterInstall(cmd) {
		question += "\n\n💡 BroCode already surfaces lint/type/deprecated checks via its LSP (`lsp_scan`) — prefer that over installing " + linterName(cmd) + " mid-task. If you truly need it, install it yourself outside BroCode."
	}
	results, err := r.askFunc(ctx, []AskQuestion{
		{
			Question: question,
			Options:  []string{"✅ Allow once", "🔁 Always allow for this session", "🚫 Deny"},
		},
	})
	if err != nil {
		return false, "approval request failed: " + err.Error(), nil
	}
	if len(results) == 0 || len(results[0].Answers) == 0 {
		return false, "user skipped the approval prompt", nil
	}

	ans := results[0].Answers[0]
	key := AllowKey(cmd)
	switch {
	case strings.Contains(ans, "Deny"):
		return false, fmt.Sprintf("user denied command %q", cmd), nil
	case strings.Contains(ans, "Always"):
		if r.allow == nil {
			r.allow = map[string]bool{}
		}
		if r.allowExact == nil {
			r.allowExact = map[string]bool{}
		}
		r.allowExact[cmd] = true
		if key != "" {
			r.allow[key] = true
		}
		return true, "", nil
	default:
		// "Allow once": approve this invocation AND remember the exact command so
		// an identical re-run within the session doesn't re-prompt.
		if r.allowExact == nil {
			r.allowExact = map[string]bool{}
		}
		r.allowExact[cmd] = true
		return true, "", nil
	}
}

func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
}

// ToolByName returns a registered tool by name, or nil when not present. Used by
// the engine for pre-flight context packing (e.g. running lsp_scan itself before
// the model does, so diagnostics land in the first prompt instead of costing a
// tool round-trip).
func (r *Registry) ToolByName(name string) Tool {
	return r.tools[name]
}

// RepoRoot returns the configured repo root (anchors the cd/pushd escape check
// and path resolution for pre-flight context packing).
func (r *Registry) RepoRoot() string {
	return r.repoRoot
}

// linterInstallPatterns are external linter/binary names whose mid-task
// install is redundant with BroCode's built-in LSP diagnostics.
var linterInstallPatterns = []string{
	"golangci-lint", "staticcheck", "revive", "ineffassign", "errcheck",
	"gosimple", "unconvert", "eslint", "flake8", "pylint", "ruff",
	"stylelint", "shellcheck", "golangci",
}

// isLinterInstall reports whether cmd is installing an external linter/analyzer
// toolchain (go install / npm i -g / pip install / cargo install / brew install
// + a known linter name). Used by the soft guard to redirect to LSP.
func isLinterInstall(cmd string) bool {
	lower := strings.ToLower(cmd)
	hasInstall := strings.Contains(lower, "go install") ||
		strings.Contains(lower, "npm install -g") || strings.Contains(lower, "npm i -g") ||
		strings.Contains(lower, "pip install") || strings.Contains(lower, "pip3 install") ||
		strings.Contains(lower, "cargo install") || strings.Contains(lower, "yarn global add") ||
		strings.Contains(lower, "brew install")
	if !hasInstall {
		return false
	}
	for _, name := range linterInstallPatterns {
		if strings.Contains(lower, name) {
			return true
		}
	}
	return false
}

// linterName returns the matched linter name for the soft-guard hint, or a
// generic phrase when none of the known names appear.
func linterName(cmd string) string {
	lower := strings.ToLower(cmd)
	for _, name := range linterInstallPatterns {
		if strings.Contains(lower, name) {
			return name
		}
	}
	return "a linter"
}

func (r *Registry) Definitions() []provider.ToolDefinition {
	// Deterministic order (sorted by name): the tool list is part of the
	// stable prompt prefix that prompt caching keys off. Iterating the map
	// directly would randomize the order on every call (Go map iteration),
	// silently invalidating the cache prefix every round and defeating both
	// Anthropic cache_control breakpoints and OpenAI's automatic prefix
	// caching — each round would pay full price again.
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	defs := make([]provider.ToolDefinition, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		defs = append(defs, provider.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return defs
}

func (r *Registry) Execute(ctx context.Context, name, argsJSON string) (result string, err error) {
	// Executor-level read-only enforcement: catches direct Execute calls that
	// never pass through GateAction (sub-agents and other non-loop callers).
	// lsp_fix/lsp_rename mutate files, so they are read-only-blocked too.
	if r.readOnly && (name == "write_file" || name == "edit_file" || name == "edit_symbol" || name == "delete_file" || name == "lsp_fix" || name == "lsp_rename") {
		return "", fmt.Errorf("tool '%s' is disabled in read-only mode (PLANNER/MINER): output your plan directly as text in your response and BroCode will save it automatically to .brocode/current_plan.md", name)
	}
	if r.readOnlyBash && name == "bash" {
		return "", fmt.Errorf("tool 'bash' is disabled in PLANNER mode (read-only): switch to BUILDER to execute commands")
	}
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("tool '%s' not registered", name)
	}

	// Panic recovery: a panicking tool (e.g. nil deref during edge-case parsing)
	// must NOT bring down the entire engine loop — recover, log, return a clean
	// error so the model sees a usable diagnostic and can proceed.
	defer func() {
		if rc := recover(); rc != nil {
			err = fmt.Errorf("tool '%s' panicked: %v", name, rc)
		}
	}()

	// Smart Context Graph: knowledge hooks are centralized here in Execute
	// so every entry point (main loop, subagents, direct calls) gets them.
	// - Mutating tools (edit/write/delete/lsp_fix/lsp_rename/lsp_autofix):
	//   invalidate BEFORE execution so we never serve a stale file hash.
	// - read_file: update AFTER execution succeeds (async, reads content).
	// - EVERY tool: record a provenance note for future self-retrieval.
	if r.knowledgeStore != nil && name != "" {
		switch name {
		case "edit_file", "edit_symbol", "write_file", "delete_file", "lsp_fix", "lsp_rename", "lsp_autofix":
			if path := extractPathFromArgs(argsJSON); path != "" {
				resolved := resolvePath(path)
				_ = r.knowledgeStore.InvalidateKnowledge("file:" + resolved)
			}
		}
	}

	result, err = t.Execute(ctx, argsJSON)

	// Catch-all self-documenting note: record provenance for EVERY tool action
	// so future sessions can recall what was done where (Phase A of the
	// Self-Aware Context plan). Async — zero added turn latency.
	if r.knowledgeStore != nil && name != "" {
		go r.recordActionNote(ctx, name, argsJSON, result, err)
	}

	// Note: read_file's Smart Context Graph capture now lives inside
	// ReadFileTool.Execute (it has the FULL file content there, so it can index
	// every symbol's line range even when only a span/shrinkwrap/head is shown
	// to the model). Writing tools below still invalidate on mutation.

	return result, err
}

// neighborsFromTurn builds KnowledgeLink edges from the other files touched in
// the same turn (co-read/edit = likely related). Capped at knowledgeMaxNeighbors.
func neighborsFromTurn(ctx context.Context, currentPath string) []store.KnowledgeLink {
	files := turnFilesFromContext(ctx)
	if files == nil {
		return nil
	}
	seen := make(map[string]bool)
	var links []store.KnowledgeLink
	for _, p := range *files {
		if p == currentPath || seen[p] {
			continue
		}
		seen[p] = true
		links = append(links, store.KnowledgeLink{Path: p, Weight: 1.0})
		if len(links) >= store.KnowledgeMaxNeighbors {
			break
		}
	}
	return links
}

// recordActionNote writes a durable, provenance-tagged note for a tool action.
// Subject is a file path when available, else a pattern/query/command. Kept
// lightweight (content is just an outcome string) so high-frequency tool calls
// stay cheap.
func (r *Registry) recordActionNote(ctx context.Context, name, argsJSON, result string, toolErr error) {
	defer func() { recover() }() // safety: never leak on note panic
	if r.knowledgeStore == nil || name == "" {
		return
	}
	subject := extractPathFromArgs(argsJSON)
	if subject == "" {
		subject = extractQueryFromArgs(argsJSON)
	}
	if subject == "" {
		return
	}
	resolved := resolvePath(subject)
	recordTurnFile(ctx, resolved)

	outcome := "ok"
	if toolErr != nil {
		outcome = "error"
	} else if strings.Contains(strings.ToLower(result), "error:") {
		outcome = "error"
	}
	kind := store.NoteExperience
	if isFileTool(name) {
		kind = store.NoteHotfile
	}
	provenance := fmt.Sprintf("tool=%s outcome=%s", name, outcome)
	tags := []string{name}
	if p := strings.TrimPrefix(resolved, "file:"); p != "" {
		tags = append(tags, p)
	}
	_ = r.knowledgeStore.RecordNote(kind, resolved, outcome, provenance, tags)
}

// knowledgeKey normalizes a file path into a knowledge table key.
func knowledgeKey(path string) string {
	return "file:" + path
}

// extractPathFromArgs pulls the "path" field from a JSON args string so the
// knowledge layer can invalidate the right entry. Best-effort: returns "" on
// parse failure.
func extractPathFromArgs(argsJSON string) string {
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) == nil {
		return args.Path
	}
	return ""
}

// extractQueryFromArgs pulls a search-style subject (pattern/query/command)
// from a JSON args string, for tools that don't take a file path.
func extractQueryFromArgs(argsJSON string) string {
	var args struct {
		Pattern string `json:"pattern"`
		Query   string `json:"query"`
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) == nil {
		switch {
		case args.Pattern != "":
			return args.Pattern
		case args.Query != "":
			return args.Query
		case args.Command != "":
			if len(args.Command) > 120 {
				return args.Command[:120]
			}
			return args.Command
		}
	}
	return ""
}

// isFileTool reports whether a tool acts on a file path (vs a search/command).
func isFileTool(name string) bool {
	switch name {
	case "read_file", "write_file", "edit_file", "edit_symbol", "delete_file",
		"lsp_fix", "lsp_rename", "lsp_autofix", "lsp_scan", "undo":
		return true
	}
	return false
}

// Lookup returns a registered tool by name, or nil if not found.
func (r *Registry) Lookup(name string) Tool {
	return r.tools[name]
}

// SubRegistry returns a copy of the registry safe for sub-agent execution:
// tool instances are shared (they are stateless), but the interactive tools
// (ask_user, review_changes) and subagent itself are dropped so a sub-agent
// can never pop a modal, ask the user, or recurse. Gated commands are always
// DENIED in a sub-agent — destructive operations must go through the main
// agent's approval modal, never a silent background run.
func (r *Registry) SubRegistry() *Registry {
	nr := &Registry{
		tools:        make(map[string]Tool, len(r.tools)),
		repoRoot:     r.repoRoot,
		allow:        make(map[string]bool),
		readOnly:     r.readOnly,
		readOnlyBash: r.readOnlyBash,
	}
	for name, t := range r.tools {
		switch name {
		case "ask_user", "review_changes", "subagent":
			continue // no interactive tools, no recursion
		}
		nr.tools[name] = t
	}
	// Non-nil askFunc whose error path DENIES the command: sub-agents cannot
	// request interactive approval, so gated commands are blocked outright.
	nr.askFunc = func(ctx context.Context, q []AskQuestion) ([]AskResult, error) {
		return nil, fmt.Errorf("sub-agents cannot run commands that require user approval — the main agent must run %q", q[0].Question)
	}
	// Same for critical file mutations (create/delete): the confirm bar only
	// exists in the main TUI, so sub-agents may not create or delete files.
	nr.fileActionFunc = func(ctx context.Context, req FileActionRequest) (FileActionDecision, error) {
		return FileActionDecision{}, fmt.Errorf("sub-agents cannot create/delete files — the main agent must run %s %s", req.Kind, req.Path)
	}
	return nr
}

// ---------------- Built-in Tools ----------------

// ReadFileTool
type ReadFileTool struct {
	// knowledgeStore is wired by SetKnowledgeStore so every read captures a
	// whole-file structural index into the Smart Context Graph even when only
	// a span/shrinkwrap/head is returned to the model. Nil disables capture.
	knowledgeStore *store.Store
}

func (t *ReadFileTool) Name() string { return "read_file" }
func (t *ReadFileTool) Description() string {
	return "Read file contents with optional line range or AST shrinkwrap mode"
}
func (t *ReadFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       map[string]any{"type": "string", "description": "Relative or absolute file path"},
			"start_line": map[string]any{"type": "integer", "description": "1-based start line (optional)"},
			"end_line":   map[string]any{"type": "integer", "description": "1-based end line (optional)"},
			"shrinkwrap": map[string]any{"type": "boolean", "description": "When true, returns an AST-compressed view of a large file: signatures, types, imports and docstrings retained, bodies stripped (~70% token reduction). Use to understand a big file's structure in one read instead of many range reads."},
		},
		"required": []string{"path"},
	}
}
func (t *ReadFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path       string `json:"path"`
		StartLine  int    `json:"start_line"`
		EndLine    int    `json:"end_line"`
		Shrinkwrap bool   `json:"shrinkwrap"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	// Leading-slash paths (" /crm_sales_backend/src/...") are a common LLM
	// habit that would fail against the filesystem root — resolve to the
	// project-relative form when the absolute one does not exist.
	args.Path = resolvePath(args.Path)

	// Semantic tool-result cache: repeated reads of the same span (or the same
	// shrinkwrap) return instantly instead of re-reading disk. Invalidated on write.
	readKey := "read_file:" + args.Path + fmt.Sprintf("|%d|%d|%v", args.StartLine, args.EndLine, args.Shrinkwrap)
	if hit, ok := toolResultCache.Get("read_file", readKey); ok {
		return hit, nil
	}

	// Native guard: never read secrets (.env, keys) or heavy dirs
	// (node_modules, vendor, ...) into the LLM context.
	if err := GuardFile(args.Path); err != nil {
		return "", err
	}

	data, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")

	// Whole-file structural capture for the Smart Context Graph. This read may
	// return only a span / shrinkwrap / head preview, but we index the ENTIRE
	// file's symbols (line ranges) and store them — so future sessions know
	// every symbol's position and never lose context to a force-cut. Runs
	// async with recover(): zero added read latency. extractSymbols is run
	// inside UpdateKnowledge from the full content, so positions cover all
	// lines even when the model only ever saw a slice.
	if t.knowledgeStore != nil {
		resolved := args.Path
		lang := detectFileLanguage(resolved)
		full := string(data)
		go func(p, content string) {
			defer func() { recover() }()
			recordTurnFile(ctx, p)
			neighbors := neighborsFromTurn(ctx, p)
			_ = t.knowledgeStore.UpdateKnowledge(knowledgeKey(p), lang, content, neighbors, nil)
		}(resolved, full)
	}

	// Range read (start_line/end_line): stream ONLY the requested span from disk
	// instead of loading the whole file into memory (P3). Essential for huge files
	// and keeps the round cheap — the model should prefer this over full reads.
	if args.StartLine > 0 || args.EndLine > 0 {
		start := args.StartLine - 1
		if start < 0 {
			start = 0
		}
		end := args.EndLine
		if end <= 0 {
			end = math.MaxInt
		}
		span, _, rerr := readLineSpan(args.Path, start, end)
		if rerr != nil {
			return "", rerr
		}
		toolResultCache.Put("read_file", readKey, span, "file:"+args.Path)
		return span, nil
	}

	// Explicit AST shrinkwrap: an optional whole-file structural overview for
	// large files. The model opts in (shrinkwrap:true) instead of falling back
	// to many range reads when it only needs signatures, types and structure.
	if args.Shrinkwrap {
		compressed := bcontext.ShrinkwrapAST(string(data), args.Path)
		if len(lines) > 150 {
			out := CapOutput(fmt.Sprintf("%s\n\n[AST shrinkwrap view of %d lines — function bodies omitted. Use read_file with start_line/end_line for specific bodies.]", compressed, len(lines)))
			toolResultCache.Put("read_file", readKey, out, "file:"+args.Path)
			return out, nil
		}
		out := CapOutput(compressed)
		toolResultCache.Put("read_file", readKey, out, "file:"+args.Path)
		return out, nil
	}

	// Lean-by-default for large files (P2): dumping a whole big file into context
	// makes the model ingest code it usually only needs one span of. For files over
	// 150 lines we return a short head preview plus actionable guidance — the model
	// targets the exact span with read_file(start_line/end_line), or requests a
	// structural overview with read_file(shrinkwrap:true). Files <=150 lines are
	// still returned in full (cheap).
	var jsonWarning string
	if strings.HasSuffix(strings.ToLower(args.Path), ".json") {
		if err := ValidateJSONNoDuplicateKeys(string(data)); err != nil {
			jsonWarning = fmt.Sprintf("[⚠️ JSON STRUCTURAL WARNING: %v]\n\n", err)
		}
	}

	if len(lines) > 150 {
		headN := 60
		if len(lines) < headN {
			headN = len(lines)
		}
		head := strings.Join(lines[:headN], "\n")
		out := fmt.Sprintf("%s[File %s has %d lines — showing first %d as a preview. Use read_file(start_line/end_line) for the exact span; or read_file(shrinkwrap:true) for a structural (signatures/types) overview. Use code_locate to find symbols across the project.]\n\n%s", jsonWarning, args.Path, len(lines), headN, head)
		toolResultCache.Put("read_file", readKey, out, "file:"+args.Path)
		return out, nil
	}

	out := jsonWarning + CapOutput(string(data))
	toolResultCache.Put("read_file", readKey, out, "file:"+args.Path)
	return out, nil
}

// detectFileLanguage returns a short stack label ("go", "ts", "python", ...)
// based on the file extension, for knowledge entry tagging.
func detectFileLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go": return "go"
	case ".ts", ".tsx", ".js", ".jsx": return "ts"
	case ".py": return "python"
	case ".rs": return "rust"
	case ".rb": return "ruby"
	case ".java": return "java"
	case ".c", ".h": return "c"
	case ".cpp", ".cc", ".cxx", ".hpp": return "cpp"
	case ".php": return "php"
	case ".sql": return "sql"
	case ".html", ".htm", ".vue", ".svelte", ".astro": return "web"
	case ".sh": return "bash"
	default: return "text"
	}
}

// readLineSpan streams lines [start, end) (0-based start, 1-based exclusive end)
// from disk without loading the entire file. Returns the joined span and the
// total line count. Used by read_file range reads so a 2000-line file costs only
// the bytes actually requested.
func readLineSpan(path string, start, end int) (string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // permit long lines
	var b strings.Builder
	total := 0
	first := true
	for sc.Scan() {
		total++
		if total > end {
			break
		}
		if total > start {
			if !first {
				b.WriteByte('\n')
			}
			b.WriteString(sc.Text())
			first = false
		}
	}
	if err := sc.Err(); err != nil {
		return "", total, err
	}
	if start >= total {
		return "", total, fmt.Errorf("start_line %d out of bounds (%d lines)", start+1, total)
	}
	return b.String(), total, nil
}

// WriteFileTool
type WriteFileTool struct{}

func (t *WriteFileTool) Name() string        { return "write_file" }
func (t *WriteFileTool) Description() string { return "Write or overwrite a file with new content. Set append=true to ADD content to the end of an existing file instead of replacing it." }
func (t *WriteFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Target file path"},
			"content": map[string]any{"type": "string", "description": "Complete file content (or text to append when append=true)"},
			"append":  map[string]any{"type": "boolean", "description": "When true, append content to the file instead of overwriting it"},
		},
		"required": []string{"path", "content"},
	}
}
func (t *WriteFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	args.Path = resolvePath(args.Path)

	// Native guard: never write into heavy dirs or sensitive files.
	if err := GuardFile(args.Path); err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(args.Path), 0755); err != nil && filepath.Dir(args.Path) != "." {
		return "", err
	}

	// Capture the previous content (if any) so the turn's change summary can
	// distinguish "created" from "modified" and render a +/- diff.
	old := ""
	if data, err := os.ReadFile(args.Path); err == nil {
		old = string(data)
		// Strip UTF-8 BOM (RFC 8259: JSON MUST NOT have BOM). Write back clean.
		if strings.HasPrefix(old, "\xef\xbb\xbf") {
			old = old[3:]
		}
	}
	// One-turn rollback window: keep a backup before overwriting.
	_ = Snapshot(args.Path)

	final := args.Content
	action := "created"
	if args.Append && old != "" {
		final = old + args.Content
		action = "appended"
	} else if old != "" {
		action = "modified"
	}

	// Syntax integrity validation: reject broken brackets/delimiters
	if old != "" {
		if err := ValidateSyntaxIntegrity(args.Path, old, final); err != nil {
			return "", fmt.Errorf("syntax integrity check failed for %s: %w. File was NOT overwritten. Please verify the code before writing.", args.Path, err)
		}
	}

	// JSON structural validation: ensure no duplicate keys in object scope
	if strings.HasSuffix(strings.ToLower(args.Path), ".json") {
		if err := ValidateJSONNoDuplicateKeys(final); err != nil {
			return "", fmt.Errorf("JSON structural error in %s: %w", args.Path, err)
		}
	}

	// Invalidate any cached reads/greps so a subsequent read sees the new content.
	toolResultCache.InvalidatePath(args.Path)

	if err := os.WriteFile(args.Path, []byte(final), 0644); err != nil {
		return "", err
	}

	RecordChange(FileChange{Path: args.Path, Action: action, Old: old, New: final})
	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(final), args.Path), nil
}

// EditFileTool
type EditFileTool struct{}

func (t *EditFileTool) Name() string { return "edit_file" }
func (t *EditFileTool) Description() string {
	return "Edit a file using surgical search & replace. Provide 'target' (the exact verbatim code snippet from read_file) and 'replacement' (the new code), or an array of 'edits' for atomic multi-chunk changes. 'start_line' and 'end_line' are optional to narrow the search window in large files."
}
func (t *EditFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":        map[string]any{"type": "string", "description": "Target file path"},
			"target":      map[string]any{"type": "string", "description": "Exact verbatim code block to replace from read_file"},
			"replacement": map[string]any{"type": "string", "description": "New replacement code"},
			"start_line":  map[string]any{"type": "integer", "description": "Optional 1-based start line to narrow the search window in large files"},
			"end_line":    map[string]any{"type": "integer", "description": "Optional 1-based end line to narrow the search window in large files"},
			"edits": map[string]any{
				"type":        "array",
				"description": "Optional array of atomic edit chunks [{target, replacement}, ...] applied sequentially with all-or-nothing rollback",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"target":      map[string]any{"type": "string", "description": "Exact verbatim code block to replace"},
						"replacement": map[string]any{"type": "string", "description": "New replacement code"},
					},
					"required": []string{"target", "replacement"},
				},
			},
		},
		"required": []string{"path"},
	}
}
func (t *EditFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path        string `json:"path"`
		Target      string `json:"target"`
		Replacement string `json:"replacement"`
		StartLine   int    `json:"start_line"`
		EndLine     int    `json:"end_line"`
		Edits       []struct {
			Target      string `json:"target"`
			Replacement string `json:"replacement"`
		} `json:"edits"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	args.Path = resolvePath(args.Path)

	// Native guard: never edit heavy dirs or sensitive files.
	if err := GuardFile(args.Path); err != nil {
		return "", err
	}

	data, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}

	// One-turn rollback window: keep a backup before modifying.
	_ = Snapshot(args.Path)

	content := string(data)
	// Strip UTF-8 BOM if present so JSON validation and text matching work cleanly.
	// The file is written back without BOM, which is the correct standard for JSON (RFC 8259).
	if strings.HasPrefix(content, "\xef\xbb\xbf") {
		content = content[3:]
	}
	newContent := content
	editRange := ""

	if len(args.Edits) > 0 {
		// Multi-chunk atomic transaction
		for idx, hunk := range args.Edits {
			if hunk.Target == "" {
				continue
			}
			res, _, err := ApplyResilientEdit(newContent, hunk.Target, hunk.Replacement)
			if err != nil {
				diag := ""
				if closest := FindClosestBlock(newContent, hunk.Target); closest != "" {
					diag = fmt.Sprintf("\nDid you mean:\n---\n%s\n---", closest)
				}
				return "", fmt.Errorf("chunk #%d failed in %s: %w.%s Tip: inspect with read_file to copy the exact code block", idx+1, args.Path, err, diag)
			}
			newContent = res
		}
		editRange = fmt.Sprintf(" (%d atomic chunks)", len(args.Edits))
	} else if args.Target != "" {
		res, tier, err := ApplyResilientEdit(content, args.Target, args.Replacement)
		if err == nil {
			newContent = res
			if tier != "exact" {
				editRange = fmt.Sprintf(" (matched via %s)", tier)
			}
		} else if args.StartLine > 0 || args.EndLine > 0 {
			// Positional fallback if target search failed
			lines := strings.Split(content, "\n")
			start := args.StartLine - 1
			if start < 0 {
				start = 0
			}
			end := args.EndLine
			if end <= 0 || end > len(lines) {
				end = len(lines)
			}
			if start > len(lines) {
				return "", fmt.Errorf("start_line %d out of bounds (%d lines)", args.StartLine, len(lines))
			}
			span := strings.Split(args.Replacement, "\n")
			updated := make([]string, 0, len(lines)-end+start+len(span))
			updated = append(updated, lines[:start]...)
			updated = append(updated, span...)
			updated = append(updated, lines[end:]...)
			newContent = strings.Join(updated, "\n")
			editRange = fmt.Sprintf(" (lines %d-%d)", args.StartLine, end)
		} else {
			diag := ""
			if closest := FindClosestBlock(content, args.Target); closest != "" {
				diag = fmt.Sprintf("\nDid you mean:\n---\n%s\n---", closest)
			}
			return "", fmt.Errorf("target block not found in %s: %w.%s Tip: use read_file first to copy the exact code block", args.Path, err, diag)
		}
	} else if args.StartLine > 0 || args.EndLine > 0 {
		// Pure positional edit without target
		lines := strings.Split(content, "\n")
		start := args.StartLine - 1
		if start < 0 {
			start = 0
		}
		end := args.EndLine
		if end <= 0 || end > len(lines) {
			end = len(lines)
		}
		if start > len(lines) {
			return "", fmt.Errorf("start_line %d out of bounds (%d lines)", args.StartLine, len(lines))
		}
		span := strings.Split(args.Replacement, "\n")
		updated := make([]string, 0, len(lines)-end+start+len(span))
		updated = append(updated, lines[:start]...)
		updated = append(updated, span...)
		updated = append(updated, lines[end:]...)
		newContent = strings.Join(updated, "\n")
		editRange = fmt.Sprintf(" (lines %d-%d)", args.StartLine, end)
	} else {
		return "", fmt.Errorf("edit_file requires 'target' text to replace or 'edits' array. Please inspect with read_file and provide the exact verbatim target block")
	}

	if newContent == content {
		return fmt.Sprintf("No change made to %s", args.Path), nil
	}

	// Syntax integrity validation: reject broken brackets, malformed JSX, unbalanced tokens
	if err := ValidateSyntaxIntegrity(args.Path, content, newContent); err != nil {
		return "", fmt.Errorf("syntax integrity check failed for %s: %w. Edit was REJECTED to prevent file corruption. Please inspect with read_file and provide a balanced replacement block.", args.Path, err)
	}

	// JSON structural validation: ensure no duplicate keys in object scope
	if strings.HasSuffix(strings.ToLower(args.Path), ".json") {
		if err := ValidateJSONNoDuplicateKeys(newContent); err != nil {
			return "", fmt.Errorf("JSON structural error in %s: %w", args.Path, err)
		}
	}

	// Invalidate any cached reads/greps so a subsequent read sees the new content.
	toolResultCache.InvalidatePath(args.Path)

	if err := os.WriteFile(args.Path, []byte(newContent), 0644); err != nil {
		return "", err
	}

	RecordChange(FileChange{Path: args.Path, Action: "modified", Old: content, New: newContent})

	edits := myers.ComputeEdits(span.URIFromPath(args.Path), content, newContent)
	unified := gotextdiff.ToUnified(args.Path, args.Path, content, edits)
	return fmt.Sprintf("Successfully updated %s%s\nDiff:\n%s", args.Path, editRange, unified), nil
}

// DeleteFileTool permanently removes a file. It is gated (GateAction asks the
// user before deletion) and its old content is recorded so the turn's change
// summary shows the deletion and the undo snapshot can restore the file.
type DeleteFileTool struct{}

func (t *DeleteFileTool) Name() string { return "delete_file" }
func (t *DeleteFileTool) Description() string {
	return "Delete a file permanently (requires user confirmation)"
}
func (t *DeleteFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Target file path to delete"},
		},
		"required": []string{"path"},
	}
}
func (t *DeleteFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	args.Path = resolvePath(args.Path)

	// Native guard: never delete inside heavy dirs or sensitive files.
	if err := GuardFile(args.Path); err != nil {
		return "", err
	}

	// Record the old content before removal so the change summary can show
	// what was deleted (and the snapshot restores it on undo).
	old := ""
	if data, err := os.ReadFile(args.Path); err == nil {
		old = string(data)
	}
	_ = Snapshot(args.Path)

	if err := os.Remove(args.Path); err != nil {
		return "", err
	}

	RecordChange(FileChange{Path: args.Path, Action: "deleted", Old: old})
	return fmt.Sprintf("Successfully deleted %s", args.Path), nil
}

// ListDirTool
type ListDirTool struct{}

func (t *ListDirTool) Name() string        { return "list_dir" }
func (t *ListDirTool) Description() string { return "List directory contents" }
func (t *ListDirTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory path"},
		},
		"required": []string{"path"},
	}
}
func (t *ListDirTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	args.Path = resolvePath(args.Path)

	if args.Path == "" {
		args.Path = "."
	}

	// Native guard: listing inside heavy dirs is pointless and noisy; the
	// name itself is visible from the parent listing.
	if err := GuardHeavyPath(args.Path); err != nil {
		return "", err
	}

	entries, err := os.ReadDir(args.Path)
	if err != nil {
		return "", err
	}

	var items []string
	for _, entry := range entries {
		suffix := ""
		if entry.IsDir() {
			suffix = "/"
		}
		items = append(items, entry.Name()+suffix)
	}

	return strings.Join(items, "\n"), nil
}

// GrepTool
type GrepTool struct{}

func (t *GrepTool) Name() string        { return "grep" }
func (t *GrepTool) Description() string { return "Search pattern in codebase (returns top 50 matches)" }
func (t *GrepTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Search pattern"},
			"path":    map[string]any{"type": "string", "description": "Search directory path"},
		},
		"required": []string{"pattern"},
	}
}
func (t *GrepTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	if args.Path == "" {
		args.Path = "."
	}
	// Leading-slash paths (a common LLM habit) would resolve against the
	// filesystem root and silently return "no matches", burning rounds.
	args.Path = resolvePath(args.Path)

	// Semantic tool-result cache: an unchanged tree returns the same grep result
	// instantly. Invalidated on any file write/edit (global scope).
	grepKey := "grep:" + args.Pattern + "|" + args.Path
	if hit, ok := toolResultCache.Get("grep", grepKey); ok {
		return hit, nil
	}

	// Fail-fast path validation: if path does not exist on disk, return a clear
	// diagnostic message so the LLM immediately knows the path is wrong
	// instead of getting "No matches found." and looping.
	if _, err := os.Stat(args.Path); os.IsNotExist(err) {
		cwd, _ := os.Getwd()
		return fmt.Sprintf("Error: search path %q does not exist (resolved relative to workspace %q). Check project structure or use list_dir.", args.Path, cwd), nil
	}

	// Native guard: never search inside heavy dirs (node_modules, vendor,
	// target, ...) or dump sensitive files.
	if err := GuardHeavyPath(args.Path); err != nil {
		return "", err
	}
	if err := GuardSensitivePath(args.Path); err != nil {
		return "", err
	}

	// Skip heavy/vendored directories so searches stay fast and relevant in
	// big monorepos (node_modules, .git, build output, etc.). Without this a
	// single grep can wade through gigabytes of dependencies and drown the
	// agent in noise — which is exactly what made earlier turns spin.
	excludeDirs := []string{
		"node_modules", "bower_components", "vendor", "dist", "build", "out",
		".git", ".next", ".nuxt", "coverage", ".cache", ".turbo", "target",
		"__pycache__", ".venv", "venv", "Pods", ".gradle", "bin", "obj",
	}
	var excl []string
	for _, d := range excludeDirs {
		excl = append(excl, "--exclude-dir="+d)
	}

	cmdArgs := append([]string{"-E", "-rn", "-I"}, append(excl, args.Pattern, args.Path)...)
	cmd := exec.CommandContext(ctx, "grep", cmdArgs...)
	output, err := cmd.Output()
	// grep exits 1 when there are NO matches — that is a normal result, not an
	// error. Only re-run with -F (fixed strings) when grep failed for another
	// reason (exit 2 = regex parse error, exit >1), otherwise "no matches"
	// would silently scan the tree twice on every empty result — the most
	// common case in an agent loop.
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			output = nil // no matches: fall through to the empty-result path
		} else if len(output) == 0 {
			// Fallback to fixed strings match (-F) if regex pattern parsing failed
			cmdArgsF := append([]string{"-rn", "-I", "-F"}, append(excl, args.Pattern, args.Path)...)
			cmdFixed := exec.CommandContext(ctx, "grep", cmdArgsF...)
			output, _ = cmdFixed.Output()
		}
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		// Case-insensitive fallback: if exact case returned 0 matches and query contains uppercase/mixed letters
		if strings.ToLower(args.Pattern) != args.Pattern || strings.ToUpper(args.Pattern) != args.Pattern {
			cmdArgsI := append([]string{"-E", "-rn", "-I", "-i"}, append(excl, args.Pattern, args.Path)...)
			cmdI := exec.CommandContext(ctx, "grep", cmdArgsI...)
			if outI, errI := cmdI.Output(); errI == nil && len(outI) > 0 {
				iLines := strings.Split(strings.TrimSpace(string(outI)), "\n")
				if len(iLines) > 0 && iLines[0] != "" {
					if len(iLines) > 30 {
						iLines = iLines[:30]
					}
					msg := fmt.Sprintf("[No exact case-sensitive matches. Found %d case-insensitive matches]:\n%s", len(iLines), strings.Join(iLines, "\n"))
					toolResultCache.Put("grep", grepKey, msg, "global")
					return msg, nil
				}
			}
		}
		toolResultCache.Put("grep", grepKey, "No matches found.", "global")
		return "No matches found.", nil
	}

	if len(lines) > 50 {
		head := strings.Join(lines[:50], "\n")
		out := fmt.Sprintf("%s\n\n[showing 50/%d matches, refine query or ask for specific file]", head, len(lines))
		toolResultCache.Put("grep", grepKey, out, "global")
		return out, nil
	}

	out := strings.Join(lines, "\n")
	toolResultCache.Put("grep", grepKey, out, "global")
	return out, nil
}

// GlobTool
type GlobTool struct{}

func (t *GlobTool) Name() string        { return "glob" }
func (t *GlobTool) Description() string { return "Find files matching pattern" }
func (t *GlobTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Glob pattern (e.g. *.go or internal/**/*.go)"},
		},
		"required": []string{"pattern"},
	}
}
func (t *GlobTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	// Tolerate patterns the model writes with a leading "/" (treating it as
	// repo-rooted) — "/*.js" or "/src/**" should mean "*.js" / "src/**"
	// relative to the working directory, not an absolute filesystem path that
	// never matches. Models (and some free model outputs) do this constantly;
	// without this a glob silently returns nothing and the agent spins.
	pattern := strings.TrimPrefix(args.Pattern, "/")
	pattern = strings.TrimPrefix(pattern, "./")

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}

	// Go's filepath.Glob does NOT support recursive "**" and a bare name like
	// "ConversationService" only matches the cwd — so "glob /ConversationService*"
	// silently finds nothing and the agent spins. Fall back to a recursive walk
	// when the direct match is empty (or the pattern has no path separator).
	if len(matches) == 0 || !strings.Contains(pattern, string(filepath.Separator)) {
		rec, walkErr := recursiveGlob(pattern)
		if walkErr != nil {
			return "", walkErr
		}
		matches = rec
	}

	if len(matches) == 0 {
		return "No matching files.", nil
	}
	if len(matches) > 50 {
		matches = matches[:50]
	}
	return strings.Join(matches, "\n"), nil
}

// recursiveGlob walks the working directory and returns paths matching a glob
// pattern. Bare names (no separator) match the file/dir basename anywhere in
// the tree (e.g. "ConversationService*"); patterns with separators are matched
// against the path relative to the cwd. Heavy/vendored directories are skipped
// and results are capped.
func recursiveGlob(pattern string) ([]string, error) {
	var matches []string
	basePattern := pattern
	if !strings.Contains(pattern, string(filepath.Separator)) {
		basePattern = filepath.Base(pattern)
	}

	// Bare names with no wildcard (e.g. "conversation") most likely target a
	// directory — match directories too so "/conversation" finds
	// src/services/conversation instead of returning nothing.
	bareNoWildcard := !strings.Contains(pattern, string(filepath.Separator)) &&
		!strings.ContainsAny(pattern, "*?[")

	walkErr := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, keep walking
		}
		if len(matches) >= 200 {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if path != "." && IsHeavyDir(d.Name()) {
				return filepath.SkipDir
			}
			if bareNoWildcard {
				if ok, _ := filepath.Match(basePattern, d.Name()); ok {
					matches = append(matches, path+"/")
				}
			}
			return nil
		}

		if strings.Contains(pattern, string(filepath.Separator)) {
			if ok, _ := filepath.Match(pattern, path); ok {
				matches = append(matches, path)
			}
			return nil
		}
		// Never surface sensitive files (.env, keys) in glob results.
		if GuardSensitivePath(path) != nil {
			return nil
		}
		if ok, _ := filepath.Match(basePattern, d.Name()); ok {
			matches = append(matches, path)
		}
		return nil
	})
	return matches, walkErr
}

// ---------------- Interactive User Questions ----------------

// AskQuestion and AskResult alias the provider types so the OpenCode CLI
// adapter (which lives in provider) can present clarification questions
// through the same interactive modal as the ask_user tool, without the
// provider package importing tool (which would be an import cycle).
type AskQuestion = provider.AskQuestion
type AskResult = provider.AskResult

// AskUserTool asks the user interactive multiple-choice questions. The Ask
// handler is wired by the UI layer; without it (headless) the tool fails
// gracefully with an error the model can read.
type AskUserTool struct {
	Ask func(ctx context.Context, questions []AskQuestion) ([]AskResult, error)
}

func (t *AskUserTool) Name() string { return "ask_user" }
func (t *AskUserTool) Description() string {
	return "Ask the user interactive multiple-choice questions when you need a decision, preference, or confirmation that tools cannot determine. Supports single-select, multi-select, and custom answers."
}
func (t *AskUserTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"questions": map[string]any{
				"type":        "array",
				"description": "One or more questions to ask (1-3 recommended). Each question has 2-6 options.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{"type": "string", "description": "The question text"},
						"options":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Answer options"},
						"multi":    map[string]any{"type": "boolean", "description": "true = user may select multiple options; false (default) = single choice"},
					},
					"required": []string{"question", "options"},
				},
			},
		},
		"required": []string{"questions"},
	}
}
func (t *AskUserTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Questions []AskQuestion `json:"questions"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	if len(args.Questions) == 0 {
		return "", fmt.Errorf("ask_user requires at least one question")
	}
	if t.Ask == nil {
		return "", fmt.Errorf("ask_user handler is not configured (running headless?)")
	}

	results, err := t.Ask(ctx, args.Questions)
	if err != nil {
		return "", fmt.Errorf("user interaction failed: %w", err)
	}
	if len(results) == 0 {
		return "The user skipped the questions without providing answers. Proceed with the most reasonable default.", nil
	}

	out, err := json.Marshal(results)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("User answers:\n%s", out), nil
}

// BashTool
type BashTool struct {
	// Container, when non-nil and Enabled, routes every command through a
	// Docker container instead of the host shell (see ContainerSandbox). It is
	// wired by Registry.SetSandbox from the sandbox.json policy.
	Container *ContainerSandbox
	// WorkDir is the project root, mounted at /workspace inside the container.
	WorkDir string
}

func (t *BashTool) Name() string { return "bash" }
func (t *BashTool) Description() string {
	return "Execute shell commands in the workspace. Fully available for exploration, git commands (diff, log, status), terminal tools (grep, ripgrep, find, jq), running test suites, compilers, and project utilities. Destructive system operations (rm -rf /, disk formatting) and dumping secret files (.env, private keys) are protected."
}
func (t *BashTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "Shell command to run"},
		},
		"required": []string{"command"},
	}
}
func (t *BashTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	// Basic safety filter for destructive commands
	lower := strings.ToLower(args.Command)
	if strings.Contains(lower, "rm -rf /") || strings.Contains(lower, "mkfs") || strings.Contains(lower, ":(){ :|:& };:") {
		return "", fmt.Errorf("prohibited destructive command")
	}

	// Native guard: block commands that read/dump sensitive files (cat .env,
	// head id_rsa, ...) so secrets never enter the LLM context. Applies inside
	// a container too — isolation is not permission to exfiltrate secrets.
	if msg := GuardSensitiveCommand(args.Command); msg != "" {
		return "", fmt.Errorf("%s", msg)
	}

	// Bound every command with a timeout so a hung process cannot stall the loop.
	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Container sandbox (opt-in): run inside Docker instead of the host shell.
	if t.Container != nil && t.Container.Enabled {
		return t.execInContainer(tctx, args.Command)
	}

	cmdStr := args.Command
	// Map /workspace references on host shell to the project root directory
	if t.WorkDir != "" && strings.Contains(cmdStr, "/workspace") {
		cmdStr = strings.ReplaceAll(cmdStr, "/workspace", t.WorkDir)
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		if bashPath, err := exec.LookPath("bash"); err == nil {
			cmd = exec.CommandContext(tctx, bashPath, "-c", cmdStr)
		} else {
			cmd = exec.CommandContext(tctx, "cmd.exe", "/c", cmdStr)
		}
	} else {
		cmd = exec.CommandContext(tctx, "sh", "-c", cmdStr)
	}
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if tctx.Err() == context.DeadlineExceeded {
		return CapOutput(fmt.Sprintf("Command timed out after 60s.\nOutput:\n%s", result)), nil
	}
	if err != nil {
		return CapOutput(fmt.Sprintf("Command failed with error: %v\nOutput:\n%s", err, result)), nil
	}
	if result == "" {
		return "Command executed successfully with no output.", nil
	}
	return CapOutput(result), nil
}

// execInContainer runs the command inside a Docker container with the project
// root mounted at /workspace. If docker is missing the tool errors clearly
// instead of falling back to the host — a silently disabled sandbox would give
// the user false isolation.
func (t *BashTool) execInContainer(ctx context.Context, command string) (string, error) {
	image := strings.TrimSpace(t.Container.Image)
	if image == "" {
		image = "alpine:3.20"
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return "", fmt.Errorf("container sandbox is enabled (image %q) but docker is not installed — install Docker Desktop/CLI or disable the container sandbox in .brocode/sandbox.json", image)
	}

	cmd := exec.CommandContext(ctx, "docker", containerRunArgs(t.WorkDir, image, command)...)
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		return CapOutput(fmt.Sprintf("Command timed out after 60s (container %s).\nOutput:\n%s", image, result)), nil
	}
	if err != nil {
		return CapOutput(fmt.Sprintf("Container command failed with error: %v\nOutput:\n%s", err, result)), nil
	}
	if result == "" {
		return "Command executed successfully in container with no output.", nil
	}
	return CapOutput(result), nil
}

// containerRunArgs builds the docker run invocation: the repo root (if any) is
// mounted read-write at /workspace and the command runs as sh -c inside the
// image. Kept as a pure function so the construction is unit-testable without
// Docker.
func containerRunArgs(workDir, image, command string) []string {
	args := []string{"run", "--rm"}
	if workDir != "" {
		args = append(args, "-v", workDir+":/workspace", "-w", "/workspace")
	}
	args = append(args, image, "sh", "-c", command)
	return args
}

// ---------------- Web & Git ----------------

var (
	stripScriptsRegex = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	stripStylesRegex  = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	stripTagsRegex    = regexp.MustCompile(`<[^>]+>`)
	collapseWSRegex   = regexp.MustCompile(`[ \t]+`)
	collapseNLRegex   = regexp.MustCompile(`\n{3,}`)
)

// htmlToText extracts readable text from an HTML document.
func htmlToText(raw string) string {
	s := stripScriptsRegex.ReplaceAllString(raw, "")
	s = stripStylesRegex.ReplaceAllString(s, "")
	s = stripTagsRegex.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = collapseWSRegex.ReplaceAllString(s, " ")
	s = collapseNLRegex.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// FetchURLTool fetches a URL and returns its readable text content.
type FetchURLTool struct{}

func (t *FetchURLTool) Name() string { return "fetch_url" }
func (t *FetchURLTool) Description() string {
	return "Fetch a URL and return its readable text content (documentation, error pages, API docs, etc.)"
}
func (t *FetchURLTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":       map[string]any{"type": "string", "description": "Full http(s) URL to fetch"},
			"max_chars": map[string]any{"type": "integer", "description": "Max characters to return (default 20000, max 100000)"},
		},
		"required": []string{"url"},
	}
}
func (t *FetchURLTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
		return "", fmt.Errorf("only http(s) URLs are supported")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "BroCode/1.0")
	resp, err := httpClientFetch.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", err
	}

	text := htmlToText(string(body))
	maxChars := args.MaxChars
	if maxChars <= 0 {
		maxChars = 20000
	} else if maxChars > 100000 {
		maxChars = 100000
	}
	if r := []rune(text); len(r) > maxChars {
		text = string(r[:maxChars]) + fmt.Sprintf("\n… [truncated, %d more chars]", len(r)-maxChars)
	}
	return text, nil
}

// GitTool runs read-only git commands only — no commands that mutate the repo.
type GitTool struct{}

func (t *GitTool) Name() string { return "git" }
func (t *GitTool) Description() string {
	return "Run git commands: status, diff, log, branch (read-only) and commit (requires user approval). Commit and other mutating operations go through the permission gate."
}
func (t *GitTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":  map[string]any{"type": "string", "enum": []string{"status", "diff", "log", "branch", "commit"}, "description": "Which git operation to run"},
			"message": map[string]any{"type": "string", "description": "commit: the commit message (required for commit)"},
			"stat":    map[string]any{"type": "boolean", "description": "diff: show file stats instead of the full diff (default false)"},
			"limit":   map[string]any{"type": "integer", "description": "log: max commits to show (default 10, max 50)"},
		},
		"required": []string{"action"},
	}
}
func (t *GitTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Action  string `json:"action"`
		Message string `json:"message"`
		Stat    bool   `json:"stat"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	var argv []string
	switch args.Action {
	case "status":
		argv = []string{"status", "--short", "--branch"}
	case "diff":
		if args.Stat {
			argv = []string{"diff", "--stat"}
		} else {
			argv = []string{"diff"}
		}
	case "log":
		limit := args.Limit
		if limit <= 0 {
			limit = 10
		} else if limit > 50 {
			limit = 50
		}
		argv = []string{"log", "--oneline", "-n", fmt.Sprintf("%d", limit)}
	case "branch":
		argv = []string{"branch", "-a"}
	case "commit":
		msg := strings.TrimSpace(args.Message)
		if msg == "" {
			return "", fmt.Errorf("commit requires a message parameter")
		}
		argv = []string{"commit", "-m", msg}
	default:
		return "", fmt.Errorf("unknown git action %q (allowed: status, diff, log, branch, commit)", args.Action)
	}

	// Bound git ops so a slow/hung repo cannot stall the agent loop.
	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, "git", argv...)
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		if result != "" {
			return result, nil
		}
		return "", fmt.Errorf("git %s failed: %w", args.Action, err)
	}
	if result == "" {
		return "(no output)", nil
	}
	return CapOutput(result), nil
}

// WebSearchTool searches the web via the Exa API (semantic search for AI
// agents). Requires the EXA_API_KEY environment variable.
type WebSearchTool struct{}

func (t *WebSearchTool) Name() string { return "web_search" }
func (t *WebSearchTool) Description() string {
	return "Search the web and return result titles, URLs and dates (docs, errors, libraries). Pair with fetch_url to read a result. Requires EXA_API_KEY."
}
func (t *WebSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "description": "Search query"},
			"num_results": map[string]any{"type": "integer", "description": "Number of results (default 5, max 10)"},
		},
		"required": []string{"query"},
	}
}
func (t *WebSearchTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Query     string `json:"query"`
		NumResult int    `json:"num_results"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	if strings.TrimSpace(args.Query) == "" {
		return "", fmt.Errorf("web_search requires a query")
	}

	num := args.NumResult
	if num <= 0 {
		num = 5
	} else if num > 10 {
		num = 10
	}

	apiKey := os.Getenv("EXA_API_KEY")
	tavilyKey := os.Getenv("TAVILY_API_KEY")

	// 1. If Exa key is provided, use Exa API
	if apiKey != "" {
		body, err := json.Marshal(map[string]any{
			"query":      args.Query,
			"numResults": num,
		})
		if err != nil {
			return "", err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.exa.ai/search", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)

		resp, err := httpClientSearch.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
				if err == nil {
					var out struct {
						Results []struct {
							Title         string `json:"title"`
							URL           string `json:"url"`
							PublishedDate string `json:"publishedDate"`
						} `json:"results"`
					}
					if json.Unmarshal(raw, &out) == nil && len(out.Results) > 0 {
						var sb strings.Builder
						for i, r := range out.Results {
							date := ""
							if r.PublishedDate != "" {
								date = " (" + r.PublishedDate[:min(10, len(r.PublishedDate))] + ")"
							}
							sb.WriteString(fmt.Sprintf("%d. %s%s\n   %s\n", i+1, r.Title, date, r.URL))
						}
						return strings.TrimSpace(sb.String()), nil
					}
				}
			}
		}
	}

	// 2. If Tavily key is provided, use Tavily API
	if tavilyKey != "" {
		body, err := json.Marshal(map[string]any{
			"query":       args.Query,
			"max_results": num,
			"api_key":     tavilyKey,
		})
		if err == nil {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(body))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				resp, err := httpClientSearch.Do(req)
				if err == nil {
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
						if err == nil {
							var tout struct {
								Results []struct {
									Title   string `json:"title"`
									URL     string `json:"url"`
									Content string `json:"content"`
								} `json:"results"`
							}
							if json.Unmarshal(raw, &tout) == nil && len(tout.Results) > 0 {
								var sb strings.Builder
								for i, r := range tout.Results {
									sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n", i+1, r.Title, r.URL))
									if r.Content != "" {
										snip := r.Content
										if len(snip) > 160 {
											snip = snip[:157] + "..."
										}
										sb.WriteString("   " + snip + "\n")
									}
								}
								return strings.TrimSpace(sb.String()), nil
							}
						}
					}
				}
			}
		}
	}

	// 3. Fallback: Zero-Config Free Web Search (DuckDuckGo Lite)
	freeResults, err := FreeWebSearch(ctx, args.Query, num)
	if err != nil {
		return "", fmt.Errorf("web search failed: %w", err)
	}
	if len(freeResults) == 0 {
		return "No results found.", nil
	}

	var sb strings.Builder
	for i, r := range freeResults {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n", i+1, r.Title, r.URL))
		if r.Snippet != "" {
			sb.WriteString("   " + r.Snippet + "\n")
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// ReviewChangesTool shows the current uncommitted diff to the user in the
// interactive modal and lets them approve or roll back the turn's changes.
type ReviewChangesTool struct {
	Ask func(ctx context.Context, questions []AskQuestion) ([]AskResult, error)
}

func (t *ReviewChangesTool) Name() string { return "review_changes" }
func (t *ReviewChangesTool) Description() string {
	return "Show the current uncommitted changes to the user for interactive review. Use after making edits: the user can approve the changes or roll them back."
}
func (t *ReviewChangesTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
func (t *ReviewChangesTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, "git", "diff")
	diffOut, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("not a git repository or git unavailable: %w", err)
	}
	diff := strings.TrimSpace(string(diffOut))
	if diff == "" {
		return "No uncommitted changes to review.", nil
	}
	diff = CapOutput(diff)

	if t.Ask == nil {
		// Headless: return the diff for the model to summarize.
		return "Current uncommitted changes:\n" + diff, nil
	}

	results, err := t.Ask(ctx, []AskQuestion{
		{
			Question: "📝 BroCode made changes — review them:\n\n```diff\n" + diff + "\n```",
			Options:  []string{"✅ Looks good, continue", "↩️ Revert this turn's changes"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("review failed: %w", err)
	}
	if len(results) == 0 || len(results[0].Answers) == 0 {
		return "User skipped the review.", nil
	}
	if strings.Contains(results[0].Answers[0], "Revert") {
		n := RestoreAllSnapshots()
		return fmt.Sprintf("User rolled back the changes (%d files restored).", n), nil
	}
	return "User approved the changes.", nil
}

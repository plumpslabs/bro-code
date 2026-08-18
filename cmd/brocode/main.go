package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/lsp"
	"github.com/plumpslabs/bro-code/internal/mcp"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/subagent"
	"github.com/plumpslabs/bro-code/internal/tool"
	"github.com/plumpslabs/bro-code/internal/ui"
	"github.com/plumpslabs/bro-code/internal/version"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "-v" || os.Args[1] == "--version") {
		fmt.Println(version.Info())
		os.Exit(0)
	}

	flagVersion := flag.Bool("v", false, "Print version and exit")
	flagVersionLong := flag.Bool("version", false, "Print version and exit")
	flagProvider := flag.String("provider", "", "LLM provider ID (opencode, deepseek, poolside, anthropic, openai, openrouter, groq, google, ollama)")
	flagModel := flag.String("model", "", "Model name (e.g. deepseek-v4-flash-free, laguna-s-2.1, claude-3-7-sonnet)")
	flagContinueLong := flag.Bool("continue", false, "Continue most recent active session")
	flagContinueShort := flag.Bool("c", false, "Continue most recent active session (shorthand)")
	flagSession := flag.String("session", "", "Resume specific session ID")
	flagReplay := flag.String("replay", "", "Replay a session's stored trajectory to stdout and exit (no LLM, no TUI)")
	flagBudget := flag.Float64("budget", 0, "Per-task cost cap in USD — the turn stops gracefully once estimated spend exceeds it (0 = unlimited)")
	flagBench := flag.String("bench", "", "Run the benchmark harness on a JSON manifest of cases (file path or single case object) and exit")
	flagLog := flag.String("log", "", "Write a real-time activity log to this file (use `tail -f` in another terminal to monitor what BroCode is doing)")
	flag.Parse()

	if *flagVersion || *flagVersionLong {
		fmt.Println(version.Info())
		os.Exit(0)
	}

	// 0. Replay mode is fully offline: render the session's chronological
	// event log (user prompts, assistant turns, tool calls, tool results,
	// compaction summaries) as plain text and exit — no provider, no TUI.
	if *flagReplay != "" {
		os.Exit(replaySession(*flagReplay))
	}

	// 1. Load Configurations
	cfg := provider.LoadConfig()

	// 2. Auto-Detect Usable Providers
	detected := provider.AutoDetect(cfg)
	if len(detected) == 0 {
		fmt.Println("No active LLM providers found.")
		os.Exit(1)
	}

	// 3. Resolve Active Provider & Model based on Precedence Rules (§10.1)
	var activeProvider provider.DetectedProvider
	if *flagProvider != "" {
		for _, d := range detected {
			if d.Info.ID == *flagProvider {
				activeProvider = d
				break
			}
		}
	}

	if activeProvider.Info.ID == "" && cfg.DefaultProvider != "" {
		for _, d := range detected {
			if d.Info.ID == cfg.DefaultProvider {
				activeProvider = d
				break
			}
		}
	}

	if activeProvider.Info.ID == "" {
		activeProvider = detected[0] // Fallback to auto-detected default (opencode CLI / Zen gateway)
	}

	activeModel := *flagModel
	if activeModel == "" && cfg.DefaultModel != "" && cfg.DefaultModel != "default" {
		// "default" is the placeholder saved when a provider had no model list
		// at connect time — never a real model. Resolve it to the provider's
		// first real model so turns don't fail on the primary provider and
		// silently route through the fallback gateway instead.
		activeModel = cfg.DefaultModel
	}
	if activeModel == "" {
		if len(activeProvider.Info.DefaultModels) > 0 {
			activeModel = activeProvider.Info.DefaultModels[0]
		} else {
			activeModel = "deepseek-v4-flash-free"
		}
	}
	// A saved model ID can be stale relative to the provider's current list
	// (e.g. "laguna-s-2.1" saved before the poolside API required the
	// "poolside/" vendor prefix). Resolve it by suffix so the primary
	// provider works instead of 404-ing and silently falling back.
	activeModel = provider.ResolveModelID(activeProvider.Info.DefaultModels, activeModel)

	// 4. Instantiate Provider Adapter
	var adapter provider.ProviderAdapter
	if activeProvider.Info.ID == "opencode" {
		adapter = provider.NewOpenCodeAdapter()
	} else if activeProvider.Info.Protocol == "anthropic" {
		adapter = provider.NewAnthropicAdapter(activeProvider.Info.DefaultBaseURL, activeProvider.APIKey)
	} else {
		adapter = provider.NewOpenAIAdapter(activeProvider.Info.DefaultBaseURL, activeProvider.APIKey)
	}

	// 4b. Benchmark mode: run the eval harness headless (cases from a JSON
	// manifest) and exit with 0 if every case passed, 1 otherwise. Uses the
	// same resolved provider+model as a normal session.
	if *flagBench != "" {
		os.Exit(runBenchmark(*flagBench, adapter, activeModel))
	}

	// 5. Initialize SQLite Store & Session Management
	st, err := store.NewStore("")
	if err != nil {
		fmt.Printf("Warning: SQLite store initialization failed (%v). Running in-memory.\n", err)
	} else {
		defer st.Close()
	}

	cwd, _ := os.Getwd()
	var sessionID string
	shouldContinue := *flagContinueLong || *flagContinueShort || *flagSession != ""

	if *flagSession != "" {
		sessionID = *flagSession
	} else if (*flagContinueLong || *flagContinueShort) && st != nil {
		sessions, err := st.ListSessionsByProjectPath(cwd)
		if err == nil && len(sessions) > 0 {
			sessionID = sessions[0].ID
		}
	}

	if sessionID == "" {
		sessionID = fmt.Sprintf("sess_%d", time.Now().Unix())
		if st != nil {
			_ = st.CreateSession(sessionID, cwd)
		}
	}

	// Context window follows the active model's declared limit (from the
	// provider config's per-model limit block, e.g. 1M for the free models),
	// falling back to 128k when the model doesn't declare one.
	maxWindow := 128000
	if w := provider.ContextWindowFor(cfg, activeProvider.Info.ID, activeModel); w > 0 {
		maxWindow = w
	}
	ctxMgr := bcontext.NewManager(sessionID, st, maxWindow)
	var initialMessages []string
	// previousPrompts seeds the TUI's up/down prompt-history with the user
	// prompts from the resumed session, so ArrowUp recalls earlier prompts
	// even before anything is typed this run.
	var previousPrompts []string

	if shouldContinue && st != nil {
		// Old resume logic re-persisted the whole log on every `-c`, leaving
		// duplicated history in the database. Purge those before restoring so
		// a resumed session never shows the same prompt multiple times.
		if removed, err := st.CleanupReplayDuplicates(sessionID); err == nil && removed > 0 {
			fmt.Printf("✓ Purged %d duplicated history events\n", removed)
		}
		events, err := st.GetSessionEvents(sessionID)
		if err == nil && len(events) > 0 {
			// Reconstruct the user-facing prompt history from user_msg events
			// (engine-injected reminders like loop guards are filtered out).
			for _, ev := range events {
				if ev.Type != "user_msg" {
					continue
				}
				var msg provider.Message
				if e := json.Unmarshal([]byte(ev.PayloadJSON), &msg); e != nil || msg.Content == "" {
					continue
				}
				if bcontext.IsEngineReminder(msg.Content) {
					continue
				}
				previousPrompts = append(previousPrompts, msg.Content)
			}
			initialMessages = append(initialMessages, fmt.Sprintf("✅ Resumed session %s (%d events total)", sessionID, len(events)))
			// RestoreSession replays only the newest events that fit ~80% of the
			// context window (a session can accumulate thousands of tool-result
			// events), re-pairs tool results with their calls, restores file change
			// summaries inline at their original chronological place, and renders
			// tool-call-only turns as compact summaries instead of raw JSON.
			initialMessages = append(initialMessages, bcontext.RestoreSession(ctxMgr, events)...)
		} else {
			initialMessages = append(initialMessages, fmt.Sprintf("⚡ Continued session %s.", sessionID))
		}
	} else {
		initialMessages = append(initialMessages, "⚡ BroCode engine active. Type a prompt or /help for commands.")
	}

	// 6. Initialize Tool Registry (anchor the permission gate to the project dir)
	tools := tool.NewRegistry()
	tools.SetRepoRoot(cwd)

	// Clear any stale edit backups (.brocode/snapshots) left by a previous
	// session that crashed mid-turn — keeps the cache dir from accumulating.
	tool.PurgeAllSnapshots()

	// Granular per-tool sandbox (.brocode/sandbox.json): deny / allow-only /
	// command patterns. Loaded once; applies to every tool call this session.
	tools.SetSandbox(tool.LoadSandbox(cwd))

	// 7. Load MCP servers (.mcp.json, .brocode/mcp.json, global, opencode)
	mcpMgr := mcp.NewManager()
	mcpMgr.LoadDefaults()
	if len(mcpMgr.ServerNames()) > 0 {
		mcpCtx, mcpCancel := context.WithCancel(context.Background())
		defer mcpCancel()
		mcpMgr.Start(mcpCtx)
		for _, mt := range mcpMgr.Tools() {
			tools.Register(mt)
		}
		for name, errMsg := range mcpMgr.Errors() {
			if errMsg != "" {
				initialMessages = append(initialMessages, fmt.Sprintf("⚠️ MCP server %s failed: %s", name, errMsg))
			}
		}
		if n := len(mcpMgr.Tools()); n > 0 {
			initialMessages = append(initialMessages, fmt.Sprintf("🔌 MCP connected: %d tools from %d servers (%s)", n, len(mcpMgr.ServerNames()), strings.Join(mcpMgr.ServerNames(), ", ")))
		}
		defer mcpMgr.Close()
	}

	// 7b. LSP code intelligence (lazy: language server spawned on first use,
	// warmed in the background at startup so the first lsp_* call is instant;
	// unused servers are reaped after the idle timeout)
	lspMgr := lsp.NewManager()
	lsp.RegisterTools(tools, lspMgr)
	lspMgr.WarmUp(cwd)
	defer lspMgr.Close()

	// 7c. Sub-agents: isolated agent loops sharing the active adapter+model.
	// Registered on the main registry so the model can delegate work to them.
	subRunner := &subagent.Runner{Adapter: adapter, Model: activeModel, Tools: tools}
	tools.Register(&subagent.Tool{Runner: subRunner})

	// 7d. Scout: background research tasks that run WHILE the main turn keeps
	// executing. The scout tool returns a receipt immediately; the engine
	// drains finished findings into the model's context at each loop step.
	scoutMgr := subagent.NewScoutManager(subRunner)
	tools.Register(&subagent.ScoutTool{Manager: scoutMgr})

	// 8. Load the initial/restored history INTO the TUI chat log so the whole
	// conversation (old + new) lives in ONE viewport and scrolls together like
	// a normal terminal (opencode / claude code style). Printing the history to
	// stdout instead split a resumed session into two disconnected regions —
	// old chat stuck in the OS scrollback, new chat in the TUI viewport — so
	// the newest content appeared to detach from the conversation.
	// 8.5 If --log is set, open the activity log so the user can `tail -f` it in
	// another terminal to watch what BroCode is doing during a (possibly slow) turn.
	var activityLog io.Writer
	if *flagLog != "" {
		if lf, lerr := os.OpenFile(*flagLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); lerr == nil {
			defer lf.Close()
			activityLog = lf
			fmt.Fprintf(lf, "=== BroCode activity log %s ===\n", time.Now().Format(time.RFC3339))
		} else {
			fmt.Fprintf(os.Stderr, "⚠️ --log: could not open %s: %v\n", *flagLog, lerr)
		}
	}

	appModel := ui.NewApp(cfg, activeProvider, activeModel, adapter, tools, ctxMgr, mcpMgr, lspMgr, scoutMgr, *flagBudget, previousPrompts, activityLog, initialMessages...)
	p := tea.NewProgram(&appModel)
	appModel.SetProgram(p)

	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running BroCode TUI: %v\n", err)
		os.Exit(1)
	}

	// Session-end memory capture: short sessions that never hit the
	// compaction threshold still leave their goal + touched files in project
	// memory. Deterministic and non-blocking (no LLM call) — it runs after
	// the TUI exits and can never hold the user's quit hostage.
	if st != nil {
		if events, err := st.GetSessionEvents(sessionID); err == nil && len(events) > 0 {
			mem := memory.NewStore(cwd)
			_ = mem.CaptureSession(sessionID, events)
		}
	}
}

// replaySession prints a session's chronological event trajectory as plain
// text and returns a process exit code (0 on success). Fully offline — the
// events are the same append-only log the resume logic replays, re-paired here
// with the mode/model stamps the turn recorded.
func replaySession(sessionID string) int {
	st, err := store.NewStore("")
	if err != nil {
		fmt.Printf("Error opening store: %v\n", err)
		return 1
	}
	defer st.Close()

	events, err := st.GetSessionEvents(sessionID)
	if err != nil {
		fmt.Printf("Error loading session %s: %v\n", sessionID, err)
		return 1
	}
	if len(events) == 0 {
		fmt.Printf("Session %s has no visible events.\n", sessionID)
		return 1
	}

	var sessions []store.Session
	if all, lerr := st.ListSessions(); lerr == nil {
		sessions = all
	}

	fmt.Print(renderReplay(sessionID, events, sessions))
	return 0
}

// renderReplay renders a session's chronological event trajectory as text.
// Extracted from replaySession so the output can be unit-tested offline.
func renderReplay(sessionID string, events []store.Event, sessions []store.Session) string {
	var b strings.Builder

	var sessionMeta string
	for _, s := range sessions {
		if s.ID == sessionID {
			sessionMeta = fmt.Sprintf("project: %s | status: %s | created: %s",
				s.ProjectPath, s.Status, s.CreatedAt.Format("2006-01-02 15:04:05"))
			break
		}
	}

	fmt.Fprintf(&b, "=== Replay: %s ===\n", sessionID)
	if sessionMeta != "" {
		fmt.Fprintf(&b, "%s\n", sessionMeta)
	}
	b.WriteString(strings.Repeat("─", 64))

	for _, ev := range events {
		fmt.Fprintf(&b, "\n[#%d] %s (%s)\n", ev.Seq, ev.Type, ev.CreatedAt.Format("15:04:05"))
		switch ev.Type {
		case "user_msg", "assistant_msg", "tool_result", "compaction_summary":
			var msg provider.Message
			if json.Unmarshal([]byte(ev.PayloadJSON), &msg) != nil {
				b.WriteString("  <unparseable payload>\n")
				continue
			}
			printEventBody(&b, ev.Type, msg)
		default:
			fmt.Fprintf(&b, "  %s\n", singleLine(ev.PayloadJSON, 240))
		}
	}
	return b.String()
}

// printEventBody renders one parsed event payload in a human-readable form,
// re-pairing tool calls with their arguments and showing the mode/model stamp.
func printEventBody(b *strings.Builder, evType string, msg provider.Message) {
	switch evType {
	case "user_msg":
		if msg.ToolCallID != "" {
			fmt.Fprintf(b, "  [tool result → %s]\n", msg.ToolCallID)
			fmt.Fprintf(b, "  %s\n", singleLine(msg.Content, 300))
			return
		}
		fmt.Fprintf(b, "  %s\n", singleLine(msg.Content, 400))
	case "assistant_msg":
		stamp := ""
		if msg.Mode != "" {
			stamp = fmt.Sprintf("[%s/%s]", msg.Mode, msg.Model)
		}
		if msg.Reasoning != "" {
			fmt.Fprintf(b, "  %s reasoning: %s\n", stamp, singleLine(msg.Reasoning, 200))
		}
		if msg.Content != "" {
			fmt.Fprintf(b, "  %s answer: %s\n", stamp, singleLine(msg.Content, 400))
		}
		for _, tc := range msg.ToolCalls {
			fmt.Fprintf(b, "  → %s(%s)\n", tc.Name, singleLine(tc.Arguments, 200))
		}
	case "tool_result":
		fmt.Fprintf(b, "  %s\n", singleLine(msg.Content, 300))
	case "compaction_summary":
		fmt.Fprintf(b, "  %s\n", singleLine(msg.Content, 300))
	}
}

// singleLine flattens multi-line text to one line for compact replay output.
func singleLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

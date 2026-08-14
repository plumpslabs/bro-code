package main

import (
	"context"
	"flag"
	"fmt"
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
)

func main() {
	flagProvider := flag.String("provider", "", "LLM provider ID (opencode, deepseek, poolside, anthropic, openai, openrouter, groq, google, ollama)")
	flagModel := flag.String("model", "", "Model name (e.g. deepseek-v4-flash-free, laguna-s-2.1, claude-3-7-sonnet)")
	flagContinueLong := flag.Bool("continue", false, "Continue most recent active session")
	flagContinueShort := flag.Bool("c", false, "Continue most recent active session (shorthand)")
	flagSession := flag.String("session", "", "Resume specific session ID")
	flag.Parse()

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
	if activeModel == "" && cfg.DefaultModel != "" {
		activeModel = cfg.DefaultModel
	}
	if activeModel == "" {
		if len(activeProvider.Info.DefaultModels) > 0 {
			activeModel = activeProvider.Info.DefaultModels[0]
		} else {
			activeModel = "deepseek-v4-flash-free"
		}
	}

	// 4. Instantiate Provider Adapter
	var adapter provider.ProviderAdapter
	if activeProvider.Info.ID == "opencode" {
		adapter = provider.NewOpenCodeAdapter()
	} else if activeProvider.Info.Protocol == "anthropic" {
		adapter = provider.NewAnthropicAdapter(activeProvider.Info.DefaultBaseURL, activeProvider.APIKey)
	} else {
		adapter = provider.NewOpenAIAdapter(activeProvider.Info.DefaultBaseURL, activeProvider.APIKey)
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

	if shouldContinue && st != nil {
		// Old resume logic re-persisted the whole log on every `-c`, leaving
		// duplicated history in the database. Purge those before restoring so
		// a resumed session never shows the same prompt multiple times.
		if removed, err := st.CleanupReplayDuplicates(sessionID); err == nil && removed > 0 {
			fmt.Printf("✓ Purged %d duplicated history events\n", removed)
		}
		events, err := st.GetSessionEvents(sessionID)
		if err == nil && len(events) > 0 {
			initialMessages = append(initialMessages, fmt.Sprintf("✅ Resumed session %s (%d events total)", sessionID, len(events)))
			// RestoreSession replays only the newest events that fit ~80% of the
			// context window (a session can accumulate thousands of tool-result
			// events), re-pairs tool results with their calls, and renders
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

	// 8. Launch Bubble Tea v2 App
	appModel := ui.NewApp(cfg, activeProvider, activeModel, adapter, tools, ctxMgr, mcpMgr, lspMgr, scoutMgr, initialMessages...)
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

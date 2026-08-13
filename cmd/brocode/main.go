package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
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

	ctxMgr := bcontext.NewManager(sessionID, st, 128000)
	var initialMessages []string

	if shouldContinue && st != nil {
		events, err := st.GetSessionEvents(sessionID)
		if err == nil && len(events) > 0 {
			initialMessages = append(initialMessages, fmt.Sprintf("✅ Resumed session %s (%d events restored)", sessionID, len(events)))
			for _, ev := range events {
				text := bcontext.ExtractEventContent(ev.PayloadJSON)
				if ev.Type == "user_msg" {
					_ = ctxMgr.AppendUserMessage(text)
					initialMessages = append(initialMessages, "YOU:\n"+text)
				} else if ev.Type == "assistant_msg" {
					_ = ctxMgr.AppendAssistantTurn("", text, nil)
					initialMessages = append(initialMessages, "BROCODE:\n"+text)
				}
			}
		} else {
			initialMessages = append(initialMessages, fmt.Sprintf("⚡ Continued session %s.", sessionID))
		}
	} else {
		initialMessages = append(initialMessages, "⚡ BroCode engine active. Type a prompt or /help for commands.")
	}

	// 6. Initialize Tool Registry
	tools := tool.NewRegistry()

	// 7. Launch Bubble Tea v2 App
	appModel := ui.NewApp(cfg, activeProvider, activeModel, adapter, tools, ctxMgr, initialMessages...)
	p := tea.NewProgram(&appModel)
	appModel.SetProgram(p)

	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running BroCode TUI: %v\n", err)
		os.Exit(1)
	}
}

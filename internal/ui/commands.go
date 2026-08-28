package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/plumpslabs/bro-code/internal/agent"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/lsp"
	"github.com/plumpslabs/bro-code/internal/mcp"
	"github.com/plumpslabs/bro-code/internal/plan"
	"github.com/plumpslabs/bro-code/internal/provenance"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/repo"
	"github.com/plumpslabs/bro-code/internal/report"
	"github.com/plumpslabs/bro-code/internal/subagent"
	"github.com/plumpslabs/bro-code/internal/tool"
	"github.com/plumpslabs/bro-code/internal/version"
)

func (m *Model) handleSlashCommand(cmd string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(cmd)
	switch parts[0] {
	case "/help":
		helpContent := `### 🚀 Core Engineering Commands
- **` + "`/ask <question>`" + `** — Ephemeral Codebase QA: Ask questions without polluting context
- **` + "`/spec <feature>`" + `** — Spec-First Gate: Draft an architectural blueprint contract before coding
- **` + "`/tournament <task>`" + `** — Multi-Candidate Solver: Run 2 parallel candidate agents on difficult bugs
- **` + "`/repair`" + `** — AI-Powered Error Repair: Auto-fix type errors & warnings from diagnostics
- **` + "`/plan`" + `** — Inspect active plan checklist (` + "`/plan archive`" + ` to archive)
- **` + "`/undo`" + `** — Time-Travel Rollback: Revert all file changes made in the last turn
- **` + "`/diff`" + `** — Show file diff (` + "`/diff <path>`" + ` for specific file)
- **` + "`/diagnose`" + `** — Scan project for type errors/warnings (` + "`/diagnose fix`" + ` to auto-fix)
- **` + "`/trace`" + `** — Code Provenance: Trace who/what introduced a change (` + "`/trace <commit>`" + `)
- **` + "`/cost`" + `** — Live token usage & spend telemetry (USD & IDR)

### ⚙️ Sessions & Configuration
- **` + "`/copy`" + `** — Copy the latest assistant response directly to OS clipboard
- **` + "`/mouse`" + `** — Toggle mouse mode (SELECT for native drag copy vs SCROLL for wheel)
- **` + "`/models`" + `** — Interactive model picker
- **` + "`/model <id>`" + `** — Quick model switch (e.g. ` + "`/model deepseek-v4-flash-free`" + `)
- **` + "`/connect`" + `** — 2-step API Key & provider setup wizard
- **` + "`/sessions`" + `**, **` + "`/history`" + `** — Switch or manage past sessions
- **` + "`/memory`" + `** — Inspect cross-session project memory
- **` + "`/mcp`" + `** — Manage connected MCP servers & tools (` + "`/mcp-reload`" + ` to refresh)
- **` + "`/lsp`" + `** — Code intelligence status (` + "`/lsp-install`" + ` to install servers)
- **` + "`/workspace`" + `** — Inspect multi-repo workspace structure
- **` + "`/search-key`" + `** — Set web search API key (Tavily/Exa)
- **` + "`/context7-key`" + `** — Set Context7 documentation API key
- **` + "`/agents`" + `** — List custom agents (` + "`/agent <name>`" + ` to switch)
- **` + "`/worktree`" + `** — Git worktree management for parallel branches
- **` + "`/report`" + `** — Export session report (` + "`/report <session>`" + `)
- **` + "`/clear`" + `**, **` + "`/new`" + `** — Clear chat or start fresh session
- **` + "`/update`" + `** — Self-update to latest version

### 🔀 Modes (Toggle with ` + "`Shift+Tab`" + `)
- **` + "`BUILDER`" + `** *(Default)* — Autonomous coding agent with full read, write, edit, & run tools
- **` + "`/builder`" + `** — Switch to Builder mode
- **` + "`PLANNER`" + `** — Read-only architecture & strategy agent
- **` + "`/planner`" + `** — Switch to Planner mode
- **` + "`MINER`" + `** — Read-only knowledge mining agent that persists facts to memory
- **` + "`/miner`" + `** — Switch to Miner mode

### 🎹 Keybindings
- **` + "`Shift+Tab`" + `** — Toggle mode (BUILDER/PLANNER/MINER)
- **` + "`Ctrl+K`" + `** / **` + "`Alt+K`" + `** — Prompt queue management
- **` + "`Ctrl+P`" + `** — Pager (full-screen answer viewer)
- **` + "`Ctrl+F`" + `** — Toggle file change diff expansion
- **` + "`Ctrl+Y`" + `** — Copy last answer to clipboard
- **` + "`Ctrl+M`" + `** — Toggle mouse mode (scroll/select)
- **` + "`Tab`" + `** — Autocomplete file mentions
- **` + "`Alt+Enter`" + `** — Insert newline in prompt
- **` + "`PgUp`" + `** / **` + "`PgDn`" + `** — Scroll chat history`
		m.appendNote("HELP:\n" + helpContent)

	case "/miner":
		m.mode = "MINER"
		m.engine.SetMode(m.mode)
		m.persistMode()
		m.appendNote("MODE:MINER\n⛏️ MINER mode active — explore the codebase and I'll persist verified knowledge (architecture, build commands, conventions, decisions, gotchas) into project memory. Shift+Tab to switch back to BUILDER.")

	case "/builder":
		m.mode = "BUILDER"
		m.engine.SetMode(m.mode)
		m.persistMode()
		m.appendNote("MODE:BUILDER\n🔨 BUILDER mode active — autonomous coding agent with full read, write, edit, and execution capabilities.")

	case "/planner":
		m.mode = "PLANNER"
		m.engine.SetMode(m.mode)
		m.persistMode()
		m.appendNote("MODE:PLANNER\n📋 PLANNER mode active — read-only architecture and strategy agent.")

	case "/mode":
		if len(parts) > 1 {
			target := strings.ToUpper(strings.TrimSpace(parts[1]))
			if target == "BUILDER" || target == "PLANNER" || target == "MINER" {
				m.mode = target
				m.engine.SetMode(m.mode)
				m.persistMode()
				m.appendNote(fmt.Sprintf("MODE:%s\n✅ Mode switched to %s", m.mode, m.mode))
				return m, nil
			}
		}
		m.appendNote("Usage: /mode <builder|planner|miner> (or toggle with Shift+Tab)")

	case "/plan":
		cwd, _ := os.Getwd()
		if len(parts) > 1 && (parts[1] == "archive" || parts[1] == "clear" || parts[1] == "reset") {
			archPath, err := plan.ArchiveCurrentPlan(cwd)
			if err != nil {
				m.appendNote("PLAN:\n" + fmt.Sprintf("⚠️ **Failed to archive plan:** %v\n\n*(No active plan found in `.brocode/current_plan.md`)*", err))
			} else {
				relPath := archPath
				if rel, err := filepath.Rel(cwd, archPath); err == nil {
					relPath = rel
				}
				m.appendNote("PLAN:\n" + fmt.Sprintf("📦 **Plan archived successfully!**\n\nSaved to: `%s`\n\n💡 Current active plan has been cleared. Switch to **PLANNER** (`Shift+Tab`) to draft a fresh goal.", relPath))
			}
			return m, nil
		}
		curPlan, err := plan.LoadCurrentPlan(cwd)
		if err != nil || curPlan == nil || len(curPlan.Steps) == 0 {
			m.appendNote("PLAN:\n" + "ℹ️ **No active plan found in `.brocode/current_plan.md`**\n\nSwitch to **PLANNER** mode (`Shift+Tab` or `/planner`) to draft an execution plan for your next feature or bugfix.")
		} else {
			m.appendNote("PLAN:\n" + plan.RenderMarkdownPlan(curPlan))
		}

	case "/memory":
		if m.memStore != nil {
			s := m.memStore.List()
			if strings.TrimSpace(s) == "" {
				s = "ℹ️ No long-term project memory entries recorded yet.\n\nSwitch to **MINER** mode (`Shift+Tab` or `/miner`) to explore and persist verified architecture, conventions, and decisions to `.brocode/memory.md`."
			}
			if m.memStore.Path() != "" {
				s += "\n\n📍 *" + m.memStore.Path() + "*"
			}
			m.appendMessages("MEMORY:\n" + s)
		} else {
			m.appendMessages("⚠️ Project memory not initialized.")
		}

	case "/cost":
		m.appendMessages("COST:\n" + m.engine.CostSummary())

	case "/ask":
		query := strings.TrimSpace(strings.TrimPrefix(cmd, "/ask"))
		if query == "" {
			m.appendNote("NOTE:\n❓ **Usage: `/ask <question>`**\n\nAsk an isolated question about the codebase without polluting your active task context.\n\n**Example:** `/ask Where is the WhatsApp webhook handler defined?`")
			return m, nil
		}
		if m.scoutMgr == nil || m.scoutMgr.Runner == nil {
			m.appendNote("NOTE:\n⚠️ **Subagent runner is not initialized for /ask.**")
			return m, nil
		}
		m.appendNote("CMD:/ask\n" + query)
		m.status = fmt.Sprintf("Answering: %s...", truncatePrompt(query))
		m.turnStart = time.Now()
		runner := m.scoutMgr.Runner
		recentCtx := extractRecentSessionContext(m.messages, 4)
		askPrompt := fmt.Sprintf(
			"%s"+
				"You are an expert codebase answering assistant. The user is asking:\n\n"+
				"\"%s\"\n\n"+
				"Instructions:\n"+
				"1. Be helpful, perceptive, and interpret informal phrasing or typos intelligently.\n"+
				"2. Language: Formulate your answer in the user's language (e.g. Bahasa Indonesia).\n"+
				"3. Working Directory: You are ALREADY in the project repository root. Do NOT attempt to run 'cd' or switch directories.\n"+
				"4. Search and inspect the actual repository using codebase tools (code_locate, grep, read_file, glob) to find relevant code, models, services, functions, and configs.\n"+
				"5. If git history or diffs are requested (e.g. comparing before/after changes), execute read-only git commands directly (e.g. 'git log -n 10 --oneline', 'git diff HEAD~1', 'git show HEAD') without using 'cd'.\n"+
				"6. Provide a clear, direct, and structured explanation citing exact file paths and code references.\n"+
				"7. Anti-loop efficiency: Once you locate the relevant functions/schemas, synthesize and output your answer directly instead of repeatedly reading file slices in small chunks.\n"+
				"8. Do NOT edit or modify any files.",
			recentCtx, query,
		)
		prog := m.prog
		return m, tea.Batch(tickCmd(), func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			ans, err := runner.RunWithProgress(ctx, askPrompt, "BUILDER", func(state loop.LoopState, info string) {
				if prog != nil {
					prog.Send(stepProgressMsg{state: state, info: info})
				}
			})
			if err != nil {
				return ephemeralAskResultMsg(fmt.Sprintf("❌ `/ask` query failed: %v", err))
			}
			return ephemeralAskResultMsg(fmt.Sprintf("ASK:\n%s\n---\n%s", query, ans))
		})

	case "/spec":
		feature := strings.TrimSpace(strings.TrimPrefix(cmd, "/spec"))
		if feature == "" {
			m.appendNote("NOTE:\n📋 **Usage: `/spec <feature description>`**\n\nDraft an Architectural Blueprint Specification Contract (ADR, endpoints, data models, blast radius) before writing code.\n\n**Example:** `/spec Multi-channel Webhook Dispatcher`")
			return m, nil
		}
		if m.scoutMgr == nil || m.scoutMgr.Runner == nil {
			m.appendNote("NOTE:\n⚠️ **Subagent runner is not initialized for /spec.**")
			return m, nil
		}
		m.appendNote("CMD:/spec\n" + feature)
		m.status = fmt.Sprintf("Drafting spec: %s...", truncatePrompt(feature))
		m.turnStart = time.Now()
		return m, executeSpecCommand(m.scoutMgr.Runner, feature, m.prog)

	case "/tournament":
		task := strings.TrimSpace(strings.TrimPrefix(cmd, "/tournament"))
		if task == "" {
			m.appendNote("NOTE:\n🏆 **Usage: `/tournament <bug or complex task>`**\n\nRun 2 parallel candidate agents with distinct strategies to find the cleanest verified solution.\n\n**Example:** `/tournament Fix race condition in connection pooling`")
			return m, nil
		}
		if m.scoutMgr == nil || m.scoutMgr.Runner == nil {
			m.appendNote("NOTE:\n⚠️ **Subagent runner is not initialized for /tournament.**")
			return m, nil
		}
		m.appendNote("CMD:/tournament\n" + task)
		m.status = fmt.Sprintf("Running tournament: %s...", truncatePrompt(task))
		m.turnStart = time.Now()
		return m, executeTournamentCommand(m.scoutMgr.Runner, task, m.prog)

	case "/update", "/upgrade":
		m.status = "🚀 Checking for updates & self-updating in background..."
		m.turnStart = time.Now()
		return m, tea.Batch(tickCmd(), func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			latest, hasUpdate, err := version.CheckLatestVersion(ctx, true)
			if err != nil {
				return ephemeralAskResultMsg(fmt.Sprintf("UPDATE:\n❌ Update check failed: %v", err))
			}
			if !hasUpdate {
				return ephemeralAskResultMsg(fmt.Sprintf("UPDATE:\n✨ You are already on the latest version of BroCode (**%s**)!\n\nNo upgrade is needed at this time.", version.Version))
			}
			msg, err := version.SelfUpdate(ctx, latest)
			if err != nil {
				return ephemeralAskResultMsg(fmt.Sprintf("UPDATE:\n❌ Upgrade failed: %v\n\nYou can manually upgrade with:\n• Windows: `irm https://raw.githubusercontent.com/plumpslabs/bro-code/main/scripts/install.ps1 | iex`\n• macOS/Linux: `curl -fsSL https://raw.githubusercontent.com/plumpslabs/bro-code/main/scripts/install.sh | bash`", err))
			}
			return ephemeralAskResultMsg(fmt.Sprintf("UPDATE:\n%s\n\n👉 Please restart BroCode to run version **%s**.", msg, latest))
		})

	case "/repair":
		errCtx := strings.TrimSpace(strings.TrimPrefix(cmd, "/repair"))
		if m.scoutMgr == nil || m.scoutMgr.Runner == nil {
			m.appendNote("REPAIR:\n⚠️ Subagent runner is not initialized for /repair.")
			return m, nil
		}
		m.appendNote("CMD:/repair\n" + errCtx)
		m.status = "Pipeline Doctor: Diagnosing & fixing failures..."
		m.turnStart = time.Now()
		return m, executeRepairCommand(m.scoutMgr.Runner, errCtx, m.prog)

	case "/diff":
		targetPath := strings.TrimSpace(strings.TrimPrefix(cmd, "/diff"))
		w := m.width
		if w <= 0 {
			w = 100
		}
		diffOut := GenerateSessionDiffSummary(targetPath, w)
		m.appendNote(diffOut)
		return m, nil

	case "/worktree":
		parts := strings.Fields(strings.TrimPrefix(cmd, "/worktree"))
		cwd, _ := os.Getwd()
		wm := tool.NewWorktreeManager(cwd)

		if len(parts) == 0 {
			list, err := wm.ListWorktrees()
			if err != nil || len(list) == 0 {
				m.appendNote("WORKTREE:\nNo isolated background worktrees active.\n\nUsage: `/worktree <task description>` to run an autonomous task in an isolated branch.\n\nSub-commands:\n• `/worktree list` — List all active worktree branches\n• `/worktree merge <branch>` — Merge worktree branch to main\n• `/worktree clean` — Remove all isolated worktrees")
				return m, nil
			}
			var sb strings.Builder
			for _, wt := range list {
				sb.WriteString(fmt.Sprintf("• **%s** (Branch: `%s`)\n  Path: `%s`\n\n", filepath.Base(wt.Directory), wt.Branch, wt.Directory))
			}
			sb.WriteString("👉 Merge a finished worktree with `/worktree merge <branch>` or delete with `/worktree clean`.")
			m.appendNote("WORKTREE:\n" + sb.String())
			return m, nil
		}

		sub := strings.ToLower(parts[0])
		switch sub {
		case "list":
			list, _ := wm.ListWorktrees()
			if len(list) == 0 {
				m.appendNote("WORKTREE:\nNo active isolated worktrees found.")
				return m, nil
			}
			var sb strings.Builder
			for _, wt := range list {
				sb.WriteString(fmt.Sprintf("• **%s** (Branch: `%s`)\n  Path: `%s`\n\n", filepath.Base(wt.Directory), wt.Branch, wt.Directory))
			}
			m.appendNote("WORKTREE:\n" + sb.String())
			return m, nil

		case "merge":
			if len(parts) < 2 {
				m.appendNote("WORKTREE:\nUsage: `/worktree merge <branch-name>`")
				return m, nil
			}
			branch := parts[1]
			out, err := wm.MergeWorktree(branch)
			if err != nil {
				m.appendNote(fmt.Sprintf("WORKTREE:\n❌ Merge failed: %v\nOutput:\n%s", err, out))
			} else {
				m.appendNote(fmt.Sprintf("WORKTREE:\n✅ Successfully merged branch `%s` into active workspace!", branch))
			}
			return m, nil

		case "clean":
			worktreeRoot := filepath.Join(cwd, ".brocode", "worktrees")
			_ = os.RemoveAll(worktreeRoot)
			m.appendNote("WORKTREE:\n🧹 Cleaned up all isolated worktrees in `.brocode/worktrees/`.")
			return m, nil

		default:
			task := strings.Join(parts, " ")
			if m.scoutMgr == nil || m.scoutMgr.Runner == nil {
				m.appendNote("WORKTREE:\n⚠️ Subagent runner is not initialized for /worktree.")
				return m, nil
			}
			wtDir, branch, err := wm.CreateWorktree(task)
			if err != nil {
				m.appendNote(fmt.Sprintf("WORKTREE:\n❌ Failed to create worktree: %v", err))
				return m, nil
			}
			m.appendNote(fmt.Sprintf("WORKTREE:\n🌿 Spawned isolated worktree: `%s` (Branch: `%s`)\n\nStarting background agent in sandbox...", wtDir, branch))
			m.status = fmt.Sprintf("Running isolated worktree task: %s...", truncatePrompt(task))
			m.turnStart = time.Now()

			return m, tea.Batch(tickCmd(), func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
				defer cancel()
				subTask := subagent.SubAgent{
					ID:        "worktree_" + branch,
					Task:      fmt.Sprintf("Work in directory %s to implement: %s", wtDir, task),
					Mode:      "BUILDER",
					TargetDir: wtDir,
					Mutates:   true,
				}
				answers, rErr := m.scoutMgr.Runner.RunMany(ctx, []subagent.SubAgent{subTask}, false, nil)
				if rErr != nil || len(answers) == 0 {
					return ephemeralAskResultMsg(fmt.Sprintf("WORKTREE:\n❌ Worktree task failed: %v", rErr))
				}
				return ephemeralAskResultMsg(fmt.Sprintf("WORKTREE:\n%s\n---\n✅ Branch: `%s`\nType `/worktree merge %s` to merge into main workspace.", answers[0], branch, branch))
			})
		}

	case "/trace", "/provenance":
		commitRef := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(cmd, "/provenance"), "/trace"))
		if commitRef == "" {
			commitRef = "HEAD"
		}
		cwd, _ := os.Getwd()
		att, valid, err := provenance.VerifyCommitAttestation(cwd, commitRef)
		if err != nil {
			m.appendNote(fmt.Sprintf("PROVENANCE:\n⚠️ No cryptographic AI attestation found for commit `%s`.\n\nError: %v\n\n💡 Tip: BroCode automatically generates cryptographic Merkle provenance on AI commits.", commitRef, err))
			return m, nil
		}

		statusStr := "✅ Valid & Verified (Tamper-Free)"
		if !valid {
			statusStr = "❌ TAMPER DETECTED / HASH MISMATCH"
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("• **Verification Status:** %s\n", statusStr))
		if att.ModelID != "" {
			sb.WriteString(fmt.Sprintf("• **AI Model:** `%s`\n", att.ModelID))
		}
		if att.AgentVersion != "" {
			sb.WriteString(fmt.Sprintf("• **Agent Version:** `%s`\n", att.AgentVersion))
		}
		if att.SessionID != "" {
			sb.WriteString(fmt.Sprintf("• **Session / Turn:** `%s` (Turn %d)\n", att.SessionID, att.TurnID))
		}
		if !att.Timestamp.IsZero() {
			sb.WriteString(fmt.Sprintf("• **Timestamp:** %s\n", att.Timestamp.Format(time.RFC3339)))
		}
		if att.TokenCostUSD > 0 {
			sb.WriteString(fmt.Sprintf("• **Turn Token Cost:** `$%.4f`\n", att.TokenCostUSD))
		}
		if att.LSPClean {
			sb.WriteString("• **LSP Diagnostics:** 🟢 Clean (0 errors before commit)\n")
		}
		if att.TestsPassed {
			sb.WriteString("• **Automated Tests:** 🟢 Passed (Exit 0)\n")
		}
		if att.UserPrompt != "" {
			sb.WriteString(fmt.Sprintf("\n📝 **Origin User Prompt:**\n> %s\n\n", att.UserPrompt))
		}
		sb.WriteString(fmt.Sprintf("• **Prompt Hash:** `%s`\n", att.PromptHash))
		sb.WriteString(fmt.Sprintf("• **Diff Hash:** `%s`\n", att.DiffHash))
		sb.WriteString(fmt.Sprintf("• **Merkle Proof:** `%s`\n", att.ProofHash))

		m.appendNote("PROVENANCE:\n" + sb.String())
		return m, nil

	case "/search-key", "/search":
		arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(cmd, "/search-key"), "/search"))
		if arg == "" {
			st := provider.GetSearchProviderStatus()
			if st.PrimaryProvider != "free" {
				maskedPrimary := st.PrimaryKey
				if len(maskedPrimary) > 8 {
					maskedPrimary = maskedPrimary[:4] + "..." + maskedPrimary[len(maskedPrimary)-4:]
				}
				if st.SecondaryProvider != "" {
					maskedSecondary := st.SecondaryKey
					if len(maskedSecondary) > 8 {
						maskedSecondary = maskedSecondary[:4] + "..." + maskedSecondary[len(maskedSecondary)-4:]
					}
					m.appendNote(fmt.Sprintf("SEARCH:\n• **Mode**: Multi-Tier AI Web Search (Active Cascade)\n• **Primary Provider**: %s (`%s`)\n• **Fallback Provider**: %s (`%s`)\n• **Fallback 2**: Zero-Config Free Engine (DuckDuckGo)\n• **Footer Badge**: `%s`\n\n👉 **Management Commands**:\n• Change primary: `/search-key <key>`\n• Reset to Free Mode: `/search-key clear`", strings.ToUpper(st.PrimaryProvider), maskedPrimary, strings.ToUpper(st.SecondaryProvider), maskedSecondary, strings.TrimPrefix(st.Badge, " · ")))
				} else {
					quotaInfo := "1,000 Free Searches/Month (tavily.com)"
					if st.PrimaryProvider == "exa" {
						quotaInfo = "Exa Neural Search API (exa.ai)"
					}
					m.appendNote(fmt.Sprintf("SEARCH:\n• **Provider**: %s\n• **API Key**: `%s`\n• **Mode**: Dedicated High-Speed AI Web Search\n• **Quota**: %s\n• **Footer Badge**: `%s`\n\n👉 **Management Commands**:\n• Set Tavily key: `/search-key tvly-xxxx` (or `/search-key tavily <key>`)\n• Set Exa key: `/search-key exa-xxxx` (or `/search-key exa <key>`)\n• Reset to Free Mode: `/search-key clear`", strings.ToUpper(st.PrimaryProvider), maskedPrimary, quotaInfo, strings.TrimPrefix(st.Badge, " · ")))
				}
			} else {
				m.appendNote("SEARCH:\n• **Current Status**: Zero-Config Free Mode (DuckDuckGo HTML / Lite / Wikipedia)\n\n👉 **Want dedicated, instant web search with 1,000 free searches/month?**\n1. Sign up for free at **https://tavily.com** (no credit card needed)\n2. Copy your API key (starts with `tvly-...`)\n3. Run: `/search-key tvly-xxxxxxxxxxxx`\n\nBroCode will save it permanently to `~/.config/brocode/config.json` and display `🌐:Tavily` in the bottom status bar!\n\n*(Also supports Exa AI via `/search-key exa <key>` or `/search-key exa-xxxx`)*")
			}
			return m, nil
		}

		lower := strings.ToLower(arg)
		if lower == "clear" || lower == "reset" || lower == "delete" || lower == "remove" {
			_ = provider.SaveSearchKey("")
			m.cfg = provider.LoadConfig()
			m.appendNote("SEARCH:\n🧹 **Search Key Cleared & Removed!**\n\nBroCode has switched to **Zero-Config Free Search Mode** (`🌐:Free`).")
			return m, nil
		}

		parts := strings.Fields(arg)
		prov := ""
		key := arg
		if len(parts) == 2 && (strings.EqualFold(parts[0], "tavily") || strings.EqualFold(parts[0], "exa")) {
			prov = strings.ToLower(parts[0])
			key = parts[1]
		}

		if err := provider.SaveSearchProviderKey(prov, key); err != nil {
			m.appendNote(fmt.Sprintf("SEARCH:\n❌ Failed to save search key: %v", err))
			return m, nil
		}
		m.cfg = provider.LoadConfig()
		_, activeProv := provider.GetActiveSearchKey()
		if activeProv == "" {
			activeProv = "tavily"
		}
		m.appendNote(fmt.Sprintf("SEARCH:\n✅ **Web Search Provider Configured Successfully!**\n\n• **Provider**: %s\n• **Status**: Active & Persisted to `~/.config/brocode/config.json`\n• **Bottom Bar**: `🌐:%s` (Active)\n\nBroCode web search is now configured for high-speed documentation and web research!", strings.ToUpper(activeProv), strings.ToUpper(activeProv)))
		return m, nil

	case "/context7-key", "/c7-key", "/context7":
		arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(cmd, "/context7-key"), "/c7-key"), "/context7"))
		if arg == "" {
			k := provider.GetActiveContext7Key()
			if k != "" {
				masked := k
				if len(k) > 8 {
					masked = k[:4] + "..." + k[len(k)-4:]
				}
				m.appendNote(fmt.Sprintf("CONTEXT7:\n• **Provider**: Context7 Official Docs API (Native REST)\n• **API Key**: `%s`\n• **Status**: Active & Verified\n• **Docs Cascade**: Layer 1 (Local/AST) ➔ Layer 2 (Context7) ➔ Layer 3 (Web Search)\n\n👉 **Management Commands**:\n• Change key: `/context7-key <new-key>`\n• Remove key: `/context7-key clear`", masked))
			} else {
				m.appendNote("CONTEXT7:\n• **Current Status**: Unconfigured (Using Web Search Fallback)\n\n👉 **Want instant, up-to-date official library documentation (Next.js, Tailwind, FastAPI, etc.)?**\n1. Sign up for free at **https://context7.com**\n2. Copy your API key (or use dashboard token)\n3. Run: `/context7-key c7_xxxxxxxxxxxx`\n\nBroCode will save it permanently to `~/.config/brocode/config.json` for zero-latency, verified docs resolution!")
			}
			return m, nil
		}

		lower := strings.ToLower(arg)
		if lower == "clear" || lower == "reset" || lower == "delete" || lower == "remove" {
			_ = provider.SaveContext7Key("")
			m.cfg = provider.LoadConfig()
			m.appendNote("CONTEXT7:\n🧹 **Context7 API Key Cleared & Removed!**\n\nBroCode documentation lookup will fall back to Web Search.")
			return m, nil
		}

		if err := provider.SaveContext7Key(arg); err != nil {
			m.appendNote(fmt.Sprintf("CONTEXT7:\n❌ Failed to save Context7 API key: %v", err))
			return m, nil
		}
		m.cfg = provider.LoadConfig()
		m.appendNote("CONTEXT7:\n✅ **Context7 API Key Configured Successfully!**\n\n• **Mode**: Native High-Speed REST Client (Zero Node.js overhead)\n• **Status**: Active & Persisted to `~/.config/brocode/config.json`\n• **Tool**: `doc_lookup` (Automatic 3-Tier Docs Cascade)\n\nBroCode is now ready to query official documentation directly!")
		return m, nil

	case "/agents":
		cwd, _ := os.Getwd()
		loader := agent.NewLoader(cwd)
		list := loader.All()
		if len(list) == 0 {
			m.appendNote("AGENTS:\nNo custom agents found.\n\nCreate custom agents in `.brocode/agents/*.md` (project) or `~/.config/brocode/agents/*.md` (global).\n\nExample file `.brocode/agents/auditor.md`:\n```markdown\n---\nname: auditor\ndescription: Security Auditor\nmode: PLANNER\ntools:\n  allow: [read_file, grep, code_locate]\n---\nAudit security and code quality...\n```")
			return m, nil
		}
		var sb strings.Builder
		for _, ag := range list {
			src := "global (~/.config/brocode/agents)"
			if ag.IsProject {
				src = "project (.brocode/agents)"
			}
			active := ""
			if m.activeAgent != nil && strings.EqualFold(m.activeAgent.Name, ag.Name) {
				active = " 🟢 [ACTIVE]"
			}
			fmt.Fprintf(&sb, "• **%s**%s [%s]\n  %s\n  Mode: %s | Source: %s\n\n",
				ag.Name, active, ag.Description, truncatePrompt(ag.Prompt), ag.Mode, src)
		}
		sb.WriteString("👉 Activate an agent with `/agent <name>` (or `/agent reset` to return to default).")
		m.appendNote("AGENTS:\n" + sb.String())
		return m, nil

	case "/agent":
		agentName := strings.TrimSpace(strings.TrimPrefix(cmd, "/agent"))
		if agentName == "" {
			if m.activeAgent != nil {
				m.appendNote(fmt.Sprintf("NOTE:\n🟢 **Custom Agent Active: %s**\n\n- **Description:** %s\n- **Mode:** %s\n- **Path:** `%s`\n\nType `/agent reset` to deactivate.",
					m.activeAgent.Name, m.activeAgent.Description, m.activeAgent.Mode, m.activeAgent.Path))
			} else {
				m.appendNote("NOTE:\nℹ️ **No custom agent active**\n\nUsage: `/agent <name>` or `/agent reset`\nList all available agents with `/agents`.")
			}
			return m, nil
		}
		if agentName == "reset" || agentName == "clear" || agentName == "off" || agentName == "none" {
			m.activeAgent = nil
			m.rebuildEngine()
			m.appendNote("NOTE:\n⚪ **Custom Agent Deactivated**\n\nReverted to standard mode: **" + m.mode + "**")
			return m, nil
		}

		cwd, _ := os.Getwd()
		if m.agentLoader == nil {
			m.agentLoader = agent.NewLoader(cwd)
		}
		targetAg := m.agentLoader.Find(agentName)
		if targetAg == nil {
			m.appendNote(fmt.Sprintf("NOTE:\n❌ **Custom Agent Not Found: `%s`**\n\nType `/agents` to view all available custom agents.", agentName))
			return m, nil
		}

		m.activeAgent = targetAg
		if targetAg.Mode != "" {
			m.mode = targetAg.Mode
		}
		m.rebuildEngine()
		m.appendNote(fmt.Sprintf("NOTE:\n🟢 **Switched to Custom Agent: %s**\n\n- **Description:** %s\n- **Mode:** %s\n- **Directives:** Loaded from `%s`",
			targetAg.Name, targetAg.Description, targetAg.Mode, targetAg.Path))
		return m, nil

	case "/lsp":
		m.appendNote("LSP:\n" + m.lspStatus())

	case "/diagnose":
		if m.lspMgr == nil {
			m.appendNote("LSP:\n⚠️ LSP not initialized.")
			return m, nil
		}
		fixMode := len(parts) > 1 && strings.TrimSpace(parts[1]) == "fix"
		cwd, _ := os.Getwd()
		m.status = "Scanning project diagnostics..."
		m.turnStart = time.Now()
		lsp := m.lspMgr
		return m, tea.Batch(tickCmd(), func() tea.Msg {
			out, err := lsp.ScanDiagnostics(context.Background(), cwd)
			if err != nil {
				return diagnoseResultMsg("LSP:\n❌ Diagnose failed: " + err.Error())
			}
			if fixMode {
				return diagnoseFixMsg(out)
			}
			out += "\n\n💡 Type `/diagnose fix` for BroCode to automatically fix all warnings/errors above."
			return diagnoseResultMsg("DIAGNOSE:\n" + out)
		})

	case "/lsp-install":
		if m.lspMgr == nil {
			m.appendNote("LSP:\n⚠️ Language Server Protocol (LSP) manager is not initialized.")
			return m, nil
		}
		lang := ""
		if len(parts) > 1 {
			lang = parts[1]
		}
		hints := m.lspMgr.InstallHints()
		if lang != "" {
			if _, ok := hints[lang]; !ok {
				m.appendNote("LSP:\n⚠️ No install needed for `" + lang + "` (already installed or unknown language).")
				return m, nil
			}
			hints = map[string]string{lang: hints[lang]}
		}
		if len(hints) == 0 {
			m.appendNote("LSP:\n✅ All language servers are already installed and active.")
			return m, nil
		}
		var sb strings.Builder
		sb.WriteString("⬇️ **Installing language servers...**\n\n")
		for l, c := range hints {
			sb.WriteString(fmt.Sprintf("- **%s**: `%s`\n", l, c))
		}
		m.appendNote("LSP:\n" + sb.String())
		m.status = "Installing language servers..."
		m.turnStart = time.Now()
		lsp := m.lspMgr
		return m, tea.Batch(tickCmd(), func() tea.Msg {
			return diagnoseResultMsg("LSP:\n" + runLSPInstalls(lsp, lang))
		})

	case "/mcp":
		m.showMCP = true
		m.mcpSel = 0
		m.mcpConfirm = ""
		m.mcpAddActive = false

	case "/mcp-reload":
		m.reloadMCP()

	case "/sessions", "/history":
		if st := m.context.Store(); st != nil {
			cwd, _ := os.Getwd()
			if list, err := st.ListSessionsByProjectPath(cwd); err == nil {
				m.sessionList = list
				m.sessionsSel = 0
				m.sessionsViewport.GotoTop()
				m.showSessions = true
			} else {
				m.appendNote("ERROR: ❌ Failed to list sessions: " + err.Error())
			}
		} else {
			m.appendNote("ERROR: ⚠️ Session store not initialized.")
		}

	case "/new":
		cwd, _ := os.Getwd()
		newSessID := fmt.Sprintf("sess_%d", time.Now().Unix())
		st := m.context.Store()
		if st != nil {
			_ = st.CreateSession(newSessID, cwd)
		}
		m.context = bcontext.NewManager(newSessID, st, m.contextWindow())
		m.rebuildEngine()
		m.messages = []string{fmt.Sprintf("MODE:BUILDER\n✅ **Started fresh session:** `%s`\n\nActive chat context has been reset to zero.", newSessID)}

	case "/models":
		m.modelOptions = provider.DiscoverModels(m.cfg)
		m.modelListCache = nil
		m.showModels = true
		m.modelsQuery = ""
		m.modelsSel = 0

	case "/connect":
		if len(parts) >= 3 {
			pID := strings.ToLower(parts[1])
			apiKey := parts[2]
			m.saveProviderKey(pID, apiKey)
		} else {
			m.showConnect = true
			m.connectStep = 0
			m.connectCustom = false
			m.connectTextInput.SetValue("")
			m.connectProviderSel = 0
		}

	case "/copy":
		lastAns := m.lastAssistantAnswer()
		if lastAns != "" {
			if err := clipboard.WriteAll(lastAns); err == nil {
				m.appendNote("NOTE:\n📋 **Copied to Clipboard!**\n\nThe last BroCode response has been copied to your OS clipboard. Paste anywhere with `Cmd+V` / `Ctrl+V`.")
			} else {
				m.appendNote("NOTE:\n❌ **Clipboard Error**\n\nFailed to copy to clipboard: " + err.Error())
			}
		} else {
			m.appendNote("NOTE:\n⚠️ **Nothing to Copy**\n\nNo BroCode response found in this session yet.")
		}

	case "/mouse":
		if m.mouseMode == "SCROLL" {
			m.mouseMode = "SELECT"
			m.appendNote("NOTE:\n🖱️ **Mouse Mode: SELECT**\n\nNative mouse drag highlight & copy is now active. You can click-drag text **without holding `Shift`**.\n\nType `/mouse` or press `Ctrl+M` to switch back to SCROLL mode.")
		} else {
			m.mouseMode = "SCROLL"
			m.appendNote("NOTE:\n🖱️ **Mouse Mode: SCROLL**\n\nMouse wheel viewport scrolling is now active.\n\nType `/mouse` or press `Ctrl+M` to switch to SELECT mode (drag to copy without Shift).")
		}

	case "/debug-context":
		m.showDebug = true

	case "/clear":
		m.messages = []string{"MODE:BUILDER\n⚡ **Chat history cleared.** Ready for next prompt."}

	case "/workspace", "/repos":
		cwd, _ := os.Getwd()
		ws := repo.DiscoverWorkspace(cwd)
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("### 📦 Workspace Root: `%s`\n\n", ws.RootPath))
		if len(ws.Repos) == 0 {
			sb.WriteString("No repositories detected in workspace.\n")
		} else {
			sb.WriteString(fmt.Sprintf("Found **%d repository/repositories**:\n\n", len(ws.Repos)))
			for _, r := range ws.Repos {
				gitBadge := "git"
				if !r.IsGit {
					gitBadge = "non-git"
				}
				sb.WriteString(fmt.Sprintf("- **%s** `[%s]` — `%s`\n", r.Name, gitBadge, r.Path))
			}
		}
		sb.WriteString("\n*Tips:* Subagents and tools can target specific repos using `target_dir: \"<repo_name>\"`.")
		m.appendNote("WORKSPACE:\n" + sb.String())

	case "/undo":
		count := tool.RestoreAllSnapshots()
		if count > 0 {
			m.appendNote(fmt.Sprintf("UNDO:\n↩️ Successfully restored %d file(s) back to pre-turn snapshot.", count))
		} else {
			m.appendNote("UNDO:\n⚠️ No live snapshots available to roll back (no files were modified in the active turn).")
		}

	case "/model":
		if len(parts) > 1 {
			target := parts[1]
			sub := strings.Split(target, "/")
			if len(sub) == 2 {
				pID := sub[0]
				m.activeModel = sub[1]
				m.switchProviderAndModel(pID, m.activeModel)
			} else {
				m.activeModel = target
				m.appendNote(fmt.Sprintf("MODE:%s\n✅ Model switched to `%s`", m.mode, m.activeModel))
				m.rebuildEngine()
			}
		} else {
			m.appendNote("MODE:" + m.mode + "\n**Usage:** `/model <provider>/<model>` or `/model <model_name>`\n\n**Examples:**\n- `/model openai/gpt-4o`\n- `/model gemini/gemini-2.5-pro`\n- `/model claude-3-5-sonnet`")
		}

	case "/report":
		if m.context == nil || m.context.Store() == nil {
			m.appendNote("REPORT:\n⚠️ Session store is not initialized.")
			return m, nil
		}
		r, err := report.Build(m.context.Store(), m.context.SessionID())
		if err != nil {
			m.appendNote(fmt.Sprintf("REPORT:\n⚠️ Failed to build session report: %v", err))
			return m, nil
		}
		if len(parts) > 1 && (parts[1] == "--json" || parts[1] == "-j" || parts[1] == "json" || parts[1] == "export") {
			jsonData, err := r.RenderJSON()
			if err != nil {
				m.appendNote(fmt.Sprintf("REPORT:\n⚠️ Failed to format JSON report: %v", err))
				return m, nil
			}
			outPath := "report.json"
			if len(parts) > 2 {
				outPath = parts[2]
			}
			if err := os.WriteFile(outPath, []byte(jsonData), 0o644); err != nil {
				m.appendNote(fmt.Sprintf("REPORT:\n⚠️ Failed to write %s: %v", outPath, err))
			} else {
				m.appendNote(fmt.Sprintf("REPORT:\n📊 Privacy-safe session report exported to `%s` (%d bytes).\n\nReady to share with community / devs for benchmarking & optimization!", outPath, len(jsonData)))
			}
			return m, nil
		}
		m.appendNote("REPORT:\n" + r.RenderMarkdown())
	}
	return m, nil
}

// runLSPInstalls executes the install command(s) for missing language servers
// (bounded 5 min each, so a slow package manager cannot hang the UI forever)
// and returns a report for the chat.
func runLSPInstalls(mgr *lsp.Manager, onlyLang string) string {
	hints := mgr.InstallHints()
	if onlyLang != "" {
		if c, ok := hints[onlyLang]; ok {
			hints = map[string]string{onlyLang: c}
		} else {
			return "⚠️ No install needed for " + onlyLang + "."
		}
	}
	var sb strings.Builder
	for lang, cmd := range hints {
		sb.WriteString(fmt.Sprintf("\n⬇️ %s: %s\n", lang, cmd))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		out, err := c.CombinedOutput()
		cancel()
		if err != nil {
			sb.WriteString("❌ " + lang + " install failed: " + err.Error() + "\n" + truncateString(string(out), 500))
		} else {
			sb.WriteString("✅ " + lang + " installed\n")
		}
	}
	sb.WriteString("\n🧠 Available now: ")
	if av := mgr.AvailableServers(); len(av) > 0 {
		sb.WriteString(strings.Join(av, ", "))
	} else {
		sb.WriteString("none")
	}
	return sb.String()
}

// truncateString shortens s to n runes with an ellipsis.
func truncateString(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// lspStatus renders the code intelligence status: which language servers are
// installed and can be used by the lsp_* tools.
func (m *Model) lspStatus() string {
	if m.lspMgr == nil {
		return "ℹ️ LSP not initialized."
	}
	langs := map[string]string{
		"go":         "gopls",
		"typescript": "typescript-language-server",
		"python":     "pyright-langserver",
		"rust":       "rust-analyzer",
		"c":          "clangd",
		"cpp":        "clangd",
	}
	var sb strings.Builder
	sb.WriteString("🧠 LSP code intelligence (lsp_definition, lsp_references, lsp_hover, lsp_diagnostics)\n")
	for lang, bin := range langs {
		_, err := exec.LookPath(bin)
		if err == nil {
			sb.WriteString(fmt.Sprintf("  ✅ %-12s %s\n", lang, bin))
		} else {
			sb.WriteString(fmt.Sprintf("  ❌ %-12s %s (not installed)\n", lang, bin))
		}
	}
	if hints := m.lspMgr.InstallHints(); len(hints) > 0 {
		sb.WriteString("\nRun /lsp-install to auto-install the missing servers, or install manually:")
		for lang, cmd := range hints {
			sb.WriteString(fmt.Sprintf("\n  %-10s %s", lang, cmd))
		}
	}
	sb.WriteString("\nThe model falls back to grep/glob/read_file when a server is missing.")
	if m.globalIndex != nil {
		sb.WriteString(fmt.Sprintf("\n🗺️ code_locate: %d files indexed (persistent per-session symbol + reference graph, no server needed)\n", m.globalIndex.FileCount()))
	}
	return strings.TrimSpace(sb.String())
}

// summarizeMCP returns a compact one-liner of connected MCP servers (names
// only) injected into OpenCode CLI prompts so the model answers MCP questions
// from context instead of exploring config files. Empty when nothing is
// connected.
func summarizeMCP(mgr *mcp.Manager) string {
	if mgr == nil {
		return ""
	}
	names := mgr.ServerNames()
	if len(names) == 0 {
		return ""
	}
	return "Connected MCP servers: " + strings.Join(names, ", ")
}

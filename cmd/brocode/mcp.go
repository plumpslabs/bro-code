package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/plumpslabs/bro-code/internal/mcp"
)

// runMCPCommand handles `brocode mcp <list|add|remove>` — managing MCP server
// config without launching the TUI (or any LLM). list reads the merged config
// from all standard locations; add/remove edit the mcpServers JSON file.
func runMCPCommand(args []string) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printMCPUsage()
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		runMCPList()
	case "add":
		runMCPAdd(args[1:])
	case "remove", "rm", "delete", "del":
		runMCPRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "❌ Unknown mcp subcommand %q\n\n", args[0])
		printMCPUsage()
		os.Exit(1)
	}
}

func printMCPUsage() {
	fmt.Print(`Manage Model Context Protocol (MCP) servers.

Usage:
  brocode mcp list                        Show configured MCP servers
  brocode mcp add <name> --type <t> ...   Add a server (stdio | http | sse)
  brocode mcp remove <name>               Remove a server
  brocode mcp help                        Show this help

Add examples:
  brocode mcp add github --type stdio --command "npx -y @modelcontextprotocol/server-github"
  brocode mcp add notion --type http --url "https://mcp.notion.com/mcp" --scope user

Flags:
  --type <stdio|http|sse>   Transport (default stdio)
  --command "<cmd> [args]"  stdio: command and args (space-separated)
  --url <url>               http/sse: endpoint URL
  --scope <project|user>    Where to save (default project → .mcp.json;
                            user → ~/.config/brocode/mcp.json)
`)
}

func runMCPList() {
	mgr := mcp.NewManager()
	mgr.LoadDefaults()
	names := mgr.ServerNames()
	if len(names) == 0 {
		fmt.Println("ℹ️  No MCP servers configured.")
		fmt.Println("   Add one with: brocode mcp add <name> --command \"npx -y <pkg>\"")
		fmt.Printf("   or edit %s / %s\n", mcp.ProjectMCPPath(), mcp.GlobalMCPPath())
		return
	}
	fmt.Printf("🔌 %d MCP server(s) configured:\n", len(names))
	for _, n := range names {
		cfg, ok := mgr.Config(n)
		if !ok {
			continue
		}
		switch cfg.Transport() {
		case "stdio":
			fmt.Printf("  - %s (stdio): %s %s\n", n, cfg.Command, strings.Join(cfg.Args, " "))
		case "http", "sse":
			fmt.Printf("  - %s (%s): %s\n", n, cfg.Type, cfg.URL)
		}
	}
	fmt.Printf("\nConfig sources: %s, %s, %s\n", mcp.ProjectMCPPath(), ".brocode/mcp.json", mcp.GlobalMCPPath())
}

func runMCPAdd(args []string) {
	fs := flag.NewFlagSet("mcp add", flag.ExitOnError)
	fs.Usage = func() { printMCPUsage() }
	typ := fs.String("type", "stdio", "transport: stdio | http | sse")
	command := fs.String("command", "", "stdio command + args (space-separated)")
	url := fs.String("url", "", "http/sse endpoint URL")
	scope := fs.String("scope", "project", "where to save: project | user")
	_ = fs.Parse(args)

	name := ""
	if fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		fmt.Fprintln(os.Stderr, "❌ Server name is required: brocode mcp add <name> ...")
		os.Exit(1)
	}

	var cfg mcp.ServerConfig
	switch strings.ToLower(*typ) {
	case "http", "streamable-http":
		if strings.TrimSpace(*url) == "" {
			fmt.Fprintln(os.Stderr, "❌ --url is required for http servers")
			os.Exit(1)
		}
		cfg = mcp.ServerConfig{Type: "http", URL: strings.TrimSpace(*url)}
	case "sse":
		if strings.TrimSpace(*url) == "" {
			fmt.Fprintln(os.Stderr, "❌ --url is required for sse servers")
			os.Exit(1)
		}
		cfg = mcp.ServerConfig{Type: "sse", URL: strings.TrimSpace(*url)}
	default: // stdio
		fields := strings.Fields(strings.TrimSpace(*command))
		if len(fields) == 0 {
			fmt.Fprintln(os.Stderr, "❌ --command is required for stdio servers (e.g. \"npx -y <pkg>\")")
			os.Exit(1)
		}
		cfg = mcp.ServerConfig{Command: fields[0], Args: fields[1:]}
	}

	path := mcp.ProjectMCPPath()
	if *scope == "user" || *scope == "global" {
		path = mcp.GlobalMCPPath()
	}
	if err := mcp.AddServerToFile(path, name, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to write %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Printf("✅ Added MCP server %q → %s\n", name, path)
	fmt.Println("   Restart BroCode or run /mcp-reload to connect.")
}

func runMCPRemove(args []string) {
	fs := flag.NewFlagSet("mcp remove", flag.ExitOnError)
	scope := fs.String("scope", "project", "where the server is saved: project | user")
	_ = fs.Parse(args)

	name := ""
	if fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		fmt.Fprintln(os.Stderr, "❌ Server name is required: brocode mcp remove <name>")
		os.Exit(1)
	}

	path := mcp.ProjectMCPPath()
	if *scope == "user" || *scope == "global" {
		path = mcp.GlobalMCPPath()
	}
	if err := mcp.RemoveServerFromFile(path, name); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to update %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Printf("✅ Removed MCP server %q from %s\n", name, path)
	fmt.Println("   Restart BroCode or run /mcp-reload to disconnect.")
}



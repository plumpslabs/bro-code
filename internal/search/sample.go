package search

// SampleCorpus is a small demo corpus (theme: coding agent tools/skills)
// so the skeleton can be tried without real data.
func SampleCorpus() []Document {
	return []Document{
		{ID: "skill-golang", Title: "Golang skill", Body: "write and review Go code, modules, testing, vet"},
		{ID: "tool-bash", Title: "Bash runner", Body: "run shell commands, capture output, bounded output buffer"},
		{ID: "tool-edit", Title: "File editor", Body: "apply unified diff hunks to files using Myers diff"},
		{ID: "skill-mcp", Title: "MCP client", Body: "connect to MCP servers, list tools, lazy load schemas"},
		{ID: "tool-search", Title: "Code search", Body: "fast repository search over source, BM25 relevance ranking"},
		{ID: "skill-testing", Title: "Testing", Body: "run go test, catch flaky tests, soak tests for memory leaks"},
		{ID: "tool-memory", Title: "Memory store", Body: "session memory with TTL, retention policy, JSONL storage"},
		{ID: "skill-performance", Title: "Performance", Body: "measure RSS and startup time, keep idle memory flatline"},
		{ID: "tool-mcp-server", Title: "MCP server", Body: "expose tools over stdio transport with streaming updates"},
	}
}

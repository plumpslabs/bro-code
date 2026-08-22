package search

import (
	"fmt"
	"strings"
	"sync"
)

// SymbolRAG provides instant (<1ms) local zero-dependency symbol resolution
// using an interned path table to minimize memory overhead across massive codebases.
type SymbolRAG struct {
	mu      sync.RWMutex
	symbols map[string]uint32 // lowercase symbol -> pathID
	paths   []string          // pathID -> filePath
	pathIDs map[string]uint32 // filePath -> pathID
}

// NewSymbolRAG creates a new local symbol RAG index.
func NewSymbolRAG() *SymbolRAG {
	return &SymbolRAG{
		symbols: make(map[string]uint32),
		paths:   make([]string, 0, 64),
		pathIDs: make(map[string]uint32),
	}
}

// IndexSymbol records a symbol mapping with interned path storage.
func (sr *SymbolRAG) IndexSymbol(symbol, filePath string) {
	if symbol == "" || filePath == "" {
		return
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()

	pid, exists := sr.pathIDs[filePath]
	if !exists {
		pid = uint32(len(sr.paths))
		sr.paths = append(sr.paths, filePath)
		sr.pathIDs[filePath] = pid
	}
	sr.symbols[strings.ToLower(symbol)] = pid
}

// RemoveFile drops every symbol pointing at the given file.
func (sr *SymbolRAG) RemoveFile(filePath string) {
	if filePath == "" {
		return
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()

	pid, exists := sr.pathIDs[filePath]
	if !exists {
		return
	}
	for sym, p := range sr.symbols {
		if p == pid {
			delete(sr.symbols, sym)
		}
	}
}

// Resolve looks up exact symbol matches.
func (sr *SymbolRAG) Resolve(symbol string) (string, bool) {
	symbol = strings.ToLower(strings.TrimSpace(symbol))
	if symbol == "" {
		return "", false
	}
	sr.mu.RLock()
	defer sr.mu.RUnlock()

	if pid, ok := sr.symbols[symbol]; ok && int(pid) < len(sr.paths) {
		path := sr.paths[pid]
		return fmt.Sprintf("📍 Symbol %q found in %s", symbol, path), true
	}
	return "", false
}

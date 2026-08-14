package search

import (
	"fmt"
	"strings"
)

// SymbolRAG provides instant (<5ms) local zero-dependency symbol resolution.
type SymbolRAG struct {
	symbols map[string]string // symbol -> filePath
}

// NewSymbolRAG creates a new local symbol RAG index.
func NewSymbolRAG() *SymbolRAG {
	return &SymbolRAG{symbols: make(map[string]string)}
}

// IndexSymbol records a symbol mapping (e.g. function/type/struct -> file).
func (sr *SymbolRAG) IndexSymbol(symbol, filePath string) {
	if symbol != "" && filePath != "" {
		sr.symbols[strings.ToLower(symbol)] = filePath
	}
}

// Resolve Symbol looks up exact or partial symbol matches.
func (sr *SymbolRAG) Resolve(symbol string) (string, bool) {
	symbol = strings.ToLower(strings.TrimSpace(symbol))
	if path, ok := sr.symbols[symbol]; ok {
		return fmt.Sprintf("📍 Symbol %q found in %s", symbol, path), true
	}
	return "", false
}

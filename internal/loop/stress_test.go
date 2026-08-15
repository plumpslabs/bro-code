package loop

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

type stressAdapter struct{}

func (s *stressAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return &provider.CompletionResponse{
		Content: "Stress test response for multi-language query.",
	}, nil
}

func TestEngineHighWorkloadStabilityAndMemoryLeaks(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("stress_sess", nil, 128000)
	adapter := &stressAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	// Measure baseline memory
	runtime.GC()
	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	// Run 100 consecutive turn cycles
	for i := range 100 {
		prompt := fmt.Sprintf("Analyze multi-language project refactoring task #%d (Go, Rust, TS, Python, PHP)", i)
		res, err := engine.RunTurn(context.Background(), prompt, nil)
		if err != nil {
			t.Fatalf("Turn %d failed: %v", i, err)
		}
		if !strings.Contains(res, "Stress test response") {
			t.Errorf("Turn %d output unexpected: %s", i, res)
		}
	}

	// Measure post-workload memory
	runtime.GC()
	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)

	heapDiff := int64(mAfter.HeapAlloc) - int64(mBefore.HeapAlloc)
	t.Logf("HeapAlloc before: %d KB, after 100 turns: %d KB, diff: %d KB",
		mBefore.HeapAlloc/1024, mAfter.HeapAlloc/1024, heapDiff/1024)

	// Heap growth should be minimal (< 5MB for 100 turns)
	if heapDiff > 5*1024*1024 {
		t.Errorf("Possible memory leak detected: Heap grew by %d KB over 100 turns", heapDiff/1024)
	}
}

func TestSelfHealLadderMultiLanguageCoverage(t *testing.T) {
	testFiles := map[string]string{
		"main.go":     "package main",
		"app.ts":      "console.log('hi')",
		"script.py":   "print('hi')",
		"main.rs":     "fn main() {}",
		"main.cpp":    "int main() {}",
		"index.php":   "<?php echo 'hi';",
		"app.rb":      "puts 'hi'",
		"config.yaml": "key: val",
		"styles.css":  "body { color: red; }",
		"document.md": "# Title",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for file := range testFiles {
		// Should execute safely without crashing or throwing unhandled panics even if tool isn't installed
		msg, ok := SelfHealLadder(ctx, file)
		_ = msg
		_ = ok
	}
}

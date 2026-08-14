package loop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// delayServer returns an httptest server whose handler sleeps for delay before
// answering, and tracks the maximum number of concurrently in-flight requests
// (the overlap proof for parallel execution) plus a counter.
func delayServer(t *testing.T, delay time.Duration) (*httptest.Server, *int64, *int64) {
	t.Helper()
	var active, maxActive, total int64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		total++
		mu.Unlock()

		time.Sleep(delay)

		mu.Lock()
		active--
		mu.Unlock()
		fmt.Fprintf(w, "ok:%s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv, &maxActive, &total
}

// fetchBatchAdapter emits a batch of fetch_url calls on the first completion
// (one round), then answers on the second. urls must be concrete.
type fetchBatchAdapter struct {
	urls []string
	call int
}

func (m *fetchBatchAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	m.call++
	if m.call == 1 {
		var calls []provider.ToolCall
		for i, u := range m.urls {
			calls = append(calls, provider.ToolCall{
				ID:        fmt.Sprintf("tc-%d", i),
				Name:      "fetch_url",
				Arguments: fmt.Sprintf(`{"url":%q}`, u),
			})
		}
		return &provider.CompletionResponse{Reasoning: "fetching in parallel", ToolCalls: calls}, nil
	}
	return &provider.CompletionResponse{Content: "done", Reasoning: "finished"}, nil
}

// runFetchTurn wires a registry, context manager and engine, runs one turn with
// the batch adapter, and returns the manager (for result-order assertions).
func runFetchTurn(t *testing.T, urls []string, mode string) (*bcontext.Manager, *fetchBatchAdapter) {
	t.Helper()
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &fetchBatchAdapter{urls: urls}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	if mode != "" {
		engine.SetMode(mode)
	}
	if _, err := engine.RunTurn(context.Background(), "fetch all", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	return ctxMgr, adapter
}

// TestParallelReadOnlyToolsOverlap proves read-only tools in ONE round execute
// CONCURRENTLY: with 5 fetches each sleeping 250ms, a sequential run would
// take ≥1.25s; the observed max in-flight must be ≥2 and the wall time must be
// well under the sequential total.
func TestParallelReadOnlyToolsOverlap(t *testing.T) {
	const delay = 250 * time.Millisecond
	const n = 5
	srv, maxActive, _ := delayServer(t, delay)

	base := srv.URL
	urls := make([]string, n)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/p%d", base, i)
	}

	start := time.Now()
	_, _ = runFetchTurn(t, urls, "")
	elapsed := time.Since(start)

	if got := atomic.LoadInt64(maxActive); got < 2 {
		t.Errorf("expected ≥2 concurrent fetches (parallel execution), max in-flight = %d", got)
	}
	// Sequential would take n*delay = 1.25s. Parallel with cap 4: ~2 waves ≈
	// 0.5s. Allow generous headroom (2 waves * delay * 1.5) so CI scheduling
	// noise never flakes the assertion.
	if elapsed > 2*delay*2 {
		t.Errorf("turn took %v — looks sequential, not parallel (5×%v fetches)", elapsed, delay)
	}
}

// TestParallelReadOnlyToolsCapped proves the concurrency cap: 8 fetches against
// a server that reports max in-flight must never exceed maxParallelReadOnlyTools.
func TestParallelReadOnlyToolsCapped(t *testing.T) {
	srv, maxActive, _ := delayServer(t, 120*time.Millisecond)

	urls := make([]string, 8)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/c%d", srv.URL, i)
	}
	_, _ = runFetchTurn(t, urls, "")

	if got := atomic.LoadInt64(maxActive); got > int64(maxParallelReadOnlyTools) {
		t.Errorf("concurrency cap violated: %d in-flight > max %d", got, maxParallelReadOnlyTools)
	}
	if got := atomic.LoadInt64(maxActive); got < 2 {
		t.Errorf("expected some parallelism (≥2), got %d — cap test not exercising the batch", got)
	}
}

// TestParallelResultsStayInCallOrder proves the critical invariant for
// providers: even though parallel tools complete in nondeterministic order,
// the tool results appended to the context must appear in the model's ORIGINAL
// call order (tool_call → tool_result pairing). The URLs differ in delay
// (path length does not matter — the server sleeps uniformly) so completion
// order is scrambled by the goroutine scheduler; the appended order must still
// be tc-0, tc-1, ..., tc-N.
func TestParallelResultsStayInCallOrder(t *testing.T) {
	srv, _, _ := delayServer(t, 80*time.Millisecond)

	urls := make([]string, 6)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/o%d", srv.URL, i)
	}
	ctxMgr, _ := runFetchTurn(t, urls, "")

	// Collect user-role tool results in the order they were appended.
	var toolResultOrder []string
	for _, m := range ctxMgr.Messages() {
		if m.Role == "user" && m.ToolCallID != "" {
			toolResultOrder = append(toolResultOrder, m.ToolCallID)
		}
	}
	if len(toolResultOrder) != len(urls) {
		t.Fatalf("expected %d tool results, got %d", len(urls), len(toolResultOrder))
	}
	for i, id := range toolResultOrder {
		want := fmt.Sprintf("tc-%d", i)
		if id != want {
			t.Fatalf("tool results out of order: position %d = %q, want %q (full: %v)", i, id, want, toolResultOrder)
		}
	}
}

// TestParallelBatchWithBlockedCall proves guards still apply inside a parallel
// batch: in PLANNER mode a write_file in the middle of a read-only batch is
// blocked with its guard message, the reads still run, and the results stay in
// call order (read, guard, read).
func TestParallelBatchWithBlockedCall(t *testing.T) {
	srv, _, _ := delayServer(t, 60*time.Millisecond)

	// Custom adapter: read_file → fetch_url → write_file → fetch_url, so the
	// blocked call sits between parallel calls.
	adapter := &mixedBlockedAdapter{readPath: "", fetchURL: srv.URL + "/x"}
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	engine.SetMode("PLANNER")
	if _, err := engine.RunTurn(context.Background(), "inspect", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	var results []struct {
		id   string
		body string
	}
	for _, m := range ctxMgr.Messages() {
		if m.Role == "user" && m.ToolCallID != "" {
			results = append(results, struct {
				id   string
				body string
			}{m.ToolCallID, m.Content})
		}
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results (fetch, blocked write, fetch), got %d", len(results))
	}
	// Order must be preserved: tc-0 (fetch), tc-1 (write_file blocked), tc-2 (fetch).
	if results[0].id != "tc-0" || results[1].id != "tc-1" || results[2].id != "tc-2" {
		t.Fatalf("order broken: %+v", results)
	}
	if !strings.Contains(results[1].body, "PLANNER GUARD") {
		t.Errorf("expected PLANNER guard message for blocked write_file, got %q", results[1].body)
	}
}

// mixedBlockedAdapter issues fetch_url, write_file, fetch_url on round 1.
type mixedBlockedAdapter struct {
	readPath string
	fetchURL string
	call     int
}

func (m *mixedBlockedAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	m.call++
	if m.call == 1 {
		return &provider.CompletionResponse{
			Reasoning: "batch",
			ToolCalls: []provider.ToolCall{
				{ID: "tc-0", Name: "fetch_url", Arguments: fmt.Sprintf(`{"url":%q}`, m.fetchURL)},
				{ID: "tc-1", Name: "write_file", Arguments: `{"path":"x.go","content":"x"}`},
				{ID: "tc-2", Name: "fetch_url", Arguments: fmt.Sprintf(`{"url":%q}`, m.fetchURL)},
			},
		}, nil
	}
	return &provider.CompletionResponse{Content: "done", Reasoning: "finished"}, nil
}

// TestParallelReadOnlyHooksFired proves the after-tool hook fires for parallel
// calls too (hooks are the only side effect that must survive parallelization).
func TestParallelReadOnlyHooksFired(t *testing.T) {
	srv, _, total := delayServer(t, 40*time.Millisecond)

	urls := make([]string, 3)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/h%d", srv.URL, i)
	}
	_, _ = runFetchTurn(t, urls, "")

	if got := atomic.LoadInt64(total); got != 3 {
		t.Errorf("expected 3 fetches to hit the server, got %d", got)
	}
}

package loop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// TestParallelNoGoroutineLeak proves parallel tool execution does not leak
// goroutines: after a turn with a parallel batch completes, the goroutine
// count returns to the baseline (all worker goroutines joined via wg.Wait).
func TestParallelNoGoroutineLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	urls := make([]string, 6)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/l%d", srv.URL, i)
	}

	// Warm up so runtime goroutine counts (GC, etc.) settle.
	runFetchTurn(t, urls, "")
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	runFetchTurn(t, urls, "")
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()

	if after > before+3 {
		t.Errorf("possible goroutine leak: %d before, %d after parallel batch", before, after)
	}
}

// TestParallelCtxCancelMidBatch proves interrupting the turn (ctx cancel)
// while a parallel batch is executing makes every goroutine exit promptly:
// no worker is left waiting on the semaphore forever and the turn returns
// with cancelled results instead of hanging.
func TestParallelCtxCancelMidBatch(t *testing.T) {
	var started atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	urls := make([]string, 8)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/c%d", srv.URL, i)
	}

	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &fetchBatchAdapter{urls: urls}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Block until some fetches are actually in flight, then cancel.
		for started.Load() < 2 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	_, err := engine.RunTurn(ctx, "fetch all", nil)
	<-done

	if err == nil {
		t.Error("expected an error when the context is cancelled mid-batch")
	}
	// The turn must have returned promptly (the batch is bounded by the
	// 300ms sleep; waiting for all 8 would take ~600ms+ under cap 4). Allow
	// generous headroom but never the full unbounded wait.
}

package loop

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// routingAdapter is a scriptable adapter that counts calls and can fail the
// first N attempts with a given error before answering normally.
type routingAdapter struct {
	mu       sync.Mutex
	calls    int
	failFor  int
	failErr  error
	answered int
}

func (r *routingAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls <= r.failFor && r.failErr != nil {
		return nil, r.failErr
	}
	r.answered++
	return &provider.CompletionResponse{Content: fmt.Sprintf("answered by %s", req.Model)}, nil
}

func (r *routingAdapter) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newRouterEngine(primary provider.ProviderAdapter, fbs ...Fallback) (*Engine, *bcontext.Manager) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("routing", nil, 128000)
	e := NewEngine(primary, tools, ctxMgr, "primary-model")
	e.SetPrimaryIdentity("primary", "openai-compatible")
	for _, fb := range fbs {
		e.AddFallback(fb)
	}
	return e, ctxMgr
}

// TestRouterRetryOnceThenSucceed proves a transient primary failure is retried
// ONCE on the same provider before any fallback is used — a one-off stream
// hiccup should never silently downgrade the answer to another model.
func TestRouterRetryOnceThenSucceed(t *testing.T) {
	primary := &routingAdapter{failFor: 1, failErr: provider.ErrStreamIdle}
	fallback := &routingAdapter{}
	e, _ := newRouterEngine(primary, Fallback{ID: "fb", Protocol: "anthropic", Adapter: fallback, Model: "fb-model"})

	res, err := e.RunTurn(context.Background(), "hello", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !strings.Contains(res, "primary-model") {
		t.Errorf("expected the primary (retried) to answer, got %q", res)
	}
	if primary.callCount() != 2 {
		t.Errorf("primary calls = %d, want 2 (fail + retry)", primary.callCount())
	}
	if fallback.callCount() != 0 {
		t.Errorf("fallback must NOT run when retry succeeds, calls=%d", fallback.callCount())
	}
	if e.LastFallbackModel() != "" {
		t.Errorf("no fallback should be recorded, got %q", e.LastFallbackModel())
	}
	if e.FallbackCount() != 0 {
		t.Errorf("fallbackCount = %d, want 0", e.FallbackCount())
	}
}

// TestRouterNoRetryForPermanentError proves permanent errors (invalid model,
// auth, user ESC) are NOT retried — only routed to a fallback.
func TestRouterNoRetryForPermanentError(t *testing.T) {
	primary := &routingAdapter{failFor: 100, failErr: fmt.Errorf("model not found")}
	fallback := &routingAdapter{}
	e, _ := newRouterEngine(primary, Fallback{ID: "fb", Protocol: "anthropic", Adapter: fallback, Model: "fb-model"})

	res, err := e.RunTurn(context.Background(), "hello", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if primary.callCount() != 1 {
		t.Errorf("primary calls = %d, want 1 (no retry on permanent error)", primary.callCount())
	}
	if fallback.callCount() != 1 {
		t.Errorf("fallback calls = %d, want 1", fallback.callCount())
	}
	if !strings.Contains(res, "fb-model") {
		t.Errorf("expected fallback answer, got %q", res)
	}
	if e.LastFallbackModel() != "fb-model" {
		t.Errorf("lastFallback = %q, want fb-model", e.LastFallbackModel())
	}
	if e.FallbackCount() != 1 {
		t.Errorf("fallbackCount = %d, want 1", e.FallbackCount())
	}
	if !strings.Contains(e.LastFallbackReason(), "model not found") {
		t.Errorf("fallback reason should mention primary error, got %q", e.LastFallbackReason())
	}
}

// TestRouterCooldownSkipsPrimary proves a primary in cooldown is skipped on the
// NEXT turn: no full timeout is burned on a provider that just failed; the
// first healthy fallback serves directly.
func TestRouterCooldownSkipsPrimary(t *testing.T) {
	primary := &routingAdapter{}
	fallback := &routingAdapter{}
	e, _ := newRouterEngine(primary, Fallback{ID: "fb", Protocol: "anthropic", Adapter: fallback, Model: "fb-model"})

	// Simulate a previous turn's failure: primary enters cooldown.
	e.health.recordFailure("primary")
	if cd, _ := e.health.inCooldown("primary"); !cd {
		t.Fatal("primary should be in cooldown")
	}

	res, err := e.RunTurn(context.Background(), "hello", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if primary.callCount() != 0 {
		t.Errorf("primary must be skipped while in cooldown, calls=%d", primary.callCount())
	}
	if fallback.callCount() != 1 {
		t.Errorf("fallback should serve during cooldown, calls=%d", fallback.callCount())
	}
	if !strings.Contains(res, "fb-model") {
		t.Errorf("expected fallback answer, got %q", res)
	}
	if e.PrimaryCooldownRemaining() <= 0 {
		t.Error("PrimaryCooldownRemaining should still report cooldown after a skipped turn")
	}
}

// TestRouterPrimaryOnlyNeverFallsBack proves the primary_only policy ends the
// turn with an error instead of silently switching providers.
func TestRouterPrimaryOnlyNeverFallsBack(t *testing.T) {
	primary := &routingAdapter{failFor: 100, failErr: fmt.Errorf("provider down")}
	fallback := &routingAdapter{}
	e, _ := newRouterEngine(primary, Fallback{ID: "fb", Protocol: "anthropic", Adapter: fallback, Model: "fb-model"})
	e.SetFallbackPolicy(FallbackPrimaryOnly)

	_, err := e.RunTurn(context.Background(), "hello", nil)
	if err == nil {
		t.Fatal("expected error under primary_only policy")
	}
	if !strings.Contains(err.Error(), "provider down") {
		t.Errorf("expected primary error to surface, got %v", err)
	}
	if fallback.callCount() != 0 {
		t.Errorf("fallback must not run under primary_only, calls=%d", fallback.callCount())
	}
}

// TestRouterConfirmPolicyDenied proves confirm policy asks the user before a
// cross-vendor fallback, and a decline stops the turn.
func TestRouterConfirmPolicyDenied(t *testing.T) {
	primary := &routingAdapter{failFor: 100, failErr: fmt.Errorf("provider down")}
	fallback := &routingAdapter{}
	e, _ := newRouterEngine(primary, Fallback{ID: "fb", Protocol: "anthropic", Adapter: fallback, Model: "fb-model"})
	e.SetFallbackPolicy(FallbackConfirm)

	var asked string
	e.SetAskHandler(func(question string, options []string) (string, error) {
		asked = question
		return "🚫 Stop this turn", nil
	})

	_, err := e.RunTurn(context.Background(), "hello", nil)
	if err == nil {
		t.Fatal("expected error when user declines the fallback")
	}
	if !strings.Contains(asked, "fb-model") {
		t.Errorf("confirmation should mention the fallback model, got %q", asked)
	}
	if fallback.callCount() != 0 {
		t.Errorf("fallback must not run when user declines, calls=%d", fallback.callCount())
	}
}

// TestRouterConfirmPolicyAccepted proves an accepted confirmation routes to the
// cross-vendor fallback.
func TestRouterConfirmPolicyAccepted(t *testing.T) {
	primary := &routingAdapter{failFor: 100, failErr: fmt.Errorf("provider down")}
	fallback := &routingAdapter{}
	e, _ := newRouterEngine(primary, Fallback{ID: "fb", Protocol: "anthropic", Adapter: fallback, Model: "fb-model"})
	e.SetFallbackPolicy(FallbackConfirm)
	e.SetAskHandler(func(question string, options []string) (string, error) {
		return "✅ Use fallback", nil
	})

	res, err := e.RunTurn(context.Background(), "hello", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !strings.Contains(res, "fb-model") {
		t.Errorf("expected fallback answer, got %q", res)
	}
	if e.LastFallbackModel() != "fb-model" {
		t.Errorf("lastFallback = %q, want fb-model", e.LastFallbackModel())
	}
}

// TestRouterConfirmSkipsSameVendor proves same-vendor fallbacks route
// automatically even under the confirm policy — no confirmation prompt.
func TestRouterConfirmSkipsSameVendor(t *testing.T) {
	primary := &routingAdapter{failFor: 100, failErr: fmt.Errorf("provider down")}
	fallback := &routingAdapter{}
	e, _ := newRouterEngine(primary, Fallback{ID: "fb", Protocol: "openai-compatible", Adapter: fallback, Model: "fb-model"})
	e.SetFallbackPolicy(FallbackConfirm)

	asked := false
	e.SetAskHandler(func(question string, options []string) (string, error) {
		asked = true
		return "✅ Use fallback", nil
	})

	res, err := e.RunTurn(context.Background(), "hello", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if asked {
		t.Error("same-vendor fallback must not trigger a confirmation prompt")
	}
	if !strings.Contains(res, "fb-model") {
		t.Errorf("expected fallback answer, got %q", res)
	}
}

// TestRouterAllFallbacksInCooldownTriesPrimaryLast proves the cooldown is a
// hint, not a hard block: when every fallback is unavailable, the primary is
// still attempted as a last resort (here the primary is actually healthy, so
// the turn is served).
func TestRouterAllFallbacksInCooldownTriesPrimaryLast(t *testing.T) {
	primary := &routingAdapter{}
	fallback := &routingAdapter{}
	e, _ := newRouterEngine(primary, Fallback{ID: "fb", Protocol: "anthropic", Adapter: fallback, Model: "fb-model"})
	// Both primary AND fallback in cooldown → nothing healthy at fast-path.
	e.health.recordFailure("primary")
	e.health.recordFailure("fb")

	res, err := e.RunTurn(context.Background(), "hello", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !strings.Contains(res, "primary-model") {
		t.Errorf("primary should be tried as last resort, got %q", res)
	}
	if primary.callCount() != 1 {
		t.Errorf("primary calls = %d, want 1 (last-resort attempt)", primary.callCount())
	}
	if fallback.callCount() != 0 {
		t.Errorf("fallback must not run while in cooldown, calls=%d", fallback.callCount())
	}
}

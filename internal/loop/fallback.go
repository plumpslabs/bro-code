package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/plumpslabs/bro-code/internal/provider"
)

// Fallback is an alternative adapter+model pair tried when the primary
// provider fails (automatic model routing).
type Fallback struct {
	// ID is a stable provider identity (e.g. "groq", "opencode") used for
	// health tracking in the adaptive router.
	ID string
	// Protocol is the wire protocol ("anthropic" / "openai-compatible"), used
	// by the "confirm" fallback policy to ask only when the fallback is a
	// different vendor than the primary.
	Protocol string
	Adapter  provider.ProviderAdapter
	Model    string
}

// FallbackPolicy controls automatic model routing when the primary provider
// fails mid-turn.
const (
	// FallbackAuto (default): retry the primary once on transient errors, then
	// route to the next healthy fallback, skipping providers in cooldown.
	FallbackAuto = "auto"
	// FallbackConfirm asks the user before serving a fallback from a DIFFERENT
	// vendor than the primary; same-vendor fallbacks route automatically.
	FallbackConfirm = "confirm"
	// FallbackPrimaryOnly never falls back — a primary failure ends the turn
	// with an error.
	FallbackPrimaryOnly = "primary_only"
)

// AddFallback registers a fallback provider+model tried on primary failure.
func (e *Engine) AddFallback(fb Fallback) {
	e.fallbacks = append(e.fallbacks, fb)
}

// SetPrimaryIdentity gives the primary provider a stable ID and wire protocol
// so the adaptive router can track its health across turns and enforce
// cross-vendor confirmation policies.
func (e *Engine) SetPrimaryIdentity(id, protocol string) {
	e.primaryID = id
	e.primaryProtocol = protocol
}

// SetFallbackPolicy sets the routing policy (FallbackAuto / FallbackConfirm /
// FallbackPrimaryOnly). The default is FallbackAuto.
func (e *Engine) SetFallbackPolicy(policy string) {
	if policy == "" {
		policy = FallbackAuto
	}
	e.fallbackPolicy = policy
}

// FallbackCount returns how many turns a fallback provider has served.
func (e *Engine) FallbackCount() int {
	return e.fallbackCount
}

// PrimaryCooldownRemaining returns how much longer the primary provider will
// be skipped before the router tries it again (0 if healthy or uncooldown).
func (e *Engine) PrimaryCooldownRemaining() time.Duration {
	if inCD, d := e.health.inCooldown(e.primaryID); inCD {
		return d
	}
	return 0
}

// LastFallbackModel returns the fallback model used in the most recent turn
// (empty when the primary model served the turn).
func (e *Engine) LastFallbackModel() string {
	return e.lastFallback
}

// LastFallbackReason returns the primary provider's error that triggered the
// fallback routing on the most recent turn (empty when the primary served it
// cleanly). Used by the CLI banner to report why a fallback model answered.
func (e *Engine) LastFallbackReason() string {
	return e.lastFallbackReason
}

// completeTurn runs a completion through the adaptive router. It returns the
// response, plus the fallback model that served it ("" when the primary
// answered). Routing policy (see FallbackPolicy): retry the primary once on
// transient errors, skip providers in cooldown, and honor the user's
// fallback policy. Every non-2xx/network outcome feeds the circuit breaker so
// a chronically failing provider is skipped on later turns instead of burning
// a full timeout each time.
func (e *Engine) completeTurn(ctx context.Context, req provider.CompletionRequest, onUpdate TurnOutputHandler) (*provider.CompletionResponse, string, error) {
	timeout := defaultModelCallTimeout
	// Log that we are now blocking on the LLM so the activity log is never
	// silent during a slow generation (otherwise it looks "stuck").
	if onUpdate != nil {
		onUpdate(e.state, fmt.Sprintf("⏳ %s: generating response…", req.Model))
	}

	// Fast path: the primary is cooling down from a recent failure — don't
	// burn a full timeout on it again; go straight to the first healthy
	// fallback. The cooldown is a hint, not a hard block: if nothing is
	// available we still try the primary as a last resort.
	if cd, _ := e.health.inCooldown(e.primaryID); cd {
		if resp, fb, fbErr := e.tryFallbacks(ctx, req, onUpdate); resp != nil {
			return resp, fb, nil
		} else if fbErr != nil {
			return nil, "", fbErr
		}
		// fall through → try the primary anyway
	}

	// Each attempt gets its OWN timeout so a single slow call can't hang the
	// whole turn — on timeout we fall back to the next healthy model instead.
	callCtx, callCancel := context.WithTimeout(ctx, timeout)
	resp, err := e.complete(callCtx, req)
	callCancel()
	if err == nil {
		e.health.recordSuccess(e.primaryID)
		return resp, "", nil
	}

	primaryErr := err
	e.health.recordFailure(e.primaryID)
	if e.fallbackPolicy == FallbackPrimaryOnly {
		return nil, "", primaryErr
	}
	// Surface a timeout clearly so the user knows we're re-routing, not frozen.
	if errors.Is(err, context.DeadlineExceeded) && onUpdate != nil {
		onUpdate(e.state, fmt.Sprintf("⚠️ %s timed out after %s — routing to fallback…", req.Model, timeout))
	}

	// A transient primary failure (stream stall, timeout, 429/5xx) deserves
	// ONE retry on the same provider before switching models. Permanent errors
	// (auth, invalid model, user ESC) are never retried.
	if provider.IsRetryable(err) {
		retryCtx, retryCancel := context.WithTimeout(ctx, timeout)
		rresp, rerr := e.complete(retryCtx, req)
		retryCancel()
		if rerr == nil {
			e.health.recordSuccess(e.primaryID)
			return rresp, "", nil
		}
	}

	// Primary still failing — route to the next healthy fallback.
	e.lastFallbackReason = primaryErr.Error()
	if resp, fb, fbErr := e.tryFallbacks(ctx, req, onUpdate); resp != nil {
		return resp, fb, nil
	} else if fbErr != nil {
		return nil, "", fbErr
	}
	return nil, "", primaryErr
}

// tryFallbacks routes the completion to the first healthy fallback in
// registration order, skipping providers currently in cooldown. Returns the
// response plus the fallback model on success. fbErr is non-nil only when a
// fallback was SELECTED but failed; (nil, "", nil) means nothing was tried
// (no fallbacks, all in cooldown, or the confirm policy declined).
func (e *Engine) tryFallbacks(ctx context.Context, req provider.CompletionRequest, onUpdate TurnOutputHandler) (resp *provider.CompletionResponse, fallbackModel string, fbErr error) {
	var lastErr error
	for _, fb := range e.fallbacks {
		if cd, _ := e.health.inCooldown(fb.ID); cd {
			continue
		}
		// Confirm policy: only ask when the fallback is a DIFFERENT vendor
		// than the primary; same-vendor fallbacks (e.g. a sibling model on the
		// same gateway) route automatically.
		if e.fallbackPolicy == FallbackConfirm && fb.Protocol != "" && fb.Protocol != e.primaryProtocol {
			ok, err := e.askFallbackConfirmation(fb.Model, fb.ID)
			if err != nil {
				return nil, "", err
			}
			if !ok {
				return nil, "", nil // user declined → stop routing
			}
		}
		fbReq := req
		fbReq.Model = fb.Model
		// Bound each fallback attempt so a slow fallback can't hang either.
		fbCtx, fbCancel := context.WithTimeout(ctx, defaultModelCallTimeout)
		fbResp, err := e.completeWith(fbCtx, fb.Adapter, fbReq)
		fbCancel()
		if err == nil {
			e.health.recordSuccess(fb.ID)
			e.lastFallback = fb.Model
			e.fallbackCount++
			if onUpdate != nil {
				onUpdate(e.state, fmt.Sprintf("⚠️ Primary provider failed — using fallback model %s", fb.Model))
			}
			return fbResp, fb.Model, nil
		}
		e.health.recordFailure(fb.ID)
		lastErr = err
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", nil
}

// askFallbackConfirmation asks the user before routing to a fallback from a
// different vendor than the primary. With no interactive layer wired it
// defaults to allow, preserving the auto behavior.
func (e *Engine) askFallbackConfirmation(model, _ string) (bool, error) {
	if e.askHandler == nil {
		return true, nil
	}
	ans, err := e.askHandler(fmt.Sprintf(
		"⚠️ Primary provider (%s) failed. Route this turn to fallback model %s?",
		e.primaryID, model,
	), []string{"✅ Use fallback", "🚫 Stop this turn"})
	if err != nil {
		return false, err
	}
	return !strings.Contains(ans, "Stop") && !strings.Contains(ans, "Deny"), nil
}

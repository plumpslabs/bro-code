package loop

import (
	"sync"
	"time"
)

// Provider health tracker for adaptive model routing. When a provider fails it
// accumulates an exponential cooldown; every subsequent turn while it is in
// cooldown skips straight to a healthy fallback instead of burning a full
// timeout on a known-dead provider. A success resets the streak so a recovered
// provider is tried again immediately (the cooldown only affects future
// attempts, never the current one).
//
// The clock is injectable (now func) so tests can drive backoff expiry without
// sleeping.
const (
	cooldownBase = 30 * time.Second
	cooldownMax  = 5 * time.Minute
)

type healthEntry struct {
	failStreak    int
	cooldownUntil time.Time
}

type providerHealth struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*healthEntry
}

func newProviderHealth() *providerHealth {
	return &providerHealth{
		now:     time.Now,
		entries: map[string]*healthEntry{},
	}
}

func (h *providerHealth) entry(id string) *healthEntry {
	e, ok := h.entries[id]
	if !ok {
		e = &healthEntry{}
		h.entries[id] = e
	}
	return e
}

// recordFailure increments the streak and extends the cooldown with exponential
// backoff, capped at cooldownMax. Unidentified providers ("") are untracked.
func (h *providerHealth) recordFailure(id string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entry(id)
	e.failStreak++
	backoff := min(cooldownBase * time.Duration(1<<minInt(e.failStreak-1, 6)), cooldownMax)
	e.cooldownUntil = h.now().Add(backoff)
}

// recordSuccess resets a provider to healthy (streak 0, no cooldown).
func (h *providerHealth) recordSuccess(id string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entry(id)
	e.failStreak = 0
	e.cooldownUntil = time.Time{}
}

// inCooldown reports whether the provider should be skipped this attempt and,
// if so, how long remains. A provider whose cooldown has expired is healthy.
// Unidentified providers ("") are never in cooldown.
func (h *providerHealth) inCooldown(id string) (bool, time.Duration) {
	if id == "" {
		return false, 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entry(id)
	if e.cooldownUntil.IsZero() {
		return false, 0
	}
	remaining := e.cooldownUntil.Sub(h.now())
	if remaining <= 0 {
		e.cooldownUntil = time.Time{}
		return false, 0
	}
	return true, remaining
}

// failStreak exposes the consecutive-failure count (for status/metrics).
func (h *providerHealth) failStreak(id string) int {
	if id == "" {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.entry(id).failStreak
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

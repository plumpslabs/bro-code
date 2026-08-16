package loop

import (
	"testing"
	"time"
)

func TestHealthBackoffGrowsAndCaps(t *testing.T) {
	h := newProviderHealth()
	now := time.Now()
	h.now = func() time.Time { return now }

	// Failure 1 → base cooldown.
	h.recordFailure("p")
	cd, rem := h.inCooldown("p")
	if !cd {
		t.Fatal("expected cooldown after first failure")
	}
	if rem != cooldownBase {
		t.Errorf("cooldown after 1st failure = %v, want %v", rem, cooldownBase)
	}

	// Failure 2 → doubled.
	now = now.Add(cooldownBase)
	h.recordFailure("p")
	_, rem = h.inCooldown("p")
	if rem != cooldownBase*2 {
		t.Errorf("cooldown after 2nd failure = %v, want %v", rem, cooldownBase*2)
	}

	// Drive the streak up past the cap → cooldown must never exceed cooldownMax.
	for range 10 {
		now = now.Add(cooldownMax)
		h.recordFailure("p")
	}
	_, rem = h.inCooldown("p")
	if rem != cooldownMax {
		t.Errorf("cooldown after many failures = %v, want capped at %v", rem, cooldownMax)
	}
}

func TestHealthSuccessResets(t *testing.T) {
	h := newProviderHealth()
	now := time.Now()
	h.now = func() time.Time { return now }

	h.recordFailure("p")
	h.recordFailure("p")
	if _, rem := h.inCooldown("p"); rem == 0 {
		t.Fatal("expected cooldown before success")
	}

	h.recordSuccess("p")
	if cd, _ := h.inCooldown("p"); cd {
		t.Error("success must clear cooldown")
	}
	if s := h.failStreak("p"); s != 0 {
		t.Errorf("failStreak after success = %d, want 0", s)
	}
}

func TestHealthCooldownExpires(t *testing.T) {
	h := newProviderHealth()
	now := time.Now()
	h.now = func() time.Time { return now }

	h.recordFailure("p")
	// Advance past the cooldown → healthy again.
	now = now.Add(cooldownBase + time.Second)
	if cd, _ := h.inCooldown("p"); cd {
		t.Error("expired cooldown must read as healthy")
	}
	// Expiry clears the entry state.
	if cd, _ := h.inCooldown("p"); cd {
		t.Error("expiry should be sticky-clear")
	}
}

func TestHealthUntrackedProviders(t *testing.T) {
	h := newProviderHealth()
	// Empty IDs are never tracked: recording must not poison anything.
	h.recordFailure("")
	if cd, _ := h.inCooldown(""); cd {
		t.Error("empty provider id must never be in cooldown")
	}
	h.recordSuccess("")
	// A named provider is unaffected by empty-ID noise.
	h.recordFailure("real")
	if cd, _ := h.inCooldown("real"); !cd {
		t.Error("named provider should be tracked")
	}
}

package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Streaming-friendly HTTP settings. A plain http.Client{Timeout: T} applies T
// to the ENTIRE request including the body read, so a long generation
// (reasoning models stream for minutes) is misclassified as failed the moment
// it exceeds T — even while tokens keep flowing. The correct split is:
//
//   - ResponseHeaderTimeout bounds only how long we wait for the first byte
//     (a provider that never answers).
//   - An idle watchdog (see IdleWatchdog) aborts the stream only when NO data
//     arrives for a sustained gap (a provider that stalled mid-generation).
//
// A stream that sends a trickle of chunks indefinitely is healthy and is never
// cut off by a wall-clock deadline.
const (
	// DefaultResponseHeaderTimeout is how long to wait for the first response
	// byte before declaring the provider unreachable.
	DefaultResponseHeaderTimeout = 90 * time.Second

	// DefaultStreamIdleTimeout is how long the stream may go without a single
	// chunk before it is treated as stalled (provider died / connection hung).
	DefaultStreamIdleTimeout = 60 * time.Second

	// TotalTimeout bounds NON-streaming completions, which have no idle signal
	// to measure — a total wall-clock deadline is the only safe bound there.
	TotalTimeout = 120 * time.Second
)

// NewStreamingHTTPClient returns an http.Client suitable for SSE streaming:
// no total body deadline (which would kill long generations), but a bounded
// wait for the response headers so a dead provider fails fast instead of
// hanging the turn.
func NewStreamingHTTPClient() *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	return &http.Client{Transport: base}
}

// IdleWatchdog aborts a stream that stops delivering data. It is the correct
// "timeout" for SSE: mark() must be called after every successfully read
// chunk, and if idleTimeout elapses with no activity the watchdog calls
// cancel(). The returned stop() releases the watchdog goroutine early (it also
// exits on its own when ctx is done). idleFired is set atomically before
// cancel() so the caller can distinguish an idle abort from a user cancel.
func IdleWatchdog(ctx context.Context, cancel context.CancelFunc, idleTimeout time.Duration) (mark func(), stop func(), idleFired func() bool) {
	var (
		mu       sync.Mutex
		last     = time.Now()
		idleOnce sync.Once
		fired    bool
		done     = make(chan struct{})
	)
	mark = func() {
		mu.Lock()
		last = time.Now()
		mu.Unlock()
	}
	stop = func() {
		idleOnce.Do(func() { close(done) })
	}
	idleFired = func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fired
	}
	go func() {
		ticker := time.NewTicker(idleTimeout / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				idle := time.Since(last) > idleTimeout
				if idle && !fired {
					fired = true
				}
				mu.Unlock()
				if idle {
					cancel()
					return
				}
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return mark, stop, idleFired
}

// APIError is a typed non-2xx response from a provider. The status code lets
// the routing layer decide whether a retry can possibly help: 429/5xx are
// transient (retry/fallback), 4xx auth/model errors are permanent.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error HTTP %d: %s", e.StatusCode, e.Body)
}

// ErrStreamIdle is returned when a provider stream stalls (no chunk within
// the idle window). It is retryable — the provider may recover on a new call.
var ErrStreamIdle = errors.New("provider stream idle timeout")

// IsRetryable reports whether a failed completion is worth retrying on the
// same provider before routing to a fallback. Permanent user-caused failures
// (cancel, invalid model, auth) are never retried; transient network stalls
// and provider overload are.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false // user pressed ESC — never retry a canceled turn
	}
	if errors.Is(err, ErrStreamIdle) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}
	// A stream that died mid-frame (broken pipe / truncated SSE) is transient.
	if err == errStreamTruncated {
		return true
	}
	return false
}

// errStreamTruncated marks an SSE stream that ended without [DONE] or a
// finish_reason — the provider cut the connection mid-generation.
var errStreamTruncated = errors.New("stream ended unexpectedly (no [DONE] or finish_reason — provider session/duration limit likely reached)")

// StreamTruncated reports the canonical truncated-stream error so adapters can
// return it consistently and routing can classify it as retryable.
func StreamTruncated() error {
	return errStreamTruncated
}

// RedactAPIError converts a generic error into its display-safe form,
// stripping any embedded credential material. Currently identity for typed
// errors; guards against leaking request bodies on the wire.
func RedactAPIError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "Authorization:") {
		return fmt.Errorf("provider request failed (credentials redacted)")
	}
	return err
}

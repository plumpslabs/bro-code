package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewStreamingHTTPClient(t *testing.T) {
	c := NewStreamingHTTPClient()
	if c.Timeout != 0 {
		t.Errorf("streaming client must have no total Timeout (would kill long generations), got %v", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.Transport)
	}
	if tr.ResponseHeaderTimeout != DefaultResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, DefaultResponseHeaderTimeout)
	}
}

func TestIdleWatchdogFiresOnSilence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, stop, idleFired := IdleWatchdog(ctx, cancel, 50*time.Millisecond)
	defer stop()

	time.Sleep(150 * time.Millisecond)
	if !idleFired() {
		t.Error("watchdog should have fired after idle window with no activity")
	}
}

func TestIdleWatchdogMarkKeepsAlive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mark, stop, idleFired := IdleWatchdog(ctx, cancel, 60*time.Millisecond)
	defer stop()

	for i := 0; i < 5; i++ {
		time.Sleep(30 * time.Millisecond)
		mark() // activity resets the idle clock
	}
	if idleFired() {
		t.Error("watchdog must NOT fire while activity is reported")
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"idle stream", ErrStreamIdle, true},
		{"truncated stream", StreamTruncated(), true},
		{"deadline", context.DeadlineExceeded, true},
		{"user cancel", context.Canceled, false},
		{"rate limit 429", &APIError{StatusCode: 429}, true},
		{"server error 503", &APIError{StatusCode: 503}, true},
		{"auth 401", &APIError{StatusCode: 401}, false},
		{"bad model 400", &APIError{StatusCode: 400}, false},
		{"plain provider error", fmt.Errorf("provider down"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRetryable(c.err); got != c.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestStreamCompleteIdleTimeout proves a provider that stalls mid-generation is
// aborted by the idle watchdog (bounded, retryable) instead of hanging or being
// killed by a wall-clock deadline.
func TestStreamCompleteIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fl.Flush()
		select {
		case <-time.After(5 * time.Second): // stall past the idle window
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, "test")
	a.StreamIdleTimeout = 150 * time.Millisecond

	start := time.Now()
	_, err := a.StreamComplete(context.Background(), CompletionRequest{Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected error for stalled stream")
	}
	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("expected ErrStreamIdle, got %v", err)
	}
	if !IsRetryable(err) {
		t.Error("idle stream timeout must be retryable")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("idle abort took %v, want well under the 5s stall", elapsed)
	}
}

// TestStreamCompleteSlowButAliveSucceeds is the KEY regression test: a
// generation that takes longer than any old wall-clock total timeout but keeps
// emitting chunks MUST complete. The old http.Client{Timeout:120s} design
// misclassified such streams as failed.
func TestStreamCompleteSlowButAliveSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%d\"}}]}\n\n", i)
			fl.Flush()
			time.Sleep(120 * time.Millisecond) // slow, but each gap < idle window
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, "test")
	a.StreamIdleTimeout = 400 * time.Millisecond

	var got string
	res, err := a.StreamComplete(context.Background(), CompletionRequest{Model: "m"}, func(d string) { got += d })
	if err != nil {
		t.Fatalf("slow-but-alive stream must succeed, got %v", err)
	}
	if got != "012" || res.Content != "012" {
		t.Errorf("expected content 012, got %q / %q", got, res.Content)
	}
}

// TestStreamCompleteTruncated proves a stream cut mid-generation (no [DONE])
// surfaces a retryable error rather than serving a half answer.
func TestStreamCompleteTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n")
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, "test")
	a.StreamIdleTimeout = 200 * time.Millisecond

	_, err := a.StreamComplete(context.Background(), CompletionRequest{Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected error for truncated stream")
	}
	if !errors.Is(err, StreamTruncated()) {
		t.Fatalf("expected StreamTruncated, got %v", err)
	}
	if !IsRetryable(err) {
		t.Error("truncated stream must be retryable")
	}
}

// TestCompleteHonorsContextDeadline proves non-streaming calls (which have no
// idle signal) still honor a caller-provided deadline even though the client
// itself has no total timeout.
func TestCompleteHonorsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, "test")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := a.Complete(ctx, CompletionRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected deadline error for slow non-stream response")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if !IsRetryable(err) {
		t.Error("deadline should be retryable")
	}
}

func TestOpenAIAdapterAPIBodyErrorWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "{\"error\":\"rate limited\"}")
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, "test")
	_, err := a.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected API error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 429 || !strings.Contains(apiErr.Body, "rate limited") {
		t.Errorf("APIError = %+v", apiErr)
	}
	if !IsRetryable(err) {
		t.Error("429 must be retryable")
	}
}

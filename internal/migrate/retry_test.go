package migrate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetrySucceedsWithoutRetryingOnFirstSuccess(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), DefaultMaxAttempts, nil, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Retry returned %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want exactly 1", calls)
	}
}

func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	calls := 0
	wantErr := errors.New("still broken")
	err := retryFast(t, 3, func() error {
		calls++
		return wantErr
	})
	if err == nil {
		t.Fatal("Retry returned nil, want an error after exhausting attempts")
	}
	if calls != 3 {
		t.Errorf("fn called %d times, want exactly 3 (maxAttempts)", calls)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("returned error does not wrap the last failure: %v", err)
	}
}

func TestRetrySucceedsOnALaterAttempt(t *testing.T) {
	calls := 0
	err := retryFast(t, 5, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry returned %v, want nil once fn starts succeeding", err)
	}
	if calls != 3 {
		t.Errorf("fn called %d times, want exactly 3 (stopped retrying once it succeeded)", calls)
	}
}

func TestRetryAbortsImmediatelyOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := Retry(ctx, DefaultMaxAttempts, nil, func() error {
		calls++
		return errors.New("would have retried")
	})
	if err == nil {
		t.Fatal("Retry returned nil for an already-cancelled context")
	}
	if calls > 1 {
		t.Errorf("fn called %d times after cancellation, want at most 1", calls)
	}
}

func TestRetryLogsEachFailedAttempt(t *testing.T) {
	var lines []string
	calls := 0
	_ = retryFastWithLog(t, 3, func(s string) { lines = append(lines, s) }, func() error {
		calls++
		return errors.New("nope")
	})
	// Logged before every retry, not before the final, non-retried failure.
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2 (one per retry, not the last attempt)", len(lines))
	}
}

// TestRetryStopsImmediatelyOnStopRetrying is a regression test for a real
// bug found live: discovery calls ReadWPConfig against every candidate
// vhost root, many of which (nginx's own ACME-challenge webroot among
// them) are never going to have a wp-config.php — a deterministic "no"
// that retrying cannot change. Before StopRetrying existed, that single
// expected miss cost 20 attempts with a growing backoff, several minutes
// per non-WordPress vhost discovery walked past.
func TestRetryStopsImmediatelyOnStopRetrying(t *testing.T) {
	calls := 0
	wantErr := errors.New("no such file")
	err := retryFast(t, DefaultMaxAttempts, func() error {
		calls++
		return StopRetrying(wantErr)
	})
	if calls != 1 {
		t.Errorf("fn called %d times, want exactly 1 (StopRetrying must not be retried)", calls)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("returned error does not unwrap to the original: %v", err)
	}
}

func TestRetryStopsAfterATransientRetrySucceedsOrStops(t *testing.T) {
	calls := 0
	err := retryFast(t, DefaultMaxAttempts, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient, keep trying")
		}
		return StopRetrying(errors.New("now a deterministic failure"))
	})
	if err == nil {
		t.Fatal("Retry returned nil, want the StopRetrying error surfaced")
	}
	if calls != 3 {
		t.Errorf("fn called %d times, want exactly 3 (2 transient retries, then StopRetrying ends it)", calls)
	}
}

func TestStopRetryingOfNilIsNil(t *testing.T) {
	if err := StopRetrying(nil); err != nil {
		t.Errorf("StopRetrying(nil) = %v, want nil", err)
	}
}

func TestBackoffIsCappedAndIncreasing(t *testing.T) {
	if backoff(1) >= backoff(2) {
		t.Error("backoff is not increasing between attempt 1 and 2")
	}
	if got := backoff(100); got != 30*time.Second {
		t.Errorf("backoff(100) = %s, want capped at 30s", got)
	}
}

// retryFast/retryFastWithLog exercise retryWithBackoff (the same loop Retry
// itself runs) with a near-zero injected delay, so attempt-counting and
// error-propagation can be tested without actually waiting through this
// package's real, multi-second-per-attempt backoff.
func retryFast(t *testing.T, maxAttempts int, fn func() error) error {
	t.Helper()
	return retryFastWithLog(t, maxAttempts, nil, fn)
}

func retryFastWithLog(t *testing.T, maxAttempts int, log func(string), fn func() error) error {
	t.Helper()
	return retryWithBackoff(context.Background(), maxAttempts, log, func(int) time.Duration { return time.Millisecond }, fn)
}

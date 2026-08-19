package migrate

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultMaxAttempts is how many times a retryable step is tried before
// giving up — the "keep retrying up to 20 times" a large, flaky transfer
// needs, without retrying forever.
const DefaultMaxAttempts = 20

// Retry calls fn until it succeeds or maxAttempts is reached, waiting a
// capped, linearly increasing backoff between attempts (3s, 6s, 9s... up to
// 30s) — long enough that a brief network drop or an SSH server momentarily
// refusing new connections during a reboot has a real chance to clear
// before the next try, short enough that 20 attempts is minutes, not hours.
//
// A context cancellation aborts immediately without spending a retry on it
// — the operator stopping the job is not a transient failure to wait out.
//
// log receives one line before each wait, so the caller can surface
// "attempt 3/20 failed: ... — retrying in 9s" in the migration log the
// operator is watching live; nil is fine when no such log exists yet (unit
// tests).
func Retry(ctx context.Context, maxAttempts int, log func(string), fn func() error) error {
	return retryWithBackoff(ctx, maxAttempts, log, backoff, fn)
}

// retryWithBackoff is Retry with the delay-per-attempt function injectable,
// so unit tests can exercise attempt-counting and error-propagation without
// actually waiting through this package's real (multi-second) backoff —
// Retry itself always calls this with the real backoff and is what every
// non-test caller uses.
func retryWithBackoff(ctx context.Context, maxAttempts int, log func(string), delayFor func(int) time.Duration, fn func() error) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		var stop stopRetrying
		if errors.As(lastErr, &stop) {
			return stop.err
		}
		if attempt == maxAttempts {
			break
		}
		delay := delayFor(attempt)
		if log != nil {
			log(fmt.Sprintf("attempt %d/%d failed: %v — retrying in %s", attempt, maxAttempts, lastErr, delay))
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("failed after %d attempts: %w", maxAttempts, lastErr)
}

// stopRetrying wraps an error to tell Retry not to try again — for a
// failure that ran successfully as far as the network is concerned and
// simply came back negative (a file that does not exist, a command that
// exited non-zero because of what it found, not because it could not
// reach the remote host at all). Retrying that exact same check twenty
// times with a growing backoff cannot change its answer; it can only turn
// one wrong guess into several minutes of one.
type stopRetrying struct{ err error }

func (s stopRetrying) Error() string { return s.err.Error() }
func (s stopRetrying) Unwrap() error { return s.err }

// StopRetrying marks err as final — Retry returns it immediately (unwrapped)
// instead of spending any more attempts on it. Wrapping nil returns nil.
func StopRetrying(err error) error {
	if err == nil {
		return nil
	}
	return stopRetrying{err}
}

func backoff(attempt int) time.Duration {
	d := time.Duration(attempt) * 3 * time.Second
	const capDur = 30 * time.Second
	if d > capDur {
		d = capDur
	}
	return d
}

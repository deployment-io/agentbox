package agent

import (
	"context"
	"sync/atomic"
	"time"
)

// activityTracker is an io.Writer that records a timestamp on every
// write and discards the data. Used in an io.MultiWriter alongside the
// subprocess's real stdout / stderr sinks so the watcher can tell when
// the agent has gone quiet. Concurrent-safe via atomic.
type activityTracker struct {
	lastActivity atomic.Int64 // unix nanoseconds
}

func newActivityTracker() *activityTracker {
	t := &activityTracker{}
	t.lastActivity.Store(time.Now().UnixNano())
	return t
}

// Write records the current time; never errors.
func (t *activityTracker) Write(p []byte) (int, error) {
	t.lastActivity.Store(time.Now().UnixNano())
	return len(p), nil
}

func (t *activityTracker) SinceLastActivity() time.Duration {
	last := t.lastActivity.Load()
	return time.Since(time.Unix(0, last))
}

// watchActivity sends on reached when SinceLastActivity exceeds timeout.
// Returns cleanly on ctx cancellation or when timeout is 0 (disabled).
func watchActivity(ctx context.Context, tracker *activityTracker, timeout time.Duration, reached chan<- struct{}) {
	if timeout <= 0 {
		return
	}

	interval := timeout / 10
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	if interval > timeout {
		interval = timeout
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if tracker.SinceLastActivity() >= timeout {
				select {
				case reached <- struct{}{}:
				case <-ctx.Done():
				}
				return
			}
		}
	}
}

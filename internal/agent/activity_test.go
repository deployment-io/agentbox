package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestActivityTracker_WriteResetsTimestamp(t *testing.T) {
	tr := newActivityTracker()

	time.Sleep(30 * time.Millisecond)
	beforeWrite := tr.SinceLastActivity()
	if beforeWrite < 25*time.Millisecond {
		t.Errorf("SinceLastActivity should be ~30ms before write, got %v", beforeWrite)
	}

	_, _ = tr.Write([]byte("hello"))
	afterWrite := tr.SinceLastActivity()
	if afterWrite > 10*time.Millisecond {
		t.Errorf("SinceLastActivity right after Write should be near zero, got %v", afterWrite)
	}
}

func TestActivityTracker_ConcurrentWritesSafe(t *testing.T) {
	tr := newActivityTracker()

	const writers = 100
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = tr.Write([]byte("data"))
			}
		}()
	}
	wg.Wait()

	// After a burst of concurrent writes, the last-activity timestamp
	// should be very recent.
	if elapsed := tr.SinceLastActivity(); elapsed > 100*time.Millisecond {
		t.Errorf("SinceLastActivity after concurrent burst = %v, want < 100ms", elapsed)
	}
}

func TestWatchActivity_FiresOnInactivity(t *testing.T) {
	tr := newActivityTracker()
	reached := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go watchActivity(ctx, tr, 150*time.Millisecond, reached)

	select {
	case <-reached:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("watcher should have fired within 2 seconds of sustained inactivity")
	}
}

func TestWatchActivity_DoesNotFireWhileActive(t *testing.T) {
	tr := newActivityTracker()
	reached := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go watchActivity(ctx, tr, 500*time.Millisecond, reached)

	// Write every 50ms for 1 second; always more recent than the
	// 500ms inactivity threshold, so the watcher must not fire.
	deadline := time.After(1 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-reached:
			t.Fatal("watcher fired while activity was present")
		case <-deadline:
			return
		case <-ticker.C:
			_, _ = tr.Write([]byte("data"))
		}
	}
}

func TestWatchActivity_DisabledWhenTimeoutZero(t *testing.T) {
	tr := newActivityTracker()
	reached := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		watchActivity(ctx, tr, 0, reached)
		close(done)
	}()

	// With timeout=0, the watcher should return immediately (disabled)
	// and never fire on reached.
	select {
	case <-done:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watcher with timeout=0 should return immediately")
	}

	select {
	case <-reached:
		t.Fatal("watcher with timeout=0 should never fire")
	default:
		// expected
	}
}

func TestWatchActivity_ReturnsOnContextCancel(t *testing.T) {
	tr := newActivityTracker()
	reached := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		watchActivity(ctx, tr, 1*time.Hour, reached)
		close(done)
	}()

	// Cancel after a short delay; the watcher should return cleanly
	// without firing on reached.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// good
	case <-time.After(1 * time.Second):
		t.Fatal("watcher should return when context cancelled")
	}

	select {
	case <-reached:
		t.Fatal("watcher should not fire when context cancelled during wait")
	default:
	}
}

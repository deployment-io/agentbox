package progress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource lets tests pin exact Snapshot values returned per call,
// and counts how many times Snapshot() was invoked.
type fakeSource struct {
	calls atomic.Int64
	snap  atomic.Pointer[Snapshot]
}

func (f *fakeSource) Snapshot() Snapshot {
	f.calls.Add(1)
	if s := f.snap.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

func (f *fakeSource) set(s Snapshot) { f.snap.Store(&s) }

// TestWritesProgressFile pins the basic happy path: Start, wait one
// tick, the file exists with the expected shape, schema_version is
// stamped, and updated_at_unix is populated.
func TestWritesProgressFile(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{}
	src.set(Snapshot{Turns: 5, InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 0})

	w := NewWriter(dir, src)
	w.Start()
	defer w.Stop()

	// Wait long enough for at least one tick + write completion.
	deadline := time.Now().Add(2 * WriteInterval)
	path := filepath.Join(dir, Filename)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("progress.json not written: %v", err)
	}
	var got Snapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SchemaVersion != schemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, schemaVersion)
	}
	if got.Turns != 5 || got.InputTokens != 1000 || got.OutputTokens != 200 {
		t.Errorf("snapshot fields not propagated: %+v", got)
	}
	if got.UpdatedAtUnix == 0 {
		t.Error("updated_at_unix should be stamped on each write")
	}
}

// TestStopFlushesFinalSnapshot pins that Stop performs one last write
// before returning. Without this, a heartbeat firing right after Stop
// would see a stale snapshot up to WriteInterval old. Approach: set
// snap to value A, Start (initial tick may or may not have fired),
// set snap to value B, immediately Stop, then read the file — must
// reflect B (the post-Stop flush) regardless of whether the periodic
// tick had time to fire.
func TestStopFlushesFinalSnapshot(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{}
	src.set(Snapshot{Turns: 1})

	w := NewWriter(dir, src)
	w.Start()

	// Update the source to a distinct value, then immediately Stop.
	// The Stop-triggered final write must capture this value.
	src.set(Snapshot{Turns: 99, InputTokens: 4242})
	w.Stop()

	data, err := os.ReadFile(filepath.Join(dir, Filename))
	if err != nil {
		t.Fatalf("progress.json not written: %v", err)
	}
	var got Snapshot
	_ = json.Unmarshal(data, &got)
	if got.Turns != 99 || got.InputTokens != 4242 {
		t.Errorf("final flush did not capture latest snapshot: %+v", got)
	}
}

// TestAtomicRename pins that no `.tmp` file is left behind after a
// successful write — the rename should sweep it into place. A leaked
// .tmp would suggest a code path that writes without the rename, or
// a write-then-fail-rename path.
func TestAtomicRename(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{}
	src.set(Snapshot{Turns: 1})

	w := NewWriter(dir, src)
	w.Start()
	defer w.Stop()

	// Wait for at least one write.
	deadline := time.Now().Add(2 * WriteInterval)
	path := filepath.Join(dir, Filename)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Tmp file must not exist.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file leaked: stat err = %v (want IsNotExist)", err)
	}
}

// TestStopIsIdempotent pins that calling Stop twice doesn't deadlock
// or panic — defensive against caller patterns where Stop fires
// from both a defer and an explicit cleanup path.
func TestStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{}
	w := NewWriter(dir, src)
	w.Start()
	w.Stop()
	w.Stop() // must not panic / deadlock
}

// TestStopWithoutStartIsNoOp pins that Stop is safe to call when
// Start was never invoked — defends against deferred-cleanup paths
// where Stop fires before construction is complete (e.g., a panic
// in NewWriter's caller before Start could be reached). Without the
// `started` guard in Stop, the bare `<-w.doneCh` would block
// forever waiting for a goroutine that was never spawned. Test
// guards against regression by running Stop in a goroutine and
// asserting it returns within a tight deadline.
func TestStopWithoutStartIsNoOp(t *testing.T) {
	w := NewWriter(t.TempDir(), &fakeSource{})
	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()
	select {
	case <-done:
		// Stop returned cleanly — contract upheld.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop without Start blocked indefinitely")
	}
}

// TestStartIsIdempotent pins symmetrically that Start can be called
// multiple times without spawning multiple goroutines (which would
// cause double-writes to the same file).
func TestStartIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{}
	w := NewWriter(dir, src)
	w.Start()
	w.Start() // must not spawn a second loop
	defer w.Stop()

	// Wait one full WriteInterval + grace; then count source calls.
	// With a single loop, we expect ~1 call. With duplicated loops,
	// we'd see ~2+ calls in the same window.
	time.Sleep(WriteInterval + 200*time.Millisecond)
	calls := src.calls.Load()
	if calls > 2 {
		t.Errorf("Start was not idempotent: source called %d times in one tick window", calls)
	}
}

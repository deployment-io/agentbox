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

// TestSkipsWriteWhenContentUnchanged pins the dedup contract: a
// write() with the same source content as the prior write must NOT
// touch the file. Without this, the runner's downstream dedup
// (which keys off UpdatedAtUnix) misfires every tick and the
// heartbeat forwards redundant payloads.
//
// Detection: each real write stamps a fresh UpdatedAtUnix. After
// two back-to-back write()s with identical source content, the
// persisted UpdatedAtUnix must equal the value from the first write
// — proof that the second write was skipped. Then we change the
// source and confirm the next write fires (UpdatedAtUnix advances,
// content reflects the change).
//
// Calls write() directly rather than going through Start/loop —
// keeps the test fast and deterministic regardless of WriteInterval.
func TestSkipsWriteWhenContentUnchanged(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{}
	src.set(Snapshot{Turns: 5, InputTokens: 100, OutputTokens: 50, CacheReadTokens: 200})

	w := NewWriter(dir, src)

	// First write — no prior baseline, must fire.
	if err := w.write(); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first := readPersistedSnapshot(t, dir)
	if first.UpdatedAtUnix == 0 {
		t.Fatal("first write didn't stamp UpdatedAtUnix")
	}

	// Sleep past 1s so the next write would observe a different
	// UpdatedAtUnix value if it actually fired (UpdatedAtUnix is
	// unix-second resolution).
	time.Sleep(1100 * time.Millisecond)

	// Second write with identical source → must be skipped.
	if err := w.write(); err != nil {
		t.Fatalf("second write: %v", err)
	}
	second := readPersistedSnapshot(t, dir)
	if second.UpdatedAtUnix != first.UpdatedAtUnix {
		t.Errorf("write was not skipped: UpdatedAtUnix advanced from %d to %d despite identical source content",
			first.UpdatedAtUnix, second.UpdatedAtUnix)
	}

	// Third write after content change → must fire and reflect new content.
	src.set(Snapshot{Turns: 6, InputTokens: 200, OutputTokens: 80, CacheReadTokens: 300})
	if err := w.write(); err != nil {
		t.Fatalf("third write: %v", err)
	}
	third := readPersistedSnapshot(t, dir)
	if third.UpdatedAtUnix == first.UpdatedAtUnix {
		t.Error("write did not fire after content change: UpdatedAtUnix unchanged")
	}
	if third.Turns != 6 || third.InputTokens != 200 {
		t.Errorf("third write didn't reflect new source content: %+v", third)
	}
}

func readPersistedSnapshot(t *testing.T, dir string) Snapshot {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, Filename))
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal persisted snapshot: %v", err)
	}
	return s
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

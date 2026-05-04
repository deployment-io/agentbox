// Package progress implements a periodic JSON snapshot of in-flight
// agent run state — turn count and token usage — for consumption by
// the runner via heartbeat. The writer drops a `progress.json` next
// to the (otherwise on-exit-only) `/result.json`; the runner polls
// the file on its existing 5s heartbeat cadence and forwards the
// snapshot to the server, where the dashboard surfaces it as live
// progress counters.
//
// Why a file vs. a stdout sentinel or a sidecar HTTP server:
//   - File is already the contract surface for `/result.json`; this
//     piggybacks on the same bind-mount the runner already inspects.
//   - Stdout sentinels couple the runner to the agent's exact output
//     format and make log parsing a load-bearing path.
//   - HTTP sidecar is overkill for a 5-second-cadence push.
//
// Atomicity: each write goes to `progress.json.tmp` and is then
// `os.Rename`'d to `progress.json`. The rename is atomic on POSIX
// filesystems (same directory), so the runner never observes a
// partial write — it sees either the previous snapshot or the new
// one, never a half-written JSON document.
//
// Cadence: WriteInterval is shorter than the runner's heartbeat (5s)
// so each heartbeat sees a snapshot that's at most ~3-5s stale.
// Bumping WriteInterval below ~1s would just churn the disk for no
// observable benefit.
package progress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Filename is the on-disk name of the progress snapshot written into
// the same directory as `/result.json`.
const Filename = "progress.json"

// schemaVersion bumps on incompatible shape changes. Additive fields
// don't require a bump — readers tolerate unknown JSON keys.
const schemaVersion = 1

// WriteInterval is the cadence at which the writer snapshots the
// source. Matched to the runner's 5s heartbeat — there's no benefit
// to writing faster than the slowest downstream reader. With the
// content-change dedup below, no-change ticks are also a no-op
// (skip the disk write entirely), so the cadence's only real cost
// is for ticks that actually have new data to publish.
const WriteInterval = 5 * time.Second

// Snapshot is the on-disk shape of `progress.json`. Mirrors the runner-
// kit `LiveProgressV1` struct field-for-field so the runner can
// unmarshal directly.
type Snapshot struct {
	SchemaVersion   int   `json:"schema_version"`
	UpdatedAtUnix   int64 `json:"updated_at_unix"`
	Turns           int   `json:"turns"`
	InputTokens     int   `json:"input_tokens"`
	OutputTokens    int   `json:"output_tokens"`
	CacheReadTokens int   `json:"cache_read_tokens"`
}

// Source is the minimal interface the Writer reads on each tick.
// Implementations adapt the agent's parser state into the on-disk
// shape. Snapshot may be called concurrently with the agent's own
// parser updates — implementations are responsible for returning a
// consistent value (typically a copy of internal state).
type Source interface {
	Snapshot() Snapshot
}

// Writer periodically serializes a Snapshot from Source into
// `<dir>/progress.json`. Goroutine lifetime is bounded by Stop —
// callers MUST Stop the writer before the directory is removed,
// otherwise a final write may race the cleanup.
type Writer struct {
	dir    string
	src    Source
	stopCh chan struct{}
	doneCh chan struct{}

	// started flips once via Start. Stop reads this to decide whether
	// to wait on doneCh — without the guard, calling Stop without
	// Start (e.g., from a panic-recovery deferred cleanup that runs
	// before Start was reached) would block forever waiting for a
	// goroutine that was never spawned.
	started   atomic.Bool
	startOnce sync.Once
	stopOnce  sync.Once

	// lastWritten holds the most-recently-persisted Snapshot's content
	// fields (Turns + token counts) so write() can skip when the
	// source's current state is bit-for-bit identical. Without this,
	// every tick stamps a fresh UpdatedAtUnix and the runner forwards
	// a "new" payload even when nothing changed — wasting heartbeat
	// RPCs and a server-side write per tick when the agent is between
	// turns. Only accessed from the loop goroutine, so no sync needed.
	lastWritten    Snapshot
	hasLastWritten bool
}

// NewWriter constructs a Writer that will drop `progress.json` into
// dir on each tick. Call Start to begin, Stop to flush + exit.
func NewWriter(dir string, src Source) *Writer {
	return &Writer{
		dir:    dir,
		src:    src,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// Start spawns the periodic-write goroutine. Idempotent — safe to
// call multiple times; only the first invocation actually starts.
func (w *Writer) Start() {
	w.startOnce.Do(func() {
		w.started.Store(true)
		go w.loop()
	})
}

// Stop signals the loop to exit and waits for it. The loop performs
// a final write before returning so the runner sees the latest state
// even if Stop fires immediately after Start. Idempotent. Stop without
// a prior Start is a no-op (returns immediately) — defends against
// deferred-cleanup paths where Stop fires before construction is
// complete.
func (w *Writer) Stop() {
	if !w.started.Load() {
		return
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.doneCh
}

func (w *Writer) loop() {
	defer close(w.doneCh)
	ticker := time.NewTicker(WriteInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			// Final flush so the runner sees the latest state on
			// shutdown — without it, the heartbeat following Stop
			// would see a stale snapshot from up to WriteInterval ago.
			_ = w.write()
			return
		case <-ticker.C:
			_ = w.write()
		}
	}
}

// write serializes a Snapshot, writes to a sibling `.tmp` file, then
// atomically renames into place. Errors are returned for the caller
// to log; the loop ignores them (a transient disk error shouldn't
// kill the goroutine — the next tick retries).
//
// Skips the write entirely (returns nil) when the source's content
// is bit-for-bit identical to the last persisted snapshot. This
// keeps `UpdatedAtUnix` honest about its semantic ("when did the
// underlying state last change") and lets the runner's downstream
// dedup actually fire — without it, every tick stamps a fresh
// timestamp and the runner forwards a redundant payload via
// heartbeat. The first write always happens (no prior baseline).
func (w *Writer) write() error {
	snap := w.src.Snapshot()
	if w.hasLastWritten && sameContent(snap, w.lastWritten) {
		return nil
	}
	snap.SchemaVersion = schemaVersion
	snap.UpdatedAtUnix = time.Now().Unix()

	final := filepath.Join(w.dir, Filename)
	tmp := final + ".tmp"

	data, err := json.Marshal(&snap)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	w.lastWritten = snap
	w.hasLastWritten = true
	return nil
}

// sameContent reports whether two Snapshots have identical "content"
// fields — the data the agent actually emits, excluding the
// stamped-by-Writer SchemaVersion and UpdatedAtUnix.
func sameContent(a, b Snapshot) bool {
	return a.Turns == b.Turns &&
		a.InputTokens == b.InputTokens &&
		a.OutputTokens == b.OutputTokens &&
		a.CacheReadTokens == b.CacheReadTokens
}

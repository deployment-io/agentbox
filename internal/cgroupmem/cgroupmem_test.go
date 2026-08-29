package cgroupmem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withCgroup lays down a fake cgroup tree and points the package at it. The
// production runner is Amazon Linux 2 (cgroup v1) while most dev machines and
// CI are v2, so without this the layout we actually ship for would go
// untested.
func withCgroup(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := root
	root = dir
	t.Cleanup(func() { root = old })
}

const fourGiB = "4294967296"

// v2 tree with a kill recorded — the case that would have answered
// 2026-08-28's "was it an OOM?" outright.
func TestReadV2RecordsAnOOMKill(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory.events":  "low 0\nhigh 0\nmax 12\noom 1\noom_kill 1\n",
		"memory.max":     fourGiB,
		"memory.peak":    "4294000000",
		"memory.current": "1048576",
	})
	s := Read()
	if !s.Available || !s.OOMKillsKnown || s.OOMKills != 1 {
		t.Fatalf("got %+v, want an available reading with a known kill count of 1", s)
	}
	if s.LimitBytes != 4<<30 || s.PeakBytes != 4294000000 {
		t.Errorf("limit/peak = %d/%d", s.LimitBytes, s.PeakBytes)
	}
	if msg := Explain(true); !strings.Contains(msg, "OOM-killed") ||
		!strings.Contains(msg, "AGENTBOX_MEMORY_BYTES") {
		t.Errorf("Explain = %q, want it to name the cause and the knob", msg)
	}
}

// Counter present and zero is a real answer — it RULES OUT memory, which is
// as useful as confirming it and must not be conflated with "unknown".
func TestSignalDeathWithNoKillRecordedRulesMemoryOut(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory.events":  "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n",
		"memory.max":     fourGiB,
		"memory.peak":    "1073741824",
		"memory.current": "1048576",
	})
	msg := Explain(true)
	if !strings.Contains(msg, "no OOM kill") || !strings.Contains(msg, "not the cause") {
		t.Errorf("Explain = %q, want it to state memory is ruled out", msg)
	}
	if strings.Contains(msg, "undetermined") {
		t.Error("a present-and-zero counter is not undetermined")
	}
}

// cgroup v1 is what the production runner actually has (Amazon Linux 2,
// kernel 5.10) — the layout most likely to go untested by accident.
func TestReadV1RecordsAnOOMKill(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory/memory.oom_control":        "oom_kill_disable 0\nunder_oom 0\noom_kill 2\n",
		"memory/memory.limit_in_bytes":     fourGiB,
		"memory/memory.max_usage_in_bytes": "4293000000",
		"memory/memory.usage_in_bytes":     "2000000",
	})
	s := Read()
	if !s.OOMKillsKnown || s.OOMKills != 2 {
		t.Fatalf("got %+v, want a known kill count of 2 from the v1 layout", s)
	}
	if msg := Explain(true); !strings.Contains(msg, "OOM-killed") {
		t.Errorf("Explain = %q", msg)
	}
}

// THE false-negative guard. oom_kill landed in memory.oom_control in kernel
// 4.13; before that the field is simply absent. A plain zero there would read
// as "not an OOM" and send the next person debugging the wrong way — exactly
// the mistake this package exists to stop.
func TestV1WithoutTheOOMCounterReportsUndetermined(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory/memory.oom_control":        "oom_kill_disable 0\nunder_oom 0\n",
		"memory/memory.limit_in_bytes":     fourGiB,
		"memory/memory.max_usage_in_bytes": "4000000000",
	})
	s := Read()
	if s.OOMKillsKnown {
		t.Fatal("an absent counter must not report as known")
	}
	msg := Explain(true)
	if !strings.Contains(msg, "undetermined") {
		t.Errorf("Explain = %q, want the cause left open", msg)
	}
	if strings.Contains(msg, "not the cause") {
		t.Error("must not rule memory out when the counter is absent")
	}
}

// Without a cgroup namespace the mount is the HOST's root cgroup, whose
// oom_kill aggregates the whole box. Attributing that to this job would
// manufacture a confident wrong answer, which is worse than none — the
// no-limit tell is what rejects it.
func TestHostRootCgroupIsRejected(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory.events":  "oom_kill 47\n",
		"memory.max":     "max",
		"memory.current": "8000000000",
	})
	if s := Read(); s.Available {
		t.Fatalf("got %+v, want the unlimited (host) cgroup discarded", s)
	}
	if msg := Explain(true); msg != "" {
		t.Errorf("Explain = %q, want silence rather than a borrowed explanation", msg)
	}
}

// v1's "unlimited" is a huge sentinel rather than a word, and it varies by
// page size and kernel — so it is matched by magnitude, not by value.
func TestV1UnlimitedSentinelIsRejected(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory/memory.oom_control":    "oom_kill 3\n",
		"memory/memory.limit_in_bytes": "9223372036854771712",
	})
	if s := Read(); s.Available {
		t.Fatalf("got %+v, want the unlimited v1 cgroup discarded", s)
	}
}

// Outside a container there is no cgroup to read, and the caller must degrade
// to saying nothing rather than guessing.
func TestNoCgroupIsSilent(t *testing.T) {
	withCgroup(t, map[string]string{})
	if s := Read(); s.Available {
		t.Fatalf("got %+v, want an unavailable reading", s)
	}
	for _, killed := range []bool{true, false} {
		if msg := Explain(killed); msg != "" {
			t.Errorf("Explain(%v) = %q, want silence", killed, msg)
		}
	}
}

// A healthy run says nothing; a run that merely ran hot says so BEFORE it
// becomes a failure, which is the whole point of reporting on success too.
func TestSuccessfulRunOnlySpeaksWhenItRanHot(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory.events": "oom_kill 0\n",
		"memory.max":    fourGiB,
		"memory.peak":   "1073741824", // 1 GiB of 4 — comfortable
	})
	if msg := Explain(false); msg != "" {
		t.Errorf("Explain = %q, want silence on a comfortable run", msg)
	}

	withCgroup(t, map[string]string{
		"memory.events": "oom_kill 0\n",
		"memory.max":    fourGiB,
		"memory.peak":   "4187593113", // ~97% of 4 GiB
	})
	if msg := Explain(false); !strings.Contains(msg, "close to the limit") {
		t.Errorf("Explain = %q, want a warning at 97%% of the limit", msg)
	}
}

// Without memory.peak (cgroup v2 before 5.19) usage-at-exit is all there is,
// and it UNDERSTATES the peak. Reporting it as though it were the peak would
// make a run that nearly died look comfortable.
func TestMissingPeakIsLabelledAsAnUnderstatement(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory.events":  "oom_kill 1\n",
		"memory.max":     fourGiB,
		"memory.current": "500000000",
	})
	msg := Explain(true)
	if !strings.Contains(msg, "usage at exit") || !strings.Contains(msg, "true peak was higher") {
		t.Errorf("Explain = %q, want the figure labelled as an understatement", msg)
	}
}

// The production-runner hedge. oom_kill was added to v1's memory.oom_control
// in kernel 4.13, and Amazon Linux 2 is exactly the kind of long-lived image
// where that assumption deserves a fallback. failcnt is universally present on
// v1 (runc reads it), so when the kill counter is missing but the cgroup did
// hit its ceiling, say so — as suspicion, not as a confirmed kill.
func TestV1FailcntCarriesTheDiagnosisWhenOOMCounterIsAbsent(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory/memory.oom_control":        "oom_kill_disable 0\nunder_oom 0\n",
		"memory/memory.limit_in_bytes":     fourGiB,
		"memory/memory.max_usage_in_bytes": "4294967296",
		"memory/memory.failcnt":            "918",
	})
	msg := Explain(true)
	if !strings.Contains(msg, "hit its memory limit 918 time(s)") {
		t.Errorf("Explain = %q, want the failcnt evidence", msg)
	}
	if !strings.Contains(msg, "likely but unconfirmed") {
		t.Error("failcnt is evidence, not proof — it must not claim a confirmed kill")
	}
	if strings.Contains(msg, "OOM-killed by the kernel") {
		t.Error("must not assert a kill the kernel never reported")
	}
}

// failcnt of zero adds nothing, so the report stays honestly undetermined
// rather than implying memory was fine.
func TestV1ZeroFailcntStaysUndetermined(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory/memory.oom_control":    "oom_kill_disable 0\nunder_oom 0\n",
		"memory/memory.limit_in_bytes": fourGiB,
		"memory/memory.failcnt":        "0",
	})
	if msg := Explain(true); !strings.Contains(msg, "undetermined") {
		t.Errorf("Explain = %q, want undetermined", msg)
	}
}

// A real kill count always wins over failcnt — the counter is proof, failcnt
// is only evidence, and reporting the weaker one would be a downgrade.
func TestOOMCounterWinsOverFailcnt(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory/memory.oom_control":        "oom_kill_disable 0\nunder_oom 0\noom_kill 1\n",
		"memory/memory.limit_in_bytes":     fourGiB,
		"memory/memory.max_usage_in_bytes": "4294967296",
		"memory/memory.failcnt":            "918",
	})
	msg := Explain(true)
	if !strings.Contains(msg, "OOM-killed by the kernel") {
		t.Errorf("Explain = %q, want the confirmed kill", msg)
	}
	if strings.Contains(msg, "unconfirmed") {
		t.Error("a present kill counter is proof; it must not be hedged")
	}
}

// NearLimit multiplies the limit in the obvious formulation, which overflows
// uint64 for a large ceiling and silently inverts the comparison. A limit this
// size is unrealistic, but a wrong answer from arithmetic is not a thing to
// leave in a diagnostic.
func TestNearLimitDoesNotOverflowOnAHugeLimit(t *testing.T) {
	huge := uint64(1) << 61
	if (Stats{Available: true, LimitBytes: huge, PeakBytes: 1024}).NearLimit() {
		t.Error("1 KiB against an exabyte limit is not near the limit")
	}
	if !(Stats{Available: true, LimitBytes: huge, PeakBytes: huge}).NearLimit() {
		t.Error("usage at the limit must register as near it")
	}
}

// Package cgroupmem reads the container's own memory cgroup: how much it may
// use, how close it came, and whether the kernel killed anything in it.
//
// It exists because an OOM kill is otherwise invisible from inside. The agent
// subprocess dies with SIGKILL and nothing says why — the runner reports
// `claude exited with error: signal: killed`, which reads like a crash and
// sends whoever is debugging it looking at the agent rather than at the memory
// limit. That happened on 2026-08-28 and cost an evening of theorising about
// causes that could have been one counter read.
//
// The counter is authoritative in a way an exit code is not: memory.events'
// oom_kill (v2) and memory.oom_control's oom_kill (v1) increment for EVERY
// process the cgroup kills, including a child while PID 1 survives — which is
// exactly the shape that confused us, since agentbox lived to report the death
// of a process it did not know had been killed by the kernel.
package cgroupmem

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroup v2 puts the container's own files at the root of the mount; v1 puts
// them under a per-controller directory. Which one is present is how we tell
// the versions apart, rather than sniffing the kernel or the mount type.
const (
	v2Events  = "memory.events"
	v2Max     = "memory.max"
	v2Peak    = "memory.peak"
	v2Current = "memory.current"

	v1OOMControl = "memory/memory.oom_control"
	v1Limit      = "memory/memory.limit_in_bytes"
	v1MaxUsage   = "memory/memory.max_usage_in_bytes"
	v1Usage      = "memory/memory.usage_in_bytes"
)

// root is the cgroup mount. A variable rather than a constant solely so the
// tests can lay down a v1 or a v2 tree and drive the real parsing against it —
// there is no other way to exercise a layout the build host does not have, and
// the production runner (Amazon Linux 2, cgroup v1) differs from most dev
// machines, so the layout we ship for is the one least likely to be covered by
// accident.
var root = "/sys/fs/cgroup"

func path(rel string) string { return filepath.Join(root, rel) }

// v1UnlimitedThreshold: cgroup v1 reports "no limit" as a huge sentinel
// (PAGE_COUNTER_MAX * page size) rather than a word, and the exact value
// varies with page size and kernel. Anything in that range is not a limit
// anyone set, so treat it as unlimited rather than printing an absurd number.
const v1UnlimitedThreshold = uint64(1) << 62

// Stats is what the cgroup will tell us. Every field is best-effort: a field
// the kernel or the mount does not expose stays zero, and Available reports
// whether anything at all was readable.
type Stats struct {
	// LimitBytes is the memory ceiling; 0 when unlimited or unknown.
	LimitBytes uint64
	// PeakBytes is the high-water mark. Zero on kernels without
	// memory.peak (cgroup v2 before 5.19), where nothing records it.
	PeakBytes uint64
	// CurrentBytes is usage at the moment of the read — the fallback when
	// no peak is recorded, and always an UNDERSTATEMENT of the peak.
	CurrentBytes uint64
	// OOMKills counts processes the kernel killed in this cgroup. Non-zero
	// is the unambiguous answer to "was this an OOM?".
	OOMKills uint64
	// OOMKillsKnown records that the counter was actually PRESENT and
	// parsed, as opposed to absent and defaulted to zero.
	//
	// The distinction is the whole point of this package. cgroup v1 gained
	// oom_kill in kernel 4.13, and a future layout could rename or drop it
	// again — in either case a plain zero would read as "not an OOM" and
	// send the next person debugging exactly where we sent ourselves. When
	// this is false the honest report is "could not determine", never "no".
	OOMKillsKnown bool
	// Available is false when no cgroup memory files could be read at all,
	// which is the normal case outside a container.
	Available bool
}

// NearLimit reports whether usage got close enough to the ceiling to be worth
// saying out loud on a run that otherwise succeeded. 90% is a threshold, not a
// measurement — the point is to surface pressure before it becomes a kill,
// since today the first sign of it is a job dying.
func (s Stats) NearLimit() bool {
	if !s.Available || s.LimitBytes == 0 {
		return false
	}
	used := s.PeakBytes
	if used == 0 {
		used = s.CurrentBytes
	}
	return used*10 >= s.LimitBytes*9
}

// Read returns what the container's memory cgroup reports. It never fails:
// unreadable means unknown, and callers degrade to saying nothing rather than
// guessing.
func Read() Stats {
	if s, ok := readV2(); ok {
		return s
	}
	if s, ok := readV1(); ok {
		return s
	}
	return Stats{}
}

func readV2() (Stats, bool) {
	events, err := os.ReadFile(path(v2Events))
	if err != nil {
		return Stats{}, false
	}
	s := Stats{Available: true}
	s.OOMKills, s.OOMKillsKnown = fieldValue(string(events), "oom_kill")
	// "max" is v2's word for unlimited, and parses to 0 here, which is what
	// LimitBytes means by unknown.
	s.LimitBytes = readUint(path(v2Max))
	s.PeakBytes = readUint(path(v2Peak))
	s.CurrentBytes = readUint(path(v2Current))
	return ownCgroupOnly(s), true
}

func readV1() (Stats, bool) {
	control, err := os.ReadFile(path(v1OOMControl))
	if err != nil {
		return Stats{}, false
	}
	s := Stats{Available: true}
	// oom_kill in memory.oom_control landed in kernel 4.13; older kernels
	// expose only under_oom, which says "right now" rather than "it
	// happened". There the field is absent, OOMKillsKnown stays false, and
	// the caller reports the limit without asserting a cause either way.
	s.OOMKills, s.OOMKillsKnown = fieldValue(string(control), "oom_kill")
	if limit := readUint(path(v1Limit)); limit < v1UnlimitedThreshold {
		s.LimitBytes = limit
	}
	s.PeakBytes = readUint(path(v1MaxUsage))
	s.CurrentBytes = readUint(path(v1Usage))
	return ownCgroupOnly(s), true
}

// ownCgroupOnly discards a reading that is not this container's.
//
// Without a cgroup namespace — cgroup v2 on older Docker, or an explicitly
// host-cgroup container — /sys/fs/cgroup is the HOST's root cgroup, and its
// oom_kill counter aggregates kills from everything on the box. Reporting
// that as ours would be worse than reporting nothing: it turns an unrelated
// OOM elsewhere on the host into a confident false explanation for this job.
//
// The runner always creates the agentbox container with a memory limit, so a
// cgroup with no limit is definitionally not ours. That is a cheap and
// reliable tell, and it fails in the safe direction — the cost of being wrong
// is losing the diagnostic, not inventing one.
func ownCgroupOnly(s Stats) Stats {
	if s.LimitBytes == 0 {
		return Stats{}
	}
	return s
}

// fieldValue pulls "<name> <number>" out of a whitespace-separated key/value
// file. Both memory.events and memory.oom_control are that shape. The second
// return distinguishes "present and zero" from "absent" — see OOMKillsKnown.
func fieldValue(content, name string) (uint64, bool) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			n, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

// readUint reads a single-number file, returning 0 for anything unreadable or
// non-numeric — which covers v2's literal "max".
func readUint(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// HumanBytes formats a byte count for a log line a person reads while
// debugging, not for machine parsing.
func HumanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return strconv.FormatUint(b, 10) + " B"
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}
	value := float64(b) / float64(div)
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + [...]string{"KiB", "MiB", "GiB", "TiB"}[exp]
}

// Explain renders what the cgroup says about a run, for the job log.
//
// killed reports whether the agent subprocess died by signal — the caller
// knows that, this package does not. Splitting it that way keeps the two
// facts honest: a signal death WITH a recorded kill is an OOM and is stated
// as one; a signal death with the counter ABSENT is left open rather than
// explained away; and a run that merely ran hot is worth a line before it
// becomes a failure.
//
// Empty means there is nothing worth saying, which is the common case.
func Explain(killed bool) string {
	s := Read()
	if !s.Available {
		return ""
	}
	usage := "peak " + HumanBytes(s.PeakBytes)
	if s.PeakBytes == 0 {
		// No high-water mark on this kernel; usage at exit is all there is,
		// and it understates the peak — say so rather than implying the run
		// stayed this low throughout.
		usage = "usage at exit " + HumanBytes(s.CurrentBytes) + " (no peak recorded; the true peak was higher)"
	}
	limit := " of a " + HumanBytes(s.LimitBytes) + " limit"

	switch {
	case s.OOMKillsKnown && s.OOMKills > 0:
		return "the agent was OOM-killed by the kernel — " + usage + limit +
			", " + strconv.FormatUint(s.OOMKills, 10) + " process(es) killed in this container. " +
			"Raise AGENTBOX_MEMORY_BYTES on the runner, or reduce the build's parallelism."
	case killed && !s.OOMKillsKnown:
		// The honest answer, and the reason OOMKillsKnown exists: this
		// kernel does not expose the counter, so silence here would read as
		// "not an OOM" when it may well have been.
		return "the agent was killed by a signal; this kernel does not expose an OOM counter, so the cause is undetermined — " +
			usage + limit + "."
	case killed:
		// Counter present and zero: genuinely not the kernel. Saying so
		// rules memory out and points the next look elsewhere.
		return "the agent was killed by a signal, but the kernel recorded no OOM kill in this container — " +
			usage + limit + ". Memory is not the cause."
	case s.NearLimit():
		return "memory ran close to the limit — " + usage + limit + "."
	}
	return ""
}

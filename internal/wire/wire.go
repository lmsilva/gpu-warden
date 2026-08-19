// Package wire is the published shape of a pass: the JSON document the
// server serves, the document squire-me reads, and the value both human
// renderers take.
//
// The internal types cannot cross a wire as they are. The verdict enums are
// integers whose values are positional - adding a state would silently
// renumber the rest under every consumer - and the report carries Kubernetes
// pod names that do not belong in a user-facing document. So the pass is
// converted exactly once, here, and everything downstream - the table, the
// page, the JSON encoder, squire-me - reads this shape. There is no
// conversion back: the renderers speak the wire's language, so nothing ever
// parses a state word into an enum again.
//
// Unmeasured is null, never zero. Every field with a Has companion on the
// internal report becomes a pointer, which is the same contract the table
// keeps by printing a dash: a value that was never read must not be
// mistakable for a value that was read as zero.
package wire

import (
	"time"

	"github.com/lmsilva/squire/internal/cycle"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/verdict"
)

// Schema versions this document. Anything a script parses is a compatibility
// surface, so the document says which contract it keeps; a breaking change
// to a name or a meaning increments it.
const Schema = 1

// Snapshot is one pass, in its published form.
type Snapshot struct {
	Schema int       `json:"schema"`
	At     time.Time `json:"at"`

	Jobs     []Job     `json:"jobs"`
	Findings []Finding `json:"findings"`
	Queue    Queue     `json:"queue"`

	// EngineFaulted records that the engine signal was dropped this pass
	// because it looked broken. It is a fleet-level judgement - the internal
	// reports each carry an identical copy - so on the wire it is said once,
	// about the pass.
	EngineFaulted bool `json:"engine_faulted"`
}

// Job is one running GPU job. States are the words the metrics already use;
// durations are seconds; measurements are pointers, null when never read.
type Job struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	// User is the display name, and falls back to "uid:N" where the site
	// cannot resolve the owner - the same fallback every other surface
	// shows, so a support conversation is about one string.
	User string `json:"user"`
	// UID is the numeric owner, when Slurm sent one. This is the machine
	// field: filtering matches on it, never on the display name.
	UID       *int   `json:"uid"`
	Partition string `json:"partition"`

	GPUs int `json:"gpus"`
	// PerGPU says whether the telemetry below was scoped to the job's own
	// devices. False means the numbers cover every GPU on the job's nodes,
	// and any honest reading of them has to know that.
	PerGPU bool `json:"per_gpu"`

	ElapsedSeconds int64 `json:"elapsed_seconds"`

	AvgUtilPct            *float64 `json:"avg_util_pct"`
	PeakUtilPct           *float64 `json:"peak_util_pct"`
	WastedGPUHours        *float64 `json:"wasted_gpu_hours"`
	Memory                *Memory  `json:"memory"`
	LitGPUs               *int     `json:"lit_gpus"`
	FirstWorkAfterSeconds *int64   `json:"first_work_after_seconds"`

	// Activity is analyzing, healthy, idle or zombie - the same words the
	// squire_job_activity metric uses for its states.
	Activity string `json:"activity"`
	// Confidence is low, medium or high, and null while the verdict is
	// still analyzing.
	Confidence *string `json:"confidence"`
	// Sizing is unknown, right_sized, over_provisioned or under_provisioned
	// - the squire_job_sizing metric's vocabulary, not the table's phrasing.
	// The two machine surfaces must agree on the same enum; how to say
	// "possibly under-provisioned" to a person is a renderer's decision.
	Sizing  string   `json:"sizing"`
	Reasons []string `json:"reasons"`
}

// Memory is the framebuffer reading. One object rather than three loose
// fields, because the three numbers come from one read and are only ever
// present together.
type Memory struct {
	PeakMiB     float64 `json:"peak_mib"`
	CapacityMiB float64 `json:"capacity_mib"`
	PeakFrac    float64 `json:"peak_frac"`
}

// Finding is one finding, with the severity as its word.
type Finding struct {
	JobID    int    `json:"job_id"`
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// Queue is the cluster's pending pressure. The field names match the
// squire_pending_* metrics for the same reason the states do.
type Queue struct {
	PendingGPUJobs int `json:"pending_gpu_jobs"`
	PendingGPUs    int `json:"pending_gpus"`
}

// From converts a pass into its published form. It is the only conversion:
// there is no To, because nothing downstream needs the internal shape back.
func From(s cycle.Snapshot) Snapshot {
	jobs := make([]Job, 0, len(s.Reports))
	faulted := false
	for _, r := range s.Reports {
		jobs = append(jobs, fromReport(r))
		if r.EngineFaulted {
			faulted = true
		}
	}
	findings := make([]Finding, 0, len(s.Findings))
	for _, f := range s.Findings {
		findings = append(findings, Finding{
			JobID: f.JobID, Rule: f.Rule,
			Severity: f.Severity.String(), Message: f.Message,
		})
	}
	return Snapshot{
		Schema:   Schema,
		At:       s.At,
		Jobs:     jobs,
		Findings: findings,
		Queue: Queue{
			PendingGPUJobs: s.Queue.PendingGPUJobs,
			PendingGPUs:    s.Queue.PendingGPUs,
		},
		EngineFaulted: faulted,
	}
}

func fromReport(r report.JobReport) Job {
	j := Job{
		ID:        r.Job.JobID,
		Name:      r.Job.Name,
		User:      r.Job.Owner(),
		Partition: r.Job.Partition,
		GPUs:      r.GPUs,
		PerGPU:    r.PerGPU,
		// Truncated, not rounded: an age is how long something has been
		// true, and 89.6 seconds of it is 89.
		ElapsedSeconds: int64(r.Elapsed / time.Second),
		Activity:       r.Verdict.Activity.String(),
		Sizing:         r.Verdict.Sizing.Code(),
		Reasons:        reasons(r.Verdict.Reasons),
	}
	if r.Job.UserID.Set {
		uid := int(r.Job.UserID.Number)
		j.UID = &uid
	}
	if r.HasData {
		j.AvgUtilPct = ptr(r.AvgUtil)
		j.PeakUtilPct = ptr(r.PeakUtil)
		j.WastedGPUHours = ptr(r.WastedH)
	}
	if r.CapacityMiB > 0 {
		j.Memory = &Memory{
			PeakMiB:     r.PeakMemMiB,
			CapacityMiB: r.CapacityMiB,
			PeakFrac:    r.PeakMemFrac,
		}
	}
	if r.HasLit {
		j.LitGPUs = ptr(r.LitGPUs)
	}
	if r.HasFirstWork {
		// Rounded, not truncated, because the table rounds: the wire and
		// the row must say the same second.
		j.FirstWorkAfterSeconds = ptr(int64(r.FirstWorkAfter.Round(time.Second) / time.Second))
	}
	if c := r.Verdict.Confidence; c != verdict.ConfNone {
		j.Confidence = ptr(c.String())
	}
	return j
}

// reasons copies the verdict's reasons as an empty list rather than null. A
// consumer iterating reasons should not need the null check Go callers never
// needed.
func reasons(rs []string) []string {
	out := make([]string, 0, len(rs))
	return append(out, rs...)
}

func ptr[T any](v T) *T { return &v }

// Names maps job id to job name. Findings carry an id and no name, so every
// presenter needs this to label a row - once, here, rather than three
// slightly different loops.
func (s Snapshot) Names() map[int]string {
	names := make(map[int]string, len(s.Jobs))
	for _, j := range s.Jobs {
		names[j.ID] = j.Name
	}
	return names
}

// PodWide reports whether any job's telemetry covered more than its own
// devices - the fact behind the table's * marker and its footnote.
func (s Snapshot) PodWide() bool {
	for _, j := range s.Jobs {
		if !j.PerGPU {
			return true
		}
	}
	return false
}

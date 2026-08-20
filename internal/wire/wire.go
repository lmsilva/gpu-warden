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
	"encoding/json"
	"io"
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

	// ClusterUnread says the node and partition lists could not be read, so
	// the configuration rules that compare a request against the hardware
	// did not run. Without it a shorter findings list reads as good news.
	ClusterUnread bool `json:"cluster_unread"`
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

// Finding kinds. Two engines report findings and they are not the same kind
// of claim: an allocation finding is about a job in the table above it, and a
// configuration finding can be about a job that is not there at all - one
// that holds no card, or has not started. A reader has to be able to tell
// them apart, and so does a script.
const (
	KindAllocation    = "allocation"
	KindConfiguration = "configuration"
)

// Finding is one finding, with the severity and the kind as their words.
//
// JobName travels with it. A finding can be about a job the jobs list does
// not contain, so a consumer that tried to look the name up there would find
// nothing - the document has to be readable on its own.
type Finding struct {
	JobID   int    `json:"job_id"`
	JobName string `json:"job_name"`
	// JobUID is the owner of the job this finding is about, null when Slurm
	// did not send one. The owner filter reads it: a finding can be about a
	// job that is not in the jobs list, and matching on that list would drop
	// every finding a user most needs to see.
	JobUID   *int   `json:"job_uid"`
	Kind     string `json:"kind"`
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
	// Names come from the whole job list, not from the jobs above: a
	// configuration finding can be about a job that holds no card.
	names := make(map[int]string, len(s.Jobs))
	uids := make(map[int]*int, len(s.Jobs))
	for _, j := range s.Jobs {
		names[j.JobID] = j.Name
		if j.UserID.Set {
			uids[j.JobID] = ptr(int(j.UserID.Number))
		}
	}
	findings := make([]Finding, 0, len(s.Findings)+len(s.ConfigFindings))
	for _, f := range s.Findings {
		findings = append(findings, Finding{
			JobID: f.JobID, JobName: names[f.JobID], JobUID: uids[f.JobID],
			Kind: KindAllocation,
			Rule: f.Rule, Severity: f.Severity.String(), Message: f.Message,
		})
	}
	// Allocation first, then configuration. The order is the document's, so
	// every reader gets the same one without sorting anything itself.
	for _, f := range s.ConfigFindings {
		findings = append(findings, Finding{
			JobID: f.JobID, JobName: names[f.JobID], JobUID: uids[f.JobID],
			Kind: KindConfiguration,
			Rule: f.Rule, Severity: f.Severity.String(), Message: f.Message,
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
		ClusterUnread: s.ClusterUnread,
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

// Encode writes a pass as JSON, whole or not at all. Marshalling to memory
// first means a failure produces an error and zero bytes, never a torn
// document a consumer half-parses - the same rule the HTML renderer keeps,
// for the same reason.
func Encode(w io.Writer, s Snapshot) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
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

// FindingFilter narrows which findings a pass shows. A zero value keeps
// everything.
//
// It touches findings only. The verdict table is what the cluster is doing
// and stays whole - hiding rows because a rule name was typed would answer a
// question nobody asked.
type FindingFilter struct {
	Rule  string // exact rule name, e.g. "no-time-limit"
	JobID int    // 0 means every job
}

// Empty reports whether this filter would keep everything.
func (f FindingFilter) Empty() bool { return f.Rule == "" && f.JobID == 0 }

// FilterFindings applies the filter. Both terms must match when both are set.
//
// An unknown rule name yields an empty list rather than an error: a script
// that greps for a rule the cluster has not tripped wants zero findings, not
// a failure, and a rule name is not a thing the server can validate without
// pinning every rule name into the wire contract.
func (s Snapshot) FilterFindings(f FindingFilter) Snapshot {
	if f.Empty() {
		return s
	}
	out := s
	out.Findings = make([]Finding, 0, len(s.Findings))
	for _, fi := range s.Findings {
		if f.Rule != "" && fi.Rule != f.Rule {
			continue
		}
		if f.JobID != 0 && fi.JobID != f.JobID {
			continue
		}
		out.Findings = append(out.Findings, fi)
	}
	return out
}

// ForUID narrows a pass to one owner's jobs, and the findings about them.
// The cluster-level facts stay: the queue, the timestamp and the engine
// caveat describe the pass rather than a job, and "2 jobs waiting for 9
// GPUs" beside your own idle allocation is half the point of looking.
//
// Matching is on the numeric uid alone - the display name is for people. A
// job whose uid Slurm did not send matches nobody: showing a job to the
// wrong person is the failure this filter exists to prevent, and an unknown
// owner cannot be shown to be the right one.
func (s Snapshot) ForUID(uid int) Snapshot {
	out := s
	out.Jobs = make([]Job, 0, len(s.Jobs))
	kept := make(map[int]bool, len(s.Jobs))
	for _, j := range s.Jobs {
		if j.UID != nil && *j.UID == uid {
			out.Jobs = append(out.Jobs, j)
			kept[j.ID] = true
		}
	}
	out.Findings = make([]Finding, 0, len(s.Findings))
	for _, f := range s.Findings {
		// Match on the finding's own owner, not on the jobs kept above. A
		// configuration finding is about a job that holds no card, so it is
		// never in that list - and dropping it would hide from a user
		// exactly the findings they can act on.
		if f.JobUID != nil && *f.JobUID == uid {
			out.Findings = append(out.Findings, f)
		}
	}
	return out
}

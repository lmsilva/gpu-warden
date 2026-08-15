// Package cross holds the findings that need both halves of what Squire
// knows: what a job asked Slurm for, and what its GPUs actually did.
//
// Neither half can make these alone. "Requested eight GPUs" is a
// configuration fact and says nothing about waste; "two devices were busy" is
// a measurement and says nothing about intent. Together they name a specific
// job holding specific hardware it never used.
//
// Pure, like internal/lint and internal/verdict: it takes plain numbers and
// returns findings, so every rule is testable with no cluster and no
// Prometheus. It borrows lint's Finding type rather than defining a second
// one, so a presenter renders findings from either source identically.
package cross

import (
	"fmt"
	"sort"
	"time"

	"github.com/lmsilva/squire/internal/lint"
)

// Job is one job's intent joined to its measurement. Every Has* field marks a
// signal that may be absent; a rule needing an absent signal stays silent
// rather than treating zero as a reading.
type Job struct {
	ID   int
	Name string
	User string

	// GPUsRequested is what Slurm allocated. Always known.
	GPUsRequested int

	// Elapsed is how long the job has been running, and Grace is the age
	// below which Squire judges nothing. A job still warming up has not yet
	// had the chance to be wasteful.
	Elapsed time.Duration
	Grace   time.Duration

	// PerGPU records whether the telemetry was scoped to this job's own
	// devices. When false the numbers cover every GPU on its nodes, so a
	// neighbour's work could be counted as this job's - and a finding built
	// on that would name the wrong person.
	PerGPU bool

	// HasLit and LitGPUs come from counting devices that crossed the burst
	// threshold. Zero lit is a measurement; not having the count is not.
	HasLit  bool
	LitGPUs int

	// HasFirstWork and FirstWorkAfter measure the gap between the job
	// starting and its first device doing work.
	HasFirstWork   bool
	FirstWorkAfter time.Duration
}

// SlowStartAfter is the delay past which a job's startup is worth mentioning.
// Container pulls and dataset staging routinely take minutes; a quarter of an
// hour before any device does work is a different scale of problem, and it is
// the platform team's problem rather than the job owner's.
const SlowStartAfter = 15 * time.Minute

// Check returns the cross-source findings for one job, ordered by rule name so
// output is stable between runs.
//
// Silence has three causes and all are deliberate: a job younger than its
// grace period, telemetry that was never scoped to the job's own devices, and
// any signal a rule needs that was not measured.
func Check(j Job) []lint.Finding {
	if j.Elapsed < j.Grace || !j.PerGPU || j.GPUsRequested <= 0 {
		return nil
	}
	var out []lint.Finding
	add := func(f lint.Finding) { out = append(out, f) }

	neverTouched(j, add)
	partiallyUsed(j, add)
	slowFirstWork(j, add)

	sort.SliceStable(out, func(a, b int) bool { return out[a].Rule < out[b].Rule })
	return out
}

// CheckAll runs Check over many jobs, preserving the caller's order.
func CheckAll(jobs []Job) []lint.Finding {
	var out []lint.Finding
	for _, j := range jobs {
		out = append(out, Check(j)...)
	}
	return out
}

// neverTouched: the job holds GPUs and not one of them has done work since it
// started. Distinct from a job that worked and stopped, which is a zombie -
// this one never began, and the usual cause is a build or an environment that
// cannot see the hardware at all.
func neverTouched(j Job, add func(lint.Finding)) {
	if !j.HasLit || j.LitGPUs > 0 {
		return
	}
	add(lint.Finding{JobID: j.ID, Rule: "gpu-requested-never-touched", Severity: lint.Warn,
		Message: fmt.Sprintf("holds %d GPU(s) and none has done any work in %s - the job may not be able to see them at all",
			j.GPUsRequested, j.Elapsed.Round(time.Minute))})
}

// partiallyUsed: the job holds more devices than it has ever lit. This is the
// finding no aggregate can produce, because the node reads as fully allocated
// and the job's average utilization is diluted across cards that never ran.
func partiallyUsed(j Job, add func(lint.Finding)) {
	if !j.HasLit || j.LitGPUs <= 0 || j.LitGPUs >= j.GPUsRequested {
		return
	}
	add(lint.Finding{JobID: j.ID, Rule: "partially-used-allocation", Severity: lint.Warn,
		Message: fmt.Sprintf("holds %d GPUs but only %d has done any work - the other %d are allocated and idle",
			j.GPUsRequested, j.LitGPUs, j.GPUsRequested-j.LitGPUs)})
}

// slowFirstWork: the devices were held for a long time before any of them
// started. Reported as a note, not a warning, because the time is often spent
// on something necessary - the point is that it is measurable and currently
// invisible, not that somebody did wrong.
func slowFirstWork(j Job, add func(lint.Finding)) {
	if !j.HasFirstWork || j.FirstWorkAfter < SlowStartAfter {
		return
	}
	add(lint.Finding{JobID: j.ID, Rule: "slow-first-gpu-work", Severity: lint.Note,
		Message: fmt.Sprintf("%s passed before any GPU did work - startup, staging or initialization held %d device(s) idle",
			j.FirstWorkAfter.Round(time.Minute), j.GPUsRequested)})
}

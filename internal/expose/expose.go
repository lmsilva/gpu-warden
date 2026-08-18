// Package expose renders JobReports in Prometheus text exposition format.
package expose

import (
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/lmsilva/squire/internal/buildinfo"
	"github.com/lmsilva/squire/internal/cycle"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/verdict"
)

// The full enum sets. Every job emits one series per state carrying 0 or 1, so
// a state a job leaves goes to 0 rather than vanishing and leaving its last
// value hanging in a graph.
var (
	activities = []verdict.Activity{verdict.Analyzing, verdict.Healthy, verdict.Idle, verdict.Zombie}
	sizings    = []verdict.Sizing{verdict.SizingUnknown, verdict.RightSized,
		verdict.OverProvisioned, verdict.UnderProvisioned}
)

// mib is the number of bytes in a mebibyte. DCGM reports framebuffer in MiB,
// but Prometheus convention is base units, so Squire converts on the way out.
const mib = 1024 * 1024

// esc escapes a label value per the Prometheus exposition format.
func esc(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	return r.Replace(v)
}

// bit renders a boolean as the 1/0 a gauge needs.
func bit(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ids renders the identity labels that every series carries. One helper rather
// than the same three labels written out per series: they have to match exactly
// or PromQL cannot join two of these together, and written out eight times they
// drift.
func ids(r report.JobReport) string {
	return fmt.Sprintf(`job_id="%d",user="%s",partition="%s"`,
		r.Job.JobID, esc(r.Job.Owner()), esc(r.Job.Partition))
}

// buildInfo says which build produced everything below it.
//
// The information is in the labels and the value is always 1, which is the
// convention for this metric everywhere: it makes the labels joinable onto
// any other series with group_left, so a graph can be annotated with the
// build that produced it.
//
// It matters more than it looks. Every other way of asking "what am I
// running" needs a shell in the container or a terminal on the host, and a
// distroless image has no shell. Scraping is the one channel that always
// works, and without this the numbers arrive with no way to say which build
// measured them.
//
// dirty is a label rather than an omission: a build from a modified tree
// reports a commit whose code is not what is running, and a claim about the
// commit without that caveat would be confidently wrong.
func buildInfo(w io.Writer) {
	rev, dirty := buildinfo.Revision()
	labels := fmt.Sprintf(`version="%s",revision="%s",dirty="%t",go_version="%s"`,
		esc(buildinfo.Version), esc(rev), dirty, esc(runtime.Version()))
	fmt.Fprintln(w, "# HELP squire_build_info The build serving these metrics. Always 1; read the labels.")
	fmt.Fprintln(w, "# TYPE squire_build_info gauge")
	fmt.Fprintf(w, "squire_build_info{%s} 1\n", labels)
}

// Write renders one scrape's worth of Squire metrics.
//
// It takes the whole snapshot rather than the pieces it reads, so every
// presenter has the same signature and a field added to a pass reaches this
// one without changing how it is called.
func Write(w io.Writer, s cycle.Snapshot) {
	reports, q := s.Reports, s.Queue
	buildInfo(w)
	fmt.Fprintln(w, "# HELP squire_job_gpu_utilization_percent Average GPU utilization per running job.")
	fmt.Fprintln(w, "# TYPE squire_job_gpu_utilization_percent gauge")
	for _, r := range reports {
		fmt.Fprintf(w, "squire_job_gpu_utilization_percent{%s} %.2f\n", ids(r), r.AvgUtil)
	}

	// Devices actually lit, next to devices held. The pair is the numerator
	// and denominator of the partially-used finding, and neither number is
	// derivable from the averaged utilization above.
	// Cluster-level pressure, with no job labels: it is a fact about the
	// queue, not about any one job. Always emitted, including zero, so a
	// dashboard can tell "nobody waiting" from "Squire is not running".
	fmt.Fprintln(w, "# HELP squire_pending_gpu_jobs Jobs waiting because the cluster is short of GPUs.")
	fmt.Fprintln(w, "# TYPE squire_pending_gpu_jobs gauge")
	fmt.Fprintf(w, "squire_pending_gpu_jobs %d\n", q.PendingGPUJobs)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "# HELP squire_pending_gpus GPU devices those waiting jobs are asking for.")
	fmt.Fprintln(w, "# TYPE squire_pending_gpus gauge")
	fmt.Fprintf(w, "squire_pending_gpus %d\n", q.PendingGPUs)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "# HELP squire_job_gpus_lit GPU devices held by the job that have done work at some point in the run.")
	fmt.Fprintln(w, "# TYPE squire_job_gpus_lit gauge")
	for _, r := range reports {
		// Only emitted when measured. A zero here means the job lit nothing,
		// so publishing an unread count as zero would export a false finding.
		if r.HasLit {
			fmt.Fprintf(w, "squire_job_gpus_lit{%s} %d\n", ids(r), r.LitGPUs)
		}
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "# HELP squire_job_gpus_held GPU devices allocated to the job.")
	fmt.Fprintln(w, "# TYPE squire_job_gpus_held gauge")
	for _, r := range reports {
		fmt.Fprintf(w, "squire_job_gpus_held{%s} %d\n", ids(r), r.GPUs)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "# HELP squire_job_gpu_hours_wasted Allocated-but-unused GPU-hours per running job.")
	fmt.Fprintln(w, "# TYPE squire_job_gpu_hours_wasted gauge")
	for _, r := range reports {
		fmt.Fprintf(w, "squire_job_gpu_hours_wasted{%s} %.2f\n", ids(r), r.WastedH)
	}

	// Axis A. One series per state per job; exactly one carries a 1.
	fmt.Fprintln(w, "# HELP squire_job_activity Job activity verdict: 1 on the state currently held (analyzing, healthy, idle, zombie).")
	fmt.Fprintln(w, "# TYPE squire_job_activity gauge")
	for _, r := range reports {
		for _, a := range activities {
			fmt.Fprintf(w, "squire_job_activity{%s,state=\"%s\"} %d\n",
				ids(r), a, bit(r.Verdict.Activity == a))
		}
	}

	// Axis B. Sizing uses Code() rather than String(): "possibly
	// under-provisioned" contains spaces, which is legal in a label value but
	// miserable to type in a PromQL matcher.
	fmt.Fprintln(w, "# HELP squire_job_sizing Job sizing verdict: 1 on the state currently held (unknown, right_sized, over_provisioned, under_provisioned).")
	fmt.Fprintln(w, "# TYPE squire_job_sizing gauge")
	for _, r := range reports {
		for _, s := range sizings {
			fmt.Fprintf(w, "squire_job_sizing{%s,state=\"%s\"} %d\n",
				ids(r), s.Code(), bit(r.Verdict.Sizing == s))
		}
	}

	// Confidence is ordinal, so it is a number (0 none, 1 low, 2 medium,
	// 3 high) rather than a label — that way an alert can say
	// "zombie AND confidence >= 3" with plain arithmetic.
	fmt.Fprintln(w, "# HELP squire_job_verdict_confidence Confidence in the activity verdict: 0 none, 1 low, 2 medium, 3 high.")
	fmt.Fprintln(w, "# TYPE squire_job_verdict_confidence gauge")
	for _, r := range reports {
		fmt.Fprintf(w, "squire_job_verdict_confidence{%s} %d\n",
			ids(r), int(r.Verdict.Confidence))
	}

	// Memory, in bytes, per Prometheus base-unit convention. Emitting peak and
	// capacity separately (rather than a precomputed ratio) lets a dashboard
	// divide them itself and keeps the raw evidence visible.
	fmt.Fprintln(w, "# HELP squire_job_gpu_memory_peak_bytes Peak framebuffer memory used on a single GPU of the job.")
	fmt.Fprintln(w, "# TYPE squire_job_gpu_memory_peak_bytes gauge")
	for _, r := range reports {
		fmt.Fprintf(w, "squire_job_gpu_memory_peak_bytes{%s} %.0f\n", ids(r), r.PeakMemMiB*mib)
	}
	fmt.Fprintln(w, "# HELP squire_job_gpu_memory_capacity_bytes Total framebuffer memory of the GPU the job holds.")
	fmt.Fprintln(w, "# TYPE squire_job_gpu_memory_capacity_bytes gauge")
	for _, r := range reports {
		fmt.Fprintf(w, "squire_job_gpu_memory_capacity_bytes{%s} %.0f\n", ids(r), r.CapacityMiB*mib)
	}

	// Kept for compatibility with anything already alerting on it. It is now
	// derived from the Activity axis rather than being an independent flag,
	// so the two can never disagree.
	fmt.Fprintln(w, "# HELP squire_job_zombie 1 if the job is judged a zombie allocation.")
	fmt.Fprintln(w, "# TYPE squire_job_zombie gauge")
	for _, r := range reports {
		fmt.Fprintf(w, "squire_job_zombie{%s} %d\n", ids(r), bit(r.IsZombie()))
	}
}

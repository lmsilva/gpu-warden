// Package expose renders JobReports in Prometheus text exposition format.
package expose

import (
	"fmt"
	"io"
	"strings"

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

// Write renders one scrape's worth of Squire metrics.
func Write(w io.Writer, reports []report.JobReport) {
	fmt.Fprintln(w, "# HELP squire_job_gpu_utilization_percent Average GPU utilization per running job.")
	fmt.Fprintln(w, "# TYPE squire_job_gpu_utilization_percent gauge")
	for _, r := range reports {
		fmt.Fprintf(w, "squire_job_gpu_utilization_percent{%s} %.2f\n", ids(r), r.AvgUtil)
	}

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

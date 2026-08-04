// Package expose renders JobReports in Prometheus text exposition format.
package expose

import (
	"fmt"
	"io"
	"strings"

	"github.com/lmsilva/gpu-warden/internal/report"
)

// esc escapes a label value per the Prometheus exposition format.
func esc(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	return r.Replace(v)
}

// Write renders one scrape's worth of warden metrics.
func Write(w io.Writer, reports []report.JobReport) {
	fmt.Fprintln(w, "# HELP warden_job_gpu_utilization_percent Average GPU utilization per running job.")
	fmt.Fprintln(w, "# TYPE warden_job_gpu_utilization_percent gauge")
	for _, r := range reports {
		fmt.Fprintf(w, "warden_job_gpu_utilization_percent{job_id=\"%d\",user=\"%s\",partition=\"%s\"} %.2f\n",
			r.Job.JobID, esc(r.Job.UserName), esc(r.Job.Partition), r.AvgUtil)
	}
	fmt.Fprintln(w, "# HELP warden_job_gpu_hours_wasted Allocated-but-unused GPU-hours per running job.")
	fmt.Fprintln(w, "# TYPE warden_job_gpu_hours_wasted gauge")
	for _, r := range reports {
		fmt.Fprintf(w, "warden_job_gpu_hours_wasted{job_id=\"%d\",user=\"%s\"} %.2f\n",
			r.Job.JobID, esc(r.Job.UserName), r.WastedH)
	}
	fmt.Fprintln(w, "# HELP warden_job_zombie 1 if the job is judged a zombie allocation.")
	fmt.Fprintln(w, "# TYPE warden_job_zombie gauge")
	for _, r := range reports {
		z := 0
		if r.Zombie {
			z = 1
		}
		fmt.Fprintf(w, "warden_job_zombie{job_id=\"%d\",user=\"%s\"} %d\n", r.Job.JobID, esc(r.Job.UserName), z)
	}
}

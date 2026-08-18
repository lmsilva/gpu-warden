package expose

import (
	"strings"
	"testing"

	"github.com/lmsilva/squire/internal/buildinfo"
	"github.com/lmsilva/squire/internal/cycle"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/slurmapi"
)

func TestWriteEscapesLabels(t *testing.T) {
	var sb strings.Builder
	Write(&sb, cycle.Snapshot{Reports: []report.JobReport{{Job: slurmapi.Job{JobID: 1,
		UserName: "evil\"} bad{", Partition: "p"}, HasData: true}}})
	out := sb.String()
	if strings.Contains(out, `user="evil"`) || !strings.Contains(out, `evil\"`) {
		t.Errorf("label value not escaped:\n%s", out)
	}
}

// TestQueueGaugesAlwaysEmitted pins a contract a dashboard depends on: the
// pending series are published even when nothing is waiting. Omitting them at
// zero would make "nobody is queued" indistinguishable from "Squire is not
// running", and the second is an alert while the first is good news.
func TestQueueGaugesAlwaysEmitted(t *testing.T) {
	var quiet strings.Builder
	Write(&quiet, cycle.Snapshot{})
	for _, want := range []string{"squire_pending_gpu_jobs 0", "squire_pending_gpus 0"} {
		if !strings.Contains(quiet.String(), want) {
			t.Errorf("an empty queue must still publish %q", want)
		}
	}

	// And the two numbers must not be swapped on the way out.
	var busy strings.Builder
	Write(&busy, cycle.Snapshot{Queue: report.Queue{PendingGPUJobs: 3, PendingGPUs: 11}})
	if !strings.Contains(busy.String(), "squire_pending_gpu_jobs 3") ||
		!strings.Contains(busy.String(), "squire_pending_gpus 11") {
		t.Errorf("queue gauges transposed or missing:\n%s", busy.String())
	}

	// Cluster facts carry no job labels - they belong to no job.
	for _, line := range strings.Split(busy.String(), "\n") {
		if strings.HasPrefix(line, "squire_pending") && strings.Contains(line, "{") {
			t.Errorf("queue pressure is a cluster fact and must carry no labels: %s", line)
		}
	}
}

// TestUnreadTelemetryIsNotEmitted pins the gate on every telemetry-derived
// series. A job appears with its allocation and verdict the moment it is
// known; its numbers appear only once they have been read. Publishing an
// unread utilization as 0% would hand every dashboard a false idle reading -
// the exact export squire_job_gpus_lit already refuses.
func TestUnreadTelemetryIsNotEmitted(t *testing.T) {
	unread := report.JobReport{Job: slurmapi.Job{JobID: 7, UserName: "cam", Partition: "all"}, GPUs: 2}
	var sb strings.Builder
	Write(&sb, cycle.Snapshot{Reports: []report.JobReport{unread}})
	out := sb.String()

	for _, series := range []string{
		"squire_job_gpu_utilization_percent{",
		"squire_job_gpu_hours_wasted{",
		"squire_job_gpu_memory_peak_bytes{",
		"squire_job_gpu_memory_capacity_bytes{",
		"squire_job_gpus_lit{",
	} {
		if strings.Contains(out, series) {
			t.Errorf("unread telemetry must not be published: %s\n%s", series, out)
		}
	}
	// What is known is still published: the allocation and the verdict.
	for _, want := range []string{"squire_job_gpus_held{", "squire_job_activity{"} {
		if !strings.Contains(out, want) {
			t.Errorf("known facts must still be published: %s\n%s", want, out)
		}
	}

	// And a measured zero is a value, not an absence.
	zero := unread
	zero.HasData = true
	var sb2 strings.Builder
	Write(&sb2, cycle.Snapshot{Reports: []report.JobReport{zero}})
	if !strings.Contains(sb2.String(), "squire_job_gpu_utilization_percent{") {
		t.Errorf("a measured zero must be published:\n%s", sb2.String())
	}
}

// TestBuildInfoIsAlwaysEmitted covers the one series that must appear even
// when the cluster is empty and every other metric is absent. A scrape that
// returned nothing at all could not say which build returned nothing.
func TestBuildInfoIsAlwaysEmitted(t *testing.T) {
	var sb strings.Builder
	Write(&sb, cycle.Snapshot{})
	out := sb.String()

	if !strings.Contains(out, "# TYPE squire_build_info gauge") {
		t.Errorf("build info must be declared:\n%s", out)
	}
	// The value carries no information; the labels do. A value other than 1
	// would break the group_left join this exists to support.
	if !strings.Contains(out, `go_version="go1`) || !strings.Contains(out, "} 1\n") {
		t.Errorf("want a labelled series with value 1:\n%s", out)
	}
	for _, label := range []string{"version=", "revision=", "tree_state="} {
		if !strings.Contains(out, label) {
			t.Errorf("build info is missing the %s label:\n%s", label, out)
		}
	}
}

// TestBuildInfoEscapesItsLabels. Version is injected at link time by whatever
// built the binary, so it is not a constant this package controls - and it
// reaches a label the same way a username does.
func TestBuildInfoEscapesItsLabels(t *testing.T) {
	was := buildinfo.Version
	buildinfo.Version = `v1"} evil{x="`
	defer func() { buildinfo.Version = was }()

	var sb strings.Builder
	Write(&sb, cycle.Snapshot{})
	out := sb.String()

	// The injected text still appears - it is the version, and Squire reports
	// what it was given. What must not appear is an unescaped quote closing
	// the label set early, which is what would turn one series into two.
	if strings.Contains(out, `version="v1"`) || !strings.Contains(out, `v1\"} evil`) {
		t.Errorf("a version string broke out of its label:\n%s", out)
	}
}

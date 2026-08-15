package expose

import (
	"strings"
	"testing"

	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/slurmapi"
)

func TestWriteEscapesLabels(t *testing.T) {
	var sb strings.Builder
	Write(&sb, []report.JobReport{{Job: slurmapi.Job{JobID: 1,
		UserName: "evil\"} bad{", Partition: "p"}, HasData: true}}, report.Queue{})
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
	Write(&quiet, nil, report.Queue{})
	for _, want := range []string{"squire_pending_gpu_jobs 0", "squire_pending_gpus 0"} {
		if !strings.Contains(quiet.String(), want) {
			t.Errorf("an empty queue must still publish %q", want)
		}
	}

	// And the two numbers must not be swapped on the way out.
	var busy strings.Builder
	Write(&busy, nil, report.Queue{PendingGPUJobs: 3, PendingGPUs: 11})
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

package cross

import (
	"testing"
	"time"

	"github.com/lmsilva/squire/internal/lint"
)

// live is a job past its grace period with per-device telemetry - the state in
// which findings are allowed at all. Cases below vary one thing from it.
func live() Job {
	return Job{ID: 1, Name: "train", User: "luis", GPUsRequested: 4,
		Elapsed: time.Hour, Grace: 15 * time.Minute, PerGPU: true,
		HasLit: true, LitGPUs: 4}
}

func rules(fs []lint.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

func TestRules(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Job)
		want []string
	}{
		{"every device working fires nothing", func(j *Job) {}, nil},
		{"holds four, lit one",
			func(j *Job) { j.LitGPUs = 1 },
			[]string{"partially-used-allocation"}},
		{"holds four, lit none",
			func(j *Job) { j.LitGPUs = 0 },
			[]string{"gpu-requested-never-touched"}},
		{"a long wait before the first device started",
			func(j *Job) { j.HasFirstWork, j.FirstWorkAfter = true, 40*time.Minute },
			[]string{"slow-first-gpu-work"}},
		{"a short startup is not worth reporting",
			func(j *Job) { j.HasFirstWork, j.FirstWorkAfter = true, 2*time.Minute },
			nil},
		{"partly used and slow to start are separate findings",
			func(j *Job) {
				j.LitGPUs = 2
				j.HasFirstWork, j.FirstWorkAfter = true, 40*time.Minute
			},
			[]string{"partially-used-allocation", "slow-first-gpu-work"}},
	}
	for _, tc := range cases {
		j := live()
		tc.mod(&j)
		got := Check(j)
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, rules(got), tc.want)
			continue
		}
		for i, r := range tc.want {
			if got[i].Rule != r {
				t.Errorf("%s: finding %d is %q, want %q", tc.name, i, got[i].Rule, r)
			}
		}
		for _, f := range got {
			t.Logf("%-30s %-4s job %d: %s", f.Rule, f.Severity, f.JobID, f.Message)
		}
	}
}

// TestNeverTouchedAndPartialAreExclusive: a job that lit nothing is not also a
// job that lit some. Reporting both would be two accusations for one fact.
func TestNeverTouchedAndPartialAreExclusive(t *testing.T) {
	j := live()
	j.LitGPUs = 0
	if got := rules(Check(j)); len(got) != 1 {
		t.Errorf("lit zero must produce exactly one finding, got %v", got)
	}
}

// TestUnmeasuredIsSilent is the contract that separates a measured zero from
// an unread count. Without it, every job on a cluster with no telemetry would
// be accused of never touching its GPUs.
func TestUnmeasuredIsSilent(t *testing.T) {
	j := live()
	j.HasLit, j.LitGPUs = false, 0
	if got := Check(j); len(got) != 0 {
		t.Errorf("an unread device count must produce nothing, got %v", rules(got))
	}
	j = live()
	j.HasFirstWork, j.FirstWorkAfter = false, 0
	if has(Check(j), "slow-first-gpu-work") {
		t.Errorf("an unread first-work time must produce nothing")
	}
}

// TestNodeWideTelemetryIsSilent: without per-device scoping the numbers cover
// every GPU on the job's nodes, so a neighbour's work could exonerate this job
// or its idleness could condemn it. Either way the finding would name the
// wrong person.
func TestNodeWideTelemetryIsSilent(t *testing.T) {
	j := live()
	j.PerGPU, j.LitGPUs = false, 0
	if got := Check(j); len(got) != 0 {
		t.Errorf("node-wide telemetry must produce nothing, got %v", rules(got))
	}
}

// TestGraceIsRespected: a job still starting up has not yet had the chance to
// waste anything, and the same grace governs the activity verdict.
func TestGraceIsRespected(t *testing.T) {
	j := live()
	j.Elapsed, j.LitGPUs = 5*time.Minute, 0
	if got := Check(j); len(got) != 0 {
		t.Errorf("a job inside its grace period must produce nothing, got %v", rules(got))
	}
}

func has(fs []lint.Finding, rule string) bool {
	for _, f := range fs {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

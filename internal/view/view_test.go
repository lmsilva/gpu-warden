package view

import (
	"strings"
	"testing"
	"time"

	"github.com/lmsilva/squire/internal/cycle"
	"github.com/lmsilva/squire/internal/lint"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/verdict"
)

// busy is a healthy job with every optional signal measured.
func busy() report.JobReport {
	return report.JobReport{
		Job:         slurmapi.Job{JobID: 101, Name: "train", UserName: "ana", Partition: "all"},
		GPUs:        2,
		Elapsed:     90 * time.Minute,
		AvgUtil:     94,
		PeakUtil:    100,
		PeakMemMiB:  4096,
		CapacityMiB: 15095,
		PeakMemFrac: 0.27,
		WastedH:     0.2,
		HasData:     true,
		PerGPU:      true,
		HasLit:      true, LitGPUs: 2,
		HasFirstWork: true, FirstWorkAfter: 58 * time.Second,
		Verdict: verdict.Verdict{
			Activity: verdict.Healthy, Sizing: verdict.RightSized,
			Confidence: verdict.ConfMedium,
			Reasons:    []string{"peak 100% over 30m0s", "avg 94% since start"},
		},
	}
}

// unmeasured is the case the dash exists for: no telemetry scoped to the
// job's own devices, and no lit or first-work reading at all.
func unmeasured() report.JobReport {
	return report.JobReport{
		Job:     slurmapi.Job{JobID: 102, Name: "wrap", UserName: "bo"},
		GPUs:    1,
		Elapsed: 2 * time.Minute,
		Verdict: verdict.Verdict{Activity: verdict.Analyzing},
	}
}

func TestTableColumns(t *testing.T) {
	var sb strings.Builder
	Table(&sb, cycle.Snapshot{Reports: []report.JobReport{busy()}}, TableOptions{})
	out := sb.String()

	for _, want := range []string{"JOBID", "ACTIVITY", "SIZING", "101", "train", "ana",
		"healthy:medium", "right-sized", "4.0/15G (27%)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q:\n%s", want, out)
		}
	}
	// The evidence columns are opt-in, because a terminal has a width budget.
	for _, unwanted := range []string{"FIRST-WORK", "peak 100% over 30m0s"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the default table must not carry %q:\n%s", unwanted, out)
		}
	}
}

func TestTableWideShowsEveryReason(t *testing.T) {
	var sb strings.Builder
	Table(&sb, cycle.Snapshot{Reports: []report.JobReport{busy()}}, TableOptions{Wide: true})
	out := sb.String()

	// Both reasons, not just the first. The first alone can assert a sizing
	// verdict and never say on what evidence.
	for _, want := range []string{"peak 100% over 30m0s", "avg 94% since start", "58s"} {
		if !strings.Contains(out, want) {
			t.Errorf("--wide is missing %q:\n%s", want, out)
		}
	}
}

// TestUnmeasuredReadsAsADash is the one that protects a principle rather than
// a layout: a measured zero and an unread value are different facts, and a
// numeric column cannot say "unknown".
func TestUnmeasuredReadsAsADash(t *testing.T) {
	r := toRow(unmeasured(), 0)
	if r.Lit != "-" {
		t.Errorf("an unread device count must be a dash, got %q", r.Lit)
	}
	if r.FirstWork != "-" {
		t.Errorf("an unread first-work time must be a dash, got %q", r.FirstWork)
	}
	if r.Mem != "-" {
		t.Errorf("an unread framebuffer capacity must be a dash, got %q", r.Mem)
	}
	if r.Sizing != "-" {
		t.Errorf("an undecided sizing axis must be a dash, got %q", r.Sizing)
	}
	// Utilization and its derived waste share the rule: no telemetry read
	// means no numbers, in all three columns at once.
	if r.Avg != "-" {
		t.Errorf("unread utilization must be a dash, got %q", r.Avg)
	}
	if r.Peak != "-" {
		t.Errorf("an unread peak must be a dash, got %q", r.Peak)
	}
	if r.Wasted != "-" {
		t.Errorf("waste derived from unread telemetry must be a dash, got %q", r.Wasted)
	}

	// And a measured zero is a number, not a dash.
	lit0 := unmeasured()
	lit0.HasLit = true
	if got := toRow(lit0, 0).Lit; got != "0" {
		t.Errorf("a measured zero must print as 0, got %q", got)
	}
	data0 := unmeasured()
	data0.HasData = true
	if got := toRow(data0, 0); got.Avg != "0" || got.Wasted != "0.0" {
		t.Errorf("measured zeros must print as numbers, got avg %q wasted %q", got.Avg, got.Wasted)
	}
}

// TestPodWideIsMarked pins the marker that says the numbers on this row
// include a neighbour's work.
func TestPodWideIsMarked(t *testing.T) {
	if got := toRow(busy(), 0).GPUs; got != "2" {
		t.Errorf("per-device telemetry needs no marker, got %q", got)
	}
	if got := toRow(unmeasured(), 0).GPUs; got != "1*" {
		t.Errorf("node-wide telemetry must be marked, got %q", got)
	}

	var sb strings.Builder
	Table(&sb, cycle.Snapshot{Reports: []report.JobReport{unmeasured()}}, TableOptions{})
	if !strings.Contains(sb.String(), podWideNote) {
		t.Errorf("the marker needs its footnote:\n%s", sb.String())
	}
}

// TestDollarRateIsOptional keeps $0.00 off every row on a cluster nobody has
// priced.
func TestDollarRateIsOptional(t *testing.T) {
	if got := toRow(busy(), 0).Wasted; got != "0.2" {
		t.Errorf("without a rate the hours stand alone, got %q", got)
	}
	if got := toRow(busy(), 0.53).Wasted; got != "0.2 ($0.11)" {
		t.Errorf("with a rate the cost follows the hours, got %q", got)
	}
}

func TestFindingsBlock(t *testing.T) {
	s := cycle.Snapshot{
		Reports: []report.JobReport{busy()},
		Findings: []lint.Finding{{
			JobID: 101, Rule: "partially-used-allocation", Severity: lint.Warn,
			Message: "holds 2 GPUs but only 1 has done any work",
		}},
	}
	var sb strings.Builder
	Table(&sb, s, TableOptions{})
	out := sb.String()

	// The finding is labelled with the job's name, which it does not carry
	// itself.
	for _, want := range []string{"SEVERITY", "warn", "partially-used-allocation", "train"} {
		if !strings.Contains(out, want) {
			t.Errorf("findings block is missing %q:\n%s", want, out)
		}
	}
}

// TestNoFindingsPrintsNothingExtra keeps a clean cluster quiet: an empty
// findings header would read as a section the reader has to check.
func TestNoFindingsPrintsNothingExtra(t *testing.T) {
	var sb strings.Builder
	Table(&sb, cycle.Snapshot{Reports: []report.JobReport{busy()}}, TableOptions{})
	if strings.Contains(sb.String(), "SEVERITY") {
		t.Errorf("no findings must mean no findings block:\n%s", sb.String())
	}
}

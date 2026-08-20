package view

import (
	"strings"
	"testing"

	"github.com/lmsilva/squire/internal/wire"
)

func ptr[T any](v T) *T { return &v }

// busy is a healthy job with every optional signal measured.
func busy() wire.Job {
	return wire.Job{
		ID: 101, Name: "train", User: "ana", Partition: "all",
		GPUs: 2, PerGPU: true,
		ElapsedSeconds: 5400,
		AvgUtilPct:     ptr(94.0), PeakUtilPct: ptr(100.0),
		WastedGPUHours: ptr(0.2),
		Memory:         &wire.Memory{PeakMiB: 4096, CapacityMiB: 15095, PeakFrac: 0.27},
		LitGPUs:        ptr(2), FirstWorkAfterSeconds: ptr(int64(58)),
		Activity: "healthy", Confidence: ptr("medium"), Sizing: "right_sized",
		Reasons: []string{"peak 100% over 30m0s", "avg 94% since start"},
	}
}

// unmeasured is the case the dash exists for: nothing optional was read, so
// every measurement is null on the wire.
func unmeasured() wire.Job {
	return wire.Job{
		ID: 102, Name: "wrap", User: "bo",
		GPUs: 1, ElapsedSeconds: 120,
		Activity: "analyzing", Sizing: "unknown",
		Reasons: []string{},
	}
}

func TestTableColumns(t *testing.T) {
	var sb strings.Builder
	Table(&sb, wire.Snapshot{Jobs: []wire.Job{busy()}}, TableOptions{})
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
	Table(&sb, wire.Snapshot{Jobs: []wire.Job{busy()}}, TableOptions{Wide: true})
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
	// Utilization and its derived waste share the rule: null on the wire is
	// a dash on the row, never a zero.
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
	lit0.LitGPUs = ptr(0)
	if got := toRow(lit0, 0).Lit; got != "0" {
		t.Errorf("a measured zero must print as 0, got %q", got)
	}
	data0 := unmeasured()
	data0.AvgUtilPct = ptr(0.0)
	data0.WastedGPUHours = ptr(0.0)
	if got := toRow(data0, 0); got.Avg != "0" || got.Wasted != "0.0" {
		t.Errorf("measured zeros must print as numbers, got avg %q wasted %q", got.Avg, got.Wasted)
	}
}

// TestSizingSpeaksHuman pins the display map. The wire says right_sized
// because the sizing metric does; a person reads a phrase. And a code this
// renderer does not know prints as itself, rather than as a dash that would
// claim nothing was decided.
func TestSizingSpeaksHuman(t *testing.T) {
	j := unmeasured()
	j.Sizing = "under_provisioned"
	if got := toRow(j, 0).Sizing; got != "possibly under-provisioned" {
		t.Errorf("want the table's phrasing, got %q", got)
	}
	j.Sizing = "half_lit"
	if got := toRow(j, 0).Sizing; got != "half_lit" {
		t.Errorf("an unknown code must pass through, got %q", got)
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
	Table(&sb, wire.Snapshot{Jobs: []wire.Job{unmeasured()}}, TableOptions{})
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
	s := wire.Snapshot{
		Jobs: []wire.Job{busy()},
		Findings: []wire.Finding{{
			JobID: 101, JobName: "train", Kind: wire.KindAllocation,
			Rule: "partially-used-allocation", Severity: "warn",
			Message: "holds 2 GPUs but only 1 has done any work",
		}},
	}
	var sb strings.Builder
	Table(&sb, s, TableOptions{})
	out := sb.String()

	// The finding is labelled with the job's name, which it does not carry
	// itself.
	for _, want := range []string{"ALLOCATION", "SEVERITY", "warn", "partially-used-allocation", "train"} {
		if !strings.Contains(out, want) {
			t.Errorf("findings block is missing %q:\n%s", want, out)
		}
	}
}

// TestNoFindingsPrintsNothingExtra keeps a clean cluster quiet: an empty
// findings header would read as a section the reader has to check.
func TestNoFindingsPrintsNothingExtra(t *testing.T) {
	var sb strings.Builder
	Table(&sb, wire.Snapshot{Jobs: []wire.Job{busy()}}, TableOptions{})
	if strings.Contains(sb.String(), "SEVERITY") {
		t.Errorf("no findings must mean no findings block:\n%s", sb.String())
	}
}

// TestTableSeparatesTheTwoKinds: two blocks under their own headings. A
// configuration finding can be about a job that has no row above it, so a
// single merged list would leave the reader looking for one.
func TestTableSeparatesTheTwoKinds(t *testing.T) {
	s := wire.Snapshot{
		Jobs: []wire.Job{busy()},
		Findings: []wire.Finding{
			{JobID: 101, JobName: "train", Kind: wire.KindAllocation,
				Rule: "partially-used-allocation", Severity: "warn", Message: "one card idle"},
			{JobID: 900, JobName: "pending-cpu-job", Kind: wire.KindConfiguration,
				Rule: "no-time-limit", Severity: "warn", Message: "no time limit set"},
		},
	}
	var sb strings.Builder
	Table(&sb, s, TableOptions{})
	out := sb.String()
	for _, want := range []string{"ALLOCATION", "CONFIGURATION", "pending-cpu-job", "no-time-limit"} {
		if !strings.Contains(out, want) {
			t.Errorf("the table is missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "ALLOCATION") > strings.Index(out, "CONFIGURATION") {
		t.Error("allocation findings come first")
	}
	// A cluster with only configuration findings prints that block alone.
	only := wire.Snapshot{Findings: []wire.Finding{s.Findings[1]}}
	var sb2 strings.Builder
	Table(&sb2, only, TableOptions{})
	if strings.Contains(sb2.String(), "ALLOCATION") {
		t.Errorf("an empty block must not print its heading:\n%s", sb2.String())
	}
}

// TestTableSaysWhenTheClusterWasUnread pins the degradation note.
func TestTableSaysWhenTheClusterWasUnread(t *testing.T) {
	var sb strings.Builder
	Table(&sb, wire.Snapshot{ClusterUnread: true}, TableOptions{})
	if !strings.Contains(sb.String(), "could not be read") {
		t.Errorf("an unread cluster must be stated:\n%s", sb.String())
	}
}
